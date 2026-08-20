package handler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotAGitRepo is returned by EnsureWorktree when the project path is not a
// git repository. Callers fall back to the legacy in-place coding path so the
// wizard quick-start (no requirement row) and non-git projects keep working.
var ErrNotAGitRepo = errors.New("not a git repository")

// worktreeRoot is the directory that hosts per-requirement worktrees for the
// given project: ~/.novaworkbench/worktrees/<project_basename>/. Placing it in
// the user home dir avoids permission issues when the project is on a mount
// that is not owned by the current user (e.g. Docker volume mounted as root).
// Git requires worktrees to be on the same filesystem as the repo; if they are
// not, EnsureWorktree will return an error and the caller falls back to
// in-place coding.
func worktreeRoot(projectPath string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback: keep the worktree next to the project (old behaviour).
		parent := filepath.Dir(projectPath)
		base := filepath.Base(projectPath)
		return filepath.Join(parent, base+".worktrees")
	}
	base := filepath.Base(projectPath)
	return filepath.Join(home, ".novaworkbench", "worktrees", base)
}

// WorktreePath returns the absolute path of the worktree for a requirement.
func WorktreePath(projectPath, reqID string) string {
	return filepath.Join(worktreeRoot(projectPath), reqID)
}

// worktreeRegistered reports whether wtPath is a registered worktree of the
// repo at projectPath (parsed from `git worktree list --porcelain`).
func worktreeRegistered(projectPath, wtPath string) (bool, error) {
	out, err := gitRun(projectPath, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	absWt, _ := filepath.Abs(wtPath)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			if ap, e := filepath.Abs(p); e == nil && ap == absWt {
				return true, nil
			}
		}
	}
	return false, nil
}

// EnsureWorktree creates (or reuses) an isolated git worktree for the
// requirement on the given branch, based off baseBranch. Returns the worktree's
// absolute path. Returns ("", nil) when branch is "" (caller wants the legacy
// path). ErrNotAGitRepo lets the caller fall back without erroring.
func EnsureWorktree(projectPath, reqID, branch, baseBranch string) (string, error) {
	if branch == "" {
		return "", nil
	}
	if _, err := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", ErrNotAGitRepo
	}
	if baseBranch == "" {
		baseBranch = "main"
	}
	wtPath := WorktreePath(projectPath, reqID)

	// Already registered → reuse it, ensuring the right branch is checked out.
	if registered, _ := worktreeRegistered(projectPath, wtPath); registered {
		if cur := currentBranch(wtPath); cur != branch && cur != "" {
			if _, err := gitRun(wtPath, "checkout", branch); err != nil {
				return "", fmt.Errorf("checkout %s in worktree: %w", branch, err)
			}
		}
		return wtPath, nil
	}

	// A leftover directory that isn't a registered worktree (half-created,
	// manually copied) would block `git worktree add` — remove it first.
	if _, err := os.Stat(wtPath); err == nil {
		_ = os.RemoveAll(wtPath)
	}

	// Try creating a NEW branch off base. If the branch already exists, fall
	// back to attaching the worktree to it (mirrors the legacy -b/checkout
	// fallback in StartCoding).
	if _, err := gitRun(projectPath, "worktree", "add", "-b", branch, wtPath, baseBranch); err == nil {
		return wtPath, nil
	}
	if out, err := gitRun(projectPath, "worktree", "add", wtPath, branch); err == nil {
		return wtPath, nil
	} else {
		return "", fmt.Errorf("git worktree add: %s: %w", out, err)
	}
}

// RemoveWorktree removes a registered worktree (and prunes stale worktree
// metadata). force=true allows removing a worktree with uncommitted/untracked
// changes (used by the explicit cleanup entry point). A missing worktree is
// treated as success after pruning.
func RemoveWorktree(projectPath, wtPath string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wtPath)
	if _, err := gitRun(projectPath, args...); err != nil {
		// The worktree may already be gone / not registered — prune metadata
		// and treat as removed so cleanup of a half-cleaned repo still clears
		// the DB fields.
		if _, statErr := os.Stat(wtPath); statErr != nil {
			_, _ = gitRun(projectPath, "worktree", "prune")
			return nil
		}
		return err
	}
	_, _ = gitRun(projectPath, "worktree", "prune")
	return nil
}
