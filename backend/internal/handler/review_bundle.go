package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
)

// ── Reviewing an unpushed branch on an Agent Server ─────────────────────────
//
// The agent host mirrors a project through its origin clone, so it only ever
// knows the refs origin knows. A branch that was never pushed used to dead-end
// here with 「分支 "x" 在 Agent 服务器的仓库副本中不存在」 — exactly the branches a
// branch Review exists for (work in progress, nothing published yet).
//
// The fix carries the missing commits over instead of refusing: the same
// git-bundle-over-SFTP transport wizard_remote.go's bundleTransport already
// uses for local-sync coding runs. Properties that matter here:
//
//   - **origin is never written.** Pushing the user's unpublished branch to a
//     shared remote just to read it would be a visible side effect of a
//     read-only operation.
//   - **The shipped commits land in a `refs/nova-review/<review_id>/*`
//     namespace**, never in refs/heads/* — the scratch clone's own branches
//     and HEAD stay untouched, and two concurrent reviews can't collide.
//   - **Nothing is checked out**, on either side. Both ends only read refs,
//     so a dirty working tree or a live worktree is safe.
//   - The refs survive the run on purpose: a later 追问 resumes the same
//     claude session in the same scratch clone and may re-diff.

// reviewRefNamespace is the ref prefix a shipped review ref lands under on the
// agent host. Per-review by construction, so parallel reviews don't collide.
const reviewRefNamespace = "refs/nova-review/"

const (
	// Bundling can walk a lot of history on the full-history fallback.
	reviewBundleTimeout = 5 * time.Minute
	// rev-parse and friends are local metadata reads.
	reviewGitQueryTimeout = 15 * time.Second
)

// reviewRefPlan is one branch that has to be made resolvable on the agent
// host: where it lives locally and what it should be called over there.
type reviewRefPlan struct {
	slot      string // "head" | "base" — keeps the remote ref name stable
	name      string // the branch name the user picked (for messages)
	localFull string // fully-qualified local ref, e.g. refs/heads/feat/x
	sha       string // the commit it points at
	remoteRef string // refs/nova-review/<review_id>/<slot>
}

// resolveRemoteReviewRefs is resolveRemoteDiffRefs plus a repair step: any
// side the agent host is missing but the Nova host has locally is shipped over
// before the diff range is handed to claude.
//
// Returns the two refs to diff, or an error whose message names the actual
// problem (branch nowhere to be found vs. transfer failure).
func resolveRemoteReviewRefs(ctx context.Context, client *gossh.Client, job *store.Job, remoteRepo, localRepo string, rv *model.CodeReview) (headRef, baseRef string, err error) {
	branch := strings.TrimSpace(rv.Branch)
	base := strings.TrimSpace(rv.BaseBranch)
	if branch == "" {
		return "", "", fmt.Errorf("目标分支不能为空")
	}
	if base == "" {
		return "", "", fmt.Errorf("基础分支不能为空")
	}

	headRef, headOK := pickRemoteRef(ctx, client, remoteRepo, branch)
	baseRef, baseOK := pickRemoteRef(ctx, client, remoteRepo, base)
	if headOK && baseOK {
		return headRef, baseRef, nil
	}

	var missing []*reviewRefPlan
	if !headOK {
		missing = append(missing, &reviewRefPlan{slot: "head", name: branch})
	}
	if !baseOK {
		missing = append(missing, &reviewRefPlan{slot: "base", name: base})
	}

	// No local checkout to ship from (project row pointing at a moved or
	// never-cloned directory): keep the pre-existing message, it is still the
	// accurate one in that situation.
	if !dirExists(localRepo) {
		return "", "", fmt.Errorf("%s 在 Agent 服务器的仓库副本中不存在（未推送到 origin 的本地分支请改用「本地执行」）", missingBranchNames(missing))
	}

	for _, p := range missing {
		localRef, ok := pickRef(ctx, localRepo, p.name, nil)
		if !ok {
			return "", "", fmt.Errorf("分支 %q 在 Agent 服务器的仓库副本与本机仓库中都不存在", p.name)
		}
		full, ferr := gitRunWithTimeout(localRepo, reviewGitQueryTimeout, nil, "rev-parse", "--symbolic-full-name", localRef)
		if ferr != nil || strings.TrimSpace(full) == "" {
			return "", "", fmt.Errorf("无法解析本地分支 %q 的完整引用名", p.name)
		}
		sha, serr := gitRunWithTimeout(localRepo, reviewGitQueryTimeout, nil, "rev-parse", "--verify", localRef+"^{commit}")
		if serr != nil || strings.TrimSpace(sha) == "" {
			return "", "", fmt.Errorf("无法解析本地分支 %q 的提交: %v", p.name, serr)
		}
		p.localFull = strings.TrimSpace(full)
		p.sha = strings.TrimSpace(sha)
		p.remoteRef = reviewRefNamespace + rv.ID + "/" + p.slot
	}

	job.Append(store.LogLine{Type: "phase", Content: "📤 " + missingBranchNames(missing) + " 未推送到 origin，正在从本机仓库同步到 Agent 服务器..."})

	// Fast path, per ref: the commit itself may already be in the scratch
	// clone's object store under a different name (force-pushed branch,
	// renamed branch, a tag). Then only a ref has to be created — no transfer.
	var ship []*reviewRefPlan
	for _, p := range missing {
		if remoteRevExists(ctx, client, remoteRepo, p.sha) && updateRemoteRef(ctx, client, remoteRepo, p.remoteRef, p.sha) == nil {
			job.Append(store.LogLine{Type: "message", Content: "♻️ " + p.name + " 的提交已存在于 Agent 服务器，直接建立引用"})
			continue
		}
		ship = append(ship, p)
	}

	if len(ship) > 0 {
		remoteBundle := "/tmp/nova-agent/" + rv.ProjectID + "/review-" + rv.ID + ".up.bundle"
		if berr := shipReviewRefsViaBundle(ctx, client, job, localRepo, remoteRepo, remoteBundle, ship); berr != nil {
			return "", "", fmt.Errorf("本地分支同步到 Agent 服务器失败: %w（也可改用「本地执行」）", berr)
		}
	}

	for _, p := range missing {
		if !remoteRevExists(ctx, client, remoteRepo, p.remoteRef) {
			return "", "", fmt.Errorf("分支 %q 同步到 Agent 服务器后仍不可用", p.name)
		}
		if p.slot == "head" {
			headRef = p.remoteRef
		} else {
			baseRef = p.remoteRef
		}
		job.Append(store.LogLine{Type: "message", Content: "✅ 已同步分支 " + p.name + " → " + p.remoteRef})
	}
	return headRef, baseRef, nil
}

// shipReviewRefsViaBundle packs the given refs into a git bundle, uploads it
// over SFTP and fetches it into the agent clone's refs/nova-review/* namespace.
//
// Two bundle shapes are tried in order:
//
//  1. **Incremental** (`--not --remotes=origin`) — only the commits origin
//     doesn't already have, typically a handful. The agent clone comes from
//     that same origin, so it supplies the prerequisites.
//  2. **Self-contained** — the full history of the refs. Needed when the
//     incremental range is empty (git refuses to create an empty bundle) or
//     when the agent clone lacks a prerequisite (force-pushed origin history).
//
// The fallback is driven by the actual failure — bundle creation OR the remote
// fetch — because a thin bundle that builds fine can still be unusable over
// there, and that only shows up at fetch time.
func shipReviewRefsViaBundle(ctx context.Context, client *gossh.Client, job *store.Job, localRepo, remoteRepo, remoteBundle string, plans []*reviewRefPlan) error {
	tmp, err := os.CreateTemp("", "nova-review-*.bundle")
	if err != nil {
		return fmt.Errorf("创建本地 bundle 临时文件失败: %w", err)
	}
	localBundle := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(localBundle)

	refs := make([]string, 0, len(plans))
	specs := make([]string, 0, len(plans))
	for _, p := range plans {
		refs = append(refs, p.localFull)
		specs = append(specs, shellQuoteSingle("+"+p.localFull+":"+p.remoteRef))
	}

	attempt := func(incremental bool) error {
		if berr := createReviewBundle(localRepo, localBundle, refs, incremental); berr != nil {
			return berr
		}
		if perr := client.PutFile(localBundle, remoteBundle, 0600); perr != nil {
			return fmt.Errorf("上传 git bundle 失败: %w", perr)
		}
		cmd := "cd " + shellQuoteSingle(remoteRepo) + " && git fetch " + shellQuoteSingle(remoteBundle) + " " + strings.Join(specs, " ")
		var tail bytes.Buffer
		exit, eerr := client.Exec(ctx, cmd, "", nil, io.Discard, &tail)
		if eerr != nil {
			return fmt.Errorf("在 Agent 服务器 fetch bundle 失败: %w", eerr)
		}
		if exit != 0 {
			return fmt.Errorf("在 Agent 服务器 fetch bundle 失败（exit=%d）: %s", exit, truncateStr(strings.TrimSpace(tail.String()), 300))
		}
		return nil
	}

	// Always clean the uploaded bundle up, including on the retry path —
	// /tmp on a shared agent host is not ours to leak into.
	defer client.Exec(context.Background(), "rm -f "+shellQuoteSingle(remoteBundle), "", nil, nil, nil)

	if err := attempt(true); err != nil {
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ 增量 bundle 不可用（" + err.Error() + "），改为打包完整历史重试..."})
		if ferr := attempt(false); ferr != nil {
			return ferr
		}
	}
	job.Append(store.LogLine{Type: "message", Content: "⬆️ 已上传本地分支的提交到 Agent 服务器"})
	return nil
}

// createReviewBundle writes the bundle holding refs.
//
// incremental excludes everything origin already has (`--not
// --remotes=origin`), which is what keeps the transfer to the handful of
// unpushed commits; git refuses to create a bundle when that range turns out
// to be empty, which is one of the two reasons the caller retries with a
// self-contained bundle.
func createReviewBundle(localRepo, bundlePath string, refs []string, incremental bool) error {
	args := append([]string{"bundle", "create", bundlePath}, refs...)
	if incremental {
		args = append(args, "--not", "--remotes=origin")
	}
	if _, err := gitRunWithTimeout(localRepo, reviewBundleTimeout, nil, args...); err != nil {
		return fmt.Errorf("生成 git bundle 失败: %w", err)
	}
	return nil
}

// remoteRevExists is `git rev-parse --verify --quiet <rev>^{commit}` on the
// agent host: true when the scratch clone can resolve rev to a commit.
func remoteRevExists(ctx context.Context, client *gossh.Client, repoDir, rev string) bool {
	if strings.TrimSpace(rev) == "" {
		return false
	}
	cmd := "cd " + shellQuoteSingle(repoDir) + " && git rev-parse --verify --quiet " + shellQuoteSingle(rev+"^{commit}")
	exit, err := client.Exec(ctx, cmd, "", nil, io.Discard, nil)
	return err == nil && exit == 0
}

// updateRemoteRef points ref at sha in the agent clone without fetching
// anything — used when the commit is already in its object store.
func updateRemoteRef(ctx context.Context, client *gossh.Client, repoDir, ref, sha string) error {
	cmd := "cd " + shellQuoteSingle(repoDir) + " && git update-ref " + shellQuoteSingle(ref) + " " + shellQuoteSingle(sha)
	exit, err := client.Exec(ctx, cmd, "", nil, io.Discard, nil)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("git update-ref 失败（exit=%d）", exit)
	}
	return nil
}

// missingBranchNames renders the branches being shipped for one log line.
func missingBranchNames(plans []*reviewRefPlan) string {
	names := make([]string, 0, len(plans))
	for _, p := range plans {
		names = append(names, "分支 "+p.name)
	}
	return strings.Join(names, " / ")
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
