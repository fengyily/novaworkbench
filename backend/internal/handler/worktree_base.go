// worktree_base.go — the "develop directly on the base branch" primitives.
//
// The no-branch mode ("直接在 base 分支上开发") used to develop straight in
// the user's project checkout (projects.local_path) and push that checkout's
// base branch. Two problems came with it:
//
//  1. the user's project directory was mutated (checkout + commits + writes)
//     while the agent worked, and
//  2. the push path assumed "base is the current branch", which does not
//     hold for a branch-mode worktree.
//
// The fix is a DETACHED HEAD worktree: `git worktree add --detach <path>
// origin/<base>` gives us the base content in an isolated directory without
// creating any branch (local or remote), and
// `git push origin HEAD:refs/heads/<base>` advances the base branch
// directly.
//
// The worktree path stays the canonical WorktreePath(projectPath, reqID) so
// every existing guard keeps working unchanged:
//
//   - WorktreePathMatches (the cross-requirement drift guard) — one line,
//     unchanged;
//   - util.EncodeClaudeSlug(req.WorktreePath) — the Claude session jsonl
//     path, so `--resume <design_sid>` still resolves;
//   - the three executors that locate the working directory via
//     requirements.worktree_path (sub_task_runner / wizard_orchestration /
//     wizard_subtask) gain isolation with ZERO changes.
package handler

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// Coding-stage worktree shapes. resolveCodingWorktreeMode picks one of them.
const (
	// codingModeBranch — isolated worktree with a real branch checked out
	// (feat/req_xxx or fix/req_xxx). The legacy default.
	codingModeBranch = "branch"
	// codingModeDetached — isolated worktree with a DETACHED HEAD parked on
	// the base line. The no-branch mode: no branch is created anywhere.
	codingModeDetached = "detached"
	// codingModeInPlace — the legacy fallback: develop directly in the
	// project directory (non-git project, or a run without a requirement id).
	codingModeInPlace = "inplace"
)

// ensureDetachedWorktreeCore creates (or converts) the requirement's worktree
// into the "detached HEAD parked on the base line" shape. It never creates a
// branch and never touches the project checkout.
//
//  1. not registered yet → `git worktree add --detach <path> <startRef>`
//  2. registered with a branch checked out (the design stage's leftover
//     feat/<reqID>) → `git checkout --detach` (detach in place, keeping the
//     working tree and the local commits) followed by a best-effort
//     `git merge --ff-only <startRef>`
//  3. already detached → the same best-effort `git merge --ff-only <startRef>`
//
// The merge is deliberately a fast-forward-only attempt: when the worktree
// already carries local commits (the design stage's work) the ff-merge fails
// and we keep the current HEAD. Local commits are NEVER discarded — no
// `reset --hard`, no `checkout --force`.
//
// Returns ErrNotAGitRepo when projectPath is not a git repository.
func ensureDetachedWorktreeCore(projectPath, reqID, baseBranch string, logf func(string)) (string, error) {
	if projectPath == "" || reqID == "" {
		return "", fmt.Errorf("ensure detached worktree: projectPath=%q reqID=%q", projectPath, reqID)
	}
	if _, err := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", ErrNotAGitRepo
	}
	// The canonical path — the drift guard, the Claude session slug and every
	// executor that reads requirements.worktree_path all key on it.
	wtPath := WorktreePath(projectPath, reqID)
	startRef := resolveStartRef(projectPath, baseBranch)
	if startRef == "" {
		startRef = "HEAD"
	}
	if registered, _ := worktreeRegistered(projectPath, wtPath); registered {
		if cur := currentBranch(wtPath); cur != "HEAD" {
			// The design stage left a feat/<reqID> branch checked out here.
			// Detach IN PLACE (no argument) so HEAD stays on the current
			// commit — the working tree and the local commits survive. Do NOT
			// `checkout --detach <startRef>` here: that would move HEAD back to
			// the base line and pull the design stage's work out of the tree.
			if out, err := gitRun(wtPath, "checkout", "--detach"); err != nil {
				return "", fmt.Errorf("detach worktree %s: %s", wtPath, strings.TrimSpace(out))
			}
			if logf != nil {
				logf("🌿 隔离 worktree 已从分支 " + cur + " 脱钩（detached HEAD），未创建新分支")
			}
			// The branch this requirement owns is now a leftover: the
			// no-branch mode's whole promise is that no branch exists for
			// this requirement. HEAD still points at the very same commit
			// (we detached in place), so the safe delete (`-d`, which refuses
			// anything not merged) cannot lose work. A branch that belongs to
			// ANOTHER requirement is left alone.
			if requirementOwnedBranch(cur, reqID) {
				if out, derr := gitRun(wtPath, "branch", "-d", cur); derr != nil {
					log.Printf("[no-branch] %s: 残留分支 %s 未删除（可忽略）: %s", reqID, cur, strings.TrimSpace(out+" "+derr.Error()))
					if logf != nil {
						logf("ℹ️ 残留分支 " + cur + " 未删除（可能被其它 worktree 占用），如需清理请手动执行: git branch -D " + cur)
					}
				} else if logf != nil {
					logf("🧹 已删除不创建分支模式下的残留本地分支: " + cur)
				}
			}
		}
		// Best-effort fast-forward onto the base line. A worktree that already
		// carries local commits cannot fast-forward — keep the current HEAD and
		// continue developing there.
		if _, mErr := gitRun(wtPath, "merge", "--ff-only", startRef); mErr != nil {
			if logf != nil {
				logf("ℹ️ 已有本地提交，无法从 " + startRef + " 快进，继续在当前 HEAD 上开发")
			}
		} else if logf != nil {
			logf("⬆️ 隔离 worktree 已对齐 " + startRef)
		}
		logLatestCommit(wtPath, logf)
		return wtPath, nil
	}
	// Not registered: drop stale metadata + a leftover directory so
	// `git worktree add` can take the path.
	if _, err := os.Stat(wtPath); err == nil {
		_, _ = gitRun(projectPath, "worktree", "prune")
		_ = os.RemoveAll(wtPath)
	}
	if out, err := gitRun(projectPath, "worktree", "add", "--detach", wtPath, startRef); err != nil {
		return "", fmt.Errorf("git worktree add --detach (%s): %s", startRef, strings.TrimSpace(out))
	}
	if logf != nil {
		logf("🌿 已创建 detached 隔离 worktree: " + wtPath + " (基于 " + startRef + ")")
	}
	logLatestCommit(wtPath, logf)
	return wtPath, nil
}

// ensureDetachedWorktreeFrom is the credential-aware wrapper around
// ensureDetachedWorktreeCore. skipSync=false runs the best-effort
// syncBaseBranch first (a plain `git fetch origin <base>` that only moves
// origin/<base> and never touches the working tree); skipSync=true is for
// callers that already fetched an equivalent upstream state (the design
// stage's hard SyncDesignBase gate) and want to avoid duplicate
// "🔄 已同步 origin/<base>" log lines on the SSE panel.
//
// projectID is the projects.id for this checkout — when non-empty the
// handler's GitCredentialService is consulted for a matching platform token
// so private HTTPS repos can be fetched. projectID="" falls back to ambient
// credentials.
func (h *WorktreeHandler) ensureDetachedWorktreeFrom(ctx context.Context, projectID, projectPath, reqID, baseBranch string, skipSync bool, logf func(string)) (string, error) {
	if projectPath == "" || reqID == "" {
		return "", fmt.Errorf("ensure detached worktree: projectPath=%q reqID=%q", projectPath, reqID)
	}
	if !skipSync {
		h.syncBaseBranch(ctx, projectID, projectPath, baseBranch, logf)
	}
	return ensureDetachedWorktreeCore(projectPath, reqID, baseBranch, logf)
}

// EnsureDetachedWorktreeLogged is the log-echoing entry point for the
// no-branch coding path — the detached counterpart of EnsureWorktreeLogged.
// It routes through the package-level defaultWorktreeH singleton so the
// project's platform token is injected into the fetch; when no handler is
// wired (unit tests) it degrades to the credential-free path.
//
// logf may be nil.
func EnsureDetachedWorktreeLogged(ctx context.Context, projectID, projectPath, reqID, baseBranch string, logf func(string)) (string, error) {
	if defaultWorktreeH == nil {
		return ensureDetachedWorktreeNoCred(ctx, projectID, projectPath, reqID, baseBranch, false, logf)
	}
	return defaultWorktreeH.ensureDetachedWorktreeFrom(ctx, projectID, projectPath, reqID, baseBranch, false, logf)
}

// ensureDetachedWorktreeNoCred is the no-handler fallback for
// EnsureDetachedWorktreeLogged. It mirrors ensureWorktreeNoCred: the
// best-effort upstream fetch runs without credential injection, then the
// shared core does the git worktree work.
func ensureDetachedWorktreeNoCred(ctx context.Context, projectID, projectPath, reqID, baseBranch string, skipSync bool, logf func(string)) (string, error) {
	_ = ctx
	_ = projectID
	if !skipSync && projectPath != "" && baseBranch != "" {
		if _, gerr := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); gerr == nil {
			if _, gerr := gitRun(projectPath, "remote", "get-url", "origin"); gerr == nil {
				if out, err := gitRunWithTimeout(projectPath, 30*time.Second,
					[]string{"GIT_TERMINAL_PROMPT=0"}, "fetch", "origin", baseBranch); err != nil {
					if logf != nil {
						logf("ℹ️ 主分支同步跳过（fetch 失败）: " + truncateStr(out, 300))
					}
				} else if logf != nil {
					logf("🔄 已同步 origin/" + baseBranch)
				}
			} else if logf != nil {
				logf("ℹ️ 项目未配置 origin 远程，跳过主分支同步")
			}
		}
	}
	return ensureDetachedWorktreeCore(projectPath, reqID, baseBranch, logf)
}

// resolveCodingWorktreeMode decides which worktree shape a coding run uses.
//
// noBranch wins over branchName — that is the fix for a long-standing defect:
// the LaunchSpec "develop immediately" path sends branch_name="feat/req_xxx"
// AND no_branch=true at the same time (wizard_immediate.go's
// resolveImmediateBranch always returns a non-empty name), and the old
// `p.BranchName != ""` check ran FIRST — so the branch silently won and the
// no_branch intent was dropped.
//
//	reqID == ""      → codingModeInPlace (no requirement row → no worktree anchor)
//	noBranch == true → codingModeDetached
//	branchName != "" → codingModeBranch
//	otherwise        → codingModeInPlace
func resolveCodingWorktreeMode(noBranch bool, branchName, reqID string) string {
	if reqID == "" {
		return codingModeInPlace
	}
	if noBranch {
		return codingModeDetached
	}
	if branchName != "" {
		return codingModeBranch
	}
	return codingModeInPlace
}

// requirementOwnedBranch reports whether a local branch name belongs to the
// given requirement — i.e. it is the requirement's own dev branch.
//
// Two shapes are recognized:
//
//	<prefix>/req_<id>   — the current convention (anchorWorktree /
//	                      resolveImmediateBranch derive it from req.ID)
//	<prefix>/<id>       — the older convention that stripped the "req_"
//	                      prefix (still present in long-lived repos, e.g.
//	                      feat/1a42496198026380)
//
// Anything else (a shared branch, a release branch, another requirement's
// branch) is NOT owned by the requirement and must never be deleted by the
// no-branch cleanup.
func requirementOwnedBranch(branch, reqID string) bool {
	branch = strings.TrimSpace(branch)
	reqID = strings.TrimSpace(reqID)
	if branch == "" || reqID == "" {
		return false
	}
	if strings.HasSuffix(branch, "/"+reqID) {
		return true
	}
	if trimmed := strings.TrimPrefix(reqID, "req_"); trimmed != reqID && trimmed != "" &&
		strings.HasSuffix(branch, "/"+trimmed) {
		return true
	}
	return false
}

// resolveNoBranchWorkDir returns the directory where a no-branch
// requirement's code actually lives.
//
// A current row points at its canonical worktree; a row written by an older
// build (which developed straight in the project directory) has an empty
// worktree_path and falls back to the project checkout — so those rows keep
// working exactly as before. A drifted path (another requirement's
// directory, or a project that moved) also falls back, mirroring
// devBranchAndDir's guard.
func resolveNoBranchWorkDir(reqRow *model.Requirement, projectPath string) string {
	if reqRow == nil || reqRow.WorktreePath == "" {
		return projectPath
	}
	matches, expected := WorktreePathMatches(reqRow.ID, reqRow.WorktreePath, projectPath)
	if !matches {
		log.Printf("[no-branch] %s: drifted worktree_path %q (expected %q) — falling back to project dir",
			reqRow.ID, reqRow.WorktreePath, expected)
		return projectPath
	}
	if _, err := os.Stat(reqRow.WorktreePath); err != nil {
		log.Printf("[no-branch] %s: worktree %q missing on disk — falling back to project dir",
			reqRow.ID, reqRow.WorktreePath)
		return projectPath
	}
	return reqRow.WorktreePath
}

// pushBaseRefspecArgs builds the git arguments that push the current HEAD
// onto the base branch.
//
// The refspec form (`HEAD:refs/heads/<base>`) is the ONLY shape the no-branch
// mode uses, and it is deliberately unconditional:
//
//   - A detached HEAD cannot carry an upstream, so `git push -u` fails with
//     "cannot set up tracking information" — the `-u` flag is never added.
//   - When the coding agent left a branch checked out inside the isolated
//     worktree (it happens: a detached HEAD looks broken, so the agent runs
//     `git checkout -b` — req_1a42496198026380, 2026-10-27), `push origin
//     <base>` would push the LOCAL base branch instead of the agent's
//     commits. The refspec form pushes whatever HEAD points at, whatever
//     branch happens to be checked out.
//
// Either way only refs/heads/<base> moves on the remote — no branch is
// created, on either side.
func pushBaseRefspecArgs(base string) []string {
	return []string{"push", "origin", "HEAD:refs/heads/" + base}
}

// resolveBaseBranch resolves the base branch of a requirement.
//
// The base branch is the baseline the user picked at the design / coding
// stage and it lives on requirements.base_branch. The project's
// default_branch is only the LAST fallback of the three-step chain — the
// auto-push path used to read it as the ONLY source, which pushed the code
// to the project main branch even when the user had chosen another base.
//
//	requirement row's base_branch > project default_branch > "main"
//
// This mirrors loadReqProject (merge.go) and MergeHandler.Push, so every
// stage now agrees on the same value.
func resolveBaseBranch(reqRow *model.Requirement, proj *model.Project) string {
	if reqRow != nil {
		if b := strings.TrimSpace(reqRow.BaseBranch); b != "" {
			return b
		}
	}
	if proj != nil {
		if b := strings.TrimSpace(proj.DefaultBranch); b != "" {
			return b
		}
	}
	return "main"
}

// unpushedCommitCount counts the commits in dir that are not yet reachable
// from the base branch — i.e. the work that would be lost if the worktree
// were deleted right now.
//
// This matters for the no-branch mode: a detached HEAD's commits are
// invisible to uncommittedFiles() (it only lists uncommitted files), so the
// "worktree is dirty" guard in the cleanup endpoint would happily delete a
// worktree that still holds unshipped commits.
//
// Returns 0 when the base ref cannot be resolved or git fails — the caller
// treats 0 as "nothing to protect".
func unpushedCommitCount(dir, base string) int {
	if dir == "" || base == "" {
		return 0
	}
	for _, ref := range []string{"origin/" + base, base} {
		if _, err := gitRun(dir, "rev-parse", "--verify", "--quiet", ref); err != nil {
			continue
		}
		out, err := gitRun(dir, "rev-list", "--count", ref+"..HEAD")
		if err != nil {
			return 0
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(out))
		if cerr != nil {
			return 0
		}
		return n
	}
	return 0
}
