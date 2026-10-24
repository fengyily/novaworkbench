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
	"github.com/novaworkbench/backend/internal/util"
)

// ErrPlanNotFound is returned when a plan id does not resolve.
var ErrPlanNotFound = errors.New("test plan not found")

// TestPlanService owns CRUD and status transitions for test_plans.
//
// Like TestEnvService it stores state and knows nothing about how the
// planner/generator agents are executed — handler/testing_plan.go owns that
// and calls back into the Mark* methods here.
//
// plan_md is a COPY of the workspace's specs/<slug>.md, harvested after the
// planner finishes. Keeping the document in the database is what lets a plan
// stay readable (and re-generatable) after the workspace is deleted.
type TestPlanService struct {
	db *db.DB
	// envSvc resolves and validates the workspace a plan belongs to.
	envSvc *TestEnvService
}

func NewTestPlanService(d *db.DB, envSvc *TestEnvService) *TestPlanService {
	return &TestPlanService{db: d, envSvc: envSvc}
}

const testPlanColumns = `id, project_id, env_id, requirement_id, title, scenario,
	plan_path, plan_md, spec_files, status, claude_session_id, last_job_id, error,
	created_at, updated_at`

// scanTestPlan decodes the JSON-array column into SpecFileList so callers get
// a usable slice instead of a raw string.
func scanTestPlan(row interface{ Scan(...any) error }) (*model.TestPlan, error) {
	var p model.TestPlan
	var createdAt, updatedAt time.Time
	if err := row.Scan(
		&p.ID, &p.ProjectID, &p.EnvID, &p.RequirementID, &p.Title, &p.Scenario,
		&p.PlanPath, &p.PlanMD, &p.SpecFiles, &p.Status, &p.ClaudeSessionID,
		&p.LastJobID, &p.Error, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	p.SpecFileList = decodeSpecFiles(p.SpecFiles)
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	return &p, nil
}

// decodeSpecFiles tolerates a malformed column by returning nil rather than
// failing the whole read — a corrupt array should degrade the UI, not make
// the plan list 500.
func decodeSpecFiles(raw string) []string {
	if strings.TrimSpace(raw) == "" {
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

// Create inserts a plan in the 'draft' state. The planner flips it to
// 'planning' as soon as the agent is actually launched, so a plan whose
// launch failed outright is distinguishable from one that never started.
func (s *TestPlanService) Create(projectID string, req model.CreatePlanReq) (*model.TestPlan, error) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		// Derive one from the scenario so the agent has a filename slug.
		title = deriveTitle(req.Scenario)
	}
	if title == "" {
		return nil, errors.New("title or scenario is required")
	}
	scenario := strings.TrimSpace(req.Scenario)
	if scenario == "" {
		return nil, errors.New("scenario is required")
	}

	env, err := s.envSvc.Get(req.EnvID)
	if err != nil {
		return nil, err
	}
	// Cross-project guard: the URL's project must own the environment, or a
	// user could attach a plan to another project's test env.
	if env.ProjectID != projectID {
		return nil, fmt.Errorf("test environment %s does not belong to project %s", req.EnvID, projectID)
	}

	id := util.NewID("tp")
	now := time.Now()
	_, err = s.db.Exec(
		`INSERT INTO test_plans
		 (id, project_id, env_id, requirement_id, title, scenario, plan_path, plan_md,
		  spec_files, status, claude_session_id, last_job_id, error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, '', '', '[]', ?, '', '', '', ?, ?)`,
		id, projectID, env.ID, strings.TrimSpace(req.RequirementID), title, scenario,
		model.TestPlanStatusDraft, now, now,
	)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Get returns one plan by id.
func (s *TestPlanService) Get(planID string) (*model.TestPlan, error) {
	row := s.db.QueryRow(`SELECT `+testPlanColumns+` FROM test_plans WHERE id = ?`, planID)
	p, err := scanTestPlan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrPlanNotFound, planID)
	}
	return p, err
}

// List returns a project's plans, optionally narrowed to one environment.
// Newest first so a freshly generated plan is at the top.
func (s *TestPlanService) List(projectID, envID string) ([]*model.TestPlan, error) {
	q := `SELECT ` + testPlanColumns + ` FROM test_plans WHERE project_id = ?`
	args := []any{projectID}
	if envID != "" {
		q += ` AND env_id = ?`
		args = append(args, envID)
	}
	q += ` ORDER BY created_at DESC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*model.TestPlan{}
	for rows.Next() {
		p, err := scanTestPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// listByEnv returns every plan attached to an environment, regardless of
// project. Used by TestRunService.ResolveScope for the {"all":true} scope,
// where the environment already scopes the query and the project is implied.
func (s *TestPlanService) listByEnv(envID string) ([]*model.TestPlan, error) {
	rows, err := s.db.Query(
		`SELECT `+testPlanColumns+` FROM test_plans WHERE env_id = ? ORDER BY created_at ASC`, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*model.TestPlan{}
	for rows.Next() {
		p, err := scanTestPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// OwnsProject is the ownership check for the direct /api/testing/plans/{id}
// routes, which carry no project id in the path.
func (s *TestPlanService) OwnsProject(planID, projectID string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM test_plans WHERE id = ? AND project_id = ?`, planID, projectID,
	).Scan(&n)
	return n > 0, err
}

// Workspace returns the absolute workspace directory for a plan's
// environment.
func (s *TestPlanService) Workspace(plan *model.TestPlan) (string, error) {
	env, err := s.envSvc.Get(plan.EnvID)
	if err != nil {
		return "", err
	}
	return s.envSvc.EnsureWorkspace(env)
}

// PlanSlug is the workspace-relative path of a plan's markdown document,
// specs/<slug>.md. Shared by the planner (which writes there) and the
// generator (which reads from there), so the two can't disagree.
func PlanSlug(title string) string {
	return slugifyTitle(title)
}

// PlanFilePath is the absolute path of a plan's markdown document.
func (s *TestPlanService) PlanFilePath(plan *model.TestPlan) (string, error) {
	ws, err := s.Workspace(plan)
	if err != nil {
		return "", err
	}
	return filepath.Join(ws, "specs", PlanSlug(plan.Title)+".md"), nil
}

// UpdatePlanMD stores the plan document and mirrors it back to
// specs/<slug>.md on disk.
//
// The file write is what makes a human edit meaningful: the generator agent
// reads the plan from disk, so without the mirror a user's edit would be
// visible in the UI and invisible to the agent that consumes it.
func (s *TestPlanService) UpdatePlanMD(planID, planMD string) error {
	plan, err := s.Get(planID)
	if err != nil {
		return err
	}
	path, err := s.PlanFilePath(plan)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir specs: %w", err)
	}
	if err := os.WriteFile(path, []byte(planMD), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	_, err = s.db.Exec(
		`UPDATE test_plans SET plan_md = ?, plan_path = ?, updated_at = ? WHERE id = ?`,
		planMD, path, time.Now(), planID,
	)
	return err
}

// UpdateTitle renames a plan and moves its markdown file to match, so the
// slug (and therefore the generator's read path) stays in sync.
func (s *TestPlanService) UpdateTitle(planID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title is required")
	}
	plan, err := s.Get(planID)
	if err != nil {
		return err
	}
	oldPath, err := s.PlanFilePath(plan)
	if err != nil {
		return err
	}

	plan.Title = title
	newPath, err := s.PlanFilePath(plan)
	if err != nil {
		return err
	}
	if oldPath != newPath {
		if _, statErr := os.Stat(oldPath); statErr == nil {
			_ = os.MkdirAll(filepath.Dir(newPath), 0o755)
			_ = os.Rename(oldPath, newPath)
		}
	}

	_, err = s.db.Exec(
		`UPDATE test_plans SET title = ?, plan_path = ?, updated_at = ? WHERE id = ?`,
		title, newPath, time.Now(), planID,
	)
	return err
}

// UpdateSpecFiles replaces the generated spec file list.
func (s *TestPlanService) UpdateSpecFiles(planID string, files []string) error {
	if files == nil {
		files = []string{}
	}
	raw, err := json.Marshal(files)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`UPDATE test_plans SET spec_files = ?, updated_at = ? WHERE id = ?`,
		string(raw), time.Now(), planID,
	)
	return err
}

// ── status transitions ─────────────────────────────────────────────────
//
//	draft → planning → planned → generating → generated
//	              ↘        ↘            ↘
//	                 failed  ←──── retry ────
//
// Every transition also stamps updated_at so the UI can show "last touched".

// MarkPlanning records that the planner agent was launched. jobID ties the
// plan to the SSE stream the frontend is watching.
func (s *TestPlanService) MarkPlanning(planID, jobID string) error {
	return s.setStatus(planID, model.TestPlanStatusPlanning, jobID, "")
}

// MarkPlanned records a successful planner run: the harvested document, its
// on-disk path and the claude session id (which a later conversation can
// resume instead of re-exploring the app).
func (s *TestPlanService) MarkPlanned(planID, planMD, planPath, sessionID string) error {
	_, err := s.db.Exec(
		`UPDATE test_plans
		 SET status = ?, plan_md = ?, plan_path = ?, claude_session_id = ?, error = '', updated_at = ?
		 WHERE id = ?`,
		model.TestPlanStatusPlanned, planMD, planPath, sessionID, time.Now(), planID,
	)
	return err
}

// MarkGenerating records that the generator agent was launched.
func (s *TestPlanService) MarkGenerating(planID, jobID string) error {
	return s.setStatus(planID, model.TestPlanStatusGenerating, jobID, "")
}

// MarkGenerated records a successful generator run and the spec files it
// produced.
func (s *TestPlanService) MarkGenerated(planID string, specFiles []string, sessionID string) error {
	if specFiles == nil {
		specFiles = []string{}
	}
	raw, err := json.Marshal(specFiles)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`UPDATE test_plans
		 SET status = ?, spec_files = ?, claude_session_id = ?, error = '', updated_at = ?
		 WHERE id = ?`,
		model.TestPlanStatusGenerated, string(raw), sessionID, time.Now(), planID,
	)
	return err
}

// MarkFailed records a failed planning/generating attempt. Retry simply
// calls MarkPlanning again, so 'failed' is not terminal.
func (s *TestPlanService) MarkFailed(planID, errMsg string) error {
	if len(errMsg) > 2000 {
		errMsg = errMsg[:2000] + "…"
	}
	_, err := s.db.Exec(
		`UPDATE test_plans SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		model.TestPlanStatusFailed, errMsg, time.Now(), planID,
	)
	return err
}

// ClearError drops a stale error message without changing status, so the UI
// stops showing a failure banner after a successful retry.
func (s *TestPlanService) ClearError(planID string) error {
	_, err := s.db.Exec(`UPDATE test_plans SET error = '', updated_at = ? WHERE id = ?`, time.Now(), planID)
	return err
}

func (s *TestPlanService) setStatus(planID, status, jobID, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE test_plans SET status = ?, last_job_id = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, jobID, errMsg, time.Now(), planID,
	)
	return err
}

// RecoverInterrupted flips stale in-flight plans back to 'failed' on boot.
// Jobs live only in memory, so a plan still marked 'planning'/'generating'
// after a restart is an orphan — and without this it would look like work
// that is still running.
func (s *TestPlanService) RecoverInterrupted() (int, error) {
	res, err := s.db.Exec(
		`UPDATE test_plans SET status = ?, error = ?, updated_at = ?
		 WHERE status IN (?, ?)`,
		model.TestPlanStatusFailed, "服务器重启，任务被中断", time.Now(),
		model.TestPlanStatusPlanning, model.TestPlanStatusGenerating,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Delete removes the plan row and its markdown file. Generated spec files
// are left alone — they are shared with runs and may be referenced by
// history; the workspace-level cleanup is the user's call via the env purge.
func (s *TestPlanService) Delete(planID string) error {
	plan, err := s.Get(planID)
	if err != nil {
		return err
	}
	if path, err := s.PlanFilePath(plan); err == nil {
		_ = os.Remove(path)
	}
	_, err = s.db.Exec(`DELETE FROM test_plans WHERE id = ?`, planID)
	return err
}

// ── helpers ────────────────────────────────────────────────────────────

// deriveTitle makes a usable plan title from a scenario's first line, so
// the user can omit the title field. Truncated by rune so a Chinese
// scenario does not produce a broken UTF-8 slug.
func deriveTitle(scenario string) string {
	line := strings.TrimSpace(scenario)
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	const maxRunes = 60
	runes := []rune(line)
	if len(runes) > maxRunes {
		line = string(runes[:maxRunes])
	}
	return line
}

// slugifyTitle converts a title into a filename-safe slug. Non-ASCII titles
// (the common case here — scenarios are usually written in Chinese) collapse
// to "plan" rather than being dropped, so distinct plans still get distinct
// directories; the id keeps them unique at the DB level.
func slugifyTitle(title string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == ' ' || r == '-' || r == '_' || r == '/' || r == '.':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			// Drop non-ASCII (Chinese, punctuation) — see the doc comment.
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "plan"
	}
	const maxSlug = 60
	if len(slug) > maxSlug {
		slug = strings.Trim(slug[:maxSlug], "-")
	}
	return slug
}
