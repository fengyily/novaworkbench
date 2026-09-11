// Package secret encrypts/decrypts sensitive credential values (SSH private
// keys, passwords) before they land in the database. The master key lives in
// ~/.novaworkbench/secret.key, generated on first start via crypto/rand and
// stored with 0600 permissions. AES-256-GCM is used so any tamper attempt
// fails at decryption time (the 16-byte auth tag is checked).
//
// Loss of the master key file renders every existing agent_servers.auth_value
// ciphertext unrecoverable — the UI must warn about this. The package returns
// an error from Init rather than falling back to a derived/insecure key.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	// nonceSize is the standard GCM nonce length. 12 bytes is the recommended
	// size — random nonces are safe with AES-GCM at this length.
	nonceSize = 12
	// keySize is the AES-256 key length.
	keySize = 32
)

var (
	mu     sync.RWMutex
	gcm    cipher.AEAD
	keyDir string
)

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

// keyPath returns the master key file path. It is overridable via the
// NOVA_SECRET_KEY_PATH env var so tests can use a tempdir.
func keyPath() string {
	if p := os.Getenv("NOVA_SECRET_KEY_PATH"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".novaworkbench", "secret.key")
}

// loadKey reads the master key from keyPath(), or generates a fresh one and
// writes it (atomic: write-thempfile + rename, 0600). A non-existent file is
// the only allowed first-run path; any other read failure surfaces to Init.
func loadKey() (cipher.AEAD, error) {
	path := keyPath()
	keyDir = filepath.Dir(path)
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return nil, fmt.Errorf("secret: create key dir: %w", err)
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) != keySize {
			return nil, fmt.Errorf("secret: master key at %s has invalid length %d (expected %d) — refusing to start", path, len(data), keySize)
		}
	case errors.Is(err, os.ErrNotExist):
		// First run — generate and atomically write a fresh key.
		fresh := make([]byte, keySize)
		if _, err := io.ReadFull(rand.Reader, fresh); err != nil {
			return nil, fmt.Errorf("secret: generate master key: %w", err)
		}
		if err := writeAtomic(path, fresh); err != nil {
			return nil, err
		}
		data = fresh
	default:
		return nil, fmt.Errorf("secret: read master key at %s: %w", path, err)
	}

	block, err := aes.NewCipher(data)
	if err != nil {
		return nil, fmt.Errorf("secret: init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: init GCM: %w", err)
	}
	return aead, nil
}

// writeAtomic writes data to path with mode 0600 by first writing a tempfile
// in the same directory then renaming. Avoids leaving a half-written key file
// on disk if the process is killed mid-write.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".secret.key.*")
	if err != nil {
		return fmt.Errorf("secret: create temp key: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// If we never renamed, best-effort cleanup.
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secret: write temp key: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secret: chmod key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secret: sync key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secret: close temp key: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("secret: rename key into place: %w", err)
	}
	return nil
}

// Init loads (or generates) the master key. Must be called once at process
// startup, before any service that touches credentials. Subsequent calls
// are no-ops. The returned error should be fatal — there is no recoverable
// mode for an unavailable master key.
func Init() error {
	mu.Lock()
	defer mu.Unlock()
	if gcm != nil {
		return nil
	}
	aead, err := loadKey()
	if err != nil {
		return err
	}
	gcm = aead
	return nil
}

// Encrypt encrypts plain with the master key. Output is base64(nonce ||
// ciphertext || tag). The empty string is rejected — callers should not
// encrypt empty values and should keep the DB column at its default empty
// string in that case.
func Encrypt(plain string) (string, error) {
	mu.RLock()
	defer mu.RUnlock()
	if gcm == nil {
		return "", errors.New("secret: package not initialized — call secret.Init first")
	}
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secret: generate nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, []byte(plain), nil)
	out := make([]byte, 0, nonceSize+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt. Empty input is returned as-is (no error) so
// callers can round-trip through Get/Update without branching. Tampered
// ciphertext (auth tag mismatch) returns an error — the stored credential
// is no longer trustworthy.
func Decrypt(ciphertext string) (string, error) {
	mu.RLock()
	defer mu.RUnlock()
	if gcm == nil {
		return "", errors.New("secret: package not initialized — call secret.Init first")
	}
	if ciphertext == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("secret: decode ciphertext: %w", err)
	}
	if len(raw) < nonceSize {
		return "", fmt.Errorf("secret: ciphertext too short")
	}
	nonce, ct := raw[:nonceSize], raw[nonceSize:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("secret: decrypt (auth tag mismatch — master key changed?): %w", err)
	}
	return string(pt), nil
}

// KeyPath returns the master key file path. Exposed so the settings UI can
// show the user where the file lives (and what to back up).
func KeyPath() string {
	return keyPath()
}
