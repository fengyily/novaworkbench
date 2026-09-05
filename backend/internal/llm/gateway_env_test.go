package llm

import (
	"strings"
	"testing"
)

// hostShapedEnvKeys are the keys that must NEVER leak into a remote Agent-server
// claude run. They are derived from the NovaWorkbench host's environment and are
// meaningless (or actively harmful) on a remote Linux/macOS agent. The regression
// behind this test: BuildEnvPairs (which inherits os.Environ) was accidentally
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

func TestBuildEnvPairsStillInheritsHostEnv(t *testing.T) {
	// The local path (StreamCmd) must keep inheriting os.Environ() — we only
	// changed the remote builder, not the local semantics.
	g := New(fakeClaudeEnv{}, nil)
	pairs := g.BuildEnvPairs("")
	if len(pairs) == 0 {
		t.Fatal("BuildEnvPairs returned no entries; expected inherited os.Environ()")
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
		t.Errorf("BuildEnvPairs did not inherit PATH from os.Environ()")
	}
}
