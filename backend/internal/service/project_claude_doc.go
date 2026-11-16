package service

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// ── Project-level CLAUDE.md ───────────────────────────────────────────────
//
// The project-level CLAUDE.md is a managed asset of the platform: it lives
// in the DB (never in the user's working tree), it is shared by every
// requirement of the project, and it is injected into the AI stages that
// need the project's conventions.
//
// These methods are deliberately plain data access — the RENDERING of the
// prompt block lives in the prompt package and is called by the handler
// layer, so this package keeps its "no prompt dependency" shape.

// ClaudeDoc returns the project-level CLAUDE.md row.
//
// A project without a document is NOT an error: the function returns
// (nil, nil) so the caller can simply skip the injection (and the prompt
// stays byte-for-byte identical to what it was before the feature existed).
func (s *ProjectService) ClaudeDoc(projectID string) (*model.ProjectClaudeDoc, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, nil
	}
	var d model.ProjectClaudeDoc
	var generatedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, project_id, md_content, source, generated_at, updated_at
		 FROM project_claude_docs WHERE project_id = ?`, projectID).
		Scan(&d.ID, &d.ProjectID, &d.Content, &d.Source, &generatedAt, &d.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if generatedAt.Valid {
		t := generatedAt.Time
		d.GeneratedAt = &t
	}
	d.Bytes = len(d.Content)
	return &d, nil
}

// UpsertClaudeDoc writes the project-level CLAUDE.md (one row per project).
//
// markGenerated controls the "last generated" stamp:
//   - true  → the content was just produced by the AI, so generated_at is
//     refreshed to now;
//   - false → a human edited the content, so generated_at keeps its
//     previous value. The badge answers "when was this last machine
//     written", and a manual edit must not fake a fresh generation.
//
// The previous stamp is READ first and then written back as a plain value,
// rather than being referenced from the conflict clause. That keeps the
// statement portable: an unqualified column reference inside
// ON CONFLICT DO UPDATE is ambiguous on PostgreSQL (SQLSTATE 42702), and
// the table-qualified form is not accepted the same way on every dialect.
func (s *ProjectService) UpsertClaudeDoc(projectID, content, source string, markGenerated bool) (*model.ProjectClaudeDoc, error) {
	if strings.TrimSpace(projectID) == "" {
		return nil, sql.ErrNoRows
	}
	now := time.Now()
	var genVal any
	if markGenerated {
		genVal = now
	} else {
		existing, err := s.ClaudeDoc(projectID)
		if err != nil {
			return nil, err
		}
		if existing != nil && existing.GeneratedAt != nil {
			genVal = *existing.GeneratedAt
		}
	}
	id := util.NewID("pcd")
	_, err := s.db.Exec(
		`INSERT INTO project_claude_docs (id, project_id, md_content, source, generated_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`+
			s.db.OnConflict(s.db.Ident("project_id"),
				`md_content = ?, source = ?, generated_at = ?, updated_at = ?`),
		id, projectID, content, source, genVal, now,
		content, source, genVal, now)
	if err != nil {
		return nil, err
	}
	return s.ClaudeDoc(projectID)
}

// DeleteClaudeDoc removes the project-level CLAUDE.md. Deleting a document
// that does not exist is a no-op (idempotent), not an error.
func (s *ProjectService) DeleteClaudeDoc(projectID string) error {
	if strings.TrimSpace(projectID) == "" {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM project_claude_docs WHERE project_id = ?`, projectID)
	return err
}

// ReadDiskClaudeDoc reads a CLAUDE.md that already exists on disk.
//
// This is the "从磁盘导入" path: the project ships its own CLAUDE.md and the
// user wants the managed copy to start from it. The file is read as-is —
// the import is a copy into the DB, the file itself is never modified.
func (s *ProjectService) ReadDiskClaudeDoc(projectPath string) (string, bool, error) {
	if strings.TrimSpace(projectPath) == "" {
		return "", false, nil
	}
	path := filepath.Join(projectPath, "CLAUDE.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

// DiskClaudeDocExists reports whether the directory the model will see
// already contains a CLAUDE.md. It is used to disambiguate a project that
// ships its own document: the CLI auto-loads that file, so the injected
// block has to say which of the two wins.
func DiskClaudeDocExists(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, "CLAUDE.md"))
	return err == nil && !st.IsDir()
}
