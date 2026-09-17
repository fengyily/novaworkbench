// Package scheduler's orchestration_queue.go owns the tick loop that drives
// auto-orchestrated sub-task dispatch. It's a peer to scheduler.go but holds
// no shared state with it — the two loops are independent (one walks
// scheduled_tasks for design/coding runs the user scheduled up-front, the
// other walks orchestration_batches for runs the wizard decided to split
// after start-coding). Keeping them separate keeps each tick's invariants
// simple: scheduled_tasks deals with future-dated work and back-pressure,
// orchestration_batches deals with restart-safe in-flight sequencing.
//
// Lifecycle:
//
//	q := scheduler.NewOrchestrationQueue(database, batchSvc, subTaskSvc, reqSvc,
//	        wizardH, limiter, settingSvc, globalSem, 10*time.Second)
//	q.Recover()      // reset orphaned running → pending, stale summary → pending
//	q.Start()        // single goroutine, 10s default tick
//	...
//	q.Stop()         // closes ticker; waits up to 2s for in-flight dispatchers
//	q.Kick()         // wake immediately (used after manual summary creation)
package scheduler

import (
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// SubTaskExecutor is the wizard's adapter surface for the two orchestration
// actions. The handler package owns the concrete implementation; the scheduler
// only depends on these two methods. Both methods are blocking — the queue
// owns a goroutine around each call so the tick itself stays short.
type SubTaskExecutor interface {
	// ExecuteOrchestratedChild runs the given child sub-task to terminal
	// status (done | error). The sub-task row has already been claimed
	// (status='running') by the queue before this is called, so the wizard
	// must NOT call MarkRunning itself; it only needs to drive the
	// execution and call Finish when the underlying claude process exits.
	ExecuteOrchestratedChild(batch *model.OrchestrationBatch, st *model.SubTask)
	// RunOrchestratorSummary drives the summary round for a single batch —
	// typically forks the orchestrator session and writes the consolidated
	// coding_plan back to requirements. The queue has already flipped the
	// batch to status='summarizing' before calling; this method is
	// responsible for MarkSummary('running') at entry, periodic heartbeats,
	// and the terminal MarkSummary('done'|'error') + MarkCompleted() on
	// success.
	RunOrchestratorSummary(batchID string)
}

// OrchestrationQueue polls orchestration_batches for active rows and
// dispatches their children (and summary round) through SubTaskExecutor.
// All fields are unexported; construct via New and mutate via the lifecycle
// methods only. The struct keeps these concurrency primitives:
//
//	stopCh  — closed by Stop() to ask the loop to exit
//	kickCh  — buffer-1 channel; Kick() pushes to wake the loop early
//	limiter — the PER-PROJECT admission gate, shared with SubTaskRunner so the
//	          manual path queues behind the same cap (see service.ProjectLimiter)
//	runSem  — process-wide OOM ceiling (env NOVA_SUBTASK_CONCURRENCY), also
//	          shared with SubTaskRunner: the total number of claude children
//	          across ALL projects can never exceed it
//
// The two gates are always taken in the order limiter → runSem (and released
// in reverse) on both dispatch paths, so they can't deadlock against each
// other.
//
// running is a WaitGroup for in-flight goroutines; Stop() drains it (with a
// timeout) so child subprocesses don't outlive a clean shutdown by too long.
// once / stopOnce guard the Start / Stop transitions so a stray second call
// is a no-op rather than a panic on a closed channel.
type OrchestrationQueue struct {
	db         *db.DB
	batchSvc   *service.OrchestrationBatchService
	subTaskSvc *service.SubTaskService
	// reqSvc resolves batch.RequirementID → requirements.project_id. The
	// orchestration_batches row carries no project_id, and the per-project gate
	// needs one, so every dispatch does this lookup before admission.
	reqSvc *service.RequirementService
	// settingSvc supplies the sub-task policy (per-project concurrency, auto
	// retry on/off, retry cap). Re-read at the top of every tick so a settings
	// change lands within one interval without a restart.
	settingSvc   *service.SettingService
	limiter      *service.ProjectLimiter
	wizardH      SubTaskExecutor
	tickInterval time.Duration

	stopCh  chan struct{}
	kickCh  chan struct{}
	runSem  chan struct{}
	running sync.WaitGroup

	once     sync.Once
	stopOnce sync.Once

	// staleAfter is the heartbeat cutoff used by selfHealStaleRunning: a
	// 'running' row whose batch_id_seq_run is older than this gets flipped
	// back to 'pending' so the tick can re-claim it. Read once at
	// construction from env NOVA_ORCH_STALE_AFTER (default 2 minutes);
	// MUST stay strictly greater than the 5s MarkHeartbeat interval or a
	// live goroutine would race its own heartbeat.
	staleAfter time.Duration
}

// NewOrchestrationQueue wires up the queue.
//
// limiter is the per-project admission gate (shared with handler.SubTaskRunner
// so manual and automatic dispatch queue behind ONE cap per project); its max
// is refreshed from settings on every tick. globalSem is the process-wide
// ceiling, also shared with the runner; pass nil to run without one (tests).
// interval is the polling period for tick(); pass 0 to fall back to a 10s
// default.
func NewOrchestrationQueue(
	database *db.DB,
	batchSvc *service.OrchestrationBatchService,
	subTaskSvc *service.SubTaskService,
	reqSvc *service.RequirementService,
	wizardH SubTaskExecutor,
	limiter *service.ProjectLimiter,
	settingSvc *service.SettingService,
	globalSem chan struct{},
	interval time.Duration,
) *OrchestrationQueue {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if limiter == nil {
		limiter = service.NewProjectLimiter(service.DefaultSubTaskProjectConcurrency)
	}
	staleAfter := 2 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("NOVA_ORCH_STALE_AFTER")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 30*time.Second {
			staleAfter = d
		} else if err != nil {
			log.Printf("[orch] invalid NOVA_ORCH_STALE_AFTER=%q: %v (using default %s)", raw, err, staleAfter)
		} else {
			log.Printf("[orch] NOVA_ORCH_STALE_AFTER=%s too short (<30s); using default %s to avoid racing the 5s heartbeat", raw, staleAfter)
		}
	}
	return &OrchestrationQueue{
		db:           database,
		batchSvc:     batchSvc,
		subTaskSvc:   subTaskSvc,
		reqSvc:       reqSvc,
		settingSvc:   settingSvc,
		limiter:      limiter,
		wizardH:      wizardH,
		tickInterval: interval,
		stopCh:       make(chan struct{}),
		kickCh:       make(chan struct{}, 1),
		runSem:       globalSem,
		staleAfter:   staleAfter,
	}
}

// subTaskPolicy reads the current sub-task execution policy, pushing the
// per-project cap into the shared limiter as a side effect (this is what makes
// a settings change hot). Falls back to the defaults when no setting service
// was wired or the read fails — a settings hiccup must never stall dispatch.
func (q *OrchestrationQueue) subTaskPolicy() (autoRetry bool, retryMax int) {
	if q.settingSvc == nil {
		return false, service.DefaultSubTaskRetryMax
	}
	conc, autoRetry, retryMax, err := q.settingSvc.SubTaskConfig()
	if err != nil {
		log.Printf("[orch] read sub-task settings: %v (using %d/%v/%d)", err, conc, autoRetry, retryMax)
	}
	q.limiter.SetMax(conc)
	return autoRetry, retryMax
}

// acquireSlot takes the per-project gate and then the process-wide ceiling,
// returning false (having released whatever it took) when either is full. The
// tick must never block on a slot: blocking here would either stall every other
// batch in the same tick or — worse — claim a DB row it cannot run.
func (q *OrchestrationQueue) acquireSlot(projectID string) bool {
	if !q.limiter.TryAcquire(projectID) {
		return false
	}
	if q.runSem == nil {
		return true
	}
	select {
	case q.runSem <- struct{}{}:
		return true
	default:
		q.limiter.Release(projectID)
		return false
	}
}

// releaseSlot is the exact inverse of acquireSlot (ceiling first, then the
// project gate). Only call it after acquireSlot returned true.
func (q *OrchestrationQueue) releaseSlot(projectID string) {
	if q.runSem != nil {
		<-q.runSem
	}
	q.limiter.Release(projectID)
}

// projectIDFor resolves the project a batch belongs to (via its requirement)
// so the per-project gate has a key. Returns false when the requirement can't
// be read — the caller skips the batch this tick rather than dispatching it
// outside the gate.
func (q *OrchestrationQueue) projectIDFor(batch *model.OrchestrationBatch) (string, bool) {
	// No requirement service wired (tests): fall back to the shared "" bucket
	// so dispatch stays gated rather than stopping altogether.
	if q.reqSvc == nil {
		return "", true
	}
	req, err := q.reqSvc.Get(batch.RequirementID)
	if err != nil || req == nil {
		log.Printf("[orch] resolve project for batch %s (req %s): %v", batch.ID, batch.RequirementID, err)
		return "", false
	}
	return req.ProjectID, true
}

// Recover is the boot-time companion to Start. It delegates to the two
// services' Recover-style entry points: the sub_tasks side flips stale
// "running" children back to "pending" so the next tick re-claims them, and
// the orchestration_batches side resets orphaned summary goroutines (no live
// heartbeat in the last 5 minutes) to "pending" so the next tick re-arms
// them. Recover must run before Start so the first tick doesn't observe
// half-recovered state. Returns the total affected rows from both passes
// for ops logging; non-nil error is bubbled up so main.go can fail loud on
// a corrupt DB.
func (q *OrchestrationQueue) Recover() error {
	nSub, err := q.subTaskSvc.RecoverInterrupted()
	if err != nil {
		return err
	}
	nBatch, err := q.batchSvc.Recover()
	if err != nil {
		return err
	}
	if nSub > 0 || nBatch > 0 {
		log.Printf("[orch] boot recovery: %d sub_tasks + %d batches reset", nSub, nBatch)
	}
	return nil
}

// Start launches the polling goroutine. Recover must run first so the first
// tick doesn't observe pre-recovery state. Subsequent calls are no-ops
// (sync.Once).
func (q *OrchestrationQueue) Start() {
	q.once.Do(func() {
		log.Printf("[orch] started (interval=%s per-project=%d process-ceiling=%d)",
			q.tickInterval, q.limiter.Max(), cap(q.runSem))
		go q.loop()
	})
}

// Stop signals the loop to exit and waits up to 2 seconds for in-flight
// goroutines to release their sem slots. The wizard's subprocesses
// themselves are not killed — they outlive the queue, matching the
// existing scheduler.Stop() contract. A second Stop call is a no-op.
func (q *OrchestrationQueue) Stop() {
	q.stopOnce.Do(func() {
		close(q.stopCh)
	})
	// The loop drops out of select on the closed stopCh and waits on
	// q.running before returning; we just race it with a deadline so a
	// stuck subprocess can't hang the process indefinitely.
	done := make(chan struct{})
	go func() {
		q.running.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Printf("[orch] Stop timed out waiting for in-flight goroutines")
	}
}

// Kick triggers an immediate tick without waiting for the next interval
// fire. Used by tryAutoOrchestrate after creating a new batch (so the first
// child doesn't wait up to 10s) and by the manual-summary handler after
// creating a summarizing-only batch. Non-blocking: a Kick issued while one
// is already pending is coalesced into a single extra tick.
func (q *OrchestrationQueue) Kick() {
	select {
	case q.kickCh <- struct{}{}:
	default:
		// already pending — skip; the next tick will pick up whatever's new
	}
}

// loop is the single polling goroutine. Same for/select shape as
// scheduler.loop() so the codebase has one consistent "long-lived loop"
// idiom. On stopCh it tears down the ticker and waits for in-flight
// dispatchers to drain; on ticker.C / kickCh it runs one tick.
func (q *OrchestrationQueue) loop() {
	ticker := time.NewTicker(q.tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-q.stopCh:
			log.Printf("[orch] stopped")
			return
		case <-ticker.C:
			q.tick()
		case <-q.kickCh:
			q.tick()
		}
	}
}

// tick runs one polling iteration. It's safe to call concurrently — only
// ever invoked from the loop above, so this is defensive documentation
// (matches scheduler.tick()).
func (q *OrchestrationQueue) tick() {
	// Pull up to 20 active batches per tick. ListActive orders by updated_at
	// ASC so a backlog drains FIFO; the limit caps per-tick work and the
	// next tick picks up whatever didn't fit.
	batches, err := q.batchSvc.ListActive(20)
	if err != nil {
		log.Printf("[orch] listActive: %v", err)
		return
	}
	// Read the policy once per tick (and refresh the limiter's cap with it) so
	// every batch in this pass sees the same configuration.
	autoRetry, retryMax := q.subTaskPolicy()
	for i := range batches {
		batch := batches[i]
		// Check stopCh between batches so a long queue can exit mid-iteration.
		select {
		case <-q.stopCh:
			return
		default:
		}
		// The per-project gate needs a project id; a batch whose requirement
		// can't be read is skipped rather than dispatched ungated.
		projectID, ok := q.projectIDFor(&batch)
		if !ok {
			continue
		}
		switch batch.Status {
		case model.BatchDispatching:
			// Self-heal BEFORE dispatch: flip any 'running' row whose
			// heartbeat is older than q.staleAfter back to 'pending' so
			// tickDispatching's ClaimNextPending can re-claim it this tick.
			// Without this, a goroutine that died mid-execution (panic in
			// runClaudeStream, missing early-exit guard, etc.) leaves the
			// row stuck at 'running' forever — RecoverInterrupted only
			// runs at startup, so the stuck state survives until a
			// backend reboot. See selfHealStaleRunning docstring for the
			// cutoff-vs-heartbeat safety reasoning.
			q.selfHealStaleRunning(&batch)
			q.tickDispatching(&batch, projectID, autoRetry, retryMax)
		case model.BatchSummarizing:
			q.tickSummarizing(&batch, projectID)
		}
	}
}

// tickDispatching handles a single batch whose status is "dispatching".
//
// Lifecycle (must stay in this order):
//
//  1. Try to grab a slot for this batch's PROJECT (per-project gate, then the
//     process-wide ceiling) — both non-blocking. If either is full we MUST
//     return without claiming any row, otherwise ClaimNextPending would flip
//     the next pending child to status='running' and the wizard's UI would
//     show it as "运行中" even though nothing is executing. Without this guard
//     every 10s tick would advance one row past the capacity, leaving a
//     growing backlog of zombie "running" rows whose goroutines never started
//     a claude process. Leaving the row 'pending' is exactly what makes the
//     card render "排队中".
//  2. With the slot held, call ClaimNextPending to atomically promote the
//     lowest-seq pending row to status='running'. If the claim fails (DB
//     error) or no row was claimable (the previous tick already promoted this
//     batch's next row), release the slot before returning so we don't leak it.
//  3. On a successful claim, spawn the wizard's ExecuteOrchestratedChild
//     goroutine; its deferred releaseSlot frees the slot when the child
//     finishes.
//
// On "no pending rows AND slot was released" we first give failed children a
// chance to be re-armed (when the user enabled subtask.auto_retry), and only
// then run the terminal-count path that flips the batch into summarizing.
func (q *OrchestrationQueue) tickDispatching(batch *model.OrchestrationBatch, projectID string, autoRetry bool, retryMax int) {
	// (1) Non-blocking admission. If full, defer until the next tick — the DB
	// row stays 'pending' so the UI shows "排队中" instead of a misleading
	// "运行中".
	if !q.acquireSlot(projectID) {
		log.Printf("[orch] tick %s: project %s at capacity (%d), deferring next claim",
			batch.ID, projectID, q.limiter.Max())
		return
	}

	// (2) Promote the next pending row. The slot is held across this DB write
	// so a parallel queue instance (or this queue's own earlier goroutine
	// that's still draining) can't race past us.
	st, ok, err := q.subTaskSvc.ClaimNextPending(batch.ID)
	if err != nil {
		log.Printf("[orch] claim %s: %v", batch.ID, err)
		q.releaseSlot(projectID)
		return
	}
	if ok {
		q.running.Add(1)
		go func(b *model.OrchestrationBatch, s *model.SubTask) {
			defer q.running.Done()
			defer q.releaseSlot(projectID)
			q.wizardH.ExecuteOrchestratedChild(b, s)
		}(batch, st)
		return
	}
	// No row to claim — release the slot we grabbed above so the next
	// tick can try again. (Common when another tick already flipped the
	// row between our admission check and our SELECT.)
	q.releaseSlot(projectID)

	// No pending row claimed — every child has hit a terminal status. Before
	// treating the batch as finished, optionally re-arm the failed children:
	// "子任务失败自动重做" is opt-in (settings key subtask.auto_retry, default
	// off) and capped by retry_max, mirroring the summary-retry branch in
	// tickSummarizing. A re-armed row leaves the 'error' bucket, so this MUST
	// run before CountTerminalByBatch — otherwise the batch would flip to
	// summarizing a tick before the retry got dispatched.
	if autoRetry {
		n, rerr := q.subTaskSvc.ReArmErroredForRetry(batch.ID, retryMax)
		if rerr != nil {
			log.Printf("[orch] re-arm errored children %s: %v", batch.ID, rerr)
		} else if n > 0 {
			log.Printf("[orch] batch %s: re-armed %d errored children for auto retry (max %d)",
				batch.ID, n, retryMax)
			// Next tick claims them in batch_seq order.
			return
		}
	}

	// Count and flip the batch to summarizing if every child is terminal.
	done, errored, err := q.subTaskSvc.CountTerminalByBatch(batch.ID)
	if err != nil {
		log.Printf("[orch] countTerminal %s: %v", batch.ID, err)
		return
	}
	if batch.TotalChildren > 0 && done+errored >= batch.TotalChildren {
		if err := q.batchSvc.MarkSummarizing(batch.ID); err != nil {
			log.Printf("[orch] markSummarizing %s: %v", batch.ID, err)
			return
		}
		log.Printf("[orch] batch %s -> summarizing (%d done / %d errored)", batch.ID, done, errored)
	}
}

// tickSummarizing handles a single batch whose status is "summarizing".
// Arms a new summary goroutine when summary_status='pending'; checks the
// heartbeat and re-arms when summary_status='running' but the heartbeat is
// stale (>5min).
//
// The summary round spawns a claude process of its own, so it goes through the
// same per-project gate as the children — otherwise a project at its cap could
// still fan out one extra process per summarizing batch. A full gate simply
// leaves summary_status='pending' for the next tick.
func (q *OrchestrationQueue) tickSummarizing(batch *model.OrchestrationBatch, projectID string) {
	switch batch.SummaryStatus {
	case model.SummaryPending:
		if !q.acquireSlot(projectID) {
			log.Printf("[orch] tick %s: project %s at capacity (%d), deferring summary",
				batch.ID, projectID, q.limiter.Max())
			return
		}
		q.running.Add(1)
		go func(b *model.OrchestrationBatch) {
			defer q.running.Done()
			defer q.releaseSlot(projectID)
			q.wizardH.RunOrchestratorSummary(b.ID)
		}(batch)
	case model.SummaryRunning:
		// Heartbeat stale: either NULL (goroutine crashed before its first
		// heartbeat) or older than 5 minutes (goroutine hung / deadlocked).
		// Reset to pending so the next tick re-arms a fresh summary.
		stale := batch.SummaryHeartbeatAt == nil ||
			time.Since(*batch.SummaryHeartbeatAt) > 5*time.Minute
		if stale {
			if err := q.batchSvc.MarkSummary(batch.ID, model.SummaryPending); err != nil {
				log.Printf("[orch] markSummary %s: %v", batch.ID, err)
				return
			}
			log.Printf("[orch] batch %s summary heartbeat stale, reset pending", batch.ID)
		}
	case model.SummaryError:
		// 失败超过上限 → 翻 BatchErrored，避免无限重试
		if batch.SummaryAttempts >= model.SummaryMaxAttempts {
			if err := q.batchSvc.MarkStatus(batch.ID, model.BatchErrored); err != nil {
				log.Printf("[orch] markErrored %s: %v", batch.ID, err)
				return
			}
			log.Printf("[orch] batch %s summary exhausted %d attempts, flipped to errored",
				batch.ID, batch.SummaryAttempts)
			return
		}
		if err := q.batchSvc.ResetErrorToPending(batch.ID); err != nil {
			log.Printf("[orch] resetSummaryError %s: %v", batch.ID, err)
			return
		}
		log.Printf("[orch] batch %s summary_status=error, re-armed pending (attempt %d/%d)",
			batch.ID, batch.SummaryAttempts+1, model.SummaryMaxAttempts)
	}
}

// selfHealStaleRunning flips 'running' rows in this batch whose heartbeat
// (batch_id_seq_run) is older than q.staleAfter back to 'pending' so the
// next tickDispatching call can re-claim them. This is the live-tick
// counterpart to RecoverInterrupted's orchestrated pass — same SQL shape,
// but (a) scoped to a single batch_id, (b) parameterized on a tighter
// cutoff, (c) safe to invoke on every 10s tick.
//
// The cutoff (default 2 minutes, env NOVA_ORCH_STALE_AFTER) MUST stay
// strictly greater than the 5-second MarkHeartbeat interval, otherwise a
// live goroutine whose DB write is just slow would race its own heartbeat
// reset and the next ClaimNextPending would re-claim the same
// (batch_id, batch_seq) — double-dispatching a single sub-task to two
// concurrent claude processes. The minimum 30s bound in the constructor
// enforces this from the env side.
//
// Caller order matters: this MUST run BEFORE tickDispatching. tickDispatching
// acquires a slot, then calls ClaimNextPending, then spawns the goroutine;
// if we healed rows first, the slot acquisition in the SAME tick can then
// claim one of those freshly-flipped rows. Without healing first, the
// running rows keep blocking the batch's terminal count (CountTerminalByBatch
// ignores 'running') and the dispatching batch can never flip to
// summarizing — even after the user manually retries the requirement.
//
// Observability: every row flipped gets its own [orch] line carrying the
// child_id / session_id / job_id / heartbeat_age. These four fields are
// what distinguish a real orphan recover (heartbeat_age ~ cutoff, no
// live goroutine) from a false-positive that killed a still-live child
// (heartbeat_age <= staleAfter but the heartbeat goroutine had stopped
// ticking). Operators inspecting the next self-heal can grep for the
// pattern; the structured fields stay machine-parseable for ops dashboards.
func (q *OrchestrationQueue) selfHealStaleRunning(batch *model.OrchestrationBatch) {
	rows, err := q.subTaskSvc.RecoverStaleRunningInBatch(batch.ID, q.staleAfter)
	if err != nil {
		log.Printf("[orch] self-heal %s: %v", batch.ID, err)
		return
	}
	if len(rows) == 0 {
		return
	}
	log.Printf("[orch] batch %s: self-healed %d stale running rows back to pending (cutoff %s)",
		batch.ID, len(rows), q.staleAfter)
	for _, r := range rows {
		// Truncate the session id in logs (UUID v4 → keep first 8 hex) so a
		// log-grep regex doesn't need to escape the full 36-char tail.
		sidShort := r.SessionID
		if len(sidShort) > 8 {
			sidShort = sidShort[:8] + "…"
		}
		jobShort := r.JobID
		if jobShort == "" {
			jobShort = "<empty>"
		}
		log.Printf("[orch]   self-healed child_id=%s session_id=%s job_id=%s heartbeat_age=%s",
			r.ID, sidShort, jobShort, r.HeartbeatAge)
	}
}