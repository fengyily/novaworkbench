//go:build !windows

// Process-group helpers for the claude CLI subprocess (POSIX). When run via
// the wizard pipeline we put claude in its own process group so we can
// SIGTERM the entire group on completion — this reaps tool/MCP grandchildren
// that some third-party proxies leave holding the stdout pipe open.
package handler

import (
	"os/exec"
	"syscall"
)

// setProcessGroup marks cmd so Start() places it in a fresh process group.
// No-op on platforms where SysProcAttr.Setpgid doesn't exist (handled by
// wizard_proc_windows.go).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGTERM to the claude subprocess's process group
// (set up via Setpgid in runClaudeStream). It is idempotent and safe to call
// after the process has already exited (errors are ignored).
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// Fall back to signaling just the direct process.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
}