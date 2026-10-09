package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/service"
)

// ErrNotAGitRepo is returned by EnsureWorktree when the project path is not a
// git repository. Callers fall back to the legacy in-place coding path so the
// wizard quick-start (no requirement row) and non-git projects keep working.
var ErrNotAGitRepo = errors.New("not a git repository")

// WorktreeHandler bundles the per-requirement worktree helpers with the
// service-layer dependency (GitCredentialService) they need to inject the
// project's platform token into `git fetch` / `git pull` calls. The handler
// is created once in main.go and stored on WizardHandler; the package-level
// free functions (EnsureWorktree / EnsureWorktreeLogged) remain as thin
// wrappers that route through SetDefaultWorktreeHandler's singleton so
// legacy callers (including the worktree_recovery_test.go unit tests, which
// never set the singleton) keep compiling unchanged.
type WorktreeHandler struct {
	gitCredSvc *service.GitCredentialService
	projectSvc *service.ProjectService
}

// NewWorktreeHandler wires the per-handler dependencies. projectSvc is
// currently retained for symmetry with the other handlers and as the
// obvious place to add project lookups in the future (no current call site
// uses it from this handler — it's plumbed in for the day syncBaseBranch
// needs to read project metadata beyond what git remote get-url returns).
func NewWorktreeHandler(projectSvc *service.ProjectService, gitCredSvc *service.GitCredentialService) *WorktreeHandler {
	return &WorktreeHandler{
		gitCredSvc: gitCredSvc,
		projectSvc: projectSvc,
	}
}

// defaultWorktreeH is the package-level singleton used by the
// EnsureWorktree / EnsureWorktreeLogged free functions. main.go calls
// SetDefaultWorktreeHandler once at startup; tests that don't wire the
// dependency (e.g. worktree_recovery_test.go) see nil and the free
// functions degrade to "no-credential-injection" mode automatically.
var defaultWorktreeH *WorktreeHandler

// SetDefaultWorktreeHandler injects the singleton used by the free
// functions. Safe to call once at startup and again from tests.
func SetDefaultWorktreeHandler(h *WorktreeHandler) {
	defaultWorktreeH = h
}

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

// WorktreePathMatches reports whether the persisted worktree_path on a
// requirement row still corresponds to the path WorktreePath would compute
// for (projectPath, reqID). The expected-path return value is what callers
// should swap to when matches==false.
//
// This is the single chokepoint every wizard entry point uses to decide
// whether to trust a persisted worktree_path or rebuild it. Without it,
// a project that moved local_path (or a DB row whose path was written for a
// different requirement) keeps pointing Claude at the previous host's
// directory — which is exactly the cross-requirement contamination that
// motivated this helper (req_c46e8d66491ae3a2 jumping into
// req_cd5079181af7335a's directory because a stale worktree_path was
// reused verbatim by anchorWorktree).
//
// Path comparison goes through filepath.Clean on both sides so trailing
// slashes / redundant ".." don't cause false negatives. Both "" storedPath
// (never anchored) and a foreign storedPath (different reqID / different
// local_path lineage) report matches==false — the caller decides whether
// to clear the DB row or just rebuild the worktree without touching the row.
func WorktreePathMatches(reqID, storedPath, projectPath string) (matches bool, expected string) {
	expected = WorktreePath(projectPath, reqID)
	if storedPath == "" {
		return false, expected
	}
	return filepath.Clean(storedPath) == filepath.Clean(expected), expected
}

// worktreeRegistered reports whether wtPath is a registered worktree of the
// repo at projectPath (parsed from `git worktree list --porcelain`).
//
// Path matching goes through filepath.EvalSymlinks on both sides so a
// symlinked home directory (common on macOS, where /var/folders/... and
// /private/var/folders/... refer to the same on-disk location but compare
// unequal as strings) doesn't make the function silently return false. A
// fallback to filepath.Abs keeps the comparison working when the wtPath
// doesn't exist yet — EvalSymlinks requires the path to be present.
func worktreeRegistered(projectPath, wtPath string) (bool, error) {
	out, err := gitRun(projectPath, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	absWt := canonicalPath(wtPath)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			if canonicalPath(p) == absWt {
				return true, nil
			}
		}
	}
	return false, nil
}

// canonicalPath returns the symlink-resolved absolute path of p. When p
// doesn't exist (e.g. we're about to register a brand-new worktree), fall
// back to filepath.Abs so the caller still gets a usable string. Used by
// worktreeRegistered to make path comparisons immune to symlinked parents.
func canonicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	abs, _ := filepath.Abs(p)
	return abs
}

// gitRunWithTimeout is gitRun with an additional context timeout and an
// arbitrary extra-env slice ("KEY=VALUE" pairs). Used by syncBaseBranch so the
// `git fetch` call cannot hang on credential prompts (GIT_TERMINAL_PROMPT=0
// disables interactive auth) and is bounded by ~30s even when the remote is
// unreachable. Failures return trimmed stderr so the caller can surface them.
func gitRunWithTimeout(dir string, timeout time.Duration, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// syncBaseBranch fetches the project's configured main branch from origin into
// the project checkout so any worktree created afterwards starts from the
// latest upstream code. Best-effort by design: no remote / offline / missing
// ref / fetch failure must never block the wizard. logf may be nil.
//
// Uses GIT_TERMINAL_PROMPT=0 + a 30s timeout on the fetch itself so private
// repos with missing/unauthorized credentials can't hang the whole Job
// waiting for an interactive prompt.
//
// projectID is the projects.id for this checkout. When non-empty the
// service-layer GitCredentialService is queried for a matching platform
// token and, if found, a temp askpass script is generated to inject HTTPS
// credentials into the fetch. SSH remotes and projects without a token
// silently fall back to ambient credentials (no error, no log noise).
func (h *WorktreeHandler) syncBaseBranch(ctx context.Context, projectID, projectPath, baseBranch string, logf func(string)) {
	if projectPath == "" || baseBranch == "" {
		return
	}
	if _, err := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		return
	}
	if _, err := gitRun(projectPath, "remote", "get-url", "origin"); err != nil {
		if logf != nil {
			logf("ℹ️ 项目未配置 origin 远程，跳过主分支同步")
		}
		return
	}
	// Do NOT use the refspec form (<base>:<base>) — git refuses to update a
	// branch that is currently checked out in the project dir or in any
	// worktree. Plain fetch only moves origin/<base>, which is what we branch
	// off of, and never touches the working tree.
	credEnv, credCleanup := h.credEnvFor(ctx, projectID)
	defer credCleanup()
	env := append([]string{"GIT_TERMINAL_PROMPT=0"}, credEnv...)
	out, err := gitRunWithTimeout(projectPath, 30*time.Second, env, "fetch", "origin", baseBranch)
	if err != nil {
		if logf != nil {
			logf("ℹ️ 主分支同步跳过（fetch 失败）: " + truncateStr(out, 300))
		}
		return
	}
	if logf != nil {
		logf("🔄 已同步 origin/" + baseBranch)
		// Surface the freshly-fetched upstream tip so the user can verify the
		// local checkout is level with the remote. Best-effort: a repo without
		// origin/<base> (fetch created no ref) simply logs nothing.
		if tip, terr := gitRun(projectPath, "log", "-1", "origin/"+baseBranch,
			"--format=%h %s (%an, %ad)", "--date=format:%Y-%m-%d %H:%M"); terr == nil {
			if tip = strings.TrimSpace(tip); tip != "" {
				logf("📌 origin/" + baseBranch + " 最新提交: " + tip)
			}
		}
	}
}

// credEnvFor is a thin wrapper around the package-level
// gitCredentialEnvForProject helper that uses the handler's stored
// gitCredSvc. It exists so a missing/nil gitCredSvc (unit tests, partial
// wiring) degrades to ambient credentials instead of panicking.
func (h *WorktreeHandler) credEnvFor(ctx context.Context, projectID string) ([]string, func()) {
	if h == nil || h.gitCredSvc == nil || projectID == "" {
		noop := func() {}
		return nil, noop
	}
	env, cleanup, _ := h.gitCredSvc.BuildEnv(ctx, projectID)
	return env, cleanup
}

// logLatestCommit emits a one-line "📌 分支最新提交: <hash> <subject> (<author>, <date>)"
// hint for the HEAD commit of dir, so the user can cross-check which commit the
// worktree ended up on after a sync/checkout. Best-effort: nil-safe (logf may be
// nil), and any git error (unborn branch / not a repo) is silently ignored — the
// commit line is a convenience, never a gate.
func logLatestCommit(dir string, logf func(string)) {
	if logf == nil || dir == "" {
		return
	}
	out, err := gitRun(dir, "log", "-1",
		"--format=%h %s (%an, %ad)", "--date=format:%Y-%m-%d %H:%M")
	if err != nil {
		return
	}
	if out = strings.TrimSpace(out); out != "" {
		logf("📌 分支最新提交: " + out)
	}
}

// resolveStartRef returns the ref a fresh requirement branch should be created
// from, preferring the freshly-fetched remote-tracking ref. Returns "" when no
// candidate exists — the caller then falls back to its legacy off-HEAD
// strategy so behaviour on a repo without origin/<base> is unchanged.
func resolveStartRef(projectPath, baseBranch string) string {
	if baseBranch == "" {
		return ""
	}
	if _, err := gitRun(projectPath, "rev-parse", "--verify", "--quiet", "origin/"+baseBranch); err == nil {
		return "origin/" + baseBranch
	}
	if _, err := gitRun(projectPath, "rev-parse", "--verify", "--quiet", baseBranch); err == nil {
		return baseBranch
	}
	return ""
}

// EnsureWorktreeLogged is the log-echoing variant of EnsureWorktree. See
// EnsureWorktree for the high-level contract; the differences are:
//
//  1. Before anything else, syncBaseBranch fetches the project's main branch
//     from origin into the project checkout so the new worktree starts from
//     upstream HEAD (not the project's possibly stale local HEAD). Best-effort:
//     missing remote / offline / fetch failure is logged via logf and ignored.
//  2. resolveStartRef picks the best start ref (origin/<baseBranch> if present,
//     else <baseBranch>). The new-worktree strategy order uses this ref as
//     strategy 1' (was previously off HEAD), so a freshly-fetched upstream
//     becomes the requirement branch's base. Strategies 2 (off HEAD) and 3
//     (attach existing branch) are preserved unchanged for repos without a
//     remote or where the base ref doesn't exist.
//  3. Reusing an existing worktree tries a best-effort `merge --ff-only
//     <startRef>` after checking out the target branch. Failure is logged and
//     the worktree is returned unchanged — local commits are NEVER discarded
//     by this function (no --force, no reset --hard).
//
// logf may be nil; when non-nil it receives one-line Chinese hints that should
// be surfaced to the user (typically wired to job.Append(store.LogLine{...})).
//
// This is a thin wrapper around ensureWorktreeFrom that hardcodes skipSync to
// false — callers must never be allowed to suppress the best-effort fetch from
// this entry point, otherwise an explicit "fetch the upstream before branching"
// guarantee is silently lost. The only caller that legitimately wants to skip
// the inner sync is the design stage's hard-sync prologue (which has already
// fetched via service.ProjectService.SyncDesignBase and would otherwise print
// duplicate "🔄 已同步 origin/<base>" lines); that path is wired directly to
// ensureWorktreeFrom with skipSync=true from wizard_common.go.
//
// projectID is the projects.id for this checkout. When non-empty the
// defaultWorktreeH's GitCredentialService is queried for a matching
// platform token and, if found, a temp askpass script is generated to
// inject HTTPS credentials into the inner syncBaseBranch. projectID=""
// (e.g. unit tests) skips credential injection and falls back to ambient
// credentials — equivalent to the pre-method behaviour.
func EnsureWorktreeLogged(ctx context.Context, projectID, projectPath, reqID, branch, baseBranch string, logf func(string)) (string, error) {
	if defaultWorktreeH == nil {
		// No handler wired (e.g. unit tests): fall back to no-credential
		// mode by routing through the no-credential free function.
		return ensureWorktreeNoCred(ctx, projectID, projectPath, reqID, branch, baseBranch, false, logf)
	}
	return defaultWorktreeH.ensureWorktreeFrom(ctx, projectID, projectPath, reqID, branch, baseBranch, false, logf)
}

// ensureWorktreeNoCred is the no-handler fallback used by EnsureWorktree
// and EnsureWorktreeLogged when SetDefaultWorktreeHandler was never called.
// It mirrors the strategy order of (*WorktreeHandler).ensureWorktreeFrom
// minus the credential-injecting syncBaseBranch — equivalent to the
// pre-method behaviour where the function was a package-level free
// function. Kept separate from the method so the test file's calls (which
// pre-date the WorktreeHandler introduction) keep working without any
// DI wiring.
func ensureWorktreeNoCred(ctx context.Context, projectID, projectPath, reqID, branch, baseBranch string, skipSync bool, logf func(string)) (string, error) {
	// Suppress unused-param lints; ctx/projectID are accepted for signature
	// symmetry with the method but the no-credential path doesn't need them.
	_ = ctx
	_ = projectID
	if branch == "" {
		return "", nil
	}
	if _, err := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", ErrNotAGitRepo
	}
	if !skipSync {
		// Inline no-credential best-effort fetch. Equivalent to the
		// pre-method free function; duplicated here (instead of factored
		// into a named helper) so the only `syncBaseBranch` declaration
		// in the package is the new method on WorktreeHandler.
		if projectPath != "" && baseBranch != "" {
			if _, gerr := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); gerr == nil {
				if _, gerr := gitRun(projectPath, "remote", "get-url", "origin"); gerr == nil {
					if out, err := gitRunWithTimeout(projectPath, 30*time.Second,
						[]string{"GIT_TERMINAL_PROMPT=0"}, "fetch", "origin", baseBranch); err != nil {
						if logf != nil {
							logf("ℹ️ 主分支同步跳过（fetch 失败）: " + truncateStr(out, 300))
						}
					} else if logf != nil {
						logf("🔄 已同步 origin/" + baseBranch)
						if tip, terr := gitRun(projectPath, "log", "-1", "origin/"+baseBranch,
							"--format=%h %s (%an, %ad)", "--date=format:%Y-%m-%d %H:%M"); terr == nil {
							if tip = strings.TrimSpace(tip); tip != "" {
								logf("📌 origin/" + baseBranch + " 最新提交: " + tip)
							}
						}
					}
				} else if logf != nil {
					logf("ℹ️ 项目未配置 origin 远程，跳过主分支同步")
				}
			}
		}
	}
	return ensureWorktreeCore(projectPath, reqID, branch, baseBranch, logf)
}

// ensureWorktreeCore is the credential-free strategy-order body shared by
// ensureWorktreeNoCred. It's a near-verbatim extraction of the original
// ensureWorktreeFrom body — broken out so the method and the no-handler
// fallback can both reuse it without one having to call the other.
func ensureWorktreeCore(projectPath, reqID, branch, baseBranch string, logf func(string)) (string, error) {
	startRef := resolveStartRef(projectPath, baseBranch)
	wtPath := WorktreePath(projectPath, reqID)
	// Already registered → reuse it, ensuring the right branch is checked out.
	if registered, _ := worktreeRegistered(projectPath, wtPath); registered {
		cur := currentBranch(wtPath)
		reuse := cur == branch || cur == ""
		if !reuse {
			if _, err := gitRun(wtPath, "checkout", branch); err != nil {
				if _, exErr := gitRun(projectPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); exErr != nil {
					if logf != nil {
						logf("ℹ️ 已注册 worktree 的目标分支 " + branch + " 在本地不存在，移除后重建 worktree")
					}
					_, _ = gitRun(projectPath, "worktree", "remove", "--force", wtPath)
					_, _ = gitRun(projectPath, "worktree", "prune")
				} else {
					return "", fmt.Errorf("checkout %s in worktree: %w", branch, err)
				}
			} else {
				reuse = true
			}
		}
		if reuse {
			if startRef != "" {
				if _, mErr := gitRun(wtPath, "merge", "--ff-only", startRef); mErr == nil {
					if logf != nil {
						logf("⬆️ 已从 " + startRef + " 更新")
					}
				} else if logf != nil {
					logf("ℹ️ 已有改动，无法从主分支快进更新，继续在当前分支工作")
				}
			}
			logLatestCommit(wtPath, logf)
			return wtPath, nil
		}
	}
	if _, err := os.Stat(wtPath); err == nil {
		_ = os.RemoveAll(wtPath)
	}
	if startRef != "" {
		if out, err := gitRun(projectPath, "worktree", "add", "-b", branch, wtPath, startRef); err == nil {
			logLatestCommit(wtPath, logf)
			return wtPath, nil
		} else if !worktreeAddBranchExists(out, err) {
			if out != "" {
				return "", fmt.Errorf("git worktree add (%s): %s", startRef, out)
			}
			return "", fmt.Errorf("git worktree add (%s): %w", startRef, err)
		}
	}
	if out, err := gitRun(projectPath, "worktree", "add", "-b", branch, wtPath); err == nil {
		logLatestCommit(wtPath, logf)
		return wtPath, nil
	} else if !worktreeAddBranchExists(out, err) {
		if out != "" {
			return "", fmt.Errorf("git worktree add: %s", out)
		}
		return "", fmt.Errorf("git worktree add: %w", err)
	}
	if baseBranch != "" {
		if out, err := gitRun(projectPath, "worktree", "add", "-b", branch, wtPath, baseBranch); err == nil {
			logLatestCommit(wtPath, logf)
			return wtPath, nil
		} else if !worktreeAddBranchExists(out, err) {
			if out != "" {
				return "", fmt.Errorf("git worktree add (%s): %s", baseBranch, out)
			}
			return "", fmt.Errorf("git worktree add (%s): %w", baseBranch, err)
		}
	}
	if out, err := gitRun(projectPath, "worktree", "add", wtPath, branch); err != nil {
		if out != "" {
			return "", fmt.Errorf("git worktree add: %s", out)
		}
		return "", fmt.Errorf("git worktree add: %w", err)
	}
	logLatestCommit(wtPath, logf)
	return wtPath, nil
}

// ensureWorktreeFrom is the shared body of EnsureWorktreeLogged / EnsureWorktree.
// skipSync=false keeps the legacy best-effort syncBaseBranch step before
// branching (default for every public caller); skipSync=true skips it for
// callers that have already done an equivalent fetch upstream and want to
// avoid duplicate "🔄 已同步 origin/<base>" log lines. See the contract block
// on EnsureWorktreeLogged above for the full behaviour list.
//
// projectID is threaded down to syncBaseBranch so the inner fetch can
// inject the project's platform token. When empty (legacy / no-credential
// path) the fetch falls back to ambient credentials — the same behaviour
// as before this method conversion.
func (h *WorktreeHandler) ensureWorktreeFrom(ctx context.Context, projectID, projectPath, reqID, branch, baseBranch string, skipSync bool, logf func(string)) (string, error) {
	if branch == "" {
		return "", nil
	}
	if _, err := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", ErrNotAGitRepo
	}
	if !skipSync {
		h.syncBaseBranch(ctx, projectID, projectPath, baseBranch, logf)
	}
	return ensureWorktreeCore(projectPath, reqID, branch, baseBranch, logf)
}

// EnsureWorktree is the log-less thin wrapper that all legacy callers use.
// New code should prefer EnsureWorktreeLogged so the user sees the sync lines
// in the Job panel. Signature and behaviour are preserved: this is the same
// function as before, just routed through ensureWorktreeFrom with a nil
// logf and skipSync hardcoded to false. The strategy order is:
//
//	1'. origin/<baseBranch> if syncBaseBranch + resolveStartRef produced one,
//	    else skip (legacy callers without a remote never had one).
//	1.  off HEAD (`worktree add -b <branch> <path>`) — always valid.
//	2.  off baseBranch if the user supplied one and the branch name conflicts.
//	3.  attach an existing branch (`worktree add <path> <branch>`).
//
// baseBranch is a hint (typically the project's default branch such as "main"
// or "master"). We do NOT trust it blindly — git fails with "invalid reference:
// <base>" when the supplied ref doesn't exist, and the previous code's fallback
// `git worktree add <path> <branch>` also failed because the branch never got
// created in that case.
//
// Returns ("", nil) when branch is "" (caller wants the legacy path).
// ErrNotAGitRepo lets the caller fall back without erroring.
//
// ctx is forwarded to the inner syncBaseBranch (so callers that need a
// deadline can supply one). projectID="" preserves the legacy
// no-credential behaviour.
func EnsureWorktree(ctx context.Context, projectID, projectPath, reqID, branch, baseBranch string) (string, error) {
	if defaultWorktreeH == nil {
		return ensureWorktreeNoCred(ctx, projectID, projectPath, reqID, branch, baseBranch, false, nil)
	}
	return defaultWorktreeH.ensureWorktreeFrom(ctx, projectID, projectPath, reqID, branch, baseBranch, false, nil)
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

// worktreeAddBranchExists reports whether a failed `git worktree add -b
// <branch>` attempt collided with an existing local branch — the
// recoverable fall-through signal in ensureWorktreeFrom's strategy order.
//
// git writes the "branch already exists" diagnostic to STDERR (not stdout),
// so the previous code's `strings.Contains(out, "already exists")` check
// silently missed it in any locale where stdout happened to be empty (i.e.
// every locale — the message only ever lands on stderr). It also missed the
// Chinese translation ("已经存在") and the matching "分支 '...' 已经存在"
// wording. This helper greps BOTH channels AND the wrapped stderr error so
// the fall-through works regardless of locale or stream routing.
func worktreeAddBranchExists(stdoutText string, err error) bool {
	combined := stdoutText
	if err != nil {
		combined += " " + err.Error()
	}
	// English / C locale
	if strings.Contains(combined, "already exists") {
		return true
	}
	// Simplified Chinese
	if strings.Contains(combined, "已经存在") {
		return true
	}
	return false
}
