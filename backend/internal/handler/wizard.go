// Package handler — wizard pipeline (requirement → code).
//
// The wizard surface is intentionally split across several files in this
// package for readability; see wizard_common.go for shared helpers,
// wizard_analyst.go / wizard_architect.go / wizard_coding.go for the
// three role-gated stages, wizard_subtask.go / wizard_orchestration.go
// for the sub-task pipeline, wizard_stream.go for the CLI stream parser,
// and wizard_jobs.go for job introspection.
package handler

import (
	"context"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

type WizardHandler struct {
	db           *db.DB
	projectSvc   projectService
	reqSvc       *service.RequirementService
	knowledgeSvc *service.KnowledgeService
	llm          *llm.Gateway
	jobs         *store.JobStore
	roleSvc      *service.RoleService
	jobLogSvc    *service.JobLogService
	claudeCfg    *service.ClaudeConfigService
	usageSvc     usageRecorder
	skillSvc     *service.SkillService
	platformSvc  *service.PlatformTokenService
	// worktreeH bundles the per-requirement worktree helpers with the
	// service-layer credential service so execStartCoding's
	// `git fetch`/`git pull` calls (defect points 2 & 4) can inject the
	// project's platform token instead of relying on ambient credentials.
	worktreeH *WorktreeHandler
	// gitCredSvc is a direct handle to the same service for the two
	// in-line `git pull` invocations in execStartCoding (line 735, 742).
	// Sharing the field with worktreeH would work too, but exposing both
	// keeps the call sites flat — each pull can be wrapped without an
	// extra method hop through WorktreeHandler.
	gitCredSvc *service.GitCredentialService
	// subTaskSvc handles persistence for sub-tasks (manually-triggered child
	// agents that fork the requirement's main-agent session). Optional — when
	// nil the sub-task endpoints are not registered (legacy / standalone
	// deployments without the feature).
	subTaskSvc *service.SubTaskService
	// agentSvrSvc exposes remote Linux/macOS SSH targets whose sealed
	// credentials are used when StartCoding runs against agent_server_id.
	// nil in standalone / non-distributed deployments.
	agentSvrSvc *service.AgentServerService
	// subTaskRunner is the shared executor for sub-task rows. Both the wizard
	// (manual sub-tasks + auto-orchestrated children) and the merge handler
	// (push + PR sub-task) delegate to it so the runtime semantics — session
	// fork, executor role persona, artifact + token persistence — stay in one
	// place.
	subTaskRunner *SubTaskRunner
	// batchSvc is the persistence layer for orchestration_batches — the
	// coordinator row that groups N sub_tasks into one restart-safe dispatch
	// run. tryAutoOrchestrate writes one alongside N sub_tasks in a single
	// transaction; RunOrchestratorSummary advances summary_status; the queue
	// (below) reads active batches every tick. Nil-safe: a missing batchSvc
	// disables both the auto-orchestrate path and the manual summary handler.
	batchSvc *service.OrchestrationBatchService
	// orchQueue is the scheduler tick loop that drives restart-safe dispatch
	// for auto-orchestrated sub-tasks. Injected AFTER construction via
	// SetOrchQueue so we don't import the scheduler package here (avoids the
	// wizard→scheduler cycle through model/JobStore). The interface is the
	// narrowest surface the wizard needs (just Kick). Nil before main.go wires
	// it; handlers must nil-check before calling.
	orchQueue interface {
		Kick()
	}
	// settingSvc exposes the settings KV (git.sync_timeout_seconds, subtask
	// policy, …) to wizard handlers. The architect-design stage re-reads
	// GitSyncTimeout() on every entry so a value change takes effect without
	// a restart — same hot-read pattern as SubTaskConfig. Optional: when nil
	// the wizard falls back to the service-layer default (60s).
	settingSvc *service.SettingService
	// summaryKickIntervalSec is the period of the summary-round heartbeat
	// (MarkSummaryHeartbeat) the RunOrchestratorSummary goroutine issues while
	// the summary is in flight. Default 5s matches the batch-recovery cutoff
	// in OrchestrationBatchService.Recover so a backend crash surfaces within
	// one heartbeat cycle.
	summaryKickIntervalSec int
	// issueMonSvc is the persistence layer for issue_monitors. Wired from
	// main.go via SetIssueMonSvc. nil in deployments without the issue-
	// monitor feature; autoPushPR's comment-back hook is then a no-op (the
	// wizard must never crash for a missing optional dep).
	issueMonSvc *service.IssueMonitorService
	// attachSvc handles per-requirement file attachments (manifest_text +
	// description block injected into every wizard prompt). Wired from
	// main.go via SetAttachmentSvc. nil before the wire up — wizard
	// stages simply skip the manifest block when nil (no behavior change
	// vs pre-feature).
	attachSvc *service.AttachmentService
	// designRevSvc handles the design_revisions table — the versioned
	// history of each requirement's design plan. Wired after construction
	// from main.go via SetDesignRevisionSvc because the wizard handler
	// participates in a constructor cycle with both the service and the
	// handlers that consume it. Required for the 「重新生成技术方案」 flow
	// (freeze current plan → run → restore on demand); nil disables just
	// the version-history wiring while every other wizard feature keeps
	// working (a deployment that doesn't need regenerating can leave it
	// nil without affecting any first-run path).
	designRevSvc *service.DesignRevisionService
}

func NewWizardHandler(database *db.DB, projectSvc projectService, reqSvc *service.RequirementService, knowledgeSvc *service.KnowledgeService, llmGateway *llm.Gateway, jobs *store.JobStore, roleSvc *service.RoleService, jobLogSvc *service.JobLogService, claudeCfg *service.ClaudeConfigService, usageSvc usageRecorder, skillSvc *service.SkillService, platformSvc *service.PlatformTokenService, agentSvrSvc *service.AgentServerService, subTaskSvc *service.SubTaskService, subTaskRunner *SubTaskRunner, batchSvc *service.OrchestrationBatchService, settingSvc *service.SettingService, worktreeH *WorktreeHandler, gitCredSvc *service.GitCredentialService) *WizardHandler {
	return &WizardHandler{
		db:                     database,
		projectSvc:             projectSvc,
		reqSvc:                 reqSvc,
		knowledgeSvc:           knowledgeSvc,
		llm:                    llmGateway,
		jobs:                   jobs,
		roleSvc:                roleSvc,
		jobLogSvc:              jobLogSvc,
		claudeCfg:              claudeCfg,
		usageSvc:               usageSvc,
		skillSvc:               skillSvc,
		platformSvc:            platformSvc,
		worktreeH:              worktreeH,
		gitCredSvc:             gitCredSvc,
		subTaskSvc:             subTaskSvc,
		agentSvrSvc:            agentSvrSvc,
		subTaskRunner:          subTaskRunner,
		batchSvc:               batchSvc,
		settingSvc:             settingSvc,
		summaryKickIntervalSec: 5,
	}
}

// SetOrchQueue injects the scheduler.OrchestrationQueue after construction.
// main.go wires the queue AFTER the wizard handler is built (the queue holds
// a SubTaskExecutor reference to the wizard), so the wizard needs a setter
// rather than a constructor argument. Passing nil disables auto-kick — the
// scheduler tick still picks the batch up on its next interval.
func (h *WizardHandler) SetOrchQueue(q interface{ Kick() }) {
	h.orchQueue = q
}

// SetIssueMonSvc injects the issue_monitor service after construction.
// Used by autoPushPR's comment-back hook (v0.5.x): the hook reads the
// monitor row to learn whether to post a single "PR created" comment on
// the originating GitHub Issue, and to flip monitor.comment_posted so a
// re-push never comments twice. Passing nil disables the hook entirely
// (the wizard must never crash for a missing optional dep).
func (h *WizardHandler) SetIssueMonSvc(svc *service.IssueMonitorService) {
	h.issueMonSvc = svc
}

// SetAttachmentSvc injects the per-requirement attachment service. The
// wizard's three role-gated stages (analyst / architect / developer) all
// share one attachment set; attachManifestFor renders it into the system
// prompt via attachManifestFor so Claude knows what files are available.
// Passing nil disables the injection (legacy / no-feature deployments).
//
// The directory Claude should treat as the attachment root depends on the
// exec env (local → ~/.novaworkbench/attachments/<req_id>; remote agent
// server → its synced mirror). For now only the local path is wired —
// Agent-server attachment sync is a follow-up; remote callers will still
// see the manifest with the local path baked into the location lines, which
// the LLM treats as opaque identifiers (it has Bash to figure out the real
// path if/when sync lands).
func (h *WizardHandler) SetAttachmentSvc(svc *service.AttachmentService) {
	h.attachSvc = svc
}

// SetDesignRevisionSvc injects the DesignRevisionService used by the
// 「重新生成技术方案」 flow (wizard_architect.go) and the design-revisions
// REST handlers (wizard_design_revision.go). Called from main.go AFTER
// NewWizardHandler because the service participates in the wizard→service
// dependency cycle. Passing nil is safe but disables both the freeze-on-
// regenerate step inside prepareArchitectDesign and every read endpoint
// the new handler registers.
func (h *WizardHandler) SetDesignRevisionSvc(svc *service.DesignRevisionService) {
	h.designRevSvc = svc
}

// attachManifestFor returns the attachment manifest markdown for a
// requirement, ready to be appended to a stage's system prompt. Empty
// string when the service isn't wired or no attachments exist — callers
// can append unconditionally without nil-checking the result.
//
// attachDir is the directory Claude should treat as the attachment root.
// Local execution passes the local root; remote Agent-server execution
// passes the synced mirror on the agent host (see resolveAttachDirFor).
// Passing "" falls back to the local root as a safe default.
func (h *WizardHandler) attachManifestFor(reqID, attachDir string) string {
	return attachManifestFor(h.attachSvc, reqID, attachDir)
}

// resolveAttachDir returns the directory the wizard should pass into
// attachManifestFor. Local runs get the local root; remote runs first
// attempt a sync to the agent server and on success use the canonical
// remote path, otherwise silently fall back to local paths so a
// intermittent SSH error doesn't break the run. Thin delegate to the
// package-level resolveAttachDirFor so SubTaskRunner can share one
// implementation without depending on WizardHandler.
func (h *WizardHandler) resolveAttachDir(ctx context.Context, reqID, serverID string) string {
	return resolveAttachDirFor(ctx, h.attachSvc, h.agentSvrSvc, reqID, serverID)
}
