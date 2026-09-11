package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// hostShapedEnvKeys are the keys that must NEVER leak into a remote Agent-server
// claude run. They are derived from the NovaWorkbench host's environment and are
// meaningless (or actively harmful) on a remote Linux/macOS agent. The regression
// behind this test: BuildEnvPairs (which inherited os.Environ) was accidentally
// used for the remote path and leaked macOS HOME=/Users/... + TMPDIR=/var/folders/...
// into the remote claude, making `claude --print ping` hang until the 5s
// preflight timeout.
var hostShapedEnvKeys = []string{
	"HOME",
	"TMPDIR",
	"TMP",
	"TEMP",
	"PATH",
	"SHELL",
	"TERM",
	"LANG",
	"LC_ALL",
	"USER",
	"LOGNAME",
	"PWD",
	"OLDPWD",
}

type fakeClaudeEnv struct{ tok, baseURL string }

func (f fakeClaudeEnv) ClaudeEnvVars() (string, string, error) { return f.tok, f.baseURL, nil }

// ClaudeEnvForConfigID mirrors the real ClaudeConfigService fallback: empty
// id = same as ClaudeEnvVars; non-empty id returns the per-config env (the
// test fake only models the active-config path, so per-id lookups are not
// differentiated from the global one here).
func (f fakeClaudeEnv) ClaudeEnvForConfigID(id string) (string, string, error) {
	return f.tok, f.baseURL, nil
}

// settingsEnv parses the --settings JSON produced by settingsArg. Fails the
// test when the flag is absent — the pinned-env cases below all require it.
func settingsEnv(t *testing.T, args []string) map[string]string {
	t.Helper()
	for i, a := range args {
		if a != "--settings" || i+1 >= len(args) {
			continue
		}
		var obj struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal([]byte(args[i+1]), &obj); err != nil {
			t.Fatalf("--settings value is not valid JSON: %v (%s)", err, args[i+1])
		}
		return obj.Env
	}
	t.Fatalf("no --settings flag in args: %v", args)
	return nil
}

func TestBuildRemoteEnvPairsDoesNotLeakHostEnv(t *testing.T) {
	g := New(fakeClaudeEnv{tok: "tok-123", baseURL: "https://example.invalid"}, nil)

	pairs := g.BuildRemoteEnvPairs("minimax-M3")

	got := map[string]bool{}
	for _, kv := range pairs {
		k, _, _ := strings.Cut(kv, "=")
		got[k] = true
	}

	// Platform-pinned keys must be present. (There is deliberately no
	// CLAUDE_ALLOW_ROOT — the Claude CLI has no such bypass; root must be
	// handled by provisioning a non-root user, see handler/agent_server.go.)
	for _, want := range []string{
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT",
	} {
		if !got[want] {
			t.Errorf("BuildRemoteEnvPairs missing platform-pinned key %q (pairs=%v)", want, pairs)
		}
	}

	// Host-shaped keys must NOT be present.
	for _, bad := range hostShapedEnvKeys {
		if got[bad] {
			t.Errorf("BuildRemoteEnvPairs leaked host env key %q (pairs=%v)", bad, pairs)
		}
	}
}

// TestLocalStreamArgsCarriesSettingsPins is the requirement's core assertion:
// a LOCAL claude launch must carry model + base URL + auth inside --settings,
// exactly the way the remote nova-agent-worker launches claude.
func TestLocalStreamArgsCarriesSettingsPins(t *testing.T) {
	g := New(fakeClaudeEnv{tok: "tok-123", baseURL: "https://example.invalid"}, nil)

	args := g.BuildStreamArgs(StreamOpts{Prompt: "hi", Model: "minimax-M3"})
	env := settingsEnv(t, args)

	if env["ANTHROPIC_AUTH_TOKEN"] != "tok-123" {
		t.Errorf("settings env ANTHROPIC_AUTH_TOKEN = %q, want tok-123", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if env["ANTHROPIC_BASE_URL"] != "https://example.invalid" {
		t.Errorf("settings env ANTHROPIC_BASE_URL = %q, want the configured base URL", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_MODEL"] != "minimax-M3" {
		t.Errorf("settings env ANTHROPIC_MODEL = %q, want minimax-M3 (session model travels in --settings, not --model)", env["ANTHROPIC_MODEL"])
	}
	for _, tier := range []string{"HAIKU", "SONNET", "OPUS"} {
		k := "ANTHROPIC_DEFAULT_" + tier + "_MODEL"
		if env[k] != "minimax-M3" {
			t.Errorf("settings env %s = %q, want minimax-M3", k, env[k])
		}
	}
	if env["CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT"] != "1" {
		t.Error("settings env missing CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1")
	}

	// --model must be gone: the model now travels inside --settings, matching
	// the worker's buildClaudeArgs (which never emits --model).
	for _, a := range args {
		if a == "--model" {
			t.Errorf("--model flag still present in args %v; it must be replaced by the --settings env block", args)
		}
	}
}

// TestLocalStreamArgsOmitsSettingsWhenUnconfigured: with no Claude config and
// no model there is nothing to pin, so --settings must be absent entirely —
// the CLI then falls back to the user's own settings.json (pre-existing
// unconfigured-platform behavior).
func TestLocalStreamArgsOmitsSettingsWhenUnconfigured(t *testing.T) {
	g := New(fakeClaudeEnv{}, nil)
	args := g.BuildStreamArgs(StreamOpts{Prompt: "hi"})
	for i, a := range args {
		if a == "--settings" {
			t.Errorf("unexpected --settings at %d in args %v", i, args)
		}
	}
}

// TestLocalEnvStripsPinnedAuthKeys: the process env must not carry a stale
// copy of the values --settings now owns. An inherited ANTHROPIC_API_KEY is
// especially dangerous — the CLI prefers it over ANTHROPIC_AUTH_TOKEN.
func TestLocalEnvStripsPinnedAuthKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-inherited")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-inherited")
	t.Setenv("ANTHROPIC_BASE_URL", "https://inherited.invalid")
	t.Setenv("PATH", "/usr/bin:/bin") // host var that must survive

	g := New(fakeClaudeEnv{tok: "tok-123", baseURL: "https://example.invalid"}, nil)
	env := g.localEnv("minimax-M3", "")

	for _, bad := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"} {
		for _, kv := range env {
			if strings.HasPrefix(kv, bad+"=") {
				t.Errorf("localEnv kept pinned key %s in the process env (%q); --settings owns it", bad, kv)
			}
		}
	}
	foundPath := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			foundPath = true
		}
	}
	if !foundPath {
		t.Error("localEnv dropped PATH; host vars must still be inherited")
	}
}

// TestLocalEnvKeepsExtras: GIT_AUTHOR_* / GIT_COMMITTER_* travel on the
// process env (they have no CLI precedence rules) so the merge step's
// `git commit --no-edit` carries a real identity.
func TestLocalEnvKeepsExtras(t *testing.T) {
	g := New(fakeClaudeEnv{}, nil)
	env := g.localEnv("", "", "GIT_AUTHOR_NAME=Nova", "GIT_COMMITTER_NAME=Nova")
	found := false
	for _, kv := range env {
		if kv == "GIT_AUTHOR_NAME=Nova" {
			found = true
		}
	}
	if !found {
		t.Errorf("localEnv dropped ExtraEnv GIT_AUTHOR_NAME=Nova (env=%v)", env)
	}
}

// TestRemoteAndLocalSettingsMatch pins the parity contract: the env map the
// remote worker folds into ITS --settings block (delivered as plain env pairs)
// must contain exactly the same keys as the local --settings JSON.
func TestRemoteAndLocalSettingsMatch(t *testing.T) {
	g := New(fakeClaudeEnv{tok: "tok-123", baseURL: "https://example.invalid"}, nil)

	localEnvMap := settingsEnv(t, g.BuildStreamArgs(StreamOpts{Prompt: "hi", Model: "m1"}))

	remote := map[string]string{}
	for _, kv := range g.BuildRemoteEnvPairs("m1") {
		k, v, _ := strings.Cut(kv, "=")
		remote[k] = v
	}

	for k, v := range remote {
		if localEnvMap[k] != v {
			t.Errorf("parity drift on %s: local --settings=%q, remote env pairs=%q", k, localEnvMap[k], v)
		}
	}
	for k, v := range localEnvMap {
		if remote[k] != v {
			t.Errorf("parity drift on %s: local --settings=%q, remote env pairs missing it (got %q)", k, v, remote[k])
		}
	}
}

// TestRedactSettings blanks the token but keeps the base URL + model visible —
// those are what an operator diffs when debugging "which gateway did this hit?".
func TestRedactSettings(t *testing.T) {
	in := `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-secret","ANTHROPIC_BASE_URL":"https://example.invalid","ANTHROPIC_MODEL":"m1"}}`
	out := RedactSettings(in)
	if strings.Contains(out, "sk-secret") {
		t.Errorf("RedactSettings leaked the token: %s", out)
	}
	if !strings.Contains(out, "https://example.invalid") || !strings.Contains(out, "m1") {
		t.Errorf("RedactSettings dropped base URL / model: %s", out)
	}
	if RedactSettings("not json") != "not json" {
		t.Error("RedactSettings should return malformed input unchanged")
	}
}

func TestBuildEnvPairsStillInheritsHostEnv(t *testing.T) {
	// The local path (StreamCmd) must keep inheriting os.Environ() — we only
	// changed where the platform pins travel, not the host-env semantics.
	g := New(fakeClaudeEnv{}, nil)
	pairs := g.localEnv("", "")
	if len(pairs) == 0 {
		t.Fatal("localEnv returned no entries; expected inherited os.Environ()")
	}
	// Every process has a PATH; the local builder should carry it through.
	foundPath := false
	for _, kv := range pairs {
		if strings.HasPrefix(kv, "PATH=") {
			foundPath = true
			break
		}
	}
	if !foundPath {
		t.Errorf("localEnv did not inherit PATH from os.Environ()")
	}
}
