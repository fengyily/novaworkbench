package handler

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
)

// claudeDocGenerationConcurrencyEnv overrides the platform-wide cap on
// concurrent CLAUDE.md generations.
const claudeDocGenerationConcurrencyEnv = "NOVA_CLAUDE_DOC_GENERATION_CONCURRENCY"

// claudeDocGenerationConcurrencyDefault is the default platform-wide cap.
//
// Two at a time is enough to keep one project moving while another waits,
// and low enough that a burst of "generate" clicks cannot spawn an
// unbounded number of claude processes — each one explores a whole
// repository for minutes.
const claudeDocGenerationConcurrencyDefault = 2

// claudeDocGenGate is the admission gate for the CLAUDE.md generation.
//
// Two levels, and both are needed:
//
//   - Per project: only ONE generation may run for a given project at a
//     time. Two concurrent runs race on the same document row — the later
//     write silently wins, so the user ends up with whichever model
//     happened to finish last — and each of them spawns a claude process
//     that explores the same repository for minutes.
//
//   - Platform-wide: a hard cap on how many generations run at once. The
//     JobStore is a shared ring buffer, so an unbounded number of
//     15-minute runs would eat the platform's job slots.
//
// The per-project slot is taken NON-blocking: the caller learns immediately
// (409 GENERATION_IN_PROGRESS) that a run is already in flight, instead of
// starting a second run that would fight the first one for the document.
// The platform-wide slot is taken BLOCKING inside the background worker, so
// a second PROJECT waits its turn rather than being rejected — the
// contention that actually hurts (two runs on the same document) is the one
// that is refused outright.
//
// The gate is in-memory only, like service.ProjectLimiter: a crash resets
// the counters, which is correct — the claude processes die with the
// backend, so there is nothing left to double count.
type claudeDocGenGate struct {
	mu sync.Mutex
	// active maps a project id to the token of the run that holds its slot.
	// The token makes the release idempotent and keeps a stale release
	// from freeing a slot that a LATER run has already taken.
	active map[string]uint64
	seq    uint64
	// globalCap is the platform-wide cap. 0 = resolve it from the
	// environment on first use, so a zero-value gate still works.
	globalCap int
	semOnce   sync.Once
	sem       chan struct{}
}

// tryAcquire takes the per-project slot for projectID.
//
// ok=false means the project already has a generation in flight. The
// returned release func must be called when the run ends; calling it twice
// is harmless.
func (g *claudeDocGenGate) tryAcquire(projectID string) (release func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active == nil {
		g.active = map[string]uint64{}
	}
	if _, busy := g.active[projectID]; busy {
		return nil, false
	}
	g.seq++
	token := g.seq
	g.active[projectID] = token
	return func() {
		g.mu.Lock()
		if g.active[projectID] == token {
			delete(g.active, projectID)
		}
		g.mu.Unlock()
	}, true
}

// acquireGlobal takes the platform-wide slot, blocking until it is free.
//
// waited reports whether the caller had to wait for a slot — the caller
// uses it to tell the user why nothing is happening yet. err is non-nil
// only when ctx is done first (the run then gives up instead of waiting
// forever behind a stuck generation).
func (g *claudeDocGenGate) acquireGlobal(ctx context.Context) (waited bool, err error) {
	g.semOnce.Do(g.initSem)
	select {
	case g.sem <- struct{}{}:
		return false, nil
	default:
	}
	select {
	case g.sem <- struct{}{}:
		return true, nil
	case <-ctx.Done():
		return true, ctx.Err()
	}
}

// releaseGlobal returns the platform-wide slot. Releasing a slot that was
// never taken is a no-op.
func (g *claudeDocGenGate) releaseGlobal() {
	g.semOnce.Do(g.initSem)
	select {
	case <-g.sem:
	default:
	}
}

// initSem creates the platform-wide semaphore with the resolved cap.
func (g *claudeDocGenGate) initSem() {
	cap := g.globalCap
	if cap < 1 {
		cap = claudeDocGenerationConcurrency()
	}
	g.sem = make(chan struct{}, cap)
}

// claudeDocGenerationConcurrency resolves the platform-wide cap from the
// environment, falling back to the default. Values below 1 are ignored —
// a zero cap would wedge every generation forever.
func claudeDocGenerationConcurrency() int {
	if v := strings.TrimSpace(os.Getenv(claudeDocGenerationConcurrencyEnv)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return claudeDocGenerationConcurrencyDefault
}
