package handler

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// 跨项目需求的「工作目录集合」。
//
// 跨项目需求横跨 N 个仓库，所以它的工作目录不是一个目录，而是 N 个仓库各一份
// 隔离 worktree —— 路径由**跨项目需求 ID** 决定：
//
//	~/.novaworkbench/worktrees/<仓库目录名>/<xreqID>
//
// 这套形状同时满足三件事：
//
//  1. **读到最新基线**：建之前先 fetch origin/<该仓库的基础分支>，并把提交锁
//     下来。整体方案是对着**上游最新提交**写的，不是对着某个可能落后几十个
//     提交、还停在别人 WIP 分支上的工作副本写的。
//  2. **不动用户的检出**：worktree 是 detached HEAD，从 origin/<base> 建，
//     不建任何分支、不快进共享检出、也不受共享检出脏工作区的影响。跨项目
//     作业**不改代码**（Write / Edit / NotebookEdit 禁用），Bash 只用来读，
//     破坏性命令由守卫挡住 —— 绝不能污染任何仓库。
//  3. **对称**：N 个仓库各一份，没有「主仓库」。这正是原来用临时目录的理由
//     （见 crossWorkDir 的历史注释）—— 现在把「对称」和「读到最新基线」兼得，
//     而不是二选一。
//
// 子需求（真正的开发单元）**不**用这套目录：它们各自在自己的仓库里建
// feat/req_xxx 分支与 worktree（一条分支一个 PR）。跨项目需求 ID 在这里
// 的作用是确定**共同基线** —— 方案、拆分都对着这一份树写。

// crossWorkspace —— 一次跨项目作业读到的全部工作目录。
type crossWorkspace struct {
	// Projects —— 参与项目，顺序与「参与项目」映射一致。
	Projects []crossWorkspaceProject
}

// crossWorkspaceProject —— 一个参与仓库的工作目录。
type crossWorkspaceProject struct {
	ProjectID string
	Name      string
	// BaseBranch —— 本次锁定的基础分支（空 = 项目默认分支）。
	BaseBranch string
	// LocalPath —— 共享检出。降级时的读取路径，也是 worktree 的宿主仓库。
	LocalPath string
	// WorktreePath —— 隔离 worktree（detached HEAD，停在 BaseSHA 上）。
	// 降级时为空。
	WorktreePath string
	// BaseSHA —— 锁定的提交（40 hex）。降级时为空。
	BaseSHA string
	// Degraded —— true = 没能锁定基线（不是 git 仓库 / 没有 origin / 仓库还没
	// 有提交），读取路径退回到共享检出。
	Degraded bool
	// DegradedReason —— 降级原因（中文，直接进作业面板）。
	DegradedReason string
}

// ReadPath is the directory the job should read this repository from.
func (p crossWorkspaceProject) ReadPath() string {
	if p.WorktreePath != "" {
		return p.WorktreePath
	}
	return p.LocalPath
}

// Baseline renders the short SHA shown in the job panel and the prompt.
func (p crossWorkspaceProject) Baseline() string {
	if p.BaseSHA == "" {
		return ""
	}
	if len(p.BaseSHA) > 7 {
		return p.BaseSHA[:7]
	}
	return p.BaseSHA
}

// crossWorkspaceDir returns the working directory of a cross-requirement job.
//
// 目录由**跨项目需求 ID** 决定（作业种类只是它下面的一个子目录）：
//
//	~/.novaworkbench/xreq/<xreqID>/<kind>
//
// 与原来的临时目录相比有两点不同：
//
//   - 它是**稳定的**：临时目录每次跑完就被删掉，重连 / 续会话时 cwd 已经不在
//     磁盘上了；这里不会。
//   - 它**看得见归属**：目录名就是跨项目需求的 ID，排查问题时一眼知道是哪个
//     需求在用它。
//
// 三个作业各用一个子目录（design / split / verify），保持「作业之间互不
// 干扰」的结构。
func crossWorkspaceDir(xreqID, kind string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("定位用户目录失败: %w", err)
	}
	xreqID = strings.TrimSpace(xreqID)
	if xreqID == "" {
		return "", fmt.Errorf("跨项目需求 id 为空")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "work"
	}
	dir := filepath.Join(home, ".novaworkbench", "xreq", xreqID, kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建工作目录失败: %w", err)
	}
	return dir, nil
}

// removeCrossWorkspaceDir removes a cross-requirement's working directories.
//
// Best-effort: a leftover directory is harmless (it holds no state the app
// depends on), so a failure here is logged, never fatal.
func removeCrossWorkspaceDir(xreqID string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	root := filepath.Join(home, ".novaworkbench", "xreq", strings.TrimSpace(xreqID))
	if root == "" || strings.TrimSpace(xreqID) == "" {
		return
	}
	if err := os.RemoveAll(root); err != nil {
		log.Printf("[xreq] 清理工作目录 %s 失败: %v", root, err)
	}
}

// buildCrossWorkspace prepares the working directories of a cross-requirement
// job: it fetches each participating repository's base branch, locks the
// resulting commit, and materializes a detached worktree pinned to it.
//
// **A failed fetch is a hard error** — the job must not go on to write a
// design against a tree we know is stale. The distinction between "the
// upstream has no such branch" and "we could not reach the upstream" is made
// by service.ProjectService.FetchBaseBranch, which names the first case
// precisely and reports the second with git's own stderr.
//
// A repository that cannot be synced at all (not a git repo / no origin /
// unborn HEAD) degrades: the job reads the shared checkout and says so in the
// panel. That is a different failure from "the fetch failed" — there is no
// upstream to sync against, so there is nothing to be stale about.
//
// logf may be nil (silent).
func (h *CrossRequirementHandler) buildCrossWorkspace(ctx context.Context, xreqID string, logf func(string)) (*crossWorkspace, error) {
	ws := &crossWorkspace{Projects: []crossWorkspaceProject{}}
	projects, err := h.svc.ListProjects(xreqID)
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 {
		return ws, nil
	}
	timeout := crossFetchTimeout(h.settingSvc)

	for _, p := range projects {
		item := crossWorkspaceProject{
			ProjectID:  p.ProjectID,
			Name:       p.ProjectName,
			BaseBranch: strings.TrimSpace(p.BaseBranch),
		}
		if item.Name == "" {
			item.Name = p.ProjectID
		}
		if h.projectSvc != nil {
			if got, pErr := h.projectSvc.Get(p.ProjectID); pErr == nil && got != nil {
				item.LocalPath = strings.TrimSpace(got.LocalPath)
				item.Name = got.Name
				if item.BaseBranch == "" {
					item.BaseBranch = strings.TrimSpace(got.DefaultBranch)
				}
			}
		}
		ws.Projects = append(ws.Projects, item)
	}

	// The base branch goes into a git command, so re-validate it on the read
	// path: legal at write time does not mean legal in a row written by an
	// older build.
	for i := range ws.Projects {
		it := &ws.Projects[i]
		base, bErr := service.NormalizeBaseBranch(it.BaseBranch)
		if bErr != nil {
			return nil, fmt.Errorf("项目「%s」的基础分支不合法: %w", it.Name, bErr)
		}
		if base == "" {
			base = "main"
		}
		it.BaseBranch = base
	}

	// Sync + materialize, one repository at a time. A failure aborts the whole
	// workspace: producing a design that silently covers only some of the
	// repositories is worse than producing none.
	for i := range ws.Projects {
		it := &ws.Projects[i]
		if it.LocalPath == "" {
			it.Degraded = true
			it.DegradedReason = "项目没有配置本地检出目录"
			if logf != nil {
				logf("⚠️ " + it.Name + "：" + it.DegradedReason + "，方案将不包含该仓库的代码")
			}
			continue
		}
		if h.worktreeH == nil {
			it.Degraded = true
			it.DegradedReason = "工作区处理器不可用"
			if logf != nil {
				logf("⚠️ " + it.Name + "：" + it.DegradedReason + "，方案将直接读取该仓库的当前工作副本")
			}
			continue
		}
		if logf != nil {
			logf("🔄 同步 origin/" + it.BaseBranch + "（" + it.Name + "）…")
		}
		res, fErr := h.projectSvc.FetchBaseBranch(ctx, it.ProjectID, it.BaseBranch, timeout)
		if fErr != nil {
			return nil, fmt.Errorf("同步项目「%s」的基础分支失败: %w", it.Name, fErr)
		}
		if res.Skipped {
			it.Degraded = true
			it.DegradedReason = res.Reason
			if logf != nil {
				logf("⚠️ " + it.Name + "：" + res.Reason + "，方案将直接读取该仓库的当前工作副本")
			}
			continue
		}
		// The detached worktree is pinned to the SHA we just locked, so the
		// design reads exactly that tree. skipSync=true: the hard fetch above
		// already moved origin/<base>.
		wtPath, wtSHA, wErr := h.ensureCrossWorktree(ctx, it.ProjectID, it.LocalPath, xreqID, it.BaseBranch, res.SHA, logf)
		if wErr != nil {
			return nil, fmt.Errorf("为项目「%s」准备隔离工作区失败: %w", it.Name, wErr)
		}
		it.WorktreePath = wtPath
		// The locked SHA is the one the model actually reads — the worktree
		// may have refused to move onto the fetched tip (it carries local
		// commits), and lying about it would make the ledger a fiction.
		it.BaseSHA = wtSHA
		if logf != nil {
			logf("📌 基线已锁定 " + it.Baseline() + "  🌿 隔离工作区 " + wtPath)
		}
	}
	return ws, nil
}

// ensureCrossWorktree materializes the cross requirement's detached worktree
// for one repository and pins it to baseSHA.
//
// It goes through the existing worktree machinery (WorktreeHandler) rather
// than hand-rolling git commands, but it deliberately does NOT reuse
// ensureDetachedWorktreeCore: that one fast-forwards an existing worktree
// onto the *current* origin/<base>, which would silently move the baseline
// the overall design was written against. Here the SHA is the contract.
//
// The worktree is detached: no branch is created in the user's repository,
// locally or remotely. The cross jobs never author changes, so a branch would
// have no purpose.
//
// It returns the worktree path AND the commit the worktree actually sits on.
// The two are usually the same, but not always: an existing worktree that
// carries local commits cannot fast-forward onto the fetched tip, and the
// repository's own principle ("local commits are NEVER discarded") wins over
// pinning. The caller records the ACTUAL commit — a ledger that claims a
// baseline the model never read is worse than no ledger.
func (h *CrossRequirementHandler) ensureCrossWorktree(ctx context.Context, projectID, projectPath, xreqID, baseBranch, baseSHA string, logf func(string)) (string, string, error) {
	if h.worktreeH == nil {
		return "", "", fmt.Errorf("worktree handler 不可用")
	}
	wtPath, err := h.worktreeH.ensureDetachedWorktreeFrom(ctx, projectID, projectPath, xreqID, baseBranch, true, logf)
	if err != nil {
		return "", "", err
	}
	head, hErr := gitRun(wtPath, "rev-parse", "HEAD")
	actual := strings.TrimSpace(head)
	if hErr != nil || actual == "" {
		return wtPath, "", nil
	}
	if baseSHA != "" && actual != baseSHA {
		// The worktree did not move onto the fetched tip. Re-pin only when it
		// is clean; a dirty worktree is left alone (its content is somebody's
		// work, not ours to throw away).
		porcelain, pErr := gitRun(wtPath, "status", "--porcelain")
		if pErr == nil && strings.TrimSpace(porcelain) == "" {
			if out, cErr := gitRun(wtPath, "checkout", "--detach", baseSHA); cErr != nil {
				return "", "", fmt.Errorf("worktree %s 对齐到基线失败: %s", wtPath, strings.TrimSpace(out))
			}
			if pinned, pErr := gitRun(wtPath, "rev-parse", "HEAD"); pErr == nil {
				actual = strings.TrimSpace(pinned)
			}
		} else if logf != nil {
			logf("ℹ️ 隔离工作区有本地改动，未对齐到最新基线，继续使用当前提交 " + shortSHA(actual))
		}
	}
	return wtPath, actual, nil
}

// crossWorkspaceDirs returns the directories handed to the CLI via --add-dir.
//
// Directories that do not exist on disk are skipped — the CLI rejects an
// unreadable --add-dir, and a project whose checkout vanished should not
// take the whole job down with it.
func crossWorkspaceDirs(ws *crossWorkspace) []string {
	if ws == nil {
		return nil
	}
	dirs := []string{}
	seen := map[string]struct{}{}
	for _, p := range ws.Projects {
		path := p.ReadPath()
		if path == "" {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		if st, sErr := os.Stat(path); sErr != nil || !st.IsDir() {
			continue
		}
		seen[path] = struct{}{}
		dirs = append(dirs, path)
	}
	return dirs
}

// crossWorkspaceBases renders the workspace as the baseline ledger rows the
// service stores.
func crossWorkspaceBases(ws *crossWorkspace) []model.CrossRequirementBase {
	if ws == nil {
		return nil
	}
	out := make([]model.CrossRequirementBase, 0, len(ws.Projects))
	for _, p := range ws.Projects {
		out = append(out, model.CrossRequirementBase{
			ProjectID:    p.ProjectID,
			BaseBranch:   p.BaseBranch,
			BaseSHA:      p.BaseSHA,
			WorktreePath: p.WorktreePath,
			Degraded:     p.Degraded,
		})
	}
	return out
}

// crossFetchTimeout resolves the per-repository fetch budget.
//
// It reuses the same KV-backed setting as the design stage
// (setting.git_sync_timeout_seconds, default 60s) so one knob governs every
// "fetch the baseline" path. A missing settings service falls back to the
// same default.
func crossFetchTimeout(settingSvc *service.SettingService) time.Duration {
	if settingSvc == nil {
		return time.Duration(service.DefaultGitSyncTimeoutSeconds) * time.Second
	}
	dur, err := settingSvc.GitSyncTimeout()
	if err != nil || dur <= 0 {
		return time.Duration(service.DefaultGitSyncTimeoutSeconds) * time.Second
	}
	return dur
}

// cleanupCrossWorktrees removes the cross requirement's worktrees from every
// participating repository.
//
// Called when the cross requirement is deleted. The baseline ledger is passed
// in because the rows are already gone by then — it is the only record of
// which worktrees this cross requirement created.
//
// Best-effort per repository: a worktree that cannot be removed is logged and
// left behind — the alternative (failing the delete) would strand the whole
// entity because one repository is unreachable.
func (h *CrossRequirementHandler) cleanupCrossWorktrees(xreqID string, bases []model.CrossRequirementBase) {
	if h.projectSvc != nil {
		for _, b := range bases {
			if b.WorktreePath == "" {
				continue
			}
			proj, pErr := h.projectSvc.Get(b.ProjectID)
			if pErr != nil || proj == nil || proj.LocalPath == "" {
				continue
			}
			if err := RemoveWorktree(proj.LocalPath, b.WorktreePath, true); err != nil {
				log.Printf("[xreq] 清理 worktree %s 失败: %v", b.WorktreePath, err)
			}
		}
	}
	removeCrossWorkspaceDir(xreqID)
}
