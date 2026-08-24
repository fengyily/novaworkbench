package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// ReportHandler generates AI-written weekly reports for a project. Generation
// runs as a background JobStore job (same pattern as review/runner): the
// backend collects git log + requirement stats, builds a prompt carrying the
// user's rule template, runs the claude CLI with stream-json, and persists the
// final Markdown to the weekly_reports table.
type ReportHandler struct {
	projectSvc *service.ProjectService
	reportSvc  *service.ReportService
	llm        *llm.Gateway
	jobs       *store.JobStore
}

func NewReportHandler(projectSvc *service.ProjectService, reportSvc *service.ReportService, llmGateway *llm.Gateway, jobs *store.JobStore) *ReportHandler {
	return &ReportHandler{
		projectSvc: projectSvc,
		reportSvc:  reportSvc,
		llm:        llmGateway,
		jobs:       jobs,
	}
}

// List returns the report history for a project (newest first).
// GET /api/projects/{id}/reports
func (h *ReportHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	items, err := h.reportSvc.List(projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// Get returns one report.
// GET /api/projects/{id}/reports/{report_id}
func (h *ReportHandler) Get(w http.ResponseWriter, r *http.Request) {
	report, err := h.reportSvc.Get(r.PathValue("report_id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "周报不存在")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// Delete removes one report.
// DELETE /api/projects/{id}/reports/{report_id}
func (h *ReportHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.reportSvc.Delete(r.PathValue("report_id")); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// GetRule returns the project's rule template (custom or the built-in default).
// GET /api/projects/{id}/reports/rule
func (h *ReportHandler) GetRule(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"rule": h.reportSvc.GetRule(r.PathValue("id"))})
}

// GetRulePresets returns the named built-in rule templates (standard / compact)
// so the UI can offer one-click fills for the rule editor.
// GET /api/projects/{id}/reports/rule-presets
func (h *ReportHandler) GetRulePresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, service.RulePresets())
}

// SaveRule persists the project's rule template.
// PUT /api/projects/{id}/reports/rule  {"rule": "..."}
func (h *ReportHandler) SaveRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rule string `json:"rule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON")
		return
	}
	if err := h.reportSvc.SaveRule(r.PathValue("id"), req.Rule); err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"rule": req.Rule})
}

// GenerateReq is the body for starting a report generation job. Period bounds
// are optional (YYYY-MM-DD); when omitted the period is this week's Monday
// through today. Rule is a per-run override that is NOT persisted. Branch /
// Author filter which git commits are summarized: Branch="" means all
// branches, Branch="." means the current branch; Author="" means everyone.
// DiffAnalysis additionally feeds each commit's full message + file stat +
// code diff to the model, so features hidden by squash-merge messages are
// still discovered from what actually changed.
type GenerateReq struct {
	PeriodStart  string `json:"period_start"`
	PeriodEnd    string `json:"period_end"`
	Rule         string `json:"rule"`
	Branch       string `json:"branch"`
	Author       string `json:"author"`
	DiffAnalysis bool   `json:"diff_analysis"`
}

// Generate creates a background job and starts the claude CLI asynchronously.
// POST /api/projects/{id}/reports/generate
func (h *ReportHandler) Generate(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	project, err := h.projectSvc.Get(projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	var req GenerateReq
	_ = json.NewDecoder(r.Body).Decode(&req) // empty body is fine — all fields optional

	start, end, perr := resolvePeriod(req.PeriodStart, req.PeriodEnd)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "BAD_PERIOD", perr.Error())
		return
	}

	rule := strings.TrimSpace(req.Rule)
	if rule == "" {
		rule = h.reportSvc.GetRule(projectID)
	}

	branch := strings.TrimSpace(req.Branch)
	author := strings.TrimSpace(req.Author)

	// Validate the branch exists before starting a job so the user gets an
	// immediate error instead of a failed background run.
	if branch != "" && branch != "." {
		if !gitBranchExists(project.LocalPath, branch) {
			writeError(w, http.StatusBadRequest, "BRANCH_NOT_FOUND", fmt.Sprintf("分支 %q 不存在", branch))
			return
		}
	}

	job := h.jobs.Create(projectID)
	writeJSON(w, http.StatusOK, map[string]string{
		"job_id":       job.ID,
		"period_start": start.Format("2006-01-02"),
		"period_end":   end.Format("2006-01-02"),
	})

	go h.runGenerate(job, project.ID, project.Name, project.LocalPath, start, end, rule, branch, author, req.DiffAnalysis)
}

// gitBranchExists reports whether a branch (local or remote-tracking) exists.
// Names starting with "-" are rejected up front so they can never be parsed
// as flags.
func gitBranchExists(projectPath, branch string) bool {
	if strings.HasPrefix(branch, "-") {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "refs/heads/"+branch)
	cmd.Dir = projectPath
	if cmd.Run() == nil {
		return true
	}
	remote := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "refs/remotes/origin/"+branch)
	remote.Dir = projectPath
	return remote.Run() == nil
}

// GitInfo is the response for the git-info endpoint: what the branch/author
// pickers in the UI need.
type GitInfo struct {
	IsGit         bool     `json:"is_git"`
	CurrentBranch string   `json:"current_branch"`
	Branches      []string `json:"branches"`
	Authors       []string `json:"authors"`
}

// GetGitInfo returns the project's branches, current branch, and recent
// commit authors for the branch/author pickers.
// GET /api/projects/{id}/reports/git-info
func (h *ReportHandler) GetGitInfo(w http.ResponseWriter, r *http.Request) {
	project, err := h.projectSvc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	info := GitInfo{Branches: []string{}, Authors: []string{}}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	probe := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	probe.Dir = project.LocalPath
	out, err := probe.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		writeJSON(w, http.StatusOK, info) // not a git repo — is_git stays false
		return
	}
	info.IsGit = true

	head := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	head.Dir = project.LocalPath
	if out, err := head.Output(); err == nil {
		info.CurrentBranch = strings.TrimSpace(string(out))
	}

	br := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/remotes")
	br.Dir = project.LocalPath
	if out, err := br.Output(); err == nil {
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name == "" {
				continue
			}
			// Collapse remote-tracking refs to their branch name; skip origin/HEAD.
			if strings.HasPrefix(name, "origin/") {
				name = strings.TrimPrefix(name, "origin/")
				if name == "HEAD" {
					continue
				}
			}
			if !seen[name] {
				seen[name] = true
				info.Branches = append(info.Branches, name)
			}
		}
	}

	// Distinct authors from the 500 most recent commits, recent-first.
	au := exec.CommandContext(ctx, "git", "log", "--all", "-n", "500", "--pretty=format:%an")
	au.Dir = project.LocalPath
	if out, err := au.Output(); err == nil {
		seen := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name != "" && !seen[name] {
				seen[name] = true
				info.Authors = append(info.Authors, name)
			}
		}
	}

	writeJSON(w, http.StatusOK, info)
}
func (h *ReportHandler) StreamJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "任务不存在")
		return
	}

	rc := http.NewResponseController(w)
	writeSSEHeaders(w)
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

// resolvePeriod parses the optional YYYY-MM-DD bounds. Defaults: this week's
// Monday 00:00 through today (local time).
func resolvePeriod(startStr, endStr string) (time.Time, time.Time, error) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	start := today
	for start.Weekday() != time.Monday {
		start = start.AddDate(0, 0, -1)
	}
	end := today

	if startStr != "" {
		t, err := time.ParseInLocation("2006-01-02", startStr, now.Location())
		if err != nil {
			return start, end, fmt.Errorf("period_start 格式应为 YYYY-MM-DD")
		}
		start = t
	}
	if endStr != "" {
		t, err := time.ParseInLocation("2006-01-02", endStr, now.Location())
		if err != nil {
			return start, end, fmt.Errorf("period_end 格式应为 YYYY-MM-DD")
		}
		end = t
	}
	if end.Before(start) {
		return start, end, fmt.Errorf("period_end 不能早于 period_start")
	}
	return start, end, nil
}

// gitLogArgs builds the shared `git log` argument prefix for the period /
// branch / author filters (rev selection only — no format flags).
func gitLogArgs(start, end time.Time, branch, author string) []string {
	args := []string{"log",
		"--since=" + start.Format("2006-01-02") + "T00:00:00",
		"--until=" + end.Format("2006-01-02") + "T23:59:59",
		"--no-merges"}
	if author != "" {
		args = append(args, "--author="+author)
	}
	switch branch {
	case "":
		args = append(args, "--all")
	case ".":
		// current branch — no rev argument needed
	default:
		// A rev, not a pathspec: "-- <name>" would filter by file path.
		args = append(args, branch)
	}
	return args
}

// gitLogBlock collects commit data for the period from the project's git repo.
// branch="" scans all branches (--all), branch="." the current branch, any
// other value is a validated branch name passed as the rev. author="" includes
// everyone, otherwise commits are filtered with --author. All git invocations
// run locally with backend-formatted date args (no shell). Returns the
// prompt-ready text block, the commit count, and whether the directory is a
// git repo at all.
func gitLogBlock(projectPath string, start, end time.Time, branch, author string) (string, int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	probe := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	probe.Dir = projectPath
	if out, err := probe.Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		return "（该项目目录不是 git 仓库，无提交数据）", 0, false
	}

	base := gitLogArgs(start, end, branch, author)
	logArgs := append(append([]string{}, base...), "--pretty=format:%h|%an|%ad|%s|%d", "--date=format:%Y-%m-%d")
	logCmd := exec.CommandContext(ctx, "git", logArgs...)
	logCmd.Dir = projectPath
	logOut, err := logCmd.Output()
	if err != nil {
		return "（git log 读取失败，无提交数据）", 0, true
	}

	lines := []string{}
	for _, l := range strings.Split(strings.TrimSpace(string(logOut)), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// The trailing %d decoration "(HEAD -> main, tag: v1)" leaks the
		// unfiltered current-branch name into --all output; drop it.
		if i := strings.LastIndex(l, "| ("); i >= 0 {
			l = strings.TrimSpace(l[:i])
		} else if strings.HasSuffix(l, "|") {
			l = strings.TrimSpace(strings.TrimSuffix(l, "|"))
		}
		lines = append(lines, l)
	}
	count := len(lines)
	if count == 0 {
		return "（该筛选条件下无提交）", 0, true
	}

	statArgs := append(append([]string{}, base...), "--shortstat", "--pretty=format:")
	statCmd := exec.CommandContext(ctx, "git", statArgs...)
	statCmd.Dir = projectPath
	statOut, _ := statCmd.Output()
	statsSummary := summarizeShortstat(string(statOut))

	const maxLogBytes = 8 * 1024
	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 条提交%s：\n", count, statsSummary)
	for i, l := range lines {
		if b.Len()+len(l) > maxLogBytes {
			fmt.Fprintf(&b, "…（剩余 %d 条已截断）", count-i)
			break
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), count, true
}

// DiffCaps bounds how much per-commit detail the diff-analysis mode feeds to
// the model — a squash-merged feature branch can carry a huge patch.
const (
	diffMaxCommits       = 25
	diffPerCommitBytes   = 12 * 1024
	diffTotalBytes       = 48 * 1024
	diffPerCommitTimeout = 10 * time.Second
)

// gitCommitDiffBlock builds the deep-analysis block: for each commit (newest
// first, capped) the FULL message (squash bodies carry the squashed history),
// the per-file --stat, and the code patch. Binary/large files degrade to their
// stat line via the per-commit byte cap.
func gitCommitDiffBlock(projectPath string, start, end time.Time, branch, author string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	listArgs := append(gitLogArgs(start, end, branch, author), "--pretty=format:%H", "--reverse")
	listCmd := exec.CommandContext(ctx, "git", listArgs...)
	listCmd.Dir = projectPath
	out, err := listCmd.Output()
	if err != nil {
		return ""
	}
	hashes := []string{}
	for _, h := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if h = strings.TrimSpace(h); h != "" {
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return ""
	}
	skipped := 0
	if len(hashes) > diffMaxCommits {
		skipped = len(hashes) - diffMaxCommits
		hashes = hashes[skipped:] // keep the most recent commits
	}

	var b strings.Builder
	for _, h := range hashes {
		if b.Len() >= diffTotalBytes {
			fmt.Fprintf(&b, "\n…（改动明细超出总量上限，其余提交仅见上方提交清单）")
			break
		}
		showCtx, showCancel := context.WithTimeout(context.Background(), diffPerCommitTimeout)
		show := exec.CommandContext(showCtx, "git", "show", h, "--no-color", "--stat", "--patch")
		show.Dir = projectPath
		showOut, err := show.Output()
		showCancel()
		if err != nil || len(showOut) == 0 {
			continue
		}
		text := string(showOut)
		if len(text) > diffPerCommitBytes {
			text = text[:diffPerCommitBytes] + "\n…（该提交 diff 过大，已截断）"
		}
		b.WriteString("──── commit ")
		b.WriteString(h)
		b.WriteString(" ────\n")
		b.WriteString(text)
		b.WriteString("\n")
	}
	if skipped > 0 {
		return fmt.Sprintf("（共 %d 条提交，仅展开最近 %d 条的改动明细）\n", skipped+len(hashes), len(hashes)) + b.String()
	}
	return b.String()
}

// summarizeShortstat totals the "N files changed, N insertions(+), N deletions(-)"
// lines produced by git log --shortstat.
func summarizeShortstat(out string) string {
	files, ins, del := 0, 0, 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, part := range strings.Split(line, ",") {
			part = strings.TrimSpace(part)
			fields := strings.Fields(part)
			if len(fields) < 2 {
				continue
			}
			var n int
			if _, err := fmt.Sscanf(fields[0], "%d", &n); err != nil {
				continue
			}
			switch {
			case strings.HasPrefix(fields[1], "file"):
				files += n
			case strings.HasPrefix(fields[1], "insertion"):
				ins += n
			case strings.HasPrefix(fields[1], "deletion"):
				del += n
			}
		}
	}
	if files == 0 && ins == 0 && del == 0 {
		return ""
	}
	return fmt.Sprintf("，变更 %d 个文件（+%d / -%d 行）", files, ins, del)
}

// runGenerate collects data, runs the claude CLI, streams events into the job,
// and persists the resulting Markdown report.
func (h *ReportHandler) runGenerate(job *store.Job, projectID, projectName, projectPath string, start, end time.Time, rule, branch, author string, diffAnalysis bool) {
	periodStart := start.Format("2006-01-02")
	periodEnd := end.Format("2006-01-02")

	// The recorded/current-branch label: "." means "current branch at runtime",
	// resolved here so the prompt and the DB row carry the real name.
	effectiveBranch := branch
	if branch == "." {
		if info := currentBranchName(projectPath); info != "" {
			effectiveBranch = info
		}
	}

	scope := "全部分支、全部作者"
	if effectiveBranch != "" || author != "" {
		parts := []string{}
		if effectiveBranch != "" {
			parts = append(parts, "分支 "+effectiveBranch)
		} else {
			parts = append(parts, "全部分支")
		}
		if author != "" {
			parts = append(parts, "作者 "+author)
		} else {
			parts = append(parts, "全部作者")
		}
		scope = strings.Join(parts, "，")
	}

	job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("📊 正在采集 %s ~ %s 的 git 提交记录（%s）...", periodStart, periodEnd, scope)})
	gitBlock, commitCount, _ := gitLogBlock(projectPath, start, end, branch, author)

	diffBlock := ""
	if diffAnalysis && commitCount > 0 {
		job.Append(store.LogLine{Type: "phase", Content: "🔬 深度分析模式：正在逐条读取提交的代码改动（可能需要几十秒）..."})
		diffBlock = gitCommitDiffBlock(projectPath, start, end, branch, author)
	}

	reqBlock, reqActivity, err := h.reportSvc.WeeklyRequirementStats(projectID, start)
	if err != nil {
		log.Printf("[report] requirement stats failed for %s: %v", projectID, err)
		reqBlock = "（需求数据读取失败）"
	}
	job.Append(store.LogLine{Type: "phase", Content: fmt.Sprintf("📋 已汇总 %d 条提交、%d 条需求动态，Claude 正在撰写周报...", commitCount, reqActivity)})

	// In diff-analysis mode the model must treat the actual code changes as
	// more authoritative than the commit message — squash merges routinely
	// hide several features behind one terse subject line.
	diffSection := ""
	truthRule := "1. 内容必须真实，仅基于以上数据撰写，不要编造未提及的工作；"
	if diffBlock != "" {
		diffSection = "【逐条提交的代码改动明细】（完整提交信息 + 文件变更统计 + 代码 diff）\n" + diffBlock + "\n\n"
		truthRule = "1. 内容必须真实，仅基于以上数据撰写，不要编造未提及的工作；\n" +
			"2. 提交信息可能不完整（例如 squash merge 的描述未覆盖全部改动），" +
			"请以代码改动明细为准：总结时优先依据 diff 中实际新增的函数、接口、页面、配置等，" +
			"补全提交信息中未提及的工作内容，不要只按提交标题归纳；\n"
	}

	prompt := fmt.Sprintf(
		"你是研发团队的周报撰写助手。请基于以下数据，为项目「%s」撰写 %s ~ %s 的周报（Markdown，中文）。\n\n"+
			"【统计范围】%s\n\n"+
			"【生成规则】（用户自定义，请优先遵守其结构与风格要求）：\n%s\n\n"+
			"【周期内 Git 提交】\n%s\n\n"+
			diffSection+
			"【需求动态】\n%s\n\n"+
			"要求：\n"+
			truthRule+
			"3. 若某板块无数据，写「无」即可；\n"+
			"4. 如需了解某次提交的具体改动，可以使用工具读取代码核实；\n"+
			"5. 直接输出 Markdown 正文，不要输出任何额外说明。",
		projectName, periodStart, periodEnd, scope, rule, gitBlock, reqBlock)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := h.llm.StreamCmd(ctx, llm.StreamOpts{Prompt: prompt, WorkDir: projectPath})

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
	scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)

	var finalResult string
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
							job.Append(store.LogLine{Type: "tool_call", Content: toolCallLabel(fmt.Sprint(bmap["name"]), inp)})
						}
					}
				}
			}
		case "result":
			if sub, _ := evt["subtype"].(string); sub == "success" {
				if result, ok := evt["result"].(string); ok {
					finalResult = strings.TrimSpace(result)
				}
			} else if sub == "error" {
				if errMsg, ok := evt["error"].(string); ok {
					job.Append(store.LogLine{Type: "error", Content: errMsg})
				}
			}
		}
	}

	waitErr := cmd.Wait()
	if waitErr != nil {
		log.Printf("[report] job %s claude exited: %v", job.ID, waitErr)
	}

	if finalResult == "" {
		job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回周报内容，请重试"})
		job.Finish(1, store.JobError)
		return
	}

	if _, err := h.reportSvc.Create(projectID, periodStart, periodEnd, effectiveBranch, author, rule, finalResult, "done"); err != nil {
		log.Printf("[report] failed to persist report for %s: %v", projectID, err)
		job.Append(store.LogLine{Type: "error", Content: "周报生成成功但保存失败: " + err.Error()})
		job.Finish(1, store.JobError)
		return
	}

	job.Append(store.LogLine{Type: "done", Content: "✅ 周报已生成并保存"})
	job.Finish(0, store.JobDone)
	log.Printf("[report] job %s finished for project %s (%s ~ %s, %s)", job.ID, projectID, periodStart, periodEnd, scope)
}

// currentBranchName resolves the checked-out branch name ("" on failure).
func currentBranchName(projectPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = projectPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
