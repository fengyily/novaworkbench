package handler

import (
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Regression test for the "拆分确认一直卡在『创建中…』" hang.
//
// Root cause: exec.CommandContext kills only the DIRECT `git` process, but
// `git ls-remote` / `git fetch` / `git push` spawn a remote helper
// (git-remote-http / git-remote-https / ssh) as a grandchild and hand it the
// stdout pipe. The grandchild survived the deadline, kept the pipe open, and
// cmd.Wait() blocked forever on it — the timeout was silently ignored and the
// HTTP request never returned.
//
// The test reproduces exactly that shape: a TCP listener that accepts the
// connection and never answers, so git's remote helper blocks on the read.
// gitRunWithTimeout must return an error within (timeout + WaitDelay) instead
// of hanging.

func requireGitBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// blackholeListener starts a TCP listener that accepts connections and then
// holds them open without ever answering — the network equivalent of a
// remote that has gone silent.
func blackholeListener(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	return ln.Addr().String(), func() {
		close(done)
		_ = ln.Close()
		for _, c := range held {
			_ = c.Close()
		}
	}
}

func TestGitRunWithTimeout_TerminatesWhenRemoteGoesSilent(t *testing.T) {
	requireGitBinary(t)

	addr, stop := blackholeListener(t)
	defer stop()

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"remote", "add", "origin", "http://" + addr + "/repo.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	const timeout = 2 * time.Second
	// The ceiling is generous on purpose: the point of the test is that the
	// call TERMINATES, not that it terminates exactly on the deadline.
	const ceiling = 30 * time.Second

	start := time.Now()
	_, err := gitRunWithTimeout(repo, timeout, nil, "ls-remote", "origin")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("ls-remote against a silent remote should fail, got no error")
	}
	if elapsed > ceiling {
		t.Fatalf("gitRunWithTimeout did not honour its %s timeout: took %s (a stuck remote must not hang the caller)", timeout, elapsed)
	}
	// It must not return before the deadline either — otherwise the timeout
	// was not the thing that ended it and the test proves nothing.
	if elapsed < timeout {
		t.Fatalf("gitRunWithTimeout returned after %s, before its %s timeout — the remote was never actually contacted", elapsed, timeout)
	}
}

func TestGitRun_TerminatesWhenRemoteGoesSilent(t *testing.T) {
	requireGitBinary(t)

	addr, stop := blackholeListener(t)
	defer stop()

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"remote", "add", "origin", "http://" + addr + "/repo.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// gitRun has its own (generous) budget — shrink it for the test so the
	// suite stays fast; the property under test is that the call TERMINATES
	// and does not wedge the goroutine forever.
	prev := gitDefaultTimeout
	gitDefaultTimeout = 2 * time.Second
	t.Cleanup(func() { gitDefaultTimeout = prev })

	done := make(chan error, 1)
	go func() {
		_, err := gitRun(repo, "ls-remote", "origin")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("ls-remote against a silent remote should fail, got no error")
		}
	case <-time.After(gitDefaultTimeout + gitWaitDelay + 15*time.Second):
		t.Fatalf("gitRun hung against a silent remote — the process-group kill / WaitDelay hardening is not in effect")
	}
}

// TestSplitStepBudget_ClampsToTheOverallDeadline pins the "the whole
// preparation shares one budget" rule: a step may never be granted more time
// than the overall deadline still allows.
func TestSplitStepBudget_ClampsToTheOverallDeadline(t *testing.T) {
	deadline := time.Now().Add(10 * time.Second)
	if got := splitStepBudget(deadline, 45*time.Second); got > 11*time.Second {
		t.Fatalf("splitStepBudget returned %s for a step budget of 45s with 10s left", got)
	}
	// A step smaller than the remaining budget is granted as-is.
	if got := splitStepBudget(deadline, 3*time.Second); got != 3*time.Second {
		t.Fatalf("splitStepBudget returned %s, want the step's own 3s", got)
	}
	// An expired deadline still yields a usable (tiny) budget so the step
	// fails fast with a real error instead of being skipped silently.
	if got := splitStepBudget(time.Now().Add(-time.Minute), 45*time.Second); got <= 0 {
		t.Fatalf("splitStepBudget returned %s for an expired deadline, want a positive sliver", got)
	}
}

// TestResolveSplitStartRef_SkipsFetchWhenLocalMatchesRemote pins the fast
// path: when the local branch and origin report the SAME sha, no fetch is
// needed — and the LOCAL ref is returned (origin/<branch> may be a stale
// remote-tracking ref that the skipped fetch would not have refreshed).
func TestResolveSplitStartRef_SkipsFetchWhenLocalMatchesRemote(t *testing.T) {
	requireGitBinary(t)

	repo := t.TempDir()
	origin := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", "--bare"},
	} {
		cmd := exec.Command("git", append([]string{"-C", origin}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"checkout", "--quiet", "-b", "featureA"},
		{"commit", "--quiet", "--allow-empty", "-m", "init"},
		{"remote", "add", "origin", origin},
		{"push", "--quiet", "origin", "featureA"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// No fetch is needed: local featureA == origin/featureA. The helper must
	// return the LOCAL ref — and report that the remote answered.
	got, reachable := resolveSplitStartRef(repo, "featureA", nil, true)
	if got != "featureA" {
		t.Fatalf("resolveSplitStartRef = %q, want the local ref %q", got, "featureA")
	}
	if !reachable {
		t.Errorf("reachable = false, want true (origin answered)")
	}

	// A branch that exists nowhere resolves to "" (the caller turns that into
	// the "来源分支不存在" error).
	if got, _ := resolveSplitStartRef(repo, "nope", nil, true); got != "" {
		t.Fatalf("resolveSplitStartRef for a missing branch = %q, want an empty string", got)
	}
}

// TestResolveSplitStartRef_UnknownRemoteIsNotFatal makes sure the fast path
// degrades to the fetch path when origin cannot be reached (no origin at all
// here) instead of failing outright.
func TestResolveSplitStartRef_UnknownRemoteIsNotFatal(t *testing.T) {
	requireGitBinary(t)

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"checkout", "--quiet", "-b", "featureA"},
		{"commit", "--quiet", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// No origin configured → ls-remote fails → the helper falls back to the
	// local branch rather than giving up, and reports the remote as
	// unreachable so the caller skips the pointless fetch.
	got, reachable := resolveSplitStartRef(repo, "featureA", nil, false)
	if got == "" {
		t.Fatalf("resolveSplitStartRef returned an empty ref for an existing local branch")
	}
	if reachable {
		t.Errorf("reachable = true, want false (origin is not configured)")
	}
	if _, err := exec.LookPath(filepath.ToSlash("git")); err != nil {
		t.Fatalf("git vanished mid-test: %v", err)
	}
}
