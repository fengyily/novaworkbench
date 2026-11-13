//go:build !windows

// Process-group hardening for git subprocesses (POSIX).
//
// Why this file exists — the "拆分确认一直卡在『创建中…』" bug:
//
// `git fetch` / `git push` / `git ls-remote` are not single processes. They
// spawn `git-remote-https` (or `ssh`, or a credential helper) as
// grandchildren and hand it the stdout/stderr pipes.
//
// The old helper was:
//
//	ctx, cancel := context.WithTimeout(...)
//	cmd := exec.CommandContext(ctx, "git", ...)
//	cmd.Stdout = &stdout   // an io.Writer ⇒ os/exec creates an OS pipe
//	err := cmd.Run()
//
// When the context deadline fires, exec.CommandContext kills only the
// DIRECT `git` process. The grandchild `git-remote-https` survives, keeps the
// write end of the stdout pipe open, and `cmd.Run()` → `cmd.Wait()` blocks
// waiting for EOF on that pipe — forever. The timeout is silently ignored
// and the HTTP request never returns: the UI sits on "创建中…" until the user
// reloads the page.
//
// The fix has two halves, and BOTH are needed:
//
//  1. The git process gets its own process group, and the cancel hook kills
//     the whole group (SIGKILL — `git-remote-https` may not act on SIGTERM).
//     Nothing survives to hold the pipe open.
//  2. `cmd.WaitDelay` bounds how long `Wait` may block on the pipes. Even if
//     some grandchild leaks (a helper we did not anticipate), Wait returns
//     instead of wedging the caller forever.
//
// Half 2 alone would fix the hang; half 1 alone would too. Both together mean
// a stuck git can neither hang the request nor leak processes.
package handler

import (
	"os/exec"
	"syscall"
	"time"
)

// gitWaitDelay — how long os/exec may keep waiting on the child's I/O pipes
// after the process was signalled, before it force-closes them.
//
// Five seconds is long enough for git to flush and exit on its own after
// SIGTERM, and short enough that a wedged remote never stalls a user-facing
// request for more than a few seconds past its deadline.
const gitWaitDelay = 5 * time.Second

// configureGitProcess prepares a git subprocess so that a context deadline
// (or a plain kill) ACTUALLY terminates it.
//
// Call it right before Start(); it mutates cmd.SysProcAttr / cmd.Cancel /
// cmd.WaitDelay.
func configureGitProcess(cmd *exec.Cmd) {
	// Own process group ⇒ the git process and every helper it spawns share a
	// pgid we can signal as a unit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			// SIGKILL, not SIGTERM: `git-remote-https` blocked in a socket
			// read does not always act on SIGTERM, and a survivor is exactly
			// what wedges cmd.Wait().
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			return nil
		}
		return cmd.Process.Kill()
	}
	// Belt and braces: even if a helper escaped the group, Wait stops
	// blocking on its pipes after this delay.
	cmd.WaitDelay = gitWaitDelay
}
