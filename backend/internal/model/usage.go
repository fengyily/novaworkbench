package model

import "time"

// TokenUsage records the LLM token consumption of one claude CLI / HTTP LLM
// invocation. One row per invocation (a multi-turn chat produces one row per
// turn). requirement_id is empty for project-level steps like PR review.
// ClaudeConfigID is the config (platform) that was active when the request ran;
// Currency is a snapshot of that config's currency so cost display survives a
// config deletion. Unit prices are NOT snapshotted — they are recomputed from
// the config's current model entries at query time.
type TokenUsage struct {
	ID                  string    `json:"id"`
	RequirementID       string    `json:"requirement_id"`
	ProjectID           string    `json:"project_id"`
	JobID               string    `json:"job_id"`
	Step                string    `json:"step"`
	Model               string    `json:"model"`
	ClaudeConfigID      string    `json:"claude_config_id"`
	Currency            string    `json:"currency"`
	InputTokens         int       `json:"input_tokens"`
	OutputTokens        int       `json:"output_tokens"`
	CacheCreationTokens int       `json:"cache_creation_tokens"`
	CacheReadTokens     int       `json:"cache_read_tokens"`
	Meta                string    `json:"meta"` // JSON string; review carries {pr_number,pr_title,branch}
	CreatedAt           time.Time `json:"created_at"`
}

// CostItem is a cost amount in one currency. Aggregations that span platforms
// with different currencies return one item per currency — amounts are never
// summed across currencies (no FX conversion). Most aggregates have one item.
type CostItem struct {
	Currency string  `json:"currency"`
	Amount   float64 `json:"amount"`
}

// UsageTotals is a sum of token counts across a set of rows, plus the
// recomputed cost grouped by currency.
type UsageTotals struct {
	InputTokens         int        `json:"input_tokens"`
	OutputTokens        int        `json:"output_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens"`
	CacheReadTokens     int        `json:"cache_read_tokens"`
	Costs               []CostItem `json:"costs"`
}

// TotalInput is input_tokens + cache_creation + cache_read — the billed
// input cost (cache reads/creations are charged as input tokens).
func (t UsageTotals) TotalInput() int {
	return t.InputTokens + t.CacheCreationTokens + t.CacheReadTokens
}

// StepUsage aggregates token usage for one step of a requirement (split by
// model so a step that ran on multiple models shows one row per model).
// Summaries carries the per-invocation "summary" text lifted from
// token_usage.meta (e.g. each 追加调整 request's first 200 chars), in the
// natural SQL order. Empty for steps that don't record a summary.
type StepUsage struct {
	Step                string     `json:"step"`
	Label               string     `json:"label"`
	Model               string     `json:"model"`
	InputTokens         int        `json:"input_tokens"`
	OutputTokens        int        `json:"output_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens"`
	CacheReadTokens     int        `json:"cache_read_tokens"`
	Count               int        `json:"count"`
	Costs               []CostItem `json:"costs"`
	Summaries           []string   `json:"summaries,omitempty"`
}

// ReqUsage aggregates token usage for one requirement (excludes review rows).
type ReqUsage struct {
	RequirementID       string     `json:"requirement_id"`
	InputTokens         int        `json:"input_tokens"`
	OutputTokens        int        `json:"output_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens"`
	CacheReadTokens     int        `json:"cache_read_tokens"`
	Costs               []CostItem `json:"costs"`
}

// ModelUsage aggregates token usage for one model within a project (excludes
// review rows). Costs are grouped by currency because the same model can run on
// different platforms with different currencies.
type ModelUsage struct {
	Model               string     `json:"model"`
	InputTokens         int        `json:"input_tokens"`
	OutputTokens        int        `json:"output_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens"`
	CacheReadTokens     int        `json:"cache_read_tokens"`
	Costs               []CostItem `json:"costs"`
}

// DailyUsage aggregates token usage for one calendar day (YYYY-MM-DD, derived
// from created_at via substr) within a project, excluding review rows.
type DailyUsage struct {
	Date                string     `json:"date"`
	InputTokens         int        `json:"input_tokens"`
	OutputTokens        int        `json:"output_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens"`
	CacheReadTokens     int        `json:"cache_read_tokens"`
	Costs               []CostItem `json:"costs"`
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
