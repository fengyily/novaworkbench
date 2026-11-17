package handler

// review_auto_iterate_test.go — the requirement-level Review's
// auto-iteration chain.
//
// The chain's contract, in one sentence: after a round finishes, the backend
// creates that round's P0/P1 fix sub-tasks on its own, waits for them, and
// starts the next round — until the code is clean, the budget runs out, or
// something fails.
//
// The tests below pin the three things that can break it:
//
//  1. the DECISION (a pure function — what happens after a round);
//  2. the IDEMPOTENCE (the chain may be driven by an event hook AND by the
//     reconcile tick, and by both at once — a round must never be advanced
//     twice, and a fix batch must never be created twice);
//  3. the API CONTRACT (what the user's request does to the persisted row).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// ── the pure decision ──────────────────────────────────────────────────────

// TestDecideAutoIterate pins the chain's rules.
//
// The table is the whole specification: for a given round outcome, the chain
// either continues or stops, and the stop REASON is part of the contract
// (the frontend renders it, and an operator reads it in the database).
func TestDecideAutoIterate(t *testing.T) {
	cases := []struct {
		name         string
		round        int
		totalRounds  int
		reviewFailed bool
		p0, p1       int
		hasFindings  bool
		wantAction   string
		wantState    string
		wantNote     string
	}{
		{
			name: "P0/P1 cleared → converged", round: 1, totalRounds: 2,
			p0: 0, p1: 0, hasFindings: true,
			wantAction: autoIterateActionStop, wantState: autoIterateStateConverged, wantNote: autoIterateNoteConverged,
		},
		{
			name: "only P3 findings → converged", round: 1, totalRounds: 3,
			p0: 0, p1: 0, hasFindings: true,
			wantAction: autoIterateActionStop, wantState: autoIterateStateConverged, wantNote: autoIterateNoteConverged,
		},
		{
			name: "round budget exhausted → max_rounds", round: 2, totalRounds: 2,
			p0: 1, p1: 0, hasFindings: true,
			wantAction: autoIterateActionStop, wantState: autoIterateStateStopped, wantNote: autoIterateNoteMaxRounds,
		},
		{
			name: "the round failed → review_failed", round: 1, totalRounds: 2,
			reviewFailed: true, p0: 0, p1: 0, hasFindings: false,
			wantAction: autoIterateActionStop, wantState: autoIterateStateStopped, wantNote: autoIterateNoteReviewFailed,
		},
		{
			name: "no usable findings list → no_findings", round: 1, totalRounds: 2,
			p0: 0, p1: 0, hasFindings: false,
			wantAction: autoIterateActionStop, wantState: autoIterateStateStopped, wantNote: autoIterateNoteNoFindings,
		},
		{
			name: "P0 outstanding, budget left → continue", round: 1, totalRounds: 2,
			p0: 1, p1: 0, hasFindings: true,
			wantAction: autoIterateActionContinue,
		},
		{
			name: "P1 outstanding, budget left → continue", round: 2, totalRounds: 3,
			p0: 0, p1: 2, hasFindings: true,
			wantAction: autoIterateActionContinue,
		},
		{
			// A failed round must NOT read as "converged" just because its
			// counts stayed zero — that would silently end the chain on a
			// crash and tell the user "P0/P1 已清零".
			name: "a failed round outranks the zero counts", round: 1, totalRounds: 2,
			reviewFailed: true, p0: 0, p1: 0, hasFindings: true,
			wantAction: autoIterateActionStop, wantState: autoIterateStateStopped, wantNote: autoIterateNoteReviewFailed,
		},
		{
			// An unusable findings list must NOT read as "converged" either:
			// "the model broke the contract" is not "the code is clean".
			name: "an unusable findings list outranks the zero counts", round: 1, totalRounds: 2,
			p0: 0, p1: 0, hasFindings: false,
			wantAction: autoIterateActionStop, wantState: autoIterateStateStopped, wantNote: autoIterateNoteNoFindings,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideAutoIterate(tc.round, tc.totalRounds, tc.reviewFailed, tc.p0, tc.p1, tc.hasFindings)
			if got.Action != tc.wantAction {
				t.Fatalf("Action = %q, want %q", got.Action, tc.wantAction)
			}
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q", got.State, tc.wantState)
			}
			if got.Note != tc.wantNote {
				t.Errorf("Note = %q, want %q", got.Note, tc.wantNote)
			}
		})
	}
}

// TestNormalizeAutoIterateRounds pins the round budget's legal range.
//
// The budget is NOT silently clamped: a pick of 0 or 11 is rejected, because
// a silently corrected pick would make the user believe they got more rounds
// than they did.
func TestNormalizeAutoIterateRounds(t *testing.T) {
	for _, ok := range []int{1, 2, 5, 10} {
		got, legal := normalizeAutoIterateRounds(ok)
		if !legal {
			t.Errorf("normalizeAutoIterateRounds(%d) = illegal, want legal", ok)
		}
		if got != ok {
			t.Errorf("normalizeAutoIterateRounds(%d) = %d, want %d", ok, got, ok)
		}
	}
	for _, bad := range []int{-1, 0, 11, 100} {
		if _, legal := normalizeAutoIterateRounds(bad); legal {
			t.Errorf("normalizeAutoIterateRounds(%d) = legal, want rejected", bad)
		}
	}
}

// TestFixTaskRef pins the ref extraction the chain's idempotence rests on.
//
// The auto-iteration chain recognises an already-created fix task by the
// finding ref in its title. A title it cannot parse must NOT be treated as
// "already created" — mistaking it for one would silently drop a finding.
func TestFixTaskRef(t *testing.T) {
	cases := map[string]string{
		"[P0-1] 修复空指针":     "P0-1",
		"[P1-3] something": "P1-3",
		"没有前缀的标题":          "",
		"[未闭合":             "",
		"":                 "",
	}
	for title, want := range cases {
		if got := fixTaskRef(title); got != want {
			t.Errorf("fixTaskRef(%q) = %q, want %q", title, got, want)
		}
	}
}

// ── the fixture ────────────────────────────────────────────────────────────

// autoIterateFixture is a self-contained requirement-level Review setup: a
// throwaway SQLite DB, a throwaway git repo, and a handler wired to both.
//
// The runner is built WITHOUT a gateway on purpose: a review run then fails
// immediately (there is no claude to talk to), which is exactly what these
// tests want — a round that ends in 'error' and therefore stops the chain,
// deterministically and without spending a model's time.
type autoIterateFixture struct {
	t          *testing.T
	db         *db.DB
	h          *RequirementReviewHandler
	subTaskSvc *service.SubTaskService
	reviewSvc  *service.CodeReviewService
	reqSvc     *service.RequirementService
	reqID      string
	repo       string
	fixSeq     int
}

func newAutoIterateFixture(t *testing.T) *autoIterateFixture {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Skipf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	repo := initScopeRepo(t)

	const reqID = "req_auto_iterate"
	if _, err := d.Exec(`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?)`,
		"proj_x", "自动迭代项目", repo); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO requirements (id, title, project_id, branch_name, base_branch, worktree_path)
		VALUES (?, ?, ?, ?, ?, ?)`,
		reqID, "自动迭代需求", "proj_x", "main", "main", repo); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}

	subTaskSvc := service.NewSubTaskService(d, nil)
	reqSvc := service.NewRequirementService(d, nil)
	projectSvc := service.NewProjectService(d, nil, nil)
	reviewSvc := service.NewCodeReviewService(d, nil)
	roleSvc := service.NewRoleService(d)
	if err := roleSvc.SeedDefaults(); err != nil {
		t.Fatalf("seed roles: %v", err)
	}
	jobs := store.NewJobStore(50)
	jobLogSvc := service.NewJobLogService(d)
	runner := NewSubTaskRunner(nil, subTaskSvc, reqSvc, jobs, nil, nil, jobLogSvc, nil, nil, nil, nil, nil, nil, 0, nil, nil, nil)

	return &autoIterateFixture{
		t:          t,
		db:         d,
		h:          NewRequirementReviewHandler(reqSvc, projectSvc, subTaskSvc, reviewSvc, nil, roleSvc, nil, nil, runner, jobs),
		subTaskSvc: subTaskSvc,
		reviewSvc:  reviewSvc,
		reqSvc:     reqSvc,
		reqID:      reqID,
		repo:       repo,
	}
}

// seedRound inserts a finished Review round plus its review sub-task.
//
// findingsJSON is stored verbatim — the chain parses it exactly the way the
// report writer produced it.
func (f *autoIterateFixture) seedRound(reviewID, title, findingsJSON string, p0, p1 int, rounds int, state string) (*model.CodeReview, *model.SubTask) {
	f.t.Helper()
	if _, err := f.db.Exec(`INSERT INTO code_reviews
		(id, project_id, review_type, requirement_id, role_key, status, title, description,
		 model, findings_json, p0_count, p1_count, auto_iterate_rounds, auto_iterate_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		reviewID, "proj_x", model.ReviewTypeRequirement, f.reqID, "reviewer",
		model.ReviewStatusDone, title, "目标", "sonnet", findingsJSON, p0, p1, rounds, state); err != nil {
		f.t.Fatalf("seed code_review: %v", err)
	}
	st, err := f.subTaskSvc.CreateReviewTask(f.reqID, title, "目标", "sonnet", "", "",
		model.SubTaskSessionModeWithContext, "", reviewID, 1)
	if err != nil {
		f.t.Fatalf("seed review sub-task: %v", err)
	}
	// The round is FINISHED, so its sub-task row is terminal too — the
	// fixture never dispatches it, and a row left at 'pending' would look
	// like a chain that is still running.
	if _, err := f.db.Exec(`UPDATE sub_tasks SET status = ? WHERE id = ?`,
		model.SubTaskStatusDone, st.ID); err != nil {
		f.t.Fatalf("mark review sub-task done: %v", err)
	}
	rv, err := f.reviewSvc.Get(reviewID)
	if err != nil {
		f.t.Fatalf("get seeded review: %v", err)
	}
	return rv, st
}

// seedFixTask inserts a fix sub-task under parentID with a given status.
//
// It is used instead of ensureAutoFixTasks when the test needs a fix task in
// a SPECIFIC state (done / error) — dispatching a real run would race the
// assertion.
func (f *autoIterateFixture) seedFixTask(parentID, title, status string) *model.SubTask {
	f.t.Helper()
	f.fixSeq++
	id := fmt.Sprintf("st_fix_%d", f.fixSeq)
	if _, err := f.db.Exec(`INSERT INTO sub_tasks
		(id, requirement_id, parent_subtask_id, title, prompt, status, source, session_mode, report_mode, task_kind, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, f.reqID, parentID, title, "修一下", status, model.SubTaskSourceManual,
		model.SubTaskSessionModeFork, model.SubTaskReportModeAuto, model.SubTaskKindCode,
		time.Now(), time.Now()); err != nil {
		f.t.Fatalf("seed fix sub-task: %v", err)
	}
	st, err := f.subTaskSvc.Get(id)
	if err != nil {
		f.t.Fatalf("get seeded fix sub-task: %v", err)
	}
	return st
}

// waitSubTasksTerminal waits until every sub-task of the requirement reached
// a terminal state.
//
// The chain DISPATCHES its fix sub-tasks for real, and those runs keep
// writing to the database — the test must not close the DB (nor delete its
// temp directory) while one of them is still running.
func (f *autoIterateFixture) waitSubTasksTerminal() {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := f.subTaskSvc.List(f.reqID)
		if err != nil {
			return
		}
		busy := false
		for i := range rows {
			switch rows[i].Status {
			case model.SubTaskStatusPending, model.SubTaskStatusRunning:
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.t.Log("a sub-task is still running — the test proceeds anyway")
}

// ── idempotence ────────────────────────────────────────────────────────────

// TestEnsureAutoFixTasks_Idempotent pins the chain's most dangerous
// property: the fix batch must never be created twice.
//
// The chain can be driven by the round's terminal hook AND by the reconcile
// tick, and in a restart scenario both can fire for the same round. A
// duplicated batch would run every fix twice (twice the tokens, twice the
// conflicting edits).
func TestEnsureAutoFixTasks_Idempotent(t *testing.T) {
	f := newAutoIterateFixture(t)
	findings := `[
		{"severity":"P0","file":"a.go","line":"1","title":"致命缺陷","detail":"d","suggestion":"s"},
		{"severity":"P1","file":"b.go","line":"2","title":"次要缺陷","detail":"d","suggestion":"s"},
		{"severity":"P3","file":"c.go","line":"3","title":"优化建议","detail":"d","suggestion":"s"}
	]`
	rv, st := f.seedRound("crv_idem", "Review · 幂等", findings, 1, 1, 2, autoIterateStateCurrent)

	if _, err := f.h.ensureAutoFixTasks(rv, st); err != nil {
		t.Fatalf("first ensureAutoFixTasks: %v", err)
	}
	children, err := f.subTaskSvc.ListChildren(st.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2 (P0 + P1, never P3): %+v", len(children), children)
	}
	// The second call must create NOTHING.
	if _, err := f.h.ensureAutoFixTasks(rv, st); err != nil {
		t.Fatalf("second ensureAutoFixTasks: %v", err)
	}
	children2, err := f.subTaskSvc.ListChildren(st.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children2) != 2 {
		t.Fatalf("children after the second call = %d, want 2 (the batch must not double)", len(children2))
	}
	// And the refs must be the P0/P1 ones.
	got := map[string]bool{}
	for i := range children2 {
		got[fixTaskRef(children2[i].Title)] = true
	}
	if !got["P0-1"] || !got["P1-1"] {
		t.Errorf("fix refs = %v, want P0-1 and P1-1", got)
	}
	if got["P3-1"] {
		t.Errorf("fix refs = %v, want no P3 (auto-fixing nits is out of scope)", got)
	}
	f.waitSubTasksTerminal()
}

// TestAdvanceChainIfReady_CreatesFixesWhenMissing covers the restart gap.
//
// The backend can die between "the round finished" and "the fix tasks were
// created". The chain must heal that hole instead of standing still.
func TestAdvanceChainIfReady_CreatesFixesWhenMissing(t *testing.T) {
	f := newAutoIterateFixture(t)
	findings := `[{"severity":"P0","file":"a.go","line":"1","title":"缺陷","detail":"d","suggestion":"s"}]`
	rv, st := f.seedRound("crv_heal", "Review · 补建", findings, 1, 0, 2, autoIterateStateCurrent)

	children, err := f.subTaskSvc.ListChildren(st.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("fixture has %d children, want 0", len(children))
	}

	f.h.advanceChainIfReady(rv, st)

	children, err = f.subTaskSvc.ListChildren(st.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1 (the chain must heal the missing fix batch)", len(children))
	}
	f.waitSubTasksTerminal()
}

// TestAdvanceChainIfReady_StopsOnFixError pins the failure semantics.
//
// A fix sub-task that ERRORED or was STOPPED is not a success. The chain
// must stop: continuing would re-review code nobody fixed, burning tokens
// until the budget ran out, and would end with a misleading "converged".
func TestAdvanceChainIfReady_StopsOnFixError(t *testing.T) {
	f := newAutoIterateFixture(t)
	findings := `[{"severity":"P0","file":"a.go","line":"1","title":"缺陷","detail":"d","suggestion":"s"}]`
	rv, st := f.seedRound("crv_fixfail", "Review · 修复失败", findings, 1, 0, 3, autoIterateStateCurrent)
	f.seedFixTask(st.ID, "[P0-1] 缺陷", model.SubTaskStatusError)

	f.h.advanceChainIfReady(rv, st)

	fresh, err := f.reviewSvc.Get(rv.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if fresh.AutoIterateState != autoIterateStateStopped {
		t.Errorf("AutoIterateState = %q, want %q", fresh.AutoIterateState, autoIterateStateStopped)
	}
	if fresh.AutoIterateNote != autoIterateNoteFixFailed {
		t.Errorf("AutoIterateNote = %q, want %q", fresh.AutoIterateNote, autoIterateNoteFixFailed)
	}
	// No next round may have been minted.
	rounds, err := f.reviewSvc.ListByRequirement(f.reqID)
	if err != nil {
		t.Fatalf("ListByRequirement: %v", err)
	}
	if len(rounds) != 1 {
		t.Errorf("rounds = %d, want 1 (a failed fix must not start another round)", len(rounds))
	}
}

// TestAdvanceChainIfReady_OnlyOnce pins that a round advances at most once.
//
// The chain has two drivers (the fix task's terminal hook and the reconcile
// tick) and both can fire for the same round. If both minted a "round N+1",
// the chain would fork into two parallel branches.
func TestAdvanceChainIfReady_OnlyOnce(t *testing.T) {
	f := newAutoIterateFixture(t)
	findings := `[{"severity":"P0","file":"a.go","line":"1","title":"缺陷","detail":"d","suggestion":"s"}]`
	rv, st := f.seedRound("crv_once", "Review · 只推进一次", findings, 1, 0, 3, autoIterateStateCurrent)
	f.seedFixTask(st.ID, "[P0-1] 缺陷", model.SubTaskStatusDone)

	f.h.advanceChainIfReady(rv, st)
	f.h.advanceChainIfReady(rv, st)

	rounds, err := f.reviewSvc.ListByRequirement(f.reqID)
	if err != nil {
		t.Fatalf("ListByRequirement: %v", err)
	}
	if len(rounds) != 2 {
		t.Fatalf("rounds = %d, want 2 (round 1 + exactly one round 2)", len(rounds))
	}
	// The new round must be round 2 and hang off round 1's sub-task.
	var next *model.CodeReview
	for i := range rounds {
		if rounds[i].ID != rv.ID {
			next = &rounds[i]
		}
	}
	if next == nil {
		t.Fatal("the next round is missing")
	}
	nextSub, err := f.subTaskSvc.ByReviewID(next.ID)
	if err != nil || nextSub == nil {
		t.Fatalf("ByReviewID(%s): %v", next.ID, err)
	}
	if nextSub.ReviewRound != 2 {
		t.Errorf("next round number = %d, want 2", nextSub.ReviewRound)
	}
	if nextSub.ParentSubtaskID != st.ID {
		t.Errorf("next round parent = %q, want %q (the chain grows at its tip)", nextSub.ParentSubtaskID, st.ID)
	}
	// The round we came from has handed over.
	fresh, err := f.reviewSvc.Get(rv.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if fresh.AutoIterateState != autoIterateStateAdvanced {
		t.Errorf("AutoIterateState = %q, want %q", fresh.AutoIterateState, autoIterateStateAdvanced)
	}
	f.waitSubTasksTerminal()
}

// ── the API contract ───────────────────────────────────────────────────────

// startReview posts to POST /api/requirements/{id}/reviews and returns the
// recorded response.
func (f *autoIterateFixture) startReview(body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+f.reqID+"/reviews", strings.NewReader(body))
	req.SetPathValue("id", f.reqID)
	rec := httptest.NewRecorder()
	f.h.StartReview(rec, req)
	return rec
}

// TestStartReview_AutoIteratePersistsConfig pins that the user's pick
// reaches the database.
//
// The chain's whole configuration lives on the code_reviews row, so a pick
// that never lands there is a pick that never happens. The row's state is
// asserted AFTER the chain has settled: the review run fails (there is no
// claude in the test environment), so the chain stops with "review_failed"
// — which itself proves the row entered the chain in the first place (a row
// that never joined a chain keeps an empty state forever).
func TestStartReview_AutoIteratePersistsConfig(t *testing.T) {
	f := newAutoIterateFixture(t)
	rec := f.startReview(`{"description":"目标","auto_iterate":true,"auto_iterate_rounds":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			ReviewID string `json:"review_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}

	fresh := waitReviewState(t, f, resp.Data.ReviewID, func(rv *model.CodeReview) bool {
		return rv.AutoIterateState != autoIterateStateCurrent
	})
	if fresh.AutoIterateRounds != 2 {
		t.Errorf("AutoIterateRounds = %d, want 2", fresh.AutoIterateRounds)
	}
	if fresh.AutoIterateState != autoIterateStateStopped {
		t.Errorf("AutoIterateState = %q, want %q (the review run failed, so the chain stopped)", fresh.AutoIterateState, autoIterateStateStopped)
	}
	if fresh.AutoIterateNote != autoIterateNoteReviewFailed {
		t.Errorf("AutoIterateNote = %q, want %q", fresh.AutoIterateNote, autoIterateNoteReviewFailed)
	}
}

// TestStartReview_AutoIterateDefaults pins that the OFF switch changes
// NOTHING: a Review launched without the auto-iteration fields behaves
// exactly as it did before the feature existed — one round, no chain.
func TestStartReview_AutoIterateDefaults(t *testing.T) {
	f := newAutoIterateFixture(t)
	rec := f.startReview(`{"description":"目标"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			ReviewID string `json:"review_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	rv, err := f.reviewSvc.Get(resp.Data.ReviewID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if rv.AutoIterateRounds != 1 {
		t.Errorf("AutoIterateRounds = %d, want 1 (the default budget)", rv.AutoIterateRounds)
	}
	if rv.AutoIterateState != "" {
		t.Errorf("AutoIterateState = %q, want %q (a review that did not ask for auto-iteration is not part of a chain)", rv.AutoIterateState, "")
	}
	// And no fix task may have been created behind the user's back.
	children, err := f.subTaskSvc.List(f.reqID)
	if err == nil && len(children) > 1 {
		t.Errorf("sub-tasks = %d, want 1 (the ordinary review must not spawn anything)", len(children))
	}
}

// TestStartReview_RoundsOutOfRange pins that an illegal round budget is
// REJECTED rather than silently corrected.
func TestStartReview_RoundsOutOfRange(t *testing.T) {
	for _, rounds := range []string{"0", "-1", "11", "100"} {
		f := newAutoIterateFixture(t)
		rec := f.startReview(`{"description":"目标","auto_iterate":true,"auto_iterate_rounds":` + rounds + `}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("rounds=%s: status = %d, want 400 (body: %s)", rounds, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "AUTO_ITERATE_ROUNDS_INVALID") {
			t.Errorf("rounds=%s: body = %s, want the AUTO_ITERATE_ROUNDS_INVALID code", rounds, rec.Body.String())
		}
		// Nothing may have been created.
		rows, err := f.reviewSvc.ListByRequirement(f.reqID)
		if err != nil {
			t.Fatalf("ListByRequirement: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("rounds=%s: %d review rows created, want 0", rounds, len(rows))
		}
	}
}

// TestStartReview_RemoteRequirementRefused pins the remote guard.
//
// A requirement whose code only lives on an Agent Server cannot run an
// auto-iteration chain: the fixes would run on the server while the next
// round's Review only sees the local tree — the chain would re-review the
// same unfixed code until the budget ran out.
func TestStartReview_RemoteRequirementRefused(t *testing.T) {
	f := newAutoIterateFixture(t)
	if _, err := f.db.Exec(`UPDATE requirements SET worktree_path = '', agent_server_id = 'agent_x' WHERE id = ?`, f.reqID); err != nil {
		t.Fatalf("update requirement: %v", err)
	}
	rec := f.startReview(`{"description":"目标","auto_iterate":true,"auto_iterate_rounds":2}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "REMOTE_NOT_SUPPORTED") {
		t.Errorf("body = %s, want the REMOTE_NOT_SUPPORTED code", rec.Body.String())
	}
	rows, err := f.reviewSvc.ListByRequirement(f.reqID)
	if err != nil {
		t.Fatalf("ListByRequirement: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d review rows created, want 0", len(rows))
	}
}

// TestRecheck_InheritsChainConfig pins that a manual 回归 Review does not
// break a running chain: the new round keeps the chain's budget and joins
// the chain.
func TestRecheck_InheritsChainConfig(t *testing.T) {
	f := newAutoIterateFixture(t)
	findings := `[{"severity":"P0","file":"a.go","line":"1","title":"缺陷","detail":"d","suggestion":"s"}]`
	rv, st := f.seedRound("crv_recheck", "Review · 回归继承", findings, 1, 0, 2, autoIterateStateCurrent)
	_ = rv
	_ = st

	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+f.reqID+"/reviews/crv_recheck/recheck", strings.NewReader(`{}`))
	req.SetPathValue("id", f.reqID)
	req.SetPathValue("rid", "crv_recheck")
	rec := httptest.NewRecorder()
	f.h.Recheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			ReviewID string `json:"review_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	fresh := waitReviewState(t, f, resp.Data.ReviewID, func(r *model.CodeReview) bool {
		return r.AutoIterateState != autoIterateStateCurrent
	})
	if fresh.AutoIterateRounds != 2 {
		t.Errorf("AutoIterateRounds = %d, want 2 (the chain's budget is inherited)", fresh.AutoIterateRounds)
	}
	if fresh.AutoIterateState != autoIterateStateStopped {
		t.Errorf("AutoIterateState = %q, want %q (the new round joined the chain and then stopped with the failed run)", fresh.AutoIterateState, autoIterateStateStopped)
	}
}

// TestRecheck_ChainBusy409 pins the guard against a manual round running
// next to the automatic one.
//
// While the chain waits for its fix sub-tasks, a manual 回归 Review would
// start a second, parallel round — two chains fighting over the same tree.
func TestRecheck_ChainBusy409(t *testing.T) {
	f := newAutoIterateFixture(t)
	findings := `[{"severity":"P0","file":"a.go","line":"1","title":"缺陷","detail":"d","suggestion":"s"}]`
	_, st := f.seedRound("crv_busy", "Review · 链忙", findings, 1, 0, 2, autoIterateStateCurrent)
	f.seedFixTask(st.ID, "[P0-1] 缺陷", model.SubTaskStatusRunning)

	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+f.reqID+"/reviews/crv_busy/recheck", strings.NewReader(`{}`))
	req.SetPathValue("id", f.reqID)
	req.SetPathValue("rid", "crv_busy")
	rec := httptest.NewRecorder()
	f.h.Recheck(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "REVIEW_CHAIN_BUSY") {
		t.Errorf("body = %s, want the REVIEW_CHAIN_BUSY code", rec.Body.String())
	}
	// No second round may have been created.
	rows, err := f.reviewSvc.ListByRequirement(f.reqID)
	if err != nil {
		t.Fatalf("ListByRequirement: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("%d review rows, want 1 (the busy chain must not fork)", len(rows))
	}
}

// waitReviewState polls a review row until pred is satisfied.
//
// The chain's last step happens in a goroutine (the round's terminal hook
// runs the decision off the request path), so the row's final state is only
// observable by waiting for it.
func waitReviewState(t *testing.T, f *autoIterateFixture, reviewID string, pred func(*model.CodeReview) bool) *model.CodeReview {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last *model.CodeReview
	for time.Now().Before(deadline) {
		rv, err := f.reviewSvc.Get(reviewID)
		if err != nil {
			t.Fatalf("get review %s: %v", reviewID, err)
		}
		last = rv
		if pred(rv) {
			return rv
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the review row never reached the expected state (last: state=%q note=%q rounds=%d)",
		last.AutoIterateState, last.AutoIterateNote, last.AutoIterateRounds)
	return nil
}
