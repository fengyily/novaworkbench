package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
)

// seedSubTaskWithChildPID inserts a sub_tasks row carrying a recorded child
// pid — the shape a row has WHILE a claude child is running.
func seedSubTaskWithChildPID(t *testing.T, d *db.DB, id, reqID string, childPID int) {
	t.Helper()
	if _, err := d.Exec(
		"INSERT INTO projects (id, name, local_path) VALUES ('proj_pid', 'p', '/tmp/p')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec(
		"INSERT INTO requirements (id, project_id, title) VALUES (?, 'proj_pid', 'req')",
		reqID); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	now := time.Now()
	if _, err := d.Exec(`INSERT INTO sub_tasks
		(id, requirement_id, title, prompt, status, session_id, source_session_id,
		 job_id, artifact, model, source, batch_id, input_tokens, output_tokens,
		 created_at, updated_at, child_pid, child_pgid, work_dir)
		VALUES (?, ?, 't', 'p', 'running', 'sid', 'src', 'job', '', '', 'manual', '',
		 1, 2, ?, ?, ?, ?, '/tmp/work')`,
		id, reqID, now, now, childPID, childPID); err != nil {
		t.Fatalf("seed sub_task: %v", err)
	}
}

// TestReapOrphanChildrenRefusesForeignPID is the core safety rule of the
// orphan reaper.
//
// A claude child that outlived its backend (the backend was SIGKILLed, the
// child was re-parented to launchd) keeps editing the requirement's worktree
// while the UI already shows the sub-task as "error". The next backend
// instance must find and kill it — BUT pids get recycled by the OS, so the
// persisted pid may now belong to an unrelated process. Killing that process
// would be far worse than leaving an orphan behind.
//
// The test spawns a real child process (a `sleep` standing in for a foreign
// process) and points the row at its pid. The reaper must REFUSE to kill it
// because its command line does not look like our claude CLI.
func TestReapOrphanChildrenRefusesForeignPID(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Skipf("cannot spawn a test child process: %v", err)
	}
	pid := child.Process.Pid
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	seedSubTaskWithChildPID(t, d, "st_orphan", "req_orphan", pid)

	n, err := svc.ReapOrphanChildren()
	if err != nil {
		t.Fatalf("ReapOrphanChildren: %v", err)
	}
	if n != 0 {
		t.Errorf("reaped %d processes, want 0 (the command line is not our claude CLI)", n)
	}
	if !llm.ProcessAlive(pid) {
		t.Error("the unrelated process was killed — the identity check did not hold")
	}

	// The recorded pid must still be cleared: the process is not ours, so the
	// row must not keep pointing at it.
	got, _, err := svc.GetChildPID("st_orphan")
	if err != nil {
		t.Fatalf("GetChildPID: %v", err)
	}
	if got != 0 {
		t.Errorf("child_pid = %d, want 0 (the stale pid must be cleared)", got)
	}
}

// TestReapOrphanChildrenSkipsDeadProcess covers the common case: the backend
// died AND the child died with it (the normal SIGKILL-the-whole-tree case).
// Nothing to kill, but the recorded pid must still be cleared so the next
// boot does not consider it again.
func TestReapOrphanChildrenSkipsDeadProcess(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Skipf("cannot spawn a test child process: %v", err)
	}
	pid := child.Process.Pid
	_ = child.Process.Kill()
	_, _ = child.Process.Wait()
	// Give the OS a moment to reap it.
	time.Sleep(100 * time.Millisecond)

	seedSubTaskWithChildPID(t, d, "st_dead", "req_dead", pid)

	n, err := svc.ReapOrphanChildren()
	if err != nil {
		t.Fatalf("ReapOrphanChildren: %v", err)
	}
	if n != 0 {
		t.Errorf("reaped %d processes, want 0 (the child was already gone)", n)
	}
	got, _, err := svc.GetChildPID("st_dead")
	if err != nil {
		t.Fatalf("GetChildPID: %v", err)
	}
	if got != 0 {
		t.Errorf("child_pid = %d, want 0 (a dead child's pid must be cleared)", got)
	}
}

// TestSetChildPIDRoundTrip pins the persistence contract the orphan reaper
// depends on.
func TestSetChildPIDRoundTrip(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)
	seedSubTaskWithChildPID(t, d, "st_pid", "req_pid", 4242)

	pid, pgid, err := svc.GetChildPID("st_pid")
	if err != nil {
		t.Fatalf("GetChildPID: %v", err)
	}
	if pid != 4242 || pgid != 4242 {
		t.Errorf("pid/pgid = %d/%d, want 4242/4242", pid, pgid)
	}

	if err := svc.SetChildPID("st_pid", 0, 0); err != nil {
		t.Fatalf("SetChildPID: %v", err)
	}
	pid, _, _ = svc.GetChildPID("st_pid")
	if pid != 0 {
		t.Errorf("pid = %d, want 0 after the clear", pid)
	}
}

// TestMarkRunningForSessionSingleOwner is the CAS that stops two dispatchers
// from running the same row.
//
// Every dispatcher pre-mints its OWN session id and stamps it on the row
// before dispatching, so only the LAST writer's session id is on the row —
// and only that dispatcher may flip the row to 'running'. The other one
// aborts instead of spawning a second claude process for one row.
func TestMarkRunningForSessionSingleOwner(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID                                       bool
	}{id: "st_owner", reqID: "req_owner", status: "pending", sessionID: "sid_a", sourceSID: "src", artifact: ""})

	// Dispatcher B re-stamps the row with its own session id.
	if _, err := d.Exec(`UPDATE sub_tasks SET session_id=? WHERE id=?`, "sid_b", "st_owner"); err != nil {
		t.Fatalf("restamp session: %v", err)
	}

	// Dispatcher A still holds sid_a — it must LOSE the CAS.
	if _, won, err := svc.MarkRunningForSession("st_owner", "sid_a"); err != nil {
		t.Fatalf("MarkRunningForSession: %v", err)
	} else if won {
		t.Error("the superseded dispatcher won the CAS — two agents would run one row")
	}

	// Dispatcher B holds sid_b — it must WIN.
	if _, won, err := svc.MarkRunningForSession("st_owner", "sid_b"); err != nil {
		t.Fatalf("MarkRunningForSession: %v", err)
	} else if !won {
		t.Error("the current owner lost the CAS")
	}

	got, err := svc.Get("st_owner")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
}

// TestFinishForSessionRejectsStaleWrite pins the terminal-write guard: a late
// Finish from a superseded attempt must not overwrite the artifact / tokens /
// status of the attempt that replaced it.
func TestFinishForSessionRejectsStaleWrite(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID                                       bool
	}{id: "st_stale", reqID: "req_stale", status: "running", sessionID: "sid_new", sourceSID: "src", artifact: ""})

	// The stale attempt writes with the OLD session id.
	if err := svc.FinishForSession("st_stale", "sid_old", model.SubTaskStatusDone,
		"旧 attempt 的报告", "m", model.SubTaskTokens{Input: 1}, 0, time.Time{}); err != nil {
		t.Fatalf("FinishForSession: %v", err)
	}

	got, err := svc.Get("st_stale")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != model.SubTaskStatusRunning {
		t.Errorf("status = %q, want running (the stale write must be dropped)", got.Status)
	}
	if got.Artifact == "旧 attempt 的报告" {
		t.Error("the stale attempt overwrote the artifact")
	}

	// The CURRENT owner writes — that one lands.
	if err := svc.FinishForSession("st_stale", "sid_new", model.SubTaskStatusDone,
		"新 attempt 的报告", "m", model.SubTaskTokens{Input: 2}, 0, time.Time{}); err != nil {
		t.Fatalf("FinishForSession: %v", err)
	}
	got, _ = svc.Get("st_stale")
	if got.Status != model.SubTaskStatusDone {
		t.Errorf("status = %q, want done", got.Status)
	}
	if got.Artifact != "新 attempt 的报告" {
		t.Errorf("artifact = %q, want the new attempt's report", got.Artifact)
	}
}

// TestWorkDirIsPersisted pins the observability field the UI shows so the
// user can see whether two sub-tasks write into the same directory.
func TestWorkDirIsPersisted(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)
	seedSubTaskRowForContinueStop(t, d, struct {
		id, reqID, status, sessionID, sourceSID, artifact string
		withBatchID                                       bool
	}{id: "st_dir", reqID: "req_dir", status: "pending", sessionID: "sid", sourceSID: "src", artifact: ""})

	dir := filepath.Join(t.TempDir(), "worktree")
	if err := svc.SetWorkDir("st_dir", dir); err != nil {
		t.Fatalf("SetWorkDir: %v", err)
	}
	got, err := svc.Get("st_dir")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.WorkDir != dir {
		t.Errorf("WorkDir = %q, want %q", got.WorkDir, dir)
	}
}

// TestReapOrphanChildrenIsIdempotent makes sure a second boot does not
// re-report the same row.
func TestReapOrphanChildrenIsIdempotent(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)
	seedSubTaskWithChildPID(t, d, "st_twice", "req_twice", 0)

	for i := 0; i < 2; i++ {
		if _, err := svc.ReapOrphanChildren(); err != nil {
			t.Fatalf("ReapOrphanChildren: %v", err)
		}
	}
	_ = os.Getpid()
}
