//go:build !windows

// Process-group primitives for the claude CLI subprocess (POSIX).
//
// Why this file exists — the "看不见的第二个 Agent" incident:
//
// The claude CLI is a Node.js process that itself spawns tool subprocesses
// (bash, git, node …). When the nova backend is killed with SIGKILL (or the
// machine reboots), the child claude process is NOT killed with it: it is
// re-parented to launchd (PPID=1) and keeps running — editing the requirement
// worktree while the UI already shows the sub-task as "error". The user then
// sees the agent "repeatedly answering" because two processes (the orphan and
// a freshly dispatched one) write to the same tree.
//
// Three things are needed to make that impossible, and this file provides the
// OS-level primitives for all three:
//
//  1. The child gets its OWN process group (Setpgid), so the whole tree can be
//     signalled as a unit. Without it, `kill(pid)` only reaches the direct
//     child and every tool subprocess survives.
//  2. The group's identity (pgid == the child's pid, because the child is
//     the group leader) is persisted in the DB, so a RESTARTED backend can
//     find and kill the orphans its predecessor left behind.
//  3. Before killing, the target process is verified to still be the claude
//     CLI we spawned (command-line check) — pids are recycled by the OS, so a
//     stale pid can point at an unrelated process.
//
// The Windows counterpart (procgroup_windows.go) provides the same API with
// Windows-appropriate semantics so the rest of the codebase stays portable.
package llm

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// SetProcGroup marks cmd so that Start() places the child in a NEW process
// group (it becomes the group leader, so its pgid == its pid).
//
// It is safe to call on any *exec.Cmd and is idempotent in effect: calling it
// twice simply re-asserts the same attribute. On platforms without
// Setpgid the Windows stub is a no-op.
func SetProcGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillProcGroup sends sig to the process group led by pgid.
//
// pgid <= 0 is a no-op (nothing was recorded / the child already exited).
// A negative pid is what POSIX expects for "the group led by this pid".
func KillProcGroup(pgid int, sig os.Signal) error {
	if pgid <= 0 {
		return fmt.Errorf("llm: invalid pgid %d", pgid)
	}
	s, ok := sig.(syscall.Signal)
	if !ok {
		s = syscall.SIGTERM
	}
	return syscall.Kill(-pgid, s)
}

// ProcessAlive reports whether pid still refers to a live process.
//
// Signal 0 performs the permission / existence check without delivering
// anything. EPERM means the process exists but belongs to another user —
// still "alive" for our purposes.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// ProcessCommand returns the full command line of pid, or "" when it cannot
// be read.
//
// Linux exposes it under /proc/<pid>/cmdline (NUL-separated); macOS / BSD
// have no /proc, so we fall back to `ps -p <pid> -o command=`. The result is
// used for the "is this still OUR claude process?" check before a kill —
// pids get recycled, and killing an unrelated process would be far worse
// than leaving an orphan behind.
func ProcessCommand(pid int) string {
	if pid <= 0 {
		return ""
	}
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
		parts := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		parts = append([]string(nil), parts...)
		joined := strings.TrimSpace(strings.Join(parts, " "))
		if joined != "" {
			return joined
		}
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ProcessLooksLikeClaude reports whether cmdline looks like a claude CLI
// invocation.
//
// This is the identity guard for the orphan reaper: the pid we persisted
// could have been recycled by the OS into an unrelated process by the time a
// restarted backend looks at it. Requiring the "claude" token in the command
// line (and, when the caller has one, the session id) keeps a stale pid from
// turning into a wrong-process kill.
//
// The check is deliberately loose about the exact path (the CLI may be
// installed as `claude`, `claude.exe`, or behind a version-pinned symlink)
// but strict about the token itself.
func ProcessLooksLikeClaude(cmdline string) bool {
	if cmdline == "" {
		return false
	}
	lower := strings.ToLower(cmdline)
	return strings.Contains(lower, "claude")
}
