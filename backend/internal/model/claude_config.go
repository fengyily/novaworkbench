package model

import "time"

// ClaudeConfig is one saved Claude CLI configuration (auth token + base URL +
// the model list this gateway/account offers). At most one row is active at a
// time (is_active=1); the active row's env vars are injected into every claude
// subprocess, and its default_model is pushed into all roles on activation.
//
// AuthToken is never JSON-serialized — API responses build a separate masked
// shape (see handler.ClaudeConfigItem).
type ClaudeConfig struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	BaseURL      string    `json:"base_url"`
	AuthToken    string    `json:"-"`
	Models       []string  `json:"models"`
	DefaultModel string    `json:"default_model"`
	IsActive     bool      `json:"is_active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ClaudeConfigItem is the masked API shape for list/detail responses. The auth
// token is never returned in full — only whether one is set and a redacted
// preview — so the UI can display state without leaking the secret.
type ClaudeConfigItem struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	BaseURL           string   `json:"base_url"`
	AuthTokenSet      bool     `json:"auth_token_set"`
	AuthTokenPreview  string   `json:"auth_token_preview"`
	Models            []string `json:"models"`
	DefaultModel      string   `json:"default_model"`
	IsActive          bool     `json:"is_active"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
}

// CreateClaudeConfigReq is the POST body for a new configuration.
type CreateClaudeConfigReq struct {
	Name         string   `json:"name"`
	BaseURL      string   `json:"base_url"`
	AuthToken    string   `json:"auth_token"`
	Models       []string `json:"models"`
	DefaultModel string   `json:"default_model"`
}

// UpdateClaudeConfigReq is the PUT body for editing a configuration. An empty
// AuthToken means "keep the existing secret"; ClearToken=true explicitly
// removes it. Models is a pointer so nil ("field omitted") means "leave models
// unchanged" while an empty slice means "clear the list".
type UpdateClaudeConfigReq struct {
	Name         string   `json:"name"`
	BaseURL      string   `json:"base_url"`
	AuthToken    string   `json:"auth_token"`
	ClearToken   bool     `json:"clear_token"`
	Models       []string `json:"models"`
	DefaultModel string   `json:"default_model"`
}
