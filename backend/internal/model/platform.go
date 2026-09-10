package model

import "time"

// PlatformToken describes one row of the platform_tokens table. Sensitive
// fields (Token, GPGPrivateKey, GPGPassphrase) carry json:"-" so they are
// excluded from JSON serialization even if a handler forgets to call Redact.
// Redact() is the second half of that defense — handlers should call it
// before writing any token-shaped payload to the wire so a future field
// added without json:"-" still gets blanked.
type PlatformToken struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Platform     string    `json:"platform"`
	BaseURL      string    `json:"base_url"`
	Token        string    `json:"token,omitempty"`
	GitUserName  string    `json:"git_user_name"`
	GitUserEmail string    `json:"git_user_email"`
	// GPG signing material. The private key + passphrase are AES-256-GCM
	// ciphertext (internal/secret); KeyID is the public 16-hex id reported
	// back to the UI after the runtime provision step imports the key.
	// GPGEnabled is the user-controlled switch — when false the ciphertexts
	// are retained but no signing happens.
	GPGKeyID      string `json:"gpg_key_id"`
	GPGEnabled    bool   `json:"gpg_enabled"`
	GPGPrivateKey string `json:"-"`
	GPGPassphrase string `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Redact blanks every secret-bearing field on the token and returns it for
// convenient chaining. Safe on a nil receiver (returns nil) so handlers can
// call `tok.Redact()` without a guard after a possibly-failing Get. The
// point is to keep the "never echo secrets" rule in one obvious place rather
// than scattered across every handler.
func (t *PlatformToken) Redact() *PlatformToken {
	if t == nil {
		return nil
	}
	t.Token = ""
	t.GPGPrivateKey = ""
	t.GPGPassphrase = ""
	return t
}
