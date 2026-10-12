package scheduler

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// IssueMonitorLaunchQueue is the dispatch-side counterpart of
// IssueMonitorQueue. The split exists because issue-monitor polling
// (IssueMonitorQueue) and issue-monitor launching (this queue) have
// different cadences, different gates, and different failure modes:
//
//   - IssueMonitorQueue polls every NOVA_ISSUE_MONITOR_INTERVAL (default
//     60s) and asks the platform "any new matching Issue?". It writes
//     requirement rows + enqueue_launches rows. The cost is mostly
//     network I/O + a DB write per matching Issue.
//
//   - IssueMonitorLaunchQueue (this file) polls every NOVA_IMPL_TICK
//     (default 30s) and drains issue_monitor_pending_launches under the
//     per-monitor max_concurrent_launches cap. The cost is starting a
//     claude subprocess — exactly the resource the cap exists to limit.
//
// Without the split, a single tick that imports 10 issues would spawn 10
// claude processes back-to-back (the v0.5.x behavior) and exhaust the
// project limiter / token budget. With the split, RunOne does zero
// dispatch work; this queue controls dispatch concurrency independently.
//
// Lifecycle:
//
//	q := scheduler.NewIssueMonitorLaunchQueue(database, issueMonSvc,
//	        schedSvc, launchFn, tickInterval)
//	q.Recover()  // no-op for v0.5.x (rows are already due)
//	q.Start()    // single goroutine
//	q.Stop()     // closes stopCh, waits for in-flight dispatchers
//
// Environment knobs:
//
//	NOVA_IMPL_TICK default "30s" — drain cadence
//
// launchFn is the wizard entry point (handler.WizardHandler.RunLaunchForMonitor);
// the queue never depends on the handler package directly. Passing the
// service.WizardLaunchFn signature keeps the seam identical to the old
// IssueMonitorService.LaunchFn field (which RunOne no longer uses — the
// dispatch moved here).
type IssueMonitorLaunchQueue struct {
	db          *db.DB
	svc         *service.IssueMonitorService
	schedSvc    *service.ScheduledTaskService
	launchFn    service.WizardLaunchFn
	tickInterval time.Duration

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewIssueMonitorLaunchQueue wires the deps. tickInterval < 1s is clamped
// to 1s to keep the in-memory watch sane. launchFn may be nil — a nil fn
// degrades to "drain rows but skip dispatch", which is what tests and
// recover-only setups want.
func NewIssueMonitorLaunchQueue(
	database *db.DB,
	issueMonSvc *service.IssueMonitorService,
	schedSvc *service.ScheduledTaskService,
	launchFn service.WizardLaunchFn,
	tickInterval time.Duration,
) *IssueMonitorLaunchQueue {
	if tickInterval < time.Second {
		tickInterval = 30 * time.Second
		if v := os.Getenv("NOVA_IMPL_TICK"); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d >= time.Second {
				tickInterval = d
			}
		}
	}
	return &IssueMonitorLaunchQueue{
		db:           database,
		svc:          issueMonSvc,
		schedSvc:     schedSvc,
		launchFn:     launchFn,
		tickInterval: tickInterval,
		stopCh:       make(chan struct{}),
	}
}

// Recover is a no-op for v0.5.x. Listed for parallelism with the other
// schedulers; a future "kill stuck launches on boot" feature would land
// here (analogous to OrchestrationQueue.Recover).
func (q *IssueMonitorLaunchQueue) Recover() error { return nil }

// Start launches the single goroutine that ticks every tickInterval.
// Idempotent.
func (q *IssueMonitorLaunchQueue) Start() {
	q.wg.Add(1)
	go q.loop()
}

// Stop closes stopCh and waits for in-flight dispatchers. Safe to call
// multiple times.
func (q *IssueMonitorLaunchQueue) Stop() {
	q.stopOnce.Do(func() { close(q.stopCh) })
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Printf("[impl-queue] stop timed out waiting for in-flight dispatchers")
	}
}

// Kick nudges the loop to fire a tick immediately. NOT IMPLEMENTED for
// v0.5.x — there's no "I just PUT a new monitor, drain RIGHT NOW" UX need
// beyond RunOne's own tick; the user is happy to wait one tickInterval.
func (q *IssueMonitorLaunchQueue) Kick() { /* no-op v0.5.x */ }

func (q *IssueMonitorLaunchQueue) loop() {
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

// tick walks active monitors and dispatches their due pending launches
// under each monitor's max_concurrent_launches cap.
//
// Per-monitor fan-out is bounded by (cap - in_flight), where:
//
//   - cap = monitor.MaxConcurrentLaunches (default 1; the form sets it).
//   - in_flight = requirements imported by THIS monitor whose status is
//     in (analyzing, designing, designed, developing). See
//     IssueMonitorService.CountInFlightLaunches.
//
// fan-out is 0 when in_flight >= cap (queue full); the row sits in
// pending_launches until a slot frees up naturally (the wizard drives
// the requirement to a terminal status on finish, decrementing in_flight
// on the next tick).
//
// Concurrency / failure: dispatch is wrapped in a goroutine per claim so
// the tick stays short. dispatchOne handles success / failure /
// no-launchFn — failures re-insert the row with exponential backoff via
// IssueMonitorService.RecordLaunchFailure (capped at 5 attempts; after
// that the row sits at run_at = +7d as the operator's signal).
func (q *IssueMonitorLaunchQueue) tick(ctx context.Context) {
	mons, err := q.svc.ListActive(ctx)
	if err != nil {
		log.Printf("[impl-queue] list active monitors failed: %v", err)
		return
	}
	for i := range mons {
		mon := mons[i]
		select {
		case <-q.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		// Snapshot the monitor so dispatch (which re-reads the row via
		// GetByID inside the goroutine) sees the latest persisted state.
		// A monitor toggled active=0 between this read and the dispatch
		// goroutine's launch call gets respected — the launch path's own
		// gates (RunLaunchForMonitor → dispatchLaunch → immediatePreGate)
		// will short-circuit on a stale / terminal requirement status.
		monCopy := mon
		if !q.dispatchMonitor(ctx, monCopy) {
			// dispatchMonitor returned false because the monitor was
			// deleted between ListActive and our claim — log + skip.
			log.Printf("[impl-queue] monitor=%s disappeared mid-tick", monCopy.ID)
			continue
		}
	}
}

// dispatchMonitor is the per-monitor slice of a tick. Returns true on
// "we did something or nothing was due", false on "monitor row gone —
// skip". Goroutines for the actual launches are spawned here so the
// outer tick can immediately move to the next monitor.
func (q *IssueMonitorLaunchQueue) dispatchMonitor(ctx context.Context, mon model.IssueMonitor) bool {
	inFlight, err := q.svc.CountInFlightLaunches(ctx, mon.ID)
	if err != nil {
		log.Printf("[impl-queue] count in-flight failed: monitor=%s err=%v", mon.ID, err)
		return true
	}
	cap := mon.MaxConcurrentLaunches
	if cap < 1 {
		cap = 1
	}
	free := cap - inFlight
	if free <= 0 {
		return true // cap reached; tick will re-evaluate next round
	}
	rows, err := q.svc.ClaimDueLaunches(ctx, mon.ID, free)
	if err != nil {
		log.Printf("[impl-queue] claim due failed: monitor=%s err=%v", mon.ID, err)
		return true
	}
	if len(rows) == 0 {
		return true
	}
	for i := range rows {
		row := rows[i]
		// Per-row goroutine so a slow claude spawn doesn't block the
		// remaining claims. Each goroutine takes its own (re-read)
		// monitor snapshot so a PUT arriving mid-batch doesn't half-apply.
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[impl-queue] panic in dispatchOne row=%s: %v", row.ID, r)
				}
			}()
			q.dispatchOne(ctx, mon.ID, row)
		}()
	}
	return true
}

// dispatchOne invokes launchFn and either drops the row (success) or
// re-inserts with bumped attempts + deferred run_at (failure). The
// monitor row is re-read here so a PUT arriving between this tick's
// ListActive and the dispatch goroutine's launch call sees fresh config
// — the wizard reads DesignModel etc. from this row, not from the row
// the tick snapshotted.
func (q *IssueMonitorLaunchQueue) dispatchOne(ctx context.Context, monitorID string, row service.PendingLaunch) {
	mon, err := q.svc.GetByID(ctx, monitorID)
	if err != nil || mon == nil {
		// Monitor deleted or DB read failed — drop the row, don't retry.
		// Re-inserting would re-queue forever against a missing parent.
		log.Printf("[impl-queue] dispatch monitor vanished: id=%s req=%s err=%v", monitorID, row.RequirementID, err)
		return
	}
	if !mon.Active {
		// Operator paused the monitor mid-flight. Drop the row; the
		// requirement is left in whatever state the wizard last touched.
		log.Printf("[impl-queue] monitor=%s disabled mid-flight; dropping row=%s req=%s", monitorID, row.ID, row.RequirementID)
		return
	}
	if q.launchFn == nil {
		// No launch fn wired (test / recover setup). Drop the row so the
		// queue doesn't grow unboundedly during a misconfiguration.
		log.Printf("[impl-queue] launchFn is nil; dropping row=%s req=%s", row.ID, row.RequirementID)
		return
	}
	spec, err := q.buildLaunchSpec(mon)
	if err != nil {
		log.Printf("[impl-queue] buildLaunchSpec failed: monitor=%s req=%s err=%v", monitorID, row.RequirementID, err)
		_, _ = q.svc.RecordLaunchFailure(ctx, row, fmt.Sprintf("buildLaunchSpec: %v", err), 5)
		return
	}
	if spec == nil {
		// Should not happen (RunOne wouldn't have enqueued), but if it does
		// we drop the row rather than poison the queue.
		log.Printf("[impl-queue] buildLaunchSpec returned nil; dropping row=%s", row.ID)
		return
	}
	mode, _, errCode, errMsg := q.launchFn(ctx, row.RequirementID, *spec)
	if errCode != "" {
		log.Printf("[impl-queue] launch failed: monitor=%s req=%s mode=%s err=%s: %s", monitorID, row.RequirementID, mode, errCode, errMsg)
		_, _ = q.svc.RecordLaunchFailure(ctx, row, fmt.Sprintf("%s: %s", errCode, errMsg), 5)
		return
	}
	// Success: row is already deleted by ClaimDueLaunches, nothing to do.
	if mode == model.LaunchModeScheduled {
		log.Printf("[impl-queue] launched scheduled: monitor=%s req=%s mode=%s", monitorID, row.RequirementID, mode)
	} else {
		log.Printf("[impl-queue] launched: monitor=%s req=%s mode=%s", monitorID, row.RequirementID, mode)
	}
}

// buildLaunchSpec mirrors IssueMonitorService.buildLaunchSpec — the
// queue needs the spec but cannot depend on the service's private
// helper. Kept thin (no normalization) because the row already went
// through normalize on Upsert; the only thing buildLaunchSpec does that
// affects the wire shape is resolve NextRunAt for the scheduled branch,
// which RunOne already persisted into the row's run_at. For the scheduled
// path the queue reuses spec.Schedule.RunAt as-is (RunOne computed it).
//
// Returns (nil, nil) for launch_mode=create_only — but create_only rows
// are NEVER enqueued (RunOne skips them), so the nil here means the row
// is stale and we should drop it.
func (q *IssueMonitorLaunchQueue) buildLaunchSpec(mon *model.IssueMonitor) (*model.LaunchSpec, error) {
	if mon.LaunchMode == model.LaunchModeCreateOnly {
		return nil, nil
	}
	autoPush := mon.AutoPush
	spec := &model.LaunchSpec{
		Mode:                 mon.LaunchMode,
		DesignModel:          mon.DesignModel,
		DesignClaudeConfigID: mon.DesignClaudeConfigID,
		DesignAgentServerID:  mon.DesignAgentServerID,
		CodingModel:          mon.CodingModel,
		CodingClaudeConfigID: mon.CodingClaudeConfigID,
		CodingAgentServerID:  mon.CodingAgentServerID,
		ReadKnowledge:        true,
		SplitTasks:           true,
		AutoPushPR:           &autoPush,
	}
	if mon.LaunchMode == model.LaunchModeScheduled {
		// RunOne already resolved NextRunAt into a concrete RFC3339
		// string and persisted it on the pending_launches row. Here we
		// reconstruct the schedule block using the monitor's recurrence
		// metadata so the scheduled dispatch path sees a coherent shape.
		// The run_at is authoritative from the queue's row, but the
		// wizard will recompute via parseRunAt — passing RunAt is enough.
		spec.Schedule = &model.LaunchScheduleSpec{
			Recurrence: model.SchedRecurOnce,
			RunAt:      time.Now().Add(time.Minute).Format(time.RFC3339),
		}
	}
	return spec, nil
}
