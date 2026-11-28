package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
)

// 跨项目需求 · 方案调整对话的 HTTP 契约。
//
// 这些是纯 HTTP 层的断言，不依赖 claude CLI：准入条件、线程的读写、以及
// 「方案会话被重跑之后要重新 fork」这条会话决策。

// seedCrossDesign 给跨项目需求装上一份整体方案（带契约章节），并记下它
// 是哪个会话写的。
func seedCrossDesign(t *testing.T, h *CrossRequirementHandler, xreqID, sessionID string) {
	t.Helper()
	markdown := "## 需求目标\n\n统一订单接口。\n\n## 跨项目接口契约\n\nGET /api/v1/orders/{id}\n\n" +
		"## 待决策事项\n\n- **接口默认超时**：3s 还是 5s？\n"
	if err := h.svc.SaveDesign(xreqID, markdown, sessionID, "job_design"); err != nil {
		t.Fatalf("save design: %v", err)
	}
}

// askAdjust issues POST /api/cross-requirements/{id}/design/adjust.
func askAdjust(t *testing.T, h *CrossRequirementHandler, xreqID string, body any) (int, map[string]any) {
	t.Helper()
	return callCross(t, h.AskAdjust, http.MethodPost, "/api/cross-requirements/"+xreqID+"/design/adjust", body)
}

// 没有整体方案 → 无从调整。
func TestCrossAdjust_AskWithoutDesign(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "还没跑方案", p1)

	code, env := askAdjust(t, h, id, map[string]any{"question": "超时取多少？"})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %v", code, env)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "XREQ_NO_DESIGN" {
		t.Fatalf("error code = %v, want XREQ_NO_DESIGN", errObj["code"])
	}
}

// 回收站里的行是只读的。
func TestCrossAdjust_AskOnTrashed(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待调整", p1)
	seedCrossDesign(t, h, id, "design-sid-1")
	if err := h.svc.MoveToTrash(id); err != nil {
		t.Fatalf("move to trash: %v", err)
	}

	code, env := askAdjust(t, h, id, map[string]any{"question": "超时取多少？"})
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %v", code, env)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "XREQ_IN_TRASH" {
		t.Fatalf("error code = %v, want XREQ_IN_TRASH", errObj["code"])
	}
}

// 问题不能为空。
func TestCrossAdjust_AskEmptyQuestion(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待调整", p1)
	seedCrossDesign(t, h, id, "design-sid-1")

	code, env := askAdjust(t, h, id, map[string]any{"question": "   "})
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %v", code, env)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "BAD_REQUEST" {
		t.Fatalf("error code = %v, want BAD_REQUEST", errObj["code"])
	}
}

// 提问会建一行 running 的轮次，并把会话账一起落库（预铸的 session id +
// fork 来源快照）。
//
// 后台那一轮会因为测试里没有 claude CLI 而失败（panic 被 runAdjust 的
// recover 吃掉），所以这里断言的是**创建时**写下的、之后不会被改写的字段。
func TestCrossAdjust_AskCreatesRunningTurn(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待调整", p1)
	seedCrossDesign(t, h, id, "design-sid-1")

	code, env := askAdjust(t, h, id, map[string]any{"question": "接口超时取 3s 还是 5s？"})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	jobID, _ := data["job_id"].(string)
	turnID, _ := data["turn_id"].(string)
	if jobID == "" || turnID == "" {
		t.Fatalf("the response must carry job_id and turn_id: %v", data)
	}

	turn, err := h.qaSvc.Get(turnID)
	if err != nil {
		t.Fatalf("the turn must be persisted: %v", err)
	}
	if turn.Question != "接口超时取 3s 还是 5s？" {
		t.Fatalf("the question was not persisted: %q", turn.Question)
	}
	if turn.JobID != jobID {
		t.Fatalf("the turn must remember its job: %q vs %q", turn.JobID, jobID)
	}
	// 预铸的会话 id 与 fork 来源快照必须落库 —— 崩在「已派发」与「已回答」
	// 之间时，这一行就是「转录属于哪个会话」的唯一凭据。
	if turn.SessionID == "" {
		t.Fatalf("the pre-minted session id must be persisted")
	}
	if turn.SourceSessionID != "design-sid-1" {
		t.Fatalf("the fork source must be the design session, got %q", turn.SourceSessionID)
	}
}

// 线程读出来是最老在前。
func TestCrossAdjust_ListThread(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待调整", p1)
	seedCrossDesign(t, h, id, "design-sid-1")

	for _, q := range []string{"第一个", "第二个"} {
		if _, err := h.qaSvc.Create(&model.CrossRequirementQATurn{
			CrossRequirementID: id, Question: q, JobID: "job_" + q, SessionID: "s_" + q,
		}); err != nil {
			t.Fatalf("seed turn: %v", err)
		}
	}

	code, env := callCross(t, h.AdjustState, http.MethodGet, "/api/cross-requirements/"+id+"/design/adjust", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	turns, _ := data["turns"].([]any)
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns, got %d: %v", len(turns), data)
	}
	first, _ := turns[0].(map[string]any)
	if first["question"] != "第一个" {
		t.Fatalf("the thread must read oldest-first: %v", turns)
	}
}

// 清空线程。
func TestCrossAdjust_ResetThread(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待调整", p1)
	seedCrossDesign(t, h, id, "design-sid-1")

	if _, err := h.qaSvc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: id, Question: "一个问题", JobID: "job_1", SessionID: "s_1",
	}); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	code, env := callCross(t, h.ResetAdjust, http.MethodDelete, "/api/cross-requirements/"+id+"/design/adjust", nil)
	if code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %v", code, env)
	}
	rows, _ := h.qaSvc.List(id)
	if len(rows) != 0 {
		t.Fatalf("the thread must be empty after a reset: %+v", rows)
	}
}

// 删掉一轮。
func TestCrossAdjust_DeleteTurn(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待调整", p1)
	seedCrossDesign(t, h, id, "design-sid-1")

	turn, err := h.qaSvc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: id, Question: "一个问题", JobID: "job_1", SessionID: "s_1",
	})
	if err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	code, env := callCross(t, h.DeleteAdjustTurn, http.MethodDelete,
		"/api/cross-requirements/"+id+"/design/adjust/turns/"+turn.ID, nil)
	if code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %v", code, env)
	}
	rows, _ := h.qaSvc.List(id)
	if len(rows) != 0 {
		t.Fatalf("the turn must be gone: %+v", rows)
	}
}

// 后端重启留下的 running 行必须被读入口修成 error —— 否则线程里是一颗永远
// 转圈的球。
func TestCrossAdjust_RepairsOrphanedTurn(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待调整", p1)
	seedCrossDesign(t, h, id, "design-sid-1")

	turn, err := h.qaSvc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: id, Question: "跑到一半", JobID: "job_gone", SessionID: "s_1",
	})
	if err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	if _, env := callCross(t, h.AdjustState, http.MethodGet, "/api/cross-requirements/"+id+"/design/adjust", nil); env == nil {
		t.Fatalf("the state endpoint must answer")
	}
	got, err := h.qaSvc.Get(turn.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.QAStatusError {
		t.Fatalf("an orphaned turn must be repaired to error, got %q", got.Status)
	}
}

// SSE 端点的归属校验：别人的作业 id 不能被吐出来（作业 id 只有 8 个 hex，
// 是能猜的）。
func TestCrossAdjust_StreamJobOwnership(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	p2 := seedCrossProject(t, d, "api-svc")
	owner := createCrossRequirement(t, h, "归属者", p1)
	other := createCrossRequirement(t, h, "别人", p2)

	job := h.jobs.Create(owner)
	code, env := callCross(t, h.StreamAdjustJob, http.MethodGet,
		"/api/cross-requirements/"+other+"/design/adjust/"+job.ID+"/stream", nil)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %v", code, env)
	}
}

// ── 会话决策 ───────────────────────────────────────────────────────────────

// 首轮 fork 方案会话；后续轮续接线程自己的会话；方案被重跑之后重新 fork。
func TestResolveCrossAdjustSession(t *testing.T) {
	// 首轮：有方案会话，没有线程 → fork。
	plan := resolveCrossAdjustSession("design-sid-1", nil)
	if plan.Mode != qaModeFork {
		t.Fatalf("mode = %q, want fork", plan.Mode)
	}
	if plan.SourceSID != "design-sid-1" {
		t.Fatalf("the fork source must be the design session, got %q", plan.SourceSID)
	}
	if plan.NewSessionID == "" || plan.NewSessionID == "design-sid-1" {
		t.Fatalf("a fork needs a fresh session id, got %q", plan.NewSessionID)
	}
	if plan.Resumed {
		t.Fatalf("a fork is not a resume")
	}

	// 后续轮：线程会话健在，源还是同一个方案会话 → resume。
	thread := &model.CrossRequirementQATurn{SessionID: "thread-sid", SourceSessionID: "design-sid-1"}
	plan = resolveCrossAdjustSession("design-sid-1", thread)
	if plan.Mode != qaModeResume {
		t.Fatalf("mode = %q, want resume", plan.Mode)
	}
	if plan.ResumeSID != "thread-sid" {
		t.Fatalf("resume sid = %q, want thread-sid", plan.ResumeSID)
	}
	if !plan.Resumed {
		t.Fatalf("a resume must be marked as such")
	}

	// 方案被重跑 → 源失配 → 重新 fork。
	plan = resolveCrossAdjustSession("design-sid-2", thread)
	if plan.Mode != qaModeFork {
		t.Fatalf("mode = %q, want fork after the plan was regenerated", plan.Mode)
	}
	if plan.SourceSID != "design-sid-2" {
		t.Fatalf("the new fork must come from the new design session, got %q", plan.SourceSID)
	}

	// 没有方案会话（手工编辑过 / 存量方案）且线程也没有 → fresh。
	plan = resolveCrossAdjustSession("", nil)
	if plan.Mode != qaModeFresh {
		t.Fatalf("mode = %q, want fresh", plan.Mode)
	}
}

// 方案没有会话 id，但线程会话还在 → 继续线程（别把用户已经谈拢的东西丢掉）。
func TestResolveCrossAdjustSession_DesignWithoutSession(t *testing.T) {
	thread := &model.CrossRequirementQATurn{SessionID: "thread-sid", SourceSessionID: "design-sid-1"}
	plan := resolveCrossAdjustSession("", thread)
	if plan.Mode != qaModeResume {
		t.Fatalf("mode = %q, want resume", plan.Mode)
	}
}

// 调整对话与方案作业跑在**同一个工作目录** —— 这是 fork 策略的地基：claude CLI
// 的会话转录按 cwd 的 slug 分目录存放，换 cwd 就找不到源会话。
func TestCrossAdjust_SharesTheDesignJobWorkingDirectory(t *testing.T) {
	designDir, err := crossWorkspaceDir("xreq_abcdef0123456789", crossRunDesign)
	if err != nil {
		t.Fatalf("crossWorkspaceDir: %v", err)
	}
	// 调整对话用的就是这个目录（runAdjust 里调的也是 crossRunDesign）。
	splitDir, err := crossWorkspaceDir("xreq_abcdef0123456789", crossRunSplit)
	if err != nil {
		t.Fatalf("crossWorkspaceDir: %v", err)
	}
	if designDir == splitDir {
		t.Fatalf("the design and split jobs must not share a directory")
	}
	if !strings.HasSuffix(designDir, "/design") {
		t.Fatalf("the design working directory must be the <id>/design folder, got %q", designDir)
	}
}

// 详情接口要带上待决策事项 —— 它是 chip 的数据源。
func TestCrossRequirementDetail_ExposesPendingDecisions(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待调整", p1)
	seedCrossDesign(t, h, id, "design-sid-1")

	detail, err := h.svc.Detail(id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %+v", detail.PendingDecisions)
	}
	if detail.PendingDecisions[0].Title != "接口默认超时" {
		t.Fatalf("unexpected pending decision: %+v", detail.PendingDecisions[0])
	}

	// 没有待决策章节时是空数组，不是 null —— 前端要么兜住 null，要么后端给
	// 空数组。这里给空数组。
	if err := h.svc.SaveDesign(id, "## 需求目标\n\nx\n\n## 跨项目接口契约\n\nGET /x\n", "", ""); err != nil {
		t.Fatalf("save design: %v", err)
	}
	detail, err = h.svc.Detail(id)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail.PendingDecisions == nil {
		t.Fatalf("pending decisions must be an empty slice, not nil")
	}
	if len(detail.PendingDecisions) != 0 {
		t.Fatalf("expected no pending decisions, got %+v", detail.PendingDecisions)
	}
}

// Purge 必须显式删掉调整对话的行 —— schema 里的 ON DELETE CASCADE 是装饰
// （SQLite 从不开 PRAGMA foreign_keys）。
func TestCrossRequirement_PurgeDeletesAdjustTurns(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待清理", p1)
	seedCrossDesign(t, h, id, "design-sid-1")

	if _, err := h.qaSvc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: id, Question: "一个问题", JobID: "job_1", SessionID: "s_1",
	}); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	if err := h.svc.MoveToTrash(id); err != nil {
		t.Fatalf("move to trash: %v", err)
	}
	if err := h.svc.Purge(id); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var count int
	if err := d.QueryRow(`SELECT count(*) FROM cross_requirement_qa_turns WHERE cross_requirement_id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("purging must delete the adjustment turns, %d left", count)
	}
}

// 讨论记录只取已答完的轮次 —— 半截的答案不是结论。
func TestCrossDesignDiscussion_SkipsUnansweredTurns(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	id := createCrossRequirement(t, h, "待调整", p1)

	answered, _ := h.qaSvc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: id, Question: "问过的", JobID: "job_a", SessionID: "s_a",
	})
	if err := h.qaSvc.MarkFinished(answered.ID, model.QAStatusDone, "答过的", "sonnet", "s_a", "src", true, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := h.qaSvc.Create(&model.CrossRequirementQATurn{
		CrossRequirementID: id, Question: "还在跑的", JobID: "job_b", SessionID: "s_b",
	}); err != nil {
		t.Fatalf("seed running turn: %v", err)
	}

	turns := h.crossDesignDiscussion(id)
	if len(turns) != 1 {
		t.Fatalf("only answered turns belong in the discussion, got %+v", turns)
	}
	if turns[0].Answer != "答过的" {
		t.Fatalf("unexpected turn: %+v", turns[0])
	}
	// 渲染出来的那一段必须真的带得上结论。
	block := prompt.BuildCrossDiscussionBlock(turns)
	if block == "" {
		t.Fatalf("a non-empty discussion must render a block")
	}
}
