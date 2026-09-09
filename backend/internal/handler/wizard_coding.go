// Package handler — wizard pipeline (requirement → code).
//
// wizard_coding.go is the encoding (developer / Agent-Server) stage:
// start-coding / adjust-coding / continue-coding handlers + the shared
// runCallbacks / finishRemoteCodingJob helpers used by the scheduler
// entry points (RunScheduledCoding) and the architect stage.
// See wizard_common.go for shared helpers, wizard_stream.go for the
// CLI stream parser, wizard_remote.go for the Agent-Server transport,
// and wizard_orchestration.go for the auto-orchestrate hook called
// from execStartCoding.

package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// StartCoding creates a background job and immediately returns its ID.
// The claude CLI runs in a goroutine; progress is parsed by runClaudeStream
// (stream_event increments + init phase + tool_call labels) and written to the
// job store, so the coding panel shows live progress instead of a frozen blank
// until the turn's batched assistant event. Subscribe via
// GET /api/wizard/jobs/{id}/stream; snapshot via GET /api/wizard/jobs/{id}.
//
// codingRunParams is the named request-body shape used by StartCoding (HTTP)
// and RunScheduledCoding (scheduler). The HTTP path decodes the body into
// this struct and passes it directly; the scheduler path constructs one with
// the fields the requirement's current state requires (title/desc/branch
// defaulted). Pulling the anonymous struct to a named type is what lets the
// scheduler reuse the exec body without re-deriving any of the request-
// shape semantics.
type codingRunParams struct {
	ProjectPath      string `json:"project_path"`
	RequirementTitle string `json:"requirement_title"`
	RequirementDesc  string `json:"requirement_desc"`
	RequirementID    string `json:"requirement_id"`
	BranchName       string `json:"branch_name"`
	BaseBranch       string `json:"base_branch"`
	Model            string `json:"model"`
	// ClaudeConfigID is the user-picked claude_configs row id from the UI
	// ModelSelect picker (frontend lifts selectedConfigId alongside the model).
	// Empty = backend resolves it from the priority chain (explicit > model
	// owner > role binding > global active) inside resolveConfigIDForRun.
	ClaudeConfigID string `json:"claude_config_id"`
	ReadKnowledge  bool   `json:"read_knowledge"`
	AgentServerID  string `json:"agent_server_id"` // empty = local execution; otherwise remote Agent server
	SplitTasks     bool   `json:"split_tasks"`     // false (default) = developer persona implements directly; true = current decomposition + auto-orchestrate flow
	// DevMode picks the coding session threading strategy: "" / "session" =
	// fork the design (or analysis) session (legacy default, Claude
	// inherits the full conversation); "design" = fresh session, hand the
	// stored design doc to the agent via the -p prompt. When empty, the
	// handler falls back to the requirement row's persisted dev_mode so a
	// re-run that omits the field stays consistent with the previous run.
	DevMode string `json:"dev_mode"`
}

func (h *WizardHandler) StartCoding(w http.ResponseWriter, r *http.Request) {
	var p codingRunParams
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON")
		return
	}

	job := h.jobs.Create(p.RequirementID)
	job.SetType("start_coding")
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	go h.execStartCoding(&p, job, nil)
}

// RunScheduledCoding is the scheduler-facing entry point. It mirrors
// StartCoding: JSON-body → struct → jobs.Create → go execStartCoding, but
// the caller hands us a fully populated codingRunParams (the scheduler
// adapter fills title/desc/branch from the current requirement row) and
// the optional cb lets us flip the scheduled_tasks row when the job
// finishes. Returns the JobStore job id.
func (h *WizardHandler) RunScheduledCoding(p *codingRunParams, cb *runCallbacks) (string, error) {
	job := h.jobs.Create(p.RequirementID)
	job.SetType("start_coding")
	go h.execStartCoding(p, job, cb)
	return job.ID, nil
}

// execStartCoding is the goroutine body extracted from StartCoding. Same
// line-for-line as the original anonymous goroutine: the JSON decode +
// jobs.Create step was already synchronous, so this is a pure extraction
// (body.X → p.X everywhere). The deferred jobLogSvc.Save that the original
// already had is preserved verbatim; the cb.OnFinish hook is new and only
// fires when a scheduler callback is provided (nil for the HTTP path).
func (h *WizardHandler) execStartCoding(p *codingRunParams, job *store.Job, cb *runCallbacks) {
	defer func() {
		// Persist the finished job's full log so a backend restart doesn't
		// wipe the development record. All exit paths above call job.Finish,
		// so by the time this defer runs the snapshot is terminal. The
		// effective model is read back from job.Model (set by SetModel
		// below once roleConfig resolves it) so the defer doesn't capture a
		// `model` local that would shadow the model package.
		lines, status, exitCode := job.Snapshot()
		if perr := h.jobLogSvc.Save(job.ID, p.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, job.Model); perr != nil {
			log.Printf("[start-coding] failed to persist job log %s: %v", job.ID, perr)
		}
	}()
	// Load the requirement row up front so we can (a) detect whether the
	// source session was created in an isolated worktree, and (b) reuse it for
	// the fork resolution below without a second Get.
	var reqRow *model.Requirement
	if p.RequirementID != "" {
		if r, err := h.reqSvc.Get(p.RequirementID); err == nil {
			reqRow = r
		}
	}
	// Stamp the development-environment provenance BEFORE the run starts.
	// Doing it up front (rather than on the success path) means a failed or
	// aborted remote run still leaves a record of WHICH Agent server holds
	// the worktree — which is exactly the state where "清理开发环境" has to
	// know where to go. UpdateDevSource forces server_id back to "" for
	// local runs, so switching a requirement from remote to local execution
	// stops routing follow-ups to the old server.
	if p.RequirementID != "" {
		devSource := service.DevSourceLocal
		if p.AgentServerID != "" {
			devSource = service.DevSourceAgent
		}
		if perr := h.reqSvc.UpdateDevSource(p.RequirementID, devSource, p.AgentServerID); perr != nil {
			log.Printf("[start-coding] failed to persist dev_source for %s: %v", p.RequirementID, perr)
		} else if reqRow != nil {
			reqRow.DevSource, reqRow.AgentServerID = devSource, p.AgentServerID
			if devSource == service.DevSourceLocal {
				reqRow.AgentServerID = ""
			}
		}
		// Stamp the dev-mode provenance alongside dev_source. Empty request
		// field → fall back to the previously persisted value (so 重新开发
		// keeps the original mode) or to "session" (legacy default for rows
		// that predate the column). UpdateDevMode normalizes invalid values
		// to "" so a bad client never corrupts the column.
		devMode := p.DevMode
		if devMode == "" && reqRow != nil {
			devMode = reqRow.DevMode
		}
		if devMode == "" {
			devMode = service.DevModeSession
		}
		if perr := h.reqSvc.UpdateDevMode(p.RequirementID, devMode); perr != nil {
			log.Printf("[start-coding] failed to persist dev_mode for %s: %v", p.RequirementID, perr)
		} else if reqRow != nil {
			reqRow.DevMode = devMode
		}
	}
	// hadWorktree records whether the upstream stage had already persisted a
	// worktree before THIS coding run — false means the design/analysis session
	// we're about to fork was created in-place (un-isolated).
	hadWorktree := reqRow != nil && reqRow.WorktreePath != ""

	// Recover the project directory if a Docker rebuild / fresh workspace
	// mount left it absent. Without this, EnsureWorktree below returns
	// ErrNotAGitRepo and the in-place checkout fails with the user-facing
	// "git checkout 失败" error. Re-clone uses the project's stored
	// remote_url + platform token; when there is no remote to restore from
	// EnsureCloned returns a clear error naming the missing path.
	if reqRow != nil {
		if cerr := h.projectSvc.EnsureCloned(reqRow.ProjectID); cerr != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + cerr.Error()})
			job.Finish(1, store.JobError)
			return
		}
	}
	// Defensive guard: an Idea must not reach the coding stage. The
	// frontend hides the "🚀 开始开发" CTA when kind=idea and only Idea
	// rows promoted via "📋 转为需求" can move on, but a stray API call
	// (curl, the legacy /wizard quick-start path, or a future caller) must
	// not silently start coding on a not-yet-defined requirement. Reject
	// with a clear message before any claude subprocess is spawned.
	if reqRow != nil && reqRow.Kind == "idea" {
		job.Append(store.LogLine{Type: "error", Content: "❌ 「想法」类需求暂不支持进入开发阶段，请先在详情页点击「📋 转为需求」升级。"})
		job.Finish(1, store.JobError)
		return
	}

	// Resolve the working directory for coding. When a branch is requested
	// AND the project is a git repo with a requirement id to key on, develop
	// in an isolated git worktree per requirement so parallel requirements
	// don't stomp each other's checkout or edit the same files. The legacy
	// path (no branch, non-git project, or no requirement_id) falls back to
	// coding directly in the project directory as before.
	workDir := p.ProjectPath
	branchDir := p.ProjectPath // where git checkout/pull run
	useWorktree := false
	baseBranch := p.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	if p.BranchName != "" && p.RequirementID != "" {
		wtPath, wtErr := EnsureWorktree(p.ProjectPath, p.RequirementID, p.BranchName, baseBranch)
		switch {
		case wtErr == nil && wtPath != "":
			workDir = wtPath
			branchDir = wtPath
			useWorktree = true
			job.Append(store.LogLine{Type: "message", Content: "🌿 已创建/复用隔离 worktree: " + wtPath})
		case errors.Is(wtErr, ErrNotAGitRepo):
			// Fall through to the legacy in-place checkout below.
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ 非 git 仓库，在项目目录直接开发"})
		default:
			job.Append(store.LogLine{Type: "error", Content: "❌ 创建 worktree 失败: " + wtErr.Error()})
			job.Finish(1, store.JobError)
			return
		}
	}

	// Checkout the development branch before coding. Skipped for worktrees
	// (`git worktree add` already checked the branch out) and skipped for
	// non-git repos (nothing to check out — proceed in place rather than
	// failing with the misleading "git checkout 失败"). For git repos we
	// mirror EnsureWorktree's robust strategy: create off the base, switch
	// to an already-existing branch, or branch off HEAD so a missing base
	// ref never produces a cryptic error.
	if p.BranchName != "" && !useWorktree {
		if _, gerr := gitRun(branchDir, "rev-parse", "--is-inside-work-tree"); gerr != nil {
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ 非 git 仓库，跳过分支切换，在项目目录直接开发"})
		} else {
			checkoutOK := false
			var lastErrOut string
			attempt := func(args ...string) (string, bool) {
				c := exec.Command("git", args...)
				c.Dir = branchDir
				out, err := c.CombinedOutput()
				trimmed := strings.TrimSpace(string(out))
				if err == nil {
					return trimmed, true
				}
				lastErrOut = trimmed
				return "", false
			}
			// 1. Create the branch off the requested base (typical happy path).
			if out, ok := attempt("checkout", "-b", p.BranchName, baseBranch); ok {
				job.Append(store.LogLine{Type: "message", Content: "🌿 " + out})
				checkoutOK = true
			} else if _, ok := attempt("checkout", p.BranchName); ok {
				// 2. Branch already exists locally — switch to it.
				job.Append(store.LogLine{Type: "message", Content: "🌿 切换到已有分支: " + p.BranchName})
				checkoutOK = true
			} else if out, ok := attempt("checkout", "-b", p.BranchName); ok {
				// 3. Base ref missing locally — fall back to branching off HEAD
				//    (matches EnsureWorktree's strategy).
				job.Append(store.LogLine{Type: "message", Content: "🌿 " + out})
				checkoutOK = true
			}
			if !checkoutOK {
				job.Append(store.LogLine{Type: "error", Content: "❌ git checkout 失败: " + lastErrOut})
				job.Finish(1, store.JobError)
				return
			}
		}
	}

	// Best-effort pull: try to update the dev branch from its upstream so
	// coding starts from the remote HEAD. This must NOT abort the coding
	// job — the repo may have no remote at all, or the branch may have no
	// upstream tracking info, in which case there is simply nothing to
	// pull and we proceed on the already-checked-out branch.
	if p.BranchName != "" {
		pullCmd := exec.Command("git", "pull", "--ff-only")
		pullCmd.Dir = branchDir
		pullOut, pullErr := pullCmd.CombinedOutput()
		if pullErr != nil {
			// No upstream on the current branch — retry against origin/<base>
			// if a remote exists. Missing remote / diverged history just means
			// "nothing to pull"; log it and keep going.
			fallbackCmd := exec.Command("git", "pull", "--ff-only", "origin", baseBranch)
			fallbackCmd.Dir = branchDir
			fbOut, fbErr := fallbackCmd.CombinedOutput()
			if fbErr != nil {
				job.Append(store.LogLine{Type: "message", Content: "ℹ️ 跳过 git pull（无远程跟踪或已分叉），继续在当前分支开发: " + strings.TrimSpace(string(append(pullOut, fbOut...)))})
			} else {
				job.Append(store.LogLine{Type: "message", Content: "⬇️ " + strings.TrimSpace(string(fbOut))})
			}
		} else {
			job.Append(store.LogLine{Type: "message", Content: "⬇️ " + strings.TrimSpace(string(pullOut))})
		}
	}

	// Persist the worktree location + branch so adjust-coding and the merge
	// step can find the isolated working tree (jobs are in-memory; the path
	// must live in the DB to survive a restart).
	if useWorktree && p.RequirementID != "" {
		if perr := h.reqSvc.UpdateWorktree(p.RequirementID, p.BranchName, workDir); perr != nil {
			log.Printf("[start-coding] failed to persist worktree for %s: %v", p.RequirementID, perr)
		}
	}

	// Session threading: the developer stage forks off the design session
	// (--resume <design_sid> --fork-session) so the developer inherits the
	// full analysis+design discussion and swaps in the developer persona; the
	// forked session gets a new id we read from the stream and persist as
	// coding_session_id. We do NOT re-feed the requirement desc / design JSON
	// — the resumed conversation already has them.
	//
	// "重新开发"语义: when a coding_session_id already exists (a prior
	// coding pass ran) we STILL fork off the design session instead of
	// --resume'ing the prior coding session. Forking mints a NEW session that
	// inherits only the requirement+design conversation, so leftover tool_use
	// / half-written code / mid-run errors from the last coding pass cannot
	// pollute the new round. The forked id overwrites coding_session_id
	// below (fork && out.sessionID != "" guard), realizing "重新开发 = 新会话".
	// First-ever coding (coding_session_id == "") takes the same path, so
	// behavior is unchanged for the genuine first pass.
	//
	// "基于方案开发" (dev_mode == "design"): deliberately ignore the design
	// / analysis / coding session chain. The new session starts fresh, and
	// the stored design doc is fed to the agent via the -p prompt further
	// down (see designDoc block). This is what the user picked when they
	// want a clean slate that still has the plan in hand.
	//
	// Graceful fallback: the legacy /wizard quick-start page has no
	// Requirement row (no requirement_id / session ids), so it can't join
	// the session chain. When no source session exists we fall back to a
	// fresh session and feed the full desc — i.e. the pre-chain behavior —
	// so that path keeps working.
	sourceSID := ""
	fork := false
	if reqRow != nil && reqRow.DevMode != service.DevModeDesign {
		if reqRow.DesignSessionID != "" {
			// 重新开发 / 首次开发: fork from the design session so the new
			// coding session carries only requirement+design, never the prior
			// coding pass's history.
			sourceSID = reqRow.DesignSessionID
			fork = true
		} else if reqRow.AnalysisSessionID != "" {
			// No design session yet but an analyst session exists — keep
			// forking off the analyst session (skip-analysis-first path).
			sourceSID = reqRow.AnalysisSessionID
			fork = true
		} else if reqRow.CodingSessionID != "" && !reqRow.SkipDesign {
			// Legacy fallback: no design / analysis session at all AND not a
			// skip-design ("直接开发") row. Resume the prior coding session
			// rather than losing threading for old data rows that predate
			// session chaining. skip-design rows fall through to the fresh
			// path below so "重新开发" mints a brand-new session instead of
			// resuming the prior one.
			sourceSID = reqRow.CodingSessionID
		}
	}

	// Pre-mint + persist the coding session id BEFORE spawning claude so it
	// survives a mid-run restart. This happens for BOTH a fork off the
	// design/analysis session (--resume <src> --fork-session --session-id
	// <new>) AND a fresh session (--session-id <new>) — the latter covers
	// skip-design "直接开发" (no analysis/design session to fork, but a
	// requirement row exists), so an interrupted direct-development run can
	// still be resumed via 继续开发 instead of redoing everything. Only the
	// legacy quick-start path (no requirement row) keeps a CLI-generated id
	// and never persists it.
	newCodingSID := ""
	if p.RequirementID != "" && (fork || sourceSID == "") {
		newCodingSID = util.NewUUID()
		if perr := h.reqSvc.UpdateCodingSession(p.RequirementID, newCodingSID); perr != nil {
			log.Printf("[start-coding] failed to persist coding session for %s: %v", p.RequirementID, perr)
		}
	}

	// Forking a source session created in-place (no persisted worktree)
	// leaks its original-dir absolute paths into the coding session, so
	// Claude edits the shared checkout instead of the worktree. Guard only
	// when we actually established a worktree this run (useWorktree=true),
	// which also excludes non-git projects (legacy in-place coding).
	if fork && useWorktree && !hadWorktree {
		job.Append(store.LogLine{Type: "error", Content: "❌ 上游会话未在隔离 worktree 中生成，请重新执行「生成技术方案」后再开始开发。"})
		job.Finish(1, store.JobError)
		return
	}

	// Role selection:
	//   • "agent" — Agent-Server execution (remote) OR local with split_tasks=false
	//     (the user explicitly chose "不拆分任务"). The agent persona's system
	//     prompt says "不要先拆分子任务" + "一次会话内完成端到端开发", which is
	//     exactly the behavior we want when the main agent is supposed to
	//     implement the requirement itself.
	//   • "developer" — local execution with split_tasks=true (default = true
	//     for legacy / no-split-switch callers). The developer persona is the
	//     统筹协调者 that decomposes into sub-tasks + emits [SUBTASKS_READY]
	//     so tryAutoOrchestrate dispatches children.
	//
	// Why not "developer" + a "直接实现" -p override when split_tasks=false?
	// The developer system prompt explicitly says "**不要直接编写项目代码**——
	// 所有具体实现工作由子Agent完成" and has the "## 何时拆分任务" block keyed
	// on "进入执行实现阶段"-style triggers; even with the prompt rewritten to
	// "直接实现需求", models reflexively follow the system prompt and split
	// anyway, defeating the user's "不拆分" choice. Routing through the agent
	// role is the only reliable fix: its persona + system prompt consistently
	// say "don't decompose, implement end-to-end" (see role_defaults.go).
	roleKey := "developer"
	if p.AgentServerID != "" || !p.SplitTasks {
		roleKey = "agent"
	}
	systemPrompt, model, claudeConfigID := h.roleConfig(roleKey)
	// Per-request model override (highest precedence); empty means role default.
	if p.Model != "" {
		model = p.Model
	}
	// Align the gateway config with the picked model: when the user picked a
	// model from a non-active config (or explicitly named a config), prefer
	// those over the role's binding so ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN
	// match ANTHROPIC_MODEL. See resolveConfigIDForRun for the priority chain.
	claudeConfigID = h.resolveConfigIDForRun(p.ClaudeConfigID, model, claudeConfigID)
	job.SetModel(model)
	var prompt string
	if sourceSID == "" {
		// Fresh-session path: no design/analysis session to fork (skip-design
		// "直接开发" rows, "基于方案开发" mode, or legacy rows without session
		// chaining). This branch used to feed a bare "## title\n\n desc" which
		// is a generic "请实现该需求" prompt — and the developer role's system
		// prompt only emits the [SUBTASKS_READY] sentinel when the -p message
		// carries an explicit "开始开发/进入执行实现阶段" trigger. Without that
		// trigger the agent did the work itself and auto-orchestration never
		// fired (see req_04acb22d06fe3525). So we now send the SAME
		// decomposition trigger as the fork branch — the only difference is
		// wording: there is no "已完成的需求分析与技术方案" to reference, so we
		// ask the agent to read the relevant files first to build context.
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ 未关联需求会话，使用独立会话开始开发。"})
		// "基于方案开发" (dev_mode == "design") hand-feeds the stored design
		// doc to the agent in the -p prompt so the new session has the plan
		// even though it never joined the design/analysis conversation.
		// designMarkdown is sourced from reqRow.DesignDocs (raw — plan Markdown
		// or legacy JSON, both formats are appended verbatim and the
		// leadIn tells the agent to treat it as the implementation plan).
		designMarkdown := ""
		if reqRow != nil && reqRow.DevMode == service.DevModeDesign {
			designMarkdown = strings.TrimSpace(reqRow.DesignDocs)
		}
		if roleKey == "agent" {
			// agent persona (Agent-Server remote OR local split_tasks=false):
			// implement the requirement directly end-to-end. No decomposition
			// trigger in the -p message, no [SUBTASKS_READY] expected, no
			// .novaworkbench/subtasks.json Write. The agent system prompt says
			// "不要先拆分子任务" and "一次会话内完成端到端开发", so the model
			// consistently implements instead of splitting.
			leadIn := "请先读取项目中的相关文件理解现有代码结构与需求上下文，然后直接实现需求：\n"
			if designMarkdown != "" {
				leadIn = "用户选择「基于方案开发」：不会接续原方案会话，而是把下面的方案作为唯一依据创建新会话直接实现。\n" +
					"请先读取项目中的相关文件理解现有代码结构，再依据方案直接实现：\n"
			}
			prompt = agentDirectPrompt(p.RequirementTitle, leadIn, workDir)
		} else {
			// developer persona + fresh-session path + split_tasks=true.
			// Keep the original decomposition trigger so the developer role
			// emits [SUBTASKS_READY] + writes .novaworkbench/subtasks.json
			// and tryAutoOrchestrate dispatches children. The split_tasks=false
			// fresh-session case is handled by the roleKey=="agent" branch
			// above (we route those requests through the agent role entirely).
			leadIn := "请先读取项目中的相关文件理解现有代码结构与需求上下文，然后立即完成**任务拆分**：\n"
			if designMarkdown != "" {
				leadIn = "用户选择「基于方案开发」：不会接续原方案会话，而是把下面的方案作为唯一依据创建新会话。\n" +
					"请先读取项目中的相关文件理解现有代码结构，然后依据方案立即完成**任务拆分**：\n"
			}
			prompt = developerDecomposePrompt(p.RequirementTitle, leadIn, workDir)
		}
		// Append the stored design doc to the prompt when dev_mode is
		// "design". Both plan Markdown and legacy JSON formats are passed
		// verbatim — the agent's tools will surface them. The block goes
		// AFTER the leadIn so the developer persona's "开始开发/进入执行实
		// 现阶段" trigger (preserved inside developerDecomposePrompt) stays
		// at the top and the [SUBTASKS_READY] orchestration path still fires.
		if designMarkdown != "" {
			prompt += "\n\n## 技术方案（来自 requirements.design_docs，原方案会话的最终产物）\n\n" + designMarkdown
		}
		if desc := strings.TrimSpace(p.RequirementDesc); desc != "" {
			if roleKey == "agent" {
				prompt += "\n\n用户在开发前的追加说明：\n" + desc
			} else {
				prompt += "\n\n用户在开发前的追加说明：\n" + desc
			}
		}
		// Context-compression handoff (legacy fresh-session path): when
		// the coding stage was previously compressed we still want the
		// summary prepended so the new coding session inherits the
		// compressed history. This branch covers pre-existing rows that
		// never had a design_session_id (and the rare user who hits
		// "重新开发" after a design session was already compressed and
		// invalidated). The summary goes at the TOP so the developer
		// treats it as ground truth; the parenthetical tells the model
		// not to act on it as if it were a fresh instruction.
		if reqRow != nil && reqRow.CodingContextSummary != "" {
			prompt = "## 上下文压缩摘要（之前的开发对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
				reqRow.CodingContextSummary + "\n\n" + prompt
		}
	} else {
		// The resumed conversation carries the requirement, analysis, and
		// design. The developer role is now a coordinator (统筹协调者) that
		// does NOT write code itself — it decomposes the work into sub-tasks
		// and emits a [SUBTASKS_READY] sentinel so tryAutoOrchestrate (called
		// at the end of this job) dispatches each child agent. The -p prompt
		// MUST carry the explicit "进入执行实现阶段" trigger that the
		// developer system prompt keys its JSON+sentinel emission on — a
		// generic "请实现该需求" does NOT hit that branch, so the agent hedges
		// into a prose plan + "等待确认" and auto-orchestration never fires.
		// The per-turn -p instruction also overrides the system prompt's
		// "always ask for confirmation" guidance so the agent emits the
		// sentinel immediately instead of waiting on the user.
		if roleKey == "agent" {
			// agent persona (Agent-Server remote OR local split_tasks=false):
			// fork/resume variant — the conversation already carries the
			// requirement + analysis + design, so the leadIn references that
			// history instead of asking the agent to re-read files.
			prompt = agentDirectPrompt(p.RequirementTitle,
				"基于已完成的需求分析与技术方案，请直接实现需求：\n", workDir)
		} else {
			// developer persona + fork/resume path + split_tasks=true.
			// Decompose the work into sub-tasks so tryAutoOrchestrate (called
			// below, gated on splitTasks) can dispatch children. The
			// split_tasks=false fork/resume case is handled by roleKey=="agent"
			// above — we route those requests through the agent role entirely.
			prompt = developerDecomposePrompt(p.RequirementTitle,
				"基于已完成的需求分析与技术方案，请立即完成**任务拆分**：\n", workDir)
		}
		if desc := strings.TrimSpace(p.RequirementDesc); desc != "" {
			if roleKey == "agent" {
				prompt += "\n\n用户在开发前的追加调整说明：\n" + desc
			} else {
				prompt += "\n\n用户在开发前的追加调整说明：\n" + desc
			}
		}
		// Coding-stage compression handoff: even when resuming the design
		// session, the prior coding turns may have been compressed. The
		// summary goes at the TOP so the developer treats it as ground
		// truth; the parenthetical tells the model not to act on it as if
		// it were a fresh instruction.
		if reqRow != nil && reqRow.CodingContextSummary != "" {
			prompt = "## 上下文压缩摘要（之前的开发对话已被压缩，请基于此继续工作，不要当作新指令）\n" +
				reqRow.CodingContextSummary + "\n\n" + prompt
		}
	}
	// Kind-specific developer tail (currently only fires for kind=issue).
	// Idea never reaches here — the frontend hides the "开始开发" CTA and
	// we double-protect by checking reqRow.Kind below.
	if reqRow != nil {
		if block := promptpkg.DeveloperBlock(reqRow.Kind, reqRow); block != "" {
			prompt += "\n\n" + block
		}
	}

	// Optional knowledge pre-read: inject the project knowledge relevant to
	// this requirement and surface what was read via a "knowledge" SSE event.
	// Default off (read_knowledge=false) keeps the legacy behavior untouched.
	var kbReadTitles []string
	if p.ReadKnowledge {
		codingProjID := ""
		if reqRow != nil {
			codingProjID = reqRow.ProjectID
		}
		kbBlock, kbTitles := h.buildKnowledgeBlock(codingProjID, p.RequirementTitle)
		if kbBlock != "" {
			prompt = kbBlock + "\n" + prompt
		}
		emitKnowledgeEvent(job, kbTitles)
		kbReadTitles = kbTitles
	}
	sessionArg := sourceSID
	forkSessionID := ""
	if fork {
		forkSessionID = newCodingSID
	} else if sourceSID == "" {
		sessionArg = newCodingSID // fresh (skip-design 直接开发): --session-id <new>
	}
	skillText := ""
	if reqRow != nil {
		skillText = reqRow.Title + " " + reqRow.Description
	}
	if block := llm.BuildSkillsBlock(h.mentionedSkills(skillText)); block != "" {
		prompt = block + prompt
	}
	// Inject the project's git committer identity (from its platform
	// token) as GIT_AUTHOR_*/GIT_COMMITTER_* env into the claude
	// subprocess. git reads these env vars over any config, so when the
	// developer role runs `git commit` via its Bash tool it carries a
	// real identity on hosts without ~/.gitconfig (e.g. the Docker
	// container). Empty on miss → no injection, git falls back to its
	// own config lookup (preserves dev-machine behaviour). Mirrors
	// MergeHandler.gitIdentityForReq via the shared lookupGitIdentity.
	var codingExtraEnv []string
	if name, email := lookupGitIdentity(h.projectSvc, h.platformSvc, reqRow); name != "" || email != "" {
		if name != "" {
			codingExtraEnv = append(codingExtraEnv, "GIT_AUTHOR_NAME="+name, "GIT_COMMITTER_NAME="+name)
		}
		if email != "" {
			codingExtraEnv = append(codingExtraEnv, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL="+email)
		}
	}
	cmd, cancel := h.llm.GenerateCode(llm.StreamOpts{
		Prompt:         prompt,
		WorkDir:        workDir,
		SystemPrompt:   systemPrompt,
		Model:          cliModelArg(model),
		ClaudeConfigID: claudeConfigID,
		SessionID:      sessionArg,
		Resume:         sourceSID != "",
		Fork:           fork,
		ForkSessionID:  forkSessionID,
		ExtraEnv:       codingExtraEnv,
	})
	// Release the 30-minute context timer the moment runClaudeStream
	// finishes (cmd.Wait returns) — the goroutine may then stay alive
	// for tryAutoOrchestrate + log emission, but the claude subprocess
	// itself is already done.
	defer cancel()
	codingProjectID := ""
	if reqRow != nil {
		codingProjectID = reqRow.ProjectID
	}
	codingUsage := h.usageCtxFor("coding", p.RequirementID, codingProjectID, job.ID, model, "", "")

	// Remote Agent-server branch: SSHs into the target, syncs the claude
	// session dir so --resume works, executes the same claude flag list on
	// the remote worktree, parses the stream-json output, and pushes the
	// code back to origin. The job log + final result semantics match the
	// local path so the frontend doesn't have to special-case anything.
	if p.AgentServerID != "" && h.agentSvrSvc != nil {
		out := h.runRemoteCoding(&remoteCodingInput{
			job:      job,
			serverID: p.AgentServerID,
			req: startCodingReq{
				ProjectPath:      p.ProjectPath,
				RequirementTitle: p.RequirementTitle,
				RequirementDesc:  p.RequirementDesc,
				RequirementID:    p.RequirementID,
				BranchName:       p.BranchName,
				BaseBranch:       p.BaseBranch,
				Model:            p.Model,
				ReadKnowledge:    p.ReadKnowledge,
				AgentServerID:    p.AgentServerID,
			},
			reqRow:         reqRow,
			prompt:         prompt,
			workDir:        workDir,
			sourceSID:      sourceSID,
			fork:           fork,
			sessionArg:     sessionArg,
			forkSessionID:  forkSessionID,
			model:          model,
			claudeConfigID: claudeConfigID,
			usage:          codingUsage,
		})
		if out.staleSession {
			job.Append(store.LogLine{Type: "error", Content: "❌ 源会话已失效，请重新发起对应阶段后再开发。"})
			job.Finish(1, store.JobError)
			return
		}
		if out.errMsg != "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult == "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		job.Append(store.LogLine{Type: "done", Content: "✅ 开发完成！"})
		if p.RequirementID != "" {
			if perr := h.reqSvc.UpdateDeveloperModel(p.RequirementID, model); perr != nil {
				log.Printf("[start-coding] failed to persist developer_model for %s: %v", p.RequirementID, perr)
			}
			// dev_source / agent_server_id were stamped up front by the
			// execStartCoding prologue (see line ~1246) — no need to re-write on
			// the success path. The prologue fires before the remote job starts
			// so the binding is correct even if the run aborts.
		}
		job.Finish(0, store.JobDone)
		log.Printf("[start-coding] remote job %s finished status=%s exit=%d", job.ID, job.Status, job.ExitCode)
		return
	}

	out := runClaudeStream(jobSink{job}, cmd, "start-coding", codingUsage)

	// The coding session id is already persisted upfront. Correct it only if
	// the CLI reported a different id than the one we pre-minted (a safety
	// net in case the --session-id override semantics ever change).
	if newCodingSID != "" && out.sessionID != "" && out.sessionID != newCodingSID {
		if perr := h.reqSvc.UpdateCodingSession(p.RequirementID, out.sessionID); perr != nil {
			log.Printf("[start-coding] Failed to persist coding session for %s: %v", p.RequirementID, perr)
		}
	}

	if out.staleSession {
		job.Append(store.LogLine{Type: "error", Content: "❌ 源会话已失效，请重新发起对应阶段后再开发。"})
		job.Finish(1, store.JobError)
		return
	}
	if out.errMsg != "" {
		job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
		job.Finish(1, store.JobError)
		return
	}
	if out.finalResult == "" {
		job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
		job.Finish(1, store.JobError)
		return
	}
	job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
	// Close the knowledge loop: mark which read entries the run actually used.
	if len(kbReadTitles) > 0 {
		items, used := evaluateKnowledgeUsage(kbReadTitles, out.toolFiles, out.finalResult)
		emitKnowledgeResultEvent(job, items, used)
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 开发完成！"})
	// Capture the main agent's task breakdown (if any) into coding_plan
	// so SubTaskPanel can show it as the suggested sub-task list. The
	// helper accepts both sentinel-wrapped and "## 任务分解"-heading
	// forms; empty result leaves coding_plan unchanged (we don't want
	// to wipe a previously-persisted plan when the agent happens to
	// re-run without emitting one).
	if p.RequirementID != "" {
		if plan := extractCodingPlan(out.finalResult); plan != "" {
			if perr := h.reqSvc.UpdateCodingPlan(p.RequirementID, plan); perr != nil {
				log.Printf("[start-coding] failed to persist coding_plan for %s: %v", p.RequirementID, perr)
			} else {
				job.Append(store.LogLine{Type: "message", Content: "📋 已捕获主Agent任务分解，存入 coding_plan"})
			}
		}
	}
	// Record the effective developer model (success path only).
	if p.RequirementID != "" {
		if perr := h.reqSvc.UpdateDeveloperModel(p.RequirementID, model); perr != nil {
			log.Printf("[start-coding] failed to persist developer_model for %s: %v", p.RequirementID, perr)
		}
		// dev_source / agent_server_id were stamped up front by the
		// execStartCoding prologue (line ~1246) before the claude subprocess
		// was spawned. Local runs also flow through the prologue (with
		// AgentServerID=""), which routes dev_source back to "local" so a
		// previously-remote requirement doesn't keep pointing at the stale
		// server after a local re-run.
	}
	// Cache the Claude CLI slug for this project on first successful local
	// start-coding so future Agent Server runs can map local session files
	// to the remote cwd's slug. Only runs locally: the Agent Server branch
	// never creates local jsonl files (they live on the remote host under
	// a deterministic slug derived from wtPath).
	if p.AgentServerID == "" && p.RequirementID != "" && reqRow != nil && reqRow.ProjectID != "" {
		if proj, perr := h.projectSvc.Get(reqRow.ProjectID); perr == nil && proj != nil {
			if _, derr := h.projectSvc.DiscoverAndCacheClaudeProjectSlug(proj.ID, proj.LocalPath); derr != nil {
				log.Printf("[start-coding] failed to cache claude_project_slug for %s: %v", proj.ID, derr)
			}
		}
	}
	job.Finish(0, store.JobDone)
	log.Printf("[start-coding] job %s finished status=%s exit=%d", job.ID, job.Status, job.ExitCode)

	// === AUTO-ORCHESTRATE ============================================
	// 主 Agent 在 start-coding 阶段已经掌握需求 / 设计 / 项目上下文。
	// 用户希望"一键编排 = 主 Agent 自动派发"——main agent 一返回
	// finalResult，立刻交给 tryAutoOrchestrate：有 [SUBTASKS_READY]
	// sentinel + JSON 时串行派发子 Agent + 异步汇总；没命中就把
	// coding_plan 当作普通任务分解展示，但不派发（保持现有行为）。
	//
	// Agent-Server 路径走 "agent" 角色，该角色的 system prompt 与 -p 指令
	// 都不要求 [SUBTASKS_READY] 哨兵 / subtasks.json；为了一致性直接跳过
	// orchestrator（不调用，即便没有 sentinel 也会安全 no-op，但调用
	// 本身会引入无谓的 goroutine + 日志噪音）。
	//
	// 当 split_tasks=false（用户选择"不拆分任务"）时，StartCoding 已经把
	// roleKey 切到 "agent" 并使用 agentDirectPrompt，-p 消息不携带任何触发
	// 语、agent 的 system prompt 也明确"不要拆分子任务"；此时再调
	// tryAutoOrchestrate 只会扫到空 payload 然后空转派发 0 个子任务，
	// 等价于一次 no-op，但仍然多开一个 goroutine + 一段 resolveSubtasksPayload
	// 的日志噪音，所以一并短路。split_tasks=true + 本地 = roleKey=="developer"，
	// 走原 developerDecomposePrompt + 派发链路，行为与改动前一致。
	//
	// 该调用改用独立 goroutine，不阻塞 start-coding 自身的 job_done
	// 信号，用户的开发启动 SSE 立即结束；子任务的进度仍由
	// dispatchOneChild 的 JobStore job 推流。
	if roleKey != "agent" && p.RequirementID != "" && newCodingSID != "" && h.subTaskSvc != nil && p.SplitTasks {
		go h.tryAutoOrchestrate(p.RequirementID, newCodingSID, out.finalResult, out.subTasksJSON, reqRow, workDir, model, claudeConfigID)
	}
}

// AdjustCoding starts a background JobStore job that resumes the prior coding
// session (--resume coding_session_id) to apply a follow-up adjustment to
// already-implemented code. Because the resumed session already carries the
// requirement, analysis, design, and the persona set by StartCoding
// (developer for local execution; agent for Agent-Server execution), we send
// ONLY the user's follow-up message as -p and inject NEITHER the role system
// prompt NOR the readProjectContext project context — re-feeding them would be
// redundant and could distort the resumed conversation. The model field is
// honored (--model) so the user's latest setting applies; we keep the lookup
// against the developer role for backward compat (the resumed session's
// persona is what determines behaviour — model only affects token routing).
//
// Only requirements with status in {"done","developing"} and a non-empty
// coding_session_id may adjust (developing = first coding pass just finished;
// done = user marked complete). The job_done handler does NOT change status and
// does NOT update coding_session_id — every adjust round resumes the SAME
// original coding session. Stale --resume (session file gone) surfaces a clear
// error instead of silently starting a fresh session.
//
// POST /api/wizard/adjust-coding { requirement_id, message } -> { job_id }
func (h *WizardHandler) AdjustCoding(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequirementID string `json:"requirement_id"`
		Message       string `json:"message"`
		Model         string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[adjust-coding] JSON decode error: %v", err)
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeError(w, 400, "INVALID", "message 不能为空")
		return
	}

	req, err := h.reqSvc.Get(body.RequirementID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "requirement not found")
		return
	}
	if req.Status != "done" && req.Status != "developing" {
		writeError(w, 409, "INVALID_STATUS", "仅开发完成或开发中的需求可追加调整（当前状态: "+req.Status+"）")
		return
	}
	if req.CodingSessionID == "" {
		writeError(w, 409, "NO_SESSION", "无 coding session，无法 resume，请重新发起 coding")
		return
	}

	proj, err := h.projectSvc.Get(req.ProjectID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "project not found")
		return
	}

	// Only the developer role's MODEL is honored (so the user's latest model
	// setting applies to follow-up turns). The system prompt is deliberately
	// omitted: the resumed coding session already carries the developer
	// persona, and re-injecting --system-prompt would replace it.
	_, model, claudeConfigID := h.roleConfig("developer")
	// Per-request model override (highest precedence); empty means role default.
	if body.Model != "" {
		model = body.Model
	}

	job := h.jobs.Create(body.RequirementID)
	job.SetType("adjust_coding")
	job.SetModel(model)
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	go func() {
		defer func() {
			// Persist the finished job's full log so a backend restart doesn't
			// wipe the adjustment record (same durability pattern as StartCoding).
			lines, status, exitCode := job.Snapshot()
			if perr := h.jobLogSvc.Save(job.ID, body.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, model); perr != nil {
				log.Printf("[adjust-coding] failed to persist job log %s: %v", job.ID, perr)
			}
		}()
		log.Printf("[adjust-coding] job %s started for %s (resume %s)", job.ID, body.RequirementID, req.CodingSessionID)
		job.Append(store.LogLine{Type: "phase", Content: "🤖 Claude 正在续接开发会话，处理追加调整..."})

		// Echo the user request so the job-stream SSE can render a "👤 调整请求"
		// bubble before the first tool/phase line. The same text is persisted
		// into token_usage.meta.summary for the project/requirement token table.
		if msg := strings.TrimSpace(body.Message); msg != "" {
			job.Append(store.LogLine{Type: "user_input", Content: msg})
		}

		// GenerateCode wraps StreamCmd with a long (>=30m) timeout — a real
		// code edit can take minutes. SystemPrompt is left empty so the resumed
		// session's persona is preserved; Prompt carries ONLY the user's
		// follow-up message (no readProjectContext project context).
		//
		// WorkDir: prefer the requirement's isolated worktree (so follow-up
		// edits land in the same worktree as the first coding pass, keeping
		// parallel requirements isolated); fall back to the project checkout
		// for legacy requirements without a worktree.
		workDir := proj.LocalPath
		if req.WorktreePath != "" {
			if _, statErr := os.Stat(req.WorktreePath); statErr == nil {
				workDir = req.WorktreePath
			}
		}
		adjustPrompt := body.Message
		if block := llm.BuildSkillsBlock(h.mentionedSkills(req.Title + " " + req.Description + " " + body.Message)); block != "" {
			adjustPrompt = block + body.Message
		}
		// Tail the kind-specific developer block so a resumed Issue session
		// stays anchored to "最小改动、修复根因" framing on every follow-up
		// turn. For requirement rows it's a no-op.
		if block := promptpkg.DeveloperBlock(req.Kind, req); block != "" {
			adjustPrompt += "\n\n" + block
		}
		// Execution-consistency: a requirement that was coded on an Agent
		// server has its working tree on THAT host, not here — the local
		// worktree either doesn't exist or lags behind origin. Route the
		// follow-up turn back to the same server so the adjustment applies to
		// the real code (and gets committed + pushed from there). Falls
		// through to local execution when the requirement was developed
		// locally, or when the agent-server service isn't wired.
		if req.AgentServerID != "" && h.agentSvrSvc != nil {
			out := h.runRemoteCoding(&remoteCodingInput{
				job:      job,
				serverID: req.AgentServerID,
				req: startCodingReq{
					RequirementTitle: req.Title,
					RequirementDesc:  body.Message,
					RequirementID:    req.ID,
					BranchName:       req.BranchName,
					AgentServerID:    req.AgentServerID,
				},
				reqRow:     req,
				prompt:     adjustPrompt,
				sourceSID:  req.CodingSessionID,
				sessionArg: req.CodingSessionID,
				model:      model,
				usage:      h.usageCtxFor("adjust_coding", body.RequirementID, req.ProjectID, job.ID, model, "", body.Message),
			})
			h.finishRemoteCodingJob(job, out, body.RequirementID, model, "adjust-coding", "✅ 追加调整完成！")
			return
		}
		cmd, cancel := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:         adjustPrompt,
			WorkDir:        workDir,
			SystemPrompt:   "", // resume 已携带 developer persona，不再注入
			Model:          cliModelArg(model),
			ClaudeConfigID: claudeConfigID,
			SessionID:      req.CodingSessionID,
			Resume:         true,
			Fork:           false,
		})
		defer cancel()
		adjustUsage := h.usageCtxFor("adjust_coding", body.RequirementID, req.ProjectID, job.ID, model, "", body.Message)
		out := runClaudeStream(jobSink{job}, cmd, "adjust-coding", adjustUsage)

		// Stale --resume: the coding session file is gone (~/.claude/ cleaned
		// or too old). Surface a clear error rather than silently starting a
		// fresh session — the user must re-run start-coding to mint a new one.
		if out.staleSession {
			job.Append(store.LogLine{Type: "error", Content: "❌ 原 coding 会话已失效（session 文件不存在），请重新发起 coding。"})
			job.Finish(1, store.JobError)
			return
		}
		if out.errMsg != "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult == "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
			job.Finish(1, store.JobError)
			return
		}
		// job_done: keep status="done" (no UpdateStatus call) and do NOT update
		// coding_session_id (no UpdateCodingSession call) — every later adjust
		// round resumes the SAME original coding session.
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		job.Append(store.LogLine{Type: "done", Content: "✅ 追加调整完成！"})
		// Record the effective developer model for this adjust round (success
		// path only — "most recent successful run" semantics).
		if perr := h.reqSvc.UpdateDeveloperModel(body.RequirementID, model); perr != nil {
			log.Printf("[adjust-coding] failed to persist developer_model for %s: %v", body.RequirementID, perr)
		}
		// agent_server_id re-binding on the success path is intentionally
		// skipped: dev_source / agent_server_id are already correct from the
		// original StartCoding prologue. An adjust turn runs on the same
		// coding session / worktree the requirement was already bound to, so
		// re-writing the binding here would either no-op (same value) or risk
		// silently re-pointing a running worktree to a different host. If the
		// user genuinely wants to switch servers, they should re-run the full
		// start-coding flow.
		job.Finish(0, store.JobDone)
		log.Printf("[adjust-coding] job %s finished for %s", job.ID, body.RequirementID)
	}()
}

// finishRemoteCodingJob applies the shared terminal-state handling for a
// runRemoteCoding outcome: map the three failure shapes (stale session /
// explicit error / empty result) onto job error frames, otherwise append the
// result + a done frame and stamp developer_model. Factored out so
// StartCoding / AdjustCoding / ContinueCoding all report remote runs
// identically — the frontend can't tell a remote job from a local one.
func (h *WizardHandler) finishRemoteCodingJob(job *store.Job, out claudeStreamOutcome, reqID, model, tag, doneMsg string) {
	switch {
	case out.staleSession:
		job.Append(store.LogLine{Type: "error", Content: "❌ 原 coding 会话已失效（session 文件不存在），请重新发起 coding。"})
		job.Finish(1, store.JobError)
		return
	case out.errMsg != "":
		job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
		job.Finish(1, store.JobError)
		return
	case out.finalResult == "":
		job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
		job.Finish(1, store.JobError)
		return
	}
	job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
	job.Append(store.LogLine{Type: "done", Content: doneMsg})
	if reqID != "" {
		if perr := h.reqSvc.UpdateDeveloperModel(reqID, model); perr != nil {
			log.Printf("[%s] failed to persist developer_model for %s: %v", tag, reqID, perr)
		}
	}
	job.Finish(0, store.JobDone)
	log.Printf("[%s] remote job %s finished for %s", tag, job.ID, reqID)
}

// ContinueCoding resumes an interrupted/cleared coding task by --resume'ing the
// persisted coding session and asking Claude to pick up where it left off. This
// is the recovery path for the "开发任务已完成（日志因服务重启已清空）" state:
// the in-memory job log is gone after a backend restart, but the coding session
// lives on disk (~/.claude/) and coding_session_id is persisted in the DB, so a
// --resume lets Claude continue the work and re-report what was done.
//
// Only requirements in status "developing" with a non-empty coding_session_id
// may continue (developing = the first coding pass was launched; the lost-log
// recovery block renders exactly in this state). Unlike start-coding — which
// FORKS off the design session to realize "重新开发" — continue-coding RESUMES
// the original coding session (Fork:false) and re-feeds NOTHING except a short
// "continue" instruction, since the resumed conversation already carries the
// requirement, design, developer persona, and prior coding progress. The
// job_done handler does NOT change status and does NOT update coding_session_id
// (every continue round resumes the SAME session). A stale --resume surfaces a
// clear error instead of silently starting a fresh session.
//
// POST /api/wizard/continue-coding { requirement_id } -> { job_id }
func (h *WizardHandler) ContinueCoding(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequirementID string `json:"requirement_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[continue-coding] JSON decode error: %v", err)
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}

	req, err := h.reqSvc.Get(body.RequirementID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "requirement not found")
		return
	}
	if req.Status != "developing" {
		writeError(w, 409, "INVALID_STATUS", "仅开发中的需求可继续开发（当前状态: "+req.Status+"）")
		return
	}
	if req.CodingSessionID == "" {
		writeError(w, 409, "NO_SESSION", "无 coding session，无法 resume，请重新发起 coding")
		return
	}

	proj, err := h.projectSvc.Get(req.ProjectID)
	if err != nil {
		writeError(w, 404, "NOT_FOUND", "project not found")
		return
	}

	// Only the developer role's MODEL is honored (so the user's latest model
	// setting applies to the continuation). The system prompt is deliberately
	// omitted: the resumed coding session already carries the developer persona,
	// and re-injecting --system-prompt would replace it (same as AdjustCoding).
	_, model, claudeConfigID := h.roleConfig("developer")

	job := h.jobs.Create(body.RequirementID)
	job.SetType("continue_coding")
	job.SetModel(model)
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	go func() {
		defer func() {
			// Persist the finished job's full log so a backend restart doesn't
			// wipe this continuation record (same durability pattern as
			// StartCoding / AdjustCoding). This is what "fills back" the lost
			// development record after a restart.
			lines, status, exitCode := job.Snapshot()
			if perr := h.jobLogSvc.Save(job.ID, body.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, model); perr != nil {
				log.Printf("[continue-coding] failed to persist job log %s: %v", job.ID, perr)
			}
		}()
		log.Printf("[continue-coding] job %s started for %s (resume %s)", job.ID, body.RequirementID, req.CodingSessionID)
		job.Append(store.LogLine{Type: "phase", Content: "🤖 Claude 正在续接开发会话，继续完成任务..."})

		// The resumed coding session already carries the requirement, design,
		// developer persona, and prior coding progress, so the prompt is only a
		// short "continue" instruction: re-inspect the workdir, finish whatever
		// is incomplete, and report what was done. No project context re-feed.
		workDir := proj.LocalPath
		if req.WorktreePath != "" {
			if _, statErr := os.Stat(req.WorktreePath); statErr == nil {
				workDir = req.WorktreePath
			}
		}
		prompt := "继续完成之前中断的开发任务。请先检查当前代码与工作区状态，" +
			"判断哪些部分已完成、哪些未完成或需要修复；然后基于技术方案继续完成剩余工作、补齐缺失内容。" +
			"最后用中文总结本次完成的内容。"
		// Kind-specific developer tail — keeps an Issue session's framing
		// ("最小改动、修复根因") present on every continue round.
		if block := promptpkg.DeveloperBlock(req.Kind, req); block != "" {
			prompt += "\n\n" + block
		}
		// Same execution-consistency rule as AdjustCoding: continue the work
		// where the working tree actually lives.
		if req.AgentServerID != "" && h.agentSvrSvc != nil {
			out := h.runRemoteCoding(&remoteCodingInput{
				job:      job,
				serverID: req.AgentServerID,
				req: startCodingReq{
					RequirementTitle: req.Title,
					RequirementID:    req.ID,
					BranchName:       req.BranchName,
					AgentServerID:    req.AgentServerID,
				},
				reqRow:     req,
				prompt:     prompt,
				sourceSID:  req.CodingSessionID,
				sessionArg: req.CodingSessionID,
				model:      model,
				usage:      h.usageCtxFor("continue_coding", body.RequirementID, req.ProjectID, job.ID, model, "", ""),
			})
			h.finishRemoteCodingJob(job, out, body.RequirementID, model, "continue-coding", "✅ 续接开发完成！")
			return
		}
		cmd, cancel := h.llm.GenerateCode(llm.StreamOpts{
			Prompt:         prompt,
			WorkDir:        workDir,
			SystemPrompt:   "", // resume 已携带 developer persona，不再注入
			Model:          cliModelArg(model),
			ClaudeConfigID: claudeConfigID,
			SessionID:      req.CodingSessionID,
			Resume:         true,
			Fork:           false,
		})
		defer cancel()
		continueUsage := h.usageCtxFor("continue_coding", body.RequirementID, req.ProjectID, job.ID, model, "", "")
		out := runClaudeStream(jobSink{job}, cmd, "continue-coding", continueUsage)

		// Stale --resume: the coding session file is gone. Surface a clear error
		// rather than silently starting fresh — the user can still 重新开发
		// (which forks off the design session) as a fallback.
		if out.staleSession {
			job.Append(store.LogLine{Type: "error", Content: "❌ 原 coding 会话已失效（session 文件不存在），请重新发起 coding 或使用重新开发。"})
			job.Finish(1, store.JobError)
			return
		}
		if out.errMsg != "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ " + out.errMsg})
			job.Finish(1, store.JobError)
			return
		}
		if out.finalResult == "" {
			job.Append(store.LogLine{Type: "error", Content: "❌ Claude 未返回结果，请重试"})
			job.Finish(1, store.JobError)
			return
		}
		// job_done: keep status="developing" (no UpdateStatus call) and do NOT
		// update coding_session_id (no UpdateCodingSession call) — every continue
		// round resumes the SAME original coding session.
		job.Append(store.LogLine{Type: "result", Content: strings.TrimSpace(out.finalResult)})
		job.Append(store.LogLine{Type: "done", Content: "✅ 继续开发完成！"})
		// Record the effective developer model for this continue round (success
		// path only — "most recent successful run" semantics).
		if perr := h.reqSvc.UpdateDeveloperModel(body.RequirementID, model); perr != nil {
			log.Printf("[continue-coding] failed to persist developer_model for %s: %v", body.RequirementID, perr)
		}
		// Re-bind the Agent Server when the continuation ran on one. Empty body
		// field = legacy client → keep the existing binding (same guard as
		// AdjustCoding).
		// dev_source / agent_server_id are intentionally NOT re-bound here — see the
		// rationale in AdjustCoding's success-path comment: the binding is set by
		// the original StartCoding prologue and a continue turn resumes the
		// same coding session on the same worktree.
		job.Finish(0, store.JobDone)
		log.Printf("[continue-coding] job %s finished for %s", job.ID, body.RequirementID)
	}()
}

// runCallbacks lets the scheduler hook into a job's terminal Finish so it
// can flip the scheduled_tasks row from running → succeeded/failed. The
// HTTP path passes nil; the scheduler path passes a closure built from
// ScheduledExecutor.
type runCallbacks struct {
	OnFinish func(jobID string, ok bool)
}
