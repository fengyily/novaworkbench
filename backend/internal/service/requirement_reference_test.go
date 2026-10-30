package service

import (
	"errors"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// seedRefReq inserts a requirement row directly so the test owns the exact
// column values (parent_id, referenced_requirements) that Create/SetParent
// would otherwise normalize away.
func seedRefReq(t *testing.T, svc *RequirementService, id, projectID, title, description, design, parentID, refs string) {
	t.Helper()
	if _, err := svc.db.Exec(
		`INSERT INTO requirements (id, project_id, title, description, design_docs, parent_id, referenced_requirements) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, projectID, title, description, design, parentID, refs,
	); err != nil {
		t.Fatalf("insert requirement %s: %v", id, err)
	}
}

// seedRefProject inserts a project row.
func seedRefProject(t *testing.T, svc *RequirementService, id, name string) {
	t.Helper()
	if _, err := svc.db.Exec(
		`INSERT OR IGNORE INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		id, name, "/tmp/"+id, "ready", "main",
	); err != nil {
		t.Fatalf("insert project %s: %v", id, err)
	}
}

func newRefSvc(t *testing.T) *RequirementService {
	t.Helper()
	d := newServiceTestDB(t)
	return NewRequirementService(d, nil)
}

// A reference set round-trips through the JSON column and comes back
// resolved: the target's display fields are filled in and the origin is
// marked "direct".
func TestSetReferences_RoundTrip(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefProject(t, svc, "proj_b", "B")
	seedRefReq(t, svc, "req_self", "proj_a", "本需求", "描述", "", "", "[]")
	seedRefReq(t, svc, "req_target", "proj_b", "被引用需求", "被引用描述", "# 既有方案", "", "[]")

	items, err := svc.SetReferences("req_self", []model.RequirementReferenceInput{
		{RequirementID: "req_target", Note: "复用其鉴权中间件"},
	})
	if err != nil {
		t.Fatalf("SetReferences: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d references, want 1", len(items))
	}
	got := items[0]
	if got.RequirementID != "req_target" {
		t.Errorf("requirement_id = %q, want req_target", got.RequirementID)
	}
	if got.Note != "复用其鉴权中间件" {
		t.Errorf("note = %q", got.Note)
	}
	if got.Origin != model.ReferenceOriginDirect {
		t.Errorf("origin = %q, want direct", got.Origin)
	}
	// The display fields are resolved for the UI …
	if got.Title != "被引用需求" {
		t.Errorf("title = %q, want 被引用需求", got.Title)
	}
	if got.ProjectName != "B" {
		t.Errorf("project_name = %q, want B", got.ProjectName)
	}
	if !got.HasDesign {
		t.Error("has_design must be true for a target with a plan")
	}
	// … and the body is NOT part of the view.
	content, err := svc.ResolveReferenceContent("req_self")
	if err != nil {
		t.Fatalf("ResolveReferenceContent: %v", err)
	}
	if len(content) != 1 || content[0].Description != "被引用描述" || content[0].DesignDocs != "# 既有方案" {
		t.Errorf("ResolveReferenceContent = %+v", content)
	}
}

// A requirement cannot reference itself — the user asked for something
// impossible and deserves to be told, not silently ignored.
func TestSetReferences_RejectsSelfReference(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefReq(t, svc, "req_self", "proj_a", "本需求", "描述", "", "", "[]")

	_, err := svc.SetReferences("req_self", []model.RequirementReferenceInput{
		{RequirementID: "req_self", Note: "自己"},
	})
	if !errors.Is(err, ErrReferenceSelf) {
		t.Fatalf("err = %v, want ErrReferenceSelf", err)
	}
}

// A dangling id would be silently dropped at read time, so the user would
// think their reference stuck. Reject it at write time instead.
func TestSetReferences_RejectsMissingTarget(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefReq(t, svc, "req_self", "proj_a", "本需求", "描述", "", "", "[]")

	_, err := svc.SetReferences("req_self", []model.RequirementReferenceInput{
		{RequirementID: "req_gone"},
	})
	if !errors.Is(err, ErrReferenceNotFound) {
		t.Fatalf("err = %v, want ErrReferenceNotFound", err)
	}
}

// Cross-project references are the whole point of the feature ("参考另一个
// 项目里已定稿的方案"), and are deliberately different from parent_id's
// same-project rule.
func TestSetReferences_AllowsCrossProject(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefProject(t, svc, "proj_b", "B")
	seedRefReq(t, svc, "req_self", "proj_a", "本需求", "描述", "", "", "[]")
	seedRefReq(t, svc, "req_target", "proj_b", "被引用需求", "描述", "", "", "[]")

	items, err := svc.SetReferences("req_self", []model.RequirementReferenceInput{
		{RequirementID: "req_target"},
	})
	if err != nil {
		t.Fatalf("SetReferences: %v", err)
	}
	if len(items) != 1 || items[0].ProjectID != "proj_b" {
		t.Fatalf("cross-project reference was dropped: %+v", items)
	}
}

// Duplicates collapse (first-seen wins) and the count is capped, so a runaway
// UI can never blow past the prompt budget.
func TestSetReferences_DedupesAndCaps(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefReq(t, svc, "req_self", "proj_a", "本需求", "描述", "", "", "[]")
	for i := 0; i < 12; i++ {
		seedRefReq(t, svc, "req_t"+string(rune('a'+i)), "proj_a", "目标"+string(rune('a'+i)), "描述", "", "", "[]")
	}

	items, err := svc.SetReferences("req_self", []model.RequirementReferenceInput{
		{RequirementID: "req_ta", Note: "第一条"},
		{RequirementID: "req_ta", Note: "重复"},
		{RequirementID: "req_tb"},
	})
	if err != nil {
		t.Fatalf("SetReferences: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d references, want 2 (dupes collapsed)", len(items))
	}
	if items[0].Note != "第一条" {
		t.Errorf("first-seen must win, got note %q", items[0].Note)
	}

	var many []model.RequirementReferenceInput
	for i := 0; i < 20; i++ {
		many = append(many, model.RequirementReferenceInput{RequirementID: "req_t" + string(rune('a'+i))})
	}
	items, err = svc.SetReferences("req_self", many)
	if err != nil {
		t.Fatalf("SetReferences: %v", err)
	}
	if len(items) != maxReferenceCount {
		t.Errorf("got %d references, want the %d cap", len(items), maxReferenceCount)
	}
}

// The note is truncated on a rune boundary — a byte cut would split a Chinese
// character and store invalid UTF-8 in the JSON column.
func TestSetReferences_TruncatesNoteOnRuneBoundary(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefReq(t, svc, "req_self", "proj_a", "本需求", "描述", "", "", "[]")
	seedRefReq(t, svc, "req_target", "proj_a", "目标", "描述", "", "", "[]")

	long := ""
	for i := 0; i < 200; i++ {
		long += "因"
	}
	items, err := svc.SetReferences("req_self", []model.RequirementReferenceInput{
		{RequirementID: "req_target", Note: long},
	})
	if err != nil {
		t.Fatalf("SetReferences: %v", err)
	}
	if len([]rune(items[0].Note)) != maxReferenceNoteRunes {
		t.Errorf("note runes = %d, want %d", len([]rune(items[0].Note)), maxReferenceNoteRunes)
	}
	for _, r := range items[0].Note {
		if r != '因' {
			t.Fatalf("note was cut mid-character: %q", items[0].Note)
		}
	}
}

// A child requirement inherits its master's reference set, marked "parent".
func TestResolveReferenceContent_InheritsFromParent(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefReq(t, svc, "req_master", "proj_a", "主需求", "描述", "", "", "[]")
	seedRefReq(t, svc, "req_child", "proj_a", "子需求", "描述", "", "req_master", "[]")
	seedRefReq(t, svc, "req_target", "proj_a", "目标", "目标描述", "# 目标方案", "", "[]")

	if _, err := svc.SetReferences("req_master", []model.RequirementReferenceInput{
		{RequirementID: "req_target", Note: "主需求引用"},
	}); err != nil {
		t.Fatalf("SetReferences(master): %v", err)
	}

	items, err := svc.ListReferences("req_child")
	if err != nil {
		t.Fatalf("ListReferences: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("child got %d references, want the inherited one", len(items))
	}
	if items[0].Origin != model.ReferenceOriginParent {
		t.Errorf("origin = %q, want parent", items[0].Origin)
	}
	if items[0].Note != "主需求引用" {
		t.Errorf("note = %q, want the inherited note", items[0].Note)
	}
}

// A master requirement that is itself a child breaks the one-level rule —
// no inheritance, but no error either.
func TestResolveReferenceContent_SkipsInvalidParent(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefProject(t, svc, "proj_b", "B")
	seedRefReq(t, svc, "req_grand", "proj_a", "顶层", "描述", "", "", "[]")
	seedRefReq(t, svc, "req_master", "proj_a", "中层", "描述", "", "req_grand", "[]")
	seedRefReq(t, svc, "req_child", "proj_a", "子需求", "描述", "", "req_master", "[]")
	seedRefReq(t, svc, "req_target", "proj_a", "目标", "描述", "", "", "[]")

	if _, err := svc.SetReferences("req_master", []model.RequirementReferenceInput{
		{RequirementID: "req_target"},
	}); err != nil {
		t.Fatalf("SetReferences: %v", err)
	}

	items, err := svc.ListReferences("req_child")
	if err != nil {
		t.Fatalf("ListReferences: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("child inherited %d references from a nested master, want 0", len(items))
	}
}

// A cross-project master is not a valid parent either — same degrade-don't-
// fail rule as the parent-context block.
func TestResolveReferenceContent_SkipsCrossProjectParent(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefProject(t, svc, "proj_b", "B")
	seedRefReq(t, svc, "req_master", "proj_b", "主需求", "描述", "", "", "[]")
	seedRefReq(t, svc, "req_child", "proj_a", "子需求", "描述", "", "req_master", "[]")
	seedRefReq(t, svc, "req_target", "proj_b", "目标", "描述", "", "", "[]")

	if _, err := svc.SetReferences("req_master", []model.RequirementReferenceInput{
		{RequirementID: "req_target"},
	}); err != nil {
		t.Fatalf("SetReferences: %v", err)
	}
	items, err := svc.ListReferences("req_child")
	if err != nil {
		t.Fatalf("ListReferences: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("child inherited %d references across projects, want 0", len(items))
	}
}

// A target that was deleted leaves a dangling id behind. The design run must
// not fail because of it — the reference is skipped and the run continues.
func TestResolveReferenceContent_SkipsDanglingReference(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefReq(t, svc, "req_self", "proj_a", "本需求", "描述", "", "", "[]")
	seedRefReq(t, svc, "req_target", "proj_a", "目标", "描述", "", "", "[]")

	if _, err := svc.SetReferences("req_self", []model.RequirementReferenceInput{
		{RequirementID: "req_target", Note: "会被删掉"},
	}); err != nil {
		t.Fatalf("SetReferences: %v", err)
	}
	if _, err := svc.db.Exec("DELETE FROM requirements WHERE id = ?", "req_target"); err != nil {
		t.Fatalf("delete target: %v", err)
	}

	items, err := svc.ListReferences("req_self")
	if err != nil {
		t.Fatalf("ListReferences must degrade, got error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("got %d references, want the dangling one skipped", len(items))
	}
}

// The master requirement must not appear in the child's reference set — it is
// already fully covered by the parent-context block, so repeating it would
// waste prompt budget and confuse the model.
func TestResolveReferenceContent_ExcludesSelfAndParent(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefReq(t, svc, "req_master", "proj_a", "主需求", "描述", "", "", "[]")
	seedRefReq(t, svc, "req_child", "proj_a", "子需求", "描述", "", "req_master", "[]")
	seedRefReq(t, svc, "req_other", "proj_a", "别的需求", "描述", "", "", "[]")

	// The master references the child itself (legal — a master may point at
	// its own child) and the child references the master directly.
	if _, err := svc.SetReferences("req_master", []model.RequirementReferenceInput{
		{RequirementID: "req_child"},
	}); err != nil {
		t.Fatalf("SetReferences(master): %v", err)
	}
	if _, err := svc.SetReferences("req_child", []model.RequirementReferenceInput{
		{RequirementID: "req_master"},
		{RequirementID: "req_other"},
	}); err != nil {
		t.Fatalf("SetReferences(child): %v", err)
	}

	items, err := svc.ListReferences("req_child")
	if err != nil {
		t.Fatalf("ListReferences: %v", err)
	}
	for _, r := range items {
		if r.RequirementID == "req_child" || r.RequirementID == "req_master" {
			t.Errorf("reference set must exclude self / parent, got %+v", items)
		}
	}
	if len(items) != 1 || items[0].RequirementID != "req_other" {
		t.Errorf("got %+v, want only req_other", items)
	}
}

// A requirement with no references must resolve to an empty (non-nil) slice
// so the JSON response is `[]` rather than `null`.
func TestListReferences_EmptyForNoReferences(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefReq(t, svc, "req_self", "proj_a", "本需求", "描述", "", "", "[]")

	items, err := svc.ListReferences("req_self")
	if err != nil {
		t.Fatalf("ListReferences: %v", err)
	}
	if items == nil {
		t.Fatal("ListReferences returned nil, want an empty slice")
	}
	if len(items) != 0 {
		t.Fatalf("got %d references, want 0", len(items))
	}
}

// A malformed JSON column must not take the design run down — the column is
// advisory context, never a hard dependency.
func TestResolveReferenceContent_ToleratesMalformedColumn(t *testing.T) {
	svc := newRefSvc(t)
	seedRefProject(t, svc, "proj_a", "A")
	seedRefReq(t, svc, "req_self", "proj_a", "本需求", "描述", "", "", "{not json")

	items, err := svc.ListReferences("req_self")
	if err != nil {
		t.Fatalf("ListReferences must tolerate a malformed column, got: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("got %d references, want 0", len(items))
	}
}
