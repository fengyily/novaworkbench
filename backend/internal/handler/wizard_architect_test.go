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
		Req:           req,
		ProjectPath:   work,
		BaseBranch:    "main",
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
		Req:           req,
		ProjectPath:   work,
		BaseBranch:    baseBranch,
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
		Req:           req,
		NewDesignSID:  "sid-pre-minted", // exactly the row currently on disk
		SourceSID:     "sid-pre-minted", // simulating a re-run that resumed it
		Fork:          false,
		SkipAnalysis:  false,
		ClaudeConfigID: "",
		Model:         "sonnet",
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
					sessionID:            "sid-stale",
					staleSession:         true,
					errMsg:               "",
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
			sessionID:             "sid-stale-local",
			staleSession:          true,
			errMsg:                "",
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
// An empty string would otherwise land in design_docs='' and make the page
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
			Success bool   `json:"success"`
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
