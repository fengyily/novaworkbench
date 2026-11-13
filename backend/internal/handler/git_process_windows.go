//go:build windows

// Windows stub for the git process hardening (see git_process_unix.go for the
// full rationale — the POSIX-only part is the process-group kill).
//
// Windows has no Setpgid, so the whole-group kill is unavailable. os.Process
// .Kill maps to TerminateProcess, which is abrupt but does terminate the
// direct git process. The WaitDelay half of the fix is portable and is what
// actually guarantees the caller cannot wedge: after the delay os/exec closes
// the I/O pipes and Wait returns even if a helper still holds them.
package handler

import (
	"os/exec"
	"time"
)

// gitWaitDelay — see git_process_unix.go.
const gitWaitDelay = 5 * time.Second

// configureGitProcess prepares a git subprocess so a deadline terminates it.
func configureGitProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = gitWaitDelay
}
