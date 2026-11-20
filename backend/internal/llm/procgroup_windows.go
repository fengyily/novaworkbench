//go:build windows

// Windows counterpart of procgroup_unix.go.
//
// Windows has no process groups in the POSIX sense: there is no Setpgid, and
// `kill(-pgid)` has no equivalent. The closest portable primitive is
// os.Process.Kill (TerminateProcess), which terminates the direct child but
// leaves its descendants alone.
//
// The Agent Server path (the one that actually runs claude on a remote host)
// is Linux-only, and the local CLI on Windows is a rare configuration, so the
// Windows build keeps the SAME API with the weaker semantics. The code that
// depends on these primitives (orphan reaping, stop) degrades to "we cannot
// kill the tree" rather than to "we kill the wrong process".
package llm

import (
	"fmt"
	"os"
	"os/exec"
)

// SetProcGroup is a no-op on Windows: cmd.SysProcAttr stays nil so the OS
// default applies.
func SetProcGroup(cmd *exec.Cmd) {}

// KillProcGroup terminates the process led by pgid. On Windows the "group"
// concept does not exist, so only the direct process can be signalled.
//
// sig is accepted for signature parity with the POSIX implementation; it is
// ignored because TerminateProcess takes no signal.
func KillProcGroup(pgid int, sig os.Signal) error {
	if pgid <= 0 {
		return fmt.Errorf("llm: invalid pgid %d", pgid)
	}
	proc, err := os.FindProcess(pgid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// ProcessAlive reports whether pid still refers to a live process.
//
// os.FindProcess on Windows actually opens a handle to the target, so a
// successful lookup proves the process exists (and the handle is released
// right after). A terminated-but-not-reaped process can still be found, so
// this is a best-effort liveness probe — good enough for the orphan reaper,
// which additionally verifies the command line before killing anything.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}

// ProcessCommand returns the command line of pid. Without /proc there is no
// cheap portable way to read it; the Windows implementation returns "" so
// callers fall back to their "cannot verify → do not kill" branch.
func ProcessCommand(pid int) string {
	return ""
}

// ProcessLooksLikeClaude always returns false on Windows: the command line
// is unavailable, so the orphan reaper must not kill anything it cannot
// positively identify as our own claude process.
func ProcessLooksLikeClaude(cmdline string) bool {
	return false
}
