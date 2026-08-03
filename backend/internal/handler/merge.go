package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
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
	projectSvc *service.ProjectService
	reqSvc     *service.RequirementService
	llm        *llm.Gateway
	jobs       *store.JobStore
	roleSvc    *service.RoleService
}

func NewMergeHandler(projectSvc *service.ProjectService, reqSvc *service.RequirementService, llmGateway *llm.Gateway, jobs *store.JobStore, roleSvc *service.RoleService) *MergeHandler {
	return &MergeHandler{
		projectSvc: projectSvc,
		reqSvc:     reqSvc,
		llm:        llmGateway,
		jobs:       jobs,
		roleSvc:    roleSvc,
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
// DefaultBranch / PlatformType). Shared by every endpoint.
func (h *MergeHandler) loadReqProject(w http.ResponseWriter, r *http.Request) (reqID, projectPath, defaultBranch, platformType string, ok bool) {
	reqID = r.PathValue("id")
	reqRow, err := h.reqSvc.Get(reqID)
	if err != nil {
		writeError(w, http.StatusNotFound, "REQ_NOT_FOUND", "需求不存在")
		return "", "", "", "", false
	}
	project, err := h.projectSvc.Get(reqRow.ProjectID)
	if err != nil || project.LocalPath == "" {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目路径不存在")
		return "", "", "", "", false
	}
	db := project.DefaultBranch
	if db == "" {
		db = "main"
	}
	return reqID, project.LocalPath, db, project.PlatformType, true
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
	out, err := gitRun(dir, "commit", "-m", msg)
	if err != nil {
		// "nothing to commit" — git exits non-zero with this message; treat as clean.
		if strings.Contains(out, "nothing to commit") || strings.Contains(out, "working tree clean") {
			return false, nil
		}
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
	reqID, dir, defaultBranch, platformType, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}

	// Not a git repo → tell the frontend merge is unavailable.
	if _, err := gitRun(dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"is_git": false, "requirement_id": reqID,
		})
		return
	}

	dev := currentBranch(dir)
	target := defaultBranch
	hasRemote := false
	remote := remoteURL(dir)
	if remote != "" {
		hasRemote = true
	}
	pf, webBase, owner, repo := parseRemote(remote, platformType)
	uncommitted := uncommittedFiles(dir)
	ahead, behind := 0, 0
	if dev != "" && target != "" && dev != "HEAD" {
		ahead, behind = aheadBehind(dir, target, dev)
	}
	prURL := buildPRURL(pf, webBase, owner, repo, target, dev)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"is_git":           true,
		"requirement_id":   reqID,
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
	})
}

// LocalMerge commits uncommitted dev-branch changes, checks out the target
// branch, and merges dev into it. On conflict the job finishes as error with a
// "conflict" log line (file list) and the repo is left mid-merge for the
// abort/resolve/continue endpoints to pick up.
// POST /api/requirements/{id}/merge/local
func (h *MergeHandler) LocalMerge(w http.ResponseWriter, r *http.Request) {
	reqID, dir, defaultBranch, _, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}
	var body struct {
		TargetBranch string `json:"target_branch"`
		CommitMessage string `json:"commit_message"`
		DeleteBranch  bool   `json:"delete_branch"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	dev := currentBranch(dir)
	if dev == "" || dev == "HEAD" {
		writeError(w, http.StatusBadRequest, "NO_BRANCH", "当前处于 detached HEAD，无法合并")
		return
	}
	target := body.TargetBranch
	if target == "" {
		target = defaultBranch
	}
	commitMsg := body.CommitMessage

	job := h.jobs.Create(reqID)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go func() {
		log.Printf("[merge/local] job %s req %s: %s → %s", job.ID, reqID, dev, target)

		// 1. Commit pending dev-branch changes first.
		if commitMsg == "" {
			commitMsg = dev
		}
		if committed, err := commitAll(dir, commitMsg); err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 提交失败: " + err.Error()})
			job.Finish(1, store.JobError)
			return
		} else if committed {
			job.Append(store.LogLine{Type: "message", Content: "💾 已提交未提交改动: " + commitMsg})
		} else {
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ 工作区干净，无需提交"})
		}

		// 2. Checkout target.
		if out, err := gitRun(dir, "checkout", target); err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 切换目标分支失败: " + out + " " + err.Error()})
			job.Finish(1, store.JobError)
			return
		} else {
			job.Append(store.LogLine{Type: "message", Content: "🌿 切换到 " + target})
		}

		// 3. Merge dev. Use --no-edit so a merge commit (if any) doesn't open an editor.
		mergeOut, mergeErr := gitRun(dir, "merge", "--no-edit", dev)
		for _, line := range strings.Split(mergeOut, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				job.Append(store.LogLine{Type: "message", Content: line})
			}
		}
		if mergeErr == nil {
			// Optional: delete the dev branch now that it's merged.
			if body.DeleteBranch {
				if out, err := gitRun(dir, "branch", "-d", dev); err != nil {
					job.Append(store.LogLine{Type: "message", Content: "ℹ️ 删除分支失败（可忽略）: " + out})
				} else {
					job.Append(store.LogLine{Type: "message", Content: "🗑️ 已删除分支: " + dev})
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
	reqID, dir, _, _, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}
	if !midMerge(dir) {
		writeError(w, http.StatusBadRequest, "NO_MERGE", "当前没有进行中的合并")
		return
	}
	job := h.jobs.Create(reqID)
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
	reqID, dir, _, _, ok := h.loadReqProject(w, r)
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

	job := h.jobs.Create(reqID)
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
		runClaudeStream(jobSink{job}, cmd, "merge-resolve")

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
	reqID, dir, _, platformType, ok := h.loadReqProject(w, r)
	if !ok {
		return
	}
	var body struct {
		CommitMessage string `json:"commit_message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	dev := currentBranch(dir)
	if dev == "" || dev == "HEAD" {
		writeError(w, http.StatusBadRequest, "NO_BRANCH", "当前处于 detached HEAD，无法推送")
		return
	}
	remote := remoteURL(dir)
	if remote == "" {
		writeError(w, http.StatusBadRequest, "NO_REMOTE", "项目未配置 origin 远程仓库，无法推送")
		return
	}

	job := h.jobs.Create(reqID)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go func() {
		log.Printf("[merge/push] job %s req %s: branch=%s", job.ID, reqID, dev)

		commitMsg := body.CommitMessage
		if commitMsg == "" {
			commitMsg = dev
		}
		if committed, err := commitAll(dir, commitMsg); err != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 提交失败: " + err.Error()})
			job.Finish(1, store.JobError)
			return
		} else if committed {
			job.Append(store.LogLine{Type: "message", Content: "💾 已提交未提交改动: " + commitMsg})
		}

		// Stream git push output line-by-line (merge stdout+stderr).
		job.Append(store.LogLine{Type: "phase", Content: "🌐 正在推送 " + dev + " 到 origin..."})
		cmd := exec.Command("git", "-C", dir, "push", "-u", "origin", dev)
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw
		if err := cmd.Start(); err != nil {
			pw.Close()
			pr.Close()
			job.Append(store.LogLine{Type: "error", Content: "❌ push 启动失败: " + err.Error()})
			job.Finish(1, store.JobError)
			return
		}
		pw.Close()
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			job.Append(store.LogLine{Type: "message", Content: line})
		}
		pr.Close()
		waitErr := cmd.Wait()
		if waitErr != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ 推送失败: " + waitErr.Error()})
			job.Finish(1, store.JobError)
			return
		}

		// Build the PR link from the remote. base = target branch (we don't
		// know the project default here without re-reading; use the project's
		// stored default via a fresh lookup is overkill — use "main" fallback is
		// wrong for non-main projects. Re-read project default branch instead.)
		_, _, defaultBranch, _ := h.loadReqProjectNoWrite(reqID)
		pf, webBase, owner, repo := parseRemote(remote, platformType)
		prURL := buildPRURL(pf, webBase, owner, repo, defaultBranch, dev)
		if prURL != "" {
			job.Append(store.LogLine{Type: "pr_link", Content: prURL})
		}
		job.Append(store.LogLine{Type: "done", Content: "✅ 已推送到 origin/" + dev})
		job.Finish(0, store.JobDone)
	}()
}

// loadReqProjectNoWrite is the non-HTTP variant of loadReqProject for use inside
// goroutines (after the response is already written). It returns sensible
// defaults and never writes to w.
func (h *MergeHandler) loadReqProjectNoWrite(reqID string) (projectPath, defaultBranch, platformType string, ok bool) {
	reqRow, err := h.reqSvc.Get(reqID)
	if err != nil {
		return "", "", "", false
	}
	project, err := h.projectSvc.Get(reqRow.ProjectID)
	if err != nil || project.LocalPath == "" {
		return "", "", "", false
	}
	db := project.DefaultBranch
	if db == "" {
		db = "main"
	}
	return project.LocalPath, db, project.PlatformType, true
}
