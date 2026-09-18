package handler

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
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
	reqSvc := service.NewRequirementService(d)
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

	reqSvc := service.NewRequirementService(d)
	projSvc := service.NewProjectService(d, nil)
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
		Req:           req,
		ProjectPath:   work,
		DefaultBranch: "main",
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

	reqSvc := service.NewRequirementService(d)
	projSvc := service.NewProjectService(d, nil)
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
		Req:           req,
		ProjectPath:   work,
		DefaultBranch: baseBranch,
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
