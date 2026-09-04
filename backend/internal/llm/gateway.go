package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeEnvProvider supplies the Claude CLI subprocess env vars (auth token /
// base URL), sourced from the active claude_configs row by ClaudeConfigService.
// nil means "use the process environment only".
type ClaudeEnvProvider interface {
	ClaudeEnvVars() (authToken, baseURL string, err error)
}

// LLMConfigProvider supplies the direct HTTP LLM channel config (base URL, API
// key, model) used for lightweight tasks like requirement formatting + title
// distillation. Called per request so runtime setting updates apply
// immediately. nil disables that channel.
type LLMConfigProvider interface {
	LLMConfig() (baseURL, apiKey, model string, err error)
}

type Gateway struct {
	binPath    string
	timeout    time.Duration
	claudeEnv  ClaudeEnvProvider
	llmCfg     LLMConfigProvider
}

// New wires the gateway with separate providers for the Claude CLI env (auth
// token / base URL, from the active claude_configs row) and the direct HTTP
// LLM channel (from the settings table). Either may be nil.
func New(claudeEnv ClaudeEnvProvider, llmCfg LLMConfigProvider) *Gateway {
	binPath := os.Getenv("CLAUDE_BIN")
	if binPath == "" {
		binPath = "claude"
	}

	timeout := 120 * time.Second
	if t := os.Getenv("CLAUDE_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil {
			timeout = d
		}
	}

	// Verify claude CLI is available; resolve to the real executable path so
	// exec.Cmd.Start() always uses an absolute ELF path rather than a wrapper
	// script or multi-hop symlink (which can cause a misleading ENOENT on Linux
	// even when the file exists).
	if abs, err := exec.LookPath(binPath); err != nil {
		fmt.Printf("[LLM] WARNING: 'claude' CLI not found in PATH. AI features will use stub responses.\n")
		fmt.Printf("[LLM] Install with: npm install -g @anthropic-ai/claude-code\n")
	} else {
		// Follow symlinks all the way to the actual binary so cmd.Path never
		// points at a shell wrapper whose shebang interpreter may be absent.
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			binPath = real
		} else {
			binPath = abs
		}
	}

	return &Gateway{binPath: binPath, timeout: timeout, claudeEnv: claudeEnv, llmCfg: llmCfg}
}

func (g *Gateway) GetBinPath() string { return g.binPath }

// mergedEnv returns the process environment with the configured Claude settings
// (ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL) overriding any inherited values,
// plus the subagent/background tier models pinned to `model` when non-empty.
// Empty configured values are not injected, so the API default / inherited env
// still applies.
//
// Model tiers: when the caller passes a model (--model), the claude CLI's
// Agent tool spawns subagents on the Sonnet tier (ANTHROPIC_DEFAULT_SONNET_MODEL)
// and fast background tasks on the Haiku tier. Those defaults are otherwise
// read from ~/.claude/settings.json — which we deliberately drop via
// --setting-sources project,local (see settingSources) — so they fall back to
// the CLI's built-in Anthropic models and break on a custom base URL
// (e.g. DeepSeek: "not found"). Pinning the tier models to the main model keeps
// every spawned subagent on the same endpoint/model as the main agent.
//
// Auth precedence: the claude CLI prefers ANTHROPIC_API_KEY over
// ANTHROPIC_AUTH_TOKEN, so an inherited ANTHROPIC_API_KEY would silently shadow
// a user-configured bearer token (and point it at the wrong auth scheme for a
// custom base URL). When a token is configured, we therefore strip any inherited
// ANTHROPIC_API_KEY from the child environment so the configured token wins.
// claudeEnvOverrides builds the map of env vars the platform must pin on every
// claude subprocess: auth token + base URL (from the active claude_configs row),
// the three tier-model pins (when --model is set), caller-supplied extras, and
// CLAUDE_ALLOW_ROOT=1. Shared by the local env builder (mergedEnv) and the
// remote env builder (BuildRemoteEnvPairs) so both paths pin the same keys.
func (g *Gateway) claudeEnvOverrides(model string, extras ...string) map[string]string {
	overrides := map[string]string{}
	if g.claudeEnv != nil {
		if tok, baseURL, err := g.claudeEnv.ClaudeEnvVars(); err == nil {
			if tok != "" {
				overrides["ANTHROPIC_AUTH_TOKEN"] = tok
			}
			if baseURL != "" {
				overrides["ANTHROPIC_BASE_URL"] = baseURL
			}
		}
	}
	if model != "" {
		overrides["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = model
		overrides["ANTHROPIC_DEFAULT_SONNET_MODEL"] = model
		overrides["ANTHROPIC_DEFAULT_OPUS_MODEL"] = model
	}
	// Extra caller-supplied overrides win over the inherited env (so the
	// merge step can pin GIT_AUTHOR_* / GIT_COMMITTER_* into the Claude
	// child process and let its `git commit --no-edit` carry a real identity
	// on Docker hosts without ~/.gitconfig mounted).
	for _, kv := range extras {
		if eq := strings.Index(kv, "="); eq > 0 {
			overrides[kv[:eq]] = kv[eq+1:]
		}
	}
	// Allow claude CLI to run under root (e.g. in Docker). The CLI blocks
	// --dangerously-skip-permissions when uid==0 unless this var is set.
	overrides["CLAUDE_ALLOW_ROOT"] = "1"
	return overrides
}

func (g *Gateway) mergedEnv(model string, extra ...string) []string {
	env := os.Environ()
	overrides := g.claudeEnvOverrides(model, extra...)
	// Keys to strip from the inherited env because they would conflict with the
	// configured auth. Only strip when we are actually injecting ANTHROPIC_AUTH_TOKEN.
	dropKeys := map[string]bool{}
	if _, ok := overrides["ANTHROPIC_AUTH_TOKEN"]; ok {
		dropKeys["ANTHROPIC_API_KEY"] = true
	}
	out := make([]string, 0, len(env)+len(overrides))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if _, ok := overrides[key]; ok {
			continue // drop inherited, re-add override below
		}
		if dropKeys[key] {
			continue // conflicting inherited auth — drop so configured token wins
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

// settingSources returns the --setting-sources value to pass to the claude CLI,
// or "" to let the CLI load its default sources (user + project + local).
//
// The claude CLI applies the `env` block from ~/.claude/settings.json OVER the
// process environment, so when we inject platform-configured auth (token/base
// URL via mergedEnv) we must drop the "user" source — otherwise the user's
// settings.json would silently shadow the configured ANTHROPIC_AUTH_TOKEN /
// ANTHROPIC_BASE_URL. We only do this when an override is actually present, so
// a platform with no Claude config still falls back to the user's settings.
func (g *Gateway) settingSources(override *bool) string {
	// Explicit override always wins — used by the remote Agent-server path
	// where the host's ~/.claude/settings.json may be stale or carry a
	// different base URL. Forcing --setting-sources project,local drops the
	// "user" source (i.e. ~/.claude/settings.json) from the merged env.
	if override != nil {
		if *override {
			return "project,local"
		}
		return ""
	}
	if g.claudeEnv == nil {
		return ""
	}
	tok, baseURL, err := g.claudeEnv.ClaudeEnvVars()
	if err != nil || (tok == "" && baseURL == "") {
		return ""
	}
	return "project,local"
}

// BuildStreamArgs is the public version of streamArgs — exposed so the remote
// Agent-server code path (handler/wizard.go runRemoteCoding) can render the
// same flag list as a remote shell command. The output is byte-identical to
// the unexported streamArgs above; both go through settingSources so a custom
// base URL stays in sync with the local execution.
func (g *Gateway) BuildStreamArgs(opts StreamOpts) []string {
	return g.streamArgs(opts.Prompt, opts.SystemPrompt, opts.Model, opts.SessionID, opts.Resume, opts.Fork, opts.ForkSessionID, opts.DisallowedTools, opts.PermissionMode, opts.OverrideSettingSources)
}

// BuildEnvPairs returns the merged KEY=VALUE environment entries that StreamCmd
// would apply to the claude CLI subprocess. Exposed so the remote path can
// prefix the same env into the remote shell command — keeping the local and
// remote executions behaviourally identical (same auth token, same
// tier-model pinning, same ExtraEnv).
//
// CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1 is always injected
// when a non-empty model is passed. Claude Code carries its own local model
// catalog; custom models (e.g. minimax-M3 behind a private base URL) are not
// in it, so the CLI would conservatively assume a 200k context window and
// log "[claude-code:unrecognized_model]". On some CLI versions this is
// non-fatal (CLI proceeds with auto-compact), on others it surfaces as
// `process exited with code 1` because the SDK can't tell the difference
// between "CLI gave up on the model" and "model call failed". Setting this
// env var restores the older "wait for the API to tell us the real window"
// behavior, which works for any model the API itself accepts — exactly
// what we want when the user is pointing at a non-Anthropic endpoint.
func (g *Gateway) BuildEnvPairs(model string, extras ...string) []string {
	if model != "" {
		extras = append(extras, "CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1")
	}
	return g.mergedEnv(model, extras...)
}

// BuildRemoteEnvPairs returns ONLY the platform-pinned env entries for a claude
// run on a remote Agent server — auth token, base URL, tier-model pins,
// CLAUDE_ALLOW_ROOT, and the model-window-enforcement bypass. Unlike
// BuildEnvPairs it does NOT inherit os.Environ() from the NovaWorkbench host.
//
// The remote nova-agent-worker spawns claude inside the remote host's own
// process environment (its real $HOME / $TMPDIR / $PATH). Inheriting the
// NovaWorkbench host env is what leaked macOS-shaped HOME=/Users/<user> and
// TMPDIR=/var/folders/<...>/T into the Linux agent: the CLI then tried to
// write ~/.claude under the bogus $HOME and `claude --print ping` hung until
// the 5s preflight timeout (surfacing as "preflight_timeout / exit_code 143").
// The remote env must therefore carry only the keys the CLI can't derive from
// the remote host itself.
func (g *Gateway) BuildRemoteEnvPairs(model string, extras ...string) []string {
	if model != "" {
		extras = append(extras, "CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1")
	}
	overrides := g.claudeEnvOverrides(model, extras...)
	out := make([]string, 0, len(overrides))
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

// streamArgs builds the shared claude CLI flag list for stream-json +
// dangerously-skip-permissions runs. When systemPrompt is non-empty it is passed
// via --system-prompt (full replace); when model is non-empty it is passed via
// --model. These come from the role config (see service.DefaultRoles).
//
// Session handling: when sessionID is non-empty, a non-resume call passes
// --session-id <uuid> (starts a new conversation with a known id); a resume call
// passes --resume <session-id> (continues that conversation). When fork is true
// (only meaningful with resume), --fork-session is added so the CLI mints a NEW
// session id that inherits the resumed conversation's full history — this lets
// us chain the analyst → architect → developer stages under one context thread
// while swapping the role persona via --system-prompt at each fork. When a
// forkSessionID is also supplied on a fork, it is passed as --session-id so the
// CALLER pre-assigns the forked session's id (the CLI honors this override)
// rather than having to read it back from the stream's init event after the
// fact — this is what lets us persist the session id before the run even starts.
// All flags combine with --system-prompt/--model/--dangerously-skip-permissions.
func (g *Gateway) streamArgs(prompt, systemPrompt, model, sessionID string, resume, fork bool, forkSessionID string, disallowedTools []string, permissionMode string, overrideSettingSources *bool) []string {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	if ss := g.settingSources(overrideSettingSources); ss != "" {
		args = append(args, "--setting-sources", ss)
	}
	if permissionMode == "plan" {
		args = append(args, "--permission-mode", "plan")
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}
	if model != "" {
		args = append([]string{"--model", model}, args...)
	}
	if systemPrompt != "" {
		if permissionMode == "plan" {
			// In plan mode the CLI's default system prompt carries the plan-mode
			// instructions (explore read-only, write plan to ~/.claude/plans/,
			// call ExitPlanMode). --system-prompt would REPLACE those and break the
			// plan workflow, so we APPEND the role persona instead.
			args = append(args, "--append-system-prompt", systemPrompt)
		} else {
			args = append(args, "--system-prompt", systemPrompt)
		}
	}
	if sessionID != "" {
		if resume {
			args = append(args, "--resume", sessionID)
			if fork {
				args = append(args, "--fork-session")
				if forkSessionID != "" {
					args = append(args, "--session-id", forkSessionID)
				}
			}
		} else {
			args = append(args, "--session-id", sessionID)
		}
	}
	// --disallowedTools denies specific tools (comma/space-separated list, per
	// the CLI's variadic flag). Used by the analyst first turn to force a
	// tool-less answer from pre-read context — some proxies mangle multi-turn
	// tool-use streaming ("Content block not found"), and the first turn
	// doesn't need tools anyway.
	if len(disallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(disallowedTools, " "))
	}
	return args
}

// StreamOpts configures a stream-json claude run. SessionID/Resume/Fork control
// conversation threading (see streamArgs); SystemPrompt/Model carry the active
// role's persona; DisallowedTools denies specific tools (e.g. file-reading tools
// for a tool-less first turn). PermissionMode controls the --permission-mode flag
// ("plan" or "" — empty means --dangerously-skip-permissions).
type StreamOpts struct {
	Prompt          string
	WorkDir         string
	SystemPrompt    string
	Model           string
	SessionID       string
	Resume          bool
	Fork            bool   // --fork-session; only meaningful when Resume is true
	ForkSessionID   string // pre-assigned id for a forked session (--session-id on --fork-session); empty = let the CLI generate one
	DisallowedTools []string
	PermissionMode  string // "plan" to use --permission-mode plan, empty for --dangerously-skip-permissions
	// ExtraEnv is layered onto the spawned claude CLI's environment as
	// KEY=VALUE entries — used to inject GIT_AUTHOR_* / GIT_COMMITTER_*
	// when the merge step asks Claude to commit on a Docker host without
	// ~/.gitconfig.
	ExtraEnv []string
	// OverrideSettingSources tri-state:
	//   nil  → auto (existing behavior): pass --setting-sources project,local
	//          ONLY when an active claude_configs row supplies a non-empty
	//          auth token or base URL (see Gateway.settingSources).
	//   *true → force override: ALWAYS pass --setting-sources project,local,
	//          dropping the user's ~/.claude/settings.json "env" block from
	//          the merged process environment. Required by the remote
	//          Agent-server path because the remote host's settings.json
	//          may carry a stale/wrong base URL that would silently shadow
	//          the platform's active claude_configs row.
	//   *false → never override: always let the CLI load user + project +
	//           local sources (default when no auth override is present).
	OverrideSettingSources *bool
}

// StreamCmd returns an unstarted *exec.Cmd configured for stream-json output
// with full tool use. The caller owns the lifecycle (Start/Wait) and parses the
// stream itself — this is the pattern for anything that needs live progress.
// The stream-json "system"/"init" event carries the session_id of the run; for
// a forked run that id is NEW unless the caller pre-assigns it via
// opts.ForkSessionID (--session-id on --fork-session). Reading it back from the
// stream is now a confirmation / safety net rather than the only source of the
// id.
func (g *Gateway) StreamCmd(ctx context.Context, opts StreamOpts) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.binPath, g.BuildStreamArgs(opts)...)
	cmd.Env = g.BuildEnvPairs(opts.Model, opts.ExtraEnv...)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	}
	return cmd
}

// runClaudeStreamJSON runs claude with stream-json + dangerously-skip-permissions,
// which gives Claude full tool-use access to read project files.
// It returns the final result text from the "result" event.
func (g *Gateway) runClaudeStreamJSON(prompt, workDir, systemPrompt, model string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, g.binPath, g.streamArgs(prompt, systemPrompt, model, "", false, false, "", nil, "", nil)...)
	cmd.Env = g.mergedEnv(model)
	if workDir != "" {
		cmd.Dir = workDir
	}

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude stream-json failed: %w", err)
	}

	// Parse each line and extract the "result" event's result field.
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if evt["type"] == "result" && evt["subtype"] == "success" {
			if result, ok := evt["result"].(string); ok {
				return strings.TrimSpace(result), nil
			}
		}
	}
	return "", fmt.Errorf("claude stream-json: no result event in output")
}

// runClaudeText runs claude in single-shot text mode (no tool use) and returns
// the raw assistant text. Lighter than runClaudeStreamJSON — used for quick
// prompts like title generation where tool access isn't needed.
func (g *Gateway) runClaudeText(prompt string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{"-p", prompt, "--output-format", "text"}
	if ss := g.settingSources(nil); ss != "" {
		args = append(args, "--setting-sources", ss)
	}
	cmd := exec.CommandContext(ctx, g.binPath, args...)
	cmd.Env = g.mergedEnv("")

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude text failed: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ExtractSubtasksJSON is the last-resort orchestration parser: when the main
// agent's reply contains a task breakdown in NO machine-readable form (no
// Write-captured subtasks.json, no sentinel+JSON, no parseable markdown
// table), the handler feeds the raw reply to this cheap single-shot call and
// asks for ONLY the {"subtasks":[{"title","prompt"}]} JSON object. feedback
// carries the previous attempt's parse error so the caller can retry once
// with the mistake made explicit (empty on the first attempt). The caller
// JSON-decodes the returned text — this method performs no validation.
func (g *Gateway) ExtractSubtasksJSON(mainReply, feedback string) (string, error) {
	// Cap the source text: a coding turn's finalResult can be tens of KB and
	// the extractor only needs the plan section.
	const maxRunes = 24000
	runes := []rune(mainReply)
	if len(runes) > maxRunes {
		mainReply = string(runes[:maxRunes]) + "\n…（后文省略）"
	}
	prompt := "下面是一个软件开发主 Agent 的任务拆分回复。请从中提取子任务列表，" +
		"只输出一个 JSON 对象，不要输出任何其他文字、解释或 markdown 代码围栏。\n" +
		"格式：{\"subtasks\":[{\"title\":\"<子任务标题>\",\"prompt\":\"<执行该子任务的完整提示词，包含涉及文件、改动内容、产物形式>\"}]}\n" +
		"规则：\n" +
		"- 每个表格行/列表项对应一个子任务；序号列（#、1、2…）不是标题。\n" +
		"- prompt 字段要自包含：子 Agent 看不到原始对话，只看你给的 prompt。\n" +
		"- 如果回复中确实没有任何任务拆分，输出 {\"subtasks\":[]}。\n"
	if feedback != "" {
		prompt += "\n你上一次的输出无法解析为合法 JSON，错误信息：" + feedback + "\n这次请严格输出合法 JSON。\n"
	}
	prompt += "\n主 Agent 的回复如下：\n" + mainReply
	return g.runClaudeText(prompt, 120*time.Second)
}

// GenerateDescriptionAndTitle reorganizes the user's raw requirement content
// into structured Markdown AND distills a concise title in a single LLM round,
// via the direct HTTP LLM channel (OpenAI-compatible, e.g. DeepSeek). This
// bypasses the claude CLI for speed — neither task needs tool use, and merging
// them keeps the title and body consistent while transmitting the content once.
// The kind argument ("issue" / "requirement" / "idea") tweaks the system
// prompt so the distilled Markdown carries the right shape: an Issue becomes
// a bug-report scaffold (现象/复现步骤/期望/实际), a Requirement keeps the
// legacy four-section layout (背景/目标/功能要点/验收标准), and an Idea
// becomes a lightweight exploratory note (灵感来源/初步设想/待回答问题).
// Empty kind falls back to the legacy behavior. The channel activates only
// when both base_url and api_key are configured; otherwise it returns an error
// and the caller falls back to the raw content for Markdown and the first
// line for the title (no claude CLI fallback, by design) so requirement
// creation never fails just because this is unavailable.
func (g *Gateway) GenerateDescriptionAndTitle(content, kind string) (markdown, title string, usage *Usage, err error) {
	if g.llmCfg == nil {
		return "", "", nil, fmt.Errorf("llm not configured: no llm config provider")
	}
	baseURL, apiKey, model, err := g.llmCfg.LLMConfig()
	if err != nil {
		return "", "", nil, fmt.Errorf("llm config unavailable: %w", err)
	}
	if baseURL == "" || apiKey == "" {
		return "", "", nil, fmt.Errorf("llm not configured: base_url and api_key required")
	}
	return formatAndTitleViaHTTP(baseURL, apiKey, model, content, kind)
}

// SummarizeIdeaToRequirement converts a finished idea-discussion thread
// (initial description + multi-turn analyst chat + accumulated
// acceptance_criteria) into a draft requirement Markdown, title, and
// acceptance-criteria list. Uses the same OpenAI-compatible HTTP LLM channel
// as GenerateDescriptionAndTitle — there is no claude CLI fallback by design,
// so the summarize action fails loud if the LLM is misconfigured.
//
// The prompt explicitly allows the model to refuse the conversion by emitting
// an empty Markdown + empty criteria; the service treats that as
// "discussion didn't converge" and refuses to create the new requirement,
// leaving the original idea intact.
//
// Token usage is not returned here: the service-side Summarizer interface
// (see internal/service/requirement.go) intentionally omits it so service
// doesn't have to import llm for a single type. If/when we want to record
// usage for the summarize step, the handler can re-invoke the LLM channel
// separately or we extend the Summarizer interface to carry usage back.
func (g *Gateway) SummarizeIdeaToRequirement(content string) (markdown, title string, criteria []string, err error) {
	if g.llmCfg == nil {
		return "", "", nil, fmt.Errorf("llm not configured: no llm config provider")
	}
	baseURL, apiKey, model, err := g.llmCfg.LLMConfig()
	if err != nil {
		return "", "", nil, fmt.Errorf("llm config unavailable: %w", err)
	}
	if baseURL == "" || apiKey == "" {
		return "", "", nil, fmt.Errorf("llm not configured: base_url and api_key required")
	}
	out, _, err := chatCompletion(baseURL, apiKey, model, summarizeIdeaToRequirementPrompt, content, 4096)
	if err != nil {
		return "", "", nil, err
	}
	var res summarizeIdeaToRequirementResult
	if jerr := json.Unmarshal([]byte(stripJSONFences(out)), &res); jerr != nil {
		return "", "", nil, fmt.Errorf("llm http: decode summarize json: %w", jerr)
	}
	res.Title = strings.Trim(res.Title, "\"'` \n\r\t")
	if res.Title == "" {
		res.Title = "（未达成共识）"
	}
	return res.Markdown, res.Title, res.AcceptanceCriteria, nil
}

func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// GenerateProjectSummary produces a short (≤120 char) Chinese project summary
// from the project's CLAUDE.md content. It uses the claude CLI in single-shot
// text mode with the project directory as CWD so Claude can also read adjacent
// files if needed. When the CLI is not in PATH it degrades to extracting the
// first non-heading paragraph of CLAUDE.md (the "stub" philosophy — keep the
// feature working without AI). A run-time failure returns an error so the
// caller can keep the previously stored value instead of clobbering it.
func (g *Gateway) GenerateProjectSummary(projectPath, claudeMD string) (string, error) {
	if claudeMD == "" {
		return "", fmt.Errorf("CLAUDE.md content is empty")
	}
	// CLI missing → degrade to a CLAUDE.md extraction so a summary still exists.
	if _, err := exec.LookPath(g.binPath); err != nil {
		return summaryFallback(claudeMD), nil
	}
	prompt := "你是一名技术文案。请基于以下项目的 CLAUDE.md 内容，用中文生成一段不超过 120 字的项目简介。" +
		"要求：纯文本一段，不要 markdown 标题/列表/代码块，不要 emoji，不要前后缀解释，直接输出简介内容本身。\n\n" +
		"CLAUDE.md 内容：\n" + claudeMD

	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	args := []string{"-p", prompt, "--output-format", "text"}
	if ss := g.settingSources(nil); ss != "" {
		args = append(args, "--setting-sources", ss)
	}
	cmd := exec.CommandContext(ctx, g.binPath, args...)
	cmd.Env = g.mergedEnv("")
	if projectPath != "" {
		cmd.Dir = projectPath
	}

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude summary failed: %w", err)
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return summaryFallback(claudeMD), nil
	}
	return out, nil
}

// summaryFallback extracts the first non-heading, non-empty paragraph from a
// CLAUDE.md body. Used when the claude CLI is unavailable so the project still
// has a usable (if less polished) description.
func summaryFallback(claudeMD string) string {
	var para []string
	for _, line := range strings.Split(claudeMD, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(t, "#") {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, t)
	}
	return strings.Join(para, " ")
}

// GenerateCode invokes Claude CLI to implement a requirement.
// Uses stream-json + dangerously-skip-permissions so Claude can read and write files.
// Returns the command; caller streams stdout. systemPrompt/model come from the
// "developer" role config. When opts.SessionID is set with Resume/Fork, the
// coding turn continues (or forks from) the design conversation so the developer
// inherits the full analysis+design context instead of being re-fed it.
func (g *Gateway) GenerateCode(opts StreamOpts) *exec.Cmd {
	// Use a long timeout for coding tasks — real implementations can take many minutes.
	codingTimeout := g.timeout
	if codingTimeout < 30*time.Minute {
		codingTimeout = 30 * time.Minute
	}
	ctx, _ := context.WithTimeout(context.Background(), codingTimeout) //nolint:govet

	return g.StreamCmd(ctx, opts)
}

// usageInt coereces a JSON-decoded numeric value (float64 / int / int64 /
// json.Number) to int, returning 0 for any non-numeric input. Used by
// ParseStreamUsage below; mirrors the same helper in handler/usage.go so
// the gateway can read stream-json usage without dragging the handler
// package into its import graph.
func usageInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// ParseStreamUsage reads the four token-count fields from the top-level
// "usage" object of a stream-json "result" event. The Claude CLI emits
// this object exactly once per turn at completion, regardless of whether
// the turn succeeded or was interrupted — so callers can record usage
// for both success and failure paths.
//
// The ok return value is false when the event carries no usage field
// (e.g. an early "result" with subtype:"error_during_execution" before
// any tokens were spent); the four int values are zero in that case.
// Callers that want "only record when at least one token was consumed"
// can additionally check `in+out+cc+cr > 0`.
func ParseStreamUsage(evt map[string]interface{}) (in, out, cc, cr int, ok bool) {
	u, exists := evt["usage"].(map[string]interface{})
	if !exists {
		return 0, 0, 0, 0, false
	}
	return usageInt(u["input_tokens"]),
		usageInt(u["output_tokens"]),
		usageInt(u["cache_creation_input_tokens"]),
		usageInt(u["cache_read_input_tokens"]),
		true
}
