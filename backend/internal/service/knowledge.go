package service

import (
	"database/sql"

	"fmt"
	"github.com/novaworkbench/backend/internal/db"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

type KnowledgeService struct{ db *db.DB }

func NewKnowledgeService(db *db.DB) *KnowledgeService { return &KnowledgeService{db: db} }

func (s *KnowledgeService) List(projectID string, category string, sourceType string, search string, limit int, offset int) ([]model.Knowledge, int, error) {
	where := "WHERE 1=1"
	args := []interface{}{}

	if projectID != "" {
		where += " AND project_id = ?"
		args = append(args, projectID)
	}
	if category != "" {
		where += " AND category = ?"
		args = append(args, category)
	}
	if sourceType != "" {
		where += " AND source_type = ?"
		args = append(args, sourceType)
	}
	if search != "" {
		// LOWER() keeps the search case-insensitive on PostgreSQL too
		// (SQLite/MySQL LIKE already is, and LOWER is a no-op there).
		where += " AND (LOWER(title) LIKE LOWER(?) OR LOWER(content) LIKE LOWER(?))"
		s := "%" + search + "%"
		args = append(args, s, s)
	}

	var total int
	s.db.QueryRow("SELECT COUNT(*) FROM knowledge "+where, args...).Scan(&total)

	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.db.Query(
		"SELECT id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at FROM knowledge "+where+" ORDER BY updated_at DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var items []model.Knowledge
	for rows.Next() {
		var k model.Knowledge
		if err := rows.Scan(&k.ID, &k.ProjectID, &k.Title, &k.Content, &k.Category, &k.SourceType, &k.SourceRef, &k.IsReviewed, &k.IsApproved, &k.CreatedAt, &k.UpdatedAt); err != nil {
			return nil, 0, err
		}
		items = append(items, k)
	}
	if items == nil {
		items = []model.Knowledge{}
	}
	return items, total, nil
}

// ListForRequirement returns the knowledge entries most relevant to a
// requirement for the wizard's optional "读取项目知识库" step. It first matches
// entries whose title/content contain any term of the requirement title
// (case-insensitive); when fewer than limit match, it tops up with the
// project's core architecture documents (CLAUDE.md / AGENTS.md / Project
// Structure — category='architecture') so the loaded set is never empty once
// the project has been scanned. Results are ordered by updated_at DESC.
func (s *KnowledgeService) ListForRequirement(projectID, requirementTitle string, limit int) ([]model.Knowledge, error) {
	if limit <= 0 {
		limit = 20
	}
	const baseCols = "SELECT id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at FROM knowledge"

	items := []model.Knowledge{}
	seen := map[string]bool{}

	// 1. Term match against the requirement title.
	terms := splitRequirementTerms(requirementTitle)
	if len(terms) > 0 {
		clauses := make([]string, 0, len(terms))
		args := []interface{}{projectID}
		for _, t := range terms {
			clauses = append(clauses, "(LOWER(title) LIKE LOWER(?) OR LOWER(content) LIKE LOWER(?))")
			sq := "%" + t + "%"
			args = append(args, sq, sq)
		}
		rows, err := s.db.Query(baseCols+" WHERE project_id = ? AND ("+strings.Join(clauses, " OR ")+") ORDER BY updated_at DESC LIMIT ?", append(args, limit)...)
		if err != nil {
			return nil, err
		}
		matched, err := scanKnowledgeRows(rows)
		if err != nil {
			return nil, err
		}
		for _, k := range matched {
			items = append(items, k)
			seen[k.ID] = true
		}
	}

	// 2. Top up with core architecture documents when the term search didn't
	// fill the requested count (covers empty titles and non-matching ones).
	if len(items) < limit {
		clauses := []string{"project_id = ?", "category = ?"}
		args := []interface{}{projectID, "architecture"}
		for id := range seen {
			clauses = append(clauses, "id <> ?")
			args = append(args, id)
		}
		rows, err := s.db.Query(baseCols+" WHERE "+strings.Join(clauses, " AND ")+" ORDER BY updated_at DESC LIMIT ?", append(args, limit-len(items))...)
		if err != nil {
			return nil, err
		}
		fill, err := scanKnowledgeRows(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, fill...)
	}
	return items, nil
}

// splitRequirementTerms tokenizes a requirement title into search terms. ASCII
// words pass through whole (length >= 2); CJK runs are additionally split into
// consecutive bigrams so a Chinese title like "登录需求" can match knowledge
// content mentioning "登录" — without this, an all-Chinese title collapses into
// one undividable term and the keyword search would almost never hit.
func splitRequirementTerms(title string) []string {
	fields := strings.FieldsFunc(title, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == '/' || r == ',' ||
			r == '：' || r == ':' || r == '、' || r == '(' || r == ')' || r == '（' || r == '）'
	})
	seen := map[string]bool{}
	var terms []string
	add := func(s string) {
		if len(s) < 2 || seen[s] {
			return
		}
		seen[s] = true
		terms = append(terms, s)
	}
	for _, f := range fields {
		runes := []rune(f)
		hasCJK := false
		for _, r := range runes {
			if r >= 0x4E00 && r <= 0x9FFF {
				hasCJK = true
				break
			}
		}
		if !hasCJK {
			add(f)
			continue
		}
		add(string(runes))
		for i := 0; i+1 < len(runes); i++ {
			add(string(runes[i : i+2]))
		}
	}
	return terms
}

// scanKnowledgeRows scans a *sql.Rows result into a Knowledge slice, closing
// the rows. Shared by ListForRequirement (the term and top-up queries).
func scanKnowledgeRows(rows *sql.Rows) ([]model.Knowledge, error) {
	defer rows.Close()
	var items []model.Knowledge
	for rows.Next() {
		var k model.Knowledge
		if err := rows.Scan(&k.ID, &k.ProjectID, &k.Title, &k.Content, &k.Category, &k.SourceType, &k.SourceRef, &k.IsReviewed, &k.IsApproved, &k.CreatedAt, &k.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, k)
	}
	if items == nil {
		items = []model.Knowledge{}
	}
	return items, nil
}

func (s *KnowledgeService) Get(id string) (*model.Knowledge, error) {
	var k model.Knowledge
	err := s.db.QueryRow("SELECT id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at FROM knowledge WHERE id = ?", id).
		Scan(&k.ID, &k.ProjectID, &k.Title, &k.Content, &k.Category, &k.SourceType, &k.SourceRef, &k.IsReviewed, &k.IsApproved, &k.CreatedAt, &k.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("knowledge not found")
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (s *KnowledgeService) Create(req model.CreateKnowledgeReq) (*model.Knowledge, error) {
	id := util.NewID("kb")
	if req.SourceType == "" {
		req.SourceType = "user_defined"
	}
	now := time.Now()

	_, err := s.db.Exec(
		"INSERT INTO knowledge (id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at) VALUES (?,?,?,?,?,?,?,0,1,?,?)",
		id, req.ProjectID, req.Title, req.Content, req.Category, req.SourceType, req.SourceRef, now, now)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *KnowledgeService) Update(id string, req model.CreateKnowledgeReq) (*model.Knowledge, error) {
	_, err := s.db.Exec(
		"UPDATE knowledge SET title=?, content=?, category=?, source_type=?, source_ref=?, updated_at=? WHERE id=?",
		req.Title, req.Content, req.Category, req.SourceType, req.SourceRef, time.Now(), id)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *KnowledgeService) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM knowledge WHERE id = ?", id)
	return err
}

func (s *KnowledgeService) ListForReview(projectID string) ([]model.Knowledge, error) {
	query := "SELECT id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at FROM knowledge WHERE is_reviewed = 0"
	args := []interface{}{}
	if projectID != "" {
		query += " AND project_id = ?"
		args = append(args, projectID)
	}
	query += " ORDER BY created_at ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []model.Knowledge
	for rows.Next() {
		var k model.Knowledge
		if err := rows.Scan(&k.ID, &k.ProjectID, &k.Title, &k.Content, &k.Category, &k.SourceType, &k.SourceRef, &k.IsReviewed, &k.IsApproved, &k.CreatedAt, &k.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, k)
	}
	if items == nil {
		items = []model.Knowledge{}
	}
	return items, nil
}

func (s *KnowledgeService) BatchReview(req model.ReviewActionReq) error {
	for _, id := range req.IDs {
		switch req.Action {
		case "approve":
			s.db.Exec("UPDATE knowledge SET is_reviewed=1, is_approved=1, updated_at=? WHERE id=?", time.Now(), id)
		case "reject":
			s.db.Exec("UPDATE knowledge SET is_reviewed=1, is_approved=0, updated_at=? WHERE id=?", time.Now(), id)
		case "skip":
			// keep is_reviewed = 0
		case "edit":
			title := req.EditedTitle
			content := req.EditedContent
			if title != "" || content != "" {
				s.db.Exec("UPDATE knowledge SET is_reviewed=1, is_approved=1, updated_at=?"+buildEditSet(title, content)+" WHERE id=?", append([]interface{}{time.Now()}, id))
			}
		}
	}
	return nil
}

func buildEditSet(title, content string) string {
	parts := []string{}
	if title != "" {
		parts = append(parts, ", title='"+strings.ReplaceAll(title, "'", "''")+"'")
	}
	if content != "" {
		parts = append(parts, ", content='"+strings.ReplaceAll(content, "'", "''")+"'")
	}
	return strings.Join(parts, "")
}
