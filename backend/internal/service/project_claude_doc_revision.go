package service

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// ── Project-level CLAUDE.md · version history ─────────────────────────────
//
// The invariant: project_claude_docs holds the CURRENT document and every
// existing read path (the injection helper, the panel) stays untouched;
// this table holds every version that was ever current.
//
// It exists because the destructive replaces are real: an AI regeneration
// and a disk import both REPLACE the document wholesale. Without a history,
// "I added three hard constraints by hand and then hit 重新生成" ends with
// the constraints gone and no way back. The snapshot is taken BEFORE the
// replace, so the outgoing version is always recoverable.
//
// The same shape as code_review_revisions / design_revisions, with owner =
// project_id.

// claudeDocRevisionKeep is how many versions are kept per project. A
// document is a few KB, so 20 versions is a bounded, negligible amount of
// storage — and enough to cover "I only just added that constraint".
const claudeDocRevisionKeep = 20

// SnapshotClaudeDoc freezes the project's current CLAUDE.md as a version.
//
// It is called right BEFORE a destructive replace, so the snapshot holds
// exactly the content that is about to be lost. Returns (nil, nil) when
// there is nothing worth keeping (no document, or an empty one) — an empty
// version would only show up in the history as an entry that renders
// nothing.
func (s *ProjectService) SnapshotClaudeDoc(projectID, reason string) (*model.ProjectClaudeDocRevision, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, nil
	}
	cur, err := s.ClaudeDoc(projectID)
	if err != nil {
		return nil, err
	}
	if cur == nil || strings.TrimSpace(cur.Content) == "" {
		return nil, nil
	}
	rev := &model.ProjectClaudeDocRevision{
		ID:        util.NewID("pcdr"),
		ProjectID: projectID,
		Content:   cur.Content,
		Source:    cur.Source,
		Reason:    reason,
		Bytes:     len(cur.Content),
		CreatedAt: time.Now().UTC(),
	}
	if _, err := s.db.Exec(
		`INSERT INTO project_claude_doc_revisions (id, project_id, md_content, source, reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		rev.ID, rev.ProjectID, rev.Content, rev.Source, rev.Reason, rev.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert project_claude_doc_revision: %w", err)
	}
	if err := s.pruneClaudeDocRevisions(projectID); err != nil {
		// A failed prune must not fail the snapshot — the version IS saved,
		// and an over-long history is a cosmetic problem.
		return rev, nil
	}
	return rev, nil
}

// pruneClaudeDocRevisions drops everything beyond the newest
// claudeDocRevisionKeep versions of a project.
//
// The rows to delete are read first and then removed one by one: a
// "DELETE … WHERE id NOT IN (SELECT … LIMIT n)" statement is not portable
// (MySQL rejects LIMIT inside an IN subquery), and the list is at most a
// couple of dozen rows.
func (s *ProjectService) pruneClaudeDocRevisions(projectID string) error {
	rows, err := s.db.Query(
		`SELECT id FROM project_claude_doc_revisions WHERE project_id = ?
		 ORDER BY created_at DESC, id DESC`, projectID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(ids) <= claudeDocRevisionKeep {
		return nil
	}
	for _, id := range ids[claudeDocRevisionKeep:] {
		if _, err := s.db.Exec(`DELETE FROM project_claude_doc_revisions WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ListClaudeDocRevisions returns a project's versions, newest-first — the
// order the history list renders them in.
func (s *ProjectService) ListClaudeDocRevisions(projectID string) ([]model.ProjectClaudeDocRevision, error) {
	if strings.TrimSpace(projectID) == "" {
		return []model.ProjectClaudeDocRevision{}, nil
	}
	rows, err := s.db.Query(
		`SELECT id, project_id, md_content, source, reason, created_at
		 FROM project_claude_doc_revisions WHERE project_id = ?
		 ORDER BY created_at DESC, id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ProjectClaudeDocRevision{}
	for rows.Next() {
		var rev model.ProjectClaudeDocRevision
		var createdAt sql.NullTime
		if err := rows.Scan(&rev.ID, &rev.ProjectID, &rev.Content, &rev.Source,
			&rev.Reason, &createdAt); err != nil {
			return nil, err
		}
		if createdAt.Valid {
			rev.CreatedAt = createdAt.Time
		}
		rev.Bytes = len(rev.Content)
		out = append(out, rev)
	}
	return out, rows.Err()
}

// RestoreClaudeDocRevision puts a frozen version back as the current
// document.
//
// The current content is snapshotted first (reason=restore), so a restore
// is itself undoable — the user can go back to the version they just
// replaced.
//
// The restored content keeps the source it was created with, but is marked
// as hand-edited: the user put it back on purpose, so the next destructive
// replace has to ask again instead of silently overwriting it.
func (s *ProjectService) RestoreClaudeDocRevision(projectID, revID string) (*model.ProjectClaudeDoc, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(revID) == "" {
		return nil, sql.ErrNoRows
	}
	var rev model.ProjectClaudeDocRevision
	var createdAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, project_id, md_content, source, reason, created_at
		 FROM project_claude_doc_revisions WHERE id = ? AND project_id = ?`, revID, projectID).
		Scan(&rev.ID, &rev.ProjectID, &rev.Content, &rev.Source, &rev.Reason, &createdAt)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(rev.Content) == "" {
		return nil, fmt.Errorf("revision %s has no content", revID)
	}
	if _, err := s.SnapshotClaudeDoc(projectID, model.ProjectClaudeDocRevisionReasonRestore); err != nil {
		return nil, err
	}
	source := rev.Source
	if source == "" {
		source = model.ProjectClaudeDocSourceManual
	}
	return s.upsertClaudeDoc(projectID, rev.Content, source, false, true)
}

// DeleteClaudeDocRevisions drops a project's whole version history. Called
// by the project purge for the same reason the follow-up rows are dropped
// explicitly there: SQLite never enables PRAGMA foreign_keys, so the DDL's
// ON DELETE CASCADE does not fire on the default driver.
func (s *ProjectService) DeleteClaudeDocRevisions(projectID string) error {
	if strings.TrimSpace(projectID) == "" {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM project_claude_doc_revisions WHERE project_id = ?`, projectID)
	return err
}
