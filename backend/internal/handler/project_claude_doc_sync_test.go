package handler

import (
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

// ── helpers ──────────────────────────────────────────────────────────────

// gitIn runs a git command in dir and fails the test when it errors.
func gitOutIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newOriginRepo creates a git repo with one commit on "main" and returns its
// path.
func newOriginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitOutIn(t, dir, "init", "-q", "-b", "main")
	gitOutIn(t, dir, "config", "user.email", "test@example.com")
	gitOutIn(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutIn(t, dir, "add", ".")
	gitOutIn(t, dir, "commit", "-qm", "initial commit")
	return dir
}

// ── tests ────────────────────────────────────────────────────────────────

// TestClaudeDocBaseBranch pins the branch resolution: the project's
// configured default branch wins, "main" is the fallback.
func TestClaudeDocBaseBranch(t *testing.T) {
	if got := claudeDocBaseBranch(nil); got != "main" {
		t.Errorf("nil project → %q, want main", got)
	}
	if got := claudeDocBaseBranch(&model.Project{}); got != "main" {
		t.Errorf("empty default branch → %q, want main", got)
	}
	if got := claudeDocBaseBranch(&model.Project{DefaultBranch: " develop "}); got != "develop" {
		t.Errorf("default branch → %q, want develop", got)
	}
}

// TestSyncClaudeDocBaseNonGitRepo pins the degradation path: a project that
// is not a git repo is reported, not crashed on, and no ref is pinned.
func TestSyncClaudeDocBaseNonGitRepo(t *testing.T) {
	dir := t.TempDir()
	var logs []string
	info := syncClaudeDocBase(dir, "main", nil, func(s string) { logs = append(logs, s) })
	if info.Ref != "" {
		t.Errorf("Ref = %q, want empty for a non-git dir", info.Ref)
	}
	if !strings.Contains(info.Note, "不是 Git 仓库") {
		t.Errorf("Note = %q, want it to mention the non-git project", info.Note)
	}
	if len(logs) == 0 {
		t.Error("expected the degradation to be reported to the stream")
	}
}

// TestSyncClaudeDocBaseFetchesLatestMain is the core contract: the base
// branch is fetched, the pinned ref is the fresh origin/main, and the
// ahead/behind counters tell the user how far the checkout drifted.
func TestSyncClaudeDocBaseFetchesLatestMain(t *testing.T) {
	origin := newOriginRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	cloneOut, err := exec.Command("git", "clone", "-q", "-b", "main", origin, clone).CombinedOutput()
	if err != nil {
		t.Fatalf("git clone: %v\n%s", err, cloneOut)
	}

	// A commit lands upstream AFTER the clone → the clone is one commit
	// behind origin/main.
	if err := os.WriteFile(filepath.Join(origin, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutIn(t, origin, "add", ".")
	gitOutIn(t, origin, "commit", "-qm", "upstream change")
	upstreamSHA := gitOutIn(t, origin, "rev-parse", "HEAD")

	var logs []string
	info := syncClaudeDocBase(clone, "main", nil, func(s string) { logs = append(logs, s) })
	if !info.Fetched {
		t.Errorf("Fetched = false, want true (note: %s)", info.Note)
	}
	if info.Ref != "origin/main" {
		t.Errorf("Ref = %q, want origin/main", info.Ref)
	}
	if info.RefSHA != upstreamSHA {
		t.Errorf("RefSHA = %q, want %q (the freshly fetched upstream tip)", info.RefSHA, upstreamSHA)
	}
	if info.Behind != 1 {
		t.Errorf("Behind = %d, want 1", info.Behind)
	}
	if info.Ahead != 0 {
		t.Errorf("Ahead = %d, want 0", info.Ahead)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"origin/main", shortSHA(upstreamSHA), "当前检出分支"} {
		if !strings.Contains(joined, want) {
			t.Errorf("stream output missing %q:\n%s", want, joined)
		}
	}
}

// TestClaudeDocGenerationRootPinsRef checks the worktree pinning: the
// generation directory holds the pinned commit's content, and the cleanup
// removes both the directory and the worktree registration.
func TestClaudeDocGenerationRootPinsRef(t *testing.T) {
	repo := newOriginRepo(t)
	// A second commit so "HEAD" differs from the first one — the worktree
	// must carry the LATEST content.
	if err := os.WriteFile(filepath.Join(repo, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutIn(t, repo, "add", ".")
	gitOutIn(t, repo, "commit", "-qm", "later commit")

	dir, cleanup, err := claudeDocGenerationRoot(repo, "HEAD")
	if err != nil {
		t.Fatalf("claudeDocGenerationRoot: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "later.txt")); statErr != nil {
		t.Errorf("pinned worktree misses the latest content: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr != nil {
		t.Errorf("pinned worktree is not a git checkout: %v", statErr)
	}
	// The user's working tree must be untouched by the pinning.
	if _, statErr := os.Stat(filepath.Join(repo, "later.txt")); statErr != nil {
		t.Errorf("the source checkout was disturbed: %v", statErr)
	}

	cleanup()
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("worktree dir still exists after cleanup: %v", statErr)
	}
	if out := gitOutIn(t, repo, "worktree", "list"); strings.Contains(out, dir) {
		t.Errorf("worktree registration survived the cleanup:\n%s", out)
	}
}

// TestClaudeDocGenerationRootRejectsEmptyRef guards the "no ref" path: the
// caller must be able to detect it and fall back to the local checkout.
func TestClaudeDocGenerationRootRejectsEmptyRef(t *testing.T) {
	repo := newOriginRepo(t)
	if _, _, err := claudeDocGenerationRoot(repo, ""); err == nil {
		t.Error("expected an error for an empty ref")
	}
}

// TestPrepareGenerationRootStreamsCommitFacts is the end-to-end contract of
// the handler wiring: the generation runs in a directory pinned to the fresh
// origin/main, and the stream carries the branch / commit facts the user
// needs to judge the result.
func TestPrepareGenerationRootStreamsCommitFacts(t *testing.T) {
	origin := newOriginRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", "-b", "main", origin, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	// Upstream moves on after the clone.
	if err := os.WriteFile(filepath.Join(origin, "upstream.txt"), []byte("u\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutIn(t, origin, "add", ".")
	gitOutIn(t, origin, "commit", "-qm", "upstream change")
	upstreamSHA := gitOutIn(t, origin, "rev-parse", "HEAD")

	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	svc := service.NewProjectService(d, nil, nil)
	const projID = "proj_claude_doc_sync"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, default_branch) VALUES (?, ?, ?, ?)`,
		projID, "sync project", clone, "main"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	h := NewProjectClaudeDocHandler(svc, nil, store.NewJobStore(4))
	proj, err := svc.Get(projID)
	if err != nil {
		t.Fatalf("svc.Get: %v", err)
	}

	job := h.jobs.Create(projID)
	gen := h.prepareGenerationRoot(job, proj, clone)
	root := gen.Dir
	if root == clone {
		t.Error("the generation must not run in the user's working tree")
	}
	if _, statErr := os.Stat(filepath.Join(root, "upstream.txt")); statErr != nil {
		t.Errorf("the pinned worktree misses the upstream commit content: %v", statErr)
	}

	lines, _, _ := job.Snapshot()
	var joined string
	for _, l := range lines {
		joined += l.Content + "\n"
	}
	for _, want := range []string{"origin/main", shortSHA(upstreamSHA), "当前检出分支"} {
		if !strings.Contains(joined, want) {
			t.Errorf("stream output missing %q:\n%s", want, joined)
		}
	}

	gen.cleanup()
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Errorf("the worktree dir survived the cleanup: %v", statErr)
	}
}

// TestSanitizeClaudeDocSnapshotArtifacts pins the safety net behind the
// snapshot rule.
//
// The prompt asks the model not to document the scratch worktree, but a
// prompt is a request: a linked worktree's `.git` is a file pointing at the
// real repository's metadata, and that is exactly the kind of detail a
// model writes down. Such a line must not survive into the stored
// document — it describes the scratch checkout, not the project, and it
// carries a machine-specific path into an asset that lives forever.
func TestSanitizeClaudeDocSnapshotArtifacts(t *testing.T) {
	snapshotDir := "/var/folders/xx/T/nova-claude-doc-1665575766"
	content := strings.Join([]string{
		"# Project",
		"",
		"- The working copy at this path is a **linked worktree** (`.git` is a file containing `gitdir: /var/folders/xx/T/nova-claude-doc-1665575766/.git/worktrees/nova-claude-doc-1665575766`).",
		"- The project uses `make build` to build.",
		"- The origin remote is `https://example.com/acme/demo.git`.",
		"",
	}, "\n")

	out := sanitizeClaudeDocSnapshotArtifacts(content, snapshotDir)
	if strings.Contains(out, "nova-claude-doc-1665575766") {
		t.Errorf("the snapshot path survived:\n%s", out)
	}
	if strings.Contains(out, "linked worktree") {
		t.Errorf("the linked-worktree line survived:\n%s", out)
	}
	for _, keep := range []string{"# Project", "make build", "example.com/acme/demo.git"} {
		if !strings.Contains(out, keep) {
			t.Errorf("the scrubber dropped legitimate content %q:\n%s", keep, out)
		}
	}

	// A document without snapshot traces is returned unchanged.
	clean := "# Project\n\n- uses make build\n"
	if got := sanitizeClaudeDocSnapshotArtifacts(clean, snapshotDir); got != clean {
		t.Errorf("clean document was modified:\n%s", got)
	}
	// No snapshot dir → no scrubbing at all.
	if got := sanitizeClaudeDocSnapshotArtifacts(content, ""); got != content {
		t.Errorf("document was modified without a snapshot dir:\n%s", got)
	}
}

// TestPrepareGenerationRootReportsSnapshot pins the flag the prompt and the
// scrubber rely on: the pinned worktree is a snapshot, the fallback to the
// user's checkout is not.
func TestPrepareGenerationRootReportsSnapshot(t *testing.T) {
	origin := newOriginRepo(t)
	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", "-b", "main", origin, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	svc := service.NewProjectService(d, nil, nil)
	const projID = "proj_claude_doc_snapshot_flag"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, default_branch) VALUES (?, ?, ?, ?)`,
		projID, "snapshot project", clone, "main"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	h := NewProjectClaudeDocHandler(svc, nil, store.NewJobStore(4))
	proj, err := svc.Get(projID)
	if err != nil {
		t.Fatalf("svc.Get: %v", err)
	}

	gen := h.prepareGenerationRoot(h.jobs.Create(projID), proj, clone)
	if !gen.Snapshot {
		t.Errorf("Snapshot = false for a pinned worktree (dir=%q)", gen.Dir)
	}
	if gen.Ref == "" || gen.RefSHA == "" {
		t.Errorf("Ref/RefSHA not reported: %q / %q", gen.Ref, gen.RefSHA)
	}
	gen.cleanup()

	// A non-git project falls back to the local checkout — no snapshot.
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := d.Exec(`UPDATE projects SET local_path = ? WHERE id = ?`, plain, projID); err != nil {
		t.Fatalf("update project: %v", err)
	}
	proj, err = svc.Get(projID)
	if err != nil {
		t.Fatalf("svc.Get: %v", err)
	}
	gen = h.prepareGenerationRoot(h.jobs.Create(projID), proj, plain)
	if gen.Snapshot {
		t.Errorf("Snapshot = true for a non-git fallback (dir=%q)", gen.Dir)
	}
	if gen.Dir != plain {
		t.Errorf("Dir = %q, want the local checkout %q", gen.Dir, plain)
	}
	gen.cleanup()
}
