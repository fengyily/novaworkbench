package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/platform"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// MergeHandler implements the post-coding "合入" (merge / PR) step. After
// start-coding leaves the working tree on the dev branch (feat/<req_id>) with
// possibly uncommitted changes, the user chooses one of two paths:
//  1. local merge — commit + merge dev into a target branch (default main),
//     with AI-assisted conflict resolution when the merge hits conflicts;
//  2. push + PR — commit + push the dev branch to origin and surface a quick
//     "create PR" link built from the repo's remote URL.
//
// All long-running work (merge, push, AI conflict resolution, conclude) runs as
// a JobStore job streamed via the existing /api/wizard/jobs/{id}[/stream]
// endpoints — same pattern as start-coding. The repo state (checked-out
// branch, mid-merge MERGE_HEAD) lives on disk, so a backend restart between
// steps is recoverable: /merge/state reads the real git state.
type MergeHandler struct {
	projectSvc  *service.ProjectService
	reqSvc      *service.RequirementService
	llm         *llm.Gateway
	jobs        *store.JobStore
	roleSvc     *service.RoleService
	platformSvc *service.PlatformTokenService
	usageSvc    usageRecorder
}

func NewMergeHandler(projectSvc *service.ProjectService, reqSvc *service.RequirementService, llmGateway *llm.Gateway, jobs *store.JobStore, roleSvc *service.RoleService, platformSvc *service.PlatformTokenService, usageSvc usageRecorder) *MergeHandler {
	return &MergeHandler{
		projectSvc:  projectSvc,
		reqSvc:      reqSvc,
		llm:         llmGateway,
		jobs:        jobs,
		roleSvc:     roleSvc,
		platformSvc: platformSvc,
		usageSvc:    usageSvc,
	}
}

// roleConfig loads the developer role's system prompt + model. On miss it
// returns empty strings so a broken role config never blocks merge resolution.
func (h *MergeHandler) roleConfig() (systemPrompt, model string) {
	r, err := h.roleSvc.GetByKey("developer")
	if err != nil {
		log.Printf("[merge] developer role not found, using CLI defaults: %v", err)
		return "", ""
	}
	return r.SystemPrompt, r.Model
}

// loadReqProject resolves the requirement + its project (LocalPath /
// DefaultBranch / PlatformType). Shared by every endpoint. Returns the
// requirement row too — its BranchName/WorktreePath drive worktree isolation.
func (h *MergeHandler) loadReqProject(w http.ResponseWriter, r *http.Request) (reqRow *model.Requirement, projectPath, defaultBranch, platformType string, ok bool) {
	reqID := r.PathValue("id")
	reqRow, err := h.reqSvc.Get(reqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "REQ_NOT_FOUND", "需求不存在")
		return nil, "", "", "", false
	}
	project, err := h.projectSvc.Get(reqRow.ProjectID)
	if err != nil || project.LocalPath == "" {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目路径不存在")
		return nil, "", "", "", false
	}
	db := project.DefaultBranch
	if db == "" {
		db = "main"
	}
	return reqRow, project.LocalPath, db, project.PlatformType, true
}

// ── git helpers (all run with git -C <dir>) ────────────────────────────────

// gitRun runs a git command in dir and returns trimmed stdout. On failure the
// error carries trimmed stderr so the caller can surface it.
func gitRun(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func currentBranch(dir string) string {
	out, err := gitRun(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// devBranchAndDir resolves the dev branch name and the directory where it is
// checked out for a requirement. For an isolated-worktree requirement whose
// worktree still exists this is the stored branch name + worktree path; in any
// other case (legacy requirement, or a worktree that was removed out-of-band)
// it falls back to the main checkout's current branch + the project directory.
func devBranchAndDir(reqRow *model.Requirement, projectPath string) (dev, dir string) {
	dir = projectPath
	dev = currentBranch(projectPath)
	if reqRow.WorktreePath != "" {
		if _, err := os.Stat(reqRow.WorktreePath); err == nil {
			dir = reqRow.WorktreePath
			dev = reqRow.BranchName
		}
	}
	return dev, dir
}

// uncommittedFiles returns the porcelain status entries (one per changed file,
// path only). Empty when the tree is clean.
func uncommittedFiles(dir string) []string {
	out, err := gitRun(dir, "status", "--porcelain")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 3 {
			continue
		}
		files = append(files, strings.TrimSpace(line[3:]))
	}
	return files
}

func conflictedFiles(dir string) []string {
	out, err := gitRun(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files
}

// midMerge reports whether a merge is in progress (MERGE_HEAD exists).
func midMerge(dir string) bool {
	_, err := gitRun(dir, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	return err == nil
}

// aheadBehind returns (ahead, behind) of dev relative to target: how many
// commits dev has that target doesn't, and vice versa.
func aheadBehind(dir, target, dev string) (ahead, behind int) {
	out, err := gitRun(dir, "rev-list", "--left-right", "--count", target+"..."+dev)
	if err != nil {
		return 0, 0
	}
	parts := strings.Fields(out)
	if len(parts) >= 1 {
		behind, _ = strconv.Atoi(parts[0]) // left = target (commits in target not in dev)
	}
	if len(parts) >= 2 {
		ahead, _ = strconv.Atoi(parts[1]) // right = dev (commits in dev not in target)
	}
	return ahead, behind
}

func remoteURL(dir string) string {
	out, err := gitRun(dir, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return out
}

// commitAll stages everything and commits with msg. Returns committed=true if a
// commit was created; false (clean tree, nothing to commit) is NOT an error.
func commitAll(dir, msg string) (committed bool, err error) {
	if _, err := gitRun(dir, "add", "-A"); err != nil {
		return false, err
	}
	// Detect a clean tree via porcelain status before committing. This is
	// locale-independent — matching git's "nothing to commit" message fails on a
	// localized git (e.g. "无文件要提交，工作区干净"), which surfaces as an
	// empty-stderr "exit status 1".
	out, err := gitRun(dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(out) == "" {
		return false, nil
	}
	if _, err := gitRun(dir, "commit", "-m", msg); err != nil {
		return false, err
	}
	return true, nil
}

// parseRemote splits a remote URL into platform, webBase, owner, repo.
// Handles git@host:o/r[.git], ssh://git@host:port/o/r[.git], https://host/o/r[.git].
func parseRemote(rawURL, fallbackPlatform string) (platform, webBase, owner, repo string) {
	host, op, rp := "", "", ""
	if strings.HasPrefix(rawURL, "git@") {
		// git@github.com:owner/repo.git
		rest := strings.TrimPrefix(rawURL, "git@")
		if at := strings.Index(rest, "@"); at == -1 {
			if idx := strings.Index(rest, ":"); idx >= 0 {
				host = rest[:idx]
				op, rp = splitOwnerRepo(rest[idx+1:])
			}
		}
	} else if strings.HasPrefix(rawURL, "ssh://") {
		// ssh://git@host:port/o/r.git  or  ssh://git@host/o/r.git
		u, err := url.Parse(rawURL)
		if err == nil {
			host = u.Hostname()
			op, rp = splitOwnerRepo(strings.TrimPrefix(u.Path, "/"))
		}
	} else {
		// https://host/o/r.git  (also http://)
		u, err := url.Parse(rawURL)
		if err == nil {
			host = u.Hostname()
			op, rp = splitOwnerRepo(strings.TrimPrefix(u.Path, "/"))
		}
	}
	owner, repo = op, rp

	// platform: explicit fallback first, else infer from host.
	platform = fallbackPlatform
	if platform == "" {
		switch {
		case host == "github.com":
			platform = "github"
		case strings.Contains(host, "gitlab"):
			platform = "gitlab"
		default:
			platform = "gitea" // reasonable default for self-hosted
		}
	}
	if host == "github.com" {
		webBase = "https://github.com"
	} else if host != "" {
		webBase = "https://" + host
	} else if platform == "github" {
		webBase = "https://github.com"
	}
	return platform, webBase, owner, repo
}

// splitOwnerRepo takes "owner/repo.git" or "owner/repo" and returns the parts.
func splitOwnerRepo(s string) (owner, repo string) {
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// buildPRURL constructs a "create PR / merge request" compare URL for the given
// platform. base = target branch, head = dev branch.
func buildPRURL(platform, webBase, owner, repo, base, head string) string {
	if webBase == "" || owner == "" || repo == "" {
		return ""
	}
	switch platform {
	case "gitlab":
		return fmt.Sprintf("%s/%s/%s/-/merge_requests/new?merge_request[source_branch]=%s&merge_request[target_branch]=%s",
			webBase, owner, repo, url.QueryEscape(head), url.QueryEscape(base))
	case "gitea":
		return fmt.Sprintf("%s/%s/%s/compare/%s...%s", webBase, owner, repo, url.PathEscape(base), url.PathEscape(head))
	default: // github
		return fmt.Sprintf("%s/%s/%s/compare/%s...%s?expand=1", webBase, owner, repo, url.PathEscape(base), url.PathEscape(head))
	}
}

// ── endpoints ───────────────────────────────────────────────────────────────

// State returns the current merge-able state of the requirement's repo:
// the dev branch (current HEAD), target branch, uncommitted changes, and a
// preview PR URL.
// GET /api/requirements/{id}/merge/state
func (h *MergeHandler) State(w http.ResponseWriter, r *http.Request) {
	reqRow, dir, defaultBranch, platformType, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}

	// Not a git repo → tell the frontend merge is unavailable.
	if _, err := gitRun(dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"is_git": false, "requirement_id": reqRow.ID,
		})
		return
	}

	// dev branch + the directory where it's checked out (the worktree when the
	// requirement has one; the main checkout otherwise).
	dev, devDir := devBranchAndDir(reqRow, dir)
	target := defaultBranch
	hasRemote := false
	remote := remoteURL(dir)
	if remote != "" {
		hasRemote = true
	}
	pf, webBase, owner, repo := parseRemote(remote, platformType)
	uncommitted := uncommittedFiles(devDir)
	ahead, behind := 0, 0
	if dev != "" && target != "" && dev != "HEAD" {
		// ahead/behind is computed from the shared .git, so the main checkout
		// (dir) works regardless of which worktree has dev checked out.
		ahead, behind = aheadBehind(dir, target, dev)
	}
	prURL := buildPRURL(pf, webBase, owner, repo, target, dev)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"is_git":           true,
		"requirement_id":   reqRow.ID,
		"dev_branch":       dev,
		"target_branch":    target,
		"uncommitted_count": len(uncommitted),
		"uncommitted_files": uncommitted,
		"ahead":            ahead,
		"behind":           behind,
		"has_remote":       hasRemote,
		"remote_url":       remote,
		"platform":         pf,
		"pr_url":           prURL,
		"mid_merge":        midMerge(dir),
		"conflict_files":   conflictedFiles(dir),
		"worktree_path":    reqRow.WorktreePath,
	})
}

// LocalMerge commits uncommitted dev-branch changes, checks out the target
// branch, and merges dev into it. On conflict the job finishes as error with a
// "conflict" log line (file list) and the repo is left mid-merge for the
// abort/resolve/continue endpoints to pick up.
// POST /api/requirements/{id}/merge/local
func (h *MergeHandler) LocalMerge(w http.ResponseWriter, r *http.Request) {
	reqRow, dir, defaultBranch, _, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}
	var body struct {
		TargetBranch string `json:"target_branch"`
		CommitMessage string `json:"commit_message"`
		DeleteBranch  bool   `json:"delete_branch"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	dev, devDir := devBranchAndDir(reqRow, dir)
	if dev == "" || dev == "HEAD" {
		writeError(w, http.StatusBadRequest, "NO_BRANCH", "当前处于 detached HEAD，无法合并")
		return
	}
	target := body.TargetBranch
	if target == "" {
		target = defaultBranch
	}
	commitMsg := body.CommitMessage
	// Worktree isolation: when the dev branch lives in its own worktree, the
	// merge into target must happen in the main checkout (a branch can only be
	// checked out in one worktree). We commit inside the worktree, then check
	// out target in the main dir to merge.
	usingWorktree := reqRow.WorktreePath != "" && devDir != dir

	job := h.jobs.Create(reqRow.ID)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go func() {
		log.Printf("[merge/local] job %s req %s: %s → %s", job.ID, reqRow.ID, dev, target)

		// 1. Commit pending dev-branch changes first (in the worktree / checkout
		//    where dev is actually checked out).
		if commitMsg == "" {
			commitMsg = dev
		}
		if committed, err := commitAll(devDir, commitMsg); err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 提交失败: " + err.Error()})
			job.Finish(1, store.JobError)
			return
		} else if committed {
			job.Append(store.LogLine{Type: "message", Content: "💾 已提交未提交改动: " + commitMsg})
		} else {
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ 工作区干净，无需提交"})
		}

		// 2. For a worktree-isolated dev branch, the main checkout must host
		//    the merge of dev → target. Refuse if the main tree is dirty — we
		//    can't safely switch it to target. Guide the user to push+PR.
		if usingWorktree {
			if dirty := uncommittedFiles(dir); len(dirty) > 0 {
				filesJSON, _ := json.Marshal(dirty)
				job.Append(store.LogLine{Type: "error", Content: "❌ 主工作区有未提交改动，无法切换到目标分支进行合入: " + string(filesJSON)})
				job.Append(store.LogLine{Type: "error", Content: "请先提交/暂存主工作区改动，或改用「推送并发起 PR」路径。"})
				job.Finish(1, store.JobError)
				return
			}
		}

		// 3. Checkout target (in the main checkout when using a worktree, else
		//    in the same dir dev was committed).
		mergeDir := dir
		if !usingWorktree {
			mergeDir = devDir
		}
		if out, err := gitRun(mergeDir, "checkout", target); err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 切换目标分支失败: " + out + " " + err.Error()})
			job.Finish(1, store.JobError)
			return
		} else {
			job.Append(store.LogLine{Type: "message", Content: "🌿 切换到 " + target})
		}

		// 4. Merge dev. Use --no-edit so a merge commit (if any) doesn't open an editor.
		mergeOut, mergeErr := gitRun(mergeDir, "merge", "--no-edit", dev)
		for _, line := range strings.Split(mergeOut, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				job.Append(store.LogLine{Type: "message", Content: line})
			}
		}
		if mergeErr == nil {
			// Optional: delete the dev branch now that it's merged.
			if body.DeleteBranch {
				// Worktree holds the dev branch's checkout — remove it first
				// (best-effort; a dirty worktree is left for the cleanup entry).
				if usingWorktree {
					if rerr := RemoveWorktree(dir, reqRow.WorktreePath, false); rerr != nil {
						job.Append(store.LogLine{Type: "message", Content: "ℹ️ 移除 worktree 失败（可能含未跟踪文件），请用「清理开发环境」处理: " + rerr.Error()})
					} else {
						job.Append(store.LogLine{Type: "message", Content: "🗑️ 已移除 worktree"})
					}
				}
				if out, err := gitRun(dir, "branch", "-d", dev); err != nil {
					job.Append(store.LogLine{Type: "message", Content: "ℹ️ 删除分支失败（可忽略）: " + out})
				} else {
					job.Append(store.LogLine{Type: "message", Content: "🗑️ 已删除分支: " + dev})
				}
				if perr := h.reqSvc.UpdateWorktree(reqRow.ID, "", ""); perr != nil {
					log.Printf("[merge/local] clear worktree fields for %s: %v", reqRow.ID, perr)
				}
			}
			job.Append(store.LogLine{Type: "done", Content: "✅ 已合并 " + dev + " → " + target})
			job.Finish(0, store.JobDone)
			return
		}

		// Merge failed — distinguish conflict from other errors. A real conflict
		// leaves conflicted files; surface them so the UI offers resolution.
		conflicts := conflictedFiles(dir)
		if len(conflicts) > 0 {
			filesJSON, _ := json.Marshal(conflicts)
			job.Append(store.LogLine{Type: "conflict", Content: "⚠️ 合并存在冲突，请解决: " + string(filesJSON)})
			job.Append(store.LogLine{Type: "error", Content: "合并未完成（仓库处于冲突态）"})
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "error", Content: "❌ 合并失败: " + mergeErr.Error()})
		job.Finish(1, store.JobError)
	}()
}

// Abort cancels an in-progress merge (git merge --abort) and returns HEAD to the
// dev branch. No-op (returns ok) when no merge is in progress.
// POST /api/requirements/{id}/merge/abort
func (h *MergeHandler) Abort(w http.ResponseWriter, r *http.Request) {
	_, dir, _, _, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}
	if midMerge(dir) {
		if _, err := gitRun(dir, "merge", "--abort"); err != nil {
			writeError(w, http.StatusInternalServerError, "ABORT_FAILED", "中止合并失败: "+err.Error())
			return
		}
	}
	// Return to the dev branch (the branch we merged from). We can't know it
	// from state alone after abort, but the requirement's coding branch follows
	// the feat/<req_id> convention; falling back to "stay on target" is safe.
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// Continue concludes a merge the user resolved manually in their editor:
// stage everything and create the merge commit, then finish the job.
// POST /api/requirements/{id}/merge/continue
func (h *MergeHandler) Continue(w http.ResponseWriter, r *http.Request) {
	reqRow, dir, _, _, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}
	if !midMerge(dir) {
		writeError(w, http.StatusBadRequest, "NO_MERGE", "当前没有进行中的合并")
		return
	}
	job := h.jobs.Create(reqRow.ID)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go func() {
		if _, err := gitRun(dir, "add", "-A"); err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ git add 失败: " + err.Error()})
			job.Finish(1, store.JobError)
			return
		}
		if remaining := conflictedFiles(dir); len(remaining) > 0 {
			filesJSON, _ := json.Marshal(remaining)
			job.Append(store.LogLine{Type: "conflict", Content: "⚠️ 仍有未解决冲突: " + string(filesJSON)})
			job.Append(store.LogLine{Type: "error", Content: "请先在编辑器中解决全部冲突再继续"})
			job.Finish(1, store.JobError)
			return
		}
		if _, err := gitRun(dir, "commit", "--no-edit"); err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 完成合并提交失败: " + err.Error()})
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "done", Content: "✅ 冲突已解决，合并完成"})
		job.Finish(0, store.JobDone)
	}()
}

// Resolve runs Claude (developer role, full tool use) to resolve the active
// merge's conflict markers, then commits to conclude the merge. Streams tool
// calls / messages into the job via runClaudeStream.
// POST /api/requirements/{id}/merge/resolve
func (h *MergeHandler) Resolve(w http.ResponseWriter, r *http.Request) {
	reqRow, dir, _, _, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}
	if !midMerge(dir) {
		writeError(w, http.StatusBadRequest, "NO_MERGE", "当前没有进行中的合并")
		return
	}
	conflicts := conflictedFiles(dir)
	if len(conflicts) == 0 {
		writeError(w, http.StatusBadRequest, "NO_CONFLICT", "没有冲突文件可解决")
		return
	}

	job := h.jobs.Create(reqRow.ID)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	systemPrompt, model := h.roleConfig()

	go func() {
		job.Append(store.LogLine{Type: "phase", Content: "🤖 Claude 正在解决合并冲突..."})

		fileList := strings.Join(conflicts, "\n")
		prompt := fmt.Sprintf("当前仓库处于 git 合并冲突状态。以下文件存在冲突标记（<<<<<<< / ======= / >>>>>>>）：\n%s\n\n"+
			"请逐个读取这些冲突文件，理解 \"ours\"（当前目标分支）与 \"theirs\"（被合并分支）双方的意图，"+
			"合理整合两边的改动、消除冲突标记后写回文件。完成后执行 `git add -A` 暂存所有已解决的文件，"+
			"再执行 `git commit --no-edit` 完成合并提交。不要留下任何冲突标记。用中文说明你的处理。", fileList)

		cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
			Prompt:       prompt,
			WorkDir:      dir,
			SystemPrompt: systemPrompt,
			Model:        model,
			// empty PermissionMode → --dangerously-skip-permissions (full tool use)
		})
		runClaudeStream(jobSink{job}, cmd, "merge-resolve", &usageCtx{
			Rec:           h.usageSvc,
			RequirementID: reqRow.ID,
			ProjectID:     reqRow.ProjectID,
			JobID:         job.ID,
			Step:          "merge",
			Model:         model,
		})

		// After the run, inspect the repo: a concluded merge no longer has
		// MERGE_HEAD. If it's gone, the AI finished; otherwise surface a hint
		// so the user can fall back to manual resolution.
		if !midMerge(dir) {
			job.Append(store.LogLine{Type: "done", Content: "✅ AI 已解决冲突并完成合并"})
			job.Finish(0, store.JobDone)
			return
		}
		if remaining := conflictedFiles(dir); len(remaining) > 0 {
			filesJSON, _ := json.Marshal(remaining)
			job.Append(store.LogLine{Type: "conflict", Content: "⚠️ AI 未能解决全部冲突: " + string(filesJSON)})
		}
		job.Append(store.LogLine{Type: "error", Content: "AI 未完成合并，请在编辑器中手动解决剩余冲突后点「继续完成合并」"})
		job.Finish(1, store.JobError)
	}()
}

// Push commits pending dev-branch changes, pushes the branch to origin, and
// appends a "pr_link" log line with a ready-to-open "create PR" URL built from
// the remote.
// POST /api/requirements/{id}/merge/push
func (h *MergeHandler) Push(w http.ResponseWriter, r *http.Request) {
	reqRow, dir, _, platformType, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}
	var body struct {
		CommitMessage string `json:"commit_message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// dev branch + the worktree/checkout where it lives. commit + push run
	// there so the push carries the dev-branch work, not the main checkout.
	dev, devDir := devBranchAndDir(reqRow, dir)
	if dev == "" || dev == "HEAD" {
		writeError(w, http.StatusBadRequest, "NO_BRANCH", "当前处于 detached HEAD，无法推送")
		return
	}
	remote := remoteURL(dir)
	if remote == "" {
		writeError(w, http.StatusBadRequest, "NO_REMOTE", "项目未配置 origin 远程仓库，无法推送")
		return
	}

	job := h.jobs.Create(reqRow.ID)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go func() {
		log.Printf("[merge/push] job %s req %s: branch=%s", job.ID, reqRow.ID, dev)

		commitMsg := body.CommitMessage
		if commitMsg == "" {
			commitMsg = dev
		}
		if committed, err := commitAll(devDir, commitMsg); err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 提交失败: " + err.Error()})
			job.Finish(1, store.JobError)
			return
		} else if committed {
			job.Append(store.LogLine{Type: "message", Content: "💾 已提交未提交改动: " + commitMsg})
		}

		// Push and surface the combined output. CombinedOutput is used instead of
		// io.Pipe — an io.Pipe write end closed before the reader drains it fails
		// with "read/write on closed pipe" (it's synchronous and unbuffered).
		job.Append(store.LogLine{Type: "phase", Content: "🌐 正在推送 " + dev + " 到 origin..."})
		out, err := exec.Command("git", "-C", devDir, "push", "-u", "origin", dev).CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				job.Append(store.LogLine{Type: "message", Content: line})
			}
		}
		if err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 推送失败: " + strings.TrimSpace(err.Error())})
			job.Finish(1, store.JobError)
			return
		}

		// Build the PR link from the remote. base = target branch.
		project, _ := h.loadProjectNoWrite(reqRow.ID)
		defaultBranch := "main"
		if project != nil && project.DefaultBranch != "" {
			defaultBranch = project.DefaultBranch
		}
		pf, webBase, owner, repo := parseRemote(remote, platformType)
		prURL := buildPRURL(pf, webBase, owner, repo, defaultBranch, dev)

		// Actually create the PR via the platform API when the project has a
		// platform token configured; otherwise fall back to the compare link.
		created := false
		if project != nil && project.PlatformType != "" && project.PlatformTokenID != "" {
			if tok, err := h.platformSvc.Get(project.PlatformTokenID); err == nil {
				if client, err := platform.New(project.PlatformType, tok.BaseURL, tok.Token); err == nil {
					title := reqRow.Title
					if title == "" {
						title = dev
					}
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					pr, cerr := client.CreatePR(ctx, remote, defaultBranch, dev, title, reqRow.Description)
					cancel()
					if cerr != nil {
						job.Append(store.LogLine{Type: "message", Content: "ℹ️ 自动创建 PR 失败（可点击下方链接手动创建）: " + cerr.Error()})
					} else if pr != nil && pr.HTMLURL != "" {
						prURL = pr.HTMLURL
						created = true
						job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("🎉 已创建 PR #%d", pr.Number)})
					}
				}
			}
		}
		if prURL != "" {
			job.Append(store.LogLine{Type: "pr_link", Content: prURL})
		}
		if created {
			job.Append(store.LogLine{Type: "done", Content: "✅ 已推送并创建 PR: " + dev})
		} else {
			job.Append(store.LogLine{Type: "done", Content: "✅ 已推送到 origin/" + dev})
		}
		job.Finish(0, store.JobDone)
	}()
}

// loadProjectNoWrite re-reads the requirement's project inside a goroutine
// (after the response is already written) to get its platform config.
func (h *MergeHandler) loadProjectNoWrite(reqID string) (*model.Project, error) {
	reqRow, err := h.reqSvc.Get(reqID)
	if err != nil {
		return nil, err
	}
	return h.projectSvc.Get(reqRow.ProjectID)
}

// Cleanup removes the requirement's isolated worktree (and prunes git's
// worktree metadata) plus its dev branch, so finished/abandoned requirements
// don't leave stray directories on disk. Refuses a dirty worktree unless
// force=true. Idempotent: a requirement without a worktree returns ok.
// POST /api/requirements/{id}/worktree/cleanup {force?:bool}
func (h *MergeHandler) Cleanup(w http.ResponseWriter, r *http.Request) {
	reqRow, dir, _, _, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}
	if reqRow.WorktreePath == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "message": "无 worktree 需要清理"})
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	wtPath := reqRow.WorktreePath
	// The worktree directory still exists → remove it via git. A dirty worktree
	// is refused unless force is set, so an in-progress dev tree isn't dropped
	// accidentally.
	if _, statErr := os.Stat(wtPath); statErr == nil {
		if !body.Force {
			if dirty := uncommittedFiles(wtPath); len(dirty) > 0 {
				filesJSON, _ := json.Marshal(dirty)
				writeError(w, http.StatusConflict, "WORKTREE_DIRTY",
					"worktree 存在未提交改动，请先提交或勾选 force 强制清理: "+string(filesJSON))
				return
			}
		}
		if err := RemoveWorktree(dir, wtPath, body.Force); err != nil {
			writeError(w, http.StatusInternalServerError, "WORKTREE_REMOVE_FAILED",
				"移除 worktree 失败: "+err.Error())
			return
		}
	}
	// Prune stale worktree metadata whether or not the dir existed, then clear
	// the DB fields so later stages fall back to the shared checkout.
	_, _ = gitRun(dir, "worktree", "prune")

	if reqRow.BranchName != "" {
		// Best-effort branch delete; non-fatal if it fails (not merged, or
		// still checked out somewhere — the user can drop it manually).
		if _, err := gitRun(dir, "branch", "-D", reqRow.BranchName); err != nil {
			log.Printf("[worktree-cleanup] branch -D %s failed (ok): %v", reqRow.BranchName, err)
		}
	}
	if perr := h.reqSvc.UpdateWorktree(reqRow.ID, "", ""); perr != nil {
		log.Printf("[worktree-cleanup] clear DB fields for %s: %v", reqRow.ID, perr)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}
