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
const (
	// splitFetchTimeout — best-effort 拉取来源分支。
	splitFetchTimeout = 30 * time.Second
	// splitBranchGitTimeout — 建分支 / 对齐分支 / 推送。
	splitBranchGitTimeout = 60 * time.Second
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

	// Best-effort fetch so a fresh origin/<A> wins over a stale local <A>.
	// A failure here is not fatal — the local branch may be perfectly good.
	if _, ferr := gitRunWithTimeout(projectPath, splitFetchTimeout, env, "fetch", "--quiet", "origin", sourceBranch); ferr != nil {
		log.Printf("[split] %s: fetch origin %s failed (continuing): %v", reqRow.ID, sourceBranch, ferr)
	}

	startRef := resolveStartRef(projectPath, sourceBranch)
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
		if _, cerr := gitRunWithTimeout(projectPath, splitBranchGitTimeout, env, "branch", dev, startRef); cerr != nil {
			return "", false, warnings, &splitStackError{
				code: "SPLIT_STACK_BRANCH_FAILED",
				msg:  fmt.Sprintf("创建主需求分支 %s 失败：%v。请检查仓库状态后重试。", dev, cerr),
			}
		}
	} else if wtDir := splitWorktreeForBranch(projectPath, dev); wtDir != "" {
		// The branch is checked out somewhere — move it with a fast-forward
		// so the working tree stays consistent with the branch pointer.
		if _, merr := gitRunWithTimeout(wtDir, splitBranchGitTimeout, env, "merge", "--ff-only", startRef); merr != nil {
			warnings = append(warnings, fmt.Sprintf(
				"主需求分支 %s 已有不在所选分支 %s 上的提交，未按所选分支重建；子需求仍基于 %s。",
				dev, sourceBranch, dev))
		}
	} else {
		// The branch exists but is not checked out anywhere, so its pointer
		// can be moved — but only when that loses no commit.
		unique := "0"
		if out, rerr := gitRunWithTimeout(projectPath, splitBranchGitTimeout, env, "rev-list", "--count", startRef+".."+dev); rerr == nil {
			unique = strings.TrimSpace(out)
		}
		if unique == "0" {
			if _, berr := gitRunWithTimeout(projectPath, splitBranchGitTimeout, env, "branch", "-f", dev, startRef); berr != nil {
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
	pushed, pushWarn, perr := h.pushSplitStackBase(projectPath, dev, env)
	if perr != nil {
		return "", false, warnings, perr
	}
	warnings = append(warnings, pushWarn...)

	h.persistSplitStackBase(reqRow, proj, dev, sourceBranch)

	return dev, pushed, warnings, nil
}

// pushSplitStackBase pushes the stack bottom to origin.
//
// A push failure is NOT automatically fatal: the branch may already exist on
// the remote (a previous run pushed it, the local copy is just behind). In
// that case the base exists where the PR needs it, so the split proceeds.
// Only when the remote genuinely lacks the branch does the split fail.
func (h *WizardHandler) pushSplitStackBase(projectPath, dev string, env []string) (bool, []string, error) {
	warnings := []string{}

	remote, rerr := gitRun(projectPath, "remote", "get-url", "origin")
	if rerr != nil || strings.TrimSpace(remote) == "" {
		warnings = append(warnings, fmt.Sprintf(
			"项目未配置 origin 远端，主需求分支 %s 未推送；子需求仍基于它开发。", dev))
		return false, warnings, nil
	}

	if _, perr := gitRunWithTimeout(projectPath, splitBranchGitTimeout, env, "push", "-u", "origin", dev); perr != nil {
		// Re-check the remote: the branch may already be there.
		if out, lerr := gitRunWithTimeout(projectPath, splitBranchGitTimeout, env, "ls-remote", "--heads", "origin", dev); lerr == nil && strings.TrimSpace(out) != "" {
			warnings = append(warnings, fmt.Sprintf(
				"远端已存在主需求分支 %s（本地改动未推送），子需求仍基于它开发。", dev))
			return true, warnings, nil
		}
		return false, warnings, &splitStackError{
			code: "SPLIT_PUSH_FAILED",
			msg: fmt.Sprintf("主需求分支 %s 推送失败：%v。子需求尚未创建 —— 请检查网络与仓库凭据后重试。",
				dev, perr),
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
