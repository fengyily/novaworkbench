// Package service — RequirementDirService handles the directories a user
// has authorized Claude to read for one requirement.
//
// This is the directory-shaped sibling of AttachmentService. An attachment is
// a file the user uploaded: the bytes are copied into NovaWorkbench's own
// store (~/.novaworkbench/attachments/<req_id>/) and Claude is handed a
// path to that copy. A requirement dir is a directory that stays exactly
// where it is on disk; the grant is passed to the claude session as
// `--add-dir <path>` so the session can read it directly.
//
// Why the grant exists: the architect stage runs claude in PLAN mode
// (`--permission-mode plan`), where a read of a path outside the session's
// working directory needs a permission decision. In a non-interactive
// (`-p`) run there is nobody to make that decision, so the run can stall or
// fail. `--add-dir` grants the directory up front, so the read is
// pre-approved and the plan-mode exploration keeps moving.
//
// The grant is per REQUIREMENT, not per session — the analyst / architect /
// developer stages of one requirement share a single set, exactly like
// requirement_attachments.
package service

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// MaxRequirementDirLabel caps the user-written note next to a granted
// directory. The path itself is bounded by the OS; only the free-text note
// needs a guard.
const MaxRequirementDirLabel = 200

// RequirementDirService stores the granted directories for requirements.
type RequirementDirService struct {
	db *db.DB
}

// NewRequirementDirService builds the store.
func NewRequirementDirService(d *db.DB) *RequirementDirService {
	return &RequirementDirService{db: d}
}

// ErrNotADirectory is returned when the granted path exists but is not a
// directory (a file, a device, a socket). The handler maps it to a 400 with
// a message that says what to do instead.
var ErrNotADirectory = errors.New("path is not a directory")

// ErrPathNotFound is returned when the granted path does not exist.
var ErrPathNotFound = errors.New("path does not exist")

// ErrRootPath is returned when the granted path is the filesystem root.
// Granting "/" is never what the user meant (they are usually typing "/" as
// a prefix), and it would hand the session the entire filesystem.
var ErrRootPath = errors.New("refusing to grant the filesystem root")

// NormalizeDirPath turns user input into the canonical absolute path that
// gets stored and handed to the claude CLI.
//
// The rules, in order:
//
//  1. Trim surrounding whitespace.
//  2. Expand a leading "~" (alone or as "~/…") to the user's home dir.
//  3. Make the path absolute against the server's working directory and
//     clean it (".", "..", duplicate separators).
//  4. Reject paths containing NUL / control characters.
//  5. Reject the filesystem root.
//
// Symlinks are deliberately NOT resolved: the stored path stays the one the
// user recognizes (so the UI shows something they typed), and repointing a
// symlink later moves the grant with it instead of pinning a stale target.
// The existence / directory checks below DO follow symlinks, so a symlink to
// a directory is a valid grant.
func NormalizeDirPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", errors.New("path required")
	}
	for _, r := range p {
		if r == 0 || r < 32 || r == 127 {
			return "", errors.New("path contains control characters")
		}
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if p == "~" {
			p = home
		} else {
			p = filepath.Join(home, p[2:])
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return "", ErrRootPath
	}
	return abs, nil
}

// validateDirPath normalizes the path and checks that it is an existing
// directory. Returns the normalized path on success.
func validateDirPath(raw string) (string, error) {
	abs, err := NormalizeDirPath(raw)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return abs, ErrPathNotFound
		}
		return abs, err
	}
	if !info.IsDir() {
		return abs, ErrNotADirectory
	}
	return abs, nil
}

// List returns every directory granted to a requirement, oldest first.
func (s *RequirementDirService) List(reqID string) ([]model.RequirementDir, error) {
	rows, err := s.db.Query(`
		SELECT id, requirement_id, path, label, added_by, added_at
		FROM requirement_dirs
		WHERE requirement_id = ?
		ORDER BY added_at ASC, id ASC
	`, reqID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RequirementDir{}
	for rows.Next() {
		var d model.RequirementDir
		var addedAt time.Time
		if err := rows.Scan(&d.ID, &d.RequirementID, &d.Path, &d.Label, &d.AddedBy, &addedAt); err != nil {
			return nil, err
		}
		d.AddedAt = addedAt
		out = append(out, d)
	}
	return out, rows.Err()
}

// Paths returns just the granted paths — what the claude CLI needs as
// `--add-dir` arguments. Returns nil (not an error) when the requirement has
// no grants or the table is empty, so callers can pass the result through
// unconditionally.
func (s *RequirementDirService) Paths(reqID string) ([]string, error) {
	if strings.TrimSpace(reqID) == "" {
		return nil, nil
	}
	dirs, err := s.List(reqID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if strings.TrimSpace(d.Path) != "" {
			out = append(out, d.Path)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Add grants a directory to a requirement.
//
// The path is normalized and validated (must exist, must be a directory)
// before anything is written. Granting the same directory twice returns the
// existing row — the (requirement_id, path) unique index backs this, and the
// label is refreshed so "just update my note" goes through the same call.
func (s *RequirementDirService) Add(reqID, rawPath, label, addedBy string) (*model.RequirementDir, error) {
	if strings.TrimSpace(reqID) == "" {
		return nil, errors.New("requirement id required")
	}
	abs, err := validateDirPath(rawPath)
	if err != nil {
		return nil, err
	}
	label = strings.TrimSpace(label)
	if len(label) > MaxRequirementDirLabel {
		label = label[:MaxRequirementDirLabel]
	}

	var existing model.RequirementDir
	var addedAt time.Time
	err = s.db.QueryRow(`
		SELECT id, requirement_id, path, label, added_by, added_at
		FROM requirement_dirs
		WHERE requirement_id = ? AND path = ?
	`, reqID, abs).Scan(&existing.ID, &existing.RequirementID, &existing.Path,
		&existing.Label, &existing.AddedBy, &addedAt)
	if err == nil {
		if existing.Label != label {
			if _, uerr := s.db.Exec(
				`UPDATE requirement_dirs SET label = ? WHERE id = ?`, label, existing.ID); uerr != nil {
				return nil, uerr
			}
			existing.Label = label
		}
		existing.AddedAt = addedAt
		return &existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	id := util.NewID("rdir")
	now := time.Now().UTC()
	if _, err := s.db.Exec(`
		INSERT INTO requirement_dirs
		  (id, requirement_id, path, label, added_by, added_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, reqID, abs, label, addedBy, now); err != nil {
		return nil, err
	}
	return &model.RequirementDir{
		ID: id, RequirementID: reqID, Path: abs,
		Label: label, AddedBy: addedBy, AddedAt: now,
	}, nil
}

// Delete revokes a grant. Scoped to the requirement so a stray dir id from
// another requirement 404s instead of revoking someone else's grant.
// Returns nil when the row does not exist (idempotent).
func (s *RequirementDirService) Delete(reqID, dirID string) error {
	res, err := s.db.Exec(
		`DELETE FROM requirement_dirs WHERE id = ? AND requirement_id = ?`, dirID, reqID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish "exists but belongs to another requirement" from
		// "does not exist at all" so the handler can answer 404 either
		// way — both are "not yours".
		return sql.ErrNoRows
	}
	return nil
}

// Exists reports whether a directory that was granted earlier is still
// present on disk. The UI uses it to warn about a grant that went stale
// (the directory was moved or unmounted); the claude run itself simply
// fails to read it, which the CLI tolerates.
func Exists(absPath string) bool {
	info, err := os.Stat(absPath)
	return err == nil && info.IsDir()
}
