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

// RequirementFindingService stores the discoveries a child requirement makes
// while it is being designed or implemented, and the review workflow that
// turns them into changes to the master requirement's plan.
//
// Why this exists: a master requirement is split into children, and each
// child discovers things the master's plan did not anticipate — a conflict
// with a sibling, a constraint the master forgot, a decision only the
// implementer could make. Today that knowledge dies with the child's own
// plan: nothing carries it back to the master, so the master plan keeps
// describing the world as it looked BEFORE the split, and the later children
// keep designing against a stale picture.
//
// The workflow mirrors the rest of the product's stage gates:
//
//	child produces → pending → a human accepts / rejects → accepted
//	                                                 → refine-doc absorbs it
//	                                                   into the master plan
//
// A finding is a *proposal*, never a fact: status decides whether it enters
// the master plan.
type RequirementFindingService struct {
	db *db.DB
}

func NewRequirementFindingService(database *db.DB) *RequirementFindingService {
	return &RequirementFindingService{db: database}
}

// Finding status values.
const (
	// FindingStatusPending — filed, not yet reviewed.
	FindingStatusPending = "pending"
	// FindingStatusAccepted — reviewed and accepted; eligible for absorption.
	FindingStatusAccepted = "accepted"
	// FindingStatusRejected — reviewed and rejected; kept for traceability.
	FindingStatusRejected = "rejected"
)

// Finding severity values.
const (
	FindingSeverityInfo      = "info"
	FindingSeverityConflict  = "conflict"
	FindingSeverityDeviation = "deviation"
	FindingSeverityBlocker   = "blocker"
)

// Finding stage values.
const (
	FindingStageDesign = "design"
	FindingStageCoding = "coding"
)

const requirementFindingCols = `id, requirement_id, parent_id, stage, severity, status,
	title, detail, resolution, source_section, parent_design_hash, absorbed_seq, absorbed_at_rev,
	created_at, updated_at, reviewed_at`

// Record files a finding produced by a child requirement.
//
// Deduplication: a re-run of the architect (or a second coding round) would
// otherwise file the same finding twice and the master's review queue would
// fill with duplicates. Findings are keyed by (requirement_id, stage, title)
// — the title is what the model wrote, so it is the only stable handle we
// have. A same-key finding is UPDATED in place, and its status is reset to
// pending ONLY when the content actually changed; an identical re-file keeps
// the reviewer's verdict.
//
// The parent hash records which version of the master plan the child was
// looking at, so the UI can flag a finding that predates a master-plan
// change.
func (s *RequirementFindingService) Record(f model.RequirementFinding) error {
	if f.RequirementID == "" {
		return errors.New("finding requires a requirement id")
	}
	if strings.TrimSpace(f.Title) == "" {
		return errors.New("finding requires a title")
	}
	now := time.Now()
	if f.ID == "" {
		f.ID = util.NewID("fnd")
	}
	if f.Stage == "" {
		f.Stage = FindingStageDesign
	}
	if f.Severity == "" {
		f.Severity = FindingSeverityInfo
	}
	if f.Status == "" {
		f.Status = FindingStatusPending
	}

	// Same key → update in place, preserving the reviewer's verdict when the
	// content did not change.
	var existingID, existingStatus, existingDetail string
	err := s.db.QueryRow(
		"SELECT id, status, detail FROM requirement_findings WHERE requirement_id = ? AND stage = ? AND title = ?",
		f.RequirementID, f.Stage, f.Title).Scan(&existingID, &existingStatus, &existingDetail)
	switch {
	case err == nil:
		status := existingStatus
		if existingDetail != f.Detail {
			// The content moved on — the old verdict no longer applies.
			status = FindingStatusPending
		}
		_, err = s.db.Exec(
			"UPDATE requirement_findings SET detail=?, severity=?, status=?, source_section=?, "+
				"parent_design_hash=?, updated_at=? WHERE id=?",
			f.Detail, f.Severity, status, f.SourceSection, f.ParentDesignHash, now, existingID)
		return err
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(
			"INSERT INTO requirement_findings ("+requirementFindingCols+") "+
				"VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			f.ID, f.RequirementID, f.ParentID, f.Stage, f.Severity, f.Status, f.Title, f.Detail,
			f.Resolution, f.SourceSection, f.ParentDesignHash, 0, 0, now, now, nil)
		return err
	default:
		return err
	}
}

// ListByParent returns the findings addressed to a master requirement — its
// review queue.
func (s *RequirementFindingService) ListByParent(parentID string) ([]model.RequirementFinding, error) {
	if strings.TrimSpace(parentID) == "" {
		return nil, nil
	}
	return s.query("SELECT "+requirementFindingCols+" FROM requirement_findings WHERE parent_id = ? ORDER BY created_at ASC", parentID)
}

// ListByRequirement returns the findings produced by one requirement.
func (s *RequirementFindingService) ListByRequirement(requirementID string) ([]model.RequirementFinding, error) {
	if strings.TrimSpace(requirementID) == "" {
		return nil, nil
	}
	return s.query("SELECT "+requirementFindingCols+" FROM requirement_findings WHERE requirement_id = ? ORDER BY created_at ASC", requirementID)
}

// ListUnabsorbed returns the accepted findings of a master requirement that
// have not been folded into its plan yet — the checklist the refine prompt
// is built from.
func (s *RequirementFindingService) ListUnabsorbed(parentID string) ([]model.RequirementFinding, error) {
	if strings.TrimSpace(parentID) == "" {
		return nil, nil
	}
	return s.query(
		"SELECT "+requirementFindingCols+" FROM requirement_findings "+
			"WHERE parent_id = ? AND status = ? AND absorbed_seq = 0 ORDER BY created_at ASC",
		parentID, FindingStatusAccepted)
}

// SetStatus records the reviewer's verdict on one finding.
//
// A finding that does not belong to the given requirement is reported as
// ErrFindingNotFound rather than silently updated — the caller checks
// ownership before accepting a verdict.
//
// ownerID is the requirement the reviewer addressed the endpoint with: the
// master that owns the queue, or the child that filed the finding. Both are
// accepted, because the review UI reaches the same row from either side.
func (s *RequirementFindingService) SetStatus(id, status, resolution string, ownerID string) error {
	if status != FindingStatusPending && status != FindingStatusAccepted && status != FindingStatusRejected {
		return fmt.Errorf("invalid finding status %q", status)
	}
	var author, parent string
	err := s.db.QueryRow("SELECT requirement_id, parent_id FROM requirement_findings WHERE id = ?", id).Scan(&author, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFindingNotFound
	}
	if err != nil {
		return err
	}
	if ownerID != "" && ownerID != author && ownerID != parent {
		return ErrFindingNotFound
	}
	now := time.Now()
	_, err = s.db.Exec(
		"UPDATE requirement_findings SET status=?, resolution=?, reviewed_at=?, updated_at=? WHERE id=?",
		status, resolution, now, now, id)
	return err
}

// MarkAbsorbed stamps the accepted findings of a master requirement as folded
// into its plan.
//
// The accounting is deliberately coarse: the model rewrites the whole
// document, so there is no reliable way to attribute which finding it
// adopted. The honest rule is "the user refined the master plan while these
// findings were pending → they were addressed". The refine prompt asks the
// model to record an explicit "暂不采纳 + 理由" for anything it rejects, so
// the outcome is still visible in the plan text.
//
// seq is a monotonically increasing absorption counter — deliberately NOT
// requirements.design_revision_no, which only increments on the regenerate
// path and is therefore not a plan version.
func (s *RequirementFindingService) MarkAbsorbed(parentID string, seq, atRev int) (int64, error) {
	if strings.TrimSpace(parentID) == "" {
		return 0, nil
	}
	res, err := s.db.Exec(
		"UPDATE requirement_findings SET absorbed_seq = ?, absorbed_at_rev = ?, updated_at = ? "+
			"WHERE parent_id = ? AND status = ? AND absorbed_seq = 0",
		seq, atRev, time.Now(), parentID, FindingStatusAccepted)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// NextAbsorbSeq returns the absorption counter to use for the next absorb
// operation on a master requirement.
func (s *RequirementFindingService) NextAbsorbSeq(parentID string) (int, error) {
	var maxSeq sql.NullInt64
	err := s.db.QueryRow(
		"SELECT MAX(absorbed_seq) FROM requirement_findings WHERE parent_id = ?", parentID).Scan(&maxSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return int(maxSeq.Int64) + 1, nil
}

// DeleteByRequirement removes the findings a requirement filed and the ones
// addressed to it.
func (s *RequirementFindingService) DeleteByRequirement(id string) error {
	_, err := s.db.Exec(
		"DELETE FROM requirement_findings WHERE requirement_id = ? OR parent_id = ?", id, id)
	return err
}

func (s *RequirementFindingService) query(sqlText string, args ...any) ([]model.RequirementFinding, error) {
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RequirementFinding{}
	for rows.Next() {
		var f model.RequirementFinding
		var createdAt, updatedAt, reviewedAt sql.NullTime
		if err := rows.Scan(&f.ID, &f.RequirementID, &f.ParentID, &f.Stage, &f.Severity, &f.Status,
			&f.Title, &f.Detail, &f.Resolution, &f.SourceSection, &f.ParentDesignHash,
			&f.AbsorbedSeq, &f.AbsorbedAtRev, &createdAt, &updatedAt, &reviewedAt); err != nil {
			return nil, err
		}
		if createdAt.Valid {
			f.CreatedAt = createdAt.Time
		}
		if updatedAt.Valid {
			f.UpdatedAt = updatedAt.Time
		}
		if reviewedAt.Valid {
			t := reviewedAt.Time
			f.ReviewedAt = &t
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ErrFindingNotFound — the finding does not exist, or does not belong to the
// requirement the caller named.
var ErrFindingNotFound = errors.New("finding not found")
