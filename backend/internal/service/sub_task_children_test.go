package service

// sub_task_children_test.go — SubTaskService.ListChildren.
//
// The requirement-level Review's auto-iteration chain asks a narrow
// question: "which fix sub-tasks belong to THIS round?". Subtree would
// answer a wider one (every descendant), and the wider answer is wrong —
// it would drag in the NEXT round's fix children and make the chain look
// unfinished long after it was.

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestSubTaskService_ListChildren pins the narrow query.
func TestSubTaskService_ListChildren(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewSubTaskService(d, nil)

	const reqID = "req_children"
	if _, err := d.Exec(`INSERT INTO requirements (id, title, project_id) VALUES (?, ?, ?)`,
		reqID, "子任务树", "proj_x"); err != nil {
		t.Skipf("seed requirement: %v", err)
	}

	// root → child1 → grandchild
	root, err := svc.Create(reqID, "root", "p", "", "", "", 0, "", model.SubTaskSessionModeFork, "", model.SubTaskSourceManual, model.SubTaskReportModeAuto, "")
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child1, err := svc.Create(reqID, "child1", "p", "", "", "", 0, "", model.SubTaskSessionModeFork, root.ID, model.SubTaskSourceManual, model.SubTaskReportModeAuto, "")
	if err != nil {
		t.Fatalf("create child1: %v", err)
	}
	if _, err := svc.Create(reqID, "grandchild", "p", "", "", "", 0, "", model.SubTaskSessionModeFork, child1.ID, model.SubTaskSourceManual, model.SubTaskReportModeAuto, ""); err != nil {
		t.Fatalf("create grandchild: %v", err)
	}
	child2, err := svc.Create(reqID, "child2", "p", "", "", "", 0, "", model.SubTaskSessionModeFork, root.ID, model.SubTaskSourceManual, model.SubTaskReportModeAuto, "")
	if err != nil {
		t.Fatalf("create child2: %v", err)
	}

	children, err := svc.ListChildren(root.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("children = %d, want 2 (the direct children only)", len(children))
	}
	ids := map[string]bool{}
	for _, c := range children {
		ids[c.ID] = true
		if c.ParentSubtaskID != root.ID {
			t.Errorf("child %s has parent %q, want %q", c.ID, c.ParentSubtaskID, root.ID)
		}
	}
	if !ids[child1.ID] || !ids[child2.ID] {
		t.Errorf("children = %v, want child1 + child2", ids)
	}

	// A leaf has no children.
	leaves, err := svc.ListChildren(child1.ID)
	if err != nil {
		t.Fatalf("ListChildren(leaf): %v", err)
	}
	if len(leaves) != 1 {
		t.Fatalf("leaf children = %d, want 1 (the grandchild)", len(leaves))
	}

	// An empty parent id is not a query — it is a no-op.
	none, err := svc.ListChildren("")
	if err != nil {
		t.Fatalf("ListChildren(\"\"): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("ListChildren(\"\") = %d rows, want 0", len(none))
	}
}
