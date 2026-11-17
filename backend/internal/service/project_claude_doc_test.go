package service

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// seedClaudeDocProject inserts a project row the doc service can attach to.
func seedClaudeDocProject(t *testing.T, s *ProjectService, id string) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?)`,
		id, "doc project", "/tmp/nova-claude-doc-"+id)
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// TestClaudeDocRoundTrip pins the CRUD contract of the project-level
// CLAUDE.md: one row per project, upsert semantics, and the rule that a
// manual edit does NOT touch the "last generated" stamp.
func TestClaudeDocRoundTrip(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewProjectService(d, nil, nil)
	const proj = "proj_doc_1"
	seedClaudeDocProject(t, svc, proj)

	// Missing document → (nil, nil), NOT an error: the injection helper
	// treats that as "no project conventions" and leaves the prompt alone.
	doc, err := svc.ClaudeDoc(proj)
	if err != nil {
		t.Fatalf("ClaudeDoc (missing): %v", err)
	}
	if doc != nil {
		t.Fatalf("ClaudeDoc (missing) = %+v, want nil", doc)
	}

	// AI generation → source=generated + generated_at set.
	doc, err = svc.UpsertClaudeDoc(proj, "# 约定\n\n用 Go 1.25。", model.ProjectClaudeDocSourceGenerated, true)
	if err != nil {
		t.Fatalf("UpsertClaudeDoc (generated): %v", err)
	}
	if doc == nil || doc.Content != "# 约定\n\n用 Go 1.25。" {
		t.Fatalf("UpsertClaudeDoc returned %+v", doc)
	}
	if doc.Source != model.ProjectClaudeDocSourceGenerated {
		t.Errorf("source = %q, want %q", doc.Source, model.ProjectClaudeDocSourceGenerated)
	}
	if doc.GeneratedAt == nil {
		t.Fatal("generated_at is nil after a generated upsert")
	}
	firstGenerated := *doc.GeneratedAt

	// Manual edit → source=manual, generated_at UNCHANGED.
	doc, err = svc.UpsertClaudeDoc(proj, "# 约定\n\n用 Go 1.26。", model.ProjectClaudeDocSourceManual, false)
	if err != nil {
		t.Fatalf("UpsertClaudeDoc (manual): %v", err)
	}
	if doc.Source != model.ProjectClaudeDocSourceManual {
		t.Errorf("source = %q, want %q", doc.Source, model.ProjectClaudeDocSourceManual)
	}
	if doc.GeneratedAt == nil {
		t.Fatal("generated_at is nil after a manual edit")
	}
	if !doc.GeneratedAt.Equal(firstGenerated) {
		t.Errorf("generated_at changed on a manual edit: %v → %v", firstGenerated, *doc.GeneratedAt)
	}

	// One row per project: the second upsert updated in place.
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM project_claude_docs WHERE project_id = ?`, proj).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1", count)
	}

	// Delete → the document is gone (nil, nil again).
	if err := svc.DeleteClaudeDoc(proj); err != nil {
		t.Fatalf("DeleteClaudeDoc: %v", err)
	}
	doc, err = svc.ClaudeDoc(proj)
	if err != nil {
		t.Fatalf("ClaudeDoc (after delete): %v", err)
	}
	if doc != nil {
		t.Fatalf("ClaudeDoc (after delete) = %+v, want nil", doc)
	}
	// Deleting twice is a no-op, not an error.
	if err := svc.DeleteClaudeDoc(proj); err != nil {
		t.Fatalf("DeleteClaudeDoc (second): %v", err)
	}
}

// TestClaudeDocManualAfterGeneratedKeepsStamp is the regression guard for
// the exact sequence the UI produces: generate → hand-edit → save. The
// "最后生成" badge must still show the generation time, not the edit time.
func TestClaudeDocManualAfterGeneratedKeepsStamp(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewProjectService(d, nil, nil)
	const proj = "proj_doc_2"
	seedClaudeDocProject(t, svc, proj)

	if _, err := svc.UpsertClaudeDoc(proj, "a", model.ProjectClaudeDocSourceGenerated, true); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	before, err := svc.ClaudeDoc(proj)
	if err != nil {
		t.Fatalf("ClaudeDoc: %v", err)
	}
	// Wait a tick so a "now" stamp would be distinguishable.
	time.Sleep(1100 * time.Millisecond)
	if _, err := svc.UpsertClaudeDoc(proj, "b", model.ProjectClaudeDocSourceManual, false); err != nil {
		t.Fatalf("upsert (manual): %v", err)
	}
	after, err := svc.ClaudeDoc(proj)
	if err != nil {
		t.Fatalf("ClaudeDoc: %v", err)
	}
	if after.GeneratedAt == nil || before.GeneratedAt == nil {
		t.Fatal("generated_at missing")
	}
	if !after.GeneratedAt.Equal(*before.GeneratedAt) {
		t.Errorf("generated_at moved on a manual edit: %v → %v", *before.GeneratedAt, *after.GeneratedAt)
	}
	if after.Content != "b" {
		t.Errorf("content = %q, want %q", after.Content, "b")
	}
}

// TestReadDiskClaudeDoc covers the "从磁盘导入" path: a project that ships
// its own CLAUDE.md can seed the managed copy from it.
func TestReadDiskClaudeDoc(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewProjectService(d, nil, nil)

	dir := t.TempDir()
	content, exists, err := svc.ReadDiskClaudeDoc(dir)
	if err != nil {
		t.Fatalf("ReadDiskClaudeDoc (missing): %v", err)
	}
	if exists {
		t.Error("exists = true for a directory without CLAUDE.md")
	}
	if content != "" {
		t.Errorf("content = %q, want empty", content)
	}

	path := dir + "/CLAUDE.md"
	if err := os.WriteFile(path, []byte("# 磁盘上的约定\n"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	content, exists, err = svc.ReadDiskClaudeDoc(dir)
	if err != nil {
		t.Fatalf("ReadDiskClaudeDoc: %v", err)
	}
	if !exists {
		t.Error("exists = false for a directory with CLAUDE.md")
	}
	if content != "# 磁盘上的约定\n" {
		t.Errorf("content = %q", content)
	}
	if !DiskClaudeDocExists(dir) {
		t.Error("DiskClaudeDocExists = false for a directory with CLAUDE.md")
	}
	if DiskClaudeDocExists(t.TempDir()) {
		t.Error("DiskClaudeDocExists = true for a directory without CLAUDE.md")
	}
}

// TestClaudeDocManualEditFlag pins the guard flag the destructive replaces
// check: a hand-edited document is marked, a machine-written one is not.
//
// Without the flag the AI regeneration silently replaces what the user
// wrote by hand — the exact P0 the review reported.
func TestClaudeDocManualEditFlag(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewProjectService(d, nil, nil)
	const proj = "proj_doc_manual"
	seedClaudeDocProject(t, svc, proj)

	// AI-generated → not hand-edited.
	doc, err := svc.UpsertClaudeDoc(proj, "ai", model.ProjectClaudeDocSourceGenerated, true)
	if err != nil {
		t.Fatalf("upsert (generated): %v", err)
	}
	if doc.ManualEditedAt != nil {
		t.Errorf("manual_edited_at = %v, want nil for a generated document", doc.ManualEditedAt)
	}

	// Hand edit → marked.
	doc, err = svc.UpsertClaudeDoc(proj, "human", model.ProjectClaudeDocSourceManual, false)
	if err != nil {
		t.Fatalf("upsert (manual): %v", err)
	}
	if doc.ManualEditedAt == nil {
		t.Fatal("manual_edited_at is nil after a hand edit")
	}

	// A regeneration that was explicitly confirmed clears the flag again —
	// the human content is gone, so nothing is protected anymore.
	doc, err = svc.UpsertClaudeDoc(proj, "ai again", model.ProjectClaudeDocSourceGenerated, true)
	if err != nil {
		t.Fatalf("upsert (regenerated): %v", err)
	}
	if doc.ManualEditedAt != nil {
		t.Errorf("manual_edited_at = %v, want nil after a confirmed regeneration", doc.ManualEditedAt)
	}
}

// TestClaudeDocRevisionSnapshotAndRestore is the recovery path: the
// version that was replaced must still be there afterwards, and restoring
// it must bring the content back.
func TestClaudeDocRevisionSnapshotAndRestore(t *testing.T) {
	d := newServiceTestDB(t)
	svc := NewProjectService(d, nil, nil)
	const proj = "proj_doc_rev"
	seedClaudeDocProject(t, svc, proj)

	// Nothing to snapshot yet.
	rev, err := svc.SnapshotClaudeDoc(proj, model.ProjectClaudeDocRevisionReasonRegenerate)
	if err != nil {
		t.Fatalf("snapshot (empty): %v", err)
	}
	if rev != nil {
		t.Fatalf("snapshot of an absent document = %+v, want nil", rev)
	}

	// The user writes something by hand…
	if _, err := svc.UpsertClaudeDoc(proj, "# 人工约定", model.ProjectClaudeDocSourceManual, false); err != nil {
		t.Fatalf("upsert (manual): %v", err)
	}
	// …and the AI replaces it.
	rev, err = svc.SnapshotClaudeDoc(proj, model.ProjectClaudeDocRevisionReasonRegenerate)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if rev == nil {
		t.Fatal("snapshot returned nil for an existing document")
	}
	if rev.Content != "# 人工约定" {
		t.Errorf("snapshot content = %q, want the hand-edited body", rev.Content)
	}
	if rev.Source != model.ProjectClaudeDocSourceManual {
		t.Errorf("snapshot source = %q, want %q", rev.Source, model.ProjectClaudeDocSourceManual)
	}
	if _, err := svc.UpsertClaudeDoc(proj, "# AI 约定", model.ProjectClaudeDocSourceGenerated, true); err != nil {
		t.Fatalf("upsert (generated): %v", err)
	}

	// The history lists the replaced version.
	revs, err := svc.ListClaudeDocRevisions(proj)
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revs) != 1 || revs[0].ID != rev.ID {
		t.Fatalf("revisions = %+v, want exactly the snapshot %s", revs, rev.ID)
	}

	// Restoring brings the hand-edited body back — and the version it
	// replaced is snapshotted on the way, so the restore is undoable.
	restored, err := svc.RestoreClaudeDocRevision(proj, rev.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Content != "# 人工约定" {
		t.Errorf("restored content = %q", restored.Content)
	}
	if restored.ManualEditedAt == nil {
		t.Error("restored document is not marked as hand-edited — the next regeneration would overwrite it silently")
	}
	revs, err = svc.ListClaudeDocRevisions(proj)
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("revisions = %d, want 2 (the restore snapshots what it replaced)", len(revs))
	}

	// An unknown revision is a not-found, not a silent no-op.
	if _, err := svc.RestoreClaudeDocRevision(proj, "pcdr_nope"); err != sql.ErrNoRows {
		t.Errorf("restore of an unknown revision: err = %v, want sql.ErrNoRows", err)
	}
}
