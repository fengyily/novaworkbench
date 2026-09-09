package model

import "time"

// OrchestrationBatch is the coordinator row for one auto-orchestrated dispatch
// run. tryAutoOrchestrate INSERTs exactly one of these alongside N sub_tasks in
// a single transaction; OrchestrationQueue's tick loop then walks the batch by
// batch_seq (atomic ClaimNextPending per child) until every child has reached
// a terminal status, at which point the batch flips to "summarizing" and a
// final summary round writes requirements.coding_plan.
//
// Status lifecycle:
//   dispatching  ─┐
//                 ├──► summarizing ──► completed
//                 └──► errored        (terminal failure)
//
// SummaryStatus runs in parallel on the same row:
//   pending ──► running ──► done
//                  └──► error  (orchestrator summary crashed; tick retries)
//
// summary_heartbeat_at is a 5s heartbeat the RunOrchestratorSummary goroutine
// writes while the summary round is in flight; a stale value (>5min) is the
// signal OrchestrationQueue uses to re-arm summary_status='pending' on the
// next tick (RecoverInterrupted on the sub_tasks side does the same for
// crashed child goroutines).
type OrchestrationBatch struct {
	ID                    string
	RequirementID         string
	OrchestratorSessionID string
	Model                 string
	WorkDir               string
	ClaudeConfigID        string
	TotalChildren         int
	Status                string
	SummaryStatus         string
	SummaryJobID          string
	SummaryHeartbeatAt    *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
	CompletedAt           *time.Time
}

const (
	// Batch-level status.
	BatchDispatching = "dispatching"
	BatchSummarizing = "summarizing"
	BatchCompleted   = "completed"
	BatchErrored     = "errored"

	// Summary-round status (orthogonal to batch.Status).
	SummaryPending = "pending"
	SummaryRunning = "running"
	SummaryDone    = "done"
	SummaryError   = "error"
)
