package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/platform"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

type ReviewHandler struct {
	projectSvc  *service.ProjectService
	platformSvc *service.PlatformTokenService
	llm         *llm.Gateway
	jobs        *store.JobStore
}

func NewReviewHandler(projectSvc *service.ProjectService, platformSvc *service.PlatformTokenService, llmGateway *llm.Gateway, jobs *store.JobStore) *ReviewHandler {
	return &ReviewHandler{
		projectSvc:  projectSvc,
		platformSvc: platformSvc,
		llm:         llmGateway,
		jobs:        jobs,
	}
}

// PRListResponse wraps the PR list with a configuration flag.
type PRListResponse struct {
	Configured bool           `json:"configured"`
	PRs        []platform.PR  `json:"prs"`
}

func gitRemoteURL(localPath string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = localPath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("无法读取 git remote origin，请确认项目已配置 remote 或在项目概览中填写 Remote URL")
	}
	return strings.TrimSpace(string(out)), nil
}

// effectiveRemoteURL returns remoteURL if set, otherwise reads from git remote origin.
func effectiveRemoteURL(localPath, remoteURL string) (string, error) {
	if remoteURL != "" {
		return remoteURL, nil
	}
	return gitRemoteURL(localPath)
}

// ListPRs fetches open PRs from the configured platform.
// GET /api/projects/{id}/prs
func (h *ReviewHandler) ListPRs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := h.projectSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	if project.PlatformType == "" {
		writeJSON(w, http.StatusOK, PRListResponse{Configured: false, PRs: []platform.PR{}})
		return
	}

	// Token is optional — public repos work without one.
	var token, baseURL string
	if project.PlatformTokenID != "" {
		tok, err := h.platformSvc.Get(project.PlatformTokenID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "TOKEN_ERROR", "获取 Token 失败: "+err.Error())
			return
		}
		token = tok.Token
		baseURL = tok.BaseURL
	}

	client, err := platform.New(project.PlatformType, baseURL, token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PLATFORM_ERROR", err.Error())
		return
	}

	repoURL, err := effectiveRemoteURL(project.LocalPath, project.RemoteURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "NO_REMOTE_URL", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	prs, err := client.ListOpenPRs(ctx, repoURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "PLATFORM_API_ERROR", err.Error())
		return
	}
	if prs == nil {
		prs = []platform.PR{}
	}

	writeJSON(w, http.StatusOK, PRListResponse{Configured: true, PRs: prs})
}

// StartReviewReq is the body for starting a code review job.
type StartReviewReq struct {
	Branch            string `json:"branch"`
	BaseBranch        string `json:"base_branch"`
	PRNumber          int    `json:"pr_number"`
	PRTitle           string `json:"pr_title"`
	ExtraRequirements string `json:"extra_requirements"`
}

// StartReview creates a review job and starts Claude CLI asynchronously.
// POST /api/projects/{id}/prs/review
func (h *ReviewHandler) StartReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	project, err := h.projectSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	var req StartReviewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Branch == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "branch 字段不能为空")
		return
	}
	if req.BaseBranch == "" {
		req.BaseBranch = detectBaseBranch(project.LocalPath)
	}

	job := h.jobs.Create(id)
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})

	go h.runReview(job, project.LocalPath, req)
}

// StreamReviewJob streams a review job via SSE.
// GET /api/projects/{id}/prs/jobs/{job_id}/stream
func (h *ReviewHandler) StreamReviewJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "任务不存在")
		return
	}

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	ch, _ := job.Subscribe()
	defer job.Unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case line, more := <-ch:
			if !more {
				_, status, exitCode := job.Snapshot()
				b, _ := json.Marshal(map[string]interface{}{
					"type":      "job_done",
					"status":    string(status),
					"exit_code": exitCode,
				})
				fmt.Fprintf(w, "data: %s\n\n", b)
				rc.Flush()
				return
			}
			b, _ := json.Marshal(line)
			fmt.Fprintf(w, "data: %s\n\n", b)
			rc.Flush()
		}
	}
}

// SubmitComment posts the review body as a comment on the PR.
// POST /api/projects/{id}/prs/{pr_number}/comment
func (h *ReviewHandler) SubmitComment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	prNumStr := r.PathValue("pr_number")
	prNumber, err := strconv.Atoi(prNumStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "pr_number 必须是整数")
		return
	}

	project, err := h.projectSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	if project.PlatformType == "" || project.PlatformTokenID == "" {
		writeError(w, http.StatusBadRequest, "NOT_CONFIGURED", "项目未配置平台 Token")
		return
	}

	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "body 不能为空")
		return
	}

	tok, err := h.platformSvc.Get(project.PlatformTokenID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TOKEN_ERROR", err.Error())
		return
	}

	client, err := platform.New(tok.Platform, tok.BaseURL, tok.Token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PLATFORM_ERROR", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	repoURL, err := effectiveRemoteURL(project.LocalPath, project.RemoteURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "NO_REMOTE_URL", err.Error())
		return
	}

	if err := client.SubmitComment(ctx, repoURL, prNumber, req.Body); err != nil {
		writeError(w, http.StatusBadGateway, "PLATFORM_API_ERROR", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "submitted"})
}

// runReview runs Claude CLI against the branch diff and streams output into the job.
func (h *ReviewHandler) runReview(job *store.Job, projectPath string, req StartReviewReq) {
	binPath := h.llm.GetBinPath()

	prContext := ""
	if req.PRTitle != "" {
		prContext = fmt.Sprintf("PR 标题：%s\n", req.PRTitle)
	}
	if req.PRNumber > 0 {
		prContext += fmt.Sprintf("PR 编号：#%d\n", req.PRNumber)
	}
	if req.ExtraRequirements != "" {
		prContext += fmt.Sprintf("额外审查要求：%s\n", req.ExtraRequirements)
	}

	prompt := fmt.Sprintf(
		"你是一位资深代码审查工程师。请对以下 PR 的改动进行全面的代码 Review。\n\n"+
			"%s"+
			"目标分支：`%s`\n基础分支：`%s`\n\n"+
			"操作步骤：\n"+
			"1. 先运行 `git fetch origin` 拉取最新远端分支\n"+
			"2. 运行 `git diff origin/%s...origin/%s` 查看所有改动\n"+
			"3. 阅读改动涉及的关键源文件\n"+
			"4. 给出结构化的 Review 报告（中文）\n\n"+
			"Review 要点：\n"+
			"1. 代码正确性：逻辑错误、边界条件、并发安全\n"+
			"2. 代码质量：可读性、命名规范、重复代码\n"+
			"3. 安全性：注入、越权、敏感信息暴露\n"+
			"4. 性能：不必要的查询、内存泄漏、大循环\n"+
			"5. 测试覆盖：关键路径是否有测试\n\n"+
			"报告格式（Markdown）：\n"+
			"## 总体评价\n（一句话总结）\n\n"+
			"## 问题清单\n（按严重程度排序，每项格式：**[严重程度]** `文件路径` — 问题描述及修改建议）\n\n"+
			"## 优点\n（值得保留的好设计）\n\n"+
			"## 总结建议\n",
		prContext, req.Branch, req.BaseBranch, req.BaseBranch, req.Branch)

	cmd := exec.Command(binPath,
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	)
	cmd.Dir = projectPath

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: "启动 Claude 失败: " + err.Error()})
		job.Finish(1, store.JobError)
		return
	}

	if err := cmd.Start(); err != nil {
		job.Append(store.LogLine{Type: "error", Content: "Claude CLI 未找到，请安装: npm install -g @anthropic-ai/claude-code"})
		job.Finish(1, store.JobError)
		return
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		switch evt["type"] {
		case "assistant":
			if msg, ok := evt["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, block := range content {
						bmap, ok := block.(map[string]interface{})
						if !ok {
							continue
						}
						switch bmap["type"] {
						case "text":
							if text, ok := bmap["text"].(string); ok && strings.TrimSpace(text) != "" {
								job.Append(store.LogLine{Type: "message", Content: text})
							}
						case "tool_use":
							inp, _ := bmap["input"].(map[string]interface{})
							label := toolCallLabel(fmt.Sprint(bmap["name"]), inp)
							job.Append(store.LogLine{Type: "tool_call", Content: label})
						}
					}
				}
			}
		case "result":
			if sub, _ := evt["subtype"].(string); sub == "success" {
				if result, ok := evt["result"].(string); ok && strings.TrimSpace(result) != "" {
					job.Append(store.LogLine{Type: "message", Content: result})
				}
			} else if sub == "error" {
				if errMsg, ok := evt["error"].(string); ok {
					job.Append(store.LogLine{Type: "error", Content: errMsg})
				}
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		exitCode := 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		if exitCode != 0 {
			log.Printf("[Review] Claude exited with code %d", exitCode)
		}
	}

	job.Finish(0, store.JobDone)
}

// detectBaseBranch reads origin/HEAD or falls back to main/master.
func detectBaseBranch(projectPath string) string {
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = projectPath
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	checkMain := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/main")
	checkMain.Dir = projectPath
	if checkMain.Run() == nil {
		return "main"
	}
	return "master"
}
