package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// Legacy setting keys retained for one-way migration into claude_configs.
// The old settings rows are left in place (non-destructive) so a rollback to
// an older binary still finds the configuration.
const (
	legacyClaudeAuthToken = "claude.anthropic_auth_token"
	legacyClaudeBaseURL   = "claude.anthropic_base_url"
)

// ErrConfigNotFound is returned when a config id does not exist.
var ErrConfigNotFound = errors.New("claude config not found")

// ErrCannotDeleteActive is returned when deleting the currently-active config.
var ErrCannotDeleteActive = errors.New("不能删除当前生效的配置，请先切换到其他配置")

// ErrDefaultModelNotInList is returned when default_model is not within models.
var ErrDefaultModelNotInList = errors.New("默认模型必须在模型列表中")

type ClaudeConfigService struct {
	db *db.DB
}

func NewClaudeConfigService(database *db.DB) *ClaudeConfigService {
	return &ClaudeConfigService{db: database}
}

// fullCols includes auth_token — for internal use (env injection, activate,
// and list which needs the token to produce a masked preview).
const fullCols = "id, name, base_url, auth_token, models, default_model, is_active, created_at, updated_at"

// decodeModels parses the JSON array stored in the models column. An empty or
// malformed value yields an empty (non-nil) slice so callers never see null.
func decodeModels(raw string) []string {
	raw = trimSpace(raw)
	if raw == "" || raw == "null" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func encodeModels(models []string) string {
	if models == nil {
		models = []string{}
	}
	b, err := json.Marshal(models)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// normalizeModels trims whitespace, drops empties, and de-duplicates while
// preserving order.
func normalizeModels(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		m = trimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

// scanFull scans a full-row config (including auth_token) into a model.
func scanFull(row interface{ Scan(...any) error }) (*model.ClaudeConfig, error) {
	var c model.ClaudeConfig
	var modelsJSON string
	var isActive int
	if err := row.Scan(&c.ID, &c.Name, &c.BaseURL, &c.AuthToken, &modelsJSON, &c.DefaultModel, &isActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Models = decodeModels(modelsJSON)
	c.IsActive = isActive != 0
	return &c, nil
}

// List returns every config ordered by creation time. The auth token is read
// (so the handler can produce a masked preview) but never serialized: the
// model tags it json:"-" and the handler always masks it before responding.
func (s *ClaudeConfigService) List() ([]model.ClaudeConfig, error) {
	rows, err := s.db.Query("SELECT " + fullCols + " FROM claude_configs ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.ClaudeConfig{}
	for rows.Next() {
		c, err := scanFull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Get returns a config by id including the raw auth token — internal use only.
func (s *ClaudeConfigService) Get(id string) (*model.ClaudeConfig, error) {
	c, err := scanFull(s.db.QueryRow("SELECT "+fullCols+" FROM claude_configs WHERE id = ?", id))
	if err == sql.ErrNoRows {
		return nil, ErrConfigNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// count returns the number of config rows.
func (s *ClaudeConfigService) count() (int, error) {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM claude_configs").Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// validateModelsDefaults normalizes models and enforces that defaultModel (if
// non-empty) is a member of models. When models is empty, defaultModel is
// forced empty (no default without a list).
func validateModelsDefaults(models []string, defaultModel string) ([]string, string, error) {
	models = normalizeModels(models)
	defaultModel = trimSpace(defaultModel)
	if len(models) == 0 {
		return models, "", nil
	}
	if defaultModel != "" {
		found := false
		for _, m := range models {
			if m == defaultModel {
				found = true
				break
			}
		}
		if !found {
			return models, defaultModel, ErrDefaultModelNotInList
		}
	}
	return models, defaultModel, nil
}

// Create inserts a new config. The first config is auto-activated so the
// system is never left without an active configuration.
func (s *ClaudeConfigService) Create(name, baseURL, authToken string, models []string, defaultModel string) (*model.ClaudeConfig, error) {
	name = trimSpace(name)
	if name == "" {
		return nil, errors.New("名称不能为空")
	}
	models, defaultModel, err := validateModelsDefaults(models, defaultModel)
	if err != nil {
		return nil, err
	}

	id := util.NewID("ccfg")
	now := time.Now()
	n, err := s.count()
	if err != nil {
		return nil, err
	}
	isActive := 0
	if n == 0 {
		isActive = 1 // first config auto-activates
	}

	if _, err := s.db.Exec(
		`INSERT INTO claude_configs (id, name, base_url, auth_token, models, default_model, is_active, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, trimSpace(baseURL), authToken, encodeModels(models), defaultModel, isActive, now, now,
	); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Update modifies a config. An empty authToken means "keep the existing
// secret"; set clearToken to explicitly remove it. A nil models slice means
// "leave the list unchanged"; a non-nil slice (empty or populated) replaces it.
func (s *ClaudeConfigService) Update(id, name, baseURL, authToken string, clearToken bool, models []string, defaultModel string) (*model.ClaudeConfig, error) {
	existing, err := s.Get(id)
	if err != nil {
		return nil, err
	}

	name = trimSpace(name)
	if name == "" {
		return nil, errors.New("名称不能为空")
	}

	// Resolve the models list: nil arg = keep existing; otherwise replace.
	var nextModels []string
	if models == nil {
		nextModels = existing.Models
	} else {
		nextModels = models
	}
	nextModels, defaultModel, err = validateModelsDefaults(nextModels, defaultModel)
	if err != nil {
		return nil, err
	}

	// Resolve the auth token.
	nextToken := existing.AuthToken
	if clearToken {
		nextToken = ""
	} else if authToken != "" {
		nextToken = authToken
	}

	if _, err := s.db.Exec(
		`UPDATE claude_configs SET name=?, base_url=?, auth_token=?, models=?, default_model=?, updated_at=? WHERE id=?`,
		name, trimSpace(baseURL), nextToken, encodeModels(nextModels), defaultModel, time.Now(), id,
	); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Activate marks id as the single active config and, in the same transaction,
// pushes its default_model into every role so the next AI task uses it. An
// empty default_model clears all role models (fall back to the CLI default).
func (s *ClaudeConfigService) Activate(id string) (appliedModel string, err error) {
	target, err := s.Get(id)
	if err != nil {
		return "", err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if _, err = tx.Exec("UPDATE claude_configs SET is_active = 0"); err != nil {
		return "", err
	}
	if _, err = tx.Exec("UPDATE claude_configs SET is_active = 1, updated_at = ? WHERE id = ?", time.Now(), id); err != nil {
		return "", err
	}
	// Push the default model into every role (empty = clear, i.e. CLI default).
	if _, err = tx.Exec("UPDATE roles SET model = ?", target.DefaultModel); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return target.DefaultModel, nil
}

// Delete removes a config. The currently-active config cannot be deleted.
func (s *ClaudeConfigService) Delete(id string) error {
	// Re-check is_active on the target row (race-safe under SQLite single writer;
	// on MySQL/PG the check-then-delete is acceptable for this low-frequency op).
	var isActive int
	err := s.db.QueryRow("SELECT is_active FROM claude_configs WHERE id = ?", id).Scan(&isActive)
	if err == sql.ErrNoRows {
		return ErrConfigNotFound
	}
	if err != nil {
		return err
	}
	if isActive != 0 {
		return ErrCannotDeleteActive
	}
	res, err := s.db.Exec("DELETE FROM claude_configs WHERE id = ?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrConfigNotFound
	}
	return nil
}

// ActiveConfig returns the currently-active config (auth token included) for
// internal use, or (nil, nil) when no config is active.
func (s *ClaudeConfigService) ActiveConfig() (*model.ClaudeConfig, error) {
	c, err := scanFull(s.db.QueryRow("SELECT " + fullCols + " FROM claude_configs WHERE is_active = 1 LIMIT 1"))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ActiveEnvVars returns the active config's auth token + base URL. Empty
// strings (no active row or fields unset) make the gateway fall back to the
// process environment — matching today's "unconfigured" behavior.
func (s *ClaudeConfigService) ActiveEnvVars() (authToken, baseURL string, err error) {
	c, err := s.ActiveConfig()
	if err != nil {
		return "", "", err
	}
	if c == nil {
		return "", "", nil
	}
	return c.AuthToken, c.BaseURL, nil
}

// ClaudeEnvVars implements llm.ClaudeEnvProvider so the gateway can pull the
// active token + base URL at command-build time.
func (s *ClaudeConfigService) ClaudeEnvVars() (authToken, baseURL string, err error) {
	return s.ActiveEnvVars()
}

// ActiveModels returns the active config's model list + default model for the
// role-settings UI. Returns nil, "", nil when no config is active.
func (s *ClaudeConfigService) ActiveModels() (models []string, defaultModel string, err error) {
	c, err := s.ActiveConfig()
	if err != nil || c == nil {
		return nil, "", err
	}
	return c.Models, c.DefaultModel, nil
}

// ModelInActiveList reports whether m is among the active config's models.
// Used for the role-model soft validation warning. When there is no active
// config or its list is empty, every value is accepted (returns true).
func (s *ClaudeConfigService) ModelInActiveList(m string) (bool, error) {
	c, err := s.ActiveConfig()
	if err != nil || c == nil {
		return true, err
	}
	if len(c.Models) == 0 {
		return true, nil
	}
	for _, x := range c.Models {
		if x == m {
			return true, nil
		}
	}
	return false, nil
}

// MigrateLegacy is a one-way, idempotent migration: if claude_configs is empty
// AND the old settings keys hold a token/base URL, seed a single "默认配置"
// row marked active. The old settings rows are left untouched.
func (s *ClaudeConfigService) MigrateLegacy() error {
	n, err := s.count()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	var token, baseURL string
	if err := s.db.QueryRow("SELECT value FROM settings WHERE "+s.db.Ident("key")+" = ?", legacyClaudeAuthToken).Scan(&token); err != nil && err != sql.ErrNoRows {
		return err
	}
	if err := s.db.QueryRow("SELECT value FROM settings WHERE "+s.db.Ident("key")+" = ?", legacyClaudeBaseURL).Scan(&baseURL); err != nil && err != sql.ErrNoRows {
		return err
	}
	if token == "" && baseURL == "" {
		return nil
	}

	id := util.NewID("ccfg")
	now := time.Now()
	if _, err := s.db.Exec(
		`INSERT INTO claude_configs (id, name, base_url, auth_token, models, default_model, is_active, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "默认配置", baseURL, token, "[]", "", 1, now, now,
	); err != nil {
		return fmt.Errorf("seed legacy claude config: %w", err)
	}
	return nil
}
