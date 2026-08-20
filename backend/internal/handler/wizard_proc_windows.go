//go:build windows

// Windows stubs for the process-group helpers — POSIX-only Setpgid / Getpgid
// don't exist on Windows, so we fall back to terminating the direct process.
// Windows job objects + CREATE_NEW_PROCESS_GROUP would be the proper
// equivalent but aren't required for our use (claude CLI on Windows doesn't
// spawn grand-children that survive the parent's exit the way it does under
// the Unix shell pipeline).
package handler

import "os/exec"

// setProcessGroup is a no-op on Windows. cmd.SysProcAttr is left nil so the
// OS default applies.
func setProcessGroup(_ *exec.Cmd) {}

// killProcessGroup calls os.Process.Kill on the direct process. On Windows
// os.Process.Kill maps to TerminateProcess — abrupt (no graceful shutdown),
// but it's the closest portable equivalent to the Unix SIGTERM-the-group
// behavior. Use os/exec.Process.Kill rather than syscall.Kill because the
// latter is not exported on Windows in the stdlib syscall package.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}