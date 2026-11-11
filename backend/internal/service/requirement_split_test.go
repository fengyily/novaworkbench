package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// ── helpers ───────────────────────────────────────────────────────────────

func newSplitSvc(t *testing.T) *RequirementService {
	t.Helper()
	d := newServiceTestDB(t)
	return NewRequirementService(d, nil)
}

func seedSplitProject(t *testing.T, svc *RequirementService, id, defaultBranch string) {
	t.Helper()
	if _, err := svc.db.Exec(
		`INSERT OR IGNORE INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		id, id, "/tmp/"+id, "ready", defaultBranch,
	); err != nil {
		t.Fatalf("insert project %s: %v", id, err)
	}
}

// seedSplitReq inserts a master requirement with an explicit base branch.
func seedSplitReq(t *testing.T, svc *RequirementService, id, projectID, title, baseBranch, baseSource, status string) {
	t.Helper()
	if _, err := svc.db.Exec(
		`INSERT INTO requirements (id, project_id, title, description, status, base_branch, base_branch_source, dev_delegated, split_batch_id)
		 VALUES (?, ?, ?, '', ?, ?, ?, 0, '')`,
		id, projectID, title, status, baseBranch, baseSource,
	); err != nil {
		t.Fatalf("insert requirement %s: %v", id, err)
	}
}

func splitTasks() []model.SplitTaskInput {
	return []model.SplitTaskInput{
		{
			Title:       "子需求 A",
			Description: "实现后端接口",
			Scope:       []string{"backend/internal/handler/a.go"},
			Acceptance:  []string{"接口返回 200"},
			Boundary:    "A 提供 /api/a",
			Order:       1,
			DependsOn:   []int{},
		},
		{
			Title:       "子需求 B",
			Description: "实现前端页面",
			Scope:       []string{"frontend/src/pages/A.tsx"},
			Acceptance:  []string{"页面可打开"},
			Boundary:    "B 调用 /api/a",
			Order:       2,
			DependsOn:   []int{1},
		},
		{
			Title:       "子需求 C",
			Description: "写集成测试",
			Scope:       []string{"backend/internal/handler/a_test.go"},
			Acceptance:  []string{"测试通过"},
			Boundary:    "C 只测 A 与 B",
			Order:       3,
			DependsOn:   []int{2},
		},
	}
}

// ── tests ─────────────────────────────────────────────────────────────────

// The whole point of the split: a batch of sub-requirements lands as SIBLINGS
// under one master, all sharing the master's base branch, all carrying the
// same batch id, and the master is flagged as having handed development down.
func TestCreateChildren_LandsSiblingsWithMasterBase(t *testing.T) {
	svc := newSplitSvc(t)
	seedSplitProject(t, svc, "proj_a", "main")
	seedSplitReq(t, svc, "req_master", "proj_a", "大需求", "release/x", model.BaseSourceExplicit, "designed")

	master, err := svc.Get("req_master")
	if err != nil {
		t.Fatalf("Get master: %v", err)
	}
	children, err := svc.CreateChildren(master, splitTasks(), nil, SplitCreateOpts{})
	if err != nil {
		t.Fatalf("CreateChildren: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("got %d children, want 3", len(children))
	}

	batch := ""
	for i, c := range children {
		if c.ParentID != "req_master" {
			t.Errorf("child %d: parent_id = %q, want req_master", i, c.ParentID)
		}
		if c.BaseBranch != "release/x" {
			t.Errorf("child %d: base_branch = %q, want release/x", i, c.BaseBranch)
		}
		if c.BaseBranchSource != model.BaseSourceParent {
			t.Errorf("child %d: base_branch_source = %q, want %q", i, c.BaseBranchSource, model.BaseSourceParent)
		}
		if c.BaseBranchSource == model.BaseSourceExplicit {
			t.Errorf("child %d: base_branch_source must never be explicit", i)
		}
		if c.DevDelegated {
			t.Errorf("child %d: dev_delegated must be false on children", i)
		}
		if c.Status != "draft" {
			t.Errorf("child %d: status = %q, want draft", i, c.Status)
		}
		if c.DesignDocs != "[]" {
			t.Errorf("child %d: design_docs = %q, want the empty-doc marker '[]'", i, c.DesignDocs)
		}
		if c.SplitBatchID == "" {
			t.Errorf("child %d: split_batch_id is empty", i)
		}
		if i == 0 {
			batch = c.SplitBatchID
		} else if c.SplitBatchID != batch {
			t.Errorf("child %d: split_batch_id = %q, want %q (same batch)", i, c.SplitBatchID, batch)
		}
		// The description is assembled deterministically from the candidate.
		if !strings.Contains(c.Description, "## 目标") ||
			!strings.Contains(c.Description, "## 涉及范围") ||
			!strings.Contains(c.Description, "## 验收标准") ||
			!strings.Contains(c.Description, "## 与其它子需求的边界") {
			t.Errorf("child %d: description is missing a section:\n%s", i, c.Description)
		}
		var criteria []string
		if err := json.Unmarshal([]byte(c.AcceptanceCriteria), &criteria); err != nil {
			t.Errorf("child %d: acceptance_criteria is not a JSON array: %v", i, err)
		} else if len(criteria) != 1 {
			t.Errorf("child %d: acceptance_criteria = %v, want 1 entry", i, criteria)
		}
	}

	// Children are siblings, never parent/child of each other.
	for i, c := range children {
		for j, other := range children {
			if i == j {
				continue
			}
			if c.ParentID == other.ID {
				t.Errorf("child %d is the parent of child %d — children must stay siblings", i, j)
			}
		}
	}

	// The master handed development down.
	updatedMaster, err := svc.Get("req_master")
	if err != nil {
		t.Fatalf("Get master after split: %v", err)
	}
	if !updatedMaster.DevDelegated {
		t.Error("master dev_delegated = false, want true after a split")
	}
}

// A master with no base of its own falls back to the project default, and
// the source is still "parent" — the child followed the master, it did not
// pick a branch itself.
func TestCreateChildren_BaseFallsBackToProjectDefault(t *testing.T) {
	svc := newSplitSvc(t)
	seedSplitProject(t, svc, "proj_a", "develop")
	seedSplitReq(t, svc, "req_master", "proj_a", "大需求", "", "", "designed")

	master, err := svc.Get("req_master")
	if err != nil {
		t.Fatalf("Get master: %v", err)
	}
	children, err := svc.CreateChildren(master, splitTasks()[:1], nil, SplitCreateOpts{})
	if err != nil {
		t.Fatalf("CreateChildren: %v", err)
	}
	if children[0].BaseBranch != "develop" {
		t.Errorf("base_branch = %q, want the project default 'develop'", children[0].BaseBranch)
	}
	if children[0].BaseBranchSource != model.BaseSourceParent {
		t.Errorf("base_branch_source = %q, want %q", children[0].BaseBranchSource, model.BaseSourceParent)
	}
}

// A cycle must abort the WHOLE batch — no orphaned rows survive.
func TestCreateChildren_RollsBackOnDependencyCycle(t *testing.T) {
	svc := newSplitSvc(t)
	seedSplitProject(t, svc, "proj_a", "main")
	seedSplitReq(t, svc, "req_master", "proj_a", "大需求", "main", model.BaseSourceProject, "designed")

	master, err := svc.Get("req_master")
	if err != nil {
		t.Fatalf("Get master: %v", err)
	}
	// A → B → A is a cycle. The ids are pre-minted (as the split handler
	// does) so the edges can reference the very rows being inserted.
	tasks := splitTasks()[:2]
	tasks[0].ID = "req_a"
	tasks[1].ID = "req_b"
	edges := []model.DependencyEdge{
		{RequirementID: "req_a", DependsOnID: "req_b"},
		{RequirementID: "req_b", DependsOnID: "req_a"},
	}
	_, err = svc.CreateChildren(master, tasks, edges, SplitCreateOpts{})
	if err == nil {
		t.Fatal("CreateChildren accepted a dependency cycle")
	}
	if !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("err = %v, want ErrDependencyCycle", err)
	}

	var n int
	if err := svc.db.QueryRow("SELECT COUNT(1) FROM requirements WHERE parent_id='req_master'").Scan(&n); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d child rows after a rolled-back cycle, want 0", n)
	}
	// The master must not have been flagged either.
	after, err := svc.Get("req_master")
	if err != nil {
		t.Fatalf("Get master: %v", err)
	}
	if after.DevDelegated {
		t.Error("master dev_delegated = true after a rolled-back split")
	}
}

// The dependency gate: a prerequisite that is not done blocks the dependent.
func TestUnmetDependencies(t *testing.T) {
	svc := newSplitSvc(t)
	seedSplitProject(t, svc, "proj_a", "main")
	seedSplitReq(t, svc, "req_master", "proj_a", "大需求", "main", model.BaseSourceProject, "designed")

	master, err := svc.Get("req_master")
	if err != nil {
		t.Fatalf("Get master: %v", err)
	}
	children, err := svc.CreateChildren(master, splitTasks(), nil, SplitCreateOpts{})
	if err != nil {
		t.Fatalf("CreateChildren: %v", err)
	}
	a, b := children[0], children[1]
	if _, err := svc.db.Exec(
		`INSERT INTO requirement_dependencies (id, requirement_id, depends_on_id, batch_id, created_at) VALUES ('dep_1', ?, ?, 'spb_x', CURRENT_TIMESTAMP)`,
		b.ID, a.ID); err != nil {
		t.Fatalf("insert edge: %v", err)
	}

	unmet, err := svc.UnmetDependencies(b.ID)
	if err != nil {
		t.Fatalf("UnmetDependencies: %v", err)
	}
	if len(unmet) != 1 || unmet[0].ID != a.ID {
		t.Fatalf("unmet = %v, want exactly %s", unmet, a.ID)
	}

	// Once the prerequisite is done, nothing is unmet.
	if _, err := svc.db.Exec("UPDATE requirements SET status='done' WHERE id=?", a.ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	unmet, err = svc.UnmetDependencies(b.ID)
	if err != nil {
		t.Fatalf("UnmetDependencies: %v", err)
	}
	if len(unmet) != 0 {
		t.Fatalf("unmet = %v, want none", unmet)
	}
}

// Deleting a prerequisite must clear the edge in BOTH directions — a dangling
// edge would block the dependent's coding gate forever.
func TestDelete_ClearsDependencyEdgesBothWays(t *testing.T) {
	svc := newSplitSvc(t)
	seedSplitProject(t, svc, "proj_a", "main")
	seedSplitReq(t, svc, "req_master", "proj_a", "大需求", "main", model.BaseSourceProject, "designed")

	master, err := svc.Get("req_master")
	if err != nil {
		t.Fatalf("Get master: %v", err)
	}
	children, err := svc.CreateChildren(master, splitTasks()[:2], nil, SplitCreateOpts{})
	if err != nil {
		t.Fatalf("CreateChildren: %v", err)
	}
	a, b := children[0], children[1]
	if _, err := svc.db.Exec(
		`INSERT INTO requirement_dependencies (id, requirement_id, depends_on_id, batch_id, created_at) VALUES ('dep_1', ?, ?, 'spb_x', CURRENT_TIMESTAMP)`,
		b.ID, a.ID); err != nil {
		t.Fatalf("insert edge: %v", err)
	}

	if err := svc.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var n int
	if err := svc.db.QueryRow("SELECT COUNT(1) FROM requirement_dependencies").Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d dependency edges after deleting the prerequisite, want 0", n)
	}
}

// A master can only be closed once every child is done.
func TestUpdateStatus_MasterNeedsAllChildrenDone(t *testing.T) {
	svc := newSplitSvc(t)
	seedSplitProject(t, svc, "proj_a", "main")
	seedSplitReq(t, svc, "req_master", "proj_a", "大需求", "main", model.BaseSourceProject, "designed")

	master, err := svc.Get("req_master")
	if err != nil {
		t.Fatalf("Get master: %v", err)
	}
	children, err := svc.CreateChildren(master, splitTasks()[:2], nil, SplitCreateOpts{})
	if err != nil {
		t.Fatalf("CreateChildren: %v", err)
	}

	if _, err := svc.UpdateStatus("req_master", "done"); err == nil {
		t.Fatal("master was marked done while its children were still open")
	} else if !errors.Is(err, ErrChildrenNotDone) {
		t.Fatalf("err = %v, want ErrChildrenNotDone", err)
	}

	for _, c := range children {
		if _, err := svc.db.Exec("UPDATE requirements SET status='done' WHERE id=?", c.ID); err != nil {
			t.Fatalf("mark child done: %v", err)
		}
	}
	if _, err := svc.UpdateStatus("req_master", "done"); err != nil {
		t.Fatalf("UpdateStatus(done) with all children done: %v", err)
	}
}
