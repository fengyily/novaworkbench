package service

import (
	"database/sql"
	"strconv"
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
)

// Setting keys for sub-task execution policy. All three are read on every
// orchestration tick / SubTaskRunner.Run entry, so a change takes effect
// without restarting the backend.
//
//   - subtask.concurrency: how many sub-tasks a SINGLE project may run at
//     once (see ProjectLimiter). Applied per project, not globally; the
//     process-wide ceiling stays on env NOVA_SUBTASK_CONCURRENCY.
//   - subtask.auto_retry:  whether a failed auto-orchestrated child is
//     re-armed automatically. Default OFF — recovery is a manual user action
//     (the card's 重做 / 继续 buttons) unless the user opts in.
//   - subtask.retry_max:   hard cap on automatic re-arms per child, mirroring
//     model.SummaryMaxAttempts, so a deterministically failing child can't
//     loop forever.
const (
	settingSubTaskConcurrency = "subtask.concurrency"
	settingSubTaskAutoRetry   = "subtask.auto_retry"
	settingSubTaskRetryMax    = "subtask.retry_max"
)

// Defaults applied when the keys are absent (fresh DB) or unparsable. They
// reproduce the pre-feature behaviour: strictly serial dispatch and no
// automatic retry.
const (
	DefaultSubTaskProjectConcurrency = 1
	DefaultSubTaskRetryMax           = 1
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

// SubTaskConfig returns the sub-task execution policy: the per-project
// concurrency cap, whether failed auto-orchestrated children are re-armed
// automatically, and the automatic-retry cap.
//
// Missing or malformed values fall back to the defaults (1 / false / 1) rather
// than erroring: the settings row is optional, and a broken value must not be
// able to stall every dispatch. A DB error IS returned — callers treat that as
// "keep the last known values".
func (s *SettingService) SubTaskConfig() (concurrency int, autoRetry bool, retryMax int, err error) {
	concurrency, autoRetry, retryMax = DefaultSubTaskProjectConcurrency, false, DefaultSubTaskRetryMax
	raw, err := s.Get(settingSubTaskConcurrency)
	if err != nil {
		return concurrency, autoRetry, retryMax, err
	}
	if n, cerr := strconv.Atoi(strings.TrimSpace(raw)); cerr == nil && n > 0 {
		concurrency = n
	}
	raw, err = s.Get(settingSubTaskAutoRetry)
	if err != nil {
		return concurrency, autoRetry, retryMax, err
	}
	if b, berr := strconv.ParseBool(strings.TrimSpace(raw)); berr == nil {
		autoRetry = b
	}
	raw, err = s.Get(settingSubTaskRetryMax)
	if err != nil {
		return concurrency, autoRetry, retryMax, err
	}
	if n, rerr := strconv.Atoi(strings.TrimSpace(raw)); rerr == nil && n >= 0 {
		retryMax = n
	}
	return concurrency, autoRetry, retryMax, nil
}

// SetSubTaskConfig persists the sub-task execution policy. Values are clamped
// here (concurrency ≥ 1, retryMax ≥ 0) so a bad payload can never be written
// in a shape the dispatch loops would have to defend against on every read.
// Nothing else happens on write: the tick loop and SubTaskRunner re-read the
// settings themselves, which is what makes the change hot without wiring the
// limiter into this service.
func (s *SettingService) SetSubTaskConfig(concurrency int, autoRetry bool, retryMax int) error {
	if concurrency < 1 {
		concurrency = 1
	}
	if retryMax < 0 {
		retryMax = 0
	}
	if err := s.Set(settingSubTaskConcurrency, strconv.Itoa(concurrency)); err != nil {
		return err
	}
	if err := s.Set(settingSubTaskAutoRetry, strconv.FormatBool(autoRetry)); err != nil {
		return err
	}
	return s.Set(settingSubTaskRetryMax, strconv.Itoa(retryMax))
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
