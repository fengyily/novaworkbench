package scheduler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// fakeExecutor records the calls the queue makes into the handler layer.
// Only DispatchOrphanSubTask matters here, but the interface demands all four.
type fakeExecutor struct {
	dispatched []string
	cutoffs    []time.Time
}

func (f *fakeExecutor) ExecuteOrchestratedChild(*model.OrchestrationBatch, *model.SubTask) {}
func (f *fakeExecutor) RunOrchestratorSummary(string)                                   {}
func (f *fakeExecutor) OnBatchDrained(string)                                             {}

func (f *fakeExecutor) DispatchOrphanSubTask(st *model.SubTask, olderThan time.Time) {
	f.dispatched = append(f.dispatched, st.ID)
	f.cutoffs = append(f.cutoffs, olderThan)
}

func newQueueTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Init(db.Config{
		Driver:     string(db.SQLite),
		SQLitePath: filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Skipf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// insertPending writes a pending sub_task with explicit job_id / batch_id /
// updated_at so the sweep's predicate can be probed directly.
func insertPending(t *testing.T, d *db.DB, id, jobID, batchID string, updatedAt time.Time) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status,
		job_id, batch_id, source, session_mode, report_mode, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "req_orphan", "row "+id, "prompt", model.SubTaskStatusPending,
		jobID, batchID, model.SubTaskSourceManual, model.SubTaskSessionModeFork,
		model.SubTaskReportModeAuto, updatedAt, updatedAt); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// TestSelfHealOrphanPending_DispatchesOnlyUnownedRows is the scheduler-side
// guard for the bug this sweep exists for.
//
// A pending row that no dispatcher owns — no job_id AND no batch_id — used
// to sit at "排队中 · 等待项目空闲" forever: the orchestration queue claims
// by batch_id, the manual path only runs on an explicit user action, and
// the boot recovery only reaches a child row once it carries a job_id (a
// row stranded before that keeps a pending child out of its reach — and
// every Review fix row is a child). The sweep is what gives such a row a
// second chance.
func TestSelfHealOrphanPending_DispatchesOnlyUnownedRows(t *testing.T) {
	d := newQueueTestDB(t)
	subTaskSvc := service.NewSubTaskService(d, nil)
	exec := &fakeExecutor{}
	q := NewOrchestrationQueue(
		d,
		service.NewOrchestrationBatchService(d),
		subTaskSvc,
		service.NewRequirementService(d, nil),
		exec,
		nil, // limiter
		nil, // settingSvc
		nil, // globalSem
		0,   // interval
	)

	old := time.Now().Add(-2 * time.Hour)
	insertPending(t, d, "st_orphan", "", "", old)
	insertPending(t, d, "st_orphan_child", "", "", old)
	insertPending(t, d, "st_withJob", "job_1", "", old)
	insertPending(t, d, "st_withBatch", "", "ob_1", old)
	insertPending(t, d, "st_fresh", "", "", time.Now())

	q.selfHealOrphanPending()

	if len(exec.dispatched) != 2 {
		t.Fatalf("dispatched %v, want exactly the two unowned pending rows", exec.dispatched)
	}
	got := map[string]bool{}
	for _, id := range exec.dispatched {
		got[id] = true
	}
	if !got["st_orphan"] || !got["st_orphan_child"] {
		t.Fatalf("dispatched %v, want st_orphan and st_orphan_child", exec.dispatched)
	}
	if got["st_withJob"] || got["st_withBatch"] || got["st_fresh"] {
		t.Fatalf("dispatched a row that is already owned or too fresh: %v", exec.dispatched)
	}
	// The cutoff handed to the executor must be the same one the row was
	// listed with — the implementation re-checks it when claiming the row.
	for _, c := range exec.cutoffs {
		if c.After(time.Now()) {
			t.Fatalf("cutoff %s is in the future", c)
		}
		if c.Before(time.Now().Add(-2 * orphanPendingMinAge)) {
			t.Fatalf("cutoff %s is older than the sweep window", c)
		}
	}
}
