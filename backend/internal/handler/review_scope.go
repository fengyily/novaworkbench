package handler

// review_scope.go — resolve WHAT a requirement-level Review is allowed to
// look at, and describe it to the reviewer model.
//
// The project-level Review has exactly one scope: `git diff base...head`.
// That is enough for a branch review, but it is NOT enough for the
// requirement-level Review, and the difference is the whole reason this file
// exists:
//
//   - A requirement-level Review's fix sub-tasks edit the WORKING TREE. The
//     fix runs `Edit` on files and never commits. A 回归 Review that only
//     looked at `base...head` would therefore re-grade the PRE-FIX code, and
//     the "fix → re-review until it passes" loop would never converge — it
//     would keep reporting the very findings the user just fixed.
//   - The fix may also create BRAND-NEW files (a missing test, a new helper).
//     Those are untracked, so they appear in NEITHER `base...head` NOR
//     `git diff HEAD` — only `git ls-files --others` lists them.
//
// So the scope has three segments:
//
//	1. committed   — `git diff base...head`
//	2. uncommitted — `git diff HEAD` (the working tree's tracked changes)
//	3. untracked   — `git ls-files --others --exclude-standard`
//
// Everything the model is asked to grade is the union of the three, and the
// prompt says so explicitly.
//
// Nothing here checks anything out. The repo may be mid-worktree or dirty;
// reading refs and diffs is safe either way.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// Review scope modes.
const (
	// reviewScopeWorktree means the requirement has a usable local worktree,
	// so the review covers committed + uncommitted + untracked changes.
	reviewScopeWorktree = "worktree"
	// reviewScopeRefs is the degraded mode: no local worktree, so only the
	// committed `base...head` range is in scope. The prompt says so loudly,
	// because a fix that lives in the working tree would be invisible.
	reviewScopeRefs = "refs"
)

// reviewScope is the resolved scope of one requirement-level Review.
type reviewScope struct {
	// Mode is reviewScopeWorktree or reviewScopeRefs.
	Mode string
	// BaseRef / HeadRef are the resolved refs (origin/<x> when it exists,
	// else the local branch).
	BaseRef string
	HeadRef string
	// WorkDir is the directory the diff commands were run in.
	WorkDir string
	// Uncommitted is true when `git diff HEAD` is non-empty.
	Uncommitted bool
	// Untracked lists the files that are not tracked by git yet.
	Untracked []string
	// Block is the Markdown section injected into the review prompt.
	Block string
	// Warning is a non-empty human-facing note (e.g. "the review only covers
	// committed changes"). The UI surfaces it so the user is never surprised
	// by a review that skipped their uncommitted fix.
	Warning string
}

// reviewScopeRequest is what the caller hands the scope resolver.
type reviewScopeRequest struct {
	// RepoDir is the directory the git commands run in — the requirement's
	// worktree when it has one, the project checkout otherwise.
	RepoDir string
	// Branch is the head branch name. Empty is only valid together with
	// HeadOverride (the "不创建分支" mode, where the requirement develops
	// directly on the base branch).
	Branch string
	// Base is the base branch name.
	Base string
	// HeadOverride pins the head ref to a literal ("HEAD"). It is how the
	// no-branch mode gets a meaningful committed range: the requirement
	// commits onto the base branch itself, so the interesting diff is
	// `base...HEAD` inside the checkout, not `base...some-feature-branch`.
	HeadOverride string
	// Env carries the askpass wiring for private HTTPS remotes.
	Env []string
}

// resolveReviewScope resolves the diff refs and then extends the scope with
// the working tree when the requirement has a local worktree.
//
// The returned error is fatal for the caller: a Review whose refs can't be
// resolved would grade an empty diff, so the caller answers 400 instead of
// starting a process.
func resolveReviewScope(ctx context.Context, req reviewScopeRequest) (reviewScope, error) {
	repoDir := req.RepoDir
	var headRef, baseRef, fetchWarning string
	var err error
	if req.HeadOverride != "" {
		// No-branch mode: only the base ref needs resolving; the head is the
		// checkout's current commit.
		baseRef, fetchWarning, err = resolveBaseRef(ctx, repoDir, req.Base, req.Env)
		if err != nil {
			return reviewScope{}, err
		}
		headRef = req.HeadOverride
	} else {
		headRef, baseRef, fetchWarning, err = resolveDiffRefs(ctx, repoDir, req.Branch, req.Base, req.Env)
		if err != nil {
			return reviewScope{}, err
		}
	}
	sc := reviewScope{
		Mode:    reviewScopeRefs,
		BaseRef: baseRef,
		HeadRef: headRef,
		WorkDir: repoDir,
	}
	if fetchWarning != "" {
		sc.Warning = fetchWarning
	}

	// Working-tree segments. A missing / non-git directory must not be fatal:
	// the committed range is still reviewable, so we degrade instead of 400.
	if repoDir == "" {
		sc.Block = buildReviewScopeBlock(sc)
		return sc, nil
	}
	if _, statErr := os.Stat(repoDir); statErr != nil {
		sc.Warning = appendWarning(sc.Warning, fmt.Sprintf("工作目录 %q 不可访问，本次 Review 只覆盖已提交的改动", repoDir))
		sc.Block = buildReviewScopeBlock(sc)
		return sc, nil
	}

	diffOut, diffErr := runGit(ctx, repoDir, req.Env, "diff", "HEAD")
	if diffErr == nil && strings.TrimSpace(diffOut) != "" {
		sc.Uncommitted = true
	}
	untracked, untrackedErr := listUntrackedFiles(ctx, repoDir, req.Env)
	if untrackedErr == nil {
		sc.Untracked = untracked
	}
	if sc.Uncommitted || len(sc.Untracked) > 0 {
		sc.Mode = reviewScopeWorktree
	}
	sc.Block = buildReviewScopeBlock(sc)
	return sc, nil
}

// resolveBaseRef resolves only the base ref (the no-branch mode's head is
// the checkout's HEAD, not a branch name).
func resolveBaseRef(ctx context.Context, repoDir, base string, env []string) (baseRef, fetchWarning string, err error) {
	if strings.TrimSpace(base) == "" {
		return "", "", fmt.Errorf("基础分支不能为空")
	}
	fetch := fetchOrigin(ctx, repoDir, env)
	warning := ""
	if !fetch.ok {
		warning = fmt.Sprintf("⚠️ git fetch origin 失败 (exit=%d): %s", fetch.exitCode, fetch.stderr)
	}
	ref, ok := pickRef(ctx, repoDir, base, env)
	if !ok {
		enrich := "在本地与 origin 上都不存在"
		if !fetch.ok {
			enrich = fmt.Sprintf("本地不存在，且 git fetch origin 失败 (exit=%d): %s。请检查项目 Token / 仓库权限，或手动 git fetch origin 后重试。", fetch.exitCode, fetch.stderr)
		}
		return "", warning, fmt.Errorf("基础分支 %q %s", base, enrich)
	}
	return ref, warning, nil
}

// resolveReviewWorkDir returns the directory a requirement-level Review runs
// in — and therefore the directory whose diff is reviewed.
//
// It is deliberately NOT the same resolution the implementation sub-task
// uses: a sub-task may CREATE a fresh worktree when the persisted one is
// gone, but a Review may not. The scope is resolved against a directory
// BEFORE the run starts, so the Review's directory has to be the one that
// already exists — otherwise the prompt would describe a diff of directory A
// while the reviewer reads directory B.
//
// The resolution is therefore: the no-branch work dir, else the persisted
// worktree when it is on disk, else the project checkout.
func resolveReviewWorkDir(req *model.Requirement, projPath string) string {
	if req == nil {
		return projPath
	}
	if req.NoBranch {
		if wd := resolveNoBranchWorkDir(req, projPath); wd != "" {
			return wd
		}
		return projPath
	}
	if req.WorktreePath != "" {
		if _, err := os.Stat(req.WorktreePath); err == nil {
			return req.WorktreePath
		}
	}
	return projPath
}

// listUntrackedFiles returns the paths git does not track yet (respecting
// .gitignore). An error means the listing is unavailable — the caller keeps
// the empty list rather than failing the whole Review.
func listUntrackedFiles(ctx context.Context, repoDir string, env []string) ([]string, error) {
	out, err := runGit(ctx, repoDir, env, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		files = append(files, line)
	}
	return files, nil
}

// runGit runs a git command in repoDir and returns its stdout. A non-zero
// exit is an error with the (truncated) stderr attached, so the caller can
// tell "not a git repo" from "command failed".
func runGit(ctx context.Context, repoDir string, env []string, args ...string) (string, error) {
	gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(gctx, "git", args...)
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if len(env) > 0 {
		cmd.Env = append(cmd.Env, env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), truncate(strings.TrimSpace(stderr.String()), 200))
	}
	return stdout.String(), nil
}

// buildReviewScopeBlock renders the scope section injected into the review
// prompt. The wording matters: the model has to understand that the three
// segments are ONE change set, and that the untracked files must be READ
// (they are not in any diff).
func buildReviewScopeBlock(sc reviewScope) string {
	var b strings.Builder
	b.WriteString("## 本次 Review 的对比范围\n\n")
	fmt.Fprintf(&b, "- 基础分支（对比基准）：`%s`\n", sc.BaseRef)
	fmt.Fprintf(&b, "- 目标分支：`%s`\n", sc.HeadRef)
	fmt.Fprintf(&b, "- 已提交改动：`git diff %s...%s`\n", sc.BaseRef, sc.HeadRef)
	if sc.Mode == reviewScopeWorktree {
		if sc.Uncommitted {
			b.WriteString("- 工作区未提交改动：`git diff HEAD`（有未提交改动，必须一并 Review）\n")
		} else {
			b.WriteString("- 工作区未提交改动：`git diff HEAD`（当前工作区干净）\n")
		}
		if len(sc.Untracked) > 0 {
			b.WriteString("- 新增未跟踪文件（尚未 git add，**务必用 Read 工具逐个打开查看**，它们不在任何 diff 里）：\n")
			for _, f := range sc.Untracked {
				fmt.Fprintf(&b, "  - `%s`\n", f)
			}
		}
		b.WriteString("\n请把上述三部分合并视为本次要 Review 的**全部**改动。\n")
	} else {
		b.WriteString("- 工作区未提交改动：**本次 Review 不覆盖**（该需求没有本地隔离工作区）\n")
		b.WriteString("\n注意：本次 Review **只覆盖已提交的改动**。若你刚改过代码但尚未提交，" +
			"这些改动不在本次范围内 —— 请在报告里说明这一点，不要因此认为代码「没有问题」。\n")
	}
	return b.String()
}

// appendWarning appends to a warning string, keeping the first one on its
// own line so the UI can render multiple notes readably.
func appendWarning(existing, add string) string {
	if add == "" {
		return existing
	}
	if existing == "" {
		return add
	}
	return existing + "\n" + add
}
