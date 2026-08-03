package model

import "time"

// Role is an AI role in the wizard pipeline (analyst / architect / developer,
// extensible). Its SystemPrompt and Model drive the claude CLI flags
// (--system-prompt / --model) for the role's stage.
type Role struct {
	ID           string    `json:"id"`
	Key          string    `json:"key"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	SystemPrompt string    `json:"system_prompt"`
	Model        string    `json:"model"`
	SortOrder    int       `json:"sort_order"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// UpdateRoleReq is the editable subset of a role (prompt + model only — the
// persona identity fields are not user-editable).
type UpdateRoleReq struct {
	SystemPrompt string `json:"system_prompt"`
	Model        string `json:"model"`
}
