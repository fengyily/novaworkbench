package model

import "time"

// ModelEntry is one model offered by a Claude config (platform), with its
// per-million-token input/output unit prices. The models column stores an array
// of these; a legacy string-array value is decoded as entries with 0 prices.
type ModelEntry struct {
	Model       string  `json:"model"`
	InputPrice  float64 `json:"input_price"`
	OutputPrice float64 `json:"output_price"`
}

// ClaudeConfig is one saved Claude CLI configuration (auth token + base URL +
// the model list + per-model prices this gateway/account offers). At most one
// row is active at a time (is_active=1); the active row's env vars are injected
// into every claude subprocess, and its default_model is pushed into all roles
// on activation. Currency is the accounting unit for this platform (USD/CNY).
//
// AuthToken is never JSON-serialized — API responses build a separate masked
// shape (see handler.ClaudeConfigItem).
type ClaudeConfig struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	BaseURL      string       `json:"base_url"`
	AuthToken    string       `json:"-"`
	Models       []ModelEntry `json:"models"`
	DefaultModel string       `json:"default_model"`
	Currency     string       `json:"currency"`
	IsActive     bool         `json:"is_active"`
	CreatedAt    time.Time    `json:"created_at"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

// ClaudeConfigItem is the masked API shape for list/detail responses. The auth
// token is never returned in full — only whether one is set and a redacted
// preview — so the UI can display state without leaking the secret. Models are
// returned as priced entries so the settings UI can edit per-model unit prices.
type ClaudeConfigItem struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	BaseURL          string       `json:"base_url"`
	AuthTokenSet     bool         `json:"auth_token_set"`
	AuthTokenPreview string       `json:"auth_token_preview"`
	Models           []ModelEntry `json:"models"`
	DefaultModel     string       `json:"default_model"`
	Currency         string       `json:"currency"`
	IsActive         bool         `json:"is_active"`
	CreatedAt        string       `json:"created_at"`
	UpdatedAt        string       `json:"updated_at"`
}

// CreateClaudeConfigReq is the POST body for a new configuration.
type CreateClaudeConfigReq struct {
	Name         string       `json:"name"`
	BaseURL      string       `json:"base_url"`
	AuthToken    string       `json:"auth_token"`
	Models       []ModelEntry `json:"models"`
	DefaultModel string       `json:"default_model"`
	Currency     string       `json:"currency"`
}

// UpdateClaudeConfigReq is the PUT body for editing a configuration. An empty
// AuthToken means "keep the existing secret"; ClearToken=true explicitly
// removes it. A nil Models means "leave models unchanged" while an empty slice
// means "clear the list".
type UpdateClaudeConfigReq struct {
	Name         string       `json:"name"`
	BaseURL      string       `json:"base_url"`
	AuthToken    string       `json:"auth_token"`
	ClearToken   bool         `json:"clear_token"`
	Models       []ModelEntry `json:"models"`
	DefaultModel string       `json:"default_model"`
	Currency     string       `json:"currency"`
}
