package service

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

type RoleService struct{ db *db.DB }

func NewRoleService(db *db.DB) *RoleService { return &RoleService{db: db} }

// roleColumns is the SELECT column list for roles, with `key` quoted for the
// active dialect (reserved word in MySQL).
func (s *RoleService) roleColumns() string {
	return "id, " + s.db.Ident("key") + ", name, description, system_prompt, model, sort_order, enabled, created_at, updated_at"
}

// SeedDefaults inserts any built-in role whose key is not yet present.
// Idempotent and per-key (not whole-table), so roles added in later releases
// (e.g. reviewer) are backfilled into existing databases on startup.
func (s *RoleService) SeedDefaults() error {
	now := time.Now()
	for _, r := range DefaultRoles() {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM roles WHERE "+s.db.Ident("key")+" = ?", r.Key).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if _, err := s.db.Exec(
			`INSERT INTO roles
			(`+s.roleColumns()+`)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Key, r.Name, r.Description, r.SystemPrompt, r.Model, r.SortOrder, r.Enabled, now, now,
		); err != nil {
			return fmt.Errorf("seed role %s: %w", r.Key, err)
		}
	}
	return nil
}

func (s *RoleService) List() ([]model.Role, error) {
	rows, err := s.db.Query("SELECT " + s.roleColumns() + " FROM roles ORDER BY sort_order ASC, " + s.db.Ident("key") + " ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roles []model.Role
	for rows.Next() {
		var r model.Role
		if err := rows.Scan(&r.ID, &r.Key, &r.Name, &r.Description, &r.SystemPrompt, &r.Model, &r.SortOrder, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}
	if roles == nil {
		roles = []model.Role{}
	}
	return roles, nil
}

func (s *RoleService) Get(id string) (*model.Role, error) {
	var r model.Role
	err := s.db.QueryRow("SELECT "+s.roleColumns()+" FROM roles WHERE id = ?", id).
		Scan(&r.ID, &r.Key, &r.Name, &r.Description, &r.SystemPrompt, &r.Model, &r.SortOrder, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("role not found")
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetByKey returns a role by its stable key (analyst/architect/developer/...),
// used by wizard handlers to load the active system prompt + model.
func (s *RoleService) GetByKey(key string) (*model.Role, error) {
	var r model.Role
	err := s.db.QueryRow("SELECT "+s.roleColumns()+" FROM roles WHERE "+s.db.Ident("key")+" = ?", key).
		Scan(&r.ID, &r.Key, &r.Name, &r.Description, &r.SystemPrompt, &r.Model, &r.SortOrder, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("role not found: %s", key)
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *RoleService) Update(id string, req model.UpdateRoleReq) (*model.Role, error) {
	if _, err := s.db.Exec("UPDATE roles SET system_prompt=?, model=?, updated_at=? WHERE id=?", req.SystemPrompt, req.Model, time.Now(), id); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Reset restores the built-in system_prompt for a role (matched by key),
// leaving the user-chosen model untouched.
func (s *RoleService) Reset(id string) (*model.Role, error) {
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	for _, d := range DefaultRoles() {
		if d.Key == r.Key {
			if _, err := s.db.Exec("UPDATE roles SET system_prompt=?, updated_at=? WHERE id=?", d.SystemPrompt, time.Now(), id); err != nil {
				return nil, err
			}
			break
		}
	}
	return s.Get(id)
}
