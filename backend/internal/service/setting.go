package service

import (
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
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
	// settingSubTaskFixSessionMode selects how a Review's fix sub-tasks
	// obtain their context:
	//
	//   - "with_context" (default): a NEW claude session that receives a
	//     curated context block (requirement + design docs + the parent's
	//     recent turns + sibling digests, capped at 32 KB).
	//   - "fork": the historical behaviour — --resume the developer's
	//     session, inheriting its FULL conversation.
	//
	// Why the default changed: forking the developer session handed the
	// fixer 95 KB of prompt and 197 tool calls, and the resulting context
	// pollution is what made the model restate the same conclusion over and
	// over. "fork" stays available as the escape hatch for the (rare) case
	// where a fixer genuinely needs the developer's full history.
	settingSubTaskFixSessionMode = "subtask.fix_session_mode"
)

// Session modes for subtask.fix_session_mode.
const (
	SubTaskFixSessionModeWithContext = "with_context"
	SubTaskFixSessionModeFork        = "fork"
)

// Defaults applied when the keys are absent (fresh DB) or unparsable. They
// reproduce the pre-feature behaviour: strictly serial dispatch and no
// automatic retry.
const (
	DefaultSubTaskProjectConcurrency = 1
	DefaultSubTaskRetryMax           = 1
)

// Setting keys + defaults for the design-stage hard git sync (see service/
// git_sync.go). Read on every architect-design entry, so a change takes effect
// without restarting the backend — same hot-reload pattern as the subtask
// settings above.
//
//   - git.sync_timeout_seconds: how long the architect-design stage may spend
//     on the local hard sync (status check → fetch → ff-only merge → resolve
//     base SHA). Bounded below to keep a misconfiguration from being useless
//     (≥ 10s) and above to bound the worst-case HTTP / pre-spawn wait
//     (≤ 600s, accommodates a slow first fetch on a large repo).
const (
	settingGitSyncTimeout = "git.sync_timeout_seconds"
	// SettingAgentWorktreeRoot is the remote Agent Server work root ("" = default).
	SettingAgentWorktreeRoot = "agent.worktree_root"
)

// Setting key for the auto-summary trigger. Read on every orchestration tick
// (10s cadence), so toggling it in the UI takes effect without restarting
// the backend — same hot-reload pattern as the subtask settings above.
//
//   - orchestration.auto_summary: when true, an orchestration batch flips
//     to "summarizing" automatically once every child is terminal (the
//     pre-feature behaviour). When false (the product default), the batch
//     stops at the new "awaiting_summary" state and waits for the user to
//     click "📝 生成汇总" in the sub-task panel. Missing key / unparsable
//     value / DB error all fail-safe to false so a broken row can never
//     re-introduce the silent auto-trigger this whole flag exists to make
//     configurable.
const (
	settingOrchestrationAutoSummary = "orchestration.auto_summary"
)

// Defaults + clamp limits for the design-stage sync timeout. The default 60s
// matches the architecture decision; the [10, 600] envelope is enforced at
// both read and write time so a bad payload can never be persisted in a
// shape the design stage would have to defend against on every call.
const (
	DefaultGitSyncTimeoutSeconds = 60
	minGitSyncTimeoutSeconds     = 10
	maxGitSyncTimeoutSeconds     = 600
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

// SubTaskFixSessionMode reads the session mode a Review's fix sub-tasks use.
//
// Missing or malformed values fall back to "with_context" (the safe default:
// a curated ≤32 KB context instead of the developer's full conversation).
// A DB error IS returned — the caller keeps its last known value.
func (s *SettingService) SubTaskFixSessionMode() (string, error) {
	raw, err := s.Get(settingSubTaskFixSessionMode)
	if err != nil {
		return SubTaskFixSessionModeWithContext, err
	}
	switch strings.TrimSpace(raw) {
	case SubTaskFixSessionModeFork:
		return SubTaskFixSessionModeFork, nil
	case SubTaskFixSessionModeWithContext:
		return SubTaskFixSessionModeWithContext, nil
	default:
		return SubTaskFixSessionModeWithContext, nil
	}
}

// SetSubTaskFixSessionMode persists the fix sub-tasks' session mode.
// An unknown value is normalized to the default so a typo can never leave
// the dispatch path without a valid mode.
func (s *SettingService) SetSubTaskFixSessionMode(mode string) error {
	switch strings.TrimSpace(mode) {
	case SubTaskFixSessionModeFork:
		mode = SubTaskFixSessionModeFork
	default:
		mode = SubTaskFixSessionModeWithContext
	}
	return s.Set(settingSubTaskFixSessionMode, mode)
}

// Setting keys for the reasoning-effort policy (claude CLI --effort). Read
// on every claude run, so a change takes effect without restarting the
// backend — same hot-reload pattern as the subtask settings above.
//
//   - effort.enabled: master switch. When false, NO run emits --effort at
//     all (the CLI keeps its own default). This is the escape hatch for a
//     CLI that doesn't know the flag, or a third-party gateway that
//     rejects the extra parameter.
//   - effort.design: default tier for the design stage (方案设计).
//   - effort.coding: default tier for the coding stage (开发实现).
//   - effort.commit: default tier for the commit stage (提交推送).
//   - effort.other:  default tier for every other run (分析 / Review /
//     答疑 / 压缩 / Wiki / 周报).
//
// The shipped values are high / medium / low / high. The "other" default is
// high on purpose: that is the CLI's own default level, so an unconfigured
// install behaves exactly like it did before --effort existed.
const (
	settingEffortEnabled = "effort.enabled"
	settingEffortDesign  = "effort.design"
	settingEffortCoding  = "effort.coding"
	settingEffortCommit  = "effort.commit"
	settingEffortOther   = "effort.other"
)

// EffortConfig is the persisted reasoning-effort policy: the master switch
// plus one default tier per pipeline stage.
type EffortConfig struct {
	// Enabled turns --effort emission on for every run. false = no run
	// emits the flag (the CLI keeps its own default).
	Enabled bool `json:"enabled"`
	// Design is the default tier for the design stage.
	Design string `json:"design"`
	// Coding is the default tier for the coding stage.
	Coding string `json:"coding"`
	// Commit is the default tier for the commit stage (提交推送).
	Commit string `json:"commit"`
	// Other is the default tier for every remaining run.
	Other string `json:"other"`
}

// ── Project CLAUDE.md injection policy ─────────────────────────────────────
//
// How the managed project-level CLAUDE.md reaches the model. All three are
// read on every stage that injects the document, so a change takes effect
// without restarting the backend — same hot-reload pattern as the settings
// above.
//
//   - claudedoc.inject_mode: "native" (default) = deliver the managed copy
//     through the Claude CLI's own project-memory channel (a materialized
//     CLAUDE.md granted via --add-dir), so the CLI loads it with the same
//     authority it gives a repository's own CLAUDE.md. "prompt" = fall back
//     to splicing the document into the user prompt (the pre-change
//     behaviour).
//   - claudedoc.exclude_repo_doc: true (default) = exclude the repository's
//     own instruction files from the CLI's load set, so the managed copy is
//     the ONLY copy in the context. false = let both coexist.
//   - claudedoc.system_prompt_mode: "append" (default) = deliver the role
//     persona with --append-system-prompt, keeping the CLI's default system
//     prompt. "replace" = --system-prompt (the pre-change behaviour).
//
// The last one exists because the pre-change code replaced the CLI's whole
// system prompt with the persona, which also removed the CLI's default
// instructions. It is kept as a switch so the previous behaviour stays one
// setting away.
const (
	settingClaudeDocInjectMode      = "claudedoc.inject_mode"
	settingClaudeDocExcludeRepoDoc  = "claudedoc.exclude_repo_doc"
	settingClaudeDocSystemPromptMod = "claudedoc.system_prompt_mode"
)

// Values of settingClaudeDocInjectMode.
const (
	// ClaudeDocInjectModeNative delivers the managed copy through the CLI's
	// native project-memory channel.
	ClaudeDocInjectModeNative = "native"
	// ClaudeDocInjectModePrompt splices the managed copy into the user
	// prompt (the fallback when materialization fails).
	ClaudeDocInjectModePrompt = "prompt"
)

// Values of settingClaudeDocSystemPromptMod.
const (
	// ClaudeDocSystemPromptAppend keeps the CLI's default system prompt and
	// appends the persona to it.
	ClaudeDocSystemPromptAppend = "append"
	// ClaudeDocSystemPromptReplace replaces the CLI's default system prompt
	// with the persona.
	ClaudeDocSystemPromptReplace = "replace"
)

// ClaudeDocInjectionConfig is the resolved injection policy.
type ClaudeDocInjectionConfig struct {
	// InjectMode is ClaudeDocInjectModeNative or ClaudeDocInjectModePrompt.
	InjectMode string
	// ExcludeRepoDoc excludes the repository's own instruction files from
	// the CLI's load set.
	ExcludeRepoDoc bool
	// SystemPromptMode is ClaudeDocSystemPromptAppend or
	// ClaudeDocSystemPromptReplace.
	SystemPromptMode string
}

// DefaultClaudeDocInjectionConfig is the shipped policy: the managed copy
// goes through the CLI's native channel, and the repository's own copy is
// excluded so exactly one document is in context.
//
// The persona mode stays at "replace" — the pre-change behaviour. The
// measurement (internal/llm/claude_md_settings.go) showed the native
// channel works with EITHER persona mode, so switching it is NOT a
// prerequisite for this feature; it is a separate change that alters the
// model's behaviour in every stage, and it deserves its own staged rollout
// with real regression runs rather than riding along with the injection
// change.
func DefaultClaudeDocInjectionConfig() ClaudeDocInjectionConfig {
	return ClaudeDocInjectionConfig{
		InjectMode:       ClaudeDocInjectModeNative,
		ExcludeRepoDoc:   true,
		SystemPromptMode: ClaudeDocSystemPromptReplace,
	}
}

// ClaudeDocInjectionConfig reads the injection policy. Missing keys and
// malformed values fall back to the shipped defaults; a DB error is returned
// so the caller can log it and keep its last known good state.
func (s *SettingService) ClaudeDocInjectionConfig() (ClaudeDocInjectionConfig, error) {
	cfg := DefaultClaudeDocInjectionConfig()
	if s == nil || s.db == nil {
		// A nil settings service means "no settings wired" — the shipped
		// defaults apply. Callers hold this behind an interface, so a nil
		// *SettingService arrives as a non-nil interface value.
		return cfg, nil
	}
	raw, err := s.Get(settingClaudeDocInjectMode)
	if err != nil {
		return cfg, err
	}
	switch strings.TrimSpace(raw) {
	case ClaudeDocInjectModeNative:
		cfg.InjectMode = ClaudeDocInjectModeNative
	case ClaudeDocInjectModePrompt:
		cfg.InjectMode = ClaudeDocInjectModePrompt
	}
	raw, err = s.Get(settingClaudeDocExcludeRepoDoc)
	if err != nil {
		return cfg, err
	}
	if b, berr := strconv.ParseBool(strings.TrimSpace(raw)); berr == nil {
		cfg.ExcludeRepoDoc = b
	}
	raw, err = s.Get(settingClaudeDocSystemPromptMod)
	if err != nil {
		return cfg, err
	}
	switch strings.TrimSpace(raw) {
	case ClaudeDocSystemPromptAppend:
		cfg.SystemPromptMode = ClaudeDocSystemPromptAppend
	case ClaudeDocSystemPromptReplace:
		cfg.SystemPromptMode = ClaudeDocSystemPromptReplace
	}
	return cfg, nil
}

// SetClaudeDocInjectionConfig persists the injection policy. Values are
// validated here (unknown values fall back to the shipped default) so a bad
// payload can never be written in a shape the injection sites would have to
// defend against on every read.
func (s *SettingService) SetClaudeDocInjectionConfig(cfg ClaudeDocInjectionConfig) error {
	mode := strings.TrimSpace(cfg.InjectMode)
	if mode != ClaudeDocInjectModeNative && mode != ClaudeDocInjectModePrompt {
		mode = DefaultClaudeDocInjectionConfig().InjectMode
	}
	if err := s.Set(settingClaudeDocInjectMode, mode); err != nil {
		return err
	}
	if err := s.Set(settingClaudeDocExcludeRepoDoc, strconv.FormatBool(cfg.ExcludeRepoDoc)); err != nil {
		return err
	}
	spMode := strings.TrimSpace(cfg.SystemPromptMode)
	if spMode != ClaudeDocSystemPromptAppend && spMode != ClaudeDocSystemPromptReplace {
		spMode = DefaultClaudeDocInjectionConfig().SystemPromptMode
	}
	return s.Set(settingClaudeDocSystemPromptMod, spMode)
}

// PersonaMode returns the configured persona delivery mode.
//
// It satisfies llm.PersonaModeProvider, so the gateway picks it up through
// the same wiring as the effort defaults (SetPersonaModeProvider).
//
// A missing or malformed value falls back to the shipped default
// ("append"), the same fallback ClaudeDocInjectionConfig uses — the two
// read the same key, so they must agree.
func (s *SettingService) PersonaMode() string {
	if s == nil || s.db == nil {
		return llm.NormalizePersonaMode(DefaultClaudeDocInjectionConfig().SystemPromptMode)
	}
	raw, err := s.Get(settingClaudeDocSystemPromptMod)
	if err != nil {
		return llm.NormalizePersonaMode(DefaultClaudeDocInjectionConfig().SystemPromptMode)
	}
	return llm.NormalizePersonaMode(raw)
}

// EffortConfig returns the reasoning-effort policy. Missing keys, malformed
// values and DB errors all fail-safe to the shipped defaults rather than
// erroring: a broken settings row must never be able to stop a run from
// starting. The DB error IS returned so callers can log it and keep their
// last known good state.
func (s *SettingService) EffortConfig() (EffortConfig, error) {
	cfg := EffortConfig{
		Enabled: true,
		Design:  llm.DefaultEffortDesign,
		Coding:  llm.DefaultEffortCoding,
		Commit:  llm.DefaultEffortCommit,
		Other:   llm.DefaultEffortOther,
	}
	raw, err := s.Get(settingEffortEnabled)
	if err != nil {
		return cfg, err
	}
	if b, berr := strconv.ParseBool(strings.TrimSpace(raw)); berr == nil {
		cfg.Enabled = b
	}
	for _, item := range []struct {
		key string
		dst *string
	}{
		{settingEffortDesign, &cfg.Design},
		{settingEffortCoding, &cfg.Coding},
		{settingEffortCommit, &cfg.Commit},
		{settingEffortOther, &cfg.Other},
	} {
		raw, err = s.Get(item.key)
		if err != nil {
			return cfg, err
		}
		if v := llm.NormalizeEffort(raw); v != "" {
			*item.dst = v
		}
	}
	return cfg, nil
}

// SetEffortConfig persists the reasoning-effort policy. Tiers are validated
// here (unknown values fall back to the shipped default) so a bad payload
// can never be written in a shape the run sites would have to defend
// against on every read.
func (s *SettingService) SetEffortConfig(cfg EffortConfig) error {
	if err := s.Set(settingEffortEnabled, strconv.FormatBool(cfg.Enabled)); err != nil {
		return err
	}
	for _, item := range []struct {
		key string
		val string
	}{
		{settingEffortDesign, cfg.Design},
		{settingEffortCoding, cfg.Coding},
		{settingEffortCommit, cfg.Commit},
		{settingEffortOther, cfg.Other},
	} {
		v := llm.NormalizeEffort(item.val)
		if v == "" {
			v = llm.DefaultEffortForStage(stageForEffortKey(item.key))
		}
		if err := s.Set(item.key, v); err != nil {
			return err
		}
	}
	return nil
}

// stageForEffortKey maps a settings key back to its pipeline stage, so the
// write path can fall back to the right built-in default when the payload
// carries an unknown tier.
func stageForEffortKey(key string) string {
	switch key {
	case settingEffortDesign:
		return llm.StageDesign
	case settingEffortCoding:
		return llm.StageCoding
	case settingEffortCommit:
		return llm.StageCommit
	case settingEffortOther:
		return llm.StageOther
	}
	return ""
}

// EffortDefaults implements llm.EffortProvider so the gateway can read the
// policy without importing this package. A DB error degrades to "no
// configuration" (ok=false) → the gateway falls back to the built-in
// defaults rather than failing a run over a settings read.
func (s *SettingService) EffortDefaults() (enabled bool, design, coding, commit, other string, ok bool) {
	cfg, err := s.EffortConfig()
	if err != nil {
		return true, "", "", "", "", false
	}
	return cfg.Enabled, cfg.Design, cfg.Coding, cfg.Commit, cfg.Other, true
}

// AutoSummaryEnabled reports whether the orchestration queue should
// automatically run a summary round after every batch's children reach
// a terminal state. Missing key / unparsable value / DB error all
// fail-safe to false (matches the product default: manual). The DB error
// is still surfaced so callers can keep the last known good state in their
// own logging, but the returned bool is always the safe value.
func (s *SettingService) AutoSummaryEnabled() (bool, error) {
	v, err := s.Get(settingOrchestrationAutoSummary)
	if err != nil {
		return false, err
	}
	if v == "" {
		return false, nil
	}
	b, perr := strconv.ParseBool(strings.TrimSpace(v))
	if perr != nil {
		return false, nil
	}
	return b, nil
}

// SetAutoSummary upserts the orchestration.auto_summary flag. Stored as
// the canonical "true"/"false" string via strconv.FormatBool (matches the
// other boolean settings — subtask.auto_retry — so a SELECT scan stays
// trivial).
func (s *SettingService) SetAutoSummary(enabled bool) error {
	return s.Set(settingOrchestrationAutoSummary, strconv.FormatBool(enabled))
}

// GitSyncTimeout returns the design-stage hard-sync timeout. Missing or
// malformed values fall back to the 60s default rather than erroring: the
// settings row is optional, and a broken value must not stall every
// architect-design run. A DB error IS returned so callers can keep the last
// known good value. The result is always within [10s, 600s] regardless of
// what was persisted.
func (s *SettingService) GitSyncTimeout() (time.Duration, error) {
	defaultDur := time.Duration(DefaultGitSyncTimeoutSeconds) * time.Second
	raw, err := s.Get(settingGitSyncTimeout)
	if err != nil {
		return defaultDur, err
	}
	n, cerr := strconv.Atoi(strings.TrimSpace(raw))
	if cerr != nil || n <= 0 {
		return defaultDur, nil
	}
	if n < minGitSyncTimeoutSeconds {
		n = minGitSyncTimeoutSeconds
	}
	if n > maxGitSyncTimeoutSeconds {
		n = maxGitSyncTimeoutSeconds
	}
	return time.Duration(n) * time.Second, nil
}

// SetGitSyncTimeout persists the design-stage hard-sync timeout, clamped to
// [10s, 600s]. The clamp runs on write so a bad payload can never end up on
// disk in a shape the design stage would have to defend against on every
// read. Nothing else happens on write: the wizard re-reads the timeout on
// every architect-design entry, which is what makes the change hot without
// wiring the sync code into this service.
func (s *SettingService) SetGitSyncTimeout(seconds int) error {
	if seconds < minGitSyncTimeoutSeconds {
		seconds = minGitSyncTimeoutSeconds
	}
	if seconds > maxGitSyncTimeoutSeconds {
		seconds = maxGitSyncTimeoutSeconds
	}
	return s.Set(settingGitSyncTimeout, strconv.Itoa(seconds))
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
