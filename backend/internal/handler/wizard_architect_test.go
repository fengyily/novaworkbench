package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// TestClearUnestablishedDesignSession pins the RC-2 guard semantics.
//
// Background (the regression this guards): prepareArchitectDesign pre-mints a
// design session id and persists it BEFORE spawning claude. If the run then
// fails without the CLI ever establishing that session, the stale id used to
// survive on the requirement row. The next run reads it back as
// req.DesignSessionID, which (a) skips the requirement-seeded prompt branch
// (`if skipAnalysis && sourceSID == ""`) so the architect prompt degrades to
// "基于我们刚才完成的需求分析对话…" with NO requirement content, and (b) sends
// `--resume <id>` against a conversation that does not exist — a permanent
// self-locking failure.
//
// The two early-return conditions are the whole safety story, so both are
// asserted explicitly: we must NOT clobber a real session.
func TestClearUnestablishedDesignSession(t *testing.T) {
	dir, _ := os.MkdirTemp("", "clear-design-sid-")
	defer os.RemoveAll(dir)
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	reqID := seedRequirementForSchedule(t, d)
	reqSvc := service.NewRequirementService(d, nil)
	h := &WizardHandler{reqSvc: reqSvc}

	const preexisting = "sid-preexisting-real-session"

	cases := []struct {
		name        string
		newDesignID string // designRunParams.NewDesignSID
		outSessID   string // claudeStreamOutcome.sessionID (from stream system/init)
		want        string
		why         string
	}{
		{
			name:        "no pre-mint this run -> leave row untouched",
			newDesignID: "",
			outSessID:   "",
			want:        preexisting,
			why:         "NewDesignSID empty means this run did not mint an id (pure resume path), nothing to roll back",
		},
		{
			name:        "CLI reported a session_id -> session was established, keep it",
			newDesignID: "sid-freshly-minted",
			outSessID:   "sid-freshly-minted",
			want:        preexisting,
			why:         "system/init arrived, so the conversation exists and must stay resumable",
		},
		{
			name:        "pre-minted but never established -> clear the stale id",
			newDesignID: "sid-freshly-minted",
			outSessID:   "",
			want:        "",
			why:         "this is the exact RC-2 poisoning case the fix targets",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Reset the row to a known non-empty state before each case so the
			// assertion can distinguish "left alone" from "cleared".
			if err := reqSvc.UpdateDesignSession(reqID, preexisting); err != nil {
				t.Fatalf("seed design_session_id: %v", err)
			}

			h.clearUnestablishedDesignSession(
				&designRunParams{NewDesignSID: c.newDesignID},
				claudeStreamOutcome{sessionID: c.outSessID},
				reqID,
			)

			got, err := reqSvc.Get(reqID)
			if err != nil {
				t.Fatalf("reload requirement: %v", err)
			}
			if got.DesignSessionID != c.want {
				t.Fatalf("DesignSessionID = %q, want %q (%s)", got.DesignSessionID, c.want, c.why)
			}
		})
	}
}

// TestPrepareDesignWorkspace_GateFailure verifies the hard-block contract
// when the design-stage gate fails: the job must be terminated as JobError,
// the design_job_id must be rolled back so a fresh run can mint a new one,
// and UpdateDesignBaseSHA must NOT have been called (a failed run never
// records a baseline — only the success path does).
//
// The original implementation hung the HTTP request for up to 60s before
// returning the worktree error, so the user had no visibility. The fix
// moves the worktree + sync logic into the goroutine and surfaces the
// failure through the SSE panel as a job error event. This test pins all
// three guarantees together so a future refactor can't silently regress any
// of them.
func TestPrepareDesignWorkspace_GateFailure(t *testing.T) {
	dir, _ := os.MkdirTemp("", "prep-design-fail-")
	defer os.RemoveAll(dir)
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	reqID := seedRequirementForSchedule(t, d)
	// Update the seeded project row in-place so its remote_url points at a
	// black hole and SyncDesignBase's fetch step fails fast. We can't
	// INSERT a new project row because the requirement's project_id already
	// references the seeded one — a second row would be orphaned.
	const projID = "proj_seed_sched" // matches seedRequirementForSchedule's project id
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGit(t, work, "git", "init", "--initial-branch=main")
	mustGit(t, work, "git", "config", "user.email", "test@nova")
	mustGit(t, work, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "git", "add", "f.txt")
	mustGit(t, work, "git", "commit", "-m", "init")
	mustGit(t, work, "git", "remote", "add", "origin", "https://127.0.0.1:1/dead.git")

	if _, err := d.Exec(
		`UPDATE projects SET local_path = ?, remote_url = ? WHERE id = ?`,
		work, "https://127.0.0.1:1/dead.git", projID); err != nil {
		t.Fatalf("update project: %v", err)
	}

	reqSvc := service.NewRequirementService(d, nil)
	projSvc := service.NewProjectService(d, nil, nil)
	settingSvc := service.NewSettingService(d)
	jobs := store.NewJobStore(8)

	req, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	// Pre-mint a design_job_id so we can verify the failure path rolls it
	// back. prepareDesignWorkspace is invoked AFTER prepareArchitectDesign
	// persists the job_id — the production sequence is job.Create →
	// UpdateDesignJob(job.ID) → return → goroutine → prepareDesignWorkspace.
	job := jobs.Create(reqID)
	if uerr := reqSvc.UpdateDesignJob(reqID, job.ID); uerr != nil {
		t.Fatalf("seed design_job_id: %v", uerr)
	}

	h := &WizardHandler{
		db:         d,
		projectSvc: projSvc,
		reqSvc:     reqSvc,
		settingSvc: settingSvc,
		jobs:       jobs,
	}

	p := &designRunParams{
		Req:         req,
		ProjectPath: work,
		BaseBranch:  "main",
		// SourceSID + SkipAnalysis both false → resume path; the prompt
		// branch matters here only in that it must NOT run on failure.
		SourceSID:    "somesid",
		SkipAnalysis: false,
	}

	ok := h.prepareDesignWorkspace(p, job)
	if ok {
		t.Fatalf("prepareDesignWorkspace must return false on gate failure")
	}
	if p.Prompt != "" {
		t.Fatalf("Prompt must remain empty on failure (got %d chars)", len(p.Prompt))
	}
	if p.WorkDir != "" {
		t.Fatalf("WorkDir must remain empty on failure, got %q", p.WorkDir)
	}

	// job must be terminated as JobError.
	lines, status, exitCode := job.Snapshot()
	if status != store.JobError {
		t.Fatalf("job status = %q, want %q (full snapshot: %+v)", status, store.JobError, lines)
	}
	if exitCode != 1 {
		t.Fatalf("job exitCode = %d, want 1", exitCode)
	}

	// design_job_id must be rolled back to "" so the next attempt can
	// mint a fresh one (mirrors the legacy early-return on wdErr).
	got, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	if got.DesignJobID != "" {
		t.Fatalf("DesignJobID = %q, want \"\" (rollback contract)", got.DesignJobID)
	}

	// design_base_sha must NOT be written on a failed run.
	if got.DesignBaseSHA != "" {
		t.Fatalf("DesignBaseSHA = %q, want \"\" on failure (no baseline on failed runs)", got.DesignBaseSHA)
	}

	// Job log must contain at least one error line carrying the SyncGate
	// message (or its raw stderr passthrough) so the SSE panel shows the
	// user what's wrong.
	var sawError bool
	for _, l := range lines {
		if l.Type == "error" {
			sawError = true
			break
		}
	}
	if !sawError {
		t.Fatalf("expected at least one error LogLine, got: %+v", lines)
	}
}

// TestPrepareDesignWorkspace_SuccessStampsBaseline verifies the success
// path: SyncDesignBase returns a real SHA, the design baseline is persisted
// onto the requirement (in the order required for crash safety — before
// claude spawn, even though we don't actually spawn here), and the SSE panel
// receives the "📌 设计基线已锁定: <7hex>" phase event. WorkDir + Prompt
// must also be populated for the goroutine to proceed.
func TestPrepareDesignWorkspace_SuccessStampsBaseline(t *testing.T) {
	dir, _ := os.MkdirTemp("", "prep-design-ok-")
	defer os.RemoveAll(dir)
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	reqID := seedRequirementForSchedule(t, d)
	// Update the seeded project row in-place (its local_path is /tmp/seed
	// which doesn't exist; point it at a real on-disk workdir so the gate
	// has something to sync).
	const projID = "proj_seed_sched" // matches seedRequirementForSchedule's project id

	// Set up a real upstream + cloned workdir. SyncDesignBase will run the
	// real gate, the anchorWorktree step will create a worktree under
	// ~/.novaworkbench/worktrees/<base>/<reqID>, and the SHA gets stamped.
	const baseBranch = "main"
	upstreamDir := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch="+baseBranch, upstreamDir)
	// seed upstream so the clone path finds HEAD.
	seed := filepath.Join(t.TempDir(), "seed")
	mustGit(t, "", "git", "clone", upstreamDir, seed)
	mustGit(t, seed, "git", "config", "user.email", "test@nova")
	mustGit(t, seed, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, seed, "git", "add", "README.md")
	mustGit(t, seed, "git", "commit", "-m", "init")
	mustGit(t, seed, "git", "push", "origin", "HEAD:"+baseBranch)

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "", "git", "clone", upstreamDir, work)
	mustGit(t, work, "git", "config", "user.email", "test@nova")
	mustGit(t, work, "git", "config", "user.name", "Test")

	if _, err := d.Exec(
		`UPDATE projects SET local_path = ?, remote_url = ? WHERE id = ?`,
		work, upstreamDir, projID); err != nil {
		t.Fatalf("update project: %v", err)
	}

	reqSvc := service.NewRequirementService(d, nil)
	projSvc := service.NewProjectService(d, nil, nil)
	settingSvc := service.NewSettingService(d)
	jobs := store.NewJobStore(8)

	req, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	job := jobs.Create(reqID)

	// Build a WizardHandler with the bare minimum fields prepareDesignWorkspace
	// touches. We don't construct via NewWizardHandler (that wires the full
	// production graph) — the existing TestClearUnestablishedDesignSession
	// already uses this same struct-literal pattern.
	h := &WizardHandler{
		db:         d,
		projectSvc: projSvc,
		reqSvc:     reqSvc,
		settingSvc: settingSvc,
		jobs:       jobs,
	}

	p := &designRunParams{
		Req:         req,
		ProjectPath: work,
		BaseBranch:  baseBranch,
		// resume path (analyst session present) — picks the simpler prompt
		// branch so the test doesn't need to fabricate a docBlock.
		SourceSID:    "analyst-sid",
		SkipAnalysis: false,
	}

	ok := h.prepareDesignWorkspace(p, job)
	if !ok {
		lines, status, _ := job.Snapshot()
		t.Fatalf("prepareDesignWorkspace must return true on success (status=%s, lines=%+v)", status, lines)
	}
	if p.WorkDir == "" {
		t.Fatalf("WorkDir must be set on success")
	}
	if p.Prompt == "" {
		t.Fatalf("Prompt must be set on success")
	}

	// design_base_sha must be persisted (a 40-hex SHA) before the goroutine
	// would have spawned claude — the merge stage reads this for the PR
	// body, so a missing stamp would silently drop the baseline.
	got, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	if got.DesignBaseSHA == "" {
		t.Fatalf("DesignBaseSHA must be non-empty on success")
	}
	if len(got.DesignBaseSHA) != 40 {
		t.Fatalf("DesignBaseSHA must be 40 hex chars, got %d (%q)", len(got.DesignBaseSHA), got.DesignBaseSHA)
	}

	// Job must have received the "📌 设计基线已锁定: <7hex>" phase event so
	// the user sees the locked baseline in the SSE panel.
	lines, _, _ := job.Snapshot()
	var sawPhase bool
	for _, l := range lines {
		if l.Type == "phase" && strings.HasPrefix(l.Content, "📌 设计基线已锁定: ") {
			sawPhase = true
			// 7-hex prefix must match the stamped SHA's first 7 chars.
			wantPrefix := got.DesignBaseSHA[:7]
			if !strings.HasPrefix(l.Content, "📌 设计基线已锁定: "+wantPrefix) {
				t.Fatalf("phase prefix mismatch: %q does not start with %q", l.Content, "📌 设计基线已锁定: "+wantPrefix)
			}
		}
	}
	if !sawPhase {
		t.Fatalf("expected 📌 设计基线已锁定 phase event, got: %+v", lines)
	}
}

// mustGit runs `<name> <args...>` with dir as the working directory and
// returns trimmed stdout. Failure is fatal to the test. Signature mirrors
// service/project_sync_test.go's mustGit so the two helpers stay swappable.
func mustGit(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", name, args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

// TestArchitectRemoteCallSitePassesTrueSourceSID is a source-level wiring
// guard for RC-1 — the actual root cause of "architect prompt has no
// requirement content".
//
// The defect was a single wrong field VALUE in a struct literal: the remote
// call site passed `sourceSID: sessionArg` instead of `sourceSID: sourceSID`.
// Since sessionArg is reassigned to the freshly-minted id on the fresh path,
// prepareRemoteAgentRun's `Resume: in.sourceSID != ""` became true even for a
// brand-new conversation, so the run sent `--resume <new-id>` (a conversation
// that does not exist) instead of `--session-id <new-id>`. Combined with the
// pre-minted id being persisted, that poisoned the requirement permanently.
//
// There is no runtime harness for this in unit tests (it needs a real Agent
// server + claude CLI), and the failure mode is invisible to the compiler
// because both fields are strings. So we assert the wiring directly against
// the AST. The dev-stage counterpart this must mirror is the runRemoteCoding
// call site in wizard_coding.go.
//
// If this test fails after a refactor: check that the remoteRunInput literal
// newRegenerateTestEnv sets up the minimum fixture for a prepareArchitectDesign
// call: a sqlite DB with the shared schema + seeded project, the requirement
// service, the design-revision service, a project service (mandatory because
// prepareArchitectDesign calls h.projectSvc.Get unconditionally), and a
// JobStore. Returning the env rather than just the handler keeps each test
// from re-running db.Init / InitDB.
func newRegenerateTestEnv(t *testing.T) (*db.DB, string, *WizardHandler, *service.RequirementService) {
	t.Helper()
	dir, _ := os.MkdirTemp("", "regen-")
	t.Cleanup(func() { os.RemoveAll(dir) })
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reqID := seedRequirementForSchedule(t, d)
	reqSvc := service.NewRequirementService(d, nil)
	projSvc := service.NewProjectService(d, nil, nil)
	jobs := store.NewJobStore(8)
	designRevSvc := service.NewDesignRevisionService(d)
	h := &WizardHandler{
		db:           d,
		reqSvc:       reqSvc,
		projectSvc:   projSvc,
		jobs:         jobs,
		designRevSvc: designRevSvc,
	}
	return d, reqID, h, reqSvc
}

// TestRegenerateForksFromAnalystSession pins the core regenerate
// session decision: when regenerate=true and BOTH design_session_id and
// analysis_session_id are present, the run must re-fork from the analyst
// session (NOT resume the old design session). This is the
// req_11e485759f583db8 fix's heart — without this switch the
// regenerated plan would just edit the prior session, which is what
// the existing 「重新分析生成」 CTA does today and the reason it was
// almost-but-not-quite-right.
//
// Status-whitelist coverage lives in TestRegenerateRejectsWrongStatus
// (separate test for a separate concern).
func TestRegenerateForksFromAnalystSession(t *testing.T) {
	d, reqID, h, reqSvc := newRegenerateTestEnv(t)
	if err := reqSvc.UpdateDesignSession(reqID, "sid-old-design"); err != nil {
		t.Fatalf("seed design_session_id: %v", err)
	}
	if err := reqSvc.UpdateAnalysisSession(reqID, "sid-analyst"); err != nil {
		t.Fatalf("seed analysis_session_id: %v", err)
	}
	if _, err := d.Exec(`UPDATE requirements SET status='designed', design_docs='old plan', design_revision_no=2 WHERE id=?`, reqID); err != nil {
		t.Fatalf("seed status/design_docs/revision_no: %v", err)
	}

	// Drive the synchronous portion of prepareArchitectDesign. The call
	// is expected to surface a downstream NON-nil *apiFailure (no real
	// git worktree / claude invocation), but the session decision +
	// Snapshot must land BEFORE that error. The state lives in the DB +
	// service so the assertions below are still meaningful even when
	// af != nil.
	_, _, _ = h.prepareArchitectDesign(
		context.Background(), reqID, "", "", "", false, "", "", true, false,
	)

	// design_revisions must contain ONE new row (frozen from the prior
	// design); rev_no must be max(rev_no)+1 = 1 since the seeded
	// design_revision_no=2 has no actual v2 row in design_revisions.
	// design_revision_no on the requirement gets stamped back to that
	// max+1 value (the next revision number) — kept equal to the
	// newest revision row's rev_no (the Snapshot stamp semantics is
	// intentionally identical to ensure the version dropdown can label
	// the current version exactly).
	var nRev int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM design_revisions WHERE requirement_id = ?`, reqID,
	).Scan(&nRev); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if nRev != 1 {
		t.Fatalf("design_revisions count = %d, want 1 (regenerate must Snapshot before run)", nRev)
	}
	var trigger string
	var revNo int
	if err := d.QueryRow(
		`SELECT trigger_kind, rev_no FROM design_revisions WHERE requirement_id = ? ORDER BY rev_no DESC LIMIT 1`, reqID,
	).Scan(&trigger, &revNo); err != nil {
		t.Fatalf("read revision row: %v", err)
	}
	if trigger != model.RevisionTriggerRegenerate {
		t.Fatalf("trigger_kind = %q, want %q", trigger, model.RevisionTriggerRegenerate)
	}
	if revNo != 1 {
		t.Fatalf("rev_no = %d, want 1 (max+1 of empty revisions table)", revNo)
	}

	// design_revision_no must be bumped to match the new revision row.
	got, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	if got.DesignRevisionNo != 1 {
		t.Fatalf("design_revision_no = %d, want 1", got.DesignRevisionNo)
	}

	// Critical invariant: design_session_id must remain the OLD value
	// (regenerate pre-mints in memory only — the old session is what
	// the user's doc-refine chat keeps resuming until finalize stamps
	// out.sessionID on success).
	if got.DesignSessionID != "sid-old-design" {
		t.Fatalf("design_session_id = %q, want %q (regenerate must NOT clobber the old session until success)",
			got.DesignSessionID, "sid-old-design")
	}
}

// TestNonRegenerateStillResumesDesignSession is the regression pin for
// R1: the regenerate gate must NOT affect the non-regenerate path. A
// pre-existing design_session_id plus regenerate=false must still
// resume that session. Without this guard a future refactor of the
// switch-statement in prepareArchitectDesign could silently flip every
// first-run / re-run into a hard "re-fork analyst" — breaking the
// doc-refine chat continuity that has been the design stage's main UX
// promise since launch.
func TestNonRegenerateStillResumesDesignSession(t *testing.T) {
	d, reqID, h, reqSvc := newRegenerateTestEnv(t)
	if err := reqSvc.UpdateDesignSession(reqID, "sid-existing"); err != nil {
		t.Fatalf("seed design_session_id: %v", err)
	}
	// Seed the analysis session so the test isn't short-circuited on
	// the NO_SESSION 400 that fires when both sourceSID and an analyst
	// session are missing on the non-skip-analysis path.
	if err := reqSvc.UpdateAnalysisSession(reqID, "sid-analyst"); err != nil {
		t.Fatalf("seed analysis_session_id: %v", err)
	}
	if _, err := d.Exec(`UPDATE requirements SET status='designing', design_docs='existing plan' WHERE id=?`, reqID); err != nil {
		t.Fatalf("seed status/design_docs: %v", err)
	}

	// The non-regenerate contract: an existing design_session_id is
	// resumed, no fork, no pre-mint, no Snapshot. design_docs is left
	// untouched (the architect hasn't even run yet — the test fails at
	// the worktree sync step, not at the design persistence step).
	_, _, _ = h.prepareArchitectDesign(
		context.Background(), reqID, "", "", "", false, "", "", false, false,
	)

	var nRev int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM design_revisions WHERE requirement_id = ?`, reqID,
	).Scan(&nRev); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if nRev != 0 {
		t.Fatalf("design_revisions count = %d, want 0 on non-regenerate path", nRev)
	}
	got, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	// Critical: design_session_id MUST remain "sid-existing" — the
	// non-regenerate resume path stays on the same session. The
	// regenerate path is the one that widens the decision. Without this
	// assertion a future edit could flip the switch-cases around and
	// silently switch every re-run into a hard "re-fork analyst",
	// breaking the doc-refine chat continuity that has been the
	// design stage's main UX promise since launch.
	if got.DesignSessionID != "sid-existing" {
		t.Fatalf("design_session_id = %q, want %q (non-regenerate resume path must NOT touch the existing session)",
			got.DesignSessionID, "sid-existing")
	}
}

// TestRegenerateRejectsWrongStatus pins the status whitelist around
// the regenerate field. A regenerate=true on anything outside
// {designing, designed, developing} returns 409 INVALID_STATUS. Server-side
// mirror of the front-end's button guard: if a future UI bug lets a
// regenerate CTA through on `draft` / `done`, the backend still rejects so
// the requirement isn't yanked out of a finished state.
//
// `developing` moved INTO the allow-list: the user reaching for a second
// plan after design has already been handed off to development is the
// primary "方案设计完成后仍需重新设计" case. The run lands back on
// 'designed' (developing→designed is a legal transition), so the new
// plan is re-approved before development continues.
//
// The allowed statuses are pinned by TestRegenerateAllowsPostDesignStatus
// (which actually drives the full Snapshot path on a fresh fixture).
// Splitting avoids cross-subtest pollution from the in-memory JobStore
// (a successful allowed run mints a live design_job_id that subsequent
// subtests would trip the 409 DESIGN_JOB_ACTIVE guard on).
func TestRegenerateRejectsWrongStatus(t *testing.T) {
	_, reqID, h, _ := newRegenerateTestEnv(t)

	cases := []struct {
		status string
		want   bool // true = expect 409 reject
	}{
		{"draft", true},
		{"analyzing", true},
		{"done", true},
		{"archived", true},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			if _, err := h.db.Exec(`UPDATE requirements SET status=? WHERE id=?`, c.status, reqID); err != nil {
				t.Fatalf("seed status: %v", err)
			}
			_, _, af := h.prepareArchitectDesign(
				context.Background(), reqID, "", "", "", false, "", "", true, false,
			)
			got := af != nil
			if got != c.want {
				t.Fatalf("regenerate on status=%q: rejected=%v, want %v (err=%v)",
					c.status, got, c.want, af)
			}
			if !got {
				t.Fatalf("regenerate on status=%q unexpectedly accepted", c.status)
			}
		})
	}
}

// TestRegenerateAllowsPostDesignStatus pins the allow-list half of the
// whitelist: status in {designing, designed, developing} lets the call
// pass the guard. We don't drive the full run (no real worktree / git
// fixture), just assert that no INVALID_STATUS failure surfaces and the
// live-job guard short-circuits the rest of the synchronous path with a
// downstream DESIGN_JOB_ACTIVE / worktree-related error rather than an
// upstream whitelist one.
func TestRegenerateAllowsPostDesignStatus(t *testing.T) {
	for _, status := range []string{"designing", "designed", "developing"} {
		t.Run(status, func(t *testing.T) {
			d, reqID, h, _ := newRegenerateTestEnv(t)
			if _, err := d.Exec(`UPDATE requirements SET status=?, design_docs='' WHERE id=?`, status, reqID); err != nil {
				t.Fatalf("seed status: %v", err)
			}
			_, _, af := h.prepareArchitectDesign(
				context.Background(), reqID, "", "", "", false, "", "", true, false,
			)
			if af != nil && af.Code == "INVALID_STATUS" {
				t.Fatalf("regenerate on allowed status=%q must NOT short-circuit on whitelist: %v", status, af)
			}
			// design_docs was empty so Snapshot is a no-op — count stays 0.
			var n int
			if err := d.QueryRow(`SELECT COUNT(*) FROM design_revisions WHERE requirement_id=?`, reqID).Scan(&n); err != nil {
				t.Fatalf("count revisions: %v", err)
			}
			if n != 0 {
				t.Fatalf("design_revisions count = %d, want 0 (no plan to freeze on empty design_docs)", n)
			}
		})
	}
}

// TestRegenerateFailurePreservesOldDesignSession pins the
// 「成功才落库」 contract on the regenerate path by directly exercising
// finalizeArchitectRun. After a successful run, design_session_id
// becomes the CLI-reported id; after a CLI-couldn't-emit-system/init
// run, design_session_id stays at the OLD value. The counterpart to
// TestArchitectRemoteMissingSessionIDClearsDesignSession — that test
// pins the NON-regenerate path's "drop the dangling pre-mint"
// behaviour, this one pins the regenerate path's "leave the user's
// old session alone".
func TestRegenerateFailurePreservesOldDesignSession(t *testing.T) {
	_, reqID, h, reqSvc := newRegenerateTestEnv(t)
	const oldSID = "sid-user-keep"
	if err := reqSvc.UpdateDesignSession(reqID, oldSID); err != nil {
		t.Fatalf("seed design_session_id: %v", err)
	}

	jobs := store.NewJobStore(8)
	job := jobs.Create(reqID)
	_ = h
	// Re-grab a handler that has its jobs wired (newRegenerateTestEnv's
	// is sufficient for finalize, but the standalone test rebuilds to
	// avoid coupling).
	h2 := &WizardHandler{reqSvc: reqSvc, jobs: jobs, designRevSvc: h.designRevSvc}

	req, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	p := &designRunParams{
		Req:            req,
		Regenerate:     true,
		NewDesignSID:   "sid-new-inmemory",
		Fork:           true,
		SkipAnalysis:   false,
		ClaudeConfigID: "",
		Model:          "sonnet",
	}

	// Triple-zero shape: no session_id (CLI crashed before system/init),
	// no error, no stale flag. Non-regenerate path clears the dangling
	// pre-mint; regenerate path leaves the user's old id intact.
	h2.finalizeArchitectRun(
		claudeStreamOutcome{sessionID: "", staleSession: false, errMsg: ""},
		p, job, nil,
		"sid-analyst-source", "sid-new-inmemory", reqID, "sonnet",
	)

	got, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	if got.DesignSessionID != oldSID {
		t.Fatalf("design_session_id = %q, want %q (regenerate failure must preserve the user's old session)",
			got.DesignSessionID, oldSID)
	}
	if got.DesignJobID != "" {
		t.Fatalf("design_job_id = %q, want \"\"", got.DesignJobID)
	}
}

// TestArchitectDesignRejectsLiveApplyJob pins the second half of the
// concurrency guard: a regenerate attempt must be rejected with 409
// when EITHER a live design job OR a live apply-doc job holds the
// design_docs column. The design-only guard is already covered by
// existing tests; the apply-doc gap was a known omission in
// wizard_immediate.go's preGate that this fix closes at the
// architect-design entry.
func TestArchitectDesignRejectsLiveApplyJob(t *testing.T) {
	d, reqID, h, reqSvc := newRegenerateTestEnv(t)
	if _, err := d.Exec(`UPDATE requirements SET status='designed', design_docs='existing' WHERE id=?`, reqID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	live := h.jobs.Create(reqID) // a live apply-job pointer
	if uerr := reqSvc.UpdateApplyJob(reqID, live.ID); uerr != nil {
		t.Fatalf("seed apply_job_id: %v", uerr)
	}

	_, _, af := h.prepareArchitectDesign(
		context.Background(), reqID, "", "", "", false, "", "", true, false,
	)
	if af == nil {
		t.Fatalf("prepareArchitectDesign accepted regenerate while apply_job alive — must reject")
	}
	if af.Code != "DESIGN_JOB_ACTIVE" {
		t.Fatalf("error code = %q, want DESIGN_JOB_ACTIVE", af.Code)
	}

	// Clear the live pointer; downstream failure (no worktree) is fine,
	// but DESIGN_JOB_ACTIVE must no longer appear.
	if uerr := reqSvc.UpdateApplyJob(reqID, ""); uerr != nil {
		t.Fatalf("clear apply_job_id: %v", uerr)
	}
	_, _, af2 := h.prepareArchitectDesign(
		context.Background(), reqID, "", "", "", false, "", "", true, false,
	)
	if af2 != nil && af2.Code == "DESIGN_JOB_ACTIVE" {
		t.Fatalf("cleared apply_job still triggers DESIGN_JOB_ACTIVE: %v", af2)
	}
}

// TestArchitectDesignRejectsLiveCodingJob pins the third concurrency
// guard: now that a regenerate is allowed from 'developing', it must not
// race a coding run that is still streaming. The coding job forked from
// the CURRENT plan, so rewriting design_docs / design_session_id
// underneath it leaves both halves describing different requirements.
func TestArchitectDesignRejectsLiveCodingJob(t *testing.T) {
	d, reqID, h, reqSvc := newRegenerateTestEnv(t)
	if _, err := d.Exec(`UPDATE requirements SET status='developing', design_docs='existing' WHERE id=?`, reqID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	live := h.jobs.Create(reqID) // a live coding-job pointer
	if uerr := reqSvc.UpdateCodingJob(reqID, live.ID); uerr != nil {
		t.Fatalf("seed coding_job_id: %v", uerr)
	}

	_, _, af := h.prepareArchitectDesign(
		context.Background(), reqID, "", "", "", false, "", "", true, false,
	)
	if af == nil {
		t.Fatalf("prepareArchitectDesign accepted regenerate while coding job alive — must reject")
	}
	if af.Code != "DESIGN_JOB_ACTIVE" {
		t.Fatalf("error code = %q, want DESIGN_JOB_ACTIVE", af.Code)
	}

	// Clear the live pointer; downstream failure (no worktree) is fine,
	// but DESIGN_JOB_ACTIVE must no longer appear.
	if uerr := reqSvc.UpdateCodingJob(reqID, ""); uerr != nil {
		t.Fatalf("clear coding_job_id: %v", uerr)
	}
	_, _, af2 := h.prepareArchitectDesign(
		context.Background(), reqID, "", "", "", false, "", "", true, false,
	)
	if af2 != nil && af2.Code == "DESIGN_JOB_ACTIVE" {
		t.Fatalf("cleared coding_job still triggers DESIGN_JOB_ACTIVE: %v", af2)
	}
}

// in execArchitectDesign still forwards the *true* source session (the value
// that is "" on a fresh/skip-analysis run) into the `sourceSID` field, and
// keeps `sessionArg` as its own separate field.
func TestArchitectRemoteCallSitePassesTrueSourceSID(t *testing.T) {
	const srcFile = "wizard_architect.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, srcFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcFile, err)
	}

	// Collect every `remoteRunInput{...}` composite literal and its field
	// assignments.
	type literal struct {
		pos    token.Position
		fields map[string]string // field name -> rendered value expression
	}
	var literals []literal

	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		ident, ok := cl.Type.(*ast.Ident)
		if !ok || ident.Name != "remoteRunInput" {
			return true
		}
		l := literal{pos: fset.Position(cl.Pos()), fields: map[string]string{}}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			l.fields[key.Name] = renderExpr(kv.Value)
		}
		literals = append(literals, l)
		return true
	})

	if len(literals) == 0 {
		t.Fatalf("no remoteRunInput composite literal found in %s — "+
			"if execArchitectDesign was refactored, update this guard to match", srcFile)
	}

	for _, l := range literals {
		got, present := l.fields["sourceSID"]
		if !present {
			t.Fatalf("%s: remoteRunInput literal at %s has no sourceSID field — "+
				"the resume decision depends on it", srcFile, l.pos)
		}
		if got != "sourceSID" {
			t.Fatalf("%s: remoteRunInput{sourceSID: %s} at %s — RC-1 regression.\n"+
				"sourceSID must be the TRUE source session (empty on a fresh/skip-analysis run); "+
				"passing sessionArg makes fresh runs emit `--resume <new-id>` against a "+
				"non-existent conversation. Mirror wizard_coding.go's runRemoteCoding call site.",
				srcFile, got, l.pos)
		}
		// sessionArg must remain its own field — collapsing the two is the bug.
		if sa, ok := l.fields["sessionArg"]; ok && sa != "sessionArg" {
			t.Fatalf("%s: remoteRunInput{sessionArg: %s} at %s — sessionArg must stay "+
				"the separately-threaded session id", srcFile, sa, l.pos)
		}
	}
}

// renderExpr produces a stable, comparable rendering for the small set of
// expressions used as field values in the literals we inspect (identifiers,
// selector expressions, basic literals). Returns a descriptive placeholder for
// anything else so the assertion above fails loudly rather than silently
// matching.
func renderExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
		return "<selector>"
	case *ast.BasicLit:
		return v.Value
	case *ast.UnaryExpr:
		return v.Op.String() + renderExpr(v.X)
	case *ast.CallExpr:
		return "<call>"
	}
	return "<expr>"
}

// silence unused-import warnings if a future refactor drops one of these.
var _ = context.Background
var _ = errors.New
var _ = model.Project{}

// TestArchitectRemoteMissingSessionIDClearsDesignSession pins the
// "remote CLI exited before emitting system/init" cleanup contract.
//
// Defect story (req_d6947f626957b7c4): prepareArchitectDesign pre-mints a
// design session id and persists it BEFORE spawning claude (so a mid-run
// crash can be resumed). When the agent-server run ends with
// out.sessionID == "" (CLI exited before system/init — e.g. upstream 5xx,
// ssh jitter, worker timeout) but out.staleSession == false and
// out.errMsg == "" (no explicit "No conversation found" yet because the
// run was silently aborted), the pre-minted id used to survive on the row.
// The next run then read it back as req.DesignSessionID and either:
//   - treated it as a fresh resume source and degraded the prompt to
//     "基于我们刚才完成的需求分析对话…" with no requirement content (when
//     sourceSID got reused), or
//   - sent `--resume <id>` against a conversation that never existed and
//     immediately hit `isStaleSessionError` on the next iteration, which
//     finalizeArchitectRun then surfaced as "技术方案会话已过期。请重新
//     生成技术方案。"
//
// The corrected-write block in finalizeArchitectRun must drop the dangling
// pre-minted id (UpdateDesignSession(id, "")) so the next attempt mints
// a fresh one and won't stale on first contact. We assert that contract
// here. (The job still routes through the planMarkdown=="" branch and is
// marked JobError with "Claude 未返回结果" — that's correct: Claude really
// did fail to produce a plan. The important invariant is that the next
// run starts from a clean slate rather than immediately re-hitting the
// stale branch.)
func TestArchitectRemoteMissingSessionIDClearsDesignSession(t *testing.T) {
	dir, _ := os.MkdirTemp("", "remote-missingsid-")
	defer os.RemoveAll(dir)
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	reqID := seedRequirementForSchedule(t, d)
	reqSvc := service.NewRequirementService(d, nil)
	if err := reqSvc.UpdateDesignSession(reqID, "sid-pre-minted"); err != nil {
		t.Fatalf("seed design_session_id: %v", err)
	}

	jobs := store.NewJobStore(8)
	job := jobs.Create(reqID)
	// Pin a design_job_id so we can assert finalizeArchitectRun clears it.
	if uerr := reqSvc.UpdateDesignJob(reqID, job.ID); uerr != nil {
		t.Fatalf("seed design_job_id: %v", uerr)
	}

	h := &WizardHandler{reqSvc: reqSvc, jobs: jobs}

	req, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	p := &designRunParams{
		Req:            req,
		NewDesignSID:   "sid-pre-minted", // exactly the row currently on disk
		SourceSID:      "sid-pre-minted", // simulating a re-run that resumed it
		Fork:           false,
		SkipAnalysis:   false,
		ClaudeConfigID: "",
		Model:          "sonnet",
	}

	// Remote-CLI-silent-exit shape: no session_id, no error string, no
	// stale flag (because the CLI never reached the `--resume` failure
	// point — it just exited silently upstream).
	h.finalizeArchitectRun(
		claudeStreamOutcome{sessionID: "", staleSession: false, errMsg: ""},
		p, job, nil,
		"", "sid-pre-minted", reqID, "sonnet",
	)

	// design_session_id must be cleared so the next run mints a fresh id.
	got, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	if got.DesignSessionID != "" {
		t.Fatalf("DesignSessionID = %q, want \"\" (dangling pre-mint must be cleared)", got.DesignSessionID)
	}
	// design_job_id must also be cleared on the terminal-error path.
	if got.DesignJobID != "" {
		t.Fatalf("DesignJobID = %q, want \"\" (terminal-error path)", got.DesignJobID)
	}
}

// TestArchitectRemoteStaleSessionHasSideTargetedMessage pins the new
// 3-way diagnostic split introduced for req_d6947f626957b7c4.
//
// Defect story: when prepareRemoteAgentRun stamped out.staleSession=true
// and out.SessionFileMissingSide="remote", the architect handler used to
// surface a generic "技术方案会话已过期" — same wording as the local-CLI
// path — and the user had no idea whether the problem was a missing
// jsonl on the agent host, a missing local jsonl, or an SFTP failure.
// The fix branches on SessionFileMissingSide and emits a side-specific
// hint. This test asserts the three side-specific messages land in the
// job log and that the design_session_id is cleared in every branch so
// the next attempt mints a fresh id.
func TestArchitectRemoteStaleSessionHasSideTargetedMessage(t *testing.T) {
	cases := []struct {
		side     string
		contains string
	}{
		{"remote", "远端 Agent Server 上未找到会话文件"},
		{"local", "本地会话文件缺失，无法继续"},
		{"sync-failed", "会话上行同步失败（SFTP/网络）"},
	}
	for _, c := range cases {
		t.Run(c.side, func(t *testing.T) {
			dir, _ := os.MkdirTemp("", "remote-stale-")
			defer os.RemoveAll(dir)
			d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
			if err != nil {
				t.Fatalf("init db: %v", err)
			}
			defer d.Close()

			reqID := seedRequirementForSchedule(t, d)
			reqSvc := service.NewRequirementService(d, nil)
			if err := reqSvc.UpdateDesignSession(reqID, "sid-stale"); err != nil {
				t.Fatalf("seed design_session_id: %v", err)
			}

			jobs := store.NewJobStore(8)
			job := jobs.Create(reqID)

			h := &WizardHandler{reqSvc: reqSvc, jobs: jobs}

			req, err := reqSvc.Get(reqID)
			if err != nil {
				t.Fatalf("reload req: %v", err)
			}
			p := &designRunParams{
				Req:          req,
				NewDesignSID: "sid-stale",
				SourceSID:    "sid-stale", // simulating a re-run that resumed the stale id
				Fork:         false,
				SkipAnalysis: true,
				Model:        "sonnet",
			}

			h.finalizeArchitectRun(
				claudeStreamOutcome{
					sessionID:              "sid-stale",
					staleSession:           true,
					errMsg:                 "",
					SessionFileMissingSide: c.side,
				},
				p, job, nil,
				"sid-stale", "sid-stale", reqID, "sonnet",
			)

			got, err := reqSvc.Get(reqID)
			if err != nil {
				t.Fatalf("reload req: %v", err)
			}
			if got.DesignSessionID != "" {
				t.Fatalf("DesignSessionID = %q, want \"\" on stale (must mint fresh next run)", got.DesignSessionID)
			}
			if got.DesignJobID != "" {
				t.Fatalf("DesignJobID = %q, want \"\" on terminal error", got.DesignJobID)
			}

			lines, status, _ := job.Snapshot()
			if status != store.JobError {
				t.Fatalf("status = %q, want JobError", status)
			}
			var found bool
			for _, l := range lines {
				if l.Type == "error" && strings.Contains(l.Content, c.contains) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected error line containing %q, got lines: %+v", c.contains, lines)
			}
		})
	}
}

// TestArchitectRemoteStaleSessionLocalSideStillReportsMissing is a
// regression pin for req_d92d397bb4ae6286.
//
// Background: when the analyst runs locally (not on the agent host) and
// then the user triggers architect-design on an agent server, the
// up-sync pre-flight in prepareRemoteAgentRun may classify
// SessionFileMissingSide as "local" (e.g. analyst jsonl was in a slug
// dir that claudeProjectsSlugDir didn't probe). The fallback in
// syncRequirementSessionsUp is the primary fix; this test pins that
// IF the fallback also fails to locate the file AND the remote still
// reports staleSession=true, finalizeArchitectRun must continue to
// surface the "本地会话文件缺失，无法继续" hint and clear
// design_session_id so the next attempt mints a fresh id. Without
// this pin, a refactor of the "local" branch could silently swallow
// the error and leave the user staring at a generic "default" message.
func TestArchitectRemoteStaleSessionLocalSideStillReportsMissing(t *testing.T) {
	dir, _ := os.MkdirTemp("", "remote-stale-local-")
	defer os.RemoveAll(dir)
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	reqID := seedRequirementForSchedule(t, d)
	reqSvc := service.NewRequirementService(d, nil)
	if err := reqSvc.UpdateDesignSession(reqID, "sid-stale-local"); err != nil {
		t.Fatalf("seed design_session_id: %v", err)
	}

	jobs := store.NewJobStore(8)
	job := jobs.Create(reqID)

	h := &WizardHandler{reqSvc: reqSvc, jobs: jobs}

	req, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	p := &designRunParams{
		Req:          req,
		NewDesignSID: "sid-stale-local",
		SourceSID:    "sid-stale-local", // design-session branch (Fork=false): SessionFileMissingSide switch applies
		Fork:         false,
		SkipAnalysis: true,
		Model:        "sonnet",
	}

	h.finalizeArchitectRun(
		claudeStreamOutcome{
			sessionID:              "sid-stale-local",
			staleSession:           true,
			errMsg:                 "",
			SessionFileMissingSide: "local",
		},
		p, job, nil,
		"sid-stale-local", "sid-stale-local", reqID, "sonnet",
	)

	got, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	if got.DesignSessionID != "" {
		t.Fatalf("DesignSessionID = %q, want \"\" on stale local (must mint fresh next run)", got.DesignSessionID)
	}
	if got.DesignJobID != "" {
		t.Fatalf("DesignJobID = %q, want \"\" on terminal error", got.DesignJobID)
	}

	lines, status, _ := job.Snapshot()
	if status != store.JobError {
		t.Fatalf("status = %q, want JobError", status)
	}
	const wantSubstring = "本地会话文件缺失"
	var found bool
	for _, l := range lines {
		if l.Type == "error" && strings.Contains(l.Content, wantSubstring) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected error line containing %q, got lines: %+v", wantSubstring, lines)
	}
}

// import-designTestEnv is the minimal fixture shared by every TestImportDesign_*
// case. It mirrors TestClearUnestablishedDesignSession's setup: a tempdir
// SQLite, the shared seedRequirementForSchedule helper, and a WizardHandler
// constructed with only reqSvc (the import path does not touch jobs / llm /
// project / claude config). Returning the env lets each test mutate the
// seeded requirement row (e.g. flip kind to wiki) before issuing the HTTP
// call, without rebuilding the whole fixture.
type importDesignTestEnv struct {
	dir    string
	d      *db.DB
	reqID  string
	reqSvc *service.RequirementService
	h      *WizardHandler
}

func newImportDesignTestEnv(t *testing.T) *importDesignTestEnv {
	t.Helper()
	dir, _ := os.MkdirTemp("", "import-design-")
	t.Cleanup(func() { os.RemoveAll(dir) })
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reqID := seedRequirementForSchedule(t, d)
	reqSvc := service.NewRequirementService(d, nil)
	return &importDesignTestEnv{
		dir:    dir,
		d:      d,
		reqID:  reqID,
		reqSvc: reqSvc,
		h:      &WizardHandler{reqSvc: reqSvc},
	}
}

// decodeEnvelope unmarshals an httptest recorder's body into the standard
// APIResponse envelope. Returns the data as a raw map (so the test can
// inspect the field-shape without coupling to the Requirement struct's full
// surface — keeps the test resilient to harmless downstream additions like
// extra nullable columns).
func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) APIResponse {
	t.Helper()
	var env APIResponse
	if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v (body=%q)", err, w.Body.String())
	}
	return env
}

// putImportBody is a tiny helper that builds a PUT request with a JSON
// body, matching the contract of WizardHandler.ImportDesign.
func putImportBody(t *testing.T, body interface{}) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return httptest.NewRequest(http.MethodPut, "/api/wizard/import-design", bytes.NewReader(raw))
}

// TestImportDesign_HappyPath — case 1 of the documented 7 cases.
//
// Paste a Markdown plan that includes a mermaid block, assert the response
// is a freshly-loaded Requirement with status="designed", design_docs
// containing the input, and the three Claude session/job columns cleared
// (the import path is terminal — it must not leave stale --resume targets
// that a later refine-doc would try to attach to).
func TestImportDesign_HappyPath(t *testing.T) {
	env := newImportDesignTestEnv(t)

	const plan = "# 方案\n\n## 整体思路\n\n```mermaid\nflowchart LR\nA-->B\n```\n\n## 步骤\n\n```bash\nls -la\n```\n"
	rec := httptest.NewRecorder()
	env.h.ImportDesign(rec, putImportBody(t, map[string]string{
		"requirement_id": env.reqID,
		"content":        plan,
		"source":         "paste",
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeEnvelope(t, rec)
	if !got.Success {
		t.Fatalf("envelope success = false; error = %+v", got.Error)
	}
	raw, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatalf("re-marshal data: %v", err)
	}
	var updated model.Requirement
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal data as Requirement: %v", err)
	}
	if updated.Status != "designed" {
		t.Errorf("Status = %q, want \"designed\"", updated.Status)
	}
	if !strings.Contains(updated.DesignDocs, "flowchart LR") {
		t.Errorf("DesignDocs missing mermaid body; got: %q", updated.DesignDocs)
	}
	if !strings.Contains(updated.DesignDocs, "# 方案") {
		t.Errorf("DesignDocs missing # 方案; got: %q", updated.DesignDocs)
	}
	if !strings.Contains(updated.DesignDocs, "ls -la") {
		t.Errorf("DesignDocs missing bash body; got: %q", updated.DesignDocs)
	}
	if updated.DesignSessionID != "" {
		t.Errorf("DesignSessionID = %q, want \"\"", updated.DesignSessionID)
	}
	if updated.DesignJobID != "" {
		t.Errorf("DesignJobID = %q, want \"\"", updated.DesignJobID)
	}
	if updated.ApplyJobID != "" {
		t.Errorf("ApplyJobID = %q, want \"\"", updated.ApplyJobID)
	}
	if updated.AnalysisEndedAt == nil {
		t.Errorf("AnalysisEndedAt = nil, want non-nil (import path stamps it via COALESCE)")
	}
}

// TestImportDesign_RejectsWikiKind — case 2.
//
// A wiki-kind requirement owns wiki_docs (not design_docs). Rejecting the
// import at the handler boundary means the frontend's {kind !== 'wiki'}
// guard is backed by a real 400 instead of silently writing to the wrong
// column.
func TestImportDesign_RejectsWikiKind(t *testing.T) {
	env := newImportDesignTestEnv(t)
	if _, err := env.d.Exec("UPDATE requirements SET kind='wiki' WHERE id=?", env.reqID); err != nil {
		t.Fatalf("seed wiki kind: %v", err)
	}

	rec := httptest.NewRecorder()
	env.h.ImportDesign(rec, putImportBody(t, map[string]string{
		"requirement_id": env.reqID,
		"content":        "# wiki plan",
		"source":         "paste",
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	env2 := decodeEnvelope(t, rec)
	if env2.Error == nil || env2.Error.Code != "KIND_NOT_SUPPORTED" {
		t.Fatalf("error = %+v, want code=KIND_NOT_SUPPORTED", env2.Error)
	}
	// design_docs must be untouched (still the default '[]' from schema).
	got, _ := env.reqSvc.Get(env.reqID)
	if got.DesignDocs != "[]" {
		t.Errorf("DesignDocs = %q, want \"[]\" (untouched)", got.DesignDocs)
	}
}

// TestImportDesign_RejectsBusyJob — case 3.
//
// When Claude is mid-run, design_job_id / apply_job_id are non-empty. We
// refuse the import rather than race a second writer on the same
// design_docs column. Asserting the persisted row is unchanged (still
// status=draft, design_docs='[]') confirms we did not half-write before
// detecting the conflict.
func TestImportDesign_RejectsBusyJob(t *testing.T) {
	env := newImportDesignTestEnv(t)
	if _, err := env.d.Exec("UPDATE requirements SET design_job_id='job_in_flight' WHERE id=?", env.reqID); err != nil {
		t.Fatalf("seed design_job_id: %v", err)
	}

	rec := httptest.NewRecorder()
	env.h.ImportDesign(rec, putImportBody(t, map[string]string{
		"requirement_id": env.reqID,
		"content":        "# plan",
		"source":         "paste",
	}))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	env2 := decodeEnvelope(t, rec)
	if env2.Error == nil || env2.Error.Code != "DESIGN_BUSY" {
		t.Fatalf("error = %+v, want code=DESIGN_BUSY", env2.Error)
	}
	got, _ := env.reqSvc.Get(env.reqID)
	if got.Status != "draft" {
		t.Errorf("Status = %q, want \"draft\" (untouched)", got.Status)
	}
	if got.DesignJobID != "job_in_flight" {
		t.Errorf("DesignJobID = %q, want \"job_in_flight\" (untouched)", got.DesignJobID)
	}
}

// TestImportDesign_RejectsEmptyContent — case 4.
//
// An empty string would otherwise land in design_docs=” and make the page
// render a blank designer pane. The handler's trim+empty check (and the
// service's defense-in-depth duplicate) both reject.
func TestImportDesign_RejectsEmptyContent(t *testing.T) {
	env := newImportDesignTestEnv(t)

	rec := httptest.NewRecorder()
	env.h.ImportDesign(rec, putImportBody(t, map[string]string{
		"requirement_id": env.reqID,
		"content":        "   \n\t  ",
		"source":         "paste",
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	env2 := decodeEnvelope(t, rec)
	if env2.Error == nil || env2.Error.Code != "INVALID_INPUT" {
		t.Fatalf("error = %+v, want code=INVALID_INPUT", env2.Error)
	}
}

// TestImportDesign_RejectsOversizeContent — case 5.
//
// 500 KB cap is enforced as a byte-length check on the raw JSON content
// (we don't rune-truncate — a 200 KB Chinese plan is normal). 500_001 bytes
// of 'x' trips the 413 branch.
func TestImportDesign_RejectsOversizeContent(t *testing.T) {
	env := newImportDesignTestEnv(t)

	oversize := strings.Repeat("x", importDesignMaxBytes+1)
	rec := httptest.NewRecorder()
	env.h.ImportDesign(rec, putImportBody(t, map[string]string{
		"requirement_id": env.reqID,
		"content":        oversize,
		"source":         "paste",
	}))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	env2 := decodeEnvelope(t, rec)
	if env2.Error == nil || env2.Error.Code != "TOO_LARGE" {
		t.Fatalf("error = %+v, want code=TOO_LARGE", env2.Error)
	}
}

// TestImportDesign_SanitizesMarkdownFence — case 6.
//
// ChatGPT/DeepSeek sometimes wrap their plan in a single
// ```` ```markdown ```` fence. The service's SanitizeDesignDoc strips it
// (per its contract — must stay in sync with the frontend's parseDesign);
// the handler therefore persists the unwrapped body, not the wrapped
// form. Asserting on the unwrapped form proves the sanitize pass ran on
// the import path (not just on the Claude path).
func TestImportDesign_SanitizesMarkdownFence(t *testing.T) {
	env := newImportDesignTestEnv(t)

	const wrapped = "```markdown\n# hello\n```"
	rec := httptest.NewRecorder()
	env.h.ImportDesign(rec, putImportBody(t, map[string]string{
		"requirement_id": env.reqID,
		"content":        wrapped,
		"source":         "paste",
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, err := env.reqSvc.Get(env.reqID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if strings.HasPrefix(got.DesignDocs, "```") {
		t.Errorf("DesignDocs still wrapped: %q", got.DesignDocs)
	}
	if !strings.Contains(got.DesignDocs, "# hello") {
		t.Errorf("DesignDocs missing # hello; got: %q", got.DesignDocs)
	}
}

// TestImportDesignFile_AcceptsMdTxt_RejectsPdf_RejectsOversize — case 7.
//
// The upload endpoint is a multipart reader; the persistence step happens
// on the second call to ImportDesign. Three sub-cases here:
//
//  1. .md upload  → 200, content matches the file bytes
//  2. .pdf upload → 400 UNSUPPORTED_FORMAT (docs explicitly scope to
//     .md/.markdown/.txt — PDFs require real parsing we don't do here)
//  3. > 2 MB upload → 413 TOO_LARGE (multipart body is rejected before
//     the form is even parsed, courtesy of MaxBytesReader)
func TestImportDesignFile_AcceptsMdTxt_RejectsPdf_RejectsOversize(t *testing.T) {
	env := newImportDesignTestEnv(t)

	t.Run("md_accepted", func(t *testing.T) {
		body, contentType := buildMultipart(t, "plan.md", []byte("# hello md\n"))
		req := httptest.NewRequest(http.MethodPost, "/api/wizard/import-design-file", body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		env.h.ImportDesignFile(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Success bool `json:"success"`
			Data    struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !resp.Success {
			t.Fatalf("success=false")
		}
		if resp.Data.Content != "# hello md\n" {
			t.Errorf("content = %q, want %q", resp.Data.Content, "# hello md\n")
		}
	})

	t.Run("pdf_rejected", func(t *testing.T) {
		body, contentType := buildMultipart(t, "plan.pdf", []byte("%PDF-1.4 fake"))
		req := httptest.NewRequest(http.MethodPost, "/api/wizard/import-design-file", body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		env.h.ImportDesignFile(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		env2 := decodeEnvelope(t, rec)
		if env2.Error == nil || env2.Error.Code != "UNSUPPORTED_FORMAT" {
			t.Fatalf("error = %+v, want code=UNSUPPORTED_FORMAT", env2.Error)
		}
	})

	t.Run("oversize_rejected", func(t *testing.T) {
		// 3 MB body > 2 MB importDesignFileMaxBody — MaxBytesReader trips
		// during ParseMultipartForm and we surface TOO_LARGE.
		big := bytes.Repeat([]byte("x"), importDesignFileMaxBody+1024)
		body, contentType := buildMultipart(t, "plan.md", big)
		req := httptest.NewRequest(http.MethodPost, "/api/wizard/import-design-file", body)
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		env.h.ImportDesignFile(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
		env2 := decodeEnvelope(t, rec)
		if env2.Error == nil || env2.Error.Code != "TOO_LARGE" {
			t.Fatalf("error = %+v, want code=TOO_LARGE", env2.Error)
		}
	})
}

// buildMultipart constructs a minimal multipart/form-data body with one
// "file" field. Returns the body reader (Position 0; the test should treat
// it as io.Reader) and the matching Content-Type header value (including
// the boundary).
func buildMultipart(t *testing.T, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(content)); err != nil {
		t.Fatalf("copy content: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return body, w.FormDataContentType()
}

// ─────────────────────────────────────────────────────────────────────────────
// 「设计者上下文」显式开关（req_8cfaec1a6c9b0691）
//
// 方案设计阶段把架构师 persona（+ 附件清单）通过 --append-system-prompt
// 注入 claude。这个注入此前是隐式的 —— 用户在 UI 上看不到、也关不掉 —— 而且
// 本地与 Agent Server 两条路径行为不一致（远端分支根本没给
// remoteRunInput.SystemPrompt 赋值 ⇒ 远端既无 persona 也无附件清单）。
//
// 开关语义：
//   勾选（默认）= persona + 清单进 system prompt。本地路径与改动前逐字节一致；
//                远端被有意对齐到本地（顺带修掉「附件传上去了却从没告诉模型」）。
//   不勾选       = system prompt 槽完全空置（连 --append-system-prompt flag
//                 都不下发），需求物料（描述 + 附件清单）改走任务提示词，
//                 claude 用它自带的上下文（含 plan-mode 内建指令）。
//
// 下面几个用例分别钉住：prepareArchitectDesign 的两态产出、
// prepareDesignWorkspace 的物料归位、以及远端分支的 SystemPrompt 透传
// （最后这条正是「远端丢 persona」的根因，必须防回归）。
// ─────────────────────────────────────────────────────────────────────────────

// newRoleContextTestEnv wires the minimum handler graph the 「设计者上下文」
// code paths touch.
//
// The role / claude-config services are NOT optional here: roleConfig only
// guards `h.roleSvc == nil` — with roleSvc wired but claudeCfg missing,
// ResolveRoleConfig would nil-deref. Both are seeded with SeedDefaults so the
// 'architect' role carries its default persona.
//
// The requirement is seeded with a description AND a design_session_id:
// the latter makes prepareArchitectDesign take the RESUME branch
// (sourceSID = design_session_id, no fork ⇒ no anchored-fork gate), which is
// the branch whose prompt today carries only the title — exactly the branch
// that needs the description moved in when the toggle is off.
//
// One attachment is seeded so the manifest is non-empty: the manifest is the
// material that must move from the system prompt into the task prompt when
// the toggle is off.
func newRoleContextTestEnv(t *testing.T) (*db.DB, string, *WizardHandler) {
	t.Helper()
	dir, _ := os.MkdirTemp("", "role-ctx-")
	t.Cleanup(func() { os.RemoveAll(dir) })
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reqID := seedRequirementForSchedule(t, d)
	if _, err := d.Exec(
		`UPDATE requirements SET description = ?, design_session_id = ? WHERE id = ?`,
		"用户希望登录页支持短信验证码登录。", "sid-design", reqID,
	); err != nil {
		t.Fatalf("seed description / design_session_id: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO requirement_attachments (id, requirement_id, filename, stored_path, mime_type, size_bytes, sha256, description) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"att_role_ctx", reqID, "spec.md", "spec.md", "text/markdown", 42, "sha-role-ctx", "登录流程规格",
	); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	roleSvc := service.NewRoleService(d)
	if err := roleSvc.SeedDefaults(); err != nil {
		t.Fatalf("seed roles: %v", err)
	}
	h := &WizardHandler{
		db:           d,
		reqSvc:       service.NewRequirementService(d, nil),
		projectSvc:   service.NewProjectService(d, nil, nil),
		settingSvc:   service.NewSettingService(d),
		jobs:         store.NewJobStore(8),
		designRevSvc: service.NewDesignRevisionService(d),
		roleSvc:      roleSvc,
		claudeCfg:    service.NewClaudeConfigService(d),
		attachSvc:    service.NewAttachmentService(d),
	}
	return d, reqID, h
}

// seedDesignWorkspaceGit points the seeded project at a real on-disk git
// checkout with a reachable origin so prepareDesignWorkspace's hard-sync gate
// and worktree anchoring succeed. Mirrors the fixture used by
// TestPrepareDesignWorkspace_SuccessStampsBaseline.
//
// Each call gets its own upstream + clone (t.TempDir() is unique per call),
// and the worktree root is derived from the project path — so two calls in
// the same test never collide on the shared worktree directory.
func seedDesignWorkspaceGit(t *testing.T, d *db.DB) {
	t.Helper()
	const projID = "proj_seed_sched" // matches seedRequirementForSchedule's project id
	const baseBranch = "main"
	upstreamDir := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch="+baseBranch, upstreamDir)
	seed := filepath.Join(t.TempDir(), "seed")
	mustGit(t, "", "git", "clone", upstreamDir, seed)
	mustGit(t, seed, "git", "config", "user.email", "test@nova")
	mustGit(t, seed, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, seed, "git", "add", "README.md")
	mustGit(t, seed, "git", "commit", "-m", "init")
	mustGit(t, seed, "git", "push", "origin", "HEAD:"+baseBranch)

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "", "git", "clone", upstreamDir, work)
	mustGit(t, work, "git", "config", "user.email", "test@nova")
	mustGit(t, work, "git", "config", "user.name", "Test")
	if _, err := d.Exec(
		`UPDATE projects SET local_path = ?, remote_url = ? WHERE id = ?`,
		work, upstreamDir, projID); err != nil {
		t.Fatalf("update project: %v", err)
	}
}

// TestPrepareArchitectDesign_SkipRoleContextEmptiesSystemPrompt pins the
// OFF state of the toggle: the system prompt slot must be COMPLETELY empty
// — not "persona without the manifest", but empty — so llm.streamArgs omits
// the --append-system-prompt flag altogether and claude falls back to its own
// default system prompt (which is what carries the plan-mode instructions).
//
// The attachment manifest must survive the move: it is requirement material
// (the user's uploaded logs / specs), not role prompt, so it is carried in
// designRunParams.AttachManifest and re-homed into the task prompt.
func TestPrepareArchitectDesign_SkipRoleContextEmptiesSystemPrompt(t *testing.T) {
	_, reqID, h := newRoleContextTestEnv(t)

	p, _, af := h.prepareArchitectDesign(context.Background(), reqID, "", "", "", false, "", "main", false, true)
	if af != nil {
		t.Fatalf("prepareArchitectDesign failed: %v", af)
	}
	if !p.SkipRoleContext {
		t.Fatalf("SkipRoleContext = false, want true")
	}
	if p.SystemPrompt != "" {
		t.Fatalf("SystemPrompt = %q, want \"\" (toggle off ⇒ no --append-system-prompt at all)", p.SystemPrompt)
	}
	if p.AttachManifest == "" {
		t.Fatalf("AttachManifest = \"\", want the manifest so it can move into the task prompt")
	}
	if !strings.Contains(p.AttachManifest, "spec.md") {
		t.Fatalf("AttachManifest = %q, want it to mention the seeded attachment", p.AttachManifest)
	}
}

// TestPrepareArchitectDesign_KeepsPersonaByDefault pins the ON state: the
// default (the toggle omitted from the request body, i.e. every legacy
// caller / curl / scheduled task) must keep today's behavior byte-for-byte —
// persona AND manifest both in the system prompt.
//
// The zero value of the internal field has to be the SAFE side: a caller
// that forgets to wire the flag must never silently lose the persona.
func TestPrepareArchitectDesign_KeepsPersonaByDefault(t *testing.T) {
	_, reqID, h := newRoleContextTestEnv(t)

	p, _, af := h.prepareArchitectDesign(context.Background(), reqID, "", "", "", false, "", "main", false, false)
	if af != nil {
		t.Fatalf("prepareArchitectDesign failed: %v", af)
	}
	if p.SkipRoleContext {
		t.Fatalf("SkipRoleContext = true, want false (default = keep the persona)")
	}
	if !strings.Contains(p.SystemPrompt, "资深软件架构师") {
		t.Fatalf("SystemPrompt must carry the architect persona by default, got %q", p.SystemPrompt)
	}
	if !strings.Contains(p.SystemPrompt, "spec.md") {
		t.Fatalf("SystemPrompt must carry the attachment manifest by default, got %q", p.SystemPrompt)
	}
}

// TestPrepareDesignWorkspace_UncheckedMovesMaterialIntoPrompt pins the
// end-to-end contract of the OFF state: the requirement description AND the
// attachment manifest must be present in the task prompt (which local and
// Agent-server runs share), because the system prompt slot is empty.
//
// The default state must NOT contain either — there the manifest lives in
// the system prompt and the description rides on the resumed analyst
// conversation. Duplicating them into the task prompt would change the
// default path's prompt bytes.
func TestPrepareDesignWorkspace_UncheckedMovesMaterialIntoPrompt(t *testing.T) {
	// ── OFF: system prompt empty, material moves into the task prompt ──
	dOff, reqOff, hOff := newRoleContextTestEnv(t)
	seedDesignWorkspaceGit(t, dOff)

	pOff, jobOff, af := hOff.prepareArchitectDesign(context.Background(), reqOff, "", "", "", false, "", "main", false, true)
	if af != nil {
		t.Fatalf("prepareArchitectDesign (off) failed: %v", af)
	}
	if !hOff.prepareDesignWorkspace(pOff, jobOff) {
		lines, status, _ := jobOff.Snapshot()
		t.Fatalf("prepareDesignWorkspace (off) failed (status=%s, lines=%+v)", status, lines)
	}
	if !strings.Contains(pOff.Prompt, "## 需求描述") {
		t.Fatalf("toggle off: task prompt must carry the requirement description, got:\n%s", pOff.Prompt)
	}
	if !strings.Contains(pOff.Prompt, "短信验证码登录") {
		t.Fatalf("toggle off: task prompt must carry the requirement description body, got:\n%s", pOff.Prompt)
	}
	if !strings.Contains(pOff.Prompt, "spec.md") {
		t.Fatalf("toggle off: task prompt must carry the attachment manifest, got:\n%s", pOff.Prompt)
	}

	// ── ON: system prompt carries both, task prompt carries neither ──
	dOn, reqOn, hOn := newRoleContextTestEnv(t)
	seedDesignWorkspaceGit(t, dOn)

	pOn, jobOn, af := hOn.prepareArchitectDesign(context.Background(), reqOn, "", "", "", false, "", "main", false, false)
	if af != nil {
		t.Fatalf("prepareArchitectDesign (on) failed: %v", af)
	}
	if !hOn.prepareDesignWorkspace(pOn, jobOn) {
		lines, status, _ := jobOn.Snapshot()
		t.Fatalf("prepareDesignWorkspace (on) failed (status=%s, lines=%+v)", status, lines)
	}
	if strings.Contains(pOn.Prompt, "## 需求描述") {
		t.Fatalf("toggle on: task prompt must NOT duplicate the requirement description, got:\n%s", pOn.Prompt)
	}
	if strings.Contains(pOn.Prompt, "spec.md") {
		t.Fatalf("toggle on: task prompt must NOT duplicate the attachment manifest, got:\n%s", pOn.Prompt)
	}
	if !strings.Contains(pOn.Prompt, "## 需求标题") {
		t.Fatalf("toggle on: task prompt must still carry the requirement title, got:\n%s", pOn.Prompt)
	}
}

// TestArchitectRemoteCallSiteForwardsSystemPrompt is a source-level wiring
// guard: the remote architect branch MUST forward p.SystemPrompt into
// remoteRunInput.SystemPrompt.
//
// This is the exact spot where the remote path used to drop the architect
// persona AND the attachment manifest — the files were SFTP'd to the agent
// host but the prompt never told the model they existed. The failure mode
// is invisible to the compiler (both sides are strings), so we assert the
// wiring directly against the AST, the same way the RC-1 guard above does
// for sourceSID.
//
// If this test fails after a refactor: check that the remoteRunInput literal
// in execArchitectDesign still carries SystemPrompt.
func TestArchitectRemoteCallSiteForwardsSystemPrompt(t *testing.T) {
	const srcFile = "wizard_architect.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, srcFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcFile, err)
	}

	type literal struct {
		pos    token.Position
		fields map[string]string
	}
	var literals []literal

	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		ident, ok := cl.Type.(*ast.Ident)
		if !ok || ident.Name != "remoteRunInput" {
			return true
		}
		l := literal{pos: fset.Position(cl.Pos()), fields: map[string]string{}}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			l.fields[key.Name] = renderExpr(kv.Value)
		}
		literals = append(literals, l)
		return true
	})

	if len(literals) == 0 {
		t.Fatalf("no remoteRunInput composite literal found in %s — "+
			"if execArchitectDesign was refactored, update this guard to match", srcFile)
	}

	for _, l := range literals {
		got, present := l.fields["SystemPrompt"]
		if !present {
			t.Fatalf("%s: remoteRunInput literal at %s has no SystemPrompt field — "+
				"the architect persona (+ attachment manifest) would never reach the remote claude",
				srcFile, l.pos)
		}
		if got != "p.SystemPrompt" {
			t.Fatalf("%s: remoteRunInput{SystemPrompt: %s} at %s — regression.\n"+
				"the remote architect branch must forward the SAME string the local branch "+
				"passes via llm.StreamOpts.SystemPrompt, so local and remote runs behave "+
				"identically for the 「设计者上下文」toggle (empty string = no "+
				"--append-system-prompt at all).",
				srcFile, got, l.pos)
		}
	}
}

// ── 继续设计 (POST /api/wizard/continue-design) ─────────────────────────────
//
// The continuation path exists for the state the pipeline otherwise cannot
// leave: an interrupted design run (backend restart, killed claude, 504,
// stall watchdog) whose partial plan / session survived. These tests pin the
// three properties the recovery depends on:
//  1. only status=designing + a non-empty design session may continue;
//  2. the run resumes that SAME session (no fork, no new id);
//  3. no design_revisions row is written (continue is not regenerate).

func TestPrepareContinueDesign_RejectsInvalidState(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		kind       string
		designSID  string
		wantStatus int
		wantCode   string
	}{
		{"draft has no run to resume", "draft", "requirement", "sid", 409, "INVALID_STATUS"},
		{"analyzing has no design run", "analyzing", "requirement", "sid", 409, "INVALID_STATUS"},
		{"designed uses regenerate instead", "designed", "requirement", "sid", 409, "INVALID_STATUS"},
		{"developing uses regenerate instead", "developing", "requirement", "sid", 409, "INVALID_STATUS"},
		{"done is terminal", "done", "requirement", "sid", 409, "INVALID_STATUS"},
		{"archived is terminal", "archived", "requirement", "sid", 409, "INVALID_STATUS"},
		{"designing without a session", "designing", "requirement", "", 409, "NO_SESSION"},
		{"wiki uses its own pipeline", "designing", "wiki", "sid", 400, "KIND_NOT_SUPPORTED"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, reqID, h, reqSvc := newRegenerateTestEnv(t)
			if _, err := d.Exec(`UPDATE requirements SET status=?, kind=? WHERE id=?`, c.status, c.kind, reqID); err != nil {
				t.Fatalf("seed status/kind: %v", err)
			}
			if c.designSID != "" {
				if err := reqSvc.UpdateDesignSession(reqID, c.designSID); err != nil {
					t.Fatalf("seed design_session_id: %v", err)
				}
			}

			_, _, af := h.prepareContinueDesign(context.Background(), reqID, false)
			if af == nil {
				t.Fatalf("expected %d %s, got success", c.wantStatus, c.wantCode)
			}
			if af.Status != c.wantStatus || af.Code != c.wantCode {
				t.Fatalf("got %d %s (%s), want %d %s", af.Status, af.Code, af.Msg, c.wantStatus, c.wantCode)
			}
		})
	}
}

func TestPrepareContinueDesign_ResumesSessionWithoutSnapshot(t *testing.T) {
	d, reqID, h, reqSvc := newRegenerateTestEnv(t)
	if err := reqSvc.UpdateDesignSession(reqID, "sid-design-interrupted"); err != nil {
		t.Fatalf("seed design_session_id: %v", err)
	}
	if err := reqSvc.UpdateAnalysisSession(reqID, "sid-analyst"); err != nil {
		t.Fatalf("seed analysis_session_id: %v", err)
	}
	if _, err := d.Exec(
		`UPDATE requirements SET status='designing', design_docs=? WHERE id=?`,
		"## 方案草稿\n\n已经探明的部分。", reqID,
	); err != nil {
		t.Fatalf("seed status/design_docs: %v", err)
	}

	p, job, af := h.prepareContinueDesign(context.Background(), reqID, false)
	if af != nil {
		t.Fatalf("prepareContinueDesign failed: %d %s %s", af.Status, af.Code, af.Msg)
	}
	if job == nil {
		t.Fatal("expected a JobStore job")
	}
	if !p.Continue {
		t.Fatal("p.Continue must be true — that is what selects the continuation prompt")
	}
	if p.SourceSID != "sid-design-interrupted" {
		t.Fatalf("SourceSID = %q, want the persisted design session", p.SourceSID)
	}
	if p.Fork {
		t.Fatal("continuation must resume, not fork")
	}
	if p.NewDesignSID != "" {
		t.Fatalf("NewDesignSID = %q, want empty (no new session minted)", p.NewDesignSID)
	}

	var nRev int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM design_revisions WHERE requirement_id = ?`, reqID,
	).Scan(&nRev); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if nRev != 0 {
		t.Fatalf("design_revisions count = %d, want 0 (continue is not regenerate)", nRev)
	}

	got, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	if got.DesignSessionID != "sid-design-interrupted" {
		t.Fatalf("design_session_id = %q, want it untouched", got.DesignSessionID)
	}
	if got.DesignJobID == "" {
		t.Fatal("design_job_id must be persisted so a refresh reconnects to the run")
	}
}

// TestPrepareDesignWorkspace_ContinueUsesContinuationPrompt pins the prompt
// branch wiring: p.Continue must REPLACE the standard "switch to the architect
// role" prompt, and the replacement must be the bare "继续" instruction — no
// requirement title, no plan contract, no partial design_docs. Re-feeding any
// of that makes the resumed model rewrite the plan instead of continuing.
func TestPrepareDesignWorkspace_ContinueUsesContinuationPrompt(t *testing.T) {
	dir, _ := os.MkdirTemp("", "prep-design-continue-")
	defer os.RemoveAll(dir)
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	reqID := seedRequirementForSchedule(t, d)
	const projID = "proj_seed_sched"
	const baseBranch = "main"

	upstreamDir := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch="+baseBranch, upstreamDir)
	seed := filepath.Join(t.TempDir(), "seed")
	mustGit(t, "", "git", "clone", upstreamDir, seed)
	mustGit(t, seed, "git", "config", "user.email", "test@nova")
	mustGit(t, seed, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, seed, "git", "add", "README.md")
	mustGit(t, seed, "git", "commit", "-m", "init")
	mustGit(t, seed, "git", "push", "origin", "HEAD:"+baseBranch)

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "", "git", "clone", upstreamDir, work)
	mustGit(t, work, "git", "config", "user.email", "test@nova")
	mustGit(t, work, "git", "config", "user.name", "Test")

	if _, err := d.Exec(
		`UPDATE projects SET local_path = ?, remote_url = ? WHERE id = ?`,
		work, upstreamDir, projID); err != nil {
		t.Fatalf("update project: %v", err)
	}

	const partial = "## 方案草稿\n\n已经完成的探索结论：先改 A，再改 B。"
	if _, err := d.Exec(
		`UPDATE requirements SET status='designing', design_docs=? WHERE id=?`, partial, reqID,
	); err != nil {
		t.Fatalf("seed design_docs: %v", err)
	}

	reqSvc := service.NewRequirementService(d, nil)
	projSvc := service.NewProjectService(d, nil, nil)
	settingSvc := service.NewSettingService(d)
	jobs := store.NewJobStore(8)

	req, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	job := jobs.Create(reqID)
	h := &WizardHandler{
		db:         d,
		projectSvc: projSvc,
		reqSvc:     reqSvc,
		settingSvc: settingSvc,
		jobs:       jobs,
	}

	p := &designRunParams{
		Req:         req,
		ProjectPath: work,
		BaseBranch:  baseBranch,
		SourceSID:   "sid-design-interrupted",
		Continue:    true,
	}

	if ok := h.prepareDesignWorkspace(p, job); !ok {
		lines, status, _ := job.Snapshot()
		t.Fatalf("prepareDesignWorkspace failed (status=%s, lines=%+v)", status, lines)
	}
	// The whole point of the continuation is a bare --resume: the session
	// already holds the requirement, the analysis and the partial plan. Any
	// re-fed material turns into a rewrite, so the prompt must START with the
	// short constant (the kind-specific output block — e.g. the Mermaid
	// directive — is still appended after it; that is an output contract, not
	// a task re-feed). SkipRoleContext=false here, so no description /
	// manifest prefixes either.
	if !strings.HasPrefix(p.Prompt, continueDesignPrompt) {
		t.Fatalf("continuation prompt = %q, want it to start with %q", p.Prompt, continueDesignPrompt)
	}
	for _, unwanted := range []string{
		"现在切换到「架构师」角色",
		"基于我们刚才完成的需求分析对话",
		partial,
		"## 需求标题",
		"上次运行保存的方案草稿",
	} {
		if strings.Contains(p.Prompt, unwanted) {
			t.Errorf("continuation prompt must not re-feed %q\n---\n%s", unwanted, p.Prompt)
		}
	}
}
