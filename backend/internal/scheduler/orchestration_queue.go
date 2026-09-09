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
//	q := scheduler.NewOrchestrationQueue(database, batchSvc, subTaskSvc, wizardH, 2, 10*time.Second)
//	q.Recover()      // reset orphaned running → pending, stale summary → pending
//	q.Start()        // single goroutine, 10s default tick
//	...
//	q.Stop()         // closes ticker; waits up to 2s for in-flight dispatchers
//	q.Kick()         // wake immediately (used after manual summary creation)
package scheduler

import (
	"log"
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
// methods only. The struct keeps three concurrency primitives:
//
//	stopCh — closed by Stop() to ask the loop to exit
//	kickCh — buffer-1 channel; Kick() pushes to wake the loop early
//	runSem — semaphore capping in-flight wizardH calls so a burst of active
//	         batches can't fan out unbounded subprocesses
//
// running is a WaitGroup for in-flight goroutines; Stop() drains it (with a
// timeout) so child subprocesses don't outlive a clean shutdown by too long.
// once / stopOnce guard the Start / Stop transitions so a stray second call
// is a no-op rather than a panic on a closed channel.
type OrchestrationQueue struct {
	db           *db.DB
	batchSvc     *service.OrchestrationBatchService
	subTaskSvc   *service.SubTaskService
	wizardH      SubTaskExecutor
	tickInterval time.Duration

	stopCh  chan struct{}
	kickCh  chan struct{}
	runSem  chan struct{}
	running sync.WaitGroup

	once     sync.Once
	stopOnce sync.Once
}

// NewOrchestrationQueue wires up the queue. concurrency caps in-flight
// wizardH calls (typically 2 — the wizard already throttles heavy claude
// work; the semaphore here mainly protects against a backlog of active
// batches at boot). interval is the polling period for tick(); pass 0 to
// fall back to a 10s default.
func NewOrchestrationQueue(
	database *db.DB,
	batchSvc *service.OrchestrationBatchService,
	subTaskSvc *service.SubTaskService,
	wizardH SubTaskExecutor,
	concurrency int,
	interval time.Duration,
) *OrchestrationQueue {
	if concurrency < 1 {
		concurrency = 2
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &OrchestrationQueue{
		db:           database,
		batchSvc:     batchSvc,
		subTaskSvc:   subTaskSvc,
		wizardH:      wizardH,
		tickInterval: interval,
		stopCh:       make(chan struct{}),
		kickCh:       make(chan struct{}, 1),
		runSem:       make(chan struct{}, concurrency),
	}
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
		log.Printf("[orch] started (interval=%s concurrency=%d)", q.tickInterval, cap(q.runSem))
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
	for i := range batches {
		batch := batches[i]
		// Check stopCh between batches so a long queue can exit mid-iteration.
		select {
		case <-q.stopCh:
			return
		default:
		}
		switch batch.Status {
		case model.BatchDispatching:
			q.tickDispatching(&batch)
		case model.BatchSummarizing:
			q.tickSummarizing(&batch)
		}
	}
}

// tickDispatching handles a single batch whose status is "dispatching".
// Atomically claims the next pending child via ClaimNextPending and, on
// success, spawns the wizard's ExecuteOrchestratedChild goroutine gated by
// the semaphore. On "no pending rows" it checks whether all children have
// reached a terminal state and, if so, flips the batch into summarizing.
func (q *OrchestrationQueue) tickDispatching(batch *model.OrchestrationBatch) {
	st, ok, err := q.subTaskSvc.ClaimNextPending(batch.ID)
	if err != nil {
		log.Printf("[orch] claim %s: %v", batch.ID, err)
		return
	}
	if ok {
		q.running.Add(1)
		go func(b *model.OrchestrationBatch, s *model.SubTask) {
			defer q.running.Done()
			defer func() { <-q.runSem }()
			q.runSem <- struct{}{}
			q.wizardH.ExecuteOrchestratedChild(b, s)
		}(batch, st)
		return
	}
	// No pending row claimed — every child must have hit a terminal status.
	// Count and flip the batch to summarizing if so.
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
func (q *OrchestrationQueue) tickSummarizing(batch *model.OrchestrationBatch) {
	switch batch.SummaryStatus {
	case model.SummaryPending:
		q.running.Add(1)
		go func(b *model.OrchestrationBatch) {
			defer q.running.Done()
			defer func() { <-q.runSem }()
			q.runSem <- struct{}{}
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
	}
}