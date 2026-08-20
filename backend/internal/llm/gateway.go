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
func (g *Gateway) mergedEnv(model string) []string {
	env := os.Environ()
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
	if len(overrides) == 0 {
		return env
	}
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
func (g *Gateway) settingSources() string {
	if g.claudeEnv == nil {
		return ""
	}
	tok, baseURL, err := g.claudeEnv.ClaudeEnvVars()
	if err != nil || (tok == "" && baseURL == "") {
		return ""
	}
	return "project,local"
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
func (g *Gateway) streamArgs(prompt, systemPrompt, model, sessionID string, resume, fork bool, forkSessionID string, disallowedTools []string, permissionMode string) []string {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	if ss := g.settingSources(); ss != "" {
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
	cmd := exec.CommandContext(ctx, g.binPath, g.streamArgs(opts.Prompt, opts.SystemPrompt, opts.Model, opts.SessionID, opts.Resume, opts.Fork, opts.ForkSessionID, opts.DisallowedTools, opts.PermissionMode)...)
	cmd.Env = g.mergedEnv(opts.Model)
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

	cmd := exec.CommandContext(ctx, g.binPath, g.streamArgs(prompt, systemPrompt, model, "", false, false, "", nil, "")...)
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
	if ss := g.settingSources(); ss != "" {
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

// GenerateDescriptionAndTitle reorganizes the user's raw requirement content
// into structured Markdown AND distills a concise title in a single LLM round,
// via the direct HTTP LLM channel (OpenAI-compatible, e.g. DeepSeek). This
// bypasses the claude CLI for speed — neither task needs tool use, and merging
// them keeps the title and body consistent while transmitting the content once.
// The channel activates only when both base_url and api_key are configured;
// otherwise it returns an error and the caller falls back to the raw content
// for Markdown and the first line for the title (no claude CLI fallback, by
// design) so requirement creation never fails just because this is unavailable.
func (g *Gateway) GenerateDescriptionAndTitle(content string) (markdown, title string, usage *Usage, err error) {
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
	return formatAndTitleViaHTTP(baseURL, apiKey, model, content)
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
	if ss := g.settingSources(); ss != "" {
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
