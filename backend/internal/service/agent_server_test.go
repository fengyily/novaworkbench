package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/secret"
	gossh "golang.org/x/crypto/ssh"
)

// startFakeSSHServer spins a TCP listener that completes the SSH handshake
// against the supplied host key + signer (or rejects when reject=true).
// Returns the listen address (host:port) so TestConnection can target it.
// The server-side keeps the channel/req pumps alive until the test cleanup
// closes the listener — without that, the client's NewClientConn sees
// "use of closed network connection" because the server side closed the
// socket immediately after NewServerConn returned.
func startFakeSSHServer(t *testing.T, signer gossh.Signer, reject bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var hostKey gossh.Signer
	if signer != nil {
		hostKey = signer
	}
	config := &gossh.ServerConfig{
		PublicKeyCallback: func(conn gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			if reject {
				return nil, fmt.Errorf("rejected")
			}
			return &gossh.Permissions{}, nil
		},
	}
	if hostKey != nil {
		config.AddHostKey(hostKey)
	}

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	go func() {
		for {
			rawConn, err := ln.Accept()
			if err != nil {
				return // test finished / listener closed
			}
			go func(c net.Conn) {
				defer c.Close()
				conn, chans, reqs, err := gossh.NewServerConn(c, config)
				if err != nil {
					return // handshake failed (expected for the reject case)
				}
				// Drain channels and requests until the test ends or the
				// client disconnects. This is enough for the client's
				// NewClientConn to complete successfully without seeing
				// "use of closed network connection".
				go func() {
					for newCh := range chans {
						_, _, _ = newCh.Accept()
					}
				}()
				for req := range reqs {
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
				}
				_ = conn
				_ = done
			}(rawConn)
		}
	}()

	return ln.Addr().String()
}

// generateSignerPair returns a fresh RSA signer used as the SSH host key
// for the fake server, plus the private-key PEM bytes in OpenSSH format (the
// value we'll encrypt and store in agent_servers.auth_value). We use
// ssh.MarshalPrivateKeyWithPassphrase with an empty passphrase so the output
// is the canonical "-----BEGIN OPENSSH PRIVATE KEY-----" PEM that
// ssh.ParsePrivateKey will accept.
func generateSignerPair(t *testing.T) (host gossh.Signer, pemBytes []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 1024) // small for speed — SSH handshake only
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	host, err = gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey host: %v", err)
	}
	pemBlock, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	var buf bytes.Buffer
	if err := pem.Encode(&buf, pemBlock); err != nil {
		t.Fatalf("pem.Encode: %v", err)
	}
	pemBytes = buf.Bytes()
	return
}

// secret.Decrypt returns an error if Init was not called. TestMain inits it
// once for the whole service-package test binary.
func TestMain(m *testing.M) {
	_ = secret.Init()
	m.Run()
}

func insertAgentServer(t *testing.T, d interface {
	Exec(string, ...any) (sql.Result, error)
}, name, host, username, authType, authCipher string, port int) string {
	t.Helper()
	id := fmt.Sprintf("agent_%d", time.Now().UnixNano())
	_, err := d.Exec(
		`INSERT INTO agent_servers (id, name, host, port, username, auth_type, auth_value, auth_value_algo, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 'aes-gcm', 'unknown', ?, ?)`,
		id, name, host, port, username, authType, authCipher, time.Now(), time.Now(),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

// A1
func TestAgentServerService_TestConnection_Success(t *testing.T) {
	hostSigner, pemBytes := generateSignerPair(t)
	addr := startFakeSSHServer(t, hostSigner, false)
	parts := strings.Split(addr, ":")
	host := parts[0]
	var port int
	fmt.Sscanf(parts[1], "%d", &port)

	cipher, err := secret.Encrypt(string(pemBytes))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	db := newTestDB(t)
	id := insertAgentServer(t, db, "fake", host, "root", "key", cipher, port)

	svc := NewAgentServerService(db)
	version, err := svc.TestConnection(context.Background(), id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(version, "connected to") {
		t.Errorf("expected 'connected to ...' in version, got %q", version)
	}
}

// A2
func TestAgentServerService_TestConnection_AuthFailed(t *testing.T) {
	hostSigner, _ := generateSignerPair(t)
	addr := startFakeSSHServer(t, hostSigner, true)
	parts := strings.Split(addr, ":")
	host := parts[0]
	var port int
	fmt.Sscanf(parts[1], "%d", &port)

	cipher, err := secret.Encrypt("doesnt-matter-rejected-anyway")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	db := newTestDB(t)
	id := insertAgentServer(t, db, "fake", host, "root", "key", cipher, port)

	svc := NewAgentServerService(db)
	_, err = svc.TestConnection(context.Background(), id)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.HasPrefix(err.Error(), "SSH_CONNECT_FAILED:") {
		t.Errorf("missing SSH_CONNECT_FAILED prefix: %q", err.Error())
	}
}

// A3
func TestAgentServerService_TestConnection_DecryptError(t *testing.T) {
	db := newTestDB(t)
	id := insertAgentServer(t, db, "fake", "127.0.0.1", "root", "key", "not-valid-ciphertext", 22)

	svc := NewAgentServerService(db)
	_, err := svc.TestConnection(context.Background(), id)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.HasPrefix(err.Error(), "AUTH_DECRYPT_FAILED:") {
		t.Errorf("missing AUTH_DECRYPT_FAILED prefix: %q", err.Error())
	}
}

// A4
func TestAgentServerService_TestConnection_NotFound(t *testing.T) {
	svc := NewAgentServerService(newTestDB(t))
	_, err := svc.TestConnection(context.Background(), "agent_does_not_exist")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.HasPrefix(err.Error(), "AGENT_NOT_FOUND:") {
		t.Errorf("missing AGENT_NOT_FOUND prefix: %q", err.Error())
	}
}