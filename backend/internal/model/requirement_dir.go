package model

import "time"

// RequirementDir is one directory the user has authorized Claude to read
// for a requirement. It complements Attachment (uploaded files): an
// attachment is copied into NovaWorkbench's own store and handed to Claude
// as a single file, whereas a requirement dir stays where it is on disk and
// is granted to the claude session via `--add-dir <path>`.
//
// The grant is per requirement, not per session: the analyst / architect /
// developer stages of one requirement all see the same set, mirroring how
// requirement_attachments is shared across the three stages.
//
// Path is the NORMALIZED absolute path (see service.NormalizeDirPath for the
// rules: `~` expansion, abs + clean). Symlinks are not resolved, so the
// stored path stays the one the user recognizes, and the same string is
// handed to the claude CLI and shown in the UI — there is no second,
// ambiguous form of the same directory.
type RequirementDir struct {
	ID            string    `json:"id"`
	RequirementID string    `json:"requirement_id"`
	Path          string    `json:"path"`
	Label         string    `json:"label"` // user-written note, optional
	AddedBy       string    `json:"added_by"`
	AddedAt       time.Time `json:"added_at"`
}
