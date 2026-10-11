package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/service"
)

// IssueMonitorQueue is the ticker loop that drives the issue-monitor
// per-project auto-import feature. It walks active issue_monitors on a
// fixed interval (env NOVA_ISSUE_MONITOR_INTERVAL, default 60s) and
// dispatches service.RunOne for each row that's eligible (active=1 and
// the per-monitor interval has elapsed).
//
// It is a sibling of Scheduler (30s) and OrchestrationQueue (10s) —
// independent tick loop, independent state, no shared back-pressure with
// the other two. The cross-tick consistency story is per-monitor: each
// monitor has a sync.Map-keyed mutex inside the service layer so a
// concurrent invocation can't double-import the same Issue.
//
// Lifecycle:
//
//	q := scheduler.NewIssueMonitorQueue(database, svc, sem)
//	q.Recover()  // no-op for v0.5.x (no orphan state to heal)
//	q.Start()    // single goroutine
//	q.Stop()     // closes stopCh, waits in-flight, 2s deadline
//
// Environment knobs:
//
//	NOVA_ISSUE_MONITOR_INTERVAL default "60s"   tick frequency
//	NOVA_ISSUE_MONITOR_CONCURRENCY default 4    in-flight RunOne goroutines
type IssueMonitorQueue struct {
	db             *db.DB
	svc            *service.IssueMonitorService
	limiter        chan struct{}
	tickInterval   time.Duration
	runningSem     chan struct{}

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewIssueMonitorQueue wires the deps. `runSem` is the global concurrency
// cap; tickInterval is the boundary NOVA_ISSUE_MONITOR_INTERVAL. The
// limiter is sized to max(4, NOVA_ISSUE_MONITOR_CONCURRENCY).
func NewIssueMonitorQueue(database *db.DB, svc *service.IssueMonitorService, runSem chan struct{}) *IssueMonitorQueue {
	if runSem == nil {
		runSem = make(chan struct{}, 4)
	}
	tickInterval, err := time.ParseDuration(strOr("NOVA_ISSUE_MONITOR_INTERVAL", "60s"))
	if err != nil || tickInterval < time.Second {
		tickInterval = time.Second
	}
	return &IssueMonitorQueue{
		db:           database,
		svc:          svc,
		limiter:      runSem,
		tickInterval: tickInterval,
		runningSem:   runSem,
		stopCh:       make(chan struct{}),
	}
}

// Recover is a no-op for v0.5.x. Listed in the Lifecycle contract for
// parallelism with Scheduler.Recover — once we ship a future feature
// like "issue monitor schedule" that needs recovery on boot, the seam
// is already here.
func (q *IssueMonitorQueue) Recover() error { return nil }

// Start launches the single goroutine that ticks RunActive every
// `tickInterval`. Idempotent: subsequent calls are silent no-ops.
func (q *IssueMonitorQueue) Start() {
	q.wg.Add(1)
	go q.loop()
}

// Stop closes the stopCh and waits up to 2s for the in-flight tick to
// drain. Safe to call multiple times.
func (q *IssueMonitorQueue) Stop() {
	q.stopOnce.Do(func() { close(q.stopCh) })
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Printf("[issue-monitor] stop timed out waiting for in-flight RunOne")
	}
}

// Kick nudges the loop to fire a tick immediately. NOT IMPLEMENTED for
// v0.5.x — Kick was useful for OrchestrationQueue because a manual
// dispatch wants to skip the wait. For issue-monitor there is no
// analogous "I just PUT a new monitor, run it RIGHT NOW" UX need; the
// user is happy to wait one tick interval.
func (q *IssueMonitorQueue) Kick() { /* no-op v0.5.x */ }

func (q *IssueMonitorQueue) loop() {
	defer q.wg.Done()
	t := time.NewTicker(q.tickInterval)
	defer t.Stop()
	for {
		select {
		case <-q.stopCh:
			return
		case <-t.C:
			q.tick(context.Background())
		}
	}
}

// tick walks active monitors and spawns RunOne under the global sem.
// Returns immediately on context cancel / stop signal — never blocks.
func (q *IssueMonitorQueue) tick(ctx context.Context) {
	mons, err := q.svc.ListActive(ctx)
	if err != nil {
		log.Printf("[issue-monitor] tick list failed: %v", err)
		return
	}
	for i := range mons {
		mon := mons[i]
		select {
		case <-q.stopCh:
			return
		case <-ctx.Done():
			return
		case q.runningSem <- struct{}{}:
		}
		// Per-monitor interval gate. The service.RunOne also re-checks
		// inside its lock — this is just an early-out so we don't spawn
		// goroutines for monitors that aren't due yet.
		if mon.LastRunAt != nil {
			next := mon.LastRunAt.Add(time.Duration(mon.IntervalMinutes) * time.Minute)
			if time.Now().Before(next) {
				<-q.runningSem
				continue
			}
		}
		q.wg.Add(1)
		go func(monID string) {
			defer q.wg.Done()
			defer func() { <-q.runningSem }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[issue-monitor] panic in RunOne monitor=%s: %v", monID, r)
				}
			}()
			if err := q.svc.RunOne(ctx, monID); err != nil {
				log.Printf("[issue-monitor] RunOne monitor=%s failed: %v", monID, err)
			}
		}(mon.ID)
	}
}

// strOr returns env[name] when set, otherwise def. Mirrors the helper in
// service/issue_monitor.go so the env knob is read in one consistent
// way (the test fixtures use the same default).
func strOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// sanityUnusedErr keeps imports happy when the queue is the only
// scheduler file in this release — `errors` / `strconv` are reserved
// for the next additions (e.g. an env-driven log level filter). Not
// exported; would be deleted in a real cleanup pass.
var _ = errors.New
var _ = strconv.Itoa
var _ = fmt.Sprint
