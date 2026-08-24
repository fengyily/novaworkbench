package service

import (
	"crypto/rand"
	"database/sql"

	"encoding/hex"
	"fmt"
	"github.com/novaworkbench/backend/internal/db"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

type PlatformTokenService struct {
	db *db.DB
}

func NewPlatformTokenService(db *db.DB) *PlatformTokenService {
	return &PlatformTokenService{db: db}
}

func (s *PlatformTokenService) List() ([]model.PlatformToken, error) {
	rows, err := s.db.Query(
		`SELECT id, name, platform, base_url, git_user_name, git_user_email, created_at, updated_at FROM platform_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []model.PlatformToken
	for rows.Next() {
		var t model.PlatformToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Platform, &t.BaseURL, &t.GitUserName, &t.GitUserEmail, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, nil
}

func (s *PlatformTokenService) Create(name, platform, baseURL, token, gitUserName, gitUserEmail string) (*model.PlatformToken, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := "tok_" + hex.EncodeToString(b)
	now := time.Now()

	_, err := s.db.Exec(
		`INSERT INTO platform_tokens (id, name, platform, base_url, token, git_user_name, git_user_email, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, platform, baseURL, token, gitUserName, gitUserEmail, now, now)
	if err != nil {
		return nil, err
	}

	return &model.PlatformToken{
		ID:           id,
		Name:         name,
		Platform:     platform,
		BaseURL:      baseURL,
		GitUserName:  gitUserName,
		GitUserEmail: gitUserEmail,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// Update rewrites the editable fields of a token row. updateSecret is the
// raw PAT to rotate — pass "" to keep the existing secret untouched (the
// common case when only the Git identity changes).
func (s *PlatformTokenService) Update(id, name, baseURL, gitUserName, gitUserEmail, updateSecret string) error {
	now := time.Now()
	var res sql.Result
	var err error
	if updateSecret != "" {
		res, err = s.db.Exec(
			`UPDATE platform_tokens SET name = ?, base_url = ?, git_user_name = ?, git_user_email = ?, token = ?, updated_at = ? WHERE id = ?`,
			name, baseURL, gitUserName, gitUserEmail, updateSecret, now, id)
	} else {
		res, err = s.db.Exec(
			`UPDATE platform_tokens SET name = ?, base_url = ?, git_user_name = ?, git_user_email = ?, updated_at = ? WHERE id = ?`,
			name, baseURL, gitUserName, gitUserEmail, now, id)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("token not found: %s", id)
	}
	return nil
}

// Get returns the token including the raw secret — for internal use only, never expose in API list.
func (s *PlatformTokenService) Get(id string) (*model.PlatformToken, error) {
	var t model.PlatformToken
	err := s.db.QueryRow(
		`SELECT id, name, platform, base_url, token, git_user_name, git_user_email, created_at, updated_at FROM platform_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.Platform, &t.BaseURL, &t.Token, &t.GitUserName, &t.GitUserEmail, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("token not found: %s", id)
	}
	return &t, err
}

func (s *PlatformTokenService) Delete(id string) error {
	res, err := s.db.Exec(`DELETE FROM platform_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("token not found: %s", id)
	}
	return nil
}
