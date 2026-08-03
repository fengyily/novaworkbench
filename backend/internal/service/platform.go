package service

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

type PlatformTokenService struct {
	db *sql.DB
}

func NewPlatformTokenService(db *sql.DB) *PlatformTokenService {
	return &PlatformTokenService{db: db}
}

func (s *PlatformTokenService) List() ([]model.PlatformToken, error) {
	rows, err := s.db.Query(
		`SELECT id, name, platform, base_url, created_at, updated_at FROM platform_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []model.PlatformToken
	for rows.Next() {
		var t model.PlatformToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Platform, &t.BaseURL, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, nil
}

func (s *PlatformTokenService) Create(name, platform, baseURL, token string) (*model.PlatformToken, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := "tok_" + hex.EncodeToString(b)
	now := time.Now()

	_, err := s.db.Exec(
		`INSERT INTO platform_tokens (id, name, platform, base_url, token, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, name, platform, baseURL, token, now, now)
	if err != nil {
		return nil, err
	}

	return &model.PlatformToken{
		ID:        id,
		Name:      name,
		Platform:  platform,
		BaseURL:   baseURL,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Get returns the token including the raw secret — for internal use only, never expose in API list.
func (s *PlatformTokenService) Get(id string) (*model.PlatformToken, error) {
	var t model.PlatformToken
	err := s.db.QueryRow(
		`SELECT id, name, platform, base_url, token, created_at, updated_at FROM platform_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.Platform, &t.BaseURL, &t.Token, &t.CreatedAt, &t.UpdatedAt)
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
