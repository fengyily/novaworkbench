package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// ── Project-level CLAUDE.md ───────────────────────────────────────────────
//
// A project-level CLAUDE.md is a managed asset: it lives in the DB, it is
// shared by every requirement of the project, and it is injected into the AI
// stages that need the project's conventions (analysis / design / coding /
// sub-tasks). The point is that the model stops re-discovering the project
// structure, the build commands and the layering rules on every requirement.
//
// Routes:
//
//	GET    /api/projects/{id}/claude-doc                → the document (200 even when absent)
//	PUT    /api/projects/{id}/claude-doc                → manual save (source=manual)
//	DELETE /api/projects/{id}/claude-doc                → drop the document
//	POST   /api/projects/{id}/claude-doc/generate       → AI generation (async job)
//	POST   /api/projects/{id}/claude-doc/import         → import an existing disk CLAUDE.md
//	GET    /api/projects/{id}/claude-doc/jobs/{job_id}/stream → SSE progress
//
// Permission model: these are project-scoped endpoints, and the project
// routes in this codebase are guarded by middleware.CurrentUser +
// ProjectService.CanAccess (not by RequirePermission — that gate is
// permission-key based and does not do per-project isolation).

// claudeDocGenerateTimeout bounds the whole generation run. Reading a large
// repository takes minutes; the default LLM timeout (120s) is far too tight
// and the stage timeouts of the wizard pipeline do not apply here.
const claudeDocGenerateTimeout = 15 * time.Minute

// claudeDocStallTimeout is the silent-output watchdog for the generation
// run: the CLI must emit something within this window or the run is killed.
const claudeDocStallTimeout = 10 * time.Minute

type ProjectClaudeDocHandler struct {
	projectSvc *service.ProjectService
	llm        *llm.Gateway
	jobs       *store.JobStore
}

func NewProjectClaudeDocHandler(projectSvc *service.ProjectService, llmGateway *llm.Gateway, jobs *store.JobStore) *ProjectClaudeDocHandler {
	return &ProjectClaudeDocHandler{projectSvc: projectSvc, llm: llmGateway, jobs: jobs}
}

// claudeDocResponse is the GET payload. `exists` is false (with an empty
// content) when the project has no document yet — a 200, not a 404, so the
// UI can render an empty state without treating it as an error.
type claudeDocResponse struct {
	Exists      bool        `json:"exists"`
	Content     string      `json:"content"`
	Source      string      `json:"source"`
	GeneratedAt *time.Time  `json:"generated_at,omitempty"`
	UpdatedAt   *time.Time  `json:"updated_at,omitempty"`
	Bytes       int         `json:"bytes"`
	Disk        diskDocInfo `json:"disk"`
}

// diskDocInfo describes the CLAUDE.md that may already exist in the project
// directory. The CLI auto-loads that file, so the UI has to say so.
type diskDocInfo struct {
	Exists bool   `json:"exists"`
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
}

// authorize resolves the project behind the {id} path value and checks that
// the caller may see it. Returns nil (after writing the error response) when
// the project is missing or invisible.
func (h *ProjectClaudeDocHandler) authorize(w http.ResponseWriter, r *http.Request) *model.Project {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "MISSING_ID", "project id missing")
		return nil
	}
	var userID string
	isAdmin := true
	if u := middleware.CurrentUser(r); u != nil {
		userID = u.UserID
		isAdmin = u.IsAdmin
	}
	if !isAdmin && userID != "" {
		ok, err := h.projectSvc.CanAccess(userID, false, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "ACCESS_CHECK_FAILED", err.Error())
			return nil
		}
		if !ok {
			writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "project not found")
			return nil
		}
	}
	p, err := h.projectSvc.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "PROJECT_NOT_FOUND", "项目不存在")
		return nil
	}
	return p
}

// Get returns the project-level CLAUDE.md.
//
// A project without a document answers 200 with exists=false — the frontend
// renders an empty state with a "生成" button, not an error banner.
func (h *ProjectClaudeDocHandler) Get(w http.ResponseWriter, r *http.Request) {
	p := h.authorize(w, r)
	if p == nil {
		return
	}
	resp := claudeDocResponse{Disk: h.diskInfo(p.LocalPath)}
	doc, err := h.projectSvc.ClaudeDoc(p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LOOKUP_FAILED", err.Error())
		return
	}
	if doc != nil {
		resp.Exists = true
		resp.Content = doc.Content
		resp.Source = doc.Source
		resp.GeneratedAt = doc.GeneratedAt
		updated := doc.UpdatedAt
		resp.UpdatedAt = &updated
		resp.Bytes = doc.Bytes
	}
	writeJSON(w, http.StatusOK, resp)
}

// diskInfo stats the CLAUDE.md that may live in the project directory.
func (h *ProjectClaudeDocHandler) diskInfo(projectPath string) diskDocInfo {
	info := diskDocInfo{}
	if strings.TrimSpace(projectPath) == "" {
		return info
	}
	path := filepath.Join(projectPath, "CLAUDE.md")
	info.Path = path
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return info
	}
	info.Exists = true
	info.Bytes = int(st.Size())
	return info
}

// Put saves a hand-edited document.
//
// source becomes "manual" and generated_at is NOT touched: the badge answers
// "when was this last machine-written", and a human edit must not look like
// a fresh generation.
func (h *ProjectClaudeDocHandler) Put(w http.ResponseWriter, r *http.Request) {
	p := h.authorize(w, r)
	if p == nil {
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" {
		writeError(w, http.StatusBadRequest, "EMPTY_CONTENT", "CLAUDE.md 内容不能为空")
		return
	}
	doc, err := h.projectSvc.UpsertClaudeDoc(p.ID, content, model.ProjectClaudeDocSourceManual, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// Delete removes the document. Deleting an absent document is a no-op.
func (h *ProjectClaudeDocHandler) Delete(w http.ResponseWriter, r *http.Request) {
	p := h.authorize(w, r)
	if p == nil {
		return
	}
	if err := h.projectSvc.DeleteClaudeDoc(p.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": p.ID, "deleted": true})
}

// Import copies an existing CLAUDE.md from the project directory into the
// DB. The file itself is never modified.
func (h *ProjectClaudeDocHandler) Import(w http.ResponseWriter, r *http.Request) {
	p := h.authorize(w, r)
	if p == nil {
		return
	}
	content, exists, err := h.projectSvc.ReadDiskClaudeDoc(p.LocalPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "READ_FAILED", err.Error())
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "DISK_DOC_NOT_FOUND", "项目目录下没有 CLAUDE.md，无法导入")
		return
	}
	if strings.TrimSpace(content) == "" {
		writeError(w, http.StatusBadRequest, "EMPTY_CONTENT", "磁盘上的 CLAUDE.md 是空文件")
		return
	}
	doc, err := h.projectSvc.UpsertClaudeDoc(p.ID, content, model.ProjectClaudeDocSourceImported, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// GenerateRequest is the POST /generate body. Focus is an optional extra
// instruction ("重点关注后端分层").
type GenerateRequest struct {
	Focus string `json:"focus"`
}

// Generate starts an AI generation run and returns its job id.
//
// The run is READ-ONLY: the tool layer blocks Write / Edit / Bash /
// NotebookEdit, so the "the document lives in the DB, never in the working
// tree" promise is enforced by the CLI flags and not merely requested in the
// prompt.
func (h *ProjectClaudeDocHandler) Generate(w http.ResponseWriter, r *http.Request) {
	p := h.authorize(w, r)
	if p == nil {
		return
	}
	var body GenerateRequest
	// An empty body is fine (the focus is optional) — only a malformed one
	// is an error.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	projectPath := strings.TrimSpace(p.LocalPath)
	if projectPath == "" {
		writeError(w, http.StatusConflict, "PROJECT_PATH_MISSING", "项目没有本地目录，无法生成 CLAUDE.md")
		return
	}
	st, err := os.Stat(projectPath)
	if err != nil || !st.IsDir() {
		writeError(w, http.StatusConflict, "PROJECT_PATH_MISSING",
			fmt.Sprintf("项目本地目录不可访问：%s", projectPath))
		return
	}
	if h.llm == nil || h.llm.GetBinPath() == "" {
		writeError(w, http.StatusServiceUnavailable, "CLAUDE_CLI_MISSING", "未找到 claude CLI，无法生成 CLAUDE.md")
		return
	}
	job := h.jobs.Create(p.ID)
	go h.runGenerate(job, p, projectPath, body.Focus)
	writeJSON(w, http.StatusOK, map[string]interface{}{"job_id": job.ID})
}

// runGenerate is the background worker behind POST /generate.
//
// On success the document is written to the DB with source=generated and a
// fresh generated_at. On failure the PREVIOUS document is left untouched —
// a failed regeneration must never destroy the asset the project already
// has.
func (h *ProjectClaudeDocHandler) runGenerate(job *store.Job, p *model.Project, projectPath, focus string) {
	job.Append(store.LogLine{Type: "phase", Content: "🔍 正在分析项目结构（只读探索，不会修改任何文件）..."})

	promptText := promptpkg.BuildClaudeDocGenerationPrompt(p.Name, focus)
	ctx, cancel := context.WithTimeout(context.Background(), claudeDocGenerateTimeout)
	defer cancel()

	cmd := h.llm.StreamCmd(ctx, llm.StreamOpts{
		Prompt:  promptText,
		WorkDir: projectPath,
		// The document must never end up in the user's working tree.
		DisallowedTools: []string{"Write", "Edit", "Bash", "NotebookEdit"},
	})
	job.SetCmd(cmd, cancel)

	out := runClaudeStream(jobSink{job}, cmd, "claude-doc", nil, nil, nil, 0, withStallTimeout(claudeDocStallTimeout))
	content := normalizeClaudeDocContent(out.finalResult)
	if strings.TrimSpace(content) == "" {
		// No usable document. The stream's own error message is the better
		// diagnosis when there is one (timeout, stale session, CLI crash);
		// otherwise it was a silent empty answer.
		msg := strings.TrimSpace(out.errMsg)
		if msg == "" {
			msg = "Claude 未返回 CLAUDE.md 内容，请重试"
		}
		job.Append(store.LogLine{Type: "error", Content: "❌ " + msg})
		job.Finish(1, store.JobError)
		return
	}
	if _, err := h.projectSvc.UpsertClaudeDoc(p.ID, content, model.ProjectClaudeDocSourceGenerated, true); err != nil {
		log.Printf("[claude-doc] failed to persist doc for project %s: %v", p.ID, err)
		job.Append(store.LogLine{Type: "error", Content: "CLAUDE.md 生成成功但保存失败: " + err.Error()})
		job.Finish(1, store.JobError)
		return
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ CLAUDE.md 已生成并保存"})
	job.Finish(0, store.JobDone)
}

// stripCodeFence removes a surrounding ```markdown fence if the model added
// one despite the contract. The stored document is the body, never a fence.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	// Drop the opening fence line (with an optional language tag).
	if idx := strings.Index(t, "\n"); idx >= 0 {
		t = t[idx+1:]
	} else {
		return ""
	}
	// Drop the closing fence, if present.
	if idx := strings.LastIndex(t, "```"); idx >= 0 {
		t = t[:idx]
	}
	return strings.TrimSpace(t)
}

// stripPreamble removes the chat-style lead-in the model sometimes adds
// before the document ("This repository is essentially a stub … Here is a
// CLAUDE.md:") even though the contract asks for the body only.
//
// The cut is made at the first Markdown H1 ("# Title" at the start of a
// line) — a CLAUDE.md always opens with its title, so anything before it is
// the model's commentary, not the document. When there is no H1 (the model
// started straight at "## Project Overview"), nothing is removed.
func stripPreamble(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "# ") {
			if i == 0 {
				return s
			}
			return strings.TrimSpace(strings.Join(lines[i:], "\n"))
		}
	}
	return s
}

// normalizeClaudeDocContent turns the model's raw answer into the document
// body that gets stored.
func normalizeClaudeDocContent(raw string) string {
	t := strings.TrimSpace(stripPreamble(stripCodeFence(raw)))
	// A preamble in front of a FENCED body survives the preamble cut with a
	// dangling closing fence at the end. Drop it only when the fence count
	// is odd — a document that legitimately ends with a code block has an
	// even number of fence lines and must keep them.
	if fenceLineCount(t)%2 == 1 {
		if idx := strings.LastIndex(t, "```"); idx >= 0 {
			t = strings.TrimSpace(t[:idx])
		}
	}
	return t
}

// fenceLineCount counts the lines that open or close a code fence.
func fenceLineCount(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			n++
		}
	}
	return n
}

// StreamJob streams the generation job's progress.
func (h *ProjectClaudeDocHandler) StreamJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	if jobID == "" {
		writeError(w, http.StatusBadRequest, "INVALID", "missing job id")
		return
	}
	job, ok := h.jobs.Get(jobID)
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	streamJobSSE(w, r, job, func(status store.JobStatus, exitCode int) []byte {
		payload := map[string]interface{}{
			"type":      "job_done",
			"status":    string(status),
			"exit_code": exitCode,
		}
		data, _ := json.Marshal(payload)
		return data
	})
}

// ── Injection helper ──────────────────────────────────────────────────────

// claudeDocReader is the slice of the project service the injection helper
// needs. The wizard's projectService interface satisfies it structurally.
type claudeDocReader interface {
	ClaudeDoc(projectID string) (*model.ProjectClaudeDoc, error)
}

// claudeDocBlock renders the project-level CLAUDE.md as a prompt section.
//
// It returns an EMPTY string when the project has no document — the caller
// then appends nothing, so a project without a CLAUDE.md keeps a prompt
// that is byte-for-byte identical to the one from before this feature
// existed. That is the whole reason the helper returns a string and not a
// "maybe append this" struct: the no-op has to be impossible to get wrong.
//
// diskPath is the directory the model will see (the work dir). When it
// contains a CLAUDE.md the block states that this one wins, because the
// Claude CLI auto-loads the file and the model would otherwise see two
// competing documents.
func claudeDocBlock(svc claudeDocReader, projectID, diskPath string) string {
	if svc == nil || strings.TrimSpace(projectID) == "" {
		return ""
	}
	doc, err := svc.ClaudeDoc(projectID)
	if err != nil {
		log.Printf("[claude-doc] load doc for project %s: %v", projectID, err)
		return ""
	}
	if doc == nil || strings.TrimSpace(doc.Content) == "" {
		return ""
	}
	return promptpkg.BuildProjectClaudeDocBlock(doc.Content, service.DiskClaudeDocExists(diskPath))
}
