package handler

import (
	"context"
	"database/sql"
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
	// genGate is the admission gate for the generation: one run per project
	// at a time, plus a platform-wide cap. See project_claude_doc_gate.go.
	genGate claudeDocGenGate
}

func NewProjectClaudeDocHandler(projectSvc *service.ProjectService, llmGateway *llm.Gateway, jobs *store.JobStore) *ProjectClaudeDocHandler {
	return &ProjectClaudeDocHandler{
		projectSvc: projectSvc,
		llm:        llmGateway,
		jobs:       jobs,
		genGate:    claudeDocGenGate{globalCap: claudeDocGenerationConcurrency()},
	}
}

// claudeDocResponse is the GET payload. `exists` is false (with an empty
// content) when the project has no document yet — a 200, not a 404, so the
// UI can render an empty state without treating it as an error.
type claudeDocResponse struct {
	Exists      bool       `json:"exists"`
	Content     string     `json:"content"`
	Source      string     `json:"source"`
	GeneratedAt *time.Time `json:"generated_at,omitempty"`
	// ManualEditedAt is when a human last edited the document. It drives
	// the "this document carries manual edits" badge and the confirmation
	// the destructive replaces ask for.
	ManualEditedAt *time.Time  `json:"manual_edited_at,omitempty"`
	UpdatedAt      *time.Time  `json:"updated_at,omitempty"`
	Bytes          int         `json:"bytes"`
	Disk           diskDocInfo `json:"disk"`
}

// diskDocInfo describes the CLAUDE.md that may already exist in the project
// directory. The CLI auto-loads that file, so the UI has to say so.
type diskDocInfo struct {
	Exists bool   `json:"exists"`
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	// ModifiedAt is the on-disk file's mtime. It is what lets the user
	// judge WHICH of the two copies is the newer one.
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	// Differs is true when the on-disk CLAUDE.md does not match the
	// managed (DB) copy — the two have diverged.
	//
	// It is only meaningful when a managed copy exists: without one there
	// is nothing to diverge from, and the disk file is simply not imported
	// yet. The UI turns this into the "磁盘版本已变更，可导入" hint —
	// without it the user keeps injecting stale conventions while their
	// edits on disk sit unnoticed.
	Differs bool `json:"differs"`
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
		resp.ManualEditedAt = doc.ManualEditedAt
		updated := doc.UpdatedAt
		resp.UpdatedAt = &updated
		resp.Bytes = doc.Bytes
	}
	// Has the on-disk copy diverged from the managed one? The comparison
	// needs a managed copy to compare against — with none, the disk file is
	// simply "not imported yet", not "outdated".
	if resp.Disk.Exists && resp.Exists && strings.TrimSpace(resp.Content) != "" {
		resp.Disk.Differs = !diskContentMatches(p.LocalPath, resp.Content)
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
	mod := st.ModTime()
	info.ModifiedAt = &mod
	return info
}

// diskContentMatches reports whether the on-disk CLAUDE.md is byte-identical
// to the managed copy.
//
// The size check comes first so a diverged document — the common case, and
// the reason this function exists — is never read from disk. Only a
// same-sized file is read, and that file is bounded by the size of the
// managed copy we already hold in memory.
func diskContentMatches(projectPath, managed string) bool {
	if strings.TrimSpace(projectPath) == "" {
		return false
	}
	path := filepath.Join(projectPath, "CLAUDE.md")
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	if st.Size() != int64(len(managed)) {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return string(data) == managed
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
//
// The version history is KEPT: the history is the safety net for exactly
// the case where a user wants the document back.
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

// ListRevisions returns the project's CLAUDE.md version history, newest
// first. Every destructive replace (an AI regeneration, a disk import)
// snapshots the outgoing document here, so a hand-edited version the user
// has already read is never lost without a way back.
func (h *ProjectClaudeDocHandler) ListRevisions(w http.ResponseWriter, r *http.Request) {
	p := h.authorize(w, r)
	if p == nil {
		return
	}
	revs, err := h.projectSvc.ListClaudeDocRevisions(p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LOOKUP_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, revs)
}

// RestoreRevision puts a frozen version back as the current document.
//
// The version that is currently served is snapshotted first, so a restore
// is itself undoable.
func (h *ProjectClaudeDocHandler) RestoreRevision(w http.ResponseWriter, r *http.Request) {
	p := h.authorize(w, r)
	if p == nil {
		return
	}
	revID := r.PathValue("rev_id")
	if revID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_ID", "revision id missing")
		return
	}
	doc, err := h.projectSvc.RestoreClaudeDocRevision(p.ID, revID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "REVISION_NOT_FOUND", "版本不存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "RESTORE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// Import copies an existing CLAUDE.md from the project directory into the
// DB. The file itself is never modified.
//
// The import REPLACES the managed document, so it is a destructive replace
// like the AI generation: when the current document carries manual edits,
// the user has to confirm before they are overwritten.
func (h *ProjectClaudeDocHandler) Import(w http.ResponseWriter, r *http.Request) {
	p := h.authorize(w, r)
	if p == nil {
		return
	}
	var body struct {
		OverwriteManual bool `json:"overwrite_manual"`
	}
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
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
	if !h.confirmOverwriteManual(w, p, body.OverwriteManual) {
		return
	}
	// Snapshot the outgoing document BEFORE it is replaced — the manual
	// edits the user is about to lose stay recoverable.
	if _, err := h.projectSvc.SnapshotClaudeDoc(p.ID, model.ProjectClaudeDocRevisionReasonImport); err != nil {
		log.Printf("[claude-doc] failed to snapshot doc for project %s: %v", p.ID, err)
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
	// OverwriteManual is the user's explicit consent to replace a
	// hand-edited document. Without it the request is refused while the
	// current document carries manual edits.
	OverwriteManual bool `json:"overwrite_manual"`
}

// confirmOverwriteManual guards the destructive replaces.
//
// A hand-edited document is the one thing in this feature a human wrote
// on purpose; an AI regeneration or a disk import replaces it wholesale.
// The user has to say yes to that — and the old content is snapshotted
// first, so the "yes" is recoverable.
//
// It writes the 409 itself and returns false when the replace must not
// proceed.
func (h *ProjectClaudeDocHandler) confirmOverwriteManual(w http.ResponseWriter, p *model.Project, overwrite bool) bool {
	if overwrite {
		return true
	}
	doc, err := h.projectSvc.ClaudeDoc(p.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LOOKUP_FAILED", err.Error())
		return false
	}
	if doc == nil || doc.ManualEditedAt == nil {
		return true
	}
	writeErrorSuggestion(w, http.StatusConflict, "MANUAL_EDIT_PROTECTED",
		"当前 CLAUDE.md 含有人工编辑的内容，重新生成/导入会覆盖它们",
		"确认覆盖后重试（overwrite_manual=true）；旧版本会自动存入历史版本，可随时恢复。")
	return false
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
	// The generation REPLACES the document, so a hand-edited one needs the
	// user's explicit consent first. The check runs BEFORE the CLI probe:
	// it is a property of the request, not of the host, and answering it
	// first keeps the 409 the user can act on.
	if !h.confirmOverwriteManual(w, p, body.OverwriteManual) {
		return
	}
	if h.llm == nil || h.llm.GetBinPath() == "" {
		writeError(w, http.StatusServiceUnavailable, "CLAUDE_CLI_MISSING", "未找到 claude CLI，无法生成 CLAUDE.md")
		return
	}
	// Admission: one generation per project at a time. A second tab (or a
	// double click that slipped through the disabled button) would start a
	// second claude process exploring the same repository, and the two runs
	// would race on the same document row — the later write silently wins,
	// so the user would end up with whichever model happened to finish last.
	// The slot is taken BEFORE the job is created, so a rejected request
	// does not even burn a JobStore slot.
	release, ok := h.genGate.tryAcquire(p.ID)
	if !ok {
		writeErrorSuggestion(w, http.StatusConflict, "GENERATION_IN_PROGRESS",
			"该项目已有 CLAUDE.md 生成任务在运行中",
			"等待当前生成结束后再试；重复触发只会让多个模型同时探索同一份仓库，最后写入的还会覆盖前一个结果。")
		return
	}
	job := h.jobs.Create(p.ID)
	go func() {
		// The slot is released on every exit path, including a panic: a
		// leaked slot would block the project's next generation forever.
		defer release()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[claude-doc] generation panicked for project %s: %v", p.ID, r)
				job.Append(store.LogLine{Type: "error", Content: "❌ 生成过程异常终止: " + fmt.Sprint(r)})
				job.Finish(1, store.JobError)
			}
		}()
		h.runGenerate(job, p, projectPath, body.Focus, body.OverwriteManual)
	}()
	writeJSON(w, http.StatusOK, map[string]interface{}{"job_id": job.ID})
}

// buildGenerationPrompt builds the prompt for a generation run and reports
// whether the generation root already contains a CLAUDE.md.
//
// The two go together on purpose: the prompt has to tell the model that the
// CLI auto-loaded that file (as project memory — as instructions, not as a
// file the model chose to read) and that it must re-derive the document
// from the code. genRoot is the directory the CLI runs in, so it — not the
// user's checkout — decides whether such a file is in reach.
func (h *ProjectClaudeDocHandler) buildGenerationPrompt(projectName, genRoot, focus string) (string, bool) {
	diskDocExists := service.DiskClaudeDocExists(genRoot)
	return promptpkg.BuildClaudeDocGenerationPrompt(projectName, focus, diskDocExists), diskDocExists
}

// runGenerate is the background worker behind POST /generate.
//
// On success the document is written to the DB with source=generated and a
// fresh generated_at. On failure the PREVIOUS document is left untouched —
// a failed regeneration must never destroy the asset the project already
// has.
//
// overwriteManual is the user's consent to replace a hand-edited document.
// The check is repeated HERE, right before the write, because the run takes
// minutes: the user may have edited the document WHILE the model was
// reading the repository, and that edit was never confirmed for overwrite.
func (h *ProjectClaudeDocHandler) runGenerate(job *store.Job, p *model.Project, projectPath, focus string, overwriteManual bool) {
	// Sync the base branch BEFORE generating: the document must describe the
	// latest main-branch code, not a stale local checkout. The commit facts
	// (branch / SHA / ahead-behind) go to the stream so the user can judge
	// what the document was built from.
	//
	// The generation is pinned to the fetched commit in a throwaway
	// worktree, so the user's working tree is never touched.
	genRoot, cleanup := h.prepareGenerationRoot(job, p, projectPath)
	defer cleanup()

	job.Append(store.LogLine{Type: "phase", Content: "🔍 正在分析项目结构（只读探索，不会修改任何文件）..."})

	// The CLI auto-loads `<cwd>/CLAUDE.md` into the session as project
	// memory, so the model may already be holding a prior version of the
	// very document it is asked to write. Tell it that this file exists and
	// that it must re-derive everything from the code — otherwise the
	// cheapest way to satisfy "write a CLAUDE.md" is to paraphrase the text
	// it was just handed.
	//
	// The flag describes the GENERATION ROOT (the cwd the CLI runs in), not
	// the user's checkout: that is where the CLI picks the file up from.
	promptText, diskDocExists := h.buildGenerationPrompt(p.Name, genRoot, focus)
	if diskDocExists {
		job.Append(store.LogLine{Type: "message",
			Content: "ℹ️ 仓库内已存在 CLAUDE.md：已要求模型以代码为准重新推导，而不是照抄既有文档。"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeDocGenerateTimeout)
	defer cancel()

	// Platform-wide admission: only a few generations may explore a
	// repository at the same time. The slot is taken AFTER the base-branch
	// sync (cheap) and BEFORE the claude run (expensive), so a queued
	// project waits with a visible reason instead of piling up claude
	// processes on the shared job slots.
	waited, err := h.genGate.acquireGlobal(ctx)
	if err != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ 等待生成槽位超时: " + err.Error()})
		job.Finish(1, store.JobError)
		return
	}
	if waited {
		job.Append(store.LogLine{Type: "phase", Content: "⏳ 正在等待生成槽位（同时生成的 CLAUDE.md 数量已达上限）..."})
	}
	defer h.genGate.releaseGlobal()

	cmd := h.llm.StreamCmd(ctx, llm.StreamOpts{
		Prompt:  promptText,
		WorkDir: genRoot,
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
	// The prompt forbids copying the on-disk CLAUDE.md, but a prompt is a
	// request, not a guarantee. When the generated document reuses most of
	// the existing one verbatim, say so on the stream: the document is
	// stored and injected into every later stage, so a silent copy of a
	// stale guide is exactly the kind of thing the user must be able to
	// catch before it becomes the project's conventions.
	if diskDocExists {
		if existing, ok, rerr := h.projectSvc.ReadDiskClaudeDoc(genRoot); rerr == nil && ok {
			if ratio := claudeDocReuseRatio(existing, content); ratio >= claudeDocReuseWarnRatio {
				job.Append(store.LogLine{Type: "warning",
					Content: fmt.Sprintf("⚠️ 生成结果复用了仓库内既有 CLAUDE.md 约 %.0f%% 的正文表述，请人工复核其中的描述是否仍与当前代码一致。", ratio*100)})
			}
		}
	}
	// Re-check the guard right before the write: the run takes minutes, and
	// the user may have saved a hand-edit in the meantime. That edit was
	// never confirmed for overwrite, so it wins — the generated document is
	// dropped and the manual one stays.
	cur, err := h.projectSvc.ClaudeDoc(p.ID)
	if err != nil {
		log.Printf("[claude-doc] failed to re-read doc for project %s: %v", p.ID, err)
	} else if cur != nil && cur.ManualEditedAt != nil && !overwriteManual {
		job.Append(store.LogLine{Type: "error",
			Content: "⚠️ 生成期间检测到人工编辑，已保留当前内容，未覆盖。如需覆盖请重新生成并确认覆盖人工编辑。"})
		job.Finish(1, store.JobError)
		return
	}
	// Snapshot the OUTGOING document before it is replaced, so a hand-edited
	// version the user has already read stays recoverable.
	if _, err := h.projectSvc.SnapshotClaudeDoc(p.ID, model.ProjectClaudeDocRevisionReasonRegenerate); err != nil {
		log.Printf("[claude-doc] failed to snapshot doc for project %s: %v", p.ID, err)
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

// prepareGenerationRoot syncs the project's base branch and returns the
// directory the generation must run in, plus a cleanup func.
//
// The directory is a throwaway worktree pinned to the freshly fetched
// origin/<base> commit — that is what makes the document reflect the latest
// main-branch content while the user's working tree stays untouched. When
// the sync or the worktree cannot be done (non-git project, unreachable
// remote, no usable ref), the generation falls back to the local checkout
// and says so in the stream.
//
// Every step reports itself through the job's log lines: the branch, the
// commit SHA and subject, and how far the checkout is from the pinned ref.
// The user needs those numbers to judge whether the document is based on the
// code they expected.
func (h *ProjectClaudeDocHandler) prepareGenerationRoot(job *store.Job, p *model.Project, projectPath string) (string, func()) {
	noop := func() {}
	emit := func(s string) {
		if job != nil && strings.TrimSpace(s) != "" {
			job.Append(store.LogLine{Type: "phase", Content: s})
		}
	}

	baseBranch := claudeDocBaseBranch(p)
	emit("🔄 正在同步主分支 " + baseBranch + "（确保 CLAUDE.md 基于最新代码）...")

	var credEnv []string
	if h.projectSvc != nil {
		env, credCleanup := h.projectSvc.BuildGitCredentialEnv(context.Background(), p.ID)
		// The askpass script is a temp file — release it when the sync is
		// done, whatever the outcome.
		defer credCleanup()
		credEnv = env
	}

	info := syncClaudeDocBase(projectPath, baseBranch, credEnv, emit)
	if info.Ref == "" {
		if info.Note == "" {
			info.Note = "未找到可用的主分支引用，基于当前工作目录生成"
		}
		emit("ℹ️ " + info.Note)
		return projectPath, noop
	}

	dir, cleanup, err := claudeDocGenerationRoot(projectPath, info.Ref)
	if err != nil {
		emit("ℹ️ 创建独立工作区失败，改用当前工作目录生成：" + err.Error())
		return projectPath, noop
	}
	emit("🗂 已在独立工作区基于 " + info.Ref + " @ " + shortSHA(info.RefSHA) + " 生成：" + dir)
	return dir, cleanup
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
// The rules are deliberately conservative, because a wrong cut DELETES
// document content and the result is stored in the DB and injected into
// every later stage:
//
//  1. the candidate title must be a Markdown H1 ("# Title" at the start of a
//     line) that sits OUTSIDE any code fence — a "# install deps" comment
//     inside a ```bash block is not a title, and treating it as one would
//     truncate the document to that comment;
//  2. no fence may appear before the title — cutting there would drop the
//     fenced content;
//  3. everything before the title must be plain prose (no heading, list,
//     table or blockquote of its own) — otherwise that text belongs to the
//     document and must be kept.
//
// When any rule fails the document is returned unchanged. Losing a
// two-sentence lead-in is acceptable; losing half the document is not.
func stripPreamble(s string) string {
	lines := strings.Split(s, "\n")
	inFence := false
	fenceSeen := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			fenceSeen = true
			continue
		}
		if inFence {
			continue
		}
		if !strings.HasPrefix(line, "# ") {
			continue
		}
		// Candidate title at line i.
		if i == 0 {
			return s
		}
		if fenceSeen {
			// A fence precedes the title — cutting here would drop it.
			return s
		}
		for _, pre := range lines[:i] {
			p := strings.TrimSpace(pre)
			if p == "" {
				continue
			}
			if !isPlainProseLine(p) {
				return s
			}
		}
		return strings.TrimSpace(strings.Join(lines[i:], "\n"))
	}
	return s
}

// unwrapWrappedFence removes a code fence that wraps the whole answer.
//
// The model sometimes wraps the document in a ```markdown fence. Unwrapping
// it is only safe when the fence is unambiguous:
//
//   - the answer holds exactly ONE fence pair;
//   - everything before the opening fence is plain prose (a document that
//     starts with a paragraph and then shows a code block must keep that
//     paragraph);
//   - nothing follows the closing fence;
//   - the fenced content starts with a Markdown heading, i.e. it really is
//     the document and not a snippet.
//
// Otherwise the answer is returned unchanged.
func unwrapWrappedFence(s string) string {
	lines := strings.Split(s, "\n")
	var fenceIdx []int
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenceIdx = append(fenceIdx, i)
		}
	}
	if len(fenceIdx) != 2 {
		return s
	}
	openIdx, closeIdx := fenceIdx[0], fenceIdx[1]
	for _, pre := range lines[:openIdx] {
		p := strings.TrimSpace(pre)
		if p == "" {
			continue
		}
		if !isPlainProseLine(p) {
			return s
		}
	}
	for _, post := range lines[closeIdx+1:] {
		if strings.TrimSpace(post) != "" {
			return s
		}
	}
	content := strings.TrimSpace(strings.Join(lines[openIdx+1:closeIdx], "\n"))
	if !startsWithHeading(content) {
		return s
	}
	return content
}

// isPlainProseLine reports whether a line is ordinary prose — i.e. it carries
// no Markdown structure of its own (heading, bullet, ordered list item,
// blockquote, table row or setext underline).
func isPlainProseLine(p string) bool {
	if p == "" {
		return false
	}
	switch p[0] {
	case '#', '-', '*', '+', '>', '|', '=':
		return false
	}
	// Ordered-list marker ("1. ", "2) ").
	if len(p) >= 3 && p[0] >= '0' && p[0] <= '9' && (p[1] == '.' || p[1] == ')') && (p[2] == ' ' || p[2] == '\t') {
		return false
	}
	return true
}

// startsWithHeading reports whether the text opens with a Markdown heading.
func startsWithHeading(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		return strings.HasPrefix(t, "# ") || strings.HasPrefix(t, "## ")
	}
	return false
}

// normalizeClaudeDocContent turns the model's raw answer into the document
// body that gets stored.
//
// Every step here may only REMOVE formatting noise (a wrapping fence, a
// chat-style lead-in) — never document content. The whole-answer fence is
// unwrapped first; the lead-in cut then runs on what is left, and the
// remaining fence pair is unwrapped afterwards (a lead-in in front of a
// fenced body is dropped by that unwrap); the lead-in cut runs once more
// in case the unwrap exposed a title.
func normalizeClaudeDocContent(raw string) string {
	t := stripCodeFence(raw)
	t = stripPreamble(t)
	t = unwrapWrappedFence(t)
	t = stripPreamble(t)
	return strings.TrimSpace(t)
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
