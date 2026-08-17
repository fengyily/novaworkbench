package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// EnvProvider supplies configurable environment variables for the claude CLI
// subprocess (auth token / base URL), sourced from the settings table by the
// SettingService. nil means "use the process environment only".
type EnvProvider interface {
	ClaudeEnvVars() (authToken, baseURL string, err error)
	// LLMConfig returns the direct HTTP LLM channel config (base URL, API key,
	// model) used for lightweight tasks like requirement formatting + title
	// distillation. Called per request so runtime setting updates apply
	// immediately.
	LLMConfig() (baseURL, apiKey, model string, err error)
}

type Gateway struct {
	binPath     string
	timeout     time.Duration
	envProvider EnvProvider
}

func New(envProvider EnvProvider) *Gateway {
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

	// Verify claude CLI is available
	if _, err := exec.LookPath(binPath); err != nil {
		fmt.Printf("[LLM] WARNING: 'claude' CLI not found in PATH. AI features will use stub responses.\n")
		fmt.Printf("[LLM] Install with: npm install -g @anthropic-ai/claude-code\n")
	}

	return &Gateway{binPath: binPath, timeout: timeout, envProvider: envProvider}
}

func (g *Gateway) GetBinPath() string { return g.binPath }

// mergedEnv returns the process environment with the configured Claude settings
// (ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL) overriding any inherited values.
// Empty configured values are not injected, so the API default / inherited env
// still applies.
//
// Auth precedence: the claude CLI prefers ANTHROPIC_API_KEY over
// ANTHROPIC_AUTH_TOKEN, so an inherited ANTHROPIC_API_KEY would silently shadow
// a user-configured bearer token (and point it at the wrong auth scheme for a
// custom base URL). When a token is configured, we therefore strip any inherited
// ANTHROPIC_API_KEY from the child environment so the configured token wins.
func (g *Gateway) mergedEnv() []string {
	env := os.Environ()
	if g.envProvider == nil {
		return env
	}
	tok, baseURL, err := g.envProvider.ClaudeEnvVars()
	if err != nil {
		return env
	}
	overrides := map[string]string{}
	if tok != "" {
		overrides["ANTHROPIC_AUTH_TOKEN"] = tok
	}
	if baseURL != "" {
		overrides["ANTHROPIC_BASE_URL"] = baseURL
	}
	if len(overrides) == 0 {
		return env
	}
	// Keys to strip from the inherited env because they would conflict with the
	// configured auth. Only strip when we are actually injecting ANTHROPIC_AUTH_TOKEN.
	dropKeys := map[string]bool{}
	if tok != "" {
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
// while swapping the role persona via --system-prompt at each fork. All three
// flags can combine with --system-prompt/--model/--dangerously-skip-permissions.
func (g *Gateway) streamArgs(prompt, systemPrompt, model, sessionID string, resume, fork bool, disallowedTools []string, permissionMode string) []string {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
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
	Fork            bool // --fork-session; only meaningful when Resume is true
	DisallowedTools []string
	PermissionMode  string // "plan" to use --permission-mode plan, empty for --dangerously-skip-permissions
}

// StreamCmd returns an unstarted *exec.Cmd configured for stream-json output
// with full tool use. The caller owns the lifecycle (Start/Wait) and parses the
// stream itself — this is the pattern for anything that needs live progress.
// The stream-json "system"/"init" event carries the session_id of the run; for
// a forked run that id is NEW (CLI-generated) and the caller must read it back
// from the stream to persist it for later --resume.
func (g *Gateway) StreamCmd(ctx context.Context, opts StreamOpts) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.binPath, g.streamArgs(opts.Prompt, opts.SystemPrompt, opts.Model, opts.SessionID, opts.Resume, opts.Fork, opts.DisallowedTools, opts.PermissionMode)...)
	cmd.Env = g.mergedEnv()
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

	cmd := exec.CommandContext(ctx, g.binPath, g.streamArgs(prompt, systemPrompt, model, "", false, false, nil, "")...)
	cmd.Env = g.mergedEnv()
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

	cmd := exec.CommandContext(ctx, g.binPath, "-p", prompt, "--output-format", "text")
	cmd.Env = g.mergedEnv()

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
func (g *Gateway) GenerateDescriptionAndTitle(content string) (markdown, title string, err error) {
	if g.envProvider == nil {
		return "", "", fmt.Errorf("llm not configured: no env provider")
	}
	baseURL, apiKey, model, err := g.envProvider.LLMConfig()
	if err != nil {
		return "", "", fmt.Errorf("llm config unavailable: %w", err)
	}
	if baseURL == "" || apiKey == "" {
		return "", "", fmt.Errorf("llm not configured: base_url and api_key required")
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
