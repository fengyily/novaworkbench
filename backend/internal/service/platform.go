package service

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/secret"
)

type PlatformTokenService struct {
	db *db.DB
}

func NewPlatformTokenService(db *db.DB) *PlatformTokenService {
	return &PlatformTokenService{db: db}
}

// List returns every platform token row with the GPG public metadata
// (key id + enabled flag) but NEVER the private key / passphrase
// ciphertexts — those are read only via Get / GPGSigningMaterial, which
// are for internal callers (wizard / merge handler) and never surface on
// an API list response.
func (s *PlatformTokenService) List() ([]model.PlatformToken, error) {
	rows, err := s.db.Query(
		`SELECT id, name, platform, base_url, git_user_name, git_user_email,
		        gpg_key_id, gpg_enabled, created_at, updated_at
		 FROM platform_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []model.PlatformToken
	for rows.Next() {
		var t model.PlatformToken
		// gpg_enabled is INTEGER in sqlite/mysql/pg. Scan into an int then
		// convert — scanning directly into *bool fails on pg's stricter type
		// mapping and on sql.NullBool's strict zero-value handling.
		var gpgEnabled int
		if err := rows.Scan(
			&t.ID, &t.Name, &t.Platform, &t.BaseURL,
			&t.GitUserName, &t.GitUserEmail,
			&t.GPGKeyID, &gpgEnabled,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		t.GPGEnabled = gpgEnabled != 0
		tokens = append(tokens, t)
	}
	return tokens, nil
}

// Create inserts a new row. The optional GPG material (private key +
// passphrase) is encrypted with AES-256-GCM via internal/secret before
// landing in the DB — same mechanism as agent_servers.auth_value. Empty
// GPG fields are stored as the empty string and do not trigger an encrypt
// call. gpg_key_id is left empty at creation time; it is back-filled by
// the runtime GPG provision step (handler.GPGProvisioning*).
func (s *PlatformTokenService) Create(
	name, platform, baseURL, token, gitUserName, gitUserEmail string,
	gpgPrivateKey, gpgPassphrase string, gpgEnabled bool,
) (*model.PlatformToken, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := "tok_" + hex.EncodeToString(b)
	now := time.Now()

	encKey, err := encryptOptional(gpgPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt gpg private key: %w", err)
	}
	encPass, err := encryptOptional(gpgPassphrase)
	if err != nil {
		return nil, fmt.Errorf("encrypt gpg passphrase: %w", err)
	}

	_, err = s.db.Exec(
		`INSERT INTO platform_tokens
		   (id, name, platform, base_url, token, git_user_name, git_user_email,
		    gpg_key_id, gpg_private_key, gpg_passphrase, gpg_enabled,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, platform, baseURL, token, gitUserName, gitUserEmail,
		"", encKey, encPass, boolToInt(gpgEnabled),
		now, now,
	)
	if err != nil {
		return nil, err
	}

	return &model.PlatformToken{
		ID:           id,
		Name:         name,
		Platform:     platform,
		BaseURL:      baseURL,
		GitUserName:  gitUserName,
		GitUserEmail: gitUserEmail,
		GPGEnabled:   gpgEnabled,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// Update rewrites the editable fields of a token row.
//
// Semantics:
//   - updateSecret: PAT — pass "" to keep the existing secret untouched
//   - gpgPrivateKey / gpgPassphrase: same — pass "" to keep existing
//     ciphertexts. The caller passes through whatever the user submitted,
//     so leaving the textarea blank in the edit modal means "no change".
//   - clearGPG: explicit "remove all GPG material" flag. Required because
//     the empty-string-is-noop rule above would otherwise make it
//     impossible to delete a previously-uploaded key from the UI.
//   - gpgEnabled is a tri-state (pointer) so the caller can distinguish
//     "leave as-is" (nil) from "set to false" (pointer to false). The
//     HTTP layer maps the request body to this tri-state.
func (s *PlatformTokenService) Update(
	id, name, baseURL, gitUserName, gitUserEmail, updateSecret string,
	gpgPrivateKey, gpgPassphrase string,
	gpgEnabled *bool, clearGPG bool,
) error {
	now := time.Now()

	// Resolve the ciphertext (or empty) values for each GPG column.
	encKey := ""
	encPass := ""
	clearKey := ""
	clearPass := ""
	keyID := ""

	if clearGPG {
		// Explicit wipe — ignore any non-empty GPG material in the request.
		encKey, encPass = "", ""
	} else {
		if gpgPrivateKey != "" {
			ek, err := secret.Encrypt(gpgPrivateKey)
			if err != nil {
				return fmt.Errorf("encrypt gpg private key: %w", err)
			}
			encKey = ek
		}
		if gpgPassphrase != "" {
			ep, err := secret.Encrypt(gpgPassphrase)
			if err != nil {
				return fmt.Errorf("encrypt gpg passphrase: %w", err)
			}
			encPass = ep
		}
	}

	// Build the UPDATE statement piecewise so we only touch the columns
	// the caller actually intends to change. Keeping each branch small
	// makes the intent ("clear" vs "rotate" vs "identity-only edit")
	// obvious in review.
	var (
		res sql.Result
		err error
	)
	switch {
	case clearGPG && updateSecret != "":
		res, err = s.db.Exec(
			`UPDATE platform_tokens SET
			   name = ?, base_url = ?, git_user_name = ?, git_user_email = ?,
			   token = ?,
			   gpg_key_id = ?, gpg_private_key = ?, gpg_passphrase = ?, gpg_enabled = 0,
			   updated_at = ?
			 WHERE id = ?`,
			name, baseURL, gitUserName, gitUserEmail, updateSecret,
			keyID, encKey, encPass, now, id,
		)
	case clearGPG:
		res, err = s.db.Exec(
			`UPDATE platform_tokens SET
			   name = ?, base_url = ?, git_user_name = ?, git_user_email = ?,
			   gpg_key_id = ?, gpg_private_key = ?, gpg_passphrase = ?, gpg_enabled = 0,
			   updated_at = ?
			 WHERE id = ?`,
			name, baseURL, gitUserName, gitUserEmail,
			keyID, encKey, encPass, now, id,
		)
	case updateSecret != "" && (encKey != "" || encPass != ""):
		res, err = s.db.Exec(
			`UPDATE platform_tokens SET
			   name = ?, base_url = ?, git_user_name = ?, git_user_email = ?,
			   token = ?,
			   gpg_private_key = ?, gpg_passphrase = ?,
			   updated_at = ?
			 WHERE id = ?`,
			name, baseURL, gitUserName, gitUserEmail, updateSecret,
			encKey, encPass, now, id,
		)
	case updateSecret != "":
		res, err = s.db.Exec(
			`UPDATE platform_tokens SET
			   name = ?, base_url = ?, git_user_name = ?, git_user_email = ?,
			   token = ?,
			   updated_at = ?
			 WHERE id = ?`,
			name, baseURL, gitUserName, gitUserEmail, updateSecret, now, id,
		)
	case encKey != "" || encPass != "":
		res, err = s.db.Exec(
			`UPDATE platform_tokens SET
			   name = ?, base_url = ?, git_user_name = ?, git_user_email = ?,
			   gpg_private_key = ?, gpg_passphrase = ?,
			   updated_at = ?
			 WHERE id = ?`,
			name, baseURL, gitUserName, gitUserEmail,
			encKey, encPass, now, id,
		)
	default:
		// Identity-only or metadata-only update.
		res, err = s.db.Exec(
			`UPDATE platform_tokens SET
			   name = ?, base_url = ?, git_user_name = ?, git_user_email = ?,
			   updated_at = ?
			 WHERE id = ?`,
			name, baseURL, gitUserName, gitUserEmail, now, id,
		)
	}
	if err != nil {
		return err
	}

	// gpg_enabled is tri-state: nil = no change, non-nil = set explicitly.
	if gpgEnabled != nil {
		res, err = s.db.Exec(
			`UPDATE platform_tokens SET gpg_enabled = ?, updated_at = ? WHERE id = ?`,
			boolToInt(*gpgEnabled), now, id,
		)
		if err != nil {
			return err
		}
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("token not found: %s", id)
	}

	// Unused-but-referenced linter guards: clearKey / clearPass are
	// placeholders for future "explicit empty string = clear this column
	// only" semantics; keeping them documented in one place keeps the
	// Update docstring honest about the supported tri-states.
	_ = clearKey
	_ = clearPass
	return nil
}

// Get returns the token including the raw PAT and the GPG ciphertexts —
// for internal use only, never expose in API list. Callers that need the
// GPG material in plaintext should call GPGSigningMaterial instead.
func (s *PlatformTokenService) Get(id string) (*model.PlatformToken, error) {
	var t model.PlatformToken
	var gpgEnabled int
	err := s.db.QueryRow(
		`SELECT id, name, platform, base_url, token,
		        git_user_name, git_user_email,
		        gpg_key_id, gpg_private_key, gpg_passphrase, gpg_enabled,
		        created_at, updated_at
		 FROM platform_tokens WHERE id = ?`, id,
	).Scan(
		&t.ID, &t.Name, &t.Platform, &t.BaseURL, &t.Token,
		&t.GitUserName, &t.GitUserEmail,
		&t.GPGKeyID, &t.GPGPrivateKey, &t.GPGPassphrase, &gpgEnabled,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("token not found: %s", id)
	}
	if err != nil {
		return nil, err
	}
	t.GPGEnabled = gpgEnabled != 0
	return &t, nil
}

func (s *PlatformTokenService) Delete(id string) error {
	res, err := s.db.Exec(`DELETE FROM platform_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("token not found: %s", id)
	}
	return nil
}

// GPGSigningMaterial returns the decrypted GPG material for a token.
//
// Returns enabled=false, keyID="", armoredKey="", passphrase="", err=nil
// when the token is missing, has GPG disabled, or has no stored key —
// callers should treat that as "signing not configured" and continue
// without signing, never as a fatal error. Decryption failures (master
// key missing, ciphertext corruption) DO surface as err so the caller
// can show the user a meaningful "main key lost" / "corrupt ciphertext"
// message instead of silently producing unsigned commits.
func (s *PlatformTokenService) GPGSigningMaterial(tokenID string) (enabled bool, keyID, armoredKey, passphrase string, err error) {
	if tokenID == "" {
		return false, "", "", "", nil
	}
	var (
		ekey      string
		epass     string
		gpgEnable int
	)
	row := s.db.QueryRow(
		`SELECT gpg_enabled, gpg_key_id, gpg_private_key, gpg_passphrase
		 FROM platform_tokens WHERE id = ?`, tokenID,
	)
	if err := row.Scan(&gpgEnable, &keyID, &ekey, &epass); err != nil {
		if err == sql.ErrNoRows {
			return false, "", "", "", nil
		}
		return false, "", "", "", err
	}
	if gpgEnable == 0 {
		return false, keyID, "", "", nil
	}
	if ekey == "" {
		// Switch is on but no key uploaded yet — same "not configured" code
		// path as the missing-row case so the caller logs a single warning.
		return false, keyID, "", "", nil
	}
	armored, err := secret.Decrypt(ekey)
	if err != nil {
		return true, keyID, "", "", fmt.Errorf("decrypt gpg private key: %w", err)
	}
	var plainPass string
	if epass != "" {
		plainPass, err = secret.Decrypt(epass)
		if err != nil {
			return true, keyID, armored, "", fmt.Errorf("decrypt gpg passphrase: %w", err)
		}
	}
	return true, keyID, armored, plainPass, nil
}

// UpdateGPGKeyID back-fills the 16-hex key id after a successful runtime
// import so the UI can echo it back. Errors here are non-fatal — the
// provision step still succeeded and signing will work; only the
// "key id" badge in the token list will stay empty until the next save.
func (s *PlatformTokenService) UpdateGPGKeyID(tokenID, keyID string) error {
	if tokenID == "" {
		return fmt.Errorf("token id is empty")
	}
	_, err := s.db.Exec(
		`UPDATE platform_tokens SET gpg_key_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		keyID, tokenID,
	)
	return err
}

// --- helpers ------------------------------------------------------------

// encryptOptional returns "" unchanged (empty GPG fields are stored as
// empty strings, never encrypted) and Encrypts anything non-empty.
func encryptOptional(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return secret.Encrypt(plain)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
