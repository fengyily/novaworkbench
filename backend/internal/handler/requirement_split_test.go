package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// newSplitTestHandler wires a WizardHandler against a throwaway SQLite DB
// with one project and a seeded requirement.
func newSplitTestHandler(t *testing.T, reqID, kind, status, designDocs, parentID, branchName string) (*WizardHandler, *service.RequirementService, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	const projID = "proj_split"
	if _, err := d.Exec(`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "P", "/tmp/p", "ready", "main"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO requirements (id, project_id, title, kind, status, design_docs, parent_id, branch_name, dev_delegated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		reqID, projID, "需求 "+reqID, kind, status, designDocs, parentID, branchName); err != nil {
		t.Fatalf("insert requirement: %v", err)
	}

	reqSvc := service.NewRequirementService(d, nil)
	h := &WizardHandler{
		reqSvc: reqSvc,
		jobs:   store.NewJobStore(10),
	}
	return h, reqSvc, d
}

// splitErrorCode runs Split and returns the HTTP status + error code.
func splitErrorCode(t *testing.T, h *WizardHandler, reqID string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+reqID+"/split", strings.NewReader("{}"))
	req.SetPathValue("id", reqID)
	rec := httptest.NewRecorder()
	h.Split(rec, req)
	if rec.Code == http.StatusOK {
		return rec.Code, ""
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, env.Error.Code
}

// Every precondition has its own error code — the user must be told WHY the
// split was refused, not shown a generic failure.
func TestSplit_PreconditionErrorCodes(t *testing.T) {
	cases := []struct {
		name       string
		reqID      string
		kind       string
		status     string
		designDocs string
		parentID   string
		branchName string
		wantCode   string
	}{
		{
			name: "wiki kind", reqID: "req_wiki", kind: "wiki", status: "designed",
			designDocs: "# 方案", wantCode: "SPLIT_KIND_UNSUPPORTED",
		},
		{
			name: "no design", reqID: "req_nodesign", kind: "requirement", status: "draft",
			designDocs: "", wantCode: "SPLIT_NO_DESIGN",
		},
		{
			name: "already a child", reqID: "req_child", kind: "requirement", status: "designed",
			designDocs: "# 方案", parentID: "req_other", wantCode: "SPLIT_PARENT_IS_CHILD",
		},
		{
			name: "archived", reqID: "req_arch", kind: "requirement", status: "archived",
			designDocs: "# 方案", wantCode: "SPLIT_PARENT_ARCHIVED",
		},
		{
			name: "currently developing", reqID: "req_dev", kind: "requirement", status: "developing",
			designDocs: "# 方案", branchName: "feat/req_dev", wantCode: "SPLIT_ALREADY_DEVELOPED",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := newSplitTestHandler(t, c.reqID, c.kind, c.status, c.designDocs, c.parentID, c.branchName)
			status, code := splitErrorCode(t, h, c.reqID)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", status)
			}
			if code != c.wantCode {
				t.Errorf("code = %q, want %q", code, c.wantCode)
			}
		})
	}
}

// A requirement that has merely been DESIGNED must stay splittable.
//
// The design stage creates the worktree and writes branch_name, so the old
// gate (branch_name != "" ⇒ "already developed") made branch-mode splitting
// a deadlock: every designed requirement looked like it was already in
// development. 321 of the 476 requirements that had a design in the wild
// were blocked by it.
//
// The "may split" case is tested on the pure function rather than through
// the HTTP layer: a request that passes the preconditions would spawn a
// claude subprocess (the test handler has no gateway) and crash the test
// binary.
func TestSplitPrecondition_DesignedWithBranchNameIsSplittable(t *testing.T) {
	cases := []struct {
		name string
		req  *model.Requirement
	}{
		{
			name: "designed with a branch name",
			req:  &model.Requirement{ID: "req_m", Kind: "requirement", Status: "designed", DesignDocs: "# 方案", BranchName: "feat/req_m"},
		},
		{
			name: "draft without a branch name",
			req:  &model.Requirement{ID: "req_m", Kind: "requirement", Status: "draft", DesignDocs: "# 方案"},
		},
		{
			name: "done requirement can be topped up",
			req:  &model.Requirement{ID: "req_m", Kind: "requirement", Status: "done", DesignDocs: "# 方案", BranchName: "feat/req_m"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, msg := splitPreconditionError(c.req); code != "" {
				t.Errorf("code = %q (%s), want the split to be allowed", code, msg)
			}
		})
	}
}

// A master that already handed its development down to a batch of children
// MAY be split again — the plan can be regenerated, which makes the previous
// breakdown stale, and the old batch is replaced (not appended to).
//
// dev_delegated used to be a one-way flag that nothing ever reset, so it
// could only be read as "this master was split once" and used as a veto.
// That made the master permanently unsplittable. Whether a re-split is
// allowed is now decided by the CHILDREN's state instead.
func TestSplitPrecondition_AlreadySplitIsAllowed(t *testing.T) {
	req := &model.Requirement{
		ID: "req_m", Kind: "requirement", Status: "designed",
		DesignDocs: "# 方案", BranchName: "feat/req_m", DevDelegated: true,
	}
	code, msg := splitPreconditionError(req)
	if code != "" {
		t.Errorf("code = %q (%s), want the split to be allowed", code, msg)
	}
}

// Only an old child that is STILL BEING DEVELOPED forbids a re-split —
// that row has a live development job behind it.
//
// A hand-attached child is not part of any batch and never blocks; a "done"
// old child does not block either (only the row is lost, the code stays in
// git).
func TestSplitChildrenBlocker(t *testing.T) {
	cases := []struct {
		name     string
		children []*model.Requirement
		wantCode string
	}{
		{
			name:     "no children",
			children: nil,
		},
		{
			name:     "a done old child may be replaced",
			children: []*model.Requirement{{ID: "req_c1", Status: "done", SplitBatchID: "spb_1"}},
		},
		{
			name:     "a developing old child blocks",
			children: []*model.Requirement{{ID: "req_c1", Status: "developing", SplitBatchID: "spb_1"}},
			wantCode: "SPLIT_CHILDREN_IN_PROGRESS",
		},
		{
			name: "one developing child among several blocks",
			children: []*model.Requirement{
				{ID: "req_c1", Status: "done", SplitBatchID: "spb_1"},
				{ID: "req_c2", Status: "developing", SplitBatchID: "spb_1"},
			},
			wantCode: "SPLIT_CHILDREN_IN_PROGRESS",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _ := splitChildrenBlocker(c.children)
			if code != c.wantCode {
				t.Errorf("code = %q, want %q", code, c.wantCode)
			}
		})
	}
}

// Only the children of a split BATCH are candidates for replacement — a
// hand-attached child (no batch id) belongs to no batch and survives.
func TestSplitBatchChildren(t *testing.T) {
	children := []*model.Requirement{
		{ID: "req_b1", SplitBatchID: "spb_1"},
		{ID: "req_manual", SplitBatchID: ""},
		{ID: "req_b2", SplitBatchID: "spb_1"},
		nil,
	}
	got := splitBatchChildren(children)
	if len(got) != 2 {
		t.Fatalf("got %d batch children, want 2", len(got))
	}
	for _, c := range got {
		if c.ID == "req_manual" {
			t.Error("a hand-attached child must not be treated as a batch child")
		}
	}
}

// A second split while the first is still running must be refused —
// otherwise two batches of duplicate children land.
func TestSplit_IdempotencyLock(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	// Simulate an in-flight split by taking the lock directly.
	st := h.splitState()
	if existing, ok := st.locks.acquire("req_m", "job_running"); !ok {
		t.Fatal("could not acquire the lock on a free requirement")
	} else if existing != "" {
		t.Fatalf("acquire returned %q, want empty", existing)
	}

	status, code := splitErrorCode(t, h, "req_m")
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
	if code != "SPLIT_IN_PROGRESS" {
		t.Errorf("code = %q, want SPLIT_IN_PROGRESS", code)
	}
}

// ConfirmSplit persists the candidates: siblings under one master, the base
// inherited from the master, the dependency edge translated from the
// batch-local ordinal, and the master flagged.
func TestConfirmSplit_PersistsChildrenAndEdges(t *testing.T) {
	h, reqSvc, d := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")
	if _, err := d.Exec("UPDATE requirements SET base_branch='release/x', base_branch_source='explicit' WHERE id='req_m'"); err != nil {
		t.Fatalf("set base branch: %v", err)
	}

	body := `{"tasks":[
		{"title":"子需求 A","description":"做 A","scope":["a.go"],"acceptance":["A 通过"],"boundary":"边界 A","order":1,"depends_on":[]},
		{"title":"子需求 B","description":"做 B","scope":["b.go"],"acceptance":["B 通过"],"boundary":"边界 B","order":2,"depends_on":[1]}
	],"references":[[1,2]]}`

	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data struct {
			Children []struct {
				ID           string   `json:"id"`
				Title        string   `json:"title"`
				BaseBranch   string   `json:"base_branch"`
				BaseSource   string   `json:"base_branch_source"`
				SplitBatchID string   `json:"split_batch_id"`
				DependsOn    []string `json:"depends_on"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp2 := resp.Data
	if len(resp2.Children) != 2 {
		t.Fatalf("got %d children, want 2", len(resp2.Children))
	}
	if resp2.Children[0].BaseBranch != "release/x" {
		t.Errorf("base_branch = %q, want release/x", resp2.Children[0].BaseBranch)
	}
	if resp2.Children[0].BaseSource != model.BaseSourceParent {
		t.Errorf("base_branch_source = %q, want %q", resp2.Children[0].BaseSource, model.BaseSourceParent)
	}
	if resp2.Children[0].SplitBatchID == "" || resp2.Children[0].SplitBatchID != resp2.Children[1].SplitBatchID {
		t.Errorf("split_batch_id = %q / %q, want the same non-empty value",
			resp2.Children[0].SplitBatchID, resp2.Children[1].SplitBatchID)
	}
	// The edge: child 2 depends on child 1.
	if len(resp2.Children[1].DependsOn) != 1 || resp2.Children[1].DependsOn[0] != resp2.Children[0].ID {
		t.Errorf("child 2 depends_on = %v, want [%s]", resp2.Children[1].DependsOn, resp2.Children[0].ID)
	}
	if len(resp2.Children[0].DependsOn) != 0 {
		t.Errorf("child 1 depends_on = %v, want none", resp2.Children[0].DependsOn)
	}

	// The master handed development down.
	master, err := reqSvc.Get("req_m")
	if err != nil {
		t.Fatalf("get master: %v", err)
	}
	if !master.DevDelegated {
		t.Error("master dev_delegated = false, want true")
	}

	// The "互相参考" link landed.
	pre, post, err := reqSvc.ListDependencies(resp2.Children[0].ID)
	if err != nil {
		t.Fatalf("ListDependencies: %v", err)
	}
	if len(pre) != 0 {
		t.Errorf("child 1 prerequisites = %v, want none", pre)
	}
	if len(post) != 1 || post[0].RequirementID != resp2.Children[1].ID {
		t.Errorf("child 1 dependents = %v, want child 2", post)
	}
}

// The batch order is the ONE thing that tells the user which child came
// first, and the dependency edges have to land on the same rows the user
// numbered.
//
// The candidates come in with non-contiguous `order` values (3, 1, 2 — the
// model is free to do that) and the edges are expressed in that same ordinal
// space. The children must land as 1, 2, 3 with the edges attached to the
// rows they named: 第三 depends on 第二, 第二 depends on 第一.
func TestConfirmSplit_PersistsBatchOrderAndKeepsEdgesAligned(t *testing.T) {
	h, reqSvc, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	body := `{"tasks":[
		{"title":"第三","order":3,"depends_on":[2]},
		{"title":"第一","order":1,"depends_on":[]},
		{"title":"第二","order":2,"depends_on":[1]}
	]}`

	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data struct {
			Children []struct {
				ID        string   `json:"id"`
				Title     string   `json:"title"`
				DependsOn []string `json:"depends_on"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	children := resp.Data.Children
	if len(children) != 3 {
		t.Fatalf("got %d children, want 3", len(children))
	}
	// The response is in the batch order (the order the candidates were
	// confirmed in), so the frontend can render the list as-is.
	wantTitles := []string{"第一", "第二", "第三"}
	for i, c := range children {
		if c.Title != wantTitles[i] {
			t.Errorf("child %d: title = %q, want %q", i, c.Title, wantTitles[i])
		}
	}
	// The edges follow the rows: 第二 depends on 第一, 第三 depends on 第二.
	if len(children[0].DependsOn) != 0 {
		t.Errorf("第一 depends_on = %v, want none", children[0].DependsOn)
	}
	if len(children[1].DependsOn) != 1 || children[1].DependsOn[0] != children[0].ID {
		t.Errorf("第二 depends_on = %v, want [%s]", children[1].DependsOn, children[0].ID)
	}
	if len(children[2].DependsOn) != 1 || children[2].DependsOn[0] != children[1].ID {
		t.Errorf("第三 depends_on = %v, want [%s]", children[2].DependsOn, children[1].ID)
	}

	// The order is persisted, and the read path hands the children back in it.
	listed, err := reqSvc.ListChildren("req_m")
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("ListChildren returned %d children, want 3", len(listed))
	}
	for i, c := range listed {
		if c.Title != wantTitles[i] {
			t.Errorf("listed child %d: title = %q, want %q (order not preserved)", i, c.Title, wantTitles[i])
		}
		if c.SplitOrder != i+1 {
			t.Errorf("listed child %d: split_order = %d, want %d", i, c.SplitOrder, i+1)
		}
	}
}

// A master that already has its own development branch hands that branch
// down as the children's base — the stacked-PR stack bottom.
func TestConfirmSplit_ChildrenBaseOnTheMasterBranch(t *testing.T) {
	h, reqSvc, d := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "feat/req_m")
	if _, err := d.Exec("UPDATE requirements SET base_branch='release/x', base_branch_source='explicit' WHERE id='req_m'"); err != nil {
		t.Fatalf("set base branch: %v", err)
	}

	body := `{"tasks":[{"title":"子需求 A","order":1,"depends_on":[]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Children []struct {
				BaseBranch string `json:"base_branch"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Data.Children) != 1 {
		t.Fatalf("got %d children, want 1", len(resp.Data.Children))
	}
	if resp.Data.Children[0].BaseBranch != "feat/req_m" {
		t.Errorf("base_branch = %q, want the master's branch feat/req_m", resp.Data.Children[0].BaseBranch)
	}

	// The master handed development down.
	master, err := reqSvc.Get("req_m")
	if err != nil {
		t.Fatalf("get master: %v", err)
	}
	if !master.DevDelegated {
		t.Error("master dev_delegated = false, want true")
	}
}

// An empty candidate list is a client error, not a server error.
func TestConfirmSplit_RejectsEmptyTaskList(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(`{"tasks":[]}`))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ListChildren returns the children plus the edges among them.
func TestListChildren_ReturnsChildrenAndDependencies(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")
	body := `{"tasks":[
		{"title":"子需求 A","order":1,"depends_on":[]},
		{"title":"子需求 B","order":2,"depends_on":[1]}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm failed: %d %s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/requirements/req_m/children", nil)
	req2.SetPathValue("id", "req_m")
	rec2 := httptest.NewRecorder()
	h.ListChildren(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d", rec2.Code)
	}
	var resp struct {
		Data struct {
			Children     []model.Requirement           `json:"children"`
			Dependencies []model.RequirementDependency `json:"dependencies"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Children) != 2 {
		t.Fatalf("got %d children, want 2", len(resp.Data.Children))
	}
	if len(resp.Data.Dependencies) != 1 {
		t.Fatalf("got %d dependencies, want 1", len(resp.Data.Dependencies))
	}
}

// ── 执行环境（本地 / Agent Server）分流 ────────────────────────────────────
//
// The split used to run the claude CLI on this machine and nothing else —
// the UI had no way to say "run it over there". Now the request body carries
// an agent_server_id and the handler routes accordingly.
//
// These tests inject a fake executor through h.splitExec. That is the ONLY
// way to assert the routing without spawning a real claude subprocess or
// opening an SSH connection: WizardHandler.llm is a concrete *llm.Gateway and
// the SSH client is a concrete *gossh.Client — neither has an interface to
// substitute.

// splitStartJob runs the Split handler with a custom request body and returns
// the HTTP status plus the id of the job the handler created.
//
// splitErrorCode covers the refusal path (it always posts "{}" and reads the
// error envelope); this one covers the happy path, where the response carries
// a job id instead of an error code.
func splitStartJob(t *testing.T, h *WizardHandler, reqID, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/"+reqID+"/split", strings.NewReader(body))
	req.SetPathValue("id", reqID)
	rec := httptest.NewRecorder()
	h.Split(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, ""
	}
	var env struct {
		Data struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, env.Data.JobID
}

// waitSplitJob polls the job store until the split job reaches a terminal
// state. The split runs in a goroutine, so the HTTP response comes back long
// before the AI run finishes.
//
// The status and the log come from Job.Snapshot() — the job's exported
// fields are written under the job's own lock, so reading them directly from
// the test would be a data race.
func waitSplitJob(t *testing.T, h *WizardHandler, jobID string) (store.JobStatus, []store.LogLine) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, ok := h.jobs.Get(jobID)
		if !ok {
			t.Fatalf("job %s is not in the store", jobID)
		}
		log, status, _ := job.Snapshot()
		if status != store.JobRunning {
			return status, log
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not finish within 3s (status = %q)", jobID, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The agent server the user picked in the UI must actually reach the
// execution layer — together with the model / claude config the user picked.
func TestSplit_UsesSelectedAgentServer(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	type captured struct {
		target   splitExecTarget
		model    string
		configID string
	}
	got := make(chan captured, 1)
	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		got <- captured{target: target, model: modelName, configID: configID}
		return []prompt.SplitTask{{Title: "子需求 A", Order: 1}}, nil, ""
	}

	status, jobID := splitStartJob(t, h, "req_m",
		`{"agent_server_id":"agent_x","model":"m2","claude_config_id":"c2"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if jobID == "" {
		t.Fatal("the response carries no job_id")
	}

	var c captured
	select {
	case c = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the split executor was never called")
	}
	if !c.target.Remote {
		t.Errorf("target.Remote = false, want true")
	}
	if c.target.AgentServerID != "agent_x" {
		t.Errorf("target.AgentServerID = %q, want agent_x", c.target.AgentServerID)
	}
	if c.model != "m2" {
		t.Errorf("model = %q, want m2", c.model)
	}
	if c.configID != "c2" {
		t.Errorf("configID = %q, want c2", c.configID)
	}

	jobStatus, jobLog := waitSplitJob(t, h, jobID)
	if jobStatus != store.JobDone {
		t.Errorf("job status = %q, want done; log = %v", jobStatus, jobLog)
	}
}

// Without an agent server the split stays on this machine — and the model the
// user picked still wins over the architect persona's default.
func TestSplit_WithoutAgentServerRunsLocally(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	type captured struct {
		target splitExecTarget
		model  string
	}
	got := make(chan captured, 1)
	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		got <- captured{target: target, model: modelName}
		return []prompt.SplitTask{{Title: "子需求 A", Order: 1}}, nil, ""
	}

	status, jobID := splitStartJob(t, h, "req_m", `{"model":"m2"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if jobID == "" {
		t.Fatal("the response carries no job_id")
	}

	var c captured
	select {
	case c = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the split executor was never called")
	}
	if c.target.Remote {
		t.Errorf("target.Remote = true, want false")
	}
	if c.target.AgentServerID != "" {
		t.Errorf("target.AgentServerID = %q, want empty", c.target.AgentServerID)
	}
	if c.model != "m2" {
		t.Errorf("model = %q, want m2", c.model)
	}

	jobStatus, jobLog := waitSplitJob(t, h, jobID)
	if jobStatus != store.JobDone {
		t.Errorf("job status = %q, want done; log = %v", jobStatus, jobLog)
	}
}

// A failed remote run must end the job as an error and leave no candidates
// behind — the UI reads the candidate list off the job result.
func TestSplit_RemoteFailureEndsJobWithError(t *testing.T) {
	h, _, _ := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")

	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		return nil, nil, "远端失败"
	}

	status, jobID := splitStartJob(t, h, "req_m", `{"agent_server_id":"agent_x"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if jobID == "" {
		t.Fatal("the response carries no job_id")
	}

	jobStatus, jobLog := waitSplitJob(t, h, jobID)
	if jobStatus != store.JobError {
		t.Errorf("job status = %q, want error; log = %v", jobStatus, jobLog)
	}
	// No candidates were published — the modal must not offer stale rows.
	if run := h.splitState().runs.get(jobID); run != nil && len(run.Candidates) != 0 {
		t.Errorf("candidates = %v, want none", run.Candidates)
	}
	// The failure reason is recorded on the registry so the state endpoint
	// can serve it after the modal is reopened.
	if run := h.splitState().runs.get(jobID); run == nil || run.Error != "远端失败" {
		t.Errorf("run error = %v, want 远端失败", run)
	}
}

// The remote path needs the agent-server service; without it the split must
// fail with a message that names the missing piece instead of panicking.
func TestRunRemoteSplitTasks_WithoutAgentServerService(t *testing.T) {
	h := &WizardHandler{}
	job := store.NewJobStore(1).Create("req_m")
	reqRow := &model.Requirement{ID: "req_m", ProjectID: "proj_split"}

	tasks, refs, errMsg := h.runRemoteSplitTasks(job, reqRow, "agent_x", "prompt", "", "m", "c", splitRuntime{}, nil)
	if errMsg == "" {
		t.Fatal("expected an error message, got none")
	}
	if !strings.Contains(errMsg, "Agent 服务器") {
		t.Errorf("error = %q, want it to mention Agent 服务器", errMsg)
	}
	if len(tasks) != 0 {
		t.Errorf("tasks = %v, want none", tasks)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none", refs)
	}
}

// ── 重新拆分（re-split）：已拆分的需求可以被再次拆分 ──────────────────────

// seedSplitBatch inserts a previous batch of split children under the master.
//
// batchID empty ⇒ a hand-attached child (no batch id): it belongs to no
// batch and must survive a re-split.
func seedSplitBatch(t *testing.T, d *db.DB, masterID, childID, title, status, batchID string, order int) {
	t.Helper()
	_, err := d.Exec(
		`INSERT INTO requirements (id, project_id, title, status, parent_id, split_batch_id, split_order, dev_delegated)
		 VALUES (?, 'proj_split', ?, ?, ?, ?, ?, 0)`,
		childID, title, status, masterID, batchID, order)
	if err != nil {
		t.Fatalf("insert child %s: %v", childID, err)
	}
}

// countChildren returns how many children the master has.
func countChildren(t *testing.T, d *db.DB, masterID string) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(1) FROM requirements WHERE parent_id=?", masterID).Scan(&n); err != nil {
		t.Fatalf("count children: %v", err)
	}
	return n
}

// A split whose previous batch still has a child IN DEVELOPMENT must be
// refused before the AI is even asked — that child has a live development
// job behind it.
func TestSplit_RefusesWhenAnOldChildIsStillDeveloping(t *testing.T) {
	h, _, d := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")
	seedSplitBatch(t, d, "req_m", "req_c1", "旧子需求", "developing", "spb_old", 1)

	status, code := splitErrorCode(t, h, "req_m")
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
	if code != "SPLIT_CHILDREN_IN_PROGRESS" {
		t.Errorf("code = %q, want SPLIT_CHILDREN_IN_PROGRESS", code)
	}
}

// A "done" old child does NOT block — the re-split only loses the row, the
// code it produced is still in git.
func TestSplit_AllowsWhenTheOldBatchIsDone(t *testing.T) {
	h, _, d := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")
	seedSplitBatch(t, d, "req_m", "req_c1", "旧子需求", "done", "spb_old", 1)

	// The split starts a job (the test handler has no gateway, so the job
	// dies in the goroutine — what matters here is that it was NOT refused).
	status, code := splitErrorCode(t, h, "req_m")
	if status == http.StatusBadRequest || code == "SPLIT_CHILDREN_IN_PROGRESS" {
		t.Errorf("status = %d, code = %q — a done old child must not block", status, code)
	}
}

// Confirming a re-split WITHOUT the explicit authorization is refused — and
// the old children are still there afterwards.
//
// Deleting rows is a hard action: the user has to say so.
func TestConfirmSplit_WithoutReplaceExistingKeepsTheOldBatch(t *testing.T) {
	h, _, d := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")
	seedSplitBatch(t, d, "req_m", "req_c1", "旧子需求", "done", "spb_old", 1)

	body := `{"tasks":[{"title":"新子需求","order":1,"depends_on":[]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body = %s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if env.Error.Code != "SPLIT_ALREADY_SPLIT" {
		t.Errorf("code = %q, want SPLIT_ALREADY_SPLIT", env.Error.Code)
	}
	// The old child must NOT have been deleted.
	if n := countChildren(t, d, "req_m"); n != 1 {
		t.Errorf("got %d children, want 1 — the old batch must survive a refused confirm", n)
	}
}

// The authorized re-split replaces the old batch: the old rows are gone, the
// new ones land with a fresh batch id, and the master moved on to the next
// split round.
func TestConfirmSplit_WithReplaceExistingReplacesTheOldBatch(t *testing.T) {
	h, reqSvc, d := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")
	seedSplitBatch(t, d, "req_m", "req_old1", "旧子需求 1", "done", "spb_old", 1)
	seedSplitBatch(t, d, "req_m", "req_old2", "旧子需求 2", "done", "spb_old", 2)
	// A hand-attached child (no batch id) is NOT part of any batch.
	seedSplitBatch(t, d, "req_m", "req_manual", "人工子需求", "draft", "", 0)

	body := `{"tasks":[
		{"title":"新子需求 A","order":1,"depends_on":[]},
		{"title":"新子需求 B","order":2,"depends_on":[1]}
	],"replace_existing":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data struct {
			Children []struct {
				ID           string `json:"id"`
				SplitBatchID string `json:"split_batch_id"`
			} `json:"children"`
			ReplacedCount   int `json:"replaced_count"`
			SplitGeneration int `json:"split_generation"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Data.Children) != 2 {
		t.Fatalf("got %d children, want 2", len(resp.Data.Children))
	}
	if resp.Data.ReplacedCount != 2 {
		t.Errorf("replaced_count = %d, want 2", resp.Data.ReplacedCount)
	}
	if resp.Data.SplitGeneration != 1 {
		t.Errorf("split_generation = %d, want 1", resp.Data.SplitGeneration)
	}
	// The new children carry a FRESH batch id — not the replaced one.
	newBatch := resp.Data.Children[0].SplitBatchID
	if newBatch == "" || newBatch == "spb_old" {
		t.Errorf("split_batch_id = %q, want a fresh non-empty batch id", newBatch)
	}
	if resp.Data.Children[1].SplitBatchID != newBatch {
		t.Errorf("children carry different batch ids: %q / %q", newBatch, resp.Data.Children[1].SplitBatchID)
	}

	// The old batch is gone; the hand-attached child survived.
	var oldLeft int
	if err := d.QueryRow("SELECT COUNT(1) FROM requirements WHERE id IN ('req_old1','req_old2')").Scan(&oldLeft); err != nil {
		t.Fatalf("count old children: %v", err)
	}
	if oldLeft != 0 {
		t.Errorf("got %d old children, want 0 — the old batch must be replaced", oldLeft)
	}
	var manualLeft int
	if err := d.QueryRow("SELECT COUNT(1) FROM requirements WHERE id='req_manual'").Scan(&manualLeft); err != nil {
		t.Fatalf("count manual child: %v", err)
	}
	if manualLeft != 1 {
		t.Error("the hand-attached child was deleted — it belongs to no batch and must survive")
	}

	// The master still handed development down, and the counter moved on.
	master, err := reqSvc.Get("req_m")
	if err != nil {
		t.Fatalf("get master: %v", err)
	}
	if !master.DevDelegated {
		t.Error("master dev_delegated = false, want true")
	}
	if master.SplitGeneration != 1 {
		t.Errorf("master split_generation = %d, want 1", master.SplitGeneration)
	}
}

// genBumpProjectSvc is a projectService that bumps the master's split
// generation on every Get.
//
// It exists to open the CAS race window deterministically: ConfirmSplit
// reads the master (and with it the generation it expects to claim), then
// calls projectSvc.Get to resolve the project BEFORE it claims the split
// round. Bumping the counter in between is exactly what a concurrent
// confirmation in another tab does — the claim then finds a moved target
// and must refuse instead of producing a second batch.
type genBumpProjectSvc struct {
	spyProjectSvc
	d     *db.DB
	reqID string
}

func (s *genBumpProjectSvc) Get(id string) (*model.Project, error) {
	if _, err := s.d.Exec("UPDATE requirements SET split_generation=split_generation+1 WHERE id=?", s.reqID); err != nil {
		return nil, err
	}
	return s.spyProjectSvc.Get(id)
}

// A concurrent confirmation must be refused with a conflict — not silently
// produce a second batch of children.
//
// The CAS is the only arbiter of the confirm path: it compares against the
// generation the caller read, and 0 rows affected means somebody else moved
// it in between.
func TestConfirmSplit_ConcurrentConfirmationIsRefused(t *testing.T) {
	h, _, d := newSplitTestHandler(t, "req_m", "requirement", "designed", "# 方案", "", "")
	// The project lookup sits between the master's read and the CAS claim.
	h.projectSvc = &genBumpProjectSvc{d: d, reqID: "req_m"}

	body := `{"tasks":[{"title":"子需求","order":1,"depends_on":[]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body = %s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if env.Error.Code != "SPLIT_CONFLICT" {
		t.Errorf("code = %q, want SPLIT_CONFLICT", env.Error.Code)
	}
	// Nothing may have been written.
	if n := countChildren(t, d, "req_m"); n != 0 {
		t.Errorf("got %d children, want 0 — a refused confirm must write nothing", n)
	}
}
