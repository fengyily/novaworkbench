package service

import (
	"database/sql"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
)

// Setting keys for the Claude CLI configuration. Stored in the settings table
// and injected as env vars when the gateway runs `claude`.
const (
	settingClaudeAuthToken = "claude.anthropic_auth_token"
	settingClaudeBaseURL   = "claude.anthropic_base_url"
)

// Setting keys for the direct HTTP LLM channel (OpenAI-compatible, e.g.
// DeepSeek). Used ONLY for lightweight tasks like requirement title
// distillation — not the claude CLI pipeline. base_url + api_key both must
// be set for the channel to activate; model may be empty (provider default).
const (
	settingLLMBaseURL = "llm.base_url"
	settingLLMAPIKey  = "llm.api_key"
	settingLLMModel   = "llm.model"
)

type SettingService struct {
	db *db.DB
}

func NewSettingService(db *db.DB) *SettingService {
	return &SettingService{db: db}
}

// Get returns the value for a key, or "" if the key does not exist.
func (s *SettingService) Get(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE "+s.db.Ident("key")+" = ?", key).Scan(&v)
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
		"INSERT INTO settings ("+s.db.Ident("key")+", value, updated_at) VALUES (?, ?, ?)"+
			s.db.OnConflict(s.db.Ident("key"), "value = ?, updated_at = ?"),
		key, value, time.Now(), value, time.Now())
	return err
}

// All returns every setting as a map.
func (s *SettingService) All() (map[string]string, error) {
	rows, err := s.db.Query("SELECT " + s.db.Ident("key") + ", value FROM settings")
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

// LLMConfig returns the direct HTTP LLM channel configuration (base URL, API
// key, model) for title distillation. For internal use by the gateway — the
// API key is secret.
func (s *SettingService) LLMConfig() (baseURL, apiKey, model string, err error) {
	if baseURL, err = s.Get(settingLLMBaseURL); err != nil {
		return "", "", "", err
	}
	if apiKey, err = s.Get(settingLLMAPIKey); err != nil {
		return "", "", "", err
	}
	if model, err = s.Get(settingLLMModel); err != nil {
		return "", "", "", err
	}
	return baseURL, apiKey, model, nil
}

// SetLLMConfig upserts the base URL + API key + model. An empty api key means
// "keep the existing secret" (so the UI can save base-URL/model-only edits
// without knowing the key). Empty base URL / model clears those fields.
func (s *SettingService) SetLLMConfig(baseURL, apiKey, model string) error {
	if apiKey != "" {
		if err := s.Set(settingLLMAPIKey, apiKey); err != nil {
			return err
		}
	}
	if err := s.Set(settingLLMBaseURL, baseURL); err != nil {
		return err
	}
	return s.Set(settingLLMModel, model)
}

// ClearLLMAPIKey removes the stored API key, deactivating the direct HTTP LLM
// channel (base URL + api key both must be set for it to activate).
func (s *SettingService) ClearLLMAPIKey() error {
	return s.Set(settingLLMAPIKey, "")
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
