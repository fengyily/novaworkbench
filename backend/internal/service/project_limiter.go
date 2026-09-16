package service

import "sync"

// ProjectLimiter is the per-project admission gate for sub-task execution.
// It is in-memory only (no DB state) and shared by BOTH dispatch paths:
//
//   - the auto-orchestration tick (scheduler.OrchestrationQueue), and
//   - the manual / merge path (handler.SubTaskRunner.Run).
//
// Before this existed each path held its own process-global semaphore, so
// "how many claude children may run at once" was neither project-scoped nor
// runtime-configurable: one long-running project could starve every other
// project, and lowering/raising the cap needed a restart. The gate is keyed on
// projects.id so the queueing the user actually asked for ("子任务从项目的角度
// 排队") is enforced where the contention really is — the project's worktree /
// git branch / dev server.
//
// The cap (max) is a single global integer applied INDEPENDENTLY per project:
// each project may have up to max children in flight, and different projects
// run in parallel. It is read from the settings table (subtask.concurrency)
// and pushed in via SetMax on every tick / Run entry, which is what makes a
// settings change take effect without a restart.
//
// Ordering contract for callers: acquire the project gate FIRST, then the
// process-wide OOM ceiling (the NOVA_SUBTASK_CONCURRENCY channel). Both paths
// follow the same order so the two limits can never deadlock against each
// other.
//
// Counts live only in this process: a crash resets them to zero, which is
// correct — the orphaned "running" rows are reset by
// SubTaskService.RecoverInterrupted at boot, so there is nothing to double
// count.
type ProjectLimiter struct {
	mu     sync.Mutex
	active map[string]int
	max    int
}

// NewProjectLimiter builds the gate with an initial cap. Values below 1 are
// normalized to 1 (a zero cap would wedge every dispatch forever).
func NewProjectLimiter(max int) *ProjectLimiter {
	if max < 1 {
		max = 1
	}
	return &ProjectLimiter{active: map[string]int{}, max: max}
}

// SetMax updates the per-project cap at runtime. Callers push the freshly
// read setting in before each admission attempt, so a PUT /api/settings/subtask
// is picked up by the next tick (≤10s) or the next Run — no restart, and the
// settings handler never needs a reference to the limiter.
//
// Lowering the cap never kills anything already running: the extra in-flight
// children simply keep their slots and TryAcquire refuses new ones until the
// count drains below the new cap.
func (l *ProjectLimiter) SetMax(n int) {
	if n < 1 {
		n = 1
	}
	l.mu.Lock()
	l.max = n
	l.mu.Unlock()
}

// Max returns the current per-project cap (for log / job-panel messages).
func (l *ProjectLimiter) Max() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.max
}

// TryAcquire reserves a slot for projectID, returning false when the project
// is already at its cap. Non-blocking by design: the orchestration tick MUST
// NOT hold a goroutine waiting (it would claim a DB row it can't run, leaving a
// zombie "running" card), so it treats false as "defer to the next tick"; the
// manual path polls instead so the user's click still eventually executes.
//
// An empty projectID (a requirement whose project could not be resolved) is
// bucketed under "" like any other key rather than bypassing the gate — an
// unresolvable project is exactly the case where unbounded fan-out is least
// understood.
func (l *ProjectLimiter) TryAcquire(projectID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[projectID] >= l.max {
		return false
	}
	l.active[projectID]++
	return true
}

// Release returns a slot previously taken by TryAcquire. Safe to call for an
// unknown key (no-op) and clamped at zero so a double release can't push the
// counter negative and silently grant an extra slot. The map entry is dropped
// at zero so a long-lived process doesn't accumulate one entry per project
// ever touched.
func (l *ProjectLimiter) Release(projectID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, ok := l.active[projectID]
	if !ok {
		return
	}
	n--
	if n <= 0 {
		delete(l.active, projectID)
		return
	}
	l.active[projectID] = n
}

// Active reports the number of in-flight slots for projectID. Read-only helper
// for logging / diagnostics.
func (l *ProjectLimiter) Active(projectID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active[projectID]
}
