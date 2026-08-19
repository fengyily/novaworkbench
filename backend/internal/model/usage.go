package model

import "time"

// TokenUsage records the LLM token consumption of one claude CLI / HTTP LLM
// invocation. One row per invocation (a multi-turn chat produces one row per
// turn). requirement_id is empty for project-level steps like PR review.
type TokenUsage struct {
	ID                  string    `json:"id"`
	RequirementID       string    `json:"requirement_id"`
	ProjectID           string    `json:"project_id"`
	JobID               string    `json:"job_id"`
	Step                string    `json:"step"`
	Model               string    `json:"model"`
	InputTokens         int       `json:"input_tokens"`
	OutputTokens        int       `json:"output_tokens"`
	CacheCreationTokens int       `json:"cache_creation_tokens"`
	CacheReadTokens     int       `json:"cache_read_tokens"`
	Meta                string    `json:"meta"` // JSON string; review carries {pr_number,pr_title,branch}
	CreatedAt           time.Time `json:"created_at"`
}

// UsageTotals is a sum of token counts across a set of rows.
type UsageTotals struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
}

// TotalInput is input_tokens + cache_creation + cache_read — the billed
// input cost (cache reads/creations are charged as input tokens).
func (t UsageTotals) TotalInput() int {
	return t.InputTokens + t.CacheCreationTokens + t.CacheReadTokens
}

// StepUsage aggregates token usage for one step of a requirement.
type StepUsage struct {
	Step                string `json:"step"`
	Label               string `json:"label"`
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheCreationTokens int    `json:"cache_creation_tokens"`
	CacheReadTokens     int    `json:"cache_read_tokens"`
	Count               int    `json:"count"`
}

// ReqUsage aggregates token usage for one requirement (excludes review rows).
type ReqUsage struct {
	RequirementID       string `json:"requirement_id"`
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheCreationTokens int    `json:"cache_creation_tokens"`
	CacheReadTokens     int    `json:"cache_read_tokens"`
}

// ReviewUsage is one PR-review token record (project-level, not counted in the
// project total). Meta is parsed into PRNumber/PRTitle/Branch for display.
type ReviewUsage struct {
	ID            string    `json:"id"`
	JobID         string    `json:"job_id"`
	PRNumber      int       `json:"pr_number"`
	PRTitle       string    `json:"pr_title"`
	Branch        string    `json:"branch"`
	InputTokens   int       `json:"input_tokens"`
	OutputTokens  int       `json:"output_tokens"`
	CreatedAt     time.Time `json:"created_at"`
}
