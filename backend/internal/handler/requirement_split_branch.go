package handler

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// 「方案拆分」的栈底分支准备。
//
// 拆分产出的子需求全部从**主需求自己的开发分支**（feat/req_<master>）上开枝，
// 它们的 PR 也全部打到这条分支上 —— 这就是堆叠 PR（stacked PR）：
//
//	feat/req_<child>  ──PR──▶  feat/req_<master>  ──PR──▶  <用户选的来源分支 A>
//
// 拆分的瞬间要做两件事，缺一不可：
//
//  1. **把栈底建出来**：feat/req_<master> 必须由用户选的 A 分支 checkout
//     出来。用户随时可能换来源分支，所以这一步要能重跑，并且**只快进、
//     绝不丢弃已有提交** —— 主需求分支上可能已经有提交（甚至已经有子需求
//     的 PR 合进去了）。
//  2. **把栈底推上远端**：堆叠 PR 的 base 必须存在于 origin 上，否则子需求
//     的 PR 全部无法创建（gh pr create --base 会因为 base 不在远端而失败）。
//     推不上去 ⇒ 整个拆分失败、不落库任何子需求 —— 半截的拆分比不拆分更糟。
//
// 降级策略：项目不是 git 仓库 / 主需求是「不创建分支」模式 / 运行环境缺少
// 项目与凭据服务时，本文件什么都不做，基线由 service 层按主需求自身推导。
// 拆分照常发生，只是拿不到堆叠栈。

// 拆分时的 git 超时。比对推送链路（pushShellStepTimeout = 10m）短得多：
// 这里跑在同步的 HTTP 请求里，不能让用户对着一个转圈按钮等十分钟。
//
// 这些数字是**用户体验**的一部分：确认子需求只有一个同步请求，用户全程盯着
// 一个「创建中…」按钮。远端不可达是最常见的失败原因，所以每一次远端往返都要
// 有自己的短上限，而不是让它们加起来变成好几分钟。
const (
	// splitLsRemoteTimeout — 查询远端某个分支的 sha（一次廉价往返）。
	// 这同时充当「远端是否可达」的探针。
	splitLsRemoteTimeout = 15 * time.Second
	// splitFetchTimeout — best-effort 拉取来源分支。
	splitFetchTimeout = 30 * time.Second
	// splitBranchGitTimeout — 建分支 / 对齐分支（纯本地操作）。
	splitBranchGitTimeout = 45 * time.Second
	// splitPushTimeout — 推送栈底分支到 origin。
	splitPushTimeout = 30 * time.Second
	// splitRecheckTimeout — 推送失败后回头确认远端是否已有该分支。
	// 这是一次廉价的「远端还在吗」探测，给它一个短上限。
	splitRecheckTimeout = 10 * time.Second
	// splitStackBudget — 整个栈底准备阶段的**总**预算。
	//
	// 单步超时之和可达数分钟，而用户全程只能盯着一个「创建中…」按钮。
	// 总预算把最坏情况钉死：超了就返回明确错误，而不是让请求无限期挂着。
	splitStackBudget = 2 * time.Minute
)

// splitStackError — 拆分栈底准备阶段的失败，带一个稳定的错误码。
type splitStackError struct {
	code string
	msg  string
}

func (e *splitStackError) Error() string { return e.msg }

// prepareSplitStackBase prepares the stack bottom for a split.
//
// It returns:
//
//   - branch  — the stack bottom (the master's dev branch). Empty means
//     "no stack": the caller must let the service layer derive the base
//     from the master row.
//   - pushed  — whether the stack bottom now exists on origin (a stacked PR
//     needs its base there).
//   - warnings — soft problems the user should know about but that don't
//     block the split.
//   - err     — a hard failure; the split must NOT be persisted.
//
// sourceBranch is the branch the master should be created from. Empty means
// "use the master's current base".
func (h *WizardHandler) prepareSplitStackBase(reqRow *model.Requirement, proj *model.Project, sourceBranch string) (branch string, pushed bool, warnings []string, err error) {
	warnings = []string{}
	if reqRow == nil {
		return "", false, warnings, nil
	}

	// Degradation gate. The unit tests build partial handlers with a nil
	// project service, and every git path below needs one (the project path
	// and the platform token). Nothing here is fatal — the split still
	// happens, just without a stack.
	if h.projectSvc == nil || h.platformSvc == nil {
		return "", false, warnings, nil
	}
	if proj == nil || strings.TrimSpace(proj.LocalPath) == "" {
		warnings = append(warnings, "项目路径不可用，子需求沿用主需求的基线。")
		return "", false, warnings, nil
	}
	projectPath := proj.LocalPath

	if _, gerr := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); gerr != nil {
		warnings = append(warnings, "项目不是 Git 仓库，子需求沿用主需求的基线。")
		return "", false, warnings, nil
	}
	if reqRow.NoBranch {
		warnings = append(warnings, "主需求为「不创建分支」模式，子需求直接基于主需求的基线开发。")
		return "", false, warnings, nil
	}

	// No origin at all → there is nothing to push to, but the stack bottom
	// branch is still created locally (the children develop on it). Only the
	// PUSH is skipped, with a warning.
	//
	// This check must come BEFORE the reachability probe below: "origin does
	// not exist" and "origin does not answer" are different situations, and
	// only the latter is a hard failure.
	hasOrigin := false
	if remoteURL, rerr := gitRun(projectPath, "remote", "get-url", "origin"); rerr == nil && strings.TrimSpace(remoteURL) != "" {
		hasOrigin = true
	}

	dev := requirementBranchName(reqRow.Kind, reqRow.ID)

	// Resolve the source branch A: explicit pick → the master's current base
	// → the project default → "main".
	sourceBranch = strings.TrimSpace(sourceBranch)
	if sourceBranch == "" {
		sourceBranch = strings.TrimSpace(reqRow.BaseBranch)
	}
	if sourceBranch == "" {
		sourceBranch = strings.TrimSpace(proj.DefaultBranch)
	}
	if sourceBranch == "" {
		sourceBranch = "main"
	}

	env, cleanup := h.splitGitEnv(reqRow)
	defer cleanup()

	// The whole preparation shares one budget: the user is staring at a
	// 「创建中…」 button, so the request must always come back — with a
	// result or with a clear reason — inside splitStackBudget.
	deadline := time.Now().Add(splitStackBudget)

	// Resolve the ref the stack bottom is created from.
	//
	// The common case is "the local branch already IS the remote branch" —
	// then `git fetch` is pure overhead (on a large repo it can take
	// minutes) and a single `git ls-remote` proves it. Only when the two
	// differ do we pay for a fetch.
	//
	// The same ls-remote doubles as the reachability probe. origin not
	// answering means the stack bottom can never be pushed, and the whole
	// split is doomed — so the user is told NOW (in seconds) instead of
	// after the push has burned its own timeout. The old path made the user
	// stare at 「创建中…」 for minutes before the same conclusion.
	//
	// When the fetch is skipped we must prefer the LOCAL ref: origin/<branch>
	// may be a stale remote-tracking ref that the skipped fetch would not
	// have refreshed.
	startRef, remoteReachable := resolveSplitStartRef(projectPath, sourceBranch, env, hasOrigin)
	if hasOrigin && !remoteReachable {
		return "", false, warnings, &splitStackError{
			code: "SPLIT_REMOTE_UNREACHABLE",
			msg:  "无法连接远端仓库 origin（网络不可达或仓库凭据无效）。子需求尚未创建 —— 请检查网络与凭据后重试。",
		}
	}
	if startRef == "" {
		return "", false, warnings, &splitStackError{
			code: "SPLIT_SOURCE_BRANCH_INVALID",
			msg:  fmt.Sprintf("来源分支 %s 不存在（本地与远端都没有），请重新选择。", sourceBranch),
		}
	}

	// Create or align the stack bottom. NEVER a force-reset: a commit that
	// is already on the master branch (or a child's PR that already landed
	// on it) must never be thrown away.
	if _, verr := gitRun(projectPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+dev); verr != nil {
		// The branch does not exist yet — create it at the source ref. This
		// is the "checkout 出来" the user asked for.
		if _, cerr := gitRunWithTimeout(projectPath, splitStepBudget(deadline, splitBranchGitTimeout), env, "branch", dev, startRef); cerr != nil {
			return "", false, warnings, &splitStackError{
				code: "SPLIT_STACK_BRANCH_FAILED",
				msg:  fmt.Sprintf("创建主需求分支 %s 失败：%v。请检查仓库状态后重试。", dev, cerr),
			}
		}
	} else if wtDir := splitWorktreeForBranch(projectPath, dev); wtDir != "" {
		// The branch is checked out somewhere — move it with a fast-forward
		// so the working tree stays consistent with the branch pointer.
		if _, merr := gitRunWithTimeout(wtDir, splitStepBudget(deadline, splitBranchGitTimeout), env, "merge", "--ff-only", startRef); merr != nil {
			warnings = append(warnings, fmt.Sprintf(
				"主需求分支 %s 已有不在所选分支 %s 上的提交，未按所选分支重建；子需求仍基于 %s。",
				dev, sourceBranch, dev))
		}
	} else {
		// The branch exists but is not checked out anywhere, so its pointer
		// can be moved — but only when that loses no commit.
		unique := "0"
		if out, rerr := gitRunWithTimeout(projectPath, splitStepBudget(deadline, splitBranchGitTimeout), env, "rev-list", "--count", startRef+".."+dev); rerr == nil {
			unique = strings.TrimSpace(out)
		}
		if unique == "0" {
			if _, berr := gitRunWithTimeout(projectPath, splitStepBudget(deadline, splitBranchGitTimeout), env, "branch", "-f", dev, startRef); berr != nil {
				warnings = append(warnings, fmt.Sprintf(
					"主需求分支 %s 未能对齐到所选分支 %s（%v）；子需求仍基于 %s。",
					dev, sourceBranch, berr, dev))
			}
		} else {
			warnings = append(warnings, fmt.Sprintf(
				"主需求分支 %s 已有 %s 个不在所选分支 %s 上的提交，未按所选分支重建；子需求仍基于 %s。",
				dev, unique, sourceBranch, dev))
		}
	}

	// Push the stack bottom to origin. Without it the children's PRs have
	// no base to target.
	pushed, pushWarn, perr := h.pushSplitStackBase(projectPath, dev, env, deadline)
	if perr != nil {
		return "", false, warnings, perr
	}
	warnings = append(warnings, pushWarn...)

	h.persistSplitStackBase(reqRow, proj, dev, sourceBranch)

	return dev, pushed, warnings, nil
}

// splitStepBudget clamps a per-step timeout to whatever is left of the
// overall deadline.
//
// Without the clamp the steps could add up to more than the total budget
// (fetch + branch + push + ls-remote each have their own timeout), and the
// request would exceed the promise made to the user.
func splitStepBudget(deadline time.Time, step time.Duration) time.Duration {
	left := time.Until(deadline)
	if left <= 0 {
		// Already out of budget — give the step a sliver so it fails fast
		// with a real error instead of being skipped silently.
		return time.Second
	}
	if left < step {
		return left
	}
	return step
}

// resolveSplitStartRef resolves the ref the stack bottom is created from,
// and reports whether origin answered at all.
//
// The fast path avoids the expensive `git fetch`: when the local branch
// exists and origin reports the SAME sha, the two are identical, so the
// fetch has nothing to bring in. `git ls-remote` is a single cheap
// round-trip; `git fetch` on a large repository can take minutes — and the
// user is watching a 「创建中…」 button the whole time.
//
// The fast path returns the LOCAL ref on purpose: origin/<branch> may be a
// stale remote-tracking ref, and the skipped fetch is exactly what would
// have refreshed it.
//
// When the two differ (or either side is missing) we fall back to a
// best-effort fetch and then to resolveStartRef's origin-first preference.
// A fetch failure is not fatal — the local branch may be perfectly good.
//
// remoteReachable is false when the ls-remote itself failed, i.e. origin did
// not answer (unreachable, wrong credentials, …). The caller then skips the
// fetch — it would fail the same way — and lets the push report the real
// problem. The point is that a dead remote is discovered in SECONDS, not
// after the push has burned its own timeout.
//
// hasOrigin=false means the repo has no remote at all: there is nothing to
// probe, the fetch is pointless and the remote is trivially "not needed".
func resolveSplitStartRef(projectPath, sourceBranch string, env []string, hasOrigin bool) (string, bool) {
	if !hasOrigin {
		return resolveStartRef(projectPath, sourceBranch), false
	}
	remoteSHA, remoteErr := splitRemoteHeadSHA(projectPath, sourceBranch, env)
	if remoteErr != nil {
		// origin did not answer. The fetch would fail the same way — skip it
		// and resolve from the local refs only.
		return resolveStartRef(projectPath, sourceBranch), false
	}
	if remoteSHA == "" {
		// origin answered but has no such branch. The fetch would fail with
		// "couldn't find remote ref" — skip it and resolve locally. This is
		// NOT an unreachable remote: the user simply picked a branch that
		// does not exist, and the caller must say so.
		return resolveStartRef(projectPath, sourceBranch), true
	}

	if local, err := gitRun(projectPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+sourceBranch); err == nil {
		if strings.TrimSpace(local) == remoteSHA {
			return sourceBranch, true
		}
	}

	if _, ferr := gitRunWithTimeout(projectPath, splitFetchTimeout, env, "fetch", "--quiet", "origin", sourceBranch); ferr != nil {
		log.Printf("[split] fetch origin %s failed (continuing): %v", sourceBranch, ferr)
	}
	return resolveStartRef(projectPath, sourceBranch), true
}

// splitRemoteHeadSHA returns the sha origin has for a branch.
//
// An error means "origin did not answer" — unreachable, missing credentials,
// … An EMPTY sha with no error means "origin answered, but it has no such
// branch". The two are different situations and the caller must not confuse
// them: the first is a network/credentials problem, the second is "you
// picked a branch that does not exist".
func splitRemoteHeadSHA(projectPath, branch string, env []string) (string, error) {
	out, err := gitRunWithTimeout(projectPath, splitLsRemoteTimeout, env, "ls-remote", "--heads", "origin", branch)
	if err != nil {
		return "", err
	}
	// `<sha>\trefs/heads/<branch>` — one line per matching ref.
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

// pushSplitStackBase pushes the stack bottom to origin.
//
// A push failure is NOT automatically fatal: the branch may already exist on
// the remote (a previous run pushed it, the local copy is just behind). In
// that case the base exists where the PR needs it, so the split proceeds.
// Only when the remote genuinely lacks the branch does the split fail.
func (h *WizardHandler) pushSplitStackBase(projectPath, dev string, env []string, deadline time.Time) (bool, []string, error) {
	warnings := []string{}

	remote, rerr := gitRun(projectPath, "remote", "get-url", "origin")
	if rerr != nil || strings.TrimSpace(remote) == "" {
		warnings = append(warnings, fmt.Sprintf(
			"项目未配置 origin 远端，主需求分支 %s 未推送；子需求仍基于它开发。", dev))
		return false, warnings, nil
	}

	if _, perr := gitRunWithTimeout(projectPath, splitStepBudget(deadline, splitPushTimeout), env, "push", "-u", "origin", dev); perr != nil {
		// Re-check the remote: the branch may already be there. This check
		// is cheap (one ls-remote) and the whole point of the fallback —
		// the base may exist remotely even though the push failed.
		if out, lerr := gitRunWithTimeout(projectPath, splitStepBudget(deadline, splitRecheckTimeout), env, "ls-remote", "--heads", "origin", dev); lerr == nil && strings.TrimSpace(out) != "" {
			warnings = append(warnings, fmt.Sprintf(
				"远端已存在主需求分支 %s（本地改动未推送），子需求仍基于它开发。", dev))
			return true, warnings, nil
		}
		return false, warnings, &splitStackError{
			code: "SPLIT_PUSH_FAILED",
			msg: fmt.Sprintf("主需求分支 %s 推送失败：%v。子需求尚未创建 —— 请检查网络与仓库凭据后重试。",
				dev, redactOriginForLog(perr.Error())),
		}
	}
	return true, warnings, nil
}

// persistSplitStackBase writes the resolved stack bottom back onto the
// master row.
//
// branch_name matters: the base resolution chain reaches a child's base
// through the PARENT's branch_name (the tier-3 lookup), and it skips the
// child's own persisted base. A master without a branch_name would send
// every child back to the project default and the stack would collapse.
//
// base_branch is only rewritten when the picked source differs from the
// master's EFFECTIVE base — otherwise a project default would get frozen
// into an explicit choice and would stop following projects.default_branch.
func (h *WizardHandler) persistSplitStackBase(reqRow *model.Requirement, proj *model.Project, dev, sourceBranch string) {
	if h.reqSvc == nil {
		return
	}
	if cur := strings.TrimSpace(reqRow.BranchName); cur == "" || cur == dev {
		if err := h.reqSvc.UpdateWorktree(reqRow.ID, dev, reqRow.WorktreePath); err != nil {
			log.Printf("[split] %s: persist branch_name %s failed: %v", reqRow.ID, dev, err)
		}
	} else {
		log.Printf("[split] %s: keeping branch_name %q (not %q)", reqRow.ID, reqRow.BranchName, dev)
	}

	currentBase := strings.TrimSpace(reqRow.BaseBranch)
	if currentBase == "" && proj != nil {
		currentBase = strings.TrimSpace(proj.DefaultBranch)
	}
	if currentBase == "" {
		currentBase = "main"
	}
	if sourceBranch == currentBase {
		return
	}
	if err := h.reqSvc.UpdateBaseBranchWithSource(reqRow.ID, sourceBranch, model.BaseSourceExplicit); err != nil {
		log.Printf("[split] %s: persist base_branch %s failed: %v", reqRow.ID, sourceBranch, err)
	}
}

// splitWorktreeForBranch returns the worktree directory in which branch is
// checked out, or "" when the branch is not checked out anywhere.
//
// It parses `git worktree list --porcelain`, whose entries look like
//
//	worktree /path/to/dir
//	HEAD <sha>
//	branch refs/heads/<name>
//
// A detached worktree has no "branch" line and is skipped.
func splitWorktreeForBranch(repoPath, branch string) string {
	out, err := gitRun(repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	want := "refs/heads/" + branch
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "worktree ") {
			cur = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			continue
		}
		if line == "branch "+want {
			if cur == "" {
				return ""
			}
			if st, serr := os.Stat(cur); serr != nil || !st.IsDir() {
				return ""
			}
			return cur
		}
	}
	return ""
}

// splitGitEnv builds the git environment for the split's git calls: the
// committer identity and the HTTPS askpass helper for the project's platform
// token. The returned cleanup removes the temp askpass script and MUST be
// called once the git calls are done.
//
// Deliberately NOT assembleGitCredEnv: that one also provisions GPG signing
// config, which is a write side effect on the user's repository and has no
// business happening during a split.
func (h *WizardHandler) splitGitEnv(reqRow *model.Requirement) (env []string, cleanup func()) {
	cleanup = func() {}
	if name, email := lookupGitIdentity(h.projectSvc, h.platformSvc, reqRow); name != "" || email != "" {
		env = append(env, buildIdentityEnv(name, email)...)
	}
	credEnv, credCleanup := gitCredentialEnv(h.projectSvc, h.platformSvc, reqRow)
	if len(credEnv) > 0 {
		env = append(env, credEnv...)
		cleanup = credCleanup
	}
	return env, cleanup
}
