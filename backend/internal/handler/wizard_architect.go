// Package handler — wizard pipeline (requirement → code).
//
// wizard_architect.go is the architect-stage design generator:
// the ArchitectDesign HTTP entry point, the scheduler-facing
// RunScheduledDesign, and the shared prepare/exec body. The design is
// captured from Claude's plan-mode stream (see wizard_stream.go) and
// persisted to design_docs. See wizard_common.go for shared helpers,
// wizard_stream.go for the CLI stream parser, and wizard_jobs.go for
// job introspection endpoints.

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// architectStallTimeout is the stall-watchdog window for the architect-design
// stage, overriding runClaudeStream's 3-minute default.
//
// Why this stage needs more room: plan mode lets Claude dispatch Explore
// sub-agents (the architect persona explicitly invites parallel exploration),
// and while the main thread waits on them it emits nothing at all on stdout.
// The 3-minute default therefore treated a perfectly healthy multi-module
// investigation as a hung proxy, killed the process group, and left
// out.planContent / out.finalResult both empty — finalizeArchitectRun then
// reported "Claude 未返回结果" and design_docs was never written, even though
// Claude had done (or was about to do) the work.
//
// 10 minutes covers observed Explore fan-outs with margin while keeping the
// watchdog meaningful: a genuinely dead proxy still gets killed and reported
// instead of hanging the job forever.
const architectStallTimeout = 10 * time.Minute

// continueDesignPrompt is the ENTIRE task prompt for a continuation run
// (POST /api/wizard/continue-design).
//
// It is deliberately one sentence. The run is a pure `--resume
// <design_session_id>` (no --fork-session, no --session-id — see
// prepareArchitectDesign's session decision and llm.StreamCmd), so the model
// already has the requirement, the analysis and everything the interrupted run
// had explored. Re-feeding the title, the "produce a complete plan" contract or
// the persisted partial design_docs does not add context — it reads as a new
// task and makes the model rewrite the plan from scratch, which is exactly the
// regenerate behaviour this endpoint exists to avoid.
const continueDesignPrompt = "继续。接着上次的进度继续完成方案设计，不要从头重来。"

// importDesignMaxBytes caps the Markdown content size for PUT
// /api/wizard/import-design (the JSON paste endpoint). 500 KB comfortably
// covers hand-written / chat-Copied plans; anything larger is almost
// certainly a wrong use of the endpoint (use Claude to summarise first).
//
// This is intentionally smaller than parseFileMaxBody (32 MB) and does NOT
// share the rune-truncation at parseFileMaxChars (50 000) — a 200 KB plan
// is not pathological and we want to keep the user's full content.
const importDesignMaxBytes = 500_000

// importDesignFileMaxMemory / importDesignFileMaxBody cap the multipart
// upload at /api/wizard/import-design-file. 2 MB is the documented limit
// (covers a dense .md export from any external tool); the in-memory and
// hard cap are the same so a single oversize upload is rejected cleanly
// by MaxBytesReader instead of spilling to a temp file.
const (
	importDesignFileMaxMemory = 2 << 20
	importDesignFileMaxBody   = 2 << 20
)

// ArchitectDesign is the architect-phase design generator. It creates a
// background JobStore job, persists its id on the requirement (so a page refresh
// can reconnect to the running job and show "executing" instead of the start
// button), and returns the job id immediately. Claude then runs in plan mode
// (stream-json) in a goroutine, writing progress into the job. On success the
// plan markdown is persisted to design_docs and the design_job_id is cleared;
// the requirement stays status=designing until the user manually marks 方案完成.
//
// Subscribe to the live stream via GET /api/wizard/jobs/{job_id}/stream and
// poll the snapshot via GET /api/wizard/jobs/{job_id} (same pattern as
// start-coding).
// designRunParams is the prepared-shape output of prepareArchitectDesign —
// every input the goroutine body needs after the synchronous validation +
// job creation + prompt build has succeeded. Splitting it out lets the
// scheduler path reuse the exact same exec body via RunScheduledDesign, so
// HTTP-driven and time-driven dispatches share one implementation of the
// architect stage (no parallel maintenance).
type designRunParams struct {
	Req         *model.Requirement
	ProjectPath string
	// BaseBranch is the user-picked (or defaulted) base branch for the
	// design stage. Resolution order: HTTP body / scheduler payload >
	// project.DefaultBranch > "main". Forwarded into
	// prepareDesignWorkspace → resolveWorkDirSynced, which threads it
	// both into SyncDesignBase (so the hard sync gate runs against the
	// user-chosen base) and into anchorWorktree (so the resulting
	// worktree is rooted at origin/<base>).
	BaseBranch   string
	SourceSID    string
	Fork         bool
	SkipAnalysis bool
	// Regenerate is true ONLY when the user explicitly clicked the
	// "重新分析生成" CTA on a requirement that already has a designed
	// plan. It widens the session decision (treat design_session_id as
	// if it were absent so we re-fork from the analyst session) and
	// disables the pre-mint-then-persist pattern that would otherwise
	// clobber the user's old design_session_id at run start. False for
	// every other entry point (HTTP first-run, scheduler, immediate,
	// …  anything that isn't an explicit "重新生成" by the user).
	//
	// Set by ArchitectDesign's HTTP body when the user submits
	// {regenerate: true}. All other prepareArchitectDesign call sites
	// pass false explicitly so the compiler catches a forgotten wiring
	// site.
	Regenerate bool
	// Continue marks a recovery run that resumes an interrupted design
	// session. ONLY ContinueDesign sets it — and it does so AFTER
	// prepareArchitectDesign returns, right before the exec goroutine
	// starts, so the prepare signature and every other caller stay
	// untouched. It changes the task prompt only: session resolution is
	// already handled by the non-regenerate branch (resume
	// design_session_id, no fork), and no revision snapshot is taken.
	Continue     bool
	NewDesignSID string
	WorkDir      string
	Prompt       string
	SystemPrompt string
	// SkipRoleContext — 「设计者上下文」开关的内部表示（取反式命名：
	// 零值 false = 带上架构师 persona = 今天的默认行为）。
	// true ⇒ system prompt 槽完全空置（不下发 --append-system-prompt），
	// 方案设计改由「需求描述 + 附件清单」+ claude CLI 自带上下文驱动。
	// 注意：这只摘掉 persona，模型 / claude_config 的解析仍走 roleConfig。
	SkipRoleContext bool
	// AttachManifest — 需求附件清单（## 📎 已附加文件）。
	// 勾选态：拼在 persona 之后进 system prompt（与今天逐字节一致）。
	// 关闭态：system prompt 槽空置，清单改挂任务提示词，否则附件失联。
	// 远端执行时清单里是远端目录（resolveAttachDir 传了 agentServerID），
	// 与文件实际同步到的位置一致。
	AttachManifest string
	Model          string // 已经应用请求体覆盖
	ClaudeConfigID string
	ReadKnowledge  bool
	// AgentServerID — agent_servers.id chosen in the design toolbar
	// dropdown (mirrors the dev-stage picker). Empty means the local
	// branch stays in effect; non-empty routes the run through
	// runRemoteArchitectDesign which dispatches the plan-mode claude
	// invocation to that Agent server over SSH.
	AgentServerID string
	// SyncMode — code-transport mode ("" / "local"). Forwarded into the
	// remote run as NOVA_SYNC_MODE; empty = origin clone/push, "local" =
	// git bundle over SFTP. Kept in designRunParams so the exec body can
	// thread it through to runRemoteArchitectDesign without re-deriving it
	// from reqRow.
	SyncMode string
	// Parent is the sub-requirement's master requirement, or nil when this
	// run has no usable parent. Kept (rather than just the rendered text)
	// because the fallback appendix rendered in finalizeArchitectRun needs
	// the row again.
	Parent *model.Requirement
	// ParentBlock is the pre-rendered prompt block carrying the parent's
	// description / plan / attachment manifest plus the 「主需求关联上下文」
	// output contract. Empty = no parent, or the parent lookup degraded —
	// both leave the prompt byte-identical to a top-level requirement's, so
	// this doubles as the "does this run need parent handling at all" flag
	// for finalizeArchitectRun.
	ParentBlock string
}

func (h *WizardHandler) ArchitectDesign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequirementID string `json:"requirement_id"`
		Model         string `json:"model"`
		// ClaudeConfigID — user-picked claude_configs row id (UI ModelSelect);
		// empty = backend resolves via the priority chain in
		// resolveConfigIDForRun.
		ClaudeConfigID string `json:"claude_config_id"`
		// AgentServerID — agent_servers.id for the remote plan-mode run;
		// empty = backend stays on the local claude CLI invocation. Placed
		// adjacent to ClaudeConfigID so the JSON wire format stays grouped
		// (model / config / runtime env).
		AgentServerID string `json:"agent_server_id"`
		ReadKnowledge bool   `json:"read_knowledge"`
		// SyncMode — "remote" (legacy default; agent clone/push via origin)
		// or "local" (git bundle over SFTP). Empty string = keep the existing
		// requirements.sync_mode value (Stamp-If-Non-Empty semantics so the
		// design and coding stages share the same value when only one side
		// is selected). Only honored when AgentServerID is non-empty; local
		// execution ignores it.
		SyncMode string `json:"sync_mode"`
		// BaseBranch — user-picked base branch for the design stage
		// (e.g. release/1.2, integration). Empty falls back to
		// project.DefaultBranch inside prepareArchitectDesign, then to
		// "main" if even that is empty. Mirrors the dev-stage
		// BaseBranch behaviour in wizard_coding.go:458-481.
		BaseBranch string `json:"base_branch"`
		// Regenerate — when true, the run re-forks the analyst session
		// instead of resuming the existing design session, pre-mints the
		// new session id in memory only (NOT persisted at start), freezes
		// the current plan into design_revisions before run start, and
		// persists the new session id only on success. Enables the
		// "设计完成后仍然可以重新生成" UX without clobbering the old plan.
		//
		// Only honored when the requirement is in {designing, designed}
		// — see prepareArchitectDesign's status whitelist.
		Regenerate bool `json:"regenerate"`
		// SkipRoleContext — 「设计者上下文」开关。false（缺省）= 带上架构师
		// 角色提示词（历史行为）；true = 不注入任何 system prompt，只传入需求
		// 描述 + 附件清单，采用 claude 自带上下文。
		// 旧客户端 / curl / 定时任务不带该字段 ⇒ false ⇒ 行为不变。
		SkipRoleContext bool `json:"skip_role_context"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p, job, af := h.prepareArchitectDesign(r.Context(), body.RequirementID, body.Model, body.ClaudeConfigID, body.AgentServerID, body.ReadKnowledge, body.SyncMode, body.BaseBranch, body.Regenerate, body.SkipRoleContext)
	if writeIfAPIError(w, af) {
		return
	}
	writeJSON(w, 200, map[string]string{"job_id": job.ID})
	go h.execArchitectDesign(p, job, nil)
}

// ContinueDesign resumes an interrupted architect-design run.
//
// This is the recovery path for the states the pipeline cannot otherwise
// leave: the backend restarted mid-run (the goroutine + claude subprocess are
// gone but requirements.design_session_id survived), claude died on a proxy
// 504, or the stall watchdog killed the process. In each case the design
// session on disk still holds everything the run had explored, and
// finalizeArchitectRun may even have persisted a PARTIAL plan — but the page
// only offered 「重新生成方案」, which forks a brand-new conversation from the
// analyst session and therefore throws that work away.
//
// The contract mirrors ContinueCoding (wizard_coding.go):
//   - only status=designing may continue (designed/developing want a fresh
//     plan → regenerate; draft/analyzing have no design run to resume);
//   - a non-empty design_session_id is required — without it there is nothing
//     to --resume;
//   - the run resumes the SAME session with regenerate=false, so no revision
//     snapshot is written and design_session_id is left untouched. On success
//     finalizeArchitectRun overwrites design_docs with the completed plan and
//     auto-promotes to designed, exactly like a normal design run.
//
// Execution environment comes from the requirement row (architect_model /
// architect_config_id / design_agent_server_id / sync_mode / base_branch),
// NOT from the request. Resuming is defined as "keep running the round that
// was interrupted": switching Agent servers mid-conversation can leave the
// session file behind on the other host, and a different base branch would
// re-anchor the worktree under a conversation that explored the old one. A
// user who wants a different environment should use 重新生成方案. The one
// exception is skip_role_context: it only shapes the system prompt and cannot
// affect whether --resume finds the session, so the page's current toggle is
// honoured.
//
// read_knowledge is intentionally false — the knowledge pre-read is a
// "before the run starts" choice and re-reading it on every recovery attempt
// would silently inflate cost.
//
// POST /api/wizard/continue-design { requirement_id, skip_role_context } -> { job_id }
func (h *WizardHandler) ContinueDesign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequirementID string `json:"requirement_id"`
		// SkipRoleContext — same inverted switch as ArchitectDesign: true
		// means "do not attach the architect persona".
		SkipRoleContext bool `json:"skip_role_context"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	p, job, af := h.prepareContinueDesign(r.Context(), body.RequirementID, body.SkipRoleContext)
	if writeIfAPIError(w, af) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"job_id": job.ID})
	go h.execArchitectDesign(p, job, nil)
}

// prepareContinueDesign is the synchronous half of ContinueDesign, split out so
// the validation / environment / flag decisions are unit-testable without
// spawning the exec goroutine (which needs git and a real claude CLI).
func (h *WizardHandler) prepareContinueDesign(ctx context.Context, requirementID string, skipRoleContext bool) (*designRunParams, *store.Job, *apiFailure) {
	if strings.TrimSpace(requirementID) == "" {
		return nil, nil, fail(400, "INVALID", "requirement_id is required")
	}
	req, err := h.reqSvc.Get(requirementID)
	if err != nil {
		return nil, nil, fail(404, "NOT_FOUND", "requirement not found")
	}
	// Wiki rows can also sit in status=designing (UpdateWikiDoc writes it) but
	// their session lives in wiki_session_id and their body in wiki_docs. The
	// design section is hidden for wiki in the UI; guard the API too so a
	// stale client cannot resume the wrong pipeline.
	if req.Kind == service.KindWiki {
		return nil, nil, fail(400, "KIND_NOT_SUPPORTED",
			"wiki-kind requirements do not support design continuation (use wiki generation)")
	}
	if req.Status != "designing" {
		return nil, nil, fail(409, "INVALID_STATUS",
			"仅设计中的需求可继续设计（当前状态: "+req.Status+"）；方案已完成或开发中请使用「重新生成方案」")
	}
	if strings.TrimSpace(req.DesignSessionID) == "" {
		return nil, nil, fail(409, "NO_SESSION",
			"无方案设计会话，无法续接，请重新生成方案")
	}

	// Persisted model: DefaultModelLabel is the UI sentinel for "role
	// default", not a real --model value.
	modelName := req.ArchitectModel
	if modelName == DefaultModelLabel {
		modelName = ""
	}

	p, job, af := h.prepareArchitectDesign(ctx, req.ID, modelName,
		req.ArchitectConfigID, req.DesignAgentServerID, false,
		req.SyncMode, req.BaseBranch, false, skipRoleContext)
	if af != nil {
		return nil, nil, af
	}
	// Set only after prepare succeeded, right before the caller starts the
	// exec goroutine. Every other entry point leaves it false, so the
	// continuation prompt can never leak into a first run / re-run.
	p.Continue = true
	return p, job, nil
}

// importDesignReq is the JSON body for PUT /api/wizard/import-design.
//
// Source is a free-form string ("paste" / "file") used only for log
// observability; the handler does not branch on it.
type importDesignReq struct {
	RequirementID string `json:"requirement_id"`
	Content       string `json:"content"`
	Source        string `json:"source"`
}

// ImportDesign — PUT /api/wizard/import-design
//
// Persists an externally-authored Markdown plan (ChatGPT / DeepSeek /
// Gemini / hand-written) as the requirement's design_docs and terminal-flips
// status to "designed". Deliberately bypasses Claude / JobStore / SSE: a
// single synchronous DB write is what the user expects when they hit
// "Import" after pasting a plan they already trust.
//
// Validation order is fail-fast, cheapest check first:
//  1. JSON body shape → 400 INVALID_INPUT
//  2. requirement_id present → 400 INVALID_INPUT
//  3. requirement exists → 404 REQUIREMENT_NOT_FOUND
//  4. kind != wiki → 400 KIND_NOT_SUPPORTED  (wiki has its own column)
//  5. no active Claude design/apply job → 409 DESIGN_BUSY  (avoid dual writers
//     racing on the same design_docs column)
//  6. content size <= 500 KB → 413 TOO_LARGE
//  7. content non-empty after trim → 400 INVALID_INPUT
//
// On success returns the freshly-loaded Requirement so the frontend can
// replace its local state without a second GET.
func (h *WizardHandler) ImportDesign(w http.ResponseWriter, r *http.Request) {
	var body importDesignReq
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT",
			"Failed to parse JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(body.RequirementID) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT",
			"requirement_id is required")
		return
	}
	if len(body.Content) > importDesignMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE",
			fmt.Sprintf("Content exceeds %d byte limit", importDesignMaxBytes))
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT",
			"content is empty")
		return
	}
	req, err := h.reqSvc.Get(body.RequirementID)
	if err != nil {
		if errors.Is(err, service.ErrRequirementNotFound) {
			writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	if req.Kind == service.KindWiki {
		writeError(w, http.StatusBadRequest, "KIND_NOT_SUPPORTED",
			"wiki-kind requirements do not support importing a design (use wiki_docs)")
		return
	}
	if req.DesignJobID != "" || req.ApplyJobID != "" {
		writeError(w, http.StatusConflict, "DESIGN_BUSY",
			"An AI design/apply task is currently running; please wait or cancel it first")
		return
	}
	updated, err := h.reqSvc.ImportDesignDoc(body.RequirementID, body.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	log.Printf("[wizard] import-design: req=%s source=%s bytes=%d", body.RequirementID, body.Source, len(body.Content))
	writeJSON(w, http.StatusOK, updated)
}

// ImportDesignFile — POST /api/wizard/import-design-file (multipart/form-data, field "file")
//
// Multipart companion to ImportDesign: extracts the raw bytes of a
// .md / .markdown / .txt upload and returns them as a JSON `{content: "..."}`
// envelope, WITHOUT persisting. The frontend then re-issues the import via
// PUT /api/wizard/import-design with the loaded content so the validation
// pipeline (wiki reject, busy reject, size cap) runs once at the persist
// boundary instead of twice.
//
// Why two endpoints rather than one multipart PUT: keeps the "import" action
// idempotent and reviewable (the user sees the loaded text in the dialog
// before confirming) and lets the upload endpoint stay stateless for any
// future "preview" use case.
//
// Hard cap: 2 MB (importDesignFileMaxBody). Larger payloads are rejected
// with 413 TOO_LARGE before any disk write. The size is enforced via
// http.MaxBytesReader so an attacker cannot stream gigabytes into a temp
// file before the switch rejects them.
func (h *WizardHandler) ImportDesignFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, importDesignFileMaxBody)

	if err := r.ParseMultipartForm(importDesignFileMaxMemory); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE",
				fmt.Sprintf("File exceeds %d MB limit", importDesignFileMaxBody>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_FORM",
			"Failed to parse multipart form: "+err.Error())
		return
	}

	f, fh, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_FILE",
			"Form field \"file\" is required: "+err.Error())
		return
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(fh.Filename))
	switch ext {
	case ".md", ".markdown", ".txt":
		raw, err := io.ReadAll(f)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "READ_FAILED",
				"Failed to read file: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"content": string(raw)})
	default:
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_FORMAT",
			fmt.Sprintf("Unsupported file format %q; accepted: .md, .markdown, .txt", ext))
	}
}

// RunScheduledDesign is the scheduler-facing entry point. It performs the
// same prepare step as ArchitectDesign (the validation is shared) and
// dispatches the exec body in a goroutine tagged with the scheduler's
// callback. Returns the JobStore job id (the scheduler records this in
// scheduled_tasks.job_id so /api/wizard/jobs/{id} can replay the log).
//
// agentServerID is the scheduled_tasks.agent_server_id value the scheduler
// already extracted at dispatch time (the scheduled task stores it on its
// own row); empty = local execution. Empty today is the common case —
// schedule_executor.go forwards p.AgentServerID directly.
//
// syncMode is the scheduled_tasks.sync_mode value the scheduler carries
// forward; empty = keep the requirement's persisted value (Stamp-If-Non-Empty
// semantics — same convention as the HTTP body).
//
// baseBranch is the scheduled_tasks.base_branch value the scheduler carries
// forward; empty falls back to project.DefaultBranch inside
// prepareArchitectDesign. Same fallback chain as the HTTP path so a cron
// user can override the base per-schedule without touching project settings.
func (h *WizardHandler) RunScheduledDesign(requirementID, model string, readKnowledge bool, agentServerID string, syncMode string, baseBranch string, cb *runCallbacks) (string, error) {
	// regenerate is reserved exclusively for the user-driven "重新生成"
	// HTTP CTA in ArchitectDesign. Schedules treat every dispatch as a
	// first-run / re-run, never a deliberate re-fork-from-scratch.
	// skipRoleContext=false: the scheduler has no UI to toggle the
	// 「设计者上下文」switch on, so it keeps the historical behavior
	// (architect persona attached).
	p, job, af := h.prepareArchitectDesign(context.Background(), requirementID, model, "", agentServerID, readKnowledge, syncMode, baseBranch, false, false)
	if af != nil {
		return "", af
	}
	go h.execArchitectDesign(p, job, cb)
	return job.ID, nil
}

// prepareArchitectDesign runs the synchronous portion of the architect-design
// stage: requirement lookup, project path / default branch resolution,
// session-thread resolution (design→analyst fallback), anchor check,
// session-id pre-mint, worktree creation, JobStore creation, and prompt
// construction. Returns (*designRunParams, *store.Job, *apiFailure); any
// non-nil apiFailure is a 4xx/5xx the HTTP path writes verbatim. The exec
// body (execArchitectDesign) consumes the params + job and writes progress
// into JobStore.
//
// NOTE: this is a refactor of the original ArchitectDesign — the body is
// line-for-line the same as the original synchronous section; only the
// writeError calls have been replaced with returning *apiFailure so the
// scheduler can reuse the same validation outcomes.
// baseBranch is the explicit override passed from the HTTP body or the
// scheduler; empty falls back to project.DefaultBranch, then to "main".
// Resolution happens inside the function body so every caller (HTTP /
// scheduler / immediate chained) shares the same fallback chain.
//
// regenerate — when true, the run re-forks the analyst session instead of
// resuming the existing design session, pre-mints the new session id in
// memory only (NOT persisted at run start), and freezes the current plan
// into design_revisions BEFORE the new run starts. Reserved exclusively
// for the user-driven "重新生成" HTTP body; scheduler / immediate /
// import paths must pass false explicitly so the compiler catches a
// forgotten wiring site (R1 in req_11e485759f583db8).
//
// skipRoleContext — 「设计者上下文」开关。false（缺省）= 带上架构师 persona
// （历史行为，本地路径逐字节不变）；true = system prompt 槽完全空置，不下发
// --append-system-prompt，方案改由「需求描述 + 附件清单」+ claude 自带上下文
// 驱动。取反式命名让零值 = 安全默认（= 今天的行为），旧调用方不带该字段 ⇒
// false ⇒ 行为不变。模型 / claude_config 的解析不受此开关影响（仍走
// roleConfig）。
func (h *WizardHandler) prepareArchitectDesign(ctx context.Context, requirementID, modelOverride, claudeConfigIDOverride string, agentServerID string, readKnowledge bool, syncMode string, baseBranch string, regenerate bool, skipRoleContext bool) (*designRunParams, *store.Job, *apiFailure) {
	id := requirementID
	if id == "" {
		return nil, nil, fail(400, "INVALID", "missing requirement id")
	}

	req, err := h.reqSvc.Get(id)
	if err != nil {
		return nil, nil, fail(404, "NOT_FOUND", "requirement not found")
	}

	// regenerate=true is reserved for the explicit 「重新生成方案」 CTA, which
	// is only meaningful once a plan exists. Server-side whitelist: status
	// must be {designing, designed, developing}; anything outside that set
	// means the user either hit the button on a phase that doesn't support
	// regenerating (draft / analyzing / done / archived) or the frontend's
	// button guard let a stale state through. We reject early with 409 so a
	// misbehaving client gets a clear error instead of an empty no-op.
	//
	// designing is allowed because the in-flight design run might have
	// produced a partial plan the user wants to discard.
	//
	// developing is allowed too: the requirement is already designed and the
	// user is asking for a fresh plan mid-development, which is exactly the
	// "方案设计完成后仍需重新设计" case. The run lands back on 'designed'
	// (developing→designed is a legal transition), so the new plan has to be
	// re-approved before development continues; coding_session_id and the
	// worktree are untouched, so continuing development afterwards resumes the
	// same coding thread. done / archived stay excluded — reopening a
	// finished requirement is a different (status-machine) decision.
	if regenerate {
		switch req.Status {
		case "designing", "designed", "developing":
			// allowed; fall through
		default:
			return nil, nil, fail(409, "INVALID_STATUS",
				"重新生成仅在 designing / designed / developing 状态下可用，当前状态: "+req.Status)
		}
	}

	project, _ := h.projectSvc.Get(req.ProjectID)
	projectPath := ""
	// Resolve the design-stage base branch: explicit override (HTTP body
	// or scheduler payload) wins, otherwise project.DefaultBranch, otherwise
	// the hard-coded "main" sentinel. The resolved value is later stamped
	// into designRunParams.BaseBranch and forwarded into both
	// SyncDesignBase (hard sync gate) and anchorWorktree (worktree root).
	effectiveBase := baseBranch
	if effectiveBase == "" && project != nil {
		effectiveBase = project.DefaultBranch
	}
	if effectiveBase == "" {
		effectiveBase = "main"
	}
	if project != nil {
		projectPath = project.LocalPath
	}
	// Persist the resolved base branch onto the requirement so the
	// development + merge stages can pick up the same base when the user
	// (a) picked one in design but forgot to re-pick it on the coding form
	// or (b) is targeting the PR at a non-default base. Stamp-if-non-empty
	// semantics: any value the user explicitly chose (or the project
	// fallback) wins, including re-runs that switch the base.
	if uerr := h.reqSvc.UpdateBaseBranch(id, effectiveBase); uerr != nil {
		log.Printf("[architect-design] failed to persist base_branch for %s: %v", id, uerr)
	}

	// Prologue: persist the design-stage agent-server binding BEFORE any
	// SSH / worktree work so a later failure still leaves a record of what
	// the user picked. Mirrors execStartCoding's UpdateDevSource prologue
	// (wizard_coding.go:157-187). Empty input clears the column so a
	// requirement that flips back to local execution doesn't keep
	// routing follow-up "继续设计" actions to a stale server.
	if uerr := h.reqSvc.UpdateDesignAgentServer(id, agentServerID); uerr != nil {
		log.Printf("[architect-design] failed to persist design_agent_server_id for %s: %v", id, uerr)
	}
	// Stamp sync_mode alongside design_agent_server_id — but only when the
	// user actually selected an Agent server (local execution never ships
	// code over SFTP so sync_mode stays untouched). Empty body sync_mode is
	// treated as "keep existing value" so the design stage alone never
	// silently overwrites what the coding stage stamped earlier; we also
	// keep a previously-empty row empty (don't introduce a value the user
	// never picked). The non-empty branch writes the normalized form.
	if agentServerID != "" {
		sm := model.NormalizeSyncMode(syncMode)
		if sm != "" || req.SyncMode == "" {
			if uerr := h.reqSvc.UpdateSyncMode(id, sm); uerr != nil {
				log.Printf("[architect-design] failed to persist sync_mode for %s: %v", id, uerr)
			} else {
				req.SyncMode = sm
			}
		}
	}

	// Session threading: the architect stage continues the SAME conversation
	// thread as the analyst. On the first design pass we fork off the analyst
	// session (--resume <analysis_sid> --fork-session) so the architect inherits
	// the full analysis discussion and swaps in the architect persona via
	// --system-prompt; the forked session gets a new id we read back from the
	// stream and persist as design_session_id. On a re-run (design_session_id
	// already set) we just --resume it. We do NOT re-feed Description /
	// AcceptanceCriteria — the resumed conversation already has them.
	//
	// regenerate=true overrides the first branch: the old design_session_id
	// is treated as if it were missing so we re-fork from the analyst
	// session, producing a fresh conversation thread identical to the first
	// design run (decision tree matches "no design yet + analysis exists").
	// The old design session + all doc-refine turns (which resume the same
	// design_session_id) are intentionally abandoned — restoring them is a
	// separate RestoreAsCurrent action on the version switcher.
	sourceSID := ""
	fork := false
	switch {
	case req.DesignSessionID != "" && !regenerate:
		sourceSID = req.DesignSessionID
	case req.AnalysisSessionID != "":
		sourceSID = req.AnalysisSessionID
		fork = true
	}
	// Skip-analysis path: when skip_analysis is set and there is no analyst
	// session to fork, run architect-design as a FRESH claude conversation
	// (no --resume/--fork-session) seeded only with the requirement title/
	// description + collectProjectContext. This bypasses the analyst stage.
	skipAnalysis := req.SkipAnalysis
	if sourceSID == "" && !skipAnalysis {
		return nil, nil, fail(400, "NO_SESSION", "尚未找到需求分析会话，请先完成「需求分析」再生成技术方案。")
	}

	// Forking the analyst session requires it to have been anchored to the
	// isolated worktree — otherwise the design (and later the coding stage that
	// forks the design) inherits original-dir absolute paths.
	if fork {
		if gerr := h.requireAnchoredFork(req, projectPath); gerr != nil {
			return nil, nil, fail(409, "UNANCHORED_SESSION", gerr.Error())
		}
	}

	// Pre-mint the design session id so the CLI's `--session-id` flag locks it
	// in for the duration of the run. Two cases produce a NEW id: forking off
	// the analyst session (--resume <src> --fork-session --session-id <new>)
	// and the skip-analysis fresh session (--session-id <new>). A re-run just
	// resumes the stored design_session_id (no new id).
	//
	// Persistence is conditional on regenerate: a non-regenerate run writes
	// the id to requirements.design_session_id RIGHT NOW so a backend crash
	// mid-run still leaves a recoverable session id on the row (this is the
	// original RC-2 defense: no mid-run crash can poison the row). A
	// regenerate run pre-mints the id in memory only — writing it now would
	// clobber the user's old design_session_id, so a failed regenerate would
	// leave the user with a new (broken) session id they cannot resume their
	// doc-refine chat against. The finalizeArchitectRun path stamps it on
	// success only (see the conditional there), keeping the old id intact
	// on every failure path.
	newDesignSID := ""
	if regenerate {
		newDesignSID = util.NewUUID()
	} else if fork || sourceSID == "" {
		newDesignSID = util.NewUUID()
		if perr := h.reqSvc.UpdateDesignSession(id, newDesignSID); perr != nil {
			log.Printf("[architect-design] failed to persist design session for %s: %v", id, perr)
		}
	}

	// Promote status to 'designing' before minting the job so the detail
	// page's stage gate (stageFor('designing') → 'architect') renders the
	// architect stream panel during the run. Only promote pre-design states;
	// a re-run from designed/developing stays put so we never downgrade. The
	// manual HTTP path already transitioned to 'designing' client-side, so
	// this is a no-op there — it only actively changes the immediate-launch
	// (launchDesignAndCoding) and scheduled (RunScheduledDesign) paths, which
	// have no client-side transition and otherwise leave status='draft' for
	// the whole run, hiding the architect panel behind the draft CTA.
	switch req.Status {
	case "draft", "analyzing":
		if _, serr := h.reqSvc.UpdateStatus(id, "designing"); serr != nil {
			log.Printf("[architect-design] failed to promote status to designing for %s: %v", id, serr)
		}
		req.Status = "designing"
	}

	// Concurrency guard against double-clicks / multi-tab races: a design
	// job or an apply-doc job already alive means another instance of this
	// stage is currently writing design_docs. Allowing a concurrent run
	// would silently destroy its progress (last writer wins). We return 409
	// instead of queuing so the user gets immediate feedback and can decide
	// to wait or refresh.
	//
	// Both pointers must be checked — design_job_id drives the
	// architect-design run, apply_job_id drives the doc-refine chat, and
	// BOTH paths write design_docs. wizard_immediate.go has the same pattern
	// for the design_job_id check (immediatePreGate) but never checked
	// apply_job_id; ArchitectDesign is the only HTTP entry that did not.
	// Catching both here closes that gap so the 4 callers of
	// prepareArchitectDesign all gate identically.
	if req.DesignJobID != "" && h.jobs.Live(req.DesignJobID) {
		return nil, nil, fail(409, "DESIGN_JOB_ACTIVE",
			"已有方案任务在执行，请等待完成或刷新页面确认状态")
	}
	if req.ApplyJobID != "" && h.jobs.Live(req.ApplyJobID) {
		return nil, nil, fail(409, "DESIGN_JOB_ACTIVE",
			"微调对话正在更新方案，请等待完成后再重新生成")
	}
	// A regenerate launched while the coding stage is still running would
	// rewrite design_docs (and design_session_id) underneath an in-flight
	// coding job whose premise is the plan it forked from. Only reachable on
	// the regenerate path — a first design pass can't overlap coding because
	// the frontend gates it behind the developer stage.
	if regenerate && req.CodingJobID != "" && h.jobs.Live(req.CodingJobID) {
		return nil, nil, fail(409, "DESIGN_JOB_ACTIVE",
			"开发任务正在执行，请等待完成或暂停开发后再重新生成方案")
	}

	// Freeze the current design plan as a version snapshot BEFORE the new
	// run starts so the user can restore v(N) after the run replaces it
	// with v(N+1). Only meaningful on regenerate (the first design pass has
	// no v1 to freeze; Snapshot returns (nil, nil) when design_docs is
	// empty). The reverse order — Snapshot AFTER the run — would lose the
	// old plan on every success path. The cleanup inside UpdateDesign /
	// ImportDesignDoc / Delete handles the rest.
	//
	// trigger = "regenerate" so the version dropdown's badge correctly
	// identifies the origin of this frozen row in case the user later asks
	// "why was v2 here?".
	if regenerate {
		snapReq, gerr := h.reqSvc.Get(id)
		if gerr == nil {
			_, _ = h.designRevSvc.Snapshot(snapReq, model.RevisionTriggerRegenerate)
		}
	}

	// Create the job BEFORE resolveWorkDirLogged so the log echo closure can
	// fan the worktree sync lines ("🔄 已同步 origin/<base>" etc.) into the SSE
	// panel. We persist design_job_id now and clear it on terminal failure
	// paths below; any fail(...) above (req not found, no source session,
	// unanchored fork) returns before reaching here so a "stuck" design_job_id
	// never leaks across runs.
	job := h.jobs.Create(id)
	job.SetType("architect_design")
	if perr := h.reqSvc.UpdateDesignJob(id, job.ID); perr != nil {
		log.Printf("[architect-design] failed to persist design_job_id for %s: %v", id, perr)
	}

	// Worktree anchoring + prompt construction used to happen synchronously
	// here. They've moved into prepareDesignWorkspace, which runs inside the
	// goroutine spawned by execArchitectDesign — moving them off the HTTP
	// request thread is what lets the SSE panel show the hard-sync phase
	// events in real time (a 60s fetch used to hang the HTTP connection
	// before the user saw anything). The design_job_id is rolled back inside
	// prepareDesignWorkspace on any failure path, keeping the "no stale
	// design_job_id across runs" invariant that the pre-move code maintained
	// via the early-return on wdErr.

	// rolePrompt = 架构师 persona；model / claudeConfigID 仍然来自同一个
	// roleConfig —— 关掉 persona 不等于关掉角色上的模型与网关绑定。
	rolePrompt, model, claudeConfigID := h.roleConfig("architect")
	// Append the per-requirement attachment manifest so the architect sees
	// the same files the analyst did and can ground the design on real
	// evidence (logs / specs / repro screenshots). When the user picked an
	// Agent Server for the design stage, resolveAttachDir (which delegates
	// to the package-level resolveAttachDirFor) copies every file over to
	// ~/.nova/attachments/<req_id>/ first; on sync failure the prompt
	// silently falls back to local paths.
	attachDir := h.resolveAttachDir(context.Background(), requirementID, agentServerID)
	manifest := h.attachManifestFor(requirementID, attachDir)

	// SystemPrompt 的组装：
	//   skipRoleContext=false（默认）→ persona + 清单，与今天逐字节一致
	//                                （本地路径零回归）；
	//   skipRoleContext=true          → 空串：streamArgs 会完全不下发
	//                                   --append-system-prompt，claude 用自己的
	//                                   默认 system prompt（含 plan-mode 内建
	//                                   指令）；清单改由任务提示词承载，否则
	//                                   附件会失联。
	systemPrompt := rolePrompt
	if manifest != "" {
		systemPrompt = systemPrompt + "\n\n" + manifest
	}
	if skipRoleContext {
		systemPrompt = ""
	}
	// Per-request model override (highest precedence); empty means role default.
	if modelOverride != "" {
		model = modelOverride
	}
	// Align the gateway config with the picked model. See resolveConfigIDForRun.
	claudeConfigID = h.resolveConfigIDForRun(claudeConfigIDOverride, model, claudeConfigID)
	job.SetModel(model)

	// Sub-requirement design needs the master's already-settled decisions in
	// view. Built here (synchronously, next to the attachment manifest above
	// so both share the same resolveAttachDir semantics) but appended to the
	// prompt inside prepareDesignWorkspace.
	parent, parentBlock := h.buildParentBlock(context.Background(), req, agentServerID)

	return &designRunParams{
		Req:           req,
		ProjectPath:   projectPath,
		BaseBranch:    effectiveBase,
		SourceSID:     sourceSID,
		Fork:          fork,
		SkipAnalysis:  skipAnalysis,
		Regenerate:    regenerate,
		NewDesignSID:  newDesignSID,
		Parent:        parent,
		ParentBlock:   parentBlock,
		// WorkDir + Prompt are populated by prepareDesignWorkspace inside
		// execArchitectDesign's goroutine — they live off the HTTP request
		// thread so the SSE panel can surface the hard-sync phase events
		// (see prepareDesignWorkspace for the full rationale).
		WorkDir:        "",
		Prompt:         "",
		SystemPrompt:   systemPrompt,
		SkipRoleContext: skipRoleContext,
		AttachManifest:  manifest,
		Model:          model,
		ClaudeConfigID: claudeConfigID,
		ReadKnowledge:  readKnowledge,
		AgentServerID:  agentServerID,
		SyncMode:       syncMode,
	}, job, nil
}

// buildParentBlock renders the master-requirement context for a
// sub-requirement's design run. Returns (nil, "") when there is nothing to
// inject, which is the overwhelmingly common case (top-level requirement) and
// leaves the prompt byte-identical to before this feature existed.
//
// Every failure path degrades to "no parent" rather than erroring out. The
// parent is *reference material* — its absence makes the design less informed
// but never wrong, so blocking the stage would trade a soft problem for a hard
// one. This mirrors roleConfig's "a broken role config must not block the
// pipeline" rule. Specifically: a deleted parent, a cross-project link, or a
// grandparent (ParentID is one level deep by schema but we re-check defensively,
// since SetParent's validation predates some rows) all log and return empty.
//
// The attachment directory is resolved through the same resolveAttachDir the
// sub-requirement's own manifest uses, so an Agent-server run gets the parent's
// files synced to that host under its own per-requirement subdirectory (owner
// id is the parent's, so no collision with the sub-requirement's mirror).
func (h *WizardHandler) buildParentBlock(ctx context.Context, req *model.Requirement, agentServerID string) (*model.Requirement, string) {
	if req == nil || strings.TrimSpace(req.ParentID) == "" {
		return nil, ""
	}
	parent, err := h.reqSvc.Get(req.ParentID)
	if err != nil {
		log.Printf("[architect-design] parent lookup failed for %s (parent=%s): %v — continuing without parent context", req.ID, req.ParentID, err)
		return nil, ""
	}
	if parent.ProjectID != req.ProjectID {
		log.Printf("[architect-design] parent %s is in project %s but child %s is in %s — skipping parent context", parent.ID, parent.ProjectID, req.ID, req.ProjectID)
		return nil, ""
	}
	if strings.TrimSpace(parent.ParentID) != "" {
		log.Printf("[architect-design] parent %s is itself a child (parent_id=%s) — nesting is one level deep, skipping parent context", parent.ID, parent.ParentID)
		return nil, ""
	}
	dir := h.resolveAttachDir(ctx, parent.ID, agentServerID)
	return parent, promptpkg.BuildParentContextBlock(parent, h.attachManifestFor(parent.ID, dir))
}

// parentTitle is the display name for the master requirement in job-log lines.
// Falls back to the id so the warning still names something actionable when
// the title is blank (possible on rows imported before Title became required).
func parentTitle(parent *model.Requirement) string {
	if parent == nil {
		return "(未知主需求)"
	}
	if t := strings.TrimSpace(parent.Title); t != "" {
		return t
	}
	return parent.ID
}

// execArchitectDesign runs the goroutine body of the architect-design stage.
// Same line-for-line as the original anonymous goroutine in ArchitectDesign,
// except `body.X` references are now `p.X`. Additions:
//   - A deferred persist of the finished job log (jobLogSvc.Save) so the
//     architect-design execution log survives a backend restart — same
//     pattern StartCoding already had, now extended here to close the
//     existing data-loss gap noted in plan-stateful-stardust.md (fact 1).
//   - The OnFinish callback fires after Save so the scheduler can flip
//     scheduled_tasks.running → succeeded/failed with the same job_id.
//   - A remote branch: when designRunParams.AgentServerID is set the run
//     dispatches to an Agent server via runRemoteArchitectDesign, mirroring
//     the dev-stage local/remote split in StartCoding. The terminal-state
//     handling is identical between branches so any future change only
//     needs to land in one place.

// prepareDesignWorkspace runs inside the architect-design goroutine spawned by
// execArchitectDesign. It owns the parts that used to live in
// prepareArchitectDesign's HTTP thread:
//
//  1. the hard git sync to origin/<default_branch> (gates + fetch + ff + SHA
//     capture), which can take up to git.sync_timeout_seconds (default 60s)
//     on a slow repo;
//  2. the worktree anchor for the requirement;
//  3. the design_base_sha stamp on the requirement (used by merge.go to
//     annotate the PR body);
//  4. the plan-mode task prompt construction (the three branches that used
//     to live at the tail of prepareArchitectDesign — analyst-resume /
//     fresh-skip-analysis / DesignContextSummary-prefix).
//
// Moving these off the HTTP request thread is what lets the SSE panel show
// the hard-sync phase events ("🔄 同步仓库基线到 origin/<base>…" / "📌 设计
// 基线已锁定: abc1234") in real time. Previously the user's HTTP connection
// hung for up to 60s before job_id came back — they had no idea anything
// was happening.
//
// Returns true on success (p.WorkDir and p.Prompt are populated), false on
// any failure (the job has already been Finished with status=JobError and
// design_job_id has been rolled back, mirroring the legacy early-return on
// wdErr). The caller should return immediately on false.
func (h *WizardHandler) prepareDesignWorkspace(p *designRunParams, job *store.Job) bool {
	// Surface the hard-sync phase up-front so the user sees something is
	// happening even on a slow network. The actual fetch line is emitted by
	// SyncToOriginBase through logf (which we wire to a job.Append below).
	job.Append(store.LogLine{Type: "phase", Content: "🔄 同步仓库基线到 origin/" + p.BaseBranch + "…"})

	// Read the timeout fresh on every design entry so a settings change takes
	// effect without restarting the backend (same hot-reload story as the
	// subtask concurrency / auto-retry settings). Missing/garbage → 60s.
	timeout, _ := h.settingSvc.GitSyncTimeout()

	// skipOriginSync=true when an Agent Server is selected: the local
	// fetch+gate is redundant (the SSH-side originTransport.PrepareRemote
	// will run its own sync against origin/<base>) and a local FETCH_FAILED
	// would wrongly hard-abort a run whose remote path is perfectly healthy.
	// The remote side still gets baseSHA via parseBaseSHAFromRemoteSyncOutput
	// (wizard_remote.go:1262-1268) and overwrites design_base_sha — so
	// returning baseSHA="" here is intentional.
	skipOriginSync := p.AgentServerID != ""
	workDir, baseSHA, err := h.resolveWorkDirSynced(context.Background(), p.Req, p.ProjectPath, p.BaseBranch, timeout, func(s string) {
		job.Append(store.LogLine{Type: "message", Content: s})
	}, skipOriginSync)
	if err != nil {
		// Render the gate failure in the same SSE panel the user is watching.
		// *service.SyncGateError carries a human-readable Msg plus the raw
		// git stderr / porcelain / log output (truncated to 2KB by
		// SyncToOriginBase) — both get their own error line so the user can
		// see the actionable Chinese hint AND the diagnostic output.
		var gateErr *service.SyncGateError
		if errors.As(err, &gateErr) {
			job.Append(store.LogLine{Type: "error", Content: gateErr.Msg})
			if gateErr.Stderr != "" {
				job.Append(store.LogLine{Type: "error", Content: gateErr.Stderr})
			}
		} else {
			job.Append(store.LogLine{Type: "error", Content: err.Error()})
		}
		// Roll back the design_job_id so the next attempt can mint a fresh
		// job; the user shouldn't see a dead "executing" pointer after the
		// gate failed. Mirrors the legacy early-return on wdErr.
		_ = h.reqSvc.UpdateDesignJob(p.Req.ID, "")
		job.Finish(1, store.JobError)
		return false
	}

	p.WorkDir = workDir

	// Stamp the design baseline SHA before spawning claude, so the record
	// survives a mid-run crash (the merge stage reads this column for the
	// PR-body annotation). Skipped when SyncDesignBase returned
	// Skipped=true (non-git / no origin / unborn HEAD) — baseSHA is "" in
	// that case and we simply omit the phase event.
	if baseSHA != "" {
		if uerr := h.reqSvc.UpdateDesignBaseSHA(p.Req.ID, baseSHA); uerr != nil {
			log.Printf("[architect-design] failed to persist design_base_sha for %s: %v", p.Req.ID, uerr)
		}
		job.Append(store.LogLine{Type: "phase", Content: "📌 设计基线已锁定: " + baseSHA[:7]})
	}

	// Plan-mode task prompt — relocated verbatim from prepareArchitectDesign
	// (the three branches: analyst-resume, fresh skip-analysis, and the
	// DesignContextSummary handoff). Logic intentionally unchanged so
	// wizard_architect_test.go's prompt-branch coverage keeps applying.
	// A fourth branch (p.Continue) replaces the whole prompt with
	// continueDesignPrompt — see the constant's doc comment.
	//
	// Plan-mode task prompt. When resuming/forking an existing conversation
	// (analyst session present), the resumed thread already carries the
	// requirement and its analysis — we just ask Claude to switch to the
	// architect role and produce a plan. On the skip-analysis path (no
	// session) we must seed the fresh conversation with the requirement plus
	// pre-read project context, since there is no prior discussion to inherit.
	// 需求标题始终作为锚点：resume 分支依赖「续接的会话已携带需求」这一假设，
	// 一旦该假设不成立（会话失效 / 被清理 / 上下文被压缩），模型就完全看不到
	// 任务内容。带上标题的成本极低，可避免该假设失效时提示词变成空壳。
	//
	// 「设计者上下文」关闭时的两块需求物料：
	//
	// ① descBlock —— resume/fork 分支今天只带需求标题，正文靠「续接的分析师
	//    会话已携带需求」这条假设；persona 缺席后该假设不再可靠，需求描述
	//    必须显式出现在任务提示词里。
	// ② 附件清单 —— system prompt 槽此时是空的，附件清单必须跟着需求物料
	//    一起挪进任务提示词，否则 Claude 看不到用户上传的日志 / 截图。
	//    清单是自包含的 markdown 块（service/attachment.go），作为任务提示词的
	//    前缀读起来完全正常（与知识库块 / Skills 块同类）。
	descBlock := ""
	if p.SkipRoleContext && p.Req.Description != "" {
		descBlock = "## 需求描述\n" + p.Req.Description + "\n\n"
	}
	prompt := "## 需求标题\n" + p.Req.Title + "\n\n" +
		descBlock +
		"现在切换到「架构师」角色。基于我们刚才完成的需求分析对话，" +
		"请阅读项目相关源文件核实技术细节，制定具体可执行的技术实现方案（plan）。" +
		"方案应涵盖：整体实现思路、需要新增或修改的文件、数据模型/数据库变更、实现风险及应对。"
	if p.Continue {
		// Continuation of an interrupted run (ContinueDesign is the only
		// writer of p.Continue). The resumed design session already carries
		// the requirement, the analysis and whatever the previous run had
		// explored, so the prompt is deliberately just "继续" — anything
		// more (title / plan contract / partial design_docs) makes the model
		// rewrite the plan instead of picking up where it stopped.
		prompt = continueDesignPrompt
		// Same skip_role_context material as the resume branch: with the
		// persona removed, the resumed session is the only source of truth
		// about the requirement, and that assumption is exactly what the
		// description backstop exists for.
		if descBlock != "" {
			prompt = descBlock + prompt
		}
	} else if p.SkipAnalysis && p.SourceSID == "" {
		docBlock, _, treeSummary := collectProjectContext(workDir, p.Req.Title)
		prompt = "现在切换到「架构师」角色。请基于以下需求与项目信息，阅读相关源文件核实技术细节，" +
			"制定具体可执行的技术实现方案（plan）。\n\n" +
			"## 需求标题\n" + p.Req.Title + "\n\n" +
			"## 需求描述\n" + p.Req.Description + "\n\n" +
			"## 项目上下文\n" + docBlock + "\n" + treeSummary + "\n\n" +
			"方案应涵盖：整体实现思路、需要新增或修改的文件、数据模型/数据库变更、实现风险及应对。" +
			"请先复述你对需求的理解，再给出方案。"
	}
	// 附件清单前缀 —— 只在关闭态出现（勾选态它在 system prompt 里）。
	// 勾选态（默认）此块是 no-op，提示词与改动前逐字节一致。
	if p.SkipRoleContext && p.AttachManifest != "" {
		prompt = p.AttachManifest + "\n\n" + prompt
	}
	// Context-compression handoff (fresh-session path only): when the
	// design stage was previously compressed, the requirement carries a
	// Chinese summary we want the architect to see as scene-setting
	// context. We only inject on the fresh-session path because the
	// resume path (skipAnalysis==false) inherits the analyst conversation
	// natively via --resume, where the prior design summary isn't
	// applicable. The prefix also goes BEFORE the rest of the prompt so
	// the model treats it as ground truth rather than as a post-hoc
	// addendum, and the parenthetical disclaimer discourages the model
	// from acting on it as if it were a fresh instruction.
	//
	// Moved out of the skip-analysis branch so it stays the OUTERMOST
	// prefix — the summary is scene-setting and must precede both the
	// attachment manifest and the requirement body.
	if p.SkipAnalysis && p.SourceSID == "" && p.Req.DesignContextSummary != "" {
		prompt = "## 上下文压缩摘要（之前的方案设计对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
			p.Req.DesignContextSummary + "\n\n" + prompt
	}
	// Sub-requirement: append the master-requirement context (its settled
	// description / plan / attachment manifest) plus the 「主需求关联上下文」
	// output contract. Placed here, after all three prompt branches
	// (analyst-resume / fresh skip-analysis / DesignContextSummary-prefix),
	// so the block is guaranteed to be present no matter which path built
	// `prompt` — duplicating it into three branches would guarantee drift.
	// Local and remote runs share p.Prompt, so this also covers Agent-server
	// execution without touching wizard_remote.go.
	if p.ParentBlock != "" {
		prompt += "\n\n" + p.ParentBlock
	}
	// Tail: append the kind-specific block (Issue / Idea framing). For an Idea
	// the user would normally have hidden this CTA in the frontend; we still
	// inject the block defensively so an out-of-band call (e.g. curl, the
	// wizard page, or a future "重新生成技术方案" path) sees consistent
	// guidance. For Requirement the block is empty.
	if block := promptpkg.ArchitectBlock(p.Req.Kind, p.Req); block != "" {
		prompt += "\n\n" + block
	}
	p.Prompt = prompt

	return true
}

func (h *WizardHandler) execArchitectDesign(p *designRunParams, job *store.Job, cb *runCallbacks) {
	defer func() {
		lines, status, exitCode := job.Snapshot()
		if perr := h.jobLogSvc.Save(job.ID, p.Req.ID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, job.Model); perr != nil {
			log.Printf("[architect-design] failed to persist job log %s: %v", job.ID, perr)
		}
		if cb != nil && cb.OnFinish != nil {
			cb.OnFinish(job.ID, status == store.JobDone)
		}
	}()

	// Hard-sync + worktree anchor + prompt build used to happen inside
	// prepareArchitectDesign's HTTP request thread. They now live in
	// prepareDesignWorkspace so the SSE panel can stream the hard-sync phase
	// events ("🔄 同步仓库基线…" / "📌 设计基线已锁定 …") in real time and the
	// user's HTTP connection returns with job_id immediately. On any
	// failure the job has already been Finished with JobError and
	// design_job_id has been rolled back; we just return.
	if !h.prepareDesignWorkspace(p, job) {
		return
	}

	id := p.Req.ID
	fork := p.Fork
	sourceSID := p.SourceSID
	newDesignSID := p.NewDesignSID
	workDir := p.WorkDir
	model := p.Model
	claudeConfigID := p.ClaudeConfigID
	prompt := p.Prompt
	skipAnalysis := p.SkipAnalysis
	req := p.Req

	log.Printf("[architect-design] job %s started for %s (fork=%v skip=%v agent=%q)", job.ID, id, fork, skipAnalysis && sourceSID == "", p.AgentServerID)

	// Optional knowledge pre-read: inject the project knowledge relevant to
	// this requirement and surface what was read via a "knowledge" SSE event.
	// Default off (read_knowledge=false) keeps the legacy behavior untouched.
	var kbReadTitles []string
	if p.ReadKnowledge {
		kbBlock, kbTitles := h.buildKnowledgeBlock(req.ProjectID, req.Title)
		if kbBlock != "" {
			prompt = kbBlock + "\n" + prompt
		}
		emitKnowledgeEvent(job, kbTitles)
		kbReadTitles = kbTitles
	}

	// Session threading is shared between local and remote — both branches
	// invoke claude with the same --resume / --fork-session / --session-id
	// flags, so a user who picked the same requirement's previous analyst
	// session to resume sees the same behavior regardless of execution
	// surface.
	sessionArg := sourceSID
	forkSessionID := ""
	if fork {
		forkSessionID = newDesignSID
	} else if sourceSID == "" {
		sessionArg = newDesignSID // skip-analysis fresh session
	}
	// Skills referenced via @slug in the requirement are materialized as real
	// SKILL.md files in the worktree (local branch; the remote branch SFTPs them
	// separately in runRemoteArchitectDesign) and referenced in the prompt via
	// /slug — Claude Code expands /slug to load the full body on demand. This
	// replaces the legacy full-content BuildSkillsBlock dump for the design stage.
	archSkills := h.mentionedSkills(req.Title + " " + req.Description)
	if err := llm.MaterializeSkillFiles(workDir, archSkills); err != nil {
		log.Printf("[architect-design] %s: best-effort skill materialize failed: %v", id, err)
	}
	if rb := llm.SkillRefBlock(archSkills); rb != "" {
		prompt = rb + prompt
	}
	prompt = llm.TranslateAtToSlash(prompt, llm.SlugsOf(archSkills))

	// Phase lines. A continuation is labelled differently so the log itself
	// tells the user the recovery resumed their previous session instead of
	// starting over.
	remotePhase := "📐 Agent 服务器 plan 模式下探索代码并制定技术方案..."
	localPhase := "📐 Claude 正在 plan 模式下探索代码并制定技术方案..."
	if p.Continue {
		remotePhase = "📐 Agent 服务器正在续接中断的方案设计会话，继续制定技术方案..."
		localPhase = "📐 Claude 正在续接中断的方案设计会话，继续制定技术方案..."
	}

	var out claudeStreamOutcome
	if p.AgentServerID != "" && h.agentSvrSvc != nil {
		// Remote branch — plan-mode claude on the picked Agent server.
		// runRemoteArchitectDesign owns SSH dial / git worktree / session
		// sync / worker invocation / NDJSON parse; this exec body only
		// owns the post-run persistence (UpdateDesign / UpdateArchitectModel
		// / knowledge result event), same as the local branch below.
		job.Append(store.LogLine{Type: "phase", Content: remotePhase})
		// RC-1: sourceSID 必须传「真实的来源会话」（fresh / 跳过分析路径下为空），
		// 与开发阶段 wizard_coding.go 中 runRemoteCoding 的调用点保持一致。
		// 若误传 sessionArg，fresh 运行会退化成 `--resume <刚铸的新 id>`（该会话
		// 并不存在），且 prepareRemoteAgentRun 的 `Resume: in.sourceSID != ""`
		// 会恒为 true，远端 claude 直接报 "No conversation found"。
		out = h.runRemoteArchitectDesign(&remoteArchitectInput{
			remoteRunInput: &remoteRunInput{
				job:            job,
				serverID:       p.AgentServerID,
				reqRow:         req,
				prompt:         prompt,
				workDir:        workDir,
				sourceSID:      sourceSID,
				fork:           fork,
				sessionArg:     sessionArg,
				forkSessionID:  forkSessionID,
				model:          model,
				claudeConfigID: claudeConfigID,
				usage:          h.usageCtxForConfig("architect_design", id, req.ProjectID, job.ID, model, "", "", claudeConfigID),
				PermissionMode: "plan",
				SyncMode:       p.SyncMode,
				// 「设计者上下文」：把架构师 persona（+ 附件清单）透传给远端
				// claude，与本地路径对齐。此前远端分支不设这个字段 ⇒ 远端既
				// 没有 persona 也没有附件清单（附件传上去了却从没告诉模型）。
				// 空串（关闭态）时 worker 不下发 --append-system-prompt，
				// 远端 claude 用自己的默认 system prompt —— 与本地一致。
				SystemPrompt: p.SystemPrompt,
				// Forward the user-picked base branch so the remote worker
				// anchors its worktree at origin/<base> and the hard sync
				// gate (inside prepareRemoteAgentRun's fallback chain) runs
				// against the same base the local path would.
				baseBranch: p.BaseBranch,
			},
		})
	} else {
		// Local branch — direct claude CLI invocation on the NovaWorkbench host.
		// context.Background(): the HTTP request has already returned, so we
		// must not tie the claude subprocess's lifetime to r.Context() (which
		// is cancelled the moment the handler returns).
		job.Append(store.LogLine{Type: "phase", Content: localPhase})
		cmd := h.llm.StreamCmd(context.Background(), llm.StreamOpts{
			Prompt:         prompt,
			WorkDir:        workDir,
			SystemPrompt:   p.SystemPrompt,
			Model:          cliModelArg(model),
			ClaudeConfigID: claudeConfigID,
			SessionID:      sessionArg,
			Resume:         sourceSID != "",
			Fork:           fork,
			ForkSessionID:  forkSessionID,
			PermissionMode: "plan",
		})
		out = runClaudeStream(jobSink{job}, cmd, "architect-design",
			h.usageCtxForConfig("architect_design", id, req.ProjectID, job.ID, model, "", "", claudeConfigID),
			nil,
			nil,
			0,
			architectStallTimeout)
	}

	h.finalizeArchitectRun(out, p, job, kbReadTitles, sourceSID, newDesignSID, id, model)
}

// clearUnestablishedDesignSession 回滚本次运行 pre-mint 的 design session id。
//
// 只在「本次真的铸了新 id」且「CLI 从未报告过任何 session_id（即 system/init
// 事件没到，会话根本没建立）」时清除 —— 否则下一次运行会把一个不存在的会话
// 当成可 resume 的来源，从而跳过带需求内容的提示词分支（见
// prepareArchitectDesign 里 `if skipAnalysis && sourceSID == ""`），并让远端
// --resume 永久失败。
//
// 两个早退条件的依据：
//   - p.NewDesignSID == ""：没有铸新 id，本次运行要么 resume 已有会话、要么
//     沿用 pre-mint 之外的路径，清空会误伤真实会话。
//   - out.sessionID != ""：CLI 报告过 session_id（其赋值来源是 stream 里的
//     system/init 事件，见 wizard_stream.go），说明会话确实建立了，不能清。
//   - p.Regenerate：regenerate 路径下 pre-mint 的 id 从未落入需求行（参见
//     prepareArchitectDesign 的 conditional persistence），所以「清除未
//     建立会话」无意义；相反，clear 会让旧 design_session_id（被保护的旧会话）
//     落空，用户就连 resume 微调对话的路都没了，直接 return。
//
// 注意：out.staleSession 分支有自己的清理逻辑（按 fork/design 维度清），
// 不要在这里或那里重复调用，避免语义重叠。
func (h *WizardHandler) clearUnestablishedDesignSession(p *designRunParams, out claudeStreamOutcome, id string) {
	if p.Regenerate {
		// 重新生成路径下，NewDesignSID 是「成功才落库」的新会话 id；
		// 这里清的是设计阶段 pre-mint 而未落库的悬挂 id —— 不需要做任何事。
		return
	}
	if p.NewDesignSID == "" || out.sessionID != "" {
		return
	}
	if err := h.reqSvc.UpdateDesignSession(id, ""); err != nil {
		log.Printf("[architect-design] failed to clear unestablished design session for %s: %v", id, err)
	}
}

// finalizeArchitectRun is the shared terminal-state handler for both the local
// and the remote branch of execArchitectDesign. Extracted so the
// stale-session / partial-result / success paths exist in exactly one place —
// any future tweak to error wording or persistence order lands here and
// automatically applies to both execution surfaces. Returns nothing; the
// caller already set up the deferred job-log / OnFinish hook.
//
// Sibling of the dev-stage inline terminal-state code in StartCoding (which
// can't be shared because it has to commit + push). architect-design has
// no commit / push step so a single helper covers both branches cleanly.
func (h *WizardHandler) finalizeArchitectRun(
	out claudeStreamOutcome,
	p *designRunParams,
	job *store.Job,
	kbReadTitles []string,
	sourceSID, newDesignSID, id, model string,
) {
	if out.staleSession {
		// The source conversation is gone. Clear whichever session id was
		// stale so the user can redo the prior stage, surface a recovery
		// hint, and clear the active job pointer. On the skip-analysis path
		// there is no source session to be stale, so this branch is a
		// no-op guard; we still surface a generic recovery hint.
		//
		// On the Agent-server path, prepareRemoteAgentRun stamps
		// out.SessionFileMissingSide with one of "remote" | "local" |
		// "sync-failed" so we can point the user at the actual cause
		// (file got cleaned up on the agent host / never existed locally /
		// SFTP upload itself failed) instead of a one-size-fits-all
		// "会话已过期". Mirrors the same three-way split in sub_task_runner.go
		// and wizard_orchestration.go so the wording stays consistent
		// across surfaces.
		if sourceSID == "" {
			job.Append(store.LogLine{Type: "error", Content: "会话异常，请重试生成技术方案。"})
		} else if p.Fork {
			_ = h.reqSvc.UpdateAnalysisSession(id, "")
			job.Append(store.LogLine{Type: "error", Content: "需求分析会话已过期。请重新进行「需求分析」后再生成技术方案。"})
		} else {
			// Design-session branch. Always reset design_session_id so the
			// next run mints a fresh id rather than resuming a stale one —
			// the symmetric counterpart of the pre-mint cleanup below.
			_ = h.reqSvc.UpdateDesignSession(id, "")
			switch out.SessionFileMissingSide {
			case "remote":
				job.Append(store.LogLine{Type: "error", Content: "远端 Agent Server 上未找到会话文件。请检查 Agent Server ~/.claude/projects/<slug>/ 是否被清理，或换一个 Agent Server 后重试。"})
			case "local":
				job.Append(store.LogLine{Type: "error", Content: "本地会话文件缺失，无法继续。请重新生成技术方案。"})
			case "sync-failed":
				job.Append(store.LogLine{Type: "error", Content: "会话上行同步失败（SFTP/网络）。本次运行未能在远端继续，已重置会话；请重试或检查 Agent Server 连通性。"})
			default:
				job.Append(store.LogLine{Type: "error", Content: "技术方案会话已过期。请重新生成技术方案。"})
			}
		}
		_ = h.reqSvc.UpdateDesignJob(id, "")
		job.Finish(1, store.JobError)
		return
	}

	// Persist the design session id (no-op when we already resumed an
	// existing one). The strategy depends on the regenerate flag:
	//
	//   - Non-regenerate: NewDesignSID was already stamped at run start so a
	//     mid-run crash can recover. If the CLI reported a different id
	//     (out.sessionID != newDesignSID && out.sessionID != sourceSID)
	//     correct it for safety. If neither value appeared, the session
	//     never established — drop the dangling pre-mint so the next run
	//     won't stale on first contact.
	//   - Regenerate: NewDesignSID was deliberately NOT persisted at run
	//     start — overwriting the user's old design_session_id mid-run would
	//     silently break their doc-refine chat if this run failed. So:
	//       * CLI reported an id → stamp it now (the run succeeded enough
	//         to either capture a real conversation or at least not crash,
	//         and the user wants to keep going in this new thread).
	//       * CLI reported nothing (crashed before system/init) → leave the
	//         old design_session_id alone entirely. The user keeps their
	//         doc-refine chat intact and can retry the regenerate.
	//     clearUnestablishedDesignSession above is gated by p.Regenerate
	//     for the same reason so the two paths don't contradict each other.
	if p.Regenerate {
		if out.sessionID != "" {
			if perr := h.reqSvc.UpdateDesignSession(id, out.sessionID); perr != nil {
				log.Printf("[architect-design] failed to persist regenerated design session for %s: %v", id, perr)
			}
		}
	} else if out.sessionID == "" {
		if newDesignSID != "" {
			if perr := h.reqSvc.UpdateDesignSession(id, ""); perr != nil {
				log.Printf("[architect-design] failed to clear unestablished design session for %s: %v", id, perr)
			}
		}
	} else if out.sessionID != newDesignSID && out.sessionID != sourceSID {
		if perr := h.reqSvc.UpdateDesignSession(id, out.sessionID); perr != nil {
			log.Printf("[architect-design] failed to persist design session for %s: %v", id, perr)
		}
	}

	// In plan mode, the full plan markdown is captured from the Write
	// tool_use event that lands in ~/.claude/plans/*.md (runClaudeStream
	// stores it in out.planContent; the remote branch's
	// parseStreamJSONFromReader does the same). Fall back to the result text
	// if capture missed it (e.g. a proxy that doesn't emit tool_use blocks
	// in the assistant event).
	planMarkdown := out.planContent
	if planMarkdown == "" {
		planMarkdown = out.finalResult
	}
	// The run ended with an error (upstream proxy 504, api_error result
	// event, or non-zero exit). The Write tool_use that populates
	// planContent fires BEFORE the model's final API call, so a 504 on that
	// trailing call leaves planMarkdown non-empty while the run actually
	// failed. Treating that as success appends a green ✅ and the user has
	// no idea the run was interrupted (and the captured plan may be
	// partial). Surface the error instead: save the captured plan as a
	// fallback so the exploration work isn't lost, but mark the job errored
	// so the UI shows the failure and the user can retry.
	if out.errMsg != "" {
		if planMarkdown != "" {
			if _, err := h.reqSvc.UpdateDesign(id, planMarkdown); err != nil {
				log.Printf("[architect-design] failed to save partial design for %s: %v", id, err)
			}
		}
		h.clearUnestablishedDesignSession(p, out, id)
		_ = h.reqSvc.UpdateDesignJob(id, "")
		job.Append(store.LogLine{Type: "error", Content: out.errMsg})
		job.Finish(1, store.JobError)
		return
	}
	if planMarkdown == "" {
		errMsg := out.errMsg
		if errMsg == "" {
			errMsg = "Claude 未返回结果，请重试"
		}
		h.clearUnestablishedDesignSession(p, out, id)
		job.Append(store.LogLine{Type: "error", Content: errMsg})
		_ = h.reqSvc.UpdateDesignJob(id, "")
		job.Finish(1, store.JobError)
		return
	}

	// Sub-requirement backstop for the 「主需求关联上下文」 contract. The
	// prompt asks the architect to write that section, but a prompt is a soft
	// guarantee and downstream (coding / sub-task split) reads ONLY this
	// requirement's design_docs — so a plan that skipped the section would
	// silently contradict or duplicate the master's decisions with nothing
	// downstream able to notice. Append the machine-written appendix so the
	// parent context is at least present and traceable.
	//
	// Deliberately not a re-run: a second claude invocation to "fix" the
	// omission would double the token cost of a run that mostly succeeded, and
	// the appendix already unblocks development. Users who want a properly
	// distilled section can hit refine-doc.
	//
	// Only runs that actually had a parent block reach this path; a top-level
	// requirement's plan is untouched (ParentBlock == "").
	if p.ParentBlock != "" && !promptpkg.HasParentContextSection(planMarkdown) {
		job.Append(store.LogLine{Type: "phase", Content: "⚠ 方案未包含「主需求关联上下文」章节，已自动追加附录（主需求：" + parentTitle(p.Parent) + "）"})
		if fallback := promptpkg.BuildParentContextFallback(p.Parent); fallback != "" {
			planMarkdown = planMarkdown + "\n\n" + fallback
		}
	}

	// Persist the design (sets status=designing) and clear the active job
	// pointer so a refresh shows the finished design instead of "executing".
	if _, err := h.reqSvc.UpdateDesign(id, planMarkdown); err != nil {
		log.Printf("[architect-design] failed to save design for %s: %v", id, err)
		job.Append(store.LogLine{Type: "error", Content: "保存技术方案失败: " + err.Error()})
		_ = h.reqSvc.UpdateDesignJob(id, "")
		job.Finish(1, store.JobError)
		return
	}
	_ = h.reqSvc.UpdateDesignJob(id, "")

	// Auto-promote designing → designed on the success path. The wizard
	// pipeline's gate for 「开始开发」 is `status=='designed'`, and the manual
	// HTTP path (POST /api/wizard/requirements/{id}/architect-design) used to
	// leave status stuck on 'designing' until the user clicked 「方案完成」
	// explicitly — see bug report for the "跳过分析 + 设计完成后不自动完成"
	// symptom. The immediate-design-coding path also passes through here, but
	// its OnFinish callback will idempotently re-write 'designed' (and later
	// 'developing') via UpdateStatus, so the extra write is a no-op.
	// SkipDesign is intentionally not flipped here — its current status is
	// 'draft', and the status-transition guard rejects draft→designed;
	// SkipDesign callers flow through gateCodingEntry which writes
	// 'developing' directly.
	if _, perr := h.reqSvc.UpdateStatus(id, "designed"); perr != nil {
		log.Printf("[architect-design] auto-promote %s to designed failed: %v", id, perr)
	}

	// Record the effective model + resolved Claude config for the architect
	// stage (success path only) so a refresh re-hydrates both the model and the
	// config dropdown, and the dev stage can fall back to a persisted config.
	if perr := h.reqSvc.UpdateArchitectModel(id, model); perr != nil {
		log.Printf("[architect-design] failed to persist architect_model for %s: %v", id, perr)
	}
	if perr := h.reqSvc.UpdateArchitectConfig(id, p.ClaudeConfigID); perr != nil {
		log.Printf("[architect-design] failed to persist architect_config_id for %s: %v", id, perr)
	}
	// Close the knowledge loop: mark which read entries the run actually used.
	if len(kbReadTitles) > 0 {
		items, used := evaluateKnowledgeUsage(kbReadTitles, out.toolFiles, planMarkdown)
		emitKnowledgeResultEvent(job, items, used)
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 技术方案已生成！"})
	job.Finish(0, store.JobDone)
	log.Printf("[architect-design] job %s finished for %s", job.ID, id)
}
