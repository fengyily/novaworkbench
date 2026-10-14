package handler

import (
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
// host still reviews local branches fine.
//
// Nothing here checks anything out. The repo may be mid-worktree or have a
// dirty tree; reading two refs is safe either way.
func resolveDiffRefs(ctx context.Context, repoDir, branch, base string, env []string) (headRef, baseRef string, err error) {
	branch = strings.TrimSpace(branch)
	base = strings.TrimSpace(base)
	if branch == "" {
		return "", "", fmt.Errorf("目标分支不能为空")
	}
	if base == "" {
		return "", "", fmt.Errorf("基础分支不能为空")
	}

	fetchOrigin(ctx, repoDir, env)

	headRef, ok := pickRef(ctx, repoDir, branch, env)
	if !ok {
		return "", "", fmt.Errorf("分支 %q 在本地与 origin 上都不存在", branch)
	}
	baseRef, ok = pickRef(ctx, repoDir, base, env)
	if !ok {
		return "", "", fmt.Errorf("基础分支 %q 在本地与 origin 上都不存在", base)
	}
	return headRef, baseRef, nil
}

// fetchOrigin refreshes remote-tracking refs. Best-effort by design: a repo
// with no remote, no network, or no credentials still reviews local refs.
func fetchOrigin(ctx context.Context, repoDir string, env []string) {
	fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(fctx, "git", "fetch", "origin", "--prune")
	cmd.Dir = repoDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	_ = cmd.Run()
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
