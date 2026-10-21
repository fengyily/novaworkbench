package service

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// DesignRevisionService keeps the version history of a requirement's
// design plan (design_revisions).
//
// The invariant: requirements.design_docs holds the CURRENT version and
// nothing in the existing read paths changed; this table holds every
// version that was ever current, so 「重新生成技术方案」 is non-destructive.
// Without it, a regenerate would be a gamble — the user trades a plan
// they have already read for one they haven't seen yet, with no way back.
//
// The architect-design stage ALWAYS overwrites design_docs on success.
// Snapshot is what gives the user the safety net: it freezes the
// about-to-be-replaced version one rev_no back so a restore later can
// bring it back unchanged.
//
// rev_no is 1-based PER REQUIREMENT and assigned by Snapshot as max+1,
// guarded by the (requirement_id, rev_no) unique index. Two concurrent
// snapshots of the same requirement would collide there rather than
// silently producing two "vN"s, but in practice they can't — a regenerate
// refuses to start while a DesignJobID / ApplyJobID is alive (see
// wizard_architect.go's 409 guards), and the user can only invoke
// "重新生成" from a non-running detail page.
//
// Raw SQL with `?` placeholders through the *db.DB wrapper, per the
// repo-wide service convention (the wrapper rebinds to $N on Postgres).
type DesignRevisionService struct {
	db *db.DB
}

func NewDesignRevisionService(database *db.DB) *DesignRevisionService {
	return &DesignRevisionService{db: database}
}

const designRevisionCols = `id, requirement_id, rev_no, trigger_kind, design_docs,
	design_base_sha, model, claude_config_id, created_at`

// Snapshot freezes req's current design plan as the next version and
// stamps that version number back onto requirements.design_revision_no.
//
// Called from the regenerate entry point (wizard_architect.go
// prepareArchitectDesign, BEFORE the new run starts), so "what the user
// is looking at" and "the newest revision row" never disagree. The new
// run still produces v(N+1) on success; the frozen row stays vN until
// they switch back via RestoreAsCurrent.
//
// An empty design_docs is NOT snapshotted: an empty version would show
// up as a selectable "vN" that renders nothing — useless dead stock in
// the version dropdown. Callers can therefore call this unconditionally
// on regenerate paths; it returns (nil, nil) when there is nothing
// worth keeping.
//
// Trigger is the model.RevisionTrigger* constant identifying what
// produced the version we're freezing. Pass "" to default to "initial"
// (kept symmetric with code_review_revision.Snapshot so UI badges stay
// uniform).
func (s *DesignRevisionService) Snapshot(req *model.Requirement, trigger string) (*model.DesignRevision, error) {
	if req == nil {
		return nil, errors.New("requirement is required")
	}
	if strings.TrimSpace(req.DesignDocs) == "" {
		return nil, nil
	}
	if trigger == "" {
		trigger = model.RevisionTriggerInitial
	}
	next, err := s.nextRevNo(req.ID)
	if err != nil {
		return nil, err
	}
	rev := &model.DesignRevision{
		ID:            util.NewID("dr"),
		RequirementID: req.ID,
		RevNo:         next,
		Trigger:       trigger,
		DesignDocs:    req.DesignDocs,
		DesignBaseSHA: req.DesignBaseSHA,
		Model:         req.ArchitectModel,
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := s.db.Exec(`INSERT INTO design_revisions (
		id, requirement_id, rev_no, trigger_kind, design_docs,
		design_base_sha, model, claude_config_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rev.ID, rev.RequirementID, rev.RevNo, rev.Trigger, rev.DesignDocs,
		rev.DesignBaseSHA, rev.Model, "", rev.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert design_revision: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE requirements SET design_revision_no = ? WHERE id = ?`,
		rev.RevNo, rev.RequirementID); err != nil {
		return nil, fmt.Errorf("stamp design_revision_no: %w", err)
	}
	req.DesignRevisionNo = rev.RevNo
	return rev, nil
}

// EnsureSnapshot guarantees that whatever plan the requirement currently
// serves also exists as a frozen version, and is a no-op when it
// already does.
//
// It exists for one case that would otherwise silently lose a plan: a
// requirement designed BEFORE this table shipped. Such a row has a
// perfectly good design_docs and (via the ALTER's default)
// design_revision_no = 0, but no revision row behind it — so a
// regenerate would replace a plan with no v1 to go back to. Called from
// the regenerate path, which must not start until the current version
// is safe.
//
// The check is "is there a row at the requirement's current
// design_revision_no", not a bare count, so it also repairs a row that
// drifted out of sync for any other reason.
func (s *DesignRevisionService) EnsureSnapshot(req *model.Requirement) error {
	if req == nil || strings.TrimSpace(req.DesignDocs) == "" {
		return nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM design_revisions WHERE requirement_id = ? AND rev_no = ?`,
		req.ID, req.DesignRevisionNo).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.Snapshot(req, model.RevisionTriggerInitial)
	return err
}

// nextRevNo returns max(rev_no)+1 for a requirement, 1 when it has no
// revisions. COALESCE keeps the empty case a single round trip and
// avoids a NULL scan.
func (s *DesignRevisionService) nextRevNo(reqID string) (int, error) {
	var maxNo int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(rev_no), 0) FROM design_revisions WHERE requirement_id = ?`,
		reqID).Scan(&maxNo)
	if err != nil {
		return 0, err
	}
	return maxNo + 1, nil
}

// ListByRequirement returns a requirement's versions oldest-first — the
// order the version switcher renders them in (v1 v2 v3 …).
func (s *DesignRevisionService) ListByRequirement(reqID string) ([]model.DesignRevision, error) {
	rows, err := s.db.Query(`SELECT `+designRevisionCols+`
		FROM design_revisions WHERE requirement_id = ?
		ORDER BY rev_no ASC`, reqID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.DesignRevision{}
	for rows.Next() {
		rev, err := scanDesignRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rev)
	}
	return out, rows.Err()
}

// Get returns one version by its per-requirement number. sql.ErrNoRows
// when the requirement has no such version — handlers map it to 404.
func (s *DesignRevisionService) Get(reqID string, revNo int) (*model.DesignRevision, error) {
	row := s.db.QueryRow(`SELECT `+designRevisionCols+`
		FROM design_revisions WHERE requirement_id = ? AND rev_no = ?`, reqID, revNo)
	return scanDesignRevision(row)
}

// RestoreAsCurrent copies an older version back onto requirements,
// making it the plan every other read path serves.
//
// Nothing is deleted and no new revision row is written: the version
// list is a record of what was PRODUCED, not of which one is selected.
// Restoring v1 after a disappointing v2 therefore leaves both on the
// list, and restoring v2 again afterwards is just as cheap.
//
// design_session_id is deliberately untouched — the session belongs to
// the last RUN, not to the selected version; the doc-refine chat should
// keep resuming it after a restore, so the user can keep refining the
// now-restored plan from where they left off.
//
// design_base_sha IS restored (it captures the repository state at the
// time the plan was authored — a v1 written against an older base may
// have stale references the user wants to evaluate alongside the plan).
func (s *DesignRevisionService) RestoreAsCurrent(reqID string, revNo int) error {
	rev, err := s.Get(reqID, revNo)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE requirements
		SET design_docs = ?, design_base_sha = ?, design_revision_no = ?, updated_at = ?
		WHERE id = ?`,
		rev.DesignDocs, rev.DesignBaseSHA, rev.RevNo, time.Now().UTC(), reqID)
	return err
}

// DeleteByRequirement drops a requirement's whole version history.
// Called by RequirementService.Delete for the same reason the
// design_revisions table's ON DELETE CASCADE does not fire on the
// default SQLite driver (SQLite never enables PRAGMA foreign_keys).
func (s *DesignRevisionService) DeleteByRequirement(reqID string) error {
	_, err := s.db.Exec(`DELETE FROM design_revisions WHERE requirement_id = ?`, reqID)
	return err
}

func scanDesignRevision(row rowScanner) (*model.DesignRevision, error) {
	var rev model.DesignRevision
	var createdAt sql.NullTime
	if err := row.Scan(&rev.ID, &rev.RequirementID, &rev.RevNo, &rev.Trigger,
		&rev.DesignDocs, &rev.DesignBaseSHA, &rev.Model, &rev.ClaudeConfigID,
		&createdAt); err != nil {
		return nil, err
	}
	if createdAt.Valid {
		rev.CreatedAt = createdAt.Time
	}
	return &rev, nil
}
