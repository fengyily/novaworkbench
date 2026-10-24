package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the sub-task safety guard introduced after the 2026-10-24
// incident (a sub-task lost its working directory mid-run, then ran
// `rm -rf <cwd>` and a clone loop instead of stopping).

// TestDestructiveCommandReason_BlocksWorkDirRemoval pins the commands the guard
// must veto. Everything here names the work directory explicitly, so a
// well-behaved child could not have meant anything but "delete the repo I am
// standing in".
func TestDestructiveCommandReason_BlocksWorkDirRemoval(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	cases := []struct{ name, cmd string }{
		{"rm -rf absolute", "rm -rf " + wd},
		{"rm -fr", "rm -fr " + wd},
		{"rm from parent", "rm -rf " + filepath.Dir(wd)},
		{"rm via $PWD", "rm -rf $PWD"},
		{"mv workdir", "mv " + wd + " /tmp/elsewhere"},
		// Commands that act on the current directory implicitly.
		{"reset --hard", "git reset --hard"},
		{"clean -fdx", "git clean -fdx"},
		{"worktree remove", "git worktree remove --force " + wd},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}
}

// TestDestructiveCommandReason_AllowsOrdinaryCleanup is the false-positive
// guard: the safety net must not break normal development commands. Build
// artifacts, test fixtures and unrelated paths are all fair game.
func TestDestructiveCommandReason_AllowsOrdinaryCleanup(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	commands := []string{
		"rm -rf build/",
		"rm -rf ./node_modules",
		"git clean -fd",
		"git checkout -- .",
		"go test ./...",
		"gh pr create --base main --head feat/x",
		"rm -rf /tmp/nova-agent",
	}
	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", cmd, wd); got != "" {
				t.Errorf("command %q must be allowed, got blocked with: %s", cmd, got)
			}
		})
	}

	// Non-Bash tools are never inspected.
	if got := destructiveCommandReason("Write", "rm -rf "+wd, wd); got != "" {
		t.Errorf("non-Bash tool must not be blocked, got: %s", got)
	}
}

// TestSubTaskSink_CheckStreamHealth proves the missing-cwd detection fires the
// moment the directory is gone and stays quiet while it exists. This is the
// language-agnostic half of the guard — it does not depend on parsing the CLI's
// English/Chinese error text.
func TestSubTaskSink_CheckStreamHealth(t *testing.T) {
	dir := t.TempDir()
	sink := subTaskSink{workDir: dir}

	if reason := sink.CheckStreamHealth(); reason != "" {
		t.Fatalf("healthy directory must not abort the run, got: %s", reason)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove: %v", err)
	}
	reason := sink.CheckStreamHealth()
	if reason == "" {
		t.Fatal("deleted working directory must abort the run")
	}
	if !strings.Contains(reason, dir) {
		t.Errorf("abort reason should name the directory, got: %s", reason)
	}

	// An unset workDir disables the check rather than matching everything.
	if reason := (subTaskSink{}).CheckStreamHealth(); reason != "" {
		t.Errorf("empty workDir must disable the health check, got: %s", reason)
	}
}

// TestSubTaskSink_GuardToolUse wires the two halves together: a Bash tool_use
// carrying `rm -rf <workDir>` must be vetoed, while a benign command passes.
func TestSubTaskSink_GuardToolUse(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	sink := subTaskSink{workDir: wd}

	if got := sink.GuardToolUse("Bash", map[string]interface{}{"command": "rm -rf " + wd}); got == "" {
		t.Fatal("rm -rf of the work directory must be vetoed")
	}
	if got := sink.GuardToolUse("Bash", map[string]interface{}{"command": "git status"}); got != "" {
		t.Errorf("benign command must pass, got: %s", got)
	}
	if got := sink.GuardToolUse("Bash", nil); got != "" {
		t.Errorf("nil input must pass, got: %s", got)
	}
}

// TestUnderDir pins the containment helper used by the worktree cleanup guard.
// A regression here would re-open the "computed path is trusted blindly" hole.
func TestUnderDir(t *testing.T) {
	root := "/Users/dev/.novaworkbench/worktrees/proj"
	cases := []struct {
		path string
		want bool
	}{
		{root + "/req_abc", true},
		{root + "/req_abc/sub", true},
		{root, false}, // the root itself is not removable
		{"/Users/dev/.novaworkbench", false},
		{"/Users/dev/workspace/repo", false},
		{"", false},
	}
	for _, c := range cases {
		if got := underDir(c.path, root); got != c.want {
			t.Errorf("underDir(%q, %q) = %v, want %v", c.path, root, got, c.want)
		}
	}
}
