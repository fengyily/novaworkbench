package handler

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// resolveDiffRefs turns a (branch, base) pair of plain branch names into the
// two git refs the review diff should actually use.
//
// The legacy PR review hard-coded `origin/<base>...origin/<branch>`, which
// only works when both sides have been pushed. A branch Review is explicitly
// for work that may never have left the machine, so each side is resolved
// independently: prefer `origin/<x>` (it's the shared truth when present),
// fall back to the local `<x>`, and error out when neither exists so the
// caller can 400 instead of letting claude burn a round on an empty diff.
//
// A best-effort `git fetch origin --prune` runs first so a freshly pushed
// branch is visible; env carries GitCredentialService.BuildEnv's askpass
// wiring for private HTTPS remotes. Fetch failure is NOT fatal — an offline
// host still reviews local branches fine — but its stderr is surfaced via
// the returned fetchWarning so the SSE stream can show "fetch failed" instead
// of the misleading "branch doesn't exist" the user would otherwise see.
//
// Nothing here checks anything out. The repo may be mid-worktree or have a
// dirty tree; reading two refs is safe either way.
func resolveDiffRefs(ctx context.Context, repoDir, branch, base string, env []string) (headRef, baseRef, fetchWarning string, err error) {
	if strings.TrimSpace(branch) == "" {
		return "", "", "", fmt.Errorf("目标分支不能为空")
	}
	if strings.TrimSpace(base) == "" {
		return "", "", "", fmt.Errorf("基础分支不能为空")
	}

	fetch := fetchOrigin(ctx, repoDir, env)
	var warning string
	if !fetch.ok {
		warning = fmt.Sprintf("⚠️ git fetch origin 失败 (exit=%d): %s", fetch.exitCode, fetch.stderr)
	}

	headRef, ok := pickRef(ctx, repoDir, branch, env)
	if !ok {
		enrich := "在本地与 origin 上都不存在"
		if !fetch.ok {
			enrich = fmt.Sprintf("本地不存在，且 git fetch origin 失败 (exit=%d): %s。请检查项目 Token / 仓库权限，或手动 git fetch origin 后重试。", fetch.exitCode, fetch.stderr)
		}
		return "", "", warning, fmt.Errorf("分支 %q %s", branch, enrich)
	}
	baseRef, ok = pickRef(ctx, repoDir, base, env)
	if !ok {
		enrich := "在本地与 origin 上都不存在"
		if !fetch.ok {
			enrich = fmt.Sprintf("本地不存在，且 git fetch origin 失败 (exit=%d): %s。请检查项目 Token / 仓库权限，或手动 git fetch origin 后重试。", fetch.exitCode, fetch.stderr)
		}
		return "", "", warning, fmt.Errorf("基础分支 %q %s", base, enrich)
	}
	return headRef, baseRef, warning, nil
}

// fetchOutcome is the result of a best-effort `git fetch origin --prune`.
// ok=true means fetch succeeded; ok=false means the caller should treat
// remote refs as possibly stale and surface the stderr/exit for diagnosis.
type fetchOutcome struct {
	ok       bool
	stderr   string // truncated to 200 chars
	exitCode int
}

// fetchOrigin refreshes remote-tracking refs. Best-effort by design: a repo
// with no remote, no network, or no credentials still reviews local refs.
// We now surface stderr+exit so callers can distinguish "branch genuinely
// missing" from "fetch failed". Mirrors the worktree/merge wiring.
func fetchOrigin(ctx context.Context, repoDir string, env []string) fetchOutcome {
	fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(fctx, "git", "fetch", "origin", "--prune")
	cmd.Dir = repoDir
	// Always prepend GIT_TERMINAL_PROMPT=0 — matches merge.go:1459 /
	// worktree.go:211. Prevents ambient-credential hangs when BuildEnv
	// soft-returns nil.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if len(env) > 0 {
		cmd.Env = append(cmd.Env, env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return fetchOutcome{ok: true}
	}
	return fetchOutcome{
		stderr:   truncate(stderr.String(), 200),
		exitCode: exitCodeFromError(err),
	}
}

// truncate keeps the first n bytes and appends an ellipsis when over the
// limit, so fetch stderr — typically a one-line fatal — survives intact
// while multi-line spammy backtraces get capped.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// exitCodeFromError unwraps an *exec.ExitError into its process exit code.
// Returns -1 for non-exit errors (signal kills, context cancellation, spawn
// failures) so the caller's "%d" formatter doesn't render a meaningless 0.
func exitCodeFromError(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// pickRef returns the ref to diff against for a branch name: `origin/<name>`
// when that remote-tracking ref exists, else the local `<name>`, else
// ("", false). A name the user already qualified with `origin/` is checked
// verbatim first so pasting a remote ref works.
func pickRef(ctx context.Context, repoDir, name string, env []string) (string, bool) {
	candidates := []string{"origin/" + name, name}
	if strings.HasPrefix(name, "origin/") || strings.HasPrefix(name, "refs/") {
		candidates = []string{name, strings.TrimPrefix(name, "origin/")}
	}
	for _, ref := range candidates {
		if refExists(ctx, repoDir, ref, env) {
			return ref, true
		}
	}
	return "", false
}

// refExists is `git rev-parse --verify --quiet <ref>^{commit}` — the
// `^{commit}` peel keeps a tag or a branch-shaped file from passing as a
// diffable ref.
func refExists(ctx context.Context, repoDir, ref string, env []string) bool {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(rctx, "git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Dir = repoDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.Run() == nil
}
