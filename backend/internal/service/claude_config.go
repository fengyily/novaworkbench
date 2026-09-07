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

// fullCols includes the config columns for internal use — including auth_token
// so env injection / activate / the masked list can reuse one scan.
const fullCols = "id, name, base_url, auth_token, models, default_model, currency, is_active, created_at, updated_at"

// DecodeModels parses the JSON stored in the models column: an array of
// {model,input_price,output_price} objects, OR a legacy array of plain model-id
// strings (recorded before pricing existed) converted to entries with 0 unit
// prices. An empty or malformed value yields an empty (non-nil) slice so
// callers never see null.
func DecodeModels(raw string) []model.ModelEntry {
	raw = trimSpace(raw)
	if raw == "" || raw == "null" {
		return []model.ModelEntry{}
	}
	var out []model.ModelEntry
	if err := json.Unmarshal([]byte(raw), &out); err == nil {
		if out == nil {
			out = []model.ModelEntry{}
		}
		return out
	}
	// Legacy string-array format: ["m1","m2"].
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return []model.ModelEntry{}
	}
	entries := make([]model.ModelEntry, 0, len(ids))
	for _, m := range ids {
		m = trimSpace(m)
		if m == "" {
			continue
		}
		entries = append(entries, model.ModelEntry{Model: m})
	}
	return entries
}

func encodeModels(models []model.ModelEntry) string {
	if models == nil {
		models = []model.ModelEntry{}
	}
	b, err := json.Marshal(models)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// normalizeModels trims model names, drops empties, and de-duplicates by model
// id while preserving order (keeping each entry's unit prices).
func normalizeModels(in []model.ModelEntry) []model.ModelEntry {
	out := make([]model.ModelEntry, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		m.Model = trimSpace(m.Model)
		if m.Model == "" || seen[m.Model] {
			continue
		}
		seen[m.Model] = true
		out = append(out, m)
	}
	return out
}

// modelEntryIDs projects a models entry list down to its model-id strings, for
// the role-settings dropdown and membership checks that only need names.
func modelEntryIDs(entries []model.ModelEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.Model)
	}
	return ids
}

func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

// scanFull scans a full-row config (including auth_token) into a model.
func scanFull(row interface{ Scan(...any) error }) (*model.ClaudeConfig, error) {
	var c model.ClaudeConfig
	var modelsJSON string
	var isActive int
	if err := row.Scan(&c.ID, &c.Name, &c.BaseURL, &c.AuthToken, &modelsJSON, &c.DefaultModel, &c.Currency, &isActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Models = DecodeModels(modelsJSON)
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
func validateModelsDefaults(models []model.ModelEntry, defaultModel string) ([]model.ModelEntry, string, error) {
	models = normalizeModels(models)
	defaultModel = trimSpace(defaultModel)
	if len(models) == 0 {
		return models, "", nil
	}
	if defaultModel != "" {
		found := false
		for _, m := range models {
			if m.Model == defaultModel {
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
func (s *ClaudeConfigService) Create(name, baseURL, authToken string, models []model.ModelEntry, defaultModel, currency string) (*model.ClaudeConfig, error) {
	name = trimSpace(name)
	if name == "" {
		return nil, errors.New("名称不能为空")
	}
	models, defaultModel, err := validateModelsDefaults(models, defaultModel)
	if err != nil {
		return nil, err
	}
	currency = trimSpace(currency)

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
		`INSERT INTO claude_configs (id, name, base_url, auth_token, models, default_model, currency, is_active, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, trimSpace(baseURL), authToken, encodeModels(models), defaultModel, currency, isActive, now, now,
	); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Update modifies a config. An empty authToken means "keep the existing
// secret"; set clearToken to explicitly remove it. A nil models slice means
// "leave the list unchanged"; a non-nil slice (empty or populated) replaces it.
func (s *ClaudeConfigService) Update(id, name, baseURL, authToken string, clearToken bool, models []model.ModelEntry, defaultModel, currency string) (*model.ClaudeConfig, error) {
	existing, err := s.Get(id)
	if err != nil {
		return nil, err
	}

	name = trimSpace(name)
	if name == "" {
		return nil, errors.New("名称不能为空")
	}
	currency = trimSpace(currency)

	// Resolve the models list: nil arg = keep existing; otherwise replace.
	var nextModels []model.ModelEntry
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
		`UPDATE claude_configs SET name=?, base_url=?, auth_token=?, models=?, default_model=?, currency=?, updated_at=? WHERE id=?`,
		name, trimSpace(baseURL), nextToken, encodeModels(nextModels), defaultModel, currency, time.Now(), id,
	); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Activate marks id as the single active config and, in the same transaction,
// pushes its default_model into every role that has NOT been bound to a
// specific Claude config. Bound roles keep their own model + config so
// per-role overrides survive a global activate (the requirement for
// req_0f2a842cd5096c52: the user can pick a non-active config's model and
// it must keep running against that config even after the global activate
// moves on). An empty default_model clears the unbound roles' models (so they
// fall back to the CLI default).
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
	// Push the default model into every UNBOUND role. Roles with a non-empty
	// claude_config_id keep their own model + config — that's exactly what the
	// "自由选择模型并固定为该模型配置" feature requires.
	if _, err = tx.Exec(
		"UPDATE roles SET model = ? WHERE "+s.db.Ident("claude_config_id")+" = ''",
		target.DefaultModel,
	); err != nil {
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

// ClaudeEnvForConfigID resolves the auth token + base URL for a SPECIFIC
// Claude config (the per-role binding lookup). An empty id falls back to the
// global active config so legacy call sites keep working. Returns
// ErrConfigNotFound when the requested id doesn't exist — callers treat that
// as "fall back to the global active config" so a stale binding never
// silently kills a run.
func (s *ClaudeConfigService) ClaudeEnvForConfigID(id string) (authToken, baseURL string, err error) {
	if id == "" {
		return s.ActiveEnvVars()
	}
	c, err := s.Get(id)
	if err != nil {
		return "", "", err
	}
	return c.AuthToken, c.BaseURL, nil
}

// ResolveRoleConfig returns the Claude configuration a role should run
// against. The role's own ClaudeConfigID wins; empty / unknown falls back to
// the global active config (so legacy unbound roles keep their old behavior).
// Returns nil + nil when no config is available at all (fresh DB / nothing
// configured) so callers can render a "no model available" state cleanly.
func (s *ClaudeConfigService) ResolveRoleConfig(role *model.Role) (*model.ClaudeConfig, error) {
	if role != nil && role.ClaudeConfigID != "" {
		c, err := s.Get(role.ClaudeConfigID)
		if err == nil {
			return c, nil
		}
		// Stale binding (config was deleted): fall back to the active one.
		// ErrConfigNotFound is the only path that triggers the fallback; any
		// other DB error is propagated so we don't hide real failures.
		if !errors.Is(err, ErrConfigNotFound) {
			return nil, err
		}
	}
	return s.ActiveConfig()
}

// ResolveConfigForModel returns the id of the claude_configs row whose
// models list contains modelName. Returns "" when no config claims it
// (or on an empty modelName) so callers can fall back to a role binding
// or the global active config without "no config" being treated as an
// error. Multiple configs may list the same model id; the first match
// (by created_at ASC, id ASC — matching List's order) wins so the lookup
// is deterministic.
//
// Used by the wizard's sub-task dispatch paths (sub_task_runner.Run and
// dispatchOneChild) so the resolved model always lands on the same gateway
// as the model name. Fixes the "model and Base URL mismatch" bug where
// modelName came from the developer role but ClaudeConfigID came from the
// executor role — when the two roles were bound to different claude_configs
// rows the request was sent to the wrong gateway.
func (s *ClaudeConfigService) ResolveConfigForModel(modelName string) (configID string, err error) {
	modelName = trimSpace(modelName)
	if modelName == "" {
		return "", nil
	}
	rows, err := s.db.Query("SELECT id, models FROM claude_configs ORDER BY created_at ASC, id ASC")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id, modelsJSON string
		if serr := rows.Scan(&id, &modelsJSON); serr != nil {
			return "", serr
		}
		for _, e := range DecodeModels(modelsJSON) {
			if e.Model == modelName {
				return id, nil
			}
		}
	}
	return "", rows.Err()
}

// ActiveModels returns the active config's model list + default model for the
// role-settings UI. Returns nil, "", nil when no config is active.
func (s *ClaudeConfigService) ActiveModels() (models []string, defaultModel string, err error) {
	c, err := s.ActiveConfig()
	if err != nil || c == nil {
		return nil, "", err
	}
	return modelEntryIDs(c.Models), c.DefaultModel, nil
}

// ActiveLaunchInfo returns the active config's base URL + default model for
// rendering the copy-paste `claude --settings '{"env":{...}}'` prefix the UI
// shows next to session ids. The auth token is deliberately NOT returned —
// tokens never leave the backend in full (same rule as the config list API);
// the pasted command works because the user's own ~/.claude auth or an
// interactive `claude /login` supplies it, while --settings pins the model +
// base URL exactly as Nova's own launches do.
func (s *ClaudeConfigService) ActiveLaunchInfo() (baseURL, defaultModel string, err error) {
	c, err := s.ActiveConfig()
	if err != nil || c == nil {
		return "", "", err
	}
	return c.BaseURL, c.DefaultModel, nil
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
	for _, e := range c.Models {
		if e.Model == m {
			return true, nil
		}
	}
	return false, nil
}

// ModelInAnyList reports whether m appears in the models list of ANY
// claude_configs row. An empty m is treated as "no opinion" and accepted.
// When no configs exist yet, every value is accepted (returns true).
//
// This loosens the role-model save gate so the user can pick a model from a
// non-active config in the settings UI. The CLI subprocess still receives
// env vars from the active config (ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN),
// so the caller is responsible for ensuring the chosen model is served by
// the active gateway — the soft warning in handler/role.go communicates
// that to the UI.
func (s *ClaudeConfigService) ModelInAnyList(m string) (bool, error) {
	if m == "" {
		return true, nil
	}
	rows, err := s.db.Query("SELECT models FROM claude_configs")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var modelsJSON string
		if err := rows.Scan(&modelsJSON); err != nil {
			return false, err
		}
		for _, e := range DecodeModels(modelsJSON) {
			if e.Model == m {
				return true, nil
			}
		}
	}
	return true, rows.Err()
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
