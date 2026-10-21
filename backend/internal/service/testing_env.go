package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/secret"
	"github.com/novaworkbench/backend/internal/util"
)

// ErrEnvBusy is returned by ClaimInit when another init is already running
// for this environment. The handler maps it to a 409 that carries the
// in-flight job id so the frontend can attach to the existing stream instead
// of making the user wait blind.
var ErrEnvBusy = errors.New("test environment is busy")

// ErrEnvNameTaken maps to a 409 on a duplicate (project_id, name).
var ErrEnvNameTaken = errors.New("a test environment with this name already exists")

// ErrEnvNotFound is returned when an id does not resolve.
var ErrEnvNotFound = errors.New("test environment not found")

// TestEnvService owns CRUD + init-state transitions for test_environments.
//
// The row is deliberately thin: it stores *configuration* (base_url, seed
// test, encrypted env vars) and the state of the one-time Playwright
// installation. It knows nothing about how the workspace is built — the
// subprocess chain lives in handler/testing_init.go, which calls back into
// MarkInitializing / MarkReady / MarkFailed here.
//
// env_vars are encrypted at rest with internal/secret (AES-256-GCM) and, like
// agent_servers.auth_value, are NEVER returned by the public accessors — they
// surface only as EnvVarsSet. Decryption happens exclusively in
// EnvVarsForRun, whose result is injected straight into the playwright
// subprocess environment (and registered with job.SetSecrets for masking).
type TestEnvService struct {
	db *db.DB
}

func NewTestEnvService(d *db.DB) *TestEnvService {
	return &TestEnvService{db: d}
}

const testEnvColumns = `id, project_id, name, base_url, workspace_override, env_vars_enc,
	seed_test, browsers, init_status, init_error, playwright_version, default_model,
	retention_runs, created_at, updated_at`

// scanTestEnv reads one row. env_vars_enc stays ciphertext in the struct;
// EnvVarsSet is derived so the UI can render "configured" without the value.
func scanTestEnv(row interface{ Scan(...any) error }) (*model.TestEnvironment, error) {
	var e model.TestEnvironment
	var createdAt, updatedAt time.Time
	if err := row.Scan(
		&e.ID, &e.ProjectID, &e.Name, &e.BaseURL, &e.WorkspaceOverride, &e.EnvVarsEnc,
		&e.SeedTest, &e.Browsers, &e.InitStatus, &e.InitError, &e.PlaywrightVersion,
		&e.DefaultModel, &e.RetentionRuns, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	e.EnvVarsSet = e.EnvVarsEnc != ""
	e.CreatedAt = createdAt
	e.UpdatedAt = updatedAt
	return &e, nil
}

// List returns every environment for a project, oldest first so the UI shows
// them in creation order. Credentials stay encrypted.
func (s *TestEnvService) List(projectID string) ([]*model.TestEnvironment, error) {
	rows, err := s.db.Query(
		`SELECT `+testEnvColumns+` FROM test_environments WHERE project_id = ? ORDER BY created_at ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*model.TestEnvironment{}
	for rows.Next() {
		e, err := scanTestEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get returns one environment. The caller must additionally verify
// env.ProjectID matches the project in the URL — see OwnsProject.
func (s *TestEnvService) Get(envID string) (*model.TestEnvironment, error) {
	row := s.db.QueryRow(`SELECT `+testEnvColumns+` FROM test_environments WHERE id = ?`, envID)
	e, err := scanTestEnv(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrEnvNotFound, envID)
	}
	return e, err
}

// OwnsProject is the ownership check for the "direct" /api/testing/envs/{id}
// routes, which carry no project id in the path. Without it a user who can
// read one project could read (and delete) another project's environment by
// guessing the id.
func (s *TestEnvService) OwnsProject(envID, projectID string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM test_environments WHERE id = ? AND project_id = ?`, envID, projectID,
	).Scan(&n)
	return n > 0, err
}

// Create inserts a new environment in the 'pending' state.
//
// env_vars are JSON-encoded then encrypted. An empty map stores the empty
// string (secret.Encrypt does the same for empty input) so EnvVarsSet stays
// false rather than decoding to an empty map on every read.
func (s *TestEnvService) Create(projectID string, req model.CreateTestEnvReq) (*model.TestEnvironment, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("name is required")
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		return nil, errors.New("base_url is required")
	}
	if err := validateBaseURL(baseURL); err != nil {
		return nil, err
	}

	browsers := normalizeBrowsers(req.Browsers)
	if browsers == "" {
		browsers = "chromium"
	}
	retention := req.RetentionRuns
	if retention <= 0 {
		retention = 20
	}
	if retention > 500 {
		return nil, errors.New("retention_runs must be <= 500")
	}

	ciphertext, err := encryptEnvVars(req.EnvVars)
	if err != nil {
		return nil, err
	}

	id := util.NewID("tenv")
	now := time.Now()
	_, err = s.db.Exec(
		`INSERT INTO test_environments
		 (id, project_id, name, base_url, workspace_override, env_vars_enc, seed_test,
		  browsers, init_status, init_error, playwright_version, default_model,
		  retention_runs, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, ?)`,
		id, projectID, name, baseURL, strings.TrimSpace(req.WorkspaceOverride),
		ciphertext, req.SeedTest, browsers, model.TestEnvStatusPending,
		strings.TrimSpace(req.DefaultModel), retention, now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %q in this project", ErrEnvNameTaken, name)
		}
		return nil, err
	}
	return s.Get(id)
}

// Update applies a patch; nil fields are left alone.
//
// EnvVars REPLACES the whole map when present (including with an empty map,
// which clears it) — the form sends "these are all the vars" rather than a
// delta, and a merge would make "delete this one var" impossible.
//
// Changing browsers or workspace_override while an init is mid-flight is
// refused: the running chain already resolved those values, so the finished
// workspace would silently disagree with the row. Re-running init picks the
// new values up.
func (s *TestEnvService) Update(envID string, req model.UpdateTestEnvReq) (*model.TestEnvironment, error) {
	existing, err := s.Get(envID)
	if err != nil {
		return nil, err
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, errors.New("name is required")
		}
		existing.Name = name
	}
	if req.BaseURL != nil {
		baseURL := strings.TrimSpace(*req.BaseURL)
		if baseURL == "" {
			return nil, errors.New("base_url is required")
		}
		if err := validateBaseURL(baseURL); err != nil {
			return nil, err
		}
		existing.BaseURL = baseURL
	}
	if req.WorkspaceOverride != nil {
		if existing.InitStatus == model.TestEnvStatusInitializing {
			return nil, errors.New("cannot change the workspace while initialization is running")
		}
		existing.WorkspaceOverride = strings.TrimSpace(*req.WorkspaceOverride)
	}
	if req.SeedTest != nil {
		existing.SeedTest = *req.SeedTest
	}
	if req.Browsers != nil {
		if existing.InitStatus == model.TestEnvStatusInitializing {
			return nil, errors.New("cannot change browsers while initialization is running")
		}
		if b := normalizeBrowsers(*req.Browsers); b != "" {
			existing.Browsers = b
		}
	}
	if req.DefaultModel != nil {
		existing.DefaultModel = strings.TrimSpace(*req.DefaultModel)
	}
	if req.RetentionRuns != nil {
		if *req.RetentionRuns <= 0 || *req.RetentionRuns > 500 {
			return nil, errors.New("retention_runs must be between 1 and 500")
		}
		existing.RetentionRuns = *req.RetentionRuns
	}

	ciphertext := existing.EnvVarsEnc
	if req.EnvVars != nil {
		c, err := encryptEnvVars(*req.EnvVars)
		if err != nil {
			return nil, err
		}
		ciphertext = c
	}

	_, err = s.db.Exec(
		`UPDATE test_environments
		 SET name = ?, base_url = ?, workspace_override = ?, env_vars_enc = ?,
		     seed_test = ?, browsers = ?, default_model = ?, retention_runs = ?, updated_at = ?
		 WHERE id = ?`,
		existing.Name, existing.BaseURL, existing.WorkspaceOverride, ciphertext,
		existing.SeedTest, existing.Browsers, existing.DefaultModel,
		existing.RetentionRuns, time.Now(), envID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %q in this project", ErrEnvNameTaken, existing.Name)
		}
		return nil, err
	}
	return s.Get(envID)
}

// Delete removes the row. purgeWorkspace additionally removes the on-disk
// directory — it is opt-in because a user may want to keep the generated
// specs after detaching them from NovaWorkbench.
func (s *TestEnvService) Delete(envID string, purgeWorkspace bool) error {
	env, err := s.Get(envID)
	if err != nil {
		return err
	}
	if purgeWorkspace {
		ws, wsErr := s.WorkspaceDir(env)
		if wsErr == nil {
			// A failure to remove the directory must NOT block the delete —
			// the row is the source of truth and leaving it behind would
			// strand the user with an undeletable environment.
			_ = os.RemoveAll(ws)
		}
	}
	_, err = s.db.Exec(`DELETE FROM test_environments WHERE id = ?`, envID)
	return err
}

// WorkspaceDir resolves the absolute path of an environment's workspace:
// the row's override if set, otherwise the default under ~/.novaworkbench.
// Note this does NOT create anything — use TestingWorkspaceDir for that.
func (s *TestEnvService) WorkspaceDir(env *model.TestEnvironment) (string, error) {
	if env.WorkspaceOverride != "" {
		return filepath.Clean(env.WorkspaceOverride), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for testing workspace: %w", err)
	}
	return filepath.Join(home, ".novaworkbench", "testing", env.ProjectID, env.ID), nil
}

// EnsureWorkspace returns the resolved workspace path and creates it.
func (s *TestEnvService) EnsureWorkspace(env *model.TestEnvironment) (string, error) {
	if env.WorkspaceOverride != "" {
		dir := filepath.Clean(env.WorkspaceOverride)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir workspace override %s: %w", dir, err)
		}
		return dir, nil
	}
	return util.TestingWorkspaceDir(env.ProjectID, env.ID)
}

// ── init state machine ──────────────────────────────────────────────────
//
//	pending ──ClaimInit──> initializing ──MarkReady──> ready
//	                             └──MarkFailed──> failed
//	failed ──ClaimInit──> initializing     (re-init is idempotent)
//	ready  ──ClaimInit──> initializing     (to upgrade Playwright)

// ClaimInit moves the row into 'initializing' and returns ErrEnvBusy if it
// is already there. This is the ONLY concurrency guard on init: two rapid
// clicks (or a double-submitted form) would otherwise run two npm installs
// into the same directory and corrupt node_modules.
//
// The check-and-set is a single conditional UPDATE, so it is atomic even
// when two requests race — the loser sees zero rows affected.
func (s *TestEnvService) ClaimInit(envID string) (*model.TestEnvironment, error) {
	res, err := s.db.Exec(
		`UPDATE test_environments
		 SET init_status = ?, init_error = '', updated_at = ?
		 WHERE id = ? AND init_status <> ?`,
		model.TestEnvStatusInitializing, time.Now(), envID, model.TestEnvStatusInitializing,
	)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		// Either the row is gone or it is already initializing. Tell the
		// caller which so it can return 404 vs 409.
		if _, err := s.Get(envID); err != nil {
			return nil, err
		}
		return nil, ErrEnvBusy
	}
	return s.Get(envID)
}

// MarkReady records a successful init plus the Playwright version that was
// actually installed (used to diagnose agent/version mismatches later).
func (s *TestEnvService) MarkReady(envID, playwrightVersion string) error {
	_, err := s.db.Exec(
		`UPDATE test_environments
		 SET init_status = ?, init_error = '', playwright_version = ?, updated_at = ?
		 WHERE id = ?`,
		model.TestEnvStatusReady, playwrightVersion, time.Now(), envID,
	)
	return err
}

// MarkFailed records an init failure. initError is truncated because npm
// and playwright emit multi-KB stacks that would bloat the row and every
// status poll that reads it.
func (s *TestEnvService) MarkFailed(envID, initError string) error {
	if len(initError) > 2000 {
		initError = initError[:2000] + "…"
	}
	_, err := s.db.Exec(
		`UPDATE test_environments SET init_status = ?, init_error = ?, updated_at = ? WHERE id = ?`,
		model.TestEnvStatusFailed, initError, time.Now(), envID,
	)
	return err
}

// ReleaseInit returns an environment to 'pending' when the handler could
// not even start the subprocess chain (e.g. npm is not on PATH). Distinct
// from MarkFailed, which records a real failure with a message.
func (s *TestEnvService) ReleaseInit(envID string) error {
	_, err := s.db.Exec(
		`UPDATE test_environments SET init_status = ?, updated_at = ? WHERE id = ? AND init_status = ?`,
		model.TestEnvStatusPending, time.Now(), envID, model.TestEnvStatusInitializing,
	)
	return err
}

// RecoverInterrupted flips stale 'initializing' rows back to 'failed' on
// boot. Jobs live only in memory, so a row still marked 'initializing' after
// a restart is an orphan whose process died with the previous server —
// leaving it would make every subsequent init return 409 forever. Same
// self-heal idea as the orchestration queue's Recover().
func (s *TestEnvService) RecoverInterrupted() (int, error) {
	res, err := s.db.Exec(
		`UPDATE test_environments
		 SET init_status = ?, init_error = ?, updated_at = ?
		 WHERE init_status = ?`,
		model.TestEnvStatusFailed, "服务器重启，初始化被中断", time.Now(),
		model.TestEnvStatusInitializing,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Status reports the environment plus its on-disk footprint. Retention
// trims runs by COUNT but a single trace.zip can be 50 MB+, so surfacing
// the size is what lets a user explain a full disk.
func (s *TestEnvService) Status(envID string) (*model.EnvStatusResp, error) {
	env, err := s.Get(envID)
	if err != nil {
		return nil, err
	}
	ws, err := s.WorkspaceDir(env)
	if err != nil {
		return nil, err
	}
	size, _ := dirSize(ws)
	env.DiskUsageBytes = size
	return &model.EnvStatusResp{Env: env, DiskUsageBytes: size, Workspace: ws}, nil
}

// EnvVarsForRun decrypts the environment's variables for injection into the
// playwright subprocess. Returns nil (not an error) when nothing is stored.
//
// The returned map must NOT be logged or persisted — the handler registers
// its values with job.SetSecrets so any echo in the job log is masked.
func (s *TestEnvService) EnvVarsForRun(env *model.TestEnvironment) (map[string]string, error) {
	if env.EnvVarsEnc == "" {
		return nil, nil
	}
	plain, err := secret.Decrypt(env.EnvVarsEnc)
	if err != nil {
		return nil, fmt.Errorf("test environment %s env vars: %w (master key changed?)", env.ID, err)
	}
	if plain == "" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(plain), &out); err != nil {
		return nil, fmt.Errorf("test environment %s env vars decode: %w", env.ID, err)
	}
	return out, nil
}

// SetEnvVarsForRun replaces the stored variable map. Used by the init chain
// and by any flow that programmatically injects a base token.
func (s *TestEnvService) SetEnvVarsForRun(envID string, kv map[string]string) error {
	ciphertext, err := encryptEnvVars(kv)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`UPDATE test_environments SET env_vars_enc = ?, updated_at = ? WHERE id = ?`,
		ciphertext, time.Now(), envID,
	)
	return err
}

// ── helpers ────────────────────────────────────────────────────────────

// encryptEnvVars JSON-encodes then AES-GCM-encrypts. An empty/nil map
// stores "" so EnvVarsSet reads false and Decrypt is never called.
func encryptEnvVars(kv map[string]string) (string, error) {
	if len(kv) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(kv)
	if err != nil {
		return "", err
	}
	return secret.Encrypt(string(raw))
}

// normalizeBrowsers accepts either a comma list ("chromium,firefox") or the
// singular values the UI radio group sends. Empty means "use the default".
func normalizeBrowsers(in string) string {
	parts := strings.Split(in, ",")
	valid := []string{"chromium", "firefox", "webkit"}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		for _, v := range valid {
			if p == v {
				out = append(out, p)
				break
			}
		}
	}
	return strings.Join(out, ",")
}

// validateBaseURL requires an http(s) origin. The value is handed to
// playwright as NOVA_BASE_URL and interpolated into playwright.config.ts, so
// rejecting non-http schemes here keeps a `file://` or `javascript:` payload
// out of both the config file and the browser.
func validateBaseURL(raw string) error {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("base_url must start with http:// or https://, got %q", raw)
	}
	if strings.ContainsAny(raw, " \t\n\"'`<>") {
		return fmt.Errorf("base_url contains characters that are unsafe to interpolate into a config file")
	}
	return nil
}

// isUniqueViolation recognises the three dialects' duplicate-key errors so
// Create/Update can surface a 409 instead of a 500.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || // sqlite
		strings.Contains(msg, "duplicate entry") || // mysql 1062
		strings.Contains(msg, "duplicate key value") // postgres 23505
}

// dirSize totals the bytes under root, stopping early past maxBytes so a
// runaway node_modules cannot make a status poll expensive. It returns the
// running total even on error, so a partial sum still beats zero.
func dirSize(root string) (int64, error) {
	const maxBytes = int64(8 << 30) // 8 GB ceiling
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			// Unreadable subtrees (permissions, races with a running test)
			// are skipped rather than aborting the whole walk.
			if os.IsPermission(err) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		if total >= maxBytes {
			return filepath.SkipAll
		}
		return nil
	})
	return total, err
}
