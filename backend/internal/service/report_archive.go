package service

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// Source-type constants for report archives. Persisted to knowledge.source_type
// and knowledge.category so the `/knowledge` page can filter and badge by
// these labels without joining any other table. Distinct from the existing
// "requirement" type used by the legacy requirement-level Archive — these
// values cover the two new "report"-shaped archives (主 Agent 汇总报告 and
// 子任务报告) the report-archive handler emits. Defined as plain string
// constants so they line up with what the handler writes and what the
// frontend filter / badge code reads.
const (
	SourceTypeDevReport     = "dev_report"
	SourceTypeSubtaskReport = "subtask_report"
)

// ReportArchiveService persists two flavors of finished-development report
// (主 Agent 汇总报告 + 子任务报告) as knowledge rows so a future requirement's
// analyst / architect / coding stages can read them via buildKnowledgeBlock.
// It is the storage half of the report-archive flow; the LLM distillation
// (Gateway.ExtractReportKnowledge) lives in the handler so the service stays
// a thin SQL layer like its siblings (RequirementService / SubTaskService /
// KnowledgeService).
//
// Idempotency: the (project_id, source_ref, source_type) key is the upsert
// identity. Re-archiving the same report re-runs the LLM distillation, then
// either UPDATEs the existing row or INSERTs a new one in a single
// transaction. knowledge has no UNIQUE constraint on that triple, so the
// SELECT-then-UPDATE/INSERT pattern is intentionally racy in the worst case
// (two parallel archives of the same report can produce two rows); the
// frontend buttons are disabled while busy and the handler runs sync, so
// collisions are vanishingly rare. The duplicate is at worst cosmetic — both
// rows render correctly under the same query.
//
// LLM interaction is intentionally absent: handler-level so this service has
// no dependency on the LLM channel and stays unit-testable with a plain
// *db.DB. The same division keeps RequirementService.Archive's tests
// deterministic.
type ReportArchiveService struct {
	db *db.DB
}

func NewReportArchiveService(d *db.DB) *ReportArchiveService {
	return &ReportArchiveService{db: d}
}

// Upsert writes (or refreshes) a single knowledge row for the given report.
// The transaction body mirrors RequirementService.Archive: SELECT existing
// id, UPDATE if present, INSERT with a fresh kb_ id otherwise. SELECT then
// write happens in the same tx so a concurrent Upsert either sees the row
// after the other commits and UPDATEs it, or sees nothing and INSERTs a new
// row — never both, never a torn write.
//
// is_reviewed is hard-coded to 0 so the row lands in the "待 Review" queue
// for human review (same convention as service/scanner.go which sets 0 for
// AI-extracted knowledge). is_approved=1 lets the row participate in
// ListForRequirement's knowledge injection immediately — that query does
// NOT filter on is_approved, so an approved-but-unreviewed row is fine.
// Reviewers can flip is_approved=0 via the existing knowledge batch-review
// endpoint if they reject the extraction.
func (s *ReportArchiveService) Upsert(projectID, sourceType, sourceRef, title, content string) (*model.Knowledge, error) {
	now := time.Now()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existingID string
	if err := tx.QueryRow(
		"SELECT id FROM knowledge WHERE project_id=? AND source_ref=? AND source_type=?",
		projectID, sourceRef, sourceType,
	).Scan(&existingID); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("query existing report archive: %w", err)
	}

	if existingID != "" {
		if _, err := tx.Exec(
			"UPDATE knowledge SET title=?, content=?, updated_at=? WHERE id=?",
			title, content, now, existingID,
		); err != nil {
			return nil, fmt.Errorf("update report archive: %w", err)
		}
	} else {
		existingID = util.NewID("kb")
		if _, err := tx.Exec(
			"INSERT INTO knowledge (id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at) VALUES (?,?,?,?,?,?,?,0,1,?,?)",
			existingID, projectID, title, content, sourceType, sourceType, sourceRef, now, now,
		); err != nil {
			return nil, fmt.Errorf("insert report archive: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetByID(existingID)
}

// GetByID fetches a single knowledge row. Used internally by Upsert to
// re-SELECT the full row (with created_at / updated_at) after the
// UPDATE/INSERT commit, and exported for completeness so tests / future
// callers can look up an archive by id without going through Upsert.
func (s *ReportArchiveService) GetByID(id string) (*model.Knowledge, error) {
	var k model.Knowledge
	err := s.db.QueryRow(
		"SELECT id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at FROM knowledge WHERE id=?",
		id,
	).Scan(&k.ID, &k.ProjectID, &k.Title, &k.Content, &k.Category, &k.SourceType, &k.SourceRef, &k.IsReviewed, &k.IsApproved, &k.CreatedAt, &k.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// ListForRequirement returns all report archives owned by a requirement, in a
// single round-trip. The map is keyed by source_ref so the frontend can look
// up "has this report been archived?" by the row's natural id (the
// requirement id for dev_report, the sub_task id for subtask_report) without
// knowing about knowledge ids at the UI layer.
//
// Two queries: one for the dev_report row (source_ref=reqID), one for all
// subtask_report rows whose source_ref is one of this requirement's
// sub_tasks. Done as two queries rather than a UNION so the sub-task side
// can stay a clean IN (subquery) — the dev_report side has exactly one row
// at most, so a single-row scan is cheaper than a UNION ALL with a constant
// column. Returns an empty map (not nil) when nothing is archived.
func (s *ReportArchiveService) ListForRequirement(reqID string) (map[string]string, error) {
	out := map[string]string{}

	devRows, err := s.db.Query(
		"SELECT id, source_ref FROM knowledge WHERE source_type=? AND source_ref=?",
		SourceTypeDevReport, reqID,
	)
	if err != nil {
		return nil, fmt.Errorf("query dev_report archives: %w", err)
	}
	for devRows.Next() {
		var id, ref string
		if err := devRows.Scan(&id, &ref); err != nil {
			devRows.Close()
			return nil, err
		}
		out[ref] = id
	}
	if err := devRows.Err(); err != nil {
		devRows.Close()
		return nil, err
	}
	devRows.Close()

	subRows, err := s.db.Query(
		"SELECT id, source_ref FROM knowledge WHERE source_type=? AND source_ref IN (SELECT id FROM sub_tasks WHERE requirement_id=?)",
		SourceTypeSubtaskReport, reqID,
	)
	if err != nil {
		return nil, fmt.Errorf("query subtask_report archives: %w", err)
	}
	defer subRows.Close()
	for subRows.Next() {
		var id, ref string
		if err := subRows.Scan(&id, &ref); err != nil {
			return nil, err
		}
		out[ref] = id
	}
	return out, subRows.Err()
}

// DeleteBySourceRefs removes knowledge rows for the given source_ref list,
// scoped by source_type. Used by:
//   - SubTaskService.DeleteByIDs: sourceType = SourceTypeSubtaskReport, ids =
//     the sub_tasks being deleted. Must run BEFORE the sub_tasks DELETE so
//     the subquery is still resolvable, and so a knowledge-row delete that
//     fails surfaces as an error before the parent row is touched.
//   - RequirementService.Delete: sourceType = SourceTypeDevReport (the
//     requirement's own dev_report) + a separate call for SourceTypeSubtaskReport
//     scoped to the requirement's sub_tasks. Same "before parent DELETE" rule.
//
// A nil/empty refs slice is a no-op (nothing to delete) so callers can pass
// `subIDs` straight through without a guard.
func (s *ReportArchiveService) DeleteBySourceRefs(sourceType string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(refs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(refs)+1)
	args[0] = sourceType
	for i, r := range refs {
		args[i+1] = r
	}
	_, err := s.db.Exec(
		"DELETE FROM knowledge WHERE source_type=? AND source_ref IN ("+placeholders+")",
		args...,
	)
	return err
}
