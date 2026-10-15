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

// CodeReviewRevisionService keeps the version history of a review's report
// (code_review_revisions).
//
// The invariant: code_reviews holds the CURRENT version and nothing in the
// existing read paths changed; this table holds every version that was ever
// current, so 「基于追问对话重新生成」 is non-destructive. Without it a
// regeneration would be a gamble — the user trades a report they have already
// read for one they haven't seen yet, with no way back.
//
// rev_no is 1-based PER REVIEW and assigned by Snapshot as max+1, guarded by
// the (review_id, rev_no) unique index. Two concurrent snapshots of the same
// review would collide there rather than silently producing two "v2"s, but in
// practice they can't: a review is either running or not, and both the run
// path and the regenerate path refuse to start while status is running.
//
// Raw SQL with `?` placeholders through the *db.DB wrapper, per the repo-wide
// service convention (the wrapper rebinds to $N on Postgres).
type CodeReviewRevisionService struct {
	db *db.DB
}

func NewCodeReviewRevisionService(database *db.DB) *CodeReviewRevisionService {
	return &CodeReviewRevisionService{db: database}
}

const revisionCols = `id, review_id, rev_no, trigger_kind, result_md, findings_json,
	p0_count, p1_count, p3_count, goal_met, goal_checks_json,
	followup_count, model, created_at`

// Snapshot freezes rv's current report as the next version and stamps that
// version number back onto code_reviews.revision_no.
//
// Called from the single terminal path of every run (handler.finishReview and
// the regenerate handler), so "what the user is looking at" and "the newest
// revision row" never disagree.
//
// A report-less outcome (a failed run, or a draft) is NOT snapshotted: an
// empty version would show up as a selectable "v2" that renders nothing.
// Callers can therefore call this unconditionally; it returns (nil, nil) when
// there is nothing worth keeping.
func (s *CodeReviewRevisionService) Snapshot(rv *model.CodeReview, trigger string, followupCount int) (*model.CodeReviewRevision, error) {
	if rv == nil {
		return nil, errors.New("review is required")
	}
	if strings.TrimSpace(rv.ResultMD) == "" {
		return nil, nil
	}
	if trigger == "" {
		trigger = model.RevisionTriggerInitial
	}
	next, err := s.nextRevNo(rv.ID)
	if err != nil {
		return nil, err
	}
	rev := &model.CodeReviewRevision{
		ID:             util.NewID("crr"),
		ReviewID:       rv.ID,
		RevNo:          next,
		Trigger:        trigger,
		ResultMD:       rv.ResultMD,
		FindingsJSON:   rv.FindingsJSON,
		P0Count:        rv.P0Count,
		P1Count:        rv.P1Count,
		P3Count:        rv.P3Count,
		GoalMet:        rv.GoalMet,
		GoalChecksJSON: rv.GoalChecksJSON,
		FollowupCount:  followupCount,
		Model:          rv.Model,
		CreatedAt:      time.Now().UTC(),
	}
	if _, err := s.db.Exec(`INSERT INTO code_review_revisions (
		id, review_id, rev_no, trigger_kind, result_md, findings_json,
		p0_count, p1_count, p3_count, goal_met, goal_checks_json,
		followup_count, model, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rev.ID, rev.ReviewID, rev.RevNo, rev.Trigger, rev.ResultMD, rev.FindingsJSON,
		rev.P0Count, rev.P1Count, rev.P3Count, rev.GoalMet, rev.GoalChecksJSON,
		rev.FollowupCount, rev.Model, rev.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert code_review_revision: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE code_reviews SET revision_no = ? WHERE id = ?`,
		rev.RevNo, rev.ReviewID); err != nil {
		return nil, fmt.Errorf("stamp revision_no: %w", err)
	}
	rv.RevisionNo = rev.RevNo
	return rev, nil
}

// EnsureSnapshot guarantees that whatever report the review currently serves
// also exists as a frozen version, and is a no-op when it already does.
//
// It exists for one case that would otherwise silently lose a report: a
// review finished BEFORE this table shipped. Such a row has a perfectly good
// result_md and (via the ALTER's default) revision_no = 1, but no revision
// row behind it — so a regeneration would replace a report with no v1 to go
// back to. Called from the regenerate path, which must not start until the
// current version is safe.
//
// The check is "is there a row at the review's current revision_no", not a
// bare count, so it also repairs a row that drifted out of sync for any
// other reason.
func (s *CodeReviewRevisionService) EnsureSnapshot(rv *model.CodeReview, followupCount int) error {
	if rv == nil || strings.TrimSpace(rv.ResultMD) == "" {
		return nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM code_review_revisions WHERE review_id = ? AND rev_no = ?`,
		rv.ID, rv.RevisionNo).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.Snapshot(rv, model.RevisionTriggerInitial, followupCount)
	return err
}

// nextRevNo returns max(rev_no)+1 for a review, 1 when it has no revisions.
// COALESCE keeps the empty case a single round trip and avoids a NULL scan.
func (s *CodeReviewRevisionService) nextRevNo(reviewID string) (int, error) {
	var maxNo int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(rev_no), 0) FROM code_review_revisions WHERE review_id = ?`,
		reviewID).Scan(&maxNo)
	if err != nil {
		return 0, err
	}
	return maxNo + 1, nil
}

// ListByReview returns a review's versions oldest-first — the order the
// version switcher renders them in (v1 v2 v3 …).
func (s *CodeReviewRevisionService) ListByReview(reviewID string) ([]model.CodeReviewRevision, error) {
	rows, err := s.db.Query(`SELECT `+revisionCols+`
		FROM code_review_revisions WHERE review_id = ?
		ORDER BY rev_no ASC`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CodeReviewRevision{}
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rev)
	}
	return out, rows.Err()
}

// Get returns one version by its per-review number. sql.ErrNoRows when the
// review has no such version — handlers map it to 404.
func (s *CodeReviewRevisionService) Get(reviewID string, revNo int) (*model.CodeReviewRevision, error) {
	row := s.db.QueryRow(`SELECT `+revisionCols+`
		FROM code_review_revisions WHERE review_id = ? AND rev_no = ?`, reviewID, revNo)
	return scanRevision(row)
}

// RestoreAsCurrent copies an older version back onto code_reviews, making it
// the report every other read path serves.
//
// Nothing is deleted and no new revision row is written: the version list is
// a record of what was PRODUCED, not of which one is selected. Restoring v1
// after a disappointing v2 therefore leaves both on the list, and restoring
// v2 again afterwards is just as cheap.
//
// claude_session_id is deliberately untouched — the session belongs to the
// last RUN, not to the selected version, and a 追问 should keep resuming it.
func (s *CodeReviewRevisionService) RestoreAsCurrent(reviewID string, revNo int) error {
	rev, err := s.Get(reviewID, revNo)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = s.db.Exec(`UPDATE code_reviews
		SET result_md = ?, findings_json = ?, p0_count = ?, p1_count = ?, p3_count = ?,
		    goal_met = ?, goal_checks_json = ?, revision_no = ?, updated_at = ?
		WHERE id = ?`,
		rev.ResultMD, rev.FindingsJSON, rev.P0Count, rev.P1Count, rev.P3Count,
		rev.GoalMet, rev.GoalChecksJSON, rev.RevNo, now, reviewID)
	return err
}

// DeleteByReview drops a review's whole version history. Called by
// CodeReviewService.Delete for the same reason the follow-up rows are dropped
// explicitly there: SQLite never enables PRAGMA foreign_keys, so the DDL's
// ON DELETE CASCADE does not fire on the default driver.
func (s *CodeReviewRevisionService) DeleteByReview(reviewID string) error {
	_, err := s.db.Exec(`DELETE FROM code_review_revisions WHERE review_id = ?`, reviewID)
	return err
}

func scanRevision(row rowScanner) (*model.CodeReviewRevision, error) {
	var rev model.CodeReviewRevision
	var createdAt sql.NullTime
	if err := row.Scan(&rev.ID, &rev.ReviewID, &rev.RevNo, &rev.Trigger,
		&rev.ResultMD, &rev.FindingsJSON, &rev.P0Count, &rev.P1Count, &rev.P3Count,
		&rev.GoalMet, &rev.GoalChecksJSON, &rev.FollowupCount, &rev.Model,
		&createdAt); err != nil {
		return nil, err
	}
	if createdAt.Valid {
		rev.CreatedAt = createdAt.Time
	}
	return &rev, nil
}
