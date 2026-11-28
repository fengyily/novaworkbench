package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FetchBaseBranch —— 跨项目作业用的「只拉取、不动工作区」基线同步。
//
// 它与 SyncToOriginBase 的差别正是跨项目方案需要的那一条：SyncToOriginBase
// 会快进用户的共享检出（因此脏工作区会被拒绝），而跨项目作业读的是一份
// detached worktree，根本不需要碰共享检出 —— 所以这里只移动 remote-tracking
// ref，并把提交锁下来。
//
// 失败语义也不同：拉不到上游就是硬错误（跨项目方案不允许基于陈旧代码产出），
// 只有「根本没有上游可同步」（非 git / 无 origin / 空仓库）才降级。

// TestFetchBaseBranch_LocksUpstreamTip — 拉取成功时锁定 origin/<base> 的
// 提交，且**不动**工作区（不建分支、不快进）。
func TestFetchBaseBranch_LocksUpstreamTip(t *testing.T) {
	const baseBranch = "main"
	upstreamDir := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch="+baseBranch, upstreamDir)

	seedDir := filepath.Join(t.TempDir(), "seed")
	mustGit(t, "", "git", "clone", upstreamDir, seedDir)
	mustGit(t, seedDir, "git", "config", "user.email", "test@nova")
	mustGit(t, seedDir, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(t, seedDir, "git", "add", "README.md")
	mustGit(t, seedDir, "git", "commit", "-m", "init")
	mustGit(t, seedDir, "git", "push", "origin", "HEAD:"+baseBranch)

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "", "git", "clone", upstreamDir, work)
	mustGit(t, work, "git", "config", "user.email", "test@nova")
	mustGit(t, work, "git", "config", "user.name", "Test")

	d := newTestDB(t)
	svc := NewProjectService(d, nil, nil)
	projID := "proj_fetch_ok"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, remote_url, status, default_branch) VALUES (?, ?, ?, ?, ?, ?)`,
		projID, "Test", work, upstreamDir, "ready", baseBranch); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	res, err := svc.FetchBaseBranch(context.Background(), projID, baseBranch, 10*time.Second)
	if err != nil {
		t.Fatalf("FetchBaseBranch: %v", err)
	}
	if res.Skipped {
		t.Fatalf("expected a real sync, got skipped: %+v", res)
	}
	if len(res.SHA) != 40 {
		t.Fatalf("SHA = %q, want 40 hex chars", res.SHA)
	}
	// The locked SHA must be the upstream tip.
	want := mustGit(t, work, "git", "rev-parse", "origin/"+baseBranch)
	if res.SHA != want {
		t.Fatalf("SHA = %q, want the upstream tip %q", res.SHA, want)
	}

	// The working tree must NOT have been moved: same branch, no new branch,
	// HEAD unchanged.
	if branch := mustGit(t, work, "git", "rev-parse", "--abbrev-ref", "HEAD"); branch != baseBranch {
		t.Fatalf("branch = %q, want %q (the fetch must not move the checkout)", branch, baseBranch)
	}
	if branches := mustGit(t, work, "git", "branch", "--list"); strings.Contains(branches, "xreq") {
		t.Fatalf("the fetch created a branch:\n%s", branches)
	}
}

// TestFetchBaseBranch_FollowsUpstream — 上游推进后重新拉取，锁定的提交必须
// 跟着走（这正是「先同步最新基础分支的内容」）。
func TestFetchBaseBranch_FollowsUpstream(t *testing.T) {
	const baseBranch = "main"
	upstreamDir := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch="+baseBranch, upstreamDir)

	seedDir := filepath.Join(t.TempDir(), "seed")
	mustGit(t, "", "git", "clone", upstreamDir, seedDir)
	mustGit(t, seedDir, "git", "config", "user.email", "test@nova")
	mustGit(t, seedDir, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(t, seedDir, "git", "add", "README.md")
	mustGit(t, seedDir, "git", "commit", "-m", "init")
	mustGit(t, seedDir, "git", "push", "origin", "HEAD:"+baseBranch)

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "", "git", "clone", upstreamDir, work)
	mustGit(t, work, "git", "config", "user.email", "test@nova")
	mustGit(t, work, "git", "config", "user.name", "Test")

	d := newTestDB(t)
	svc := NewProjectService(d, nil, nil)
	projID := "proj_fetch_follows"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, remote_url, status, default_branch) VALUES (?, ?, ?, ?, ?, ?)`,
		projID, "Test", work, upstreamDir, "ready", baseBranch); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	first, err := svc.FetchBaseBranch(context.Background(), projID, baseBranch, 10*time.Second)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	// Upstream moves on.
	if err := os.WriteFile(filepath.Join(seedDir, "next.md"), []byte("next"), 0644); err != nil {
		t.Fatalf("write next.md: %v", err)
	}
	mustGit(t, seedDir, "git", "add", "next.md")
	mustGit(t, seedDir, "git", "commit", "-m", "second")
	mustGit(t, seedDir, "git", "push", "origin", "HEAD:"+baseBranch)

	second, err := svc.FetchBaseBranch(context.Background(), projID, baseBranch, 10*time.Second)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if second.SHA == first.SHA {
		t.Fatalf("SHA did not move after an upstream commit (%s)", first.SHA)
	}
}

// TestFetchBaseBranch_SkipsWhenNoUpstream — 非 git / 无 origin 的项目没有上游
// 可同步，降级（Skipped）而不是报错。
func TestFetchBaseBranch_SkipsWhenNoUpstream(t *testing.T) {
	d := newTestDB(t)
	svc := NewProjectService(d, nil, nil)

	t.Run("NotAGitRepo", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "plain")
		if err := os.MkdirAll(plain, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		projID := "proj_fetch_plain"
		if _, err := d.Exec(
			`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
			projID, "Plain", plain, "ready", "main"); err != nil {
			t.Fatalf("insert project: %v", err)
		}
		res, err := svc.FetchBaseBranch(context.Background(), projID, "main", 5*time.Second)
		if err != nil {
			t.Fatalf("FetchBaseBranch: %v", err)
		}
		if !res.Skipped {
			t.Fatalf("expected skipped, got %+v", res)
		}
		if res.Reason == "" {
			t.Fatal("skipped result must explain itself")
		}
		if res.SHA != "" {
			t.Fatalf("skipped result carries a SHA %q", res.SHA)
		}
	})

	t.Run("NoOrigin", func(t *testing.T) {
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

		projID := "proj_fetch_no_origin"
		if _, err := d.Exec(
			`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
			projID, "NoOrigin", work, "ready", "main"); err != nil {
			t.Fatalf("insert project: %v", err)
		}
		res, err := svc.FetchBaseBranch(context.Background(), projID, "main", 5*time.Second)
		if err != nil {
			t.Fatalf("FetchBaseBranch: %v", err)
		}
		if !res.Skipped {
			t.Fatalf("expected skipped, got %+v", res)
		}
	})
}

// TestFetchBaseBranch_FetchFailureIsHardError — 拉不到上游是硬错误。
func TestFetchBaseBranch_FetchFailureIsHardError(t *testing.T) {
	const baseBranch = "main"
	upstreamDir := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch="+baseBranch, upstreamDir)

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
	// origin points at a path that does not exist → the fetch fails.
	mustGit(t, work, "git", "remote", "add", "origin", filepath.Join(t.TempDir(), "gone"))

	d := newTestDB(t)
	svc := NewProjectService(d, nil, nil)
	projID := "proj_fetch_broken"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "Broken", work, "ready", baseBranch); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	res, err := svc.FetchBaseBranch(context.Background(), projID, baseBranch, 10*time.Second)
	if err == nil {
		t.Fatalf("expected a hard error, got %+v", res)
	}
	if res.Skipped {
		t.Fatalf("a failed fetch must not degrade to skipped: %+v", res)
	}
	if !strings.Contains(err.Error(), baseBranch) {
		t.Fatalf("error = %v, want it to name the branch", err)
	}
}

// TestFetchBaseBranch_UpstreamLacksBranchIsNamed — 上游确实没有这条分支时，
// 错误必须点名分支（配置错误，值得指名道姓）。
func TestFetchBaseBranch_UpstreamLacksBranchIsNamed(t *testing.T) {
	const baseBranch = "main"
	upstreamDir := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch="+baseBranch, upstreamDir)

	seedDir := filepath.Join(t.TempDir(), "seed")
	mustGit(t, "", "git", "clone", upstreamDir, seedDir)
	mustGit(t, seedDir, "git", "config", "user.email", "test@nova")
	mustGit(t, seedDir, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(t, seedDir, "git", "add", "README.md")
	mustGit(t, seedDir, "git", "commit", "-m", "init")
	mustGit(t, seedDir, "git", "push", "origin", "HEAD:"+baseBranch)

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "", "git", "clone", upstreamDir, work)
	mustGit(t, work, "git", "config", "user.email", "test@nova")
	mustGit(t, work, "git", "config", "user.name", "Test")

	d := newTestDB(t)
	svc := NewProjectService(d, nil, nil)
	projID := "proj_fetch_missing_branch"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, remote_url, status, default_branch) VALUES (?, ?, ?, ?, ?, ?)`,
		projID, "Test", work, upstreamDir, "ready", baseBranch); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	_, err := svc.FetchBaseBranch(context.Background(), projID, "release/2026q1", 10*time.Second)
	if err == nil {
		t.Fatal("expected an error for a branch the upstream does not have")
	}
	if !strings.Contains(err.Error(), "release/2026q1") {
		t.Fatalf("error = %v, want it to name the missing branch", err)
	}
}
