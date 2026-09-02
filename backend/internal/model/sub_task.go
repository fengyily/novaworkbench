package model

import "time"

// SubTask is a manually-triggered child agent that runs under a requirement's
// developing stage. Each sub-task forks the requirement's coding_session_id
// (the "main agent" established by start-coding), inherits the full context
// (project structure, design docs, prior conversation), and runs in its own
// claude process with its own session id. The artifact Markdown is persisted
// on completion so the report survives JobStore ring-buffer eviction.
//
// Sub-tasks are intentionally NOT coupled to any single job: the JobStore is
// in-memory only and may evict the live job; the artifact column is the
// durable record.
type SubTask struct {
	ID              string     `json:"id"`
	RequirementID   string     `json:"requirement_id"`
	Title           string     `json:"title"`
	Prompt          string     `json:"prompt"`
	// Status mirrors the JobStore job lifecycle: pending → running → done | error.
	// The handler transitions pending→running when the goroutine starts, and
	// running→done|error in the deferred Finish call.
	Status          string     `json:"status"`
	// SessionID is the new claude session id pre-minted before the child agent
	// spawns (so a crash between pre-mint and Start still recovers). It is the
	// id passed to --session-id with --fork-session.
	SessionID       string     `json:"session_id"`
	// SourceSessionID is the parent session being forked — typically
	// requirements.coding_session_id. Empty when there is no parent (e.g. no
	// start-coding has run yet); the handler falls back to design_session_id.
	SourceSessionID string     `json:"source_session_id"`
	// JobID is the in-memory JobStore job id; empty once the ring buffer has
	// evicted the job (the artifact is still available on the row).
	JobID           string     `json:"job_id"`
	// Artifact is the final Markdown report written when the child agent
	// finishes. Header (title + prompt + model + timestamps) is prepended by
	// the wizard handler so the report reads standalone. Empty when the
	// sub-task hasn't completed yet.
	Artifact        string     `json:"artifact"`
	// Model is the effective --model value dispatched to the child agent.
	// Mirrors requirements.developer_model / requirements.designer_model;
	// stored per-row so the SubTaskPanel can show "默认模型" badges without
	// joining back to the role table.
	Model           string     `json:"model"`
	// InputTokens / OutputTokens / Cache* mirror the token_usage columns the
	// wizard already records under step="sub_task". Persisted on the row so
	// the SubTaskCard header can render "🪙 12.4k in / 3.1k out" inline
	// without a second SELECT. Zero on a sub-task that hasn't finished yet
	// (the parent UI shows a spinner / "⏳ 计量中…" instead of the badge).
	InputTokens         int        `json:"input_tokens"`
	OutputTokens        int        `json:"output_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens"`
	CacheReadTokens     int        `json:"cache_read_tokens"`
	// CostCents is the resolved cost (config unit-price × tokens, same
	// formula the dashboard uses) in cents of the platform's currency.
	// Zero when the model has no pricing configured yet — the UI shows "—"
	// rather than "$0.00" so the user knows pricing wasn't computed.
	CostCents           int        `json:"cost_cents"`
	// DurationSeconds is wall-clock time from MarkRunning to Finish. Zero
	// while the sub-task is still running; the UI starts a live counter once
	// it sees status=running. Stamped at Finish so a long-finished card
	// keeps a stable "耗时 4m12s" even after JobStore eviction.
	DurationSeconds     int        `json:"duration_seconds"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

// Sub-task status values, kept as plain string constants so they line up with
// the values the SQL column carries and the frontend type union accepts.
const (
	SubTaskStatusPending = "pending"
	SubTaskStatusRunning = "running"
	SubTaskStatusDone    = "done"
	SubTaskStatusError   = "error"
)

// SubTaskTokens is the four-field token view the wizard handler hands the
// Finish method so it can persist the terminal result-event counts without
// re-extracting them from the stream-json log line. Pulled into its own
// struct so adding more token fields (cache_creation_details, etc.) is a
// one-line change here instead of an evolving Finish() signature.
type SubTaskTokens struct {
	Input          int
	Output         int
	CacheCreation  int
	CacheRead      int
}
