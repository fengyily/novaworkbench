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

// TestDestructiveCommandReason_AllowsUnrelatedWorktreeRemoval is the
// regression test for req_10fdcd929761f813: a sub-task was killed because it
// tried to remove a scratch worktree it had created itself, with the working
// directory merely mentioned in the `cd` prefix of the command.
//
// The verdict must depend on the path the destructive operation points at,
// not on whether the command string mentions the working directory.
func TestDestructiveCommandReason_AllowsUnrelatedWorktreeRemoval(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	commands := []string{
		"cd " + wd + " && git worktree remove /tmp/basecheck && echo \"scratch worktree removed\" && ls /tmp/basecheck 2>&1 | head -2",
		"cd " + wd + " && rm -rf /tmp/scratch-build",
		"cd " + wd + " && mv /tmp/a /tmp/b",
		"git worktree remove /tmp/other",
		"git worktree remove --force /tmp/other",
		"rm -rf " + wd + "/build",
		"rm -rf build/ 2>&1",
		"rm -rf build/ > /dev/null",
	}
	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", cmd, wd); got != "" {
				t.Errorf("command %q must be allowed, got blocked with: %s", cmd, got)
			}
		})
	}
}

// TestDestructiveCommandReason_BlocksEdgeCases pins the boundary cases the
// operand-based verdict has to keep vetoing: cwd-implicit operators, the
// current directory, its parent, and operands that cannot be resolved
// statically (which fail closed).
func TestDestructiveCommandReason_BlocksEdgeCases(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	cases := []struct{ name, cmd string }{
		{"worktree remove without path", "git worktree remove"},
		{"worktree remove current dir", "git worktree remove ."},
		{"worktree remove parent dir", "git worktree remove .."},
		{"worktree remove workdir", "git worktree remove --force " + wd},
		{"rm -rf current dir", "rm -rf ."},
		{"rm -rf parent dir", "rm -rf .."},
		{"rm -rf glob", "rm -rf *"},
		{"rm -rf $PWD", "rm -rf $PWD"},
		{"rm -rf $HOME", "rm -rf $HOME"},
		{"rm -rf $PWD with redirect", "rm -rf $PWD 2>/dev/null"},
		{"rm -rf home shortcut", "rm -rf ~"},
		{"worktree remove $PWD", "git worktree remove \"$PWD\""},
		{"worktree remove command substitution", "git worktree remove \"$(pwd)\""},
		{"rm -rf parent of workdir", "rm -rf " + filepath.Dir(wd)},
		{"reset hard", "git reset --hard HEAD~1"},
		{"clean fdx", "git clean -fdx"},
		{"clean xfd", "git clean -xfd"},
		{"clean f and x separate", "git clean -f -x"},
		{"mv workdir", "mv " + wd + " /tmp/elsewhere"},
		{"mv parent of workdir", "mv " + filepath.Dir(wd) + " /tmp/elsewhere"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}
}

// TestClassifyPath pins the path-relation helper the whole verdict rests on.
func TestClassifyPath(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	cases := []struct {
		path string
		want pathRelation
	}{
		{wd, relSelf},
		{filepath.Dir(wd), relAncestor},
		{filepath.Join(wd, "build"), relDescendant},
		{"/tmp/basecheck", relUnrelated},
		{"/Users/dev/workspace/repo", relUnrelated},
	}
	for _, c := range cases {
		if got := classifyPath(c.path, wd); got != c.want {
			t.Errorf("classifyPath(%q, %q) = %v, want %v", c.path, wd, got, c.want)
		}
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
