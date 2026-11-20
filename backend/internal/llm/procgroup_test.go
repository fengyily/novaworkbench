package llm

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// TestStreamArgsForkResumeShape pins the argv shape the 「继续执行」 path
// depends on: --resume <old> --fork-session --session-id <new>.
//
// The old behaviour was --resume <old> with NO --fork-session, so the new
// turn was appended to the very file the previous attempt wrote. When that
// attempt was an orphan (a SIGKILLed backend leaves the claude child
// running), two processes were writing the same transcript — and the model
// re-derived the same conclusion over and over.
func TestStreamArgsForkResumeShape(t *testing.T) {
	g := &Gateway{}
	args := g.BuildStreamArgs(StreamOpts{
		Prompt:        "继续完成之前的子任务。",
		SessionID:     "old-session",
		Resume:        true,
		Fork:          true,
		ForkSessionID: "new-session",
	})

	resumeIdx := indexOfArg(args, "--resume")
	if resumeIdx == -1 {
		t.Fatalf("args = %v, want --resume", args)
	}
	if args[resumeIdx+1] != "old-session" {
		t.Errorf("--resume %q, want old-session", args[resumeIdx+1])
	}
	if indexOfArg(args, "--fork-session") == -1 {
		t.Errorf("args = %v, want --fork-session (the new turn must land in its own file)", args)
	}
	sidIdx := indexOfArg(args, "--session-id")
	if sidIdx == -1 {
		t.Fatalf("args = %v, want --session-id", args)
	}
	if args[sidIdx+1] != "new-session" {
		t.Errorf("--session-id %q, want new-session", args[sidIdx+1])
	}
}

// TestStreamArgsResumeInPlaceShape pins the OLD shape is still reachable for
// the paths that want it (a plain --resume with no fork).
func TestStreamArgsResumeInPlaceShape(t *testing.T) {
	g := &Gateway{}
	args := g.BuildStreamArgs(StreamOpts{
		Prompt:    "p",
		SessionID: "only-session",
		Resume:    true,
	})
	if indexOfArg(args, "--resume") == -1 {
		t.Fatalf("args = %v, want --resume", args)
	}
	if indexOfArg(args, "--fork-session") != -1 {
		t.Errorf("args = %v, want no --fork-session", args)
	}
}

// TestProcessCommandReadsTheCommandLine pins the identity probe the orphan
// reaper uses: it must be able to read a live process's command line, and it
// must recognize our own claude CLI in it.
func TestProcessCommandReadsTheCommandLine(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a test process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	pid := cmd.Process.Pid
	if !ProcessAlive(pid) {
		t.Fatal("ProcessAlive = false for a just-started process")
	}
	line := ProcessCommand(pid)
	if line == "" {
		t.Fatal("ProcessCommand returned an empty command line")
	}
	if !strings.Contains(line, "sleep") {
		t.Errorf("ProcessCommand = %q, want it to mention the process name", line)
	}
	// The claude check must NOT match an unrelated process — it is the guard
	// that stops the reaper from killing a recycled pid's new owner.
	if ProcessLooksLikeClaude(line) {
		t.Errorf("ProcessLooksLikeClaude(%q) = true, want false for a non-claude process", line)
	}
}

// TestProcessAliveOnDeadProcess pins the negative case.
func TestProcessAliveOnDeadProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a test process: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	// A reaped pid is not alive.
	if ProcessAlive(pid) {
		// On Linux the pid may linger as a zombie until reaped; the test
		// reaps it via Wait, so it should be gone. Tolerate the rare
		// reuse by only failing when the pid is a DIFFERENT live process.
		if line := ProcessCommand(pid); line != "" && strings.Contains(line, "sleep") {
			t.Errorf("ProcessAlive = true for a reaped process (cmdline %q)", line)
		}
	}
	if ProcessAlive(0) {
		t.Error("ProcessAlive(0) = true, want false")
	}
	if ProcessAlive(-1) {
		t.Error("ProcessAlive(-1) = true, want false")
	}
}

// TestProcessLooksLikeClaude pins the identity check the orphan reaper
// depends on.
func TestProcessLooksLikeClaude(t *testing.T) {
	if ProcessLooksLikeClaude("") {
		t.Error("an empty command line must not match claude")
	}
	if ProcessLooksLikeClaude("/usr/bin/grep -r foo /tmp") {
		t.Error("an unrelated command line must not match claude")
	}
	if !ProcessLooksLikeClaude("/Users/x/.local/bin/claude -p hello --output-format stream-json") {
		t.Error("a claude CLI command line must match")
	}
}

// TestSetProcGroupPutsTheChildInItsOwnGroup pins the process-group
// invariant the whole orphan story rests on: the child must be a group
// leader, so signalling the group reaches its tool subprocesses too.
func TestSetProcGroupPutsTheChildInItsOwnGroup(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	SetProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a test process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Skipf("Getpgid unsupported here: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Errorf("pgid = %d, want %d (the child must lead its own group)", pgid, cmd.Process.Pid)
	}
}
