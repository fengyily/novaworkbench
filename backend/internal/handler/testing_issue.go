package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/platform"
	"github.com/novaworkbench/backend/internal/service"
)

// zeroTime is the "unset" sentinel for the nullable started_at column.
var zeroTime time.Time

// issueBodyCap bounds the error excerpt pasted into a platform Issue. The
// full text stays in NovaWorkbench; the issue carries enough to triage.
const issueBodyCap = 4000

// CreateCaseIssue POST /api/testing/cases/{case_id}/issue
//
// Opens a GitHub / GitLab / Gitea Issue for a failing test case and records
// the link back on the case. Idempotent: a case that already has an issue
// returns it rather than opening a second one.
func (h *TestingHandler) CreateCaseIssue(w http.ResponseWriter, r *http.Request) {
	if h.platformSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_WIRED", "平台 Token 未配置，无法创建 Issue")
		return
	}

	tc, run, project, ok := h.loadCaseContext(w, r)
	if !ok {
		return
	}
	if tc.IssueURL != "" {
		// Already escalated — hand back the existing link so a double-click
		// is harmless rather than filing a duplicate.
		writeJSON(w, http.StatusOK, map[string]any{
			"issue_url":    tc.IssueURL,
			"issue_number": tc.IssueNumber,
			"existing":     true,
		})
		return
	}
	if project.PlatformTokenID == "" {
		writeErrorSuggestion(w, http.StatusBadRequest, "NO_PLATFORM_TOKEN",
			"该项目未配置平台 Token",
			"请先在「设置 → 平台 Token」中添加 Token，并绑定到该项目。")
		return
	}
	if strings.TrimSpace(project.RemoteURL) == "" {
		writeError(w, http.StatusBadRequest, "NO_REMOTE", "该项目未配置远程仓库地址")
		return
	}

	tok, err := h.platformSvc.Get(project.PlatformTokenID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "TOKEN_LOAD_FAILED", err.Error())
		return
	}
	client, err := platform.New(tok.Platform, tok.BaseURL, tok.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PLATFORM_UNSUPPORTED", err.Error())
		return
	}

	var req model.CreateCaseIssueReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	env, _ := h.envSvc.Get(run.EnvID)
	title := issueTitle(tc, run)
	body := h.buildIssueBody(r, tc, run, env)

	iss, err := client.CreateIssue(r.Context(), project.RemoteURL, title, body, req.Labels)
	if err != nil {
		writeError(w, http.StatusBadGateway, "CREATE_ISSUE_FAILED", err.Error())
		return
	}
	if err := h.runSvc.AttachIssueURL(tc.ID, iss.HTMLURL, iss.Number); err != nil {
		log.Printf("[testing] attach issue url for case %s: %v", tc.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issue_url":    iss.HTMLURL,
		"issue_number": iss.Number,
	})
}

// CreateCaseRequirement POST /api/testing/cases/{case_id}/requirement
//
// Turns a failing case into a local requirement so it flows back into the
// normal 开发 pipeline. Idempotent for the same reason as CreateCaseIssue.
func (h *TestingHandler) CreateCaseRequirement(w http.ResponseWriter, r *http.Request) {
	if h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_WIRED", "需求服务未启用")
		return
	}

	tc, run, _, ok := h.loadCaseContext(w, r)
	if !ok {
		return
	}
	if tc.RequirementID != "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"requirement_id": tc.RequirementID,
			"existing":       true,
		})
		return
	}

	newID, err := h.reqSvc.CreateFromTestCase(run.ProjectID, tc, run)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CREATE_REQUIREMENT_FAILED", err.Error())
		return
	}
	if err := h.runSvc.AttachRequirementID(tc.ID, newID); err != nil {
		log.Printf("[testing] attach requirement for case %s: %v", tc.ID, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"requirement_id": newID})
}

// loadCaseContext fetches the case, its run and its project, writing the
// appropriate error response and returning ok=false on any failure.
func (h *TestingHandler) loadCaseContext(w http.ResponseWriter, r *http.Request) (*model.TestRunCase, *model.TestRun, *model.Project, bool) {
	tc, err := h.runSvc.GetCase(r.PathValue("case_id"))
	if errors.Is(err, service.ErrCaseNotFound) {
		writeError(w, http.StatusNotFound, "CASE_NOT_FOUND", "测试用例不存在")
		return nil, nil, nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CASE_LOAD_FAILED", err.Error())
		return nil, nil, nil, false
	}
	run, err := h.runSvc.Get(tc.RunID)
	if err != nil {
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND", "测试运行不存在")
		return nil, nil, nil, false
	}
	project, err := h.projectSvc.Get(run.ProjectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return nil, nil, nil, false
	}
	return tc, run, project, true
}

// issueTitle renders a scannable issue title from the failing case.
func issueTitle(tc *model.TestRunCase, run *model.TestRun) string {
	title := fmt.Sprintf("[自动化测试失败] %s", tc.Title)
	if tc.File != "" {
		title += fmt.Sprintf(" (%s)", tc.File)
	}
	if len(title) > 200 {
		title = title[:200]
	}
	return title
}

// buildIssueBody assembles the Markdown body: what failed, the error, and
// links back to NovaWorkbench for the screenshot and trace.
//
// Evidence is linked rather than inlined — a trace.zip can be tens of MB and
// a screenshot is a binary that would be rejected by the API — and the links
// carry the run id so a reader can reconstruct the context.
func (h *TestingHandler) buildIssueBody(r *http.Request, tc *model.TestRunCase, run *model.TestRun, env *model.TestEnvironment) string {
	var b strings.Builder
	b.WriteString("由 NovaWorkbench 自动化测试自动创建。\n\n")

	fmt.Fprintf(&b, "**测试文件**：`%s`\n\n", tc.File)
	fmt.Fprintf(&b, "**运行 ID**：`%s`\n\n", run.ID)
	if env != nil {
		fmt.Fprintf(&b, "**被测地址**：%s\n\n", env.BaseURL)
	}
	if run.StartedAt != (zeroTime) {
		fmt.Fprintf(&b, "**运行时间**：%s\n\n", run.StartedAt.Format("2006-01-02 15:04:05"))
	}

	if tc.Error != "" {
		b.WriteString("## 失败原因\n\n```\n")
		b.WriteString(truncateForPrompt(tc.Error, issueBodyCap))
		b.WriteString("\n```\n\n")
	}

	if len(tc.AttachmentList) > 0 {
		b.WriteString("## 证据\n\n")
		for _, att := range tc.AttachmentList {
			url := h.artifactURL(r, run.ID, att.Path)
			if url == "" {
				continue
			}
			fmt.Fprintf(&b, "- [%s](%s)\n", att.Name, url)
		}
		b.WriteString("\n")
	}

	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "完整报告：[在 NovaWorkbench 中查看](%s)\n", h.runURL(r, run))
	return b.String()
}

// artifactURL builds an absolute URL to one evidence file so it is clickable
// from the platform's issue view. Returns "" when the request carries no
// usable host (e.g. behind a proxy that strips it) rather than emitting a
// broken link.
func (h *TestingHandler) artifactURL(r *http.Request, runID, relPath string) string {
	if relPath == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		return ""
	}
	// PathEscape each segment so a filename with a space or a slash-bearing
	// name still produces a valid URL.
	parts := strings.Split(relPath, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return fmt.Sprintf("%s://%s/api/testing/runs/%s/files/%s",
		scheme, host, runID, strings.Join(parts, "/"))
}

// runURL is the deep link back to the run detail view, so a reader landing
// on the issue lands on the run that produced it.
func (h *TestingHandler) runURL(r *http.Request, run *model.TestRun) string {
	host := r.Host
	if host == "" || run == nil {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/projects/%s?tab=testing&run=%s",
		scheme, host, url.PathEscape(run.ProjectID), url.PathEscape(run.ID))
}
