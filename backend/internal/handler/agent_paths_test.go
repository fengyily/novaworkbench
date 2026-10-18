package handler

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

func TestAgentWorktreeRoot(t *testing.T) {
	cases := []struct{ env, want string }{
		{"", "/tmp/nova-agent"},
		{"/var/lib/nova-agent", "/var/lib/nova-agent"},
		{"/var/lib/nova-agent/", "/var/lib/nova-agent"},
		{"  /data/nova//  ", "/data/nova"},
	}
	for _, tc := range cases {
		t.Setenv("NOVA_AGENT_WORKTREE_ROOT", tc.env)
		if got := agentWorktreeRoot(); got != tc.want {
			t.Errorf("env=%q: got %q, want %q", tc.env, got, tc.want)
		}
	}
}

// TestAgentWorktreeRootPerServer pins the precedence chain for the per-server
// work root: env (operator-wide override) > agent_servers.worktree_root >
// global settings value > /tmp/nova-agent.
func TestAgentWorktreeRootPerServer(t *testing.T) {
	t.Setenv("NOVA_AGENT_WORKTREE_ROOT", "")
	SetAgentWorktreeRootSetting("/data/global-default")
	t.Cleanup(func() { SetAgentWorktreeRootSetting("") })

	cases := []struct {
		name string
		srv  *model.AgentServer
		want string
	}{
		{"nil server falls back to global", nil, "/data/global-default"},
		{"empty column falls back to global", &model.AgentServer{}, "/data/global-default"},
		{"per-server wins over global", &model.AgentServer{WorktreeRoot: "/data/agent-a/"}, "/data/agent-a"},
		{"whitespace-only column falls back", &model.AgentServer{WorktreeRoot: "   "}, "/data/global-default"},
	}
	for _, tc := range cases {
		if got := agentWorktreeRootFor(tc.srv); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}

	// Env is the operator-wide hard override, even when a server has its own
	// value — matching the pre-per-server behaviour documented in the UI.
	t.Setenv("NOVA_AGENT_WORKTREE_ROOT", "/data/forced")
	if got := agentWorktreeRootFor(&model.AgentServer{WorktreeRoot: "/data/agent-a"}); got != "/data/forced" {
		t.Errorf("env override: got %q, want %q", got, "/data/forced")
	}
}

func TestAgentMinFreeMB(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", 1024}, {"2048", 2048}, {"abc", 1024}, {"0", 1024}, {"-5", 1024},
	}
	for _, tc := range cases {
		t.Setenv("NOVA_AGENT_MIN_FREE_MB", tc.env)
		if got := agentMinFreeMB(); got != tc.want {
			t.Errorf("env=%q: got %d, want %d", tc.env, got, tc.want)
		}
	}
}

func TestIsDiskFullError(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"unable to write", "error: unable to write file src/main.go\nerror: unable to write file go.mod", true},
		{"reset index", "fatal: Could not reset index file to revision 'HEAD'.", true},
		{"enospc", "write error: No space left on device", true},
		{"branch exists", "fatal: a branch named 'requirement-x' already exists", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := isDiskFullError(tc.out); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseDfPk(t *testing.T) {
	out := "Filesystem     1024-blocks   Used Available Capacity Mounted on\ntmpfs               989184 789504    199680      80% /tmp\n"
	info, ok := parseDfPk(out)
	if !ok || info.Mount != "/tmp" || info.UsedKB != 789504 || info.AvailKB != 199680 {
		t.Fatalf("unexpected parse: %+v ok=%v", info, ok)
	}
	if _, ok := parseDfPk("garbage"); ok {
		t.Fatal("expected failure on garbage")
	}
}

func TestWorktreeAddCmdsUseRoot(t *testing.T) {
	cmds := worktreeAddCmds("/r/p/base", "/r/p/req_1", "requirement-req_1", "main")
	if len(cmds) != 3 {
		t.Fatalf("want 3 strategies, got %d", len(cmds))
	}
}
