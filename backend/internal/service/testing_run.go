package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// ErrRunNotFound / ErrCaseNotFound are returned when an id does not resolve.
var (
	ErrRunNotFound  = errors.New("test run not found")
	ErrCaseNotFound = errors.New("test case not found")
)

// errLen bounds how much of a case's error text is persisted. Playwright
// stacks with full DOM dumps can reach hundreds of KB, which would bloat
// every list query that touches test_run_cases.
const errLen = 8 * 1024

// TestRunService owns test_runs + test_run_cases: creation, the JSON report
// ingest, retention trimming and the failure→issue close-loop backfills.
//
// Batch parallelism is deliberately NOT modeled here. A run is ONE
// `npx playwright test` subprocess and Playwright's own workers provide the
// concurrency; the only thing this service guards is one run per environment
// at a time (enforced by the handler), so two runs cannot fight over the same
// working directory.
type TestRunService struct {
	db      *db.DB
	envSvc  *TestEnvService
	planSvc *TestPlanService
}

func NewTestRunService(d *db.DB, envSvc *TestEnvService, planSvc *TestPlanService) *TestRunService {
	return &TestRunService{db: d, envSvc: envSvc, planSvc: planSvc}
}

const testRunColumns = `id, project_id, env_id, scope, status, total, passed, failed,
	flaky, skipped, healer_used, healed_from, report_path, job_id, started_at,
	finished_at, duration_ms, error`

func scanTestRun(row interface{ Scan(...any) error }) (*model.TestRun, error) {
	var r model.TestRun
	var startedAt time.Time
	var finishedAt sql.NullTime
	if err := row.Scan(
		&r.ID, &r.ProjectID, &r.EnvID, &r.Scope, &r.Status, &r.Total, &r.Passed,
		&r.Failed, &r.Flaky, &r.Skipped, &r.HealerUsed, &r.HealedFrom, &r.ReportPath,
		&r.JobID, &startedAt, &finishedAt, &r.DurationMs, &r.Error,
	); err != nil {
		return nil, err
	}
	r.StartedAt = startedAt
	if finishedAt.Valid {
		t := finishedAt.Time
		r.FinishedAt = &t
	}
	// surface the replayable scope as structured JSON for the API
	var scope any
	if err := json.Unmarshal([]byte(r.Scope), &scope); err == nil {
		r.ScopeJSON = scope
	} else {
		r.ScopeJSON = map[string]any{}
	}
	return &r, nil
}

// Create inserts a run in the 'running' state.
func (s *TestRunService) Create(projectID, envID string, scope model.RunScope, jobID string) (*model.TestRun, error) {
	env, err := s.envSvc.Get(envID)
	if err != nil {
		return nil, err
	}
	if env.ProjectID != projectID {
		return nil, fmt.Errorf("test environment %s does not belong to project %s", envID, projectID)
	}

	raw, err := json.Marshal(scope)
	if err != nil {
		return nil, err
	}
	id := util.NewID("trun")
	_, err = s.db.Exec(
		`INSERT INTO test_runs (id, project_id, env_id, scope, status, job_id, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, projectID, env.ID, string(raw), model.TestRunStatusRunning, jobID, time.Now(),
	)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Get returns one run by id.
func (s *TestRunService) Get(runID string) (*model.TestRun, error) {
	row := s.db.QueryRow(`SELECT `+testRunColumns+` FROM test_runs WHERE id = ?`, runID)
	r, err := scanTestRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
	}
	return r, err
}

// List returns a project's runs, newest first, optionally per environment.
func (s *TestRunService) List(projectID, envID string) ([]*model.TestRun, error) {
	q := `SELECT ` + testRunColumns + ` FROM test_runs WHERE project_id = ?`
	args := []any{projectID}
	if envID != "" {
		q += ` AND env_id = ?`
		args = append(args, envID)
	}
	q += ` ORDER BY started_at DESC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*model.TestRun{}
	for rows.Next() {
		r, err := scanTestRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OwnsProject guards the direct /api/testing/runs/{id} routes.
func (s *TestRunService) OwnsProject(runID, projectID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM test_runs WHERE id = ? AND project_id = ?`, runID, projectID).Scan(&n)
	return n > 0, err
}

// Workspace returns the absolute workspace directory for a run's env.
func (s *TestRunService) Workspace(run *model.TestRun) (string, error) {
	env, err := s.envSvc.Get(run.EnvID)
	if err != nil {
		return "", err
	}
	return s.envSvc.EnsureWorkspace(env)
}

// RunDir is the per-run directory holding the JSON report and all evidence
// (screenshots, traces). Everything under here is what the artifacts route
// may serve, and what retention trimming deletes.
func (s *TestRunService) RunDir(run *model.TestRun) (string, error) {
	ws, err := s.Workspace(run)
	if err != nil {
		return "", err
	}
	return filepath.Join(ws, "runs", run.ID), nil
}

// ResolveScope turns a run scope into the concrete, de-duplicated list of
// spec files to pass to `npx playwright test`.
//
// scope.All walks every generated plan in the environment; plan_ids narrows
// to those plans; spec_files passes explicit paths through. An empty result
// is an error rather than "run zero tests", because an empty playwright
// invocation silently exits 0 and would report a misleading pass.
func (s *TestRunService) ResolveScope(envID string, scope model.RunScope) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	add := func(list []string) {
		for _, f := range list {
			f = strings.TrimSpace(f)
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			files = append(files, f)
		}
	}

	if len(scope.SpecFiles) > 0 {
		add(scope.SpecFiles)
	} else if len(scope.PlanIDs) > 0 {
		for _, pid := range scope.PlanIDs {
			plan, err := s.planSvc.Get(pid)
			if err != nil {
				return nil, err
			}
			if plan.EnvID != envID {
				return nil, fmt.Errorf("test plan %s does not belong to this environment", pid)
			}
			add(plan.SpecFileList)
		}
	} else {
		plans, err := s.planSvc.ListByEnv(envID)
		if err != nil {
			return nil, err
		}
		for _, p := range plans {
			add(p.SpecFileList)
		}
	}

	if len(files) == 0 {
		return nil, errors.New("没有可执行的测试用例，请先生成测试代码")
	}
	// Stable order so a re-run is comparable to its predecessor.
	sort.Strings(files)
	return files, nil
}

// ListByEnv exposes the environment-scoped plan list so ResolveScope can
// resolve the {"all":true} scope without needing a project id.
func (s *TestPlanService) ListByEnv(envID string) ([]*model.TestPlan, error) {
	return s.listByEnv(envID)
}

// ── report ingest ──────────────────────────────────────────────────────

// pwReport mirrors the subset of Playwright's JSON reporter schema we read.
// Suites nest arbitrarily (describe blocks nest), so ingestion recurses.
type pwReport struct {
	Config struct {
		RootDir string `json:"rootDir"`
	} `json:"config"`
	Stats struct {
		Expected   int `json:"expected"`
		Unexpected int `json:"unexpected"`
		Flaky      int `json:"flaky"`
		Skipped    int `json:"skipped"`
	} `json:"stats"`
	Suites []pwSuite `json:"suites"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type pwSuite struct {
	Title  string    `json:"title"`
	File   string    `json:"file"`
	Suites []pwSuite `json:"suites"`
	Specs  []pwSpec  `json:"specs"`
}

type pwSpec struct {
	Title string   `json:"title"`
	OK    bool     `json:"ok"`
	Tests []pwTest `json:"tests"`
}

type pwTest struct {
	ProjectName string `json:"projectName"`
	Status      string `json:"status"` // expected | unexpected | flaky | skipped
	Results     []struct {
		Status   string `json:"status"`
		Duration int    `json:"duration"`
		Error    *struct {
			Message string `json:"message"`
			Stack   string `json:"stack"`
		} `json:"error"`
		Attachments []struct {
			Name        string `json:"name"`
			Path        string `json:"path"`
			ContentType string `json:"contentType"`
		} `json:"attachments"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"results"`
	Errors []struct {
		Message string `json:"message"`
		Stack   string `json:"stack"`
	} `json:"errors"`
}

// IngestReport parses a Playwright JSON report into test_run_cases and
// backfills the run's counters and terminal status.
//
// Attachment paths arrive ABSOLUTE from the reporter. They are rewritten to
// be relative to the run directory, and anything that resolves outside it is
// DROPPED — the artifacts route serves by joining this relative path onto the
// run dir, so an absolute or traversing path here would be a directory
// traversal primitive.
func (s *TestRunService) IngestReport(runID, reportPath string) error {
	run, err := s.Get(runID)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		return fmt.Errorf("read playwright report %s: %w", reportPath, err)
	}
	var rep pwReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("parse playwright report %s: %w", reportPath, err)
	}

	runDir, err := s.RunDir(run)
	if err != nil {
		return err
	}

	cases := make([]*model.TestRunCase, 0, 32)
	collectCases(rep.Suites, rep.Config.RootDir, runDir, &cases)

	// A replacement ingest (the healer re-run writes a fresh run id) must not
	// double up. Clear first so a retried ingest is idempotent.
	if _, err := s.db.Exec(`DELETE FROM test_run_cases WHERE run_id = ?`, runID); err != nil {
		return err
	}
	for _, c := range cases {
		attachments, _ := json.Marshal(c.AttachmentList)
		_, err := s.db.Exec(
			`INSERT INTO test_run_cases
			 (id, run_id, file, title, status, error, duration_ms, attachments,
			  issue_url, issue_number, requirement_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', 0, '')`,
			util.NewID("trc"), runID, c.File, c.Title, c.Status, c.Error,
			c.DurationMs, string(attachments),
		)
		if err != nil {
			return err
		}
	}

	// Count from the cases we actually stored rather than from stats, so the
	// ledger always matches the rows the user can click.
	counts := map[string]int{}
	for _, c := range cases {
		counts[c.Status]++
	}
	total := len(cases)
	failed := counts[model.TestCaseStatusFailed] + counts[model.TestCaseStatusTimedOut] + counts[model.TestCaseStatusInterrupted]
	status := model.TestRunStatusPassed
	if failed > 0 {
		status = model.TestRunStatusFailed
	}

	duration := 0
	if run.StartedAt != (time.Time{}) {
		duration = int(time.Since(run.StartedAt).Milliseconds())
	}
	_, err = s.db.Exec(
		`UPDATE test_runs
		 SET total = ?, passed = ?, failed = ?, flaky = ?, skipped = ?, status = ?,
		     finished_at = ?, duration_ms = ?, error = ''
		 WHERE id = ?`,
		total, counts[model.TestCaseStatusPassed], failed, counts[model.TestCaseStatusFlaky],
		counts[model.TestCaseStatusSkipped], status, time.Now(), duration, runID,
	)
	return err
}

// collectCases walks the suite tree depth-first, building one case per spec.
// titles are joined with " › " so nested describe blocks stay legible.
func collectCases(suites []pwSuite, rootDir, runDir string, out *[]*model.TestRunCase) {
	for _, suite := range suites {
		// A suite with no file is a bare folder / config group — recurse only.
		collectCases(suite.Suites, rootDir, runDir, out)
		if suite.File == "" {
			continue
		}
		for _, spec := range suite.Specs {
			if len(spec.Tests) == 0 {
				continue
			}
			title := strings.Trim(strings.Join([]string{suite.Title, spec.Title}, " › "), " ›")
			for _, test := range spec.Tests {
				c := buildCase(suite.File, title, test, rootDir, runDir)
				*out = append(*out, c)
			}
		}
	}
}

// buildCase converts one Playwright test entry into a stored case.
func buildCase(file, title string, test pwTest, rootDir, runDir string) *model.TestRunCase {
	c := &model.TestRunCase{
		File:           relativizeSpecPath(file, rootDir),
		Title:          title,
		AttachmentList: []model.TestAttachment{},
	}

	// Seed the status from the test-level outcome.
	switch test.Status {
	case "flaky":
		c.Status = model.TestCaseStatusFlaky
	case "skipped":
		c.Status = model.TestCaseStatusSkipped
	case "expected":
		c.Status = model.TestCaseStatusPassed
	case "unexpected":
		c.Status = model.TestCaseStatusFailed
	}

	// The LAST result decides the final state — for a retried test that is
	// the attempt that actually determined the outcome.
	if len(test.Results) > 0 {
		last := test.Results[len(test.Results)-1]
		c.DurationMs = last.Duration
		switch last.Status {
		case "passed":
			c.Status = model.TestCaseStatusPassed
		case "skipped":
			c.Status = model.TestCaseStatusSkipped
		case "timedOut":
			c.Status = model.TestCaseStatusTimedOut
		case "interrupted":
			c.Status = model.TestCaseStatusInterrupted
		case "failed":
			c.Status = model.TestCaseStatusFailed
		}
		if last.Error != nil && last.Error.Message != "" {
			c.Error = truncateErr(last.Error.Message)
		}
		for _, att := range last.Attachments {
			if rel, ok := safeRelativePath(att.Path, runDir); ok {
				c.AttachmentList = append(c.AttachmentList, model.TestAttachment{
					Name:        att.Name,
					Path:        rel,
					ContentType: att.ContentType,
				})
			}
		}
	}

	// Re-apply flaky LAST. The reporter marks a test flaky when it needed
	// retries and eventually passed, so the last result reads "passed" and
	// would otherwise erase the flakiness. Flakiness is the actionable
	// signal — it is what the healer agent and the run ledger key off — so
	// it must win over the raw last-result status.
	if test.Status == "flaky" {
		c.Status = model.TestCaseStatusFlaky
	}

	// A test can carry errors without any result (e.g. failed to load).
	if c.Error == "" {
		for _, e := range test.Errors {
			if e.Message != "" {
				c.Error = truncateErr(e.Message)
				break
			}
		}
	}
	return c
}

// relativizeSpecPath makes a spec path workspace-relative ("tests/x.spec.ts").
//
// The reporter emits `file` either absolute or already relative to rootDir
// depending on version. An already-relative path must be passed through
// untouched — running filepath.Rel against rootDir would turn
// "tests/login.spec.ts" into "../../…/tests/login.spec.ts" or strip it down
// to the basename.
func relativizeSpecPath(file, rootDir string) string {
	if file == "" {
		return ""
	}
	clean := filepath.Clean(file)
	if !filepath.IsAbs(clean) {
		return filepath.ToSlash(clean)
	}
	if rootDir != "" {
		if rel, err := filepath.Rel(filepath.Clean(rootDir), clean); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filepath.Base(clean))
}

// safeRelativePath converts an absolute attachment path into one relative to
// runDir, and reports false for anything that escapes it.
//
// The check uses filepath.Rel and an explicit ".." test rather than
// strings.HasPrefix, because /runs/abc-evil has "/runs/abc" as a prefix and
// would slip past a naive check.
func safeRelativePath(absPath, runDir string) (string, bool) {
	if absPath == "" {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(runDir), filepath.Clean(absPath))
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func truncateErr(s string) string {
	if len(s) <= errLen {
		return s
	}
	return s[:errLen] + "\n… (已截断)"
}

// GetCases returns a run's cases in report order.
func (s *TestRunService) GetCases(runID string) ([]*model.TestRunCase, error) {
	rows, err := s.db.Query(
		`SELECT id, run_id, file, title, status, error, duration_ms, attachments,
		        issue_url, issue_number, requirement_id
		 FROM test_run_cases WHERE run_id = ? ORDER BY rowid ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*model.TestRunCase{}
	for rows.Next() {
		var c model.TestRunCase
		if err := rows.Scan(&c.ID, &c.RunID, &c.File, &c.Title, &c.Status, &c.Error,
			&c.DurationMs, &c.Attachments, &c.IssueURL, &c.IssueNumber, &c.RequirementID); err != nil {
			return nil, err
		}
		var atts []model.TestAttachment
		if err := json.Unmarshal([]byte(c.Attachments), &atts); err != nil {
			atts = []model.TestAttachment{}
		}
		c.AttachmentList = atts
		out = append(out, &c)
	}
	return out, rows.Err()
}

// GetCase returns one case by id.
func (s *TestRunService) GetCase(caseID string) (*model.TestRunCase, error) {
	var c model.TestRunCase
	err := s.db.QueryRow(
		`SELECT id, run_id, file, title, status, error, duration_ms, attachments,
		        issue_url, issue_number, requirement_id
		 FROM test_run_cases WHERE id = ?`, caseID,
	).Scan(&c.ID, &c.RunID, &c.File, &c.Title, &c.Status, &c.Error,
		&c.DurationMs, &c.Attachments, &c.IssueURL, &c.IssueNumber, &c.RequirementID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrCaseNotFound, caseID)
	}
	if err != nil {
		return nil, err
	}
	var atts []model.TestAttachment
	if err := json.Unmarshal([]byte(c.Attachments), &atts); err != nil {
		atts = []model.TestAttachment{}
	}
	c.AttachmentList = atts
	return &c, nil
}

// FailingCases returns the run's non-passing cases — the input to the healer
// agent and to the close-loop UI.
func (s *TestRunService) FailingCases(runID string) ([]*model.TestRunCase, error) {
	cases, err := s.GetCases(runID)
	if err != nil {
		return nil, err
	}
	out := []*model.TestRunCase{}
	for _, c := range cases {
		if c.Status != model.TestCaseStatusPassed && c.Status != model.TestCaseStatusSkipped {
			out = append(out, c)
		}
	}
	return out, nil
}

// AttachIssueURL backfills the close-loop. Guarded so a second click cannot
// open a duplicate issue — the caller checks IsEmpty, this enforces it.
func (s *TestRunService) AttachIssueURL(caseID, issueURL string, issueNumber int) error {
	res, err := s.db.Exec(
		`UPDATE test_run_cases SET issue_url = ?, issue_number = ? WHERE id = ? AND issue_url = ''`,
		issueURL, issueNumber, caseID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("case already has an issue or does not exist")
	}
	return nil
}

// AttachRequirementID backfills the "转为本地需求" close-loop.
func (s *TestRunService) AttachRequirementID(caseID, requirementID string) error {
	res, err := s.db.Exec(
		`UPDATE test_run_cases SET requirement_id = ? WHERE id = ? AND requirement_id = ''`,
		requirementID, caseID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("case already has a requirement or does not exist")
	}
	return nil
}

// ── lifecycle ──────────────────────────────────────────────────────────

// FinishRun records a terminal status for a run that did NOT go through
// IngestReport (a process crash, a cancel, an unreadable report).
func (s *TestRunService) FinishRun(runID, status, errMsg string) error {
	if len(errMsg) > 2000 {
		errMsg = errMsg[:2000] + "…"
	}
	run, err := s.Get(runID)
	if err != nil {
		return err
	}
	duration := 0
	if run.StartedAt != (time.Time{}) {
		duration = int(time.Since(run.StartedAt).Milliseconds())
	}
	_, err = s.db.Exec(
		`UPDATE test_runs SET status = ?, error = ?, finished_at = ?, duration_ms = ? WHERE id = ?`,
		status, errMsg, time.Now(), duration, runID)
	return err
}

// MarkHealerUsed records that the healer agent ran for a run's failures.
func (s *TestRunService) MarkHealerUsed(runID string) error {
	_, err := s.db.Exec(`UPDATE test_runs SET healer_used = 1 WHERE id = ?`, runID)
	return err
}

// RecoverInterrupted flips orphaned 'running' runs to 'error' on boot. Jobs
// are in-memory only, so a row still 'running' after restart has no process
// behind it.
func (s *TestRunService) RecoverInterrupted() (int, error) {
	res, err := s.db.Exec(
		`UPDATE test_runs SET status = ?, error = ?, finished_at = ? WHERE status = ?`,
		model.TestRunStatusError, "服务器重启，运行被中断", time.Now(), model.TestRunStatusRunning)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// TrimRetention deletes the oldest runs beyond the environment's retention
// limit, removing both the DB rows (and their cases, via the FK cascade
// SQLite does NOT enforce — so the cases are removed explicitly) and the
// on-disk evidence directories.
//
// Disk, not rows, is the real constraint here: a single trace.zip can be
// tens of MB, so keeping 20 runs is a meaningful cap rather than a cosmetic
// one.
func (s *TestRunService) TrimRetention(envID string) error {
	env, err := s.envSvc.Get(envID)
	if err != nil {
		return err
	}
	keep := env.RetentionRuns
	if keep <= 0 {
		keep = 20
	}

	rows, err := s.db.Query(`SELECT id FROM test_runs WHERE env_id = ? ORDER BY started_at DESC`, envID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) <= keep {
		return nil
	}

	for _, runID := range ids[keep:] {
		// Remove the directory first: a row pointing at a deleted directory
		// is a broken UI entry, whereas a directory with no row is just
		// orphaned disk the user can clean.
		if run, err := s.Get(runID); err == nil {
			if dir, err := s.RunDir(run); err == nil {
				_ = os.RemoveAll(dir)
			}
		}
		if _, err := s.db.Exec(`DELETE FROM test_run_cases WHERE run_id = ?`, runID); err != nil {
			return err
		}
		if _, err := s.db.Exec(`DELETE FROM test_runs WHERE id = ?`, runID); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes a run, its cases and its evidence directory.
func (s *TestRunService) Delete(runID string) error {
	if run, err := s.Get(runID); err == nil {
		if dir, err := s.RunDir(run); err == nil {
			_ = os.RemoveAll(dir)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM test_run_cases WHERE run_id = ?`, runID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM test_runs WHERE id = ?`, runID)
	return err
}
