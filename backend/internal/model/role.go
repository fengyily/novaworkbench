package model

import "time"

// Role is an AI role in the wizard pipeline (analyst / architect / developer,
// extensible). Its SystemPrompt drives the --system-prompt flag, Model drives
// the --model flag, and ClaudeConfigID pins the role to a specific Claude
// configuration (auth token + base URL). When ClaudeConfigID is empty, the
// role runs against the global active config (legacy single-config behavior).
type Role struct {
	ID             string    `json:"id"`
	Key            string    `json:"key"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	SystemPrompt   string    `json:"system_prompt"`
	Model          string    `json:"model"`
	// ClaudeConfigID is the row id from claude_configs that the role is bound
	// to. The gateway looks up this row at run time and injects its
	// ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN into the claude subprocess
	// (instead of the global active config's). Empty = the role uses the
	// global active config, so old rows keep their pre-binding behavior.
	ClaudeConfigID string    `json:"claude_config_id"`
	SortOrder      int       `json:"sort_order"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// UpdateRoleReq is the editable subset of a role (prompt + model + binding).
// Persona identity fields (key, name, description) and the enable flag are
// not user-editable. ClaudeConfigID="" unsets the binding (fall back to the
// global active config).
type UpdateRoleReq struct {
	SystemPrompt   string `json:"system_prompt"`
	Model          string `json:"model"`
	ClaudeConfigID string `json:"claude_config_id"`
}
