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
	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

type WizardHandler struct {
	db           *db.DB
	projectSvc   *service.ProjectService
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
	// summaryKickIntervalSec is the period of the summary-round heartbeat
	// (MarkSummaryHeartbeat) the RunOrchestratorSummary goroutine issues while
	// the summary is in flight. Default 5s matches the batch-recovery cutoff
	// in OrchestrationBatchService.Recover so a backend crash surfaces within
	// one heartbeat cycle.
	summaryKickIntervalSec int
}

func NewWizardHandler(database *db.DB, projectSvc *service.ProjectService, reqSvc *service.RequirementService, knowledgeSvc *service.KnowledgeService, llmGateway *llm.Gateway, jobs *store.JobStore, roleSvc *service.RoleService, jobLogSvc *service.JobLogService, claudeCfg *service.ClaudeConfigService, usageSvc usageRecorder, skillSvc *service.SkillService, platformSvc *service.PlatformTokenService, agentSvrSvc *service.AgentServerService, subTaskSvc *service.SubTaskService, subTaskRunner *SubTaskRunner, batchSvc *service.OrchestrationBatchService) *WizardHandler {
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
		subTaskSvc:             subTaskSvc,
		agentSvrSvc:            agentSvrSvc,
		subTaskRunner:          subTaskRunner,
		batchSvc:               batchSvc,
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
