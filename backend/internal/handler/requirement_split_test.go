package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
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
// must not be split again — a second batch would produce a second,
// conflicting set of children.
func TestSplitPrecondition_AlreadySplitIsRefused(t *testing.T) {
	req := &model.Requirement{
		ID: "req_m", Kind: "requirement", Status: "designed",
		DesignDocs: "# 方案", BranchName: "feat/req_m", DevDelegated: true,
	}
	code, _ := splitPreconditionError(req)
	if code != "SPLIT_ALREADY_SPLIT" {
		t.Errorf("code = %q, want SPLIT_ALREADY_SPLIT", code)
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
