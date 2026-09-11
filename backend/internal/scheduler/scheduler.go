// Package scheduler dispatches scheduled_tasks rows to the wizard's exec
// path on a fixed tick. It owns NO business logic — every row's body
// runs through Executor, which the wizard handler owns (so service /
// scheduler never imports handler; the dependency is handler → scheduler, as
// expected for a backend service).
//
// Lifecycle:
//
//	s := scheduler.New(db, exec)
//	s.Recover()    // promote orphaned running → failed at boot
//	s.Start()      // single goroutine, 30s default tick
//	...
//	s.Stop()       // closes ticker; waits up to 2s for in-flight dispatchers
//
// Environment knobs:
//
//	NOVA_SCHED_INTERVAL       default "30s"   (e.g. "10s", "1m")
//	NOVA_SCHED_CONCURRENCY   default 2       in-flight dispatches
//	NOVA_SCHED_MAX_LATENESS  default "24h"   run_at floor; older pending → failed
package scheduler

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// DesignParams is the wizard-facing shape for a scheduled architect-design.
// Model == "" means "use the role default" — the wizard resolves it via
// roleConfig at execution time so a config switch after the row was
// scheduled still applies.
type DesignParams struct {
	RequirementID string
	Model         string
	ReadKnowledge bool
}

// CodingParams is the wizard-facing shape for a scheduled start-coding.
// Same '' == role-default semantics for Model. AgentServerID == "" means
// local execution. SplitTasks == false routes the wizard to the agent
// persona (end-to-end implementation rather than decomposition).
type CodingParams struct {
	RequirementID string
	BranchName    string
	BaseBranch    string
	Model         string
	AgentServerID string
	SplitTasks    bool
	ReadKnowledge bool
}

// Executor is the wizard's adapter surface. The handler package provides
// the concrete implementation (handler/schedule_executor.go). Every method
// returns the JobStore job id on success; non-nil err means the dispatch
// failed and the row should be marked failed.
type Executor interface {
	RunScheduledDesign(ctx context.Context, p DesignParams) (jobID string, err error)
	RunScheduledCoding(ctx context.Context, p CodingParams) (jobID string, err error)
}

// Scheduler polls scheduled_tasks for due rows and dispatches them through
// the Executor. All fields are unexported; construct via New and mutate via
// the lifecycle methods only.
type Scheduler struct {
	db          *db.DB
	exec        Executor
	svc         *service.ScheduledTaskService
	interval    time.Duration
	maxLateness time.Duration
	sem         chan struct{}
	stopCh      chan struct{}
	doneCh      chan struct{}
	wg          sync.WaitGroup
	once        sync.Once
}

// New builds a Scheduler. The interval / concurrency / maxLateness defaults
// can be overridden via NOVA_SCHED_INTERVAL / NOVA_SCHED_CONCURRENCY /
// NOVA_SCHED_MAX_LATENESS. An invalid env value is logged and ignored so a
// typo doesn't wedge startup.
func New(database *db.DB, exec Executor) *Scheduler {
	interval := parseDuration("NOVA_SCHED_INTERVAL", 30*time.Second)
	concurrency := parseInt("NOVA_SCHED_CONCURRENCY", 2)
	if concurrency < 1 {
		concurrency = 1
	}
	maxLateness := parseDuration("NOVA_SCHED_MAX_LATENESS", 24*time.Hour)
	return &Scheduler{
		db:          database,
		exec:        exec,
		svc:         service.NewScheduledTaskService(database),
		interval:    interval,
		maxLateness: maxLateness,
		sem:         make(chan struct{}, concurrency),
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
}

// Recover promotes any leftover running rows to failed with an explanatory
// error_message. Called once at boot before Start so the first tick sees a
// clean state.
func (s *Scheduler) Recover() (int64, error) {
	n, err := s.svc.RecoverInterrupted()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		log.Printf("[scheduler] recovered %d interrupted scheduled task(s) from previous run", n)
	}
	return n, nil
}

// Start launches the polling goroutine. Stop terminates it. Recover must
// run before Start so the first tick doesn't double-dispatch rows that
// were orphaned by a previous process crash.
func (s *Scheduler) Start() {
	s.once.Do(func() {
		s.wg.Add(1)
		go s.loop()
	})
}

// Stop signals the loop to exit and waits up to 2 seconds for in-flight
// dispatchers to release their sem slots. Dispatchers are not killed — they
// outlive the scheduler and the underlying claude subprocess; only the
// polling loop is bounded.
func (s *Scheduler) Stop() {
	select {
	case <-s.stopCh:
		// already stopped
		return
	default:
		close(s.stopCh)
	}
	// Wait up to 2s for the polling loop to exit. The loop will be in
	// either select (immediate) or mid-tick (already drained). 2s is the
	// sleep between tick.Done draining and the next iteration's select;
	// on a fresh tick the loop is back in select before we wake up.
	select {
	case <-s.doneCh:
	case <-time.After(2 * time.Second):
		log.Printf("[scheduler] Stop timed out waiting for loop to exit")
	}
}

// loop is the single polling goroutine. Same for/select shape as
// handler/sse.go:46 (SSE heartbeat) so the codebase has one consistent
// "long-lived loop" idiom.
func (s *Scheduler) loop() {
	defer s.wg.Done()
	defer close(s.doneCh)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	log.Printf("[scheduler] started (interval=%s concurrency=%d maxLateness=%s)", s.interval, cap(s.sem), s.maxLateness)
	for {
		select {
		case <-s.stopCh:
			log.Printf("[scheduler] stopped")
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// tick runs one polling iteration: expunge zombie tasks, claim due rows,
// dispatch each in a goroutine gated by the semaphore. Safe to call
// concurrently (only ever invoked from the loop above, so this is just
// defensive documentation).
func (s *Scheduler) tick() {
	// now MUST be UTC wall-clock: the write path (service.ScheduledTaskService
	// Create) normalizes run_at to UTC before INSERT, and SQLite/Postgres both
	// compare TIMESTAMP columns as wall-clock text. Using local time here made
	// due-row lookups fire up to a full server UTC-offset early (e.g. +08:00).
	// If TIMESTAMP is ever switched to TIMESTAMPTZ, re-audit Due/FailExpired.
	now := time.Now().UTC()

	// 1. Fail any pending rows older than the maxLateness floor so a
	//    pile-up during a long downtime isn't blindly re-fired. Tasks
	//    within the window still get a normal dispatch attempt.
	if n, err := s.svc.FailExpired(now, s.maxLateness,
		"计划时间已过期（服务器当时离线超过 "+s.maxLateness.String()+"），任务自动放弃。"); err != nil {
		log.Printf("[scheduler] FailExpired error: %v", err)
	} else if n > 0 {
		log.Printf("[scheduler] failed %d expired scheduled task(s)", n)
	}

	// 2. Pull up to 50 due rows. Bound the SELECT cost and the per-tick
	//    dispatch fan-out so a backlog can't burst hundreds of subprocesses
	//    on the next tick.
	due, err := s.svc.Due(now, 50)
	if err != nil {
		log.Printf("[scheduler] Due error: %v", err)
		return
	}
	if len(due) == 0 {
		return
	}

	for _, t := range due {
		select {
		case <-s.stopCh:
			return
		default:
		}
		// Atomic claim: pending → running. Loser sees RowsAffected==0 and
		// moves on (someone else already grabbed it, or it was canceled).
		claimed, err := s.svc.Claim(t.ID, now)
		if err != nil {
			log.Printf("[scheduler] claim %s error: %v", t.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		// Bounded concurrency: take a sem slot before spawning the
		// goroutine so we never have more than cap(s.sem) in-flight.
		s.wg.Add(1)
		go func(t model.ScheduledTask) {
			defer s.wg.Done()
			defer func() { <-s.sem }()
			s.sem <- struct{}{}
			s.dispatch(t)
		}(t)
	}
}

// dispatch routes a claimed row to the matching Executor method. Success
// path doesn't flip the row's status — the wizard exec body's OnFinish
// callback does that, so we don't double-write. Failure path marks the row
// failed immediately with the returned error so the user sees a clear
// cause (the wizard exec body never ran).
//
// The (JobID, SchedID) pair is injected into ctx so the executor's
// OnFinish closure knows which scheduled_tasks row to flip. JobID here is
// the scheduler's bookkeeping slot — the wizard exec body allocates its
// own in-memory JobStore job id internally and returns it via the
// Executor return value (or via OnFinish's first arg).
func (s *Scheduler) dispatch(t model.ScheduledTask) {
	ctx := context.WithValue(context.Background(), SchedCtxKey{}, SchedCtxValue{
		JobID:   "", // reserved — wizard allocates its own JobStore id
		SchedID: t.ID,
	})
	var jobID string
	var err error
	switch t.TaskType {
	case model.SchedTypeDesign:
		jobID, err = s.exec.RunScheduledDesign(ctx, DesignParams{
			RequirementID: t.RequirementID,
			Model:         t.Model,
			ReadKnowledge: t.ReadKnowledge,
		})
	case model.SchedTypeCoding:
		jobID, err = s.exec.RunScheduledCoding(ctx, CodingParams{
			RequirementID: t.RequirementID,
			BranchName:    t.BranchName,
			BaseBranch:    t.BaseBranch,
			Model:         t.Model,
			AgentServerID: t.AgentServerID,
			SplitTasks:    t.SplitTasks,
			ReadKnowledge: t.ReadKnowledge,
		})
	default:
		// Defensive — DB guard at Create time already rejects bad types.
		err = fmt.Errorf("unknown task_type %q", t.TaskType)
	}
	if err != nil {
		log.Printf("[scheduler] dispatch %s (%s → %s) failed: %v", t.ID, t.TaskType, t.RequirementID, err)
		if ferr := s.svc.Finish(t.ID, false, jobID, err.Error()); ferr != nil {
			log.Printf("[scheduler] Finish failed for %s: %v", t.ID, ferr)
		}
		return
	}
	log.Printf("[scheduler] dispatched %s (%s → %s) as job %s", t.ID, t.TaskType, t.RequirementID, jobID)
}

// SchedCtxKey is the context.Value key the scheduler uses to hand the
// scheduled_tasks row id to Executor implementations. SchedCtxValue
// carries the (JobID, SchedID) pair; JobID is reserved for future use
// (today wizard allocates its own JobStore job id internally and ignores
// the scheduler-side slot). Both types are exported so the handler
// package's Executor impl can read them — unexported duplicates in two
// packages are different Go types and context.Value lookup would fail
// silently (issue: "scheduler ctx not provided").
type SchedCtxKey struct{}

type SchedCtxValue struct {
	JobID   string
	SchedID string
}

// parseDuration reads an env var as a time.Duration. Falls back to def on
// empty / unparseable. Keeps startup robust to typos in env files.
func parseDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("[scheduler] invalid %s=%q, falling back to %s", key, v, def)
		return def
	}
	if d < time.Second {
		log.Printf("[scheduler] %s=%q too small, clamping to 1s", key, v)
		d = time.Second
	}
	return d
}

func parseInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("[scheduler] invalid %s=%q, falling back to %d", key, v, def)
		return def
	}
	return n
}