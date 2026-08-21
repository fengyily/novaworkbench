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

type SkillService struct{ db *db.DB }

func NewSkillService(db *db.DB) *SkillService { return &SkillService{db: db} }

const skillColumns = "id, name, slug, content, description, enabled, source_url, created_at, updated_at"

func scanSkill(s interface{ Scan(...any) error }, r *model.Skill) error {
	return s.Scan(&r.ID, &r.Name, &r.Slug, &r.Content, &r.Description, &r.Enabled, &r.SourceURL, &r.CreatedAt, &r.UpdatedAt)
}

func (s *SkillService) List() ([]model.Skill, error) {
	rows, err := s.db.Query("SELECT " + skillColumns + " FROM skills ORDER BY name ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var skills []model.Skill
	for rows.Next() {
		var sk model.Skill
		if err := scanSkill(rows, &sk); err != nil {
			return nil, err
		}
		skills = append(skills, sk)
	}
	if skills == nil {
		skills = []model.Skill{}
	}
	return skills, nil
}

func (s *SkillService) Get(id string) (*model.Skill, error) {
	var sk model.Skill
	err := scanSkill(s.db.QueryRow("SELECT "+skillColumns+" FROM skills WHERE id = ?", id), &sk)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("skill not found")
	}
	if err != nil {
		return nil, err
	}
	return &sk, nil
}

func (s *SkillService) Create(req model.CreateSkillReq) (*model.Skill, error) {
	id := util.NewID("skill")
	now := time.Now()
	if _, err := s.db.Exec(
		"INSERT INTO skills (id, name, slug, content, description, enabled, source_url, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)",
		id, req.Name, req.Slug, req.Content, req.Description, true, req.SourceURL, now, now,
	); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *SkillService) Update(id string, req model.UpdateSkillReq) (*model.Skill, error) {
	if _, err := s.db.Exec(
		"UPDATE skills SET name=?, slug=?, content=?, description=?, enabled=?, updated_at=? WHERE id=?",
		req.Name, req.Slug, req.Content, req.Description, req.Enabled, time.Now(), id,
	); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *SkillService) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM skills WHERE id = ?", id)
	return err
}

// EnabledSkills returns slug + content pairs for all enabled skills.
func (s *SkillService) EnabledSkills() ([]struct{ Slug, Content string }, error) {
	rows, err := s.db.Query("SELECT slug, content FROM skills WHERE enabled = 1 ORDER BY name ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ Slug, Content string }
	for rows.Next() {
		var e struct{ Slug, Content string }
		if err := rows.Scan(&e.Slug, &e.Content); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// SkillsBySlug returns slug + content pairs for the given slugs. Used to
// inject only the skills @mentioned in a requirement's description.
func (s *SkillService) SkillsBySlug(slugs []string) ([]struct{ Slug, Content string }, error) {
	if len(slugs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(slugs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, len(slugs))
	for i, slug := range slugs {
		args[i] = slug
	}
	rows, err := s.db.Query(
		"SELECT slug, content FROM skills WHERE slug IN ("+placeholders+") ORDER BY name ASC",
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ Slug, Content string }
	for rows.Next() {
		var e struct{ Slug, Content string }
		if err := rows.Scan(&e.Slug, &e.Content); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
