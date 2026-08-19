package service

import (
	"database/sql"
	"os"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
)

// Setting keys for the direct HTTP LLM channel (OpenAI-compatible, e.g.
// DeepSeek). Used ONLY for lightweight tasks like requirement title
// distillation — not the claude CLI pipeline. base_url + api_key both must
// be set for the channel to activate; model may be empty (provider default).
//
// The Claude CLI configuration (auth token / base URL) no longer lives here;
// it is stored in the dedicated claude_configs table (see ClaudeConfigService).
const (
	settingLLMBaseURL = "llm.base_url"
	settingLLMAPIKey  = "llm.api_key"
	settingLLMModel   = "llm.model"

	// settingCodingTimeout is the max wall-clock duration for a single coding
	// task (start-coding / adjust-coding), stored as a Go duration string
	// ("2h", "90m"). Editable from the Claude settings page; applies to the
	// next task without a restart.
	settingCodingTimeout = "llm.coding_timeout"
)

// SettingService persists arbitrary key/value settings. The Claude CLI
// configuration now lives in the dedicated claude_configs table (see
// ClaudeConfigService); this service still owns the direct HTTP LLM channel
// settings used for lightweight tasks like requirement title distillation.
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

// CodingTimeout returns the coding-task timeout, resolved from the settings
// table first, then the CLAUDE_CODING_TIMEOUT env, then a 2h default. Always
// returns a positive duration — a DB read error or unparseable value falls
// through to the next source rather than failing the caller.
func (s *SettingService) CodingTimeout() time.Duration {
	if v, err := s.Get(settingCodingTimeout); err == nil && v != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > 0 {
			return d
		}
	}
	if t := os.Getenv("CLAUDE_CODING_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(t)); err == nil && d > 0 {
			return d
		}
	}
	return 2 * time.Hour
}

// SetCodingTimeout persists the coding-task timeout as a duration string.
func (s *SettingService) SetCodingTimeout(d time.Duration) error {
	return s.Set(settingCodingTimeout, d.String())
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
