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
	// ClaudeEnvForConfigID resolves the env for a SPECIFIC Claude config
	// (the per-role binding lookup). Empty id = same as ClaudeEnvVars
	// (the global active config). Implementations fall back to the active
	// config on a missing id so a stale binding never silently kills a run.
	ClaudeEnvForConfigID(id string) (authToken, baseURL string, err error)
}

// LLMConfigProvider supplies the direct HTTP LLM channel config (base URL, API
// key, model) used for lightweight tasks like requirement formatting + title
// distillation. Called per request so runtime setting updates apply
// immediately. nil disables that channel.
type LLMConfigProvider interface {
	LLMConfig() (baseURL, apiKey, model string, err error)
}

type Gateway struct {
	binPath   string
	timeout   time.Duration
	claudeEnv ClaudeEnvProvider
	llmCfg    LLMConfigProvider
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

// localEnv builds the claude subprocess's process env for a LOCAL run. It
// inherits os.Environ() (PATH / HOME / LANG / … are still needed by the CLI
// and its Bash tool children), layers caller-supplied extras on top, and
// strips the inherited ANTHROPIC_* auth keys that the --settings env block
// (settingsArg) now owns.
//
// Why the pins no longer travel here: the platform's auth token / base URL /
// model / tier pins are delivered via `claude --settings '{"env":{…}}'` — the
// same channel the remote nova-agent-worker uses (its buildSettingsArg). The
// CLI applies a settings `env` block OVER the process environment, so a pin in
// both places is redundant at best and, for auth, actively dangerous:
//
//   - ANTHROPIC_API_KEY: the CLI prefers it over ANTHROPIC_AUTH_TOKEN when both
//     are present. An inherited key would therefore win over the configured
//     bearer token and point the request at the wrong auth scheme for a custom
//     base URL — strip it whenever a token is pinned.
//   - ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL: keeping an inherited copy in
//     the process env while --settings carries a different one makes
//     "which value is live?" ambiguous when debugging. Strip them when pinned
//     so --settings is the single source (mirrors the remote worker, whose
//     child env is the agent host's own environment with no platform keys).
//
// extras (e.g. GIT_AUTHOR_* / GIT_COMMITTER_* for merges on hosts without
// ~/.gitconfig) are plain non-secret vars with no CLI precedence rules, so
// they stay on the process env exactly as before.
func (g *Gateway) localEnv(model, configID string, extra ...string) []string {
	pinned := g.settingsEnvOverrides(model, configID)
	_, pinToken := pinned["ANTHROPIC_AUTH_TOKEN"]
	_, pinBase := pinned["ANTHROPIC_BASE_URL"]
	dropKeys := map[string]bool{}
	if pinToken {
		dropKeys["ANTHROPIC_API_KEY"] = true // CLI prefers it over AUTH_TOKEN
	}
	if pinToken || pinBase {
		dropKeys["ANTHROPIC_AUTH_TOKEN"] = true
		dropKeys["ANTHROPIC_BASE_URL"] = true
	}
	extraKeys := map[string]bool{}
	for _, kv := range extra {
		if eq := strings.Index(kv, "="); eq > 0 {
			extraKeys[kv[:eq]] = true
		}
	}
	out := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if dropKeys[key] && !extraKeys[key] {
			continue // conflicting inherited auth — --settings owns it now
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// resolveClaudeEnv picks the per-call or global env (token + base URL) from
// the configured provider. Empty configID → the global active config.
// Implementations are expected to fall back to the active config on a stale
// binding, so a missing config id never silently kills a run.
func (g *Gateway) resolveClaudeEnv(configID string) (authToken, baseURL string, err error) {
	if g.claudeEnv == nil {
		return "", "", nil
	}
	return g.claudeEnv.ClaudeEnvForConfigID(configID)
}

// settingSources returns the --setting-sources value to pass to the claude CLI,
// or "" to let the CLI load its default sources (user + project + local).
//
// The claude CLI applies settings `env` blocks OVER the process environment —
// the CLI-default ~/.claude/settings.json first, then our --settings JSON on
// top. When a platform-configured auth token / base URL exists we therefore
// still drop the "user" source: it keeps stray user-settings keys (permissions,
// hooks, a top-level "model") from leaking into a run that is supposed to be
// governed by the claude_configs row alone. We only do this when an override
// is actually present, so a platform with no Claude config still falls back to
// the user's settings.
func (g *Gateway) settingSources(configID string, override *bool) string {
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
	tok, baseURL, err := g.resolveClaudeEnv(configID)
	if err != nil || (tok == "" && baseURL == "") {
		return ""
	}
	return "project,local"
}

// settingsEnvOverrides builds the env block delivered to the claude CLI via
// the --settings JSON string ("{"env":{...}}"). Keys, in order of importance:
//   - ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL — from the resolved
//     claude_configs row (per-role binding via configID, else the global
//     active config). Empty configured values are omitted so the CLI's own
//     defaults still apply on an unconfigured platform.
//   - ANTHROPIC_DEFAULT_{HAIKU,SONNET,OPUS}_MODEL — pinned to `model` when
//     non-empty, so subagents spawned by the Agent tool land on the same
//     endpoint/model as the main agent instead of the CLI's built-in tiers.
//   - ANTHROPIC_MODEL — the session model (the documented env equivalent of
//     the old --model flag).
//   - CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT — set with a
//     non-empty model so the CLI skips its local model-catalog check (custom
//     models behind private base URLs are not in the catalog).
//
// extras are caller-supplied KEY=VALUE entries layered on top (e.g.
// GIT_AUTHOR_* / GIT_COMMITTER_* for merges on hosts without ~/.gitconfig).
//
// This is the SINGLE source of the platform-pinned claude env: the local path
// serializes it into --settings (settingsArg) and the remote path keeps
// sending it as plain env pairs (BuildRemoteEnvPairsWithConfig). Keeping one
// builder means the two execution surfaces can never drift on auth / base
// URL / model pinning.
func (g *Gateway) settingsEnvOverrides(model, configID string, extras ...string) map[string]string {
	overrides := map[string]string{}
	if g.claudeEnv != nil {
		tok, baseURL, err := g.resolveClaudeEnv(configID)
		if err == nil {
			if tok != "" {
				overrides["ANTHROPIC_AUTH_TOKEN"] = tok
			}
			if baseURL != "" {
				overrides["ANTHROPIC_BASE_URL"] = baseURL
			}
		}
	}
	if model != "" {
		overrides["ANTHROPIC_MODEL"] = model
		overrides["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = model
		overrides["ANTHROPIC_DEFAULT_SONNET_MODEL"] = model
		overrides["ANTHROPIC_DEFAULT_OPUS_MODEL"] = model
		overrides["CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT"] = "1"
	}
	for _, kv := range extras {
		if eq := strings.Index(kv, "="); eq > 0 {
			overrides[kv[:eq]] = kv[eq+1:]
		}
	}
	return overrides
}

// settingsArg renders the inline JSON string for `claude --settings`. The claude
// CLI accepts a settings file path OR an inline JSON string; the inline form
// lands at the TOP of the non-managed settings stack, so its "env" block
// overrides the process environment AND any ~/.claude/settings.json on the
// machine. That is where the platform now delivers auth token / base URL /
// model / tier pins on every LOCAL claude spawn — mirroring the remote
// nova-agent-worker's buildSettingsArg, so both execution surfaces launch
// claude the same way (requirement: 本机与 Agent Server 的 Claude 启动方式一致).
//
// Returns "" when there is nothing to pin, so the caller can omit --settings
// entirely and leave the CLI defaults untouched.
//
// Secret handling: the JSON travels in the process argv, visible via `ps`.
// That is the same exposure the remote worker already accepts (it logs the
// redacted command line), and argv is only readable by the same uid — the
// env-var approach this replaces had the identical property.
func (g *Gateway) settingsArg(model, configID string, extras ...string) string {
	envBlock := g.settingsEnvOverrides(model, configID, extras...)
	if len(envBlock) == 0 {
		return ""
	}
	payload, err := json.Marshal(map[string]json.RawMessage{
		"env": mustMarshalMap(envBlock),
	})
	if err != nil {
		// json.Marshal of a flat string map cannot fail in practice; the guard
		// keeps the signature honest without inventing an error path upstream.
		return ""
	}
	return string(payload)
}

// mustMarshalMap serializes a flat string map for the --settings env block.
// Separated from settingsArg only so the error-free invariant is local.
func mustMarshalMap(m map[string]string) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// RedactSettings blanks the auth credentials inside a --settings JSON string so
// the value is safe to log (the handler's logClaudeCmd prints a "copy-paste
// ready" command line, and the job panel renders it verbatim). Base URL /
// model / tier pins are kept in full — those are exactly what an operator
// needs to see when debugging "which gateway did this run hit?".
//
// On a JSON parse failure the raw string is returned unchanged: a malformed
// settings block is itself the thing worth seeing in the log, and hiding it
// entirely would trade a diagnosable bug for an opaque one.
func RedactSettings(jsonStr string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &obj); err != nil {
		return jsonStr
	}
	var env map[string]string
	if raw, ok := obj["env"]; ok {
		if err := json.Unmarshal(raw, &env); err != nil {
			return jsonStr
		}
	}
	if len(env) == 0 {
		return jsonStr
	}
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if v, ok := env[k]; ok && v != "" {
			env[k] = "***"
		}
	}
	b, err := json.Marshal(env)
	if err != nil {
		return jsonStr
	}
	obj["env"] = b
	out, err := json.Marshal(obj)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

// BuildStreamArgs is the public version of streamArgs — exposed so the remote
// Agent-server code path (handler/wizard.go runRemoteCoding) can render the
// same flag list as a remote shell command. The output is byte-identical to
// the unexported streamArgs above; both go through settingSources so a custom
// base URL stays in sync with the local execution.
func (g *Gateway) BuildStreamArgs(opts StreamOpts) []string {
	return g.streamArgs(opts.Prompt, opts.SystemPrompt, opts.Model, opts.ClaudeConfigID, opts.SessionID, opts.Resume, opts.Fork, opts.ForkSessionID, opts.DisallowedTools, opts.PermissionMode, opts.OverrideSettingSources)
}

// BuildRemoteEnvPairs returns ONLY the platform-pinned env entries for a claude
// run on a remote Agent server — auth token, base URL, tier-model pins, and
// the model-window-enforcement bypass. It does NOT inherit os.Environ() from
// the NovaWorkbench host.
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
	return g.BuildRemoteEnvPairsWithConfig(model, "", extras...)
}

// BuildRemoteEnvPairsWithConfig returns ONLY the platform-pinned claude env
// entries for a run on a remote Agent server — auth token, base URL, session
// model, tier-model pins, and the model-window-enforcement bypass. It does NOT
// inherit os.Environ() from the NovaWorkbench host.
//
// The remote nova-agent-worker spawns claude inside the remote host's own
// process environment (its real $HOME / $TMPDIR / $PATH). Inheriting the
// NovaWorkbench host env is what leaked macOS-shaped HOME=/Users/<user> and
// TMPDIR=/var/folders/<...>/T into the Linux agent: the CLI then tried to
// write ~/.claude under the bogus $HOME and `claude --print ping` hung until
// the 5s preflight timeout (surfacing as "preflight_timeout / exit_code 143").
// The remote env must therefore carry only the keys the CLI can't derive from
// the remote host itself.
//
// Key parity with the local path: the pairs are built from the SAME
// settingsEnvOverrides map that serializes into the local --settings JSON.
// The worker folds this map into its own --settings block (buildSettingsArg),
// so both execution surfaces pin identical keys — one builder means they can
// never drift on auth / base URL / model pinning.
func (g *Gateway) BuildRemoteEnvPairsWithConfig(model, configID string, extras ...string) []string {
	overrides := g.settingsEnvOverrides(model, configID, extras...)
	out := make([]string, 0, len(overrides))
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

// streamArgs builds the shared claude CLI flag list for stream-json +
// dangerously-skip-permissions runs. When systemPrompt is non-empty it is passed
// via --system-prompt (full replace). Model + auth + base URL travel INSIDE the
// --settings JSON "env" block (see settingsArg) instead of a --model flag +
// process env — matching the remote nova-agent-worker's invocation shape
// (buildSettingsArg + buildClaudeArgs) so 本机与 Agent Server 的启动方式一致.
//
// configID drives both --settings (the per-config auth + base URL resolution)
// and --setting-sources: when set, the gateway checks the per-config env
// (rather than the global active env) to decide whether the "user"
// ~/.claude/settings.json source must be dropped. The role's binding thus
// controls both the auth + base URL AND whether the CLI applies the
// --setting-sources project,local override.
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
// All flags combine with --system-prompt/--dangerously-skip-permissions.
func (g *Gateway) streamArgs(prompt, systemPrompt, model, configID, sessionID string, resume, fork bool, forkSessionID string, disallowedTools []string, permissionMode string, overrideSettingSources *bool) []string {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	if ss := g.settingSources(configID, overrideSettingSources); ss != "" {
		args = append(args, "--setting-sources", ss)
	}
	if permissionMode == "plan" {
		args = append(args, "--permission-mode", "plan")
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}
	// --settings carries the platform-pinned env (auth token / base URL /
	// model / tier pins) at the top of the CLI's settings stack. Prepended so
	// it lands before the -p block, mirroring the worker's buildClaudeArgs
	// ordering. Empty model AND no configured auth → omitted entirely.
	if sa := g.settingsArg(model, configID); sa != "" {
		args = append([]string{"--settings", sa}, args...)
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
	// ClaudeConfigID pins this claude run to a specific claude_configs row —
	// the gateway looks up that row's auth + base URL and injects them into
	// the subprocess env instead of the global active config. Empty =
	// "fall back to the global active config" (legacy single-config path).
	// Handlers resolve the role's bound config here so the user's chosen
	// model runs against the user's chosen gateway.
	ClaudeConfigID  string
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
	//          ONLY when the resolved (per-config) auth token or base URL is
	//          non-empty. See Gateway.settingSources.
	//   *true → force override: ALWAYS pass --setting-sources project,local,
	//          dropping the user's ~/.claude/settings.json "env" block from
	//          the merged process environment. Required by the remote
	//          Agent-server path because the remote host's settings.json
	//          may carry a stale/wrong base URL that would silently shadow
	//          the platform's claude_configs row.
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
	// Process env keeps only host vars + ExtraEnv; the platform pins (auth /
	// base URL / model / tier pins) travel in the --settings JSON that
	// BuildStreamArgs already emitted. localEnv strips the inherited
	// ANTHROPIC_* keys the settings block owns so there is exactly one source.
	cmd.Env = g.localEnv(opts.Model, opts.ClaudeConfigID, opts.ExtraEnv...)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	}
	// WaitDelay (Go 1.20+): on context cancel, send SIGTERM first and wait up
	// to this long before escalating to SIGKILL. Without it, the runtime
	// races to SIGKILL the Node.js child the moment the parent ctx ends,
	// truncating its session jsonl tail and surfacing as "system exits for no
	// reason" once we count those orphaned writes against the parent's RSS.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// runClaudeStreamJSON runs claude with stream-json + dangerously-skip-permissions,
// which gives Claude full tool-use access to read project files.
// It returns the final result text from the "result" event.
func (g *Gateway) runClaudeStreamJSON(prompt, workDir, systemPrompt, model string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, g.binPath, g.streamArgs(prompt, systemPrompt, model, "", "", false, false, "", nil, "", nil)...)
	cmd.Env = g.localEnv(model, "")
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

// textArgs builds the flag list for a single-shot `--output-format text` run
// (no tool use). It mirrors the streamArgs shape for the settings-related
// flags: the platform pins travel in the --settings JSON and --setting-sources
// drops the user source when a pin is configured, so a quick text call hits
// the exact same model + endpoint as a full streaming run.
func (g *Gateway) textArgs(prompt string) []string {
	args := []string{"-p", prompt, "--output-format", "text"}
	if ss := g.settingSources("", nil); ss != "" {
		args = append(args, "--setting-sources", ss)
	}
	if sa := g.settingsArg("", ""); sa != "" {
		args = append(args, "--settings", sa)
	}
	return args
}

// runClaudeText runs claude in single-shot text mode (no tool use) and returns
// the raw assistant text. Lighter than runClaudeStreamJSON — used for quick
// prompts like title generation where tool access isn't needed.
func (g *Gateway) runClaudeText(prompt string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, g.binPath, g.textArgs(prompt)...)
	cmd.Env = g.localEnv("", "")

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

	cmd := exec.CommandContext(ctx, g.binPath, g.textArgs(prompt)...)
	cmd.Env = g.localEnv("", "")
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
// Returns the command and its cancel function; the caller MUST invoke the cancel
// function when the run completes (the coding handler does so via `defer cancel()`)
// so the long timeout's timer is released rather than held until the deadline.
// Caller streams stdout. systemPrompt/model come from the "developer" role config.
// When opts.SessionID is set with Resume/Fork, the coding turn continues (or forks
// from) the design conversation so the developer inherits the full
// analysis+design context instead of being re-fed it.
func (g *Gateway) GenerateCode(opts StreamOpts) (*exec.Cmd, context.CancelFunc) {
	// Use a long timeout for coding tasks — real implementations can take many minutes.
	codingTimeout := g.timeout
	if codingTimeout < 30*time.Minute {
		codingTimeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), codingTimeout)

	return g.StreamCmd(ctx, opts), cancel
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
