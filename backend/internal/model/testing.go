package model

import "time"

// ── 自动化测试（Playwright Test Agents）─────────────────────────────────
// Models for the automated-testing module: one Playwright workspace per
// TestEnvironment, AI-authored TestPlans, batch TestRuns and their per-case
// TestRunCases. Column mapping is done by hand in service/testing_*.go via
// explicit column lists + positional Scan, matching the rest of this package
// (there are no `db:` tags anywhere — these are pure JSON DTOs).

// Init statuses for TestEnvironment. No CHECK constraint exists; the
// service layer owns this whitelist.
const (
	TestEnvStatusPending      = "pending"
	TestEnvStatusInitializing = "initializing"
	TestEnvStatusReady        = "ready"
	TestEnvStatusFailed       = "failed"
)

// TestPlan statuses. draft is the state between the row insert and the
// planner process starting; failed is terminal-for-now but retryable (a
// retry flips it back to planning).
const (
	TestPlanStatusDraft      = "draft"
	TestPlanStatusPlanning   = "planning"
	TestPlanStatusPlanned    = "planned"
	TestPlanStatusGenerating = "generating"
	TestPlanStatusGenerated  = "generated"
	TestPlanStatusFailed     = "failed"
)

// TestRun statuses. 'failed' means the suite ran and cases failed, while
// 'error' means the process itself broke (missing binary, unreadable
// report) — the distinction matters because only 'failed' can trigger the
// healer agent.
const (
	TestRunStatusRunning  = "running"
	TestRunStatusPassed   = "passed"
	TestRunStatusFailed   = "failed"
	TestRunStatusCanceled = "canceled"
	TestRunStatusError    = "error"
)

// TestRunCase statuses, mirroring the Playwright report's status strings.
// timedOut / interrupted are kept distinct from failed because their error
// text says something different ("Test timeout exceeded" vs an assertion).
const (
	TestCaseStatusPassed      = "passed"
	TestCaseStatusFailed      = "failed"
	TestCaseStatusFlaky       = "flaky"
	TestCaseStatusSkipped     = "skipped"
	TestCaseStatusTimedOut    = "timedOut"
	TestCaseStatusInterrupted = "interrupted"
)

// TestEnvironment is one configured Playwright workspace: a base URL to
// test against, a seed test used to authenticate/explore, encrypted env
// vars, and the state of its one-time initialization.
type TestEnvironment struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	// WorkspaceOverride points the generated specs at a directory inside
	// the user's own repo. Empty means the default
	// ~/.novaworkbench/testing/<project_id>/<id>/ directory.
	WorkspaceOverride string `json:"workspace_override"`
	// EnvVarsEnc is the AES-256-GCM blob produced by internal/secret. It is
	// never serialized — the JSON tag is "-" and the API exposes only
	// EnvVarsSet so the UI can show "configured" without leaking values.
	EnvVarsEnc string `json:"-"`
	// EnvVarsSet mirrors "EnvVarsEnc is non-empty", computed by the service
	// on read. Never persisted.
	EnvVarsSet bool   `json:"env_vars_set"`
	SeedTest   string `json:"seed_test"`
	Browsers   string `json:"browsers"`
	InitStatus string `json:"init_status"`
	InitError  string `json:"init_error"`
	// PlaywrightVersion is captured after init ("Version 1.56.1"). Recorded
	// so a user reporting "the healer doesn't work" can be checked against
	// the version the agents were generated for.
	PlaywrightVersion string `json:"playwright_version"`
	DefaultModel      string `json:"default_model"`
	// RetentionRuns caps how many runs (DB rows AND their on-disk
	// screenshot/trace directories) survive per environment.
	RetentionRuns int       `json:"retention_runs"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// DiskUsageBytes is computed on demand by the status endpoint, not
	// stored. Tagged omitempty so list responses stay small.
	DiskUsageBytes int64 `json:"disk_usage_bytes,omitempty"`
}

// EnvStatusResp is the payload of GET /api/testing/envs/{id}/status — the
// environment plus its on-disk footprint, which is what tells a user why
// their disk is filling up.
type EnvStatusResp struct {
	Env            *TestEnvironment `json:"env"`
	DiskUsageBytes int64            `json:"disk_usage_bytes"`
	// Workspace is the resolved absolute path actually in use (override or
	// default). Surfaced so a user can open the generated specs.
	Workspace string `json:"workspace"`
}

// TestPlan is one AI-authored test plan plus the spec files generated from
// it. plan_md is a copy of the on-disk specs/<slug>.md so the plan remains
// readable after the workspace is deleted.
type TestPlan struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	EnvID         string `json:"env_id"`
	RequirementID string `json:"requirement_id"`
	Title         string `json:"title"`
	Scenario      string `json:"scenario"`
	PlanPath      string `json:"plan_path"`
	PlanMD        string `json:"plan_md"`
	// SpecFiles is stored as a JSON array string in the DB; the service
	// decodes it into SpecFileList on read and re-encodes on write.
	SpecFiles string `json:"-"`
	// SpecFileList is the decoded form, populated by the service. This is
	// what the API actually returns.
	SpecFileList []string `json:"spec_files"`
	Status       string   `json:"status"`
	// ClaudeSessionID lets a follow-up conversation resume the planner's
	// session instead of re-exploring the app from scratch.
	ClaudeSessionID string    `json:"claude_session_id"`
	LastJobID       string    `json:"last_job_id"`
	Error           string    `json:"error"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// TestRun is one batch execution of one or more spec files.
type TestRun struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	EnvID     string `json:"env_id"`
	// Scope is the replayable selection: {"all":true} | {"plan_ids":[…]} |
	// {"spec_files":[…]}. Stored as a JSON string.
	Scope     string `json:"-"`
	ScopeJSON any    `json:"scope"`
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Passed    int    `json:"passed"`
	Failed    int    `json:"failed"`
	Flaky     int    `json:"flaky"`
	Skipped   int    `json:"skipped"`
	// HealerUsed is 0/1 in the DB (INTEGER), surfaced as a bool.
	HealerUsed bool `json:"healer_used"`
	// HealedFrom points at the run that failed before the healer agent ran.
	// Set on the VERIFICATION run, pointing back at its predecessor.
	HealedFrom string     `json:"healed_from"`
	ReportPath string     `json:"report_path"`
	JobID      string     `json:"job_id"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMs int        `json:"duration_ms"`
	Error      string     `json:"error"`
}

// TestRunCase is one test case within a run, parsed out of the Playwright
// JSON report.
type TestRunCase struct {
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	// File is the workspace-relative spec path, e.g. "tests/login.spec.ts".
	File  string `json:"file"`
	Title string `json:"title"`
	// Status is one of the TestCaseStatus* constants.
	Status     string `json:"status"`
	Error      string `json:"error"`
	DurationMs int    `json:"duration_ms"`
	// Attachments is the JSON array string as stored; AttachmentList is the
	// decoded form the API returns.
	Attachments    string           `json:"-"`
	AttachmentList []TestAttachment `json:"attachments"`
	// IssueURL / IssueNumber are backfilled when the failure is escalated to
	// a platform Issue; RequirementID when it is escalated to a local
	// requirement. Both being non-empty is what makes the buttons
	// idempotent (a second click would create a duplicate).
	IssueURL      string `json:"issue_url"`
	IssueNumber   int    `json:"issue_number"`
	RequirementID string `json:"requirement_id"`
}

// TestAttachment is one evidence file (screenshot, trace, video) produced
// by a test case. Path is RELATIVE to runs/<run_id>/ — absolute paths from
// the Playwright report are converted on ingest and anything escaping the
// run directory is dropped, so this value is always safe to join onto the
// run dir when serving.
type TestAttachment struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
}

// RunScope is the request/DB shape of a run's selection. Exactly one of the
// three fields should be set; the service resolves whichever is present
// into a concrete list of spec files.
type RunScope struct {
	All       bool     `json:"all,omitempty"`
	PlanIDs   []string `json:"plan_ids,omitempty"`
	SpecFiles []string `json:"spec_files,omitempty"`
}

// ── Request bodies ─────────────────────────────────────────────────────

// CreateTestEnvReq creates a test environment. EnvVars is a plain map that
// the service JSON-encodes and encrypts into env_vars_enc; it is never
// echoed back.
type CreateTestEnvReq struct {
	Name              string            `json:"name"`
	BaseURL           string            `json:"base_url"`
	WorkspaceOverride string            `json:"workspace_override"`
	SeedTest          string            `json:"seed_test"`
	Browsers          string            `json:"browsers"`
	DefaultModel      string            `json:"default_model"`
	RetentionRuns     int               `json:"retention_runs"`
	EnvVars           map[string]string `json:"env_vars"`
}

// UpdateTestEnvReq is a partial update — every field is optional and only
// the non-nil ones are written. EnvVars, when present, REPLACES the whole
// map (rather than merging), so the UI can send "these are all the vars".
type UpdateTestEnvReq struct {
	Name              *string            `json:"name"`
	BaseURL           *string            `json:"base_url"`
	WorkspaceOverride *string            `json:"workspace_override"`
	SeedTest          *string            `json:"seed_test"`
	Browsers          *string            `json:"browsers"`
	DefaultModel      *string            `json:"default_model"`
	RetentionRuns     *int               `json:"retention_runs"`
	EnvVars           *map[string]string `json:"env_vars"`
}

// CreatePlanReq creates a plan AND kicks off the planner agent, returning
// a job_id the frontend subscribes to. Model / ClaudeConfigID bind the run
// to a specific model the way the review toolbar does.
type CreatePlanReq struct {
	Title          string `json:"title"`
	Scenario       string `json:"scenario"`
	EnvID          string `json:"env_id"`
	RequirementID  string `json:"requirement_id"`
	Model          string `json:"model"`
	ClaudeConfigID string `json:"claude_config_id"`
}

// UpdatePlanReq carries a human-edited plan document; the handler mirrors
// it back to specs/<slug>.md so the generator agent sees the edit.
type UpdatePlanReq struct {
	PlanMD  string `json:"plan_md"`
	Title   string `json:"title"`
	Comment string `json:"comment"`
}

// StartRunReq launches a batch run. Healer requests the heal-then-verify
// chain for any cases that fail.
type StartRunReq struct {
	Scope          RunScope `json:"scope"`
	Healer         bool     `json:"healer"`
	Model          string   `json:"model"`
	ClaudeConfigID string   `json:"claude_config_id"`
}

// CreateCaseIssueReq is the optional body for escalating a failing case to
// a platform Issue.
type CreateCaseIssueReq struct {
	// Labels are best-effort — Gitea only accepts numeric label ids, so its
	// implementation drops them and prefixes the body with [autotest].
	Labels []string `json:"labels"`
}
