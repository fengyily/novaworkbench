package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// A "互相参考" pair is undirected, but only the out-edge used to be stored —
// the requirement on the "to" side never saw the "from" side's plan. Both
// sides must be written.
func TestApplySplitReferences_WritesBothDirections(t *testing.T) {
	d := newParentCtxDB(t)
	reqSvc := service.NewRequirementService(d, nil)
	h := &WizardHandler{reqSvc: reqSvc}

	const projID = "proj_split_refs"
	if _, err := d.Exec(`INSERT OR IGNORE INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "SplitRefs", "/tmp/splitrefs", "ready", "main"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	children := []*model.Requirement{
		{ID: "req_s1", ProjectID: projID, Title: "一"},
		{ID: "req_s2", ProjectID: projID, Title: "二"},
	}
	for _, c := range children {
		if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title, description) VALUES (?, ?, ?, ?)`,
			c.ID, c.ProjectID, c.Title, "描述"); err != nil {
			t.Fatalf("insert %s: %v", c.ID, err)
		}
	}

	err := h.applySplitReferences(children, []model.SplitReferencePair{
		{From: 1, To: 2, Reason: "2 的上报格式由 1 定义"},
	})
	if err != nil {
		t.Fatalf("applySplitReferences: %v", err)
	}

	for _, id := range []string{"req_s1", "req_s2"} {
		var blob string
		if err := d.QueryRow(`SELECT referenced_requirements FROM requirements WHERE id = ?`, id).Scan(&blob); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		var refs []model.RequirementReferenceInput
		if err := json.Unmarshal([]byte(blob), &refs); err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
		if len(refs) != 1 {
			t.Fatalf("%s references = %d, want 1 (the pair must be visible from both sides)", id, len(refs))
		}
		other := "req_s2"
		if id == "req_s2" {
			other = "req_s1"
		}
		if refs[0].RequirementID != other {
			t.Fatalf("%s references %q, want %q", id, refs[0].RequirementID, other)
		}
		// The model's reason must survive — the split contract says it is
		// the architect's own judgment and "写废话等于没判断".
		if !strings.Contains(refs[0].Note, "2 的上报格式由 1 定义") {
			t.Fatalf("%s note = %q, want the model's reason", id, refs[0].Note)
		}
	}
}

// An empty reason falls back to the generic note rather than storing "".
func TestApplySplitReferences_EmptyReasonFallsBack(t *testing.T) {
	d := newParentCtxDB(t)
	reqSvc := service.NewRequirementService(d, nil)
	h := &WizardHandler{reqSvc: reqSvc}

	const projID = "proj_split_refs2"
	if _, err := d.Exec(`INSERT OR IGNORE INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "SplitRefs2", "/tmp/splitrefs2", "ready", "main"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	children := []*model.Requirement{
		{ID: "req_t1", ProjectID: projID, Title: "一"},
		{ID: "req_t2", ProjectID: projID, Title: "二"},
	}
	for _, c := range children {
		if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title, description) VALUES (?, ?, ?, ?)`,
			c.ID, c.ProjectID, c.Title, "描述"); err != nil {
			t.Fatalf("insert %s: %v", c.ID, err)
		}
	}
	if err := h.applySplitReferences(children, []model.SplitReferencePair{{From: 1, To: 2}}); err != nil {
		t.Fatalf("applySplitReferences: %v", err)
	}
	var blob string
	if err := d.QueryRow(`SELECT referenced_requirements FROM requirements WHERE id = ?`, "req_t1").Scan(&blob); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(blob, "同批次拆分，互相关联") {
		t.Fatalf("note = %q, want the fallback note", blob)
	}
}

// The confirm endpoint's body must accept both the legacy "[from, to]" shape
// and the richer object shape — a browser tab left open across a deploy
// still has to work.
func TestSplitReferencePair_UnmarshalBothShapes(t *testing.T) {
	var legacy model.SplitReferencePair
	if err := json.Unmarshal([]byte(`[1,2]`), &legacy); err != nil {
		t.Fatalf("legacy shape: %v", err)
	}
	if legacy.From != 1 || legacy.To != 2 {
		t.Fatalf("legacy = %+v", legacy)
	}

	var rich model.SplitReferencePair
	if err := json.Unmarshal([]byte(`{"from":3,"to":4,"reason":"原因"}`), &rich); err != nil {
		t.Fatalf("object shape: %v", err)
	}
	if rich.From != 3 || rich.To != 4 || rich.Reason != "原因" {
		t.Fatalf("object = %+v", rich)
	}

	var bad model.SplitReferencePair
	if err := json.Unmarshal([]byte(`[1]`), &bad); err == nil {
		t.Fatal("a 1-element array must be rejected")
	}
}
