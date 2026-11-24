package service

import (
	"sync"
)

// WorktreeLimiter is the per-WORKING-DIRECTORY admission gate for sub-task
// execution. It is the second half of the concurrency story that
// ProjectLimiter started:
//
//   - ProjectLimiter answers "how many sub-tasks may this PROJECT run at
//     once" (settings → 子任务 → 并发数).
//   - WorktreeLimiter answers "may two sub-tasks write into the SAME
//     directory at once" — and the answer is always no.
//
// Why the second gate exists: the project gate is keyed on projects.id, but
// the thing that actually conflicts is the FILESYSTEM. Two sub-tasks of the
// SAME requirement share one worktree (requirements.worktree_path), so with
// the project cap at 2 they used to run concurrently and both write the same
// files. The observed symptom was "the agent keeps answering the same thing":
// each child's probe result was invalidated by the sibling's edit, so the
// model re-verified and re-stated its conclusion over and over.
//
// The gate is in-memory only (no DB state) and shared by BOTH dispatch paths
// (the manual runner and the orchestration queue), exactly like
// ProjectLimiter.
//
// Ordering contract for callers: the project gate is taken FIRST, then the
// process-wide OOM ceiling, then this one. All three are "hold A while
// waiting for B" in the SAME order on every path, so they can never deadlock
// against each other.
//
// Counts live only in this process: a crash resets them to zero, which is
// correct — the orphaned "running" rows are reset by
// SubTaskService.RecoverInterrupted at boot, and the leftover claude
// processes are reaped by SubTaskService.ReapOrphanChildren.
type WorktreeLimiter struct {
	mu     sync.Mutex
	active map[string]int
}

// NewWorktreeLimiter builds an empty gate.
func NewWorktreeLimiter() *WorktreeLimiter {
	return &WorktreeLimiter{active: map[string]int{}}
}

// TryAcquire reserves the single slot of the given working directory,
// returning false when another sub-task already holds it.
//
// Non-blocking by design: the orchestration tick MUST NOT hold a goroutine
// waiting (it would claim a DB row it can't run, leaving a zombie "running"
// card), so it treats false as "defer to the next tick"; the manual path
// polls instead so the user's click still eventually executes.
//
// An empty dir is bucketed under "" like any other key rather than
// bypassing the gate — two sub-tasks with an unknown working directory are
// exactly the case where unbounded fan-out is least understood.
func (l *WorktreeLimiter) TryAcquire(dir string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[dir] >= 1 {
		return false
	}
	l.active[dir]++
	return true
}

// Release returns the slot previously taken by TryAcquire. Safe to call for
// an unknown key (no-op) and clamped at zero so a double release can't push
// the counter negative and silently grant an extra slot. The map entry is
// dropped at zero so a long-lived process doesn't accumulate one entry per
// directory ever touched.
func (l *WorktreeLimiter) Release(dir string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.active[dir]
	if !ok {
		return
	}
	n--
	if n <= 0 {
		delete(l.active, dir)
		return
	}
	l.active[dir] = n
}

// Active reports the number of in-flight sub-tasks for dir. Read-only helper
// for logging / diagnostics.
func (l *WorktreeLimiter) Active(dir string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active[dir]
}
