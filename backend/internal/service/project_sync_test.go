package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// TestEnsureClonedAndSynced_FourScenarios is the integration smoke for the
// "方案设计前同步仓库到工作目录" Step 6 verification matrix. It exercises
// EnsureClonedAndSynced against real on-disk git repos so the four user-visible
// behaviours (clone / fetch / skip / fail) can be asserted with go test:
//
//	Case 1: 目录缺失 + 有 remote        → Cloned=true,  sync_status=ok
//	Case 2: 目录存在 + 远端领先         → Fetched=true, sync_status=ok
//	Case 3: 目录存在 + remote_url=""    → Skipped=true, sync_status=idle
//	Case 4: 目录存在 + 远端不可达       → Err!=nil, sync_status=error (non-fatal)
//
// These mirror the four manual UI scenarios from the design doc — the
// go-test run is the closest automated equivalent without spinning up the
// dev server + clicking through the wizard panel.
func TestEnsureClonedAndSynced_FourScenarios(t *testing.T) {
	d := newTestDB(t)
	svc := NewProjectService(d, nil)
	ctx := context.Background()

	// upstream: a real bare repo on disk; we push commits to it across cases.
	upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch=main", upstreamDir)

	// helper: clone upstream into a working tree and commit a marker file.
	cloneAndCommit := func(t *testing.T, workdir, msg, file string) string {
		t.Helper()
		mustGit(t, "", "git", "clone", upstreamDir, workdir)
		mustGit(t, workdir, "git", "config", "user.email", "test@nova")
		mustGit(t, workdir, "git", "config", "user.name", "Test")
		// After `git clone` the working tree is already on the default branch
		// (main), so `git checkout -b main` fails with "branch already exists"
		// on the second invocation. `git checkout main` would also be a no-op
		// (we're already there); just commit on the current branch.
		if err := os.WriteFile(filepath.Join(workdir, file), []byte(msg), 0644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		mustGit(t, workdir, "git", "add", file)
		mustGit(t, workdir, "git", "commit", "-m", msg)
		sha := mustGit(t, workdir, "git", "rev-parse", "HEAD")
		mustGit(t, workdir, "git", "push", "origin", "HEAD:main")
		return strings.TrimSpace(sha)
	}

	// Initial commit so origin/main exists for the clone path.
	cloneAndCommit(t, filepath.Join(t.TempDir(), "seed"), "seed", "README.md")

	addProject := func(localPath, remote string) string {
		t.Helper()
		p := &model.Project{
			ID:            "proj_" + filepath.Base(localPath),
			Name:          filepath.Base(localPath),
			LocalPath:     localPath,
			RemoteURL:     remote,
			Status:        "active",
			DefaultBranch: "main",
		}
		if _, err := d.Exec(
			`INSERT INTO projects (id, name, local_path, remote_url, status, default_branch) VALUES (?, ?, ?, ?, ?, ?)`,
			p.ID, p.Name, p.LocalPath, p.RemoteURL, p.Status, p.DefaultBranch); err != nil {
			t.Fatalf("insert project: %v", err)
		}
		return p.ID
	}

	readSyncStatus := func(t *testing.T, id string) (status, commit string, syncedAt time.Time) {
		t.Helper()
		var nullTime *time.Time
		err := d.QueryRow(
			`SELECT sync_status, last_synced_commit, last_synced_at FROM projects WHERE id = ?`, id,
		).Scan(&status, &commit, &nullTime)
		if err != nil {
			t.Fatalf("read sync: %v", err)
		}
		if nullTime != nil {
			syncedAt = *nullTime
		}
		return
	}

	// ── Case 1: 目录缺失 + 有 remote → clone ───────────────────────────
	t.Run("Case1_MissingDir_Clones", func(t *testing.T) {
		projLocal := filepath.Join(t.TempDir(), "case1") // intentionally NOT created
		id := addProject(projLocal, upstreamDir)
		res, err := svc.EnsureClonedAndSynced(ctx, id, nil)
		if err != nil {
			t.Fatalf("EnsureClonedAndSynced: %v", err)
		}
		if !res.Cloned {
			t.Fatalf("expected Cloned=true, got %+v", res)
		}
		if res.NewSHA == "" {
			t.Fatalf("expected non-empty NewSHA after clone, got %+v", res)
		}
		if _, err := os.Stat(projLocal); err != nil {
			t.Fatalf("expected %s to exist after clone: %v", projLocal, err)
		}
		status, commit, syncedAt := readSyncStatus(t, id)
		if status != SyncStatusOK {
			t.Fatalf("expected sync_status=%q, got %q", SyncStatusOK, status)
		}
		if commit == "" {
			t.Fatalf("expected non-empty last_synced_commit, got empty")
		}
		if syncedAt.IsZero() {
			t.Fatalf("expected non-zero last_synced_at")
		}
	})

	// ── Case 2: 目录存在 + 远端领先 → fetch ────────────────────────────
	t.Run("Case2_RemoteAhead_Fetches", func(t *testing.T) {
		projLocal := filepath.Join(t.TempDir(), "case2")
		id := addProject(projLocal, upstreamDir)
		// Initial clone via the service (not strictly needed but mirrors real flow).
		if _, err := svc.EnsureClonedAndSynced(ctx, id, nil); err != nil {
			t.Fatalf("initial sync: %v", err)
		}
		_, beforeCommit, _ := readSyncStatus(t, id)

		// Push a new commit to upstream so local is behind.
		newSHA := cloneAndCommit(t, filepath.Join(t.TempDir(), "pusher2"), "second commit", "NEWS.md")
		if newSHA == beforeCommit {
			t.Fatalf("test setup error: upstream did not advance (%s)", newSHA)
		}

		res, err := svc.EnsureClonedAndSynced(ctx, id, nil)
		if err != nil {
			t.Fatalf("EnsureClonedAndSynced: %v", err)
		}
		if !res.Fetched {
			t.Fatalf("expected Fetched=true, got %+v", res)
		}
		if res.NewSHA == "" {
			t.Fatalf("expected non-empty NewSHA after fetch, got %+v", res)
		}
		status, commit, _ := readSyncStatus(t, id)
		if status != SyncStatusOK {
			t.Fatalf("expected sync_status=%q, got %q", SyncStatusOK, status)
		}
		if commit != res.NewSHA {
			t.Fatalf("expected last_synced_commit=%s, got %s", res.NewSHA, commit)
		}
	})

	// ── Case 3: 目录存在 + remote_url="" → skip ────────────────────────
	t.Run("Case3_NoRemote_Skips", func(t *testing.T) {
		projLocal := filepath.Join(t.TempDir(), "case3")
		if err := os.MkdirAll(projLocal, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		id := addProject(projLocal, "") // no remote_url
		res, err := svc.EnsureClonedAndSynced(ctx, id, nil)
		if err != nil {
			t.Fatalf("EnsureClonedAndSynced: %v", err)
		}
		if !res.Skipped {
			t.Fatalf("expected Skipped=true, got %+v", res)
		}
		status, _, _ := readSyncStatus(t, id)
		// sync_status stays at its schema default ('idle') for skipped runs.
		if status != "idle" {
			t.Fatalf("expected sync_status to remain idle, got %q", status)
		}
	})

	// ── Case 4: 远端不可达 → sync_status=error, 不阻塞 ────────────────
	t.Run("Case4_UnreachableRemote_FallsBack", func(t *testing.T) {
		projLocal := filepath.Join(t.TempDir(), "case4")
		if err := os.MkdirAll(projLocal, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Make projLocal a git repo with origin pointing at a black hole so
		// `git fetch` fails fast (the service caps the wait at 30s).
		mustGit(t, projLocal, "git", "init", "--initial-branch=main")
		mustGit(t, projLocal, "git", "config", "user.email", "test@nova")
		mustGit(t, projLocal, "git", "config", "user.name", "Test")
		if err := os.WriteFile(filepath.Join(projLocal, "stub"), []byte("hi"), 0644); err != nil {
			t.Fatalf("write: %v", err)
		}
		mustGit(t, projLocal, "git", "add", "stub")
		mustGit(t, projLocal, "git", "commit", "-m", "stub")
		mustGit(t, projLocal, "git", "remote", "add", "origin", "https://127.0.0.1:1/dead.git")

		id := addProject(projLocal, "https://127.0.0.1:1/dead.git")

		// Use a tight ctx so we don't wait the full timeout in CI; the service
		// caps inner fetch at 30s anyway.
		fctx, cancel := context.WithTimeout(ctx, 35*time.Second)
		defer cancel()
		res, err := svc.EnsureClonedAndSynced(fctx, id, nil)
		if err != nil {
			t.Fatalf("EnsureClonedAndSynced must NOT return fatal error on fetch failure: %v", err)
		}
		if res.Err == nil {
			t.Fatalf("expected res.Err to surface the fetch failure, got %+v", res)
		}
		if res.Fetched {
			t.Fatalf("expected Fetched=false on failure, got %+v", res)
		}
		status, _, syncedAt := readSyncStatus(t, id)
		if status != SyncStatusError {
			t.Fatalf("expected sync_status=%q, got %q", SyncStatusError, status)
		}
		if syncedAt.IsZero() {
			t.Fatalf("expected last_synced_at stamped even on failure (audit trail)")
		}
	})
}

// mustGit executes `name args...` in dir and trims stdout. Failures are fatal.
// Named differently from project_style_test.go's mustRun (which is variadic
// without a name argument) to avoid duplicate-declaration collisions in the
// same package.
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