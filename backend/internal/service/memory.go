package service

import (
	"database/sql"

	"fmt"
	"github.com/novaworkbench/backend/internal/db"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

type MemoryService struct{ db *db.DB }

func NewMemoryService(db *db.DB) *MemoryService { return &MemoryService{db: db} }

func (s *MemoryService) List(projectID string, memType string, tags string, search string, limit int, offset int) ([]model.Memory, int, error) {
	where := "WHERE 1=1"
	args := []interface{}{}

	if projectID != "" {
		where += " AND project_id = ?"
		args = append(args, projectID)
	}
	if memType != "" {
		where += " AND type = ?"
		args = append(args, memType)
	}
	if search != "" {
		// LOWER() keeps the search case-insensitive on PostgreSQL too
		// (SQLite/MySQL LIKE already is, and LOWER is a no-op there).
		where += " AND (LOWER(title) LIKE LOWER(?) OR LOWER(content) LIKE LOWER(?))"
		s := "%" + search + "%"
		args = append(args, s, s)
	}

	var total int
	s.db.QueryRow("SELECT COUNT(*) FROM memories "+where, args...).Scan(&total)

	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.db.Query(
		"SELECT id, project_id, type, title, content, source, file_path, tags, created_at, updated_at, valid_until FROM memories "+where+" ORDER BY updated_at DESC LIMIT ? OFFSET ?",
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var memories []model.Memory
	for rows.Next() {
		var m model.Memory
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Type, &m.Title, &m.Content, &m.Source, &m.FilePath, &m.Tags, &m.CreatedAt, &m.UpdatedAt, &m.ValidUntil); err != nil {
			return nil, 0, err
		}
		memories = append(memories, m)
	}
	if memories == nil {
		memories = []model.Memory{}
	}
	return memories, total, nil
}

func (s *MemoryService) Get(id string) (*model.Memory, error) {
	var m model.Memory
	err := s.db.QueryRow("SELECT id, project_id, type, title, content, source, file_path, tags, created_at, updated_at, valid_until FROM memories WHERE id = ?", id).
		Scan(&m.ID, &m.ProjectID, &m.Type, &m.Title, &m.Content, &m.Source, &m.FilePath, &m.Tags, &m.CreatedAt, &m.UpdatedAt, &m.ValidUntil)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("memory not found")
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *MemoryService) Create(req model.CreateMemoryReq) (*model.Memory, error) {
	id := util.NewID("mem")
	if req.Type == "" {
		req.Type = "business_context"
	}
	if req.Tags == "" {
		req.Tags = "[]"
	}
	now := time.Now()

	var validUntil *time.Time
	if req.ValidUntil != "" {
		t, err := time.Parse(time.RFC3339, req.ValidUntil)
		if err == nil {
			validUntil = &t
		}
	}

	_, err := s.db.Exec(
		"INSERT INTO memories (id, project_id, type, title, content, source, file_path, tags, created_at, updated_at, valid_until) VALUES (?,?,?,?,?,'user_input','',?,?,?,?)",
		id, req.ProjectID, req.Type, req.Title, req.Content, req.Tags, now, now, validUntil)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *MemoryService) Update(id string, req model.CreateMemoryReq) (*model.Memory, error) {
	_, err := s.db.Exec(
		"UPDATE memories SET type=?, title=?, content=?, tags=?, updated_at=? WHERE id=?",
		req.Type, req.Title, req.Content, req.Tags, time.Now(), id)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *MemoryService) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM memories WHERE id = ?", id)
	return err
}
