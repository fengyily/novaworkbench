package service

import (
	"database/sql"
	"strings"
	"time"
)

// Setting keys for the Claude CLI configuration. Stored in the settings table
// and injected as env vars when the gateway runs `claude`.
const (
	settingClaudeAuthToken = "claude.anthropic_auth_token"
	settingClaudeBaseURL   = "claude.anthropic_base_url"
)

type SettingService struct {
	db *sql.DB
}

func NewSettingService(db *sql.DB) *SettingService {
	return &SettingService{db: db}
}

// Get returns the value for a key, or "" if the key does not exist.
func (s *SettingService) Get(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// Set upserts a key/value pair.
func (s *SettingService) Set(key, value string) error {
	_, err := s.db.Exec(
		"INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = ?",
		key, value, time.Now(), value, time.Now())
	return err
}

// All returns every setting as a map.
func (s *SettingService) All() (map[string]string, error) {
	rows, err := s.db.Query("SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// ClaudeConfig returns the raw auth token + base URL configured for the claude
// CLI. For internal use (gateway env injection) — the token is secret.
func (s *SettingService) ClaudeConfig() (authToken, baseURL string, err error) {
	if authToken, err = s.Get(settingClaudeAuthToken); err != nil {
		return "", "", err
	}
	if baseURL, err = s.Get(settingClaudeBaseURL); err != nil {
		return "", "", err
	}
	return authToken, baseURL, nil
}

// SetClaudeConfig upserts the auth token + base URL. An empty auth token means
// "keep the existing value" (so the UI can save base-URL-only edits without
// knowing the secret). An empty base URL clears it (use the API default).
func (s *SettingService) SetClaudeConfig(authToken, baseURL string) error {
	if authToken != "" {
		if err := s.Set(settingClaudeAuthToken, authToken); err != nil {
			return err
		}
	}
	return s.Set(settingClaudeBaseURL, baseURL)
}

// ClearClaudeAuthToken removes the stored auth token, so the claude CLI falls
// back to its default authentication (inherited env / login).
func (s *SettingService) ClearClaudeAuthToken() error {
	return s.Set(settingClaudeAuthToken, "")
}

// ClaudeEnvVars implements llm.EnvProvider so the gateway can pull the current
// token + base URL at command-build time without importing service types.
func (s *SettingService) ClaudeEnvVars() (authToken, baseURL string, err error) {
	return s.ClaudeConfig()
}

// MaskToken returns a redacted preview of a secret token for API responses.
// Long tokens show the first 4 and last 4 characters; short ones are fully
// masked. An empty token returns "".
func MaskToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
