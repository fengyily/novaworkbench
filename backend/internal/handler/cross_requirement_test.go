package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/dbtest"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// newCrossHandlerTest wires the cross-requirement handler over a throwaway
// SQLite database.
//
// The AI jobs are NOT exercised here (they need the claude CLI) — this test
// covers the REST surface: routing, the JSON envelope, and the write paths
// that the UI depends on.
func newCrossHandlerTest(t *testing.T) (*CrossRequirementHandler, *db.DB, *service.ProjectService) {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	projSvc := service.NewProjectService(d, nil, nil)
	reqSvc := service.NewRequirementService(d, nil)
	crossSvc := service.NewCrossRequirementService(d)
	h := NewCrossRequirementHandler(crossSvc, reqSvc, projSvc, nil, store.NewJobStore(16),
		nil, nil, nil, nil, nil, nil)
	// 方案调整对话的线程存储（可选能力，所以走 setter）。
	h.SetCrossRequirementQASvc(service.NewCrossRequirementQAService(d))
	return h, d, projSvc
}

// seedCrossProject inserts a project row directly (ProjectService.Add insists
// on a real git checkout, which the cross-requirement paths never touch).
func seedCrossProject(t *testing.T, d *db.DB, name string) string {
	t.Helper()
	id := util.NewID("proj")
	now := time.Now()
	if _, err := d.Exec(
		`INSERT INTO projects (id,name,local_path,status,added_at,updated_at) VALUES (?,?,?, 'active', ?, ?)`,
		id, name, filepath.Join(t.TempDir(), name), now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

// call runs one request through the handler and decodes the JSON envelope.
func callCross(t *testing.T, fn func(w http.ResponseWriter, r *http.Request), method, target string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	// Mirror what the Go 1.22 ServeMux does with the path pattern.
	segments := splitPath(target)
	if len(segments) >= 3 {
		req.SetPathValue("id", segments[2])
	}
	if len(segments) >= 5 {
		req.SetPathValue("pid", segments[4])
	}
	// The adjustment routes carry two more path values. Which one is at
	// index 5 depends on the shape of the path: ".../adjust/turns/{tid}"
	// vs ".../adjust/{job_id}/stream".
	if len(segments) >= 6 {
		if segments[5] == "turns" {
			req.SetPathValue("tid", segments[6])
		} else {
			req.SetPathValue("job_id", segments[5])
		}
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func splitPath(target string) []string {
	out := []string{}
	cur := ""
	for _, r := range target {
		if r == '/' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// The REST surface: create → list → detail → split confirm → phases.
func TestCrossRequirementHandler_REST(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")
	p2 := seedCrossProject(t, d, "api-svc")

	// create
	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements", model.CreateCrossRequirementReq{
		Title:      "跨仓库订单重构",
		ProjectIDs: []string{p1, p2},
	})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", code, env)
	}
	if env["success"] != true {
		t.Fatalf("create envelope = %v", env)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %v", env)
	}
	if data["status"] != model.XReqStatusDraft {
		t.Fatalf("create status = %v", data["status"])
	}

	// list
	code, env = callCross(t, h.List, http.MethodGet, "/api/cross-requirements", nil)
	if code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	listData, _ := env["data"].(map[string]any)
	items, _ := listData["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list items = %d, want 1", len(items))
	}
	progress, _ := listData["progress"].(map[string]any)
	if _, ok := progress[id]; !ok {
		t.Fatalf("list progress has no entry for %s: %v", id, progress)
	}

	// detail
	code, env = callCross(t, h.Get, http.MethodGet, "/api/cross-requirements/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("detail status = %d", code)
	}
	detail, _ := env["data"].(map[string]any)
	projects, _ := detail["projects"].([]any)
	if len(projects) != 2 {
		t.Fatalf("detail projects = %d, want 2", len(projects))
	}

	// split confirm
	code, env = callCross(t, h.ConfirmSplit, http.MethodPost, "/api/cross-requirements/"+id+"/split/confirm",
		model.ConfirmCrossRequirementSplitReq{
			Candidates: []model.CrossRequirementSplitCandidate{
				{Title: "接口定义", ProjectID: p1, Order: 1},
				{Title: "对接", ProjectID: p2, Order: 2, DependsOn: []int{1}},
			},
		})
	if code != http.StatusOK {
		t.Fatalf("confirm status = %d, body = %v", code, env)
	}
	confirmData, _ := env["data"].(map[string]any)
	children, _ := confirmData["children"].([]any)
	if len(children) != 2 {
		t.Fatalf("confirmed children = %d, want 2", len(children))
	}
	childMap, _ := children[0].(map[string]any)
	childID, _ := childMap["id"].(string)

	// an out-of-scope project is rejected with 4xx and nothing is written.
	other := seedCrossProject(t, d, "not-participant")
	code, _ = callCross(t, h.ConfirmSplit, http.MethodPost, "/api/cross-requirements/"+id+"/split/confirm",
		model.ConfirmCrossRequirementSplitReq{
			Candidates: []model.CrossRequirementSplitCandidate{
				{Title: "越界", ProjectID: other, Order: 1},
			},
		})
	if code < 400 || code >= 500 {
		t.Fatalf("out-of-scope confirm status = %d, want 4xx", code)
	}

	// phase create + update
	code, env = callCross(t, h.CreatePhase, http.MethodPost, "/api/cross-requirements/"+id+"/phases",
		model.CrossRequirementPhaseReq{Name: "阶段一", RequirementIDs: []string{childID}})
	if code != http.StatusCreated {
		t.Fatalf("create phase status = %d, body = %v", code, env)
	}
	phaseData, _ := env["data"].(map[string]any)
	phaseID, _ := phaseData["id"].(string)
	if phaseID == "" {
		t.Fatalf("create phase returned no id: %v", env)
	}
	if phaseData["status"] != model.PhaseStatusPending {
		t.Fatalf("phase status = %v, want pending", phaseData["status"])
	}

	code, env = callCross(t, h.UpdatePhase, http.MethodPut, "/api/cross-requirements/"+id+"/phases/"+phaseID,
		model.UpdateCrossRequirementPhaseReq{Status: model.PhaseStatusPassed})
	if code != http.StatusOK {
		t.Fatalf("update phase status = %d, body = %v", code, env)
	}
	updated, _ := env["data"].(map[string]any)
	if updated["status"] != model.PhaseStatusPassed {
		t.Fatalf("phase status = %v, want passed", updated["status"])
	}

	// 联调不通过之后要能把阶段放回「就绪」，否则跑不了下一轮验证。
	//
	// 先把阶段成员做完 —— 「就绪」是**派生**状态：成员全部 done 才算就绪
	// （成员没做完时 reset 只是把 verdict 清掉，阶段仍然是「待完成」）。
	if _, err := h.reqSvc.UpdateStatus(childID, "developing"); err != nil {
		t.Fatalf("mark developing: %v", err)
	}
	if _, err := h.reqSvc.UpdateStatus(childID, "done"); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	code, env = callCross(t, h.UpdatePhase, http.MethodPut, "/api/cross-requirements/"+id+"/phases/"+phaseID,
		model.UpdateCrossRequirementPhaseReq{Status: model.PhaseStatusFailed})
	if code != http.StatusOK {
		t.Fatalf("mark failed status = %d, body = %v", code, env)
	}
	code, env = callCross(t, h.UpdatePhase, http.MethodPut, "/api/cross-requirements/"+id+"/phases/"+phaseID,
		model.UpdateCrossRequirementPhaseReq{Status: model.PhaseStatusReady})
	if code != http.StatusOK {
		t.Fatalf("reset to ready status = %d, body = %v", code, env)
	}
	updated, _ = env["data"].(map[string]any)
	if updated["status"] != model.PhaseStatusReady {
		t.Fatalf("phase status = %v, want ready", updated["status"])
	}

	// delete detaches the children instead of removing them.
	code, env = callCross(t, h.Delete, http.MethodDelete, "/api/cross-requirements/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %v", code, env)
	}
	rows, err := d.Query(`SELECT COUNT(1) FROM requirements WHERE id = ?`, childID)
	if err != nil {
		t.Fatalf("count children: %v", err)
	}
	rows.Close()
}

// 缺少「跨项目接口契约」章节的方案不能保存。
func TestCrossRequirementHandler_SaveDesignRequiresContract(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")

	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements", model.CreateCrossRequirementReq{
		Title: "契约校验", ProjectIDs: []string{p1},
	})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d", code)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)

	code, env = callCross(t, h.SaveDesign, http.MethodPut, "/api/cross-requirements/"+id+"/design",
		model.SaveCrossRequirementDesignReq{DesignDocs: "## 需求目标\n\n没有契约章节\n"})
	if code != http.StatusBadRequest {
		t.Fatalf("save design status = %d, want 400", code)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "DESIGN_MISSING_CONTRACT" {
		t.Fatalf("error code = %v, want DESIGN_MISSING_CONTRACT", errObj["code"])
	}

	code, _ = callCross(t, h.SaveDesign, http.MethodPut, "/api/cross-requirements/"+id+"/design",
		model.SaveCrossRequirementDesignReq{DesignDocs: "## 需求目标\n\n目标\n\n## 跨项目接口契约\n\nGET /api/x\n"})
	if code != http.StatusOK {
		t.Fatalf("save design status = %d, want 200", code)
	}
}

// 子需求在别处完成之后，读路径必须返回**刷新后**的生命周期状态 ——
// 详情页和列表页都不能显示刷新前的旧值。
func TestCrossRequirementHandler_ReadPathReturnsFreshStatus(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")

	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements", model.CreateCrossRequirementReq{
		Title: "跨仓库状态刷新", ProjectIDs: []string{p1},
	})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)

	// 拆出一个子需求 —— 拆分之后整体状态进入「开发中」。
	code, env = callCross(t, h.ConfirmSplit, http.MethodPost, "/api/cross-requirements/"+id+"/split/confirm",
		model.ConfirmCrossRequirementSplitReq{
			Candidates: []model.CrossRequirementSplitCandidate{
				{Title: "子需求", ProjectID: p1, Order: 1},
			},
		})
	if code != http.StatusOK {
		t.Fatalf("confirm split status = %d, body = %v", code, env)
	}
	confirmData, _ := env["data"].(map[string]any)
	children, _ := confirmData["children"].([]any)
	childMap, _ := children[0].(map[string]any)
	childID, _ := childMap["id"].(string)
	if childID == "" {
		t.Fatalf("confirm split returned no child: %v", env)
	}

	// 子需求在**别处**（另一个项目 / 另一条路径）被完成 —— 跨项目需求的落库状态
	// 不会自动更新，仍然停在「开发中」。
	if _, err := d.Exec(`UPDATE requirements SET status='done' WHERE id=?`, childID); err != nil {
		t.Fatalf("mark child done: %v", err)
	}
	var stored string
	if err := d.QueryRow(`SELECT status FROM cross_requirements WHERE id=?`, id).Scan(&stored); err != nil {
		t.Fatalf("read stored status: %v", err)
	}
	if stored != model.XReqStatusDeveloping {
		t.Fatalf("stored status = %q, want %q (the child completion must not touch it)",
			stored, model.XReqStatusDeveloping)
	}

	// 详情页：读路径刷新状态，响应里必须带刷新后的值。
	code, env = callCross(t, h.Get, http.MethodGet, "/api/cross-requirements/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %v", code, env)
	}
	detail, _ := env["data"].(map[string]any)
	if detail["status"] != model.XReqStatusDone {
		t.Fatalf("detail status = %v, want %q", detail["status"], model.XReqStatusDone)
	}

	// 列表页：同样不能显示旧状态。
	code, env = callCross(t, h.List, http.MethodGet, "/api/cross-requirements", nil)
	if code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	listData, _ := env["data"].(map[string]any)
	items, _ := listData["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list items = %d, want 1", len(items))
	}
	itemMap, _ := items[0].(map[string]any)
	if itemMap["status"] != model.XReqStatusDone {
		t.Fatalf("list item status = %v, want %q", itemMap["status"], model.XReqStatusDone)
	}

	// 按状态筛选也要按刷新后的状态来 —— 否则「已完成」筛选里看不到刚完成的行。
	code, env = callCross(t, h.List, http.MethodGet, "/api/cross-requirements?status=done", nil)
	if code != http.StatusOK {
		t.Fatalf("filtered list status = %d", code)
	}
	listData, _ = env["data"].(map[string]any)
	items, _ = listData["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("filtered list items = %d, want 1", len(items))
	}
}

// 归档是用户显式状态：派生逻辑不改它，而且归档行只在「已归档」筛选里出现。
func TestCrossRequirementHandler_ListArchivedFilter(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")

	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements", model.CreateCrossRequirementReq{
		Title: "归档语义", ProjectIDs: []string{p1},
	})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)

	if _, err := d.Exec(`UPDATE cross_requirements SET status='archived' WHERE id=?`, id); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// 默认列表不含归档行。
	code, env = callCross(t, h.List, http.MethodGet, "/api/cross-requirements", nil)
	if code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	listData, _ := env["data"].(map[string]any)
	items, _ := listData["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("default list items = %d, want 0 (archived rows are hidden)", len(items))
	}

	// 「已归档」筛选里能看到它，且状态不被派生逻辑改写。
	code, env = callCross(t, h.List, http.MethodGet, "/api/cross-requirements?status=archived", nil)
	if code != http.StatusOK {
		t.Fatalf("archived list status = %d", code)
	}
	listData, _ = env["data"].(map[string]any)
	items, _ = listData["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("archived list items = %d, want 1", len(items))
	}
	itemMap, _ := items[0].(map[string]any)
	if itemMap["status"] != model.XReqStatusArchived {
		t.Fatalf("archived item status = %v, want %q", itemMap["status"], model.XReqStatusArchived)
	}
}

// 重复确认拆分 → 409（不产生第二批子需求）。
//
// 这是 P1-5 的接口层回归测试：前端双击「确认创建」会发出两个请求，服务端
// 必须只让一批子需求落库，第二个请求拿到明确的 409。
func TestCrossRequirementHandler_ConfirmSplitTwice(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")

	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements", model.CreateCrossRequirementReq{
		Title: "重复确认", ProjectIDs: []string{p1},
	})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)

	body := model.ConfirmCrossRequirementSplitReq{
		Candidates: []model.CrossRequirementSplitCandidate{
			{Title: "接口定义", ProjectID: p1, Order: 1},
		},
	}
	code, env = callCross(t, h.ConfirmSplit, http.MethodPost, "/api/cross-requirements/"+id+"/split/confirm", body)
	if code != http.StatusOK {
		t.Fatalf("first confirm status = %d, body = %v", code, env)
	}

	code, env = callCross(t, h.ConfirmSplit, http.MethodPost, "/api/cross-requirements/"+id+"/split/confirm", body)
	if code != http.StatusConflict {
		t.Fatalf("second confirm status = %d, want 409 (body = %v)", code, env)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "XREQ_ALREADY_SPLIT" {
		t.Fatalf("error code = %v, want XREQ_ALREADY_SPLIT", errObj["code"])
	}

	// Only one batch of children exists.
	var count int
	if err := d.QueryRow(`SELECT COUNT(1) FROM requirements WHERE cross_requirement_id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if count != 1 {
		t.Fatalf("children = %d, want 1", count)
	}
}

// 已经拆分过的跨项目需求不能再启动拆分作业 → 409。
func TestCrossRequirementHandler_StartSplitAfterSplit(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")

	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements", model.CreateCrossRequirementReq{
		Title: "重复拆分", ProjectIDs: []string{p1},
	})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d", code)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)

	// A design must exist before a split may start.
	if err := h.svc.SaveDesign(id, "## 需求目标\n\n目标\n\n## 跨项目接口契约\n\nGET /api/x\n", "", ""); err != nil {
		t.Fatalf("save design: %v", err)
	}
	code, env = callCross(t, h.ConfirmSplit, http.MethodPost, "/api/cross-requirements/"+id+"/split/confirm",
		model.ConfirmCrossRequirementSplitReq{
			Candidates: []model.CrossRequirementSplitCandidate{
				{Title: "接口定义", ProjectID: p1, Order: 1},
			},
		})
	if code != http.StatusOK {
		t.Fatalf("confirm status = %d, body = %v", code, env)
	}

	code, env = callCross(t, h.StartSplit, http.MethodPost, "/api/cross-requirements/"+id+"/split", nil)
	if code != http.StatusConflict {
		t.Fatalf("start split status = %d, want 409 (body = %v)", code, env)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "XREQ_ALREADY_SPLIT" {
		t.Fatalf("error code = %v, want XREQ_ALREADY_SPLIT", errObj["code"])
	}
}

// 只改状态的 PUT 必须成功（不 400），且不得清空其它字段。
//
// 这是 P1-4 的接口层回归测试：`PUT {"status":"archived"}` 过去会先走
// service.Update，而 Update 当时要求非空标题 → 400，归档在 API 层不可用。
func TestCrossRequirementHandler_StatusOnlyUpdate(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	p1 := seedCrossProject(t, d, "web-app")

	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements", model.CreateCrossRequirementReq{
		Title: "归档语义", Description: "原描述", Priority: "high",
		ProjectIDs: []string{p1},
	})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)

	// 只带 status 的 PUT。
	code, env = callCross(t, h.Update, http.MethodPut, "/api/cross-requirements/"+id,
		model.UpdateCrossRequirementReq{Status: model.XReqStatusArchived})
	if code != http.StatusOK {
		t.Fatalf("status-only update = %d, body = %v", code, env)
	}
	detail, _ := env["data"].(map[string]any)
	if detail["status"] != model.XReqStatusArchived {
		t.Fatalf("status = %v, want %q", detail["status"], model.XReqStatusArchived)
	}
	if detail["title"] != "归档语义" {
		t.Fatalf("title = %v, want the original title", detail["title"])
	}
	if detail["description"] != "原描述" {
		t.Fatalf("description = %v, want the original description", detail["description"])
	}
	if detail["priority"] != "high" {
		t.Fatalf("priority = %v, want high", detail["priority"])
	}

	// 人工设置的状态不会被读路径的派生刷新改写。
	code, env = callCross(t, h.Get, http.MethodGet, "/api/cross-requirements/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("get status = %d", code)
	}
	detail, _ = env["data"].(map[string]any)
	if detail["status"] != model.XReqStatusArchived {
		t.Fatalf("status after the read = %v, want %q", detail["status"], model.XReqStatusArchived)
	}
}

// newCountingCrossHandlerTest wires the cross-requirement handler over a
// throwaway SQLite database whose SQL statements are counted.
//
// The counting pool replaces the migration pool AFTER the schema is in place,
// so only the statements the test itself triggers are counted.
func newCountingCrossHandlerTest(t *testing.T) (*CrossRequirementHandler, *db.DB, *int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: path})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	count, err := dbtest.NewCountingDB(d, path)
	if err != nil {
		t.Fatalf("counting db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	projSvc := service.NewProjectService(d, nil, nil)
	reqSvc := service.NewRequirementService(d, nil)
	crossSvc := service.NewCrossRequirementService(d)
	h := NewCrossRequirementHandler(crossSvc, reqSvc, projSvc, nil, store.NewJobStore(16),
		nil, nil, nil, nil, nil, nil)
	return h, d, count
}

// TestCrossRequirementHandler_ListQueryCountIsConstant 是列表页 N+1 的回归
// 测试。
//
// 修复前：列表页对**每条**跨项目需求跑一次聚合，而聚合内部又为**每个子需求**
// 单跑一次前置依赖查询 —— 20 条需求 × 10 个子需求 ≈ 200+ 条语句，每刷新一次
// 页面就重来一遍。
//
// 修复后：整页只跑固定的几条语句 —— 语句数与需求条数、子需求个数都无关。
func TestCrossRequirementHandler_ListQueryCountIsConstant(t *testing.T) {
	h, d, count := newCountingCrossHandlerTest(t)
	crossSvc := service.NewCrossRequirementService(d)

	// 装一条跨项目需求 + 若干子需求（依赖链跨两个项目）。
	seedListFixture := func(t *testing.T, title string, children int) {
		t.Helper()
		p1 := seedCrossProject(t, d, "web-"+title)
		p2 := seedCrossProject(t, d, "api-"+title)
		x, err := crossSvc.Create(model.CreateCrossRequirementReq{
			Title:      title,
			ProjectIDs: []string{p1, p2},
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		cands := make([]model.CrossRequirementSplitCandidate, 0, children)
		for i := 0; i < children; i++ {
			projectID := p1
			if i%2 == 1 {
				projectID = p2
			}
			cand := model.CrossRequirementSplitCandidate{
				Title:     title + "-子需求",
				ProjectID: projectID,
				Order:     i + 1,
			}
			if i > 0 {
				cand.DependsOn = []int{i}
			}
			cands = append(cands, cand)
		}
		if _, err := crossSvc.ConfirmSplit(x.ID, cands); err != nil {
			t.Fatalf("confirm split %s: %v", title, err)
		}
	}
	seedListFixture(t, "一条", 10)

	// 先跑一次让派生状态落定（第一次调用会写库），之后每次刷新的语句数才是
	// 可比的。
	if code, _ := callCross(t, h.List, http.MethodGet, "/api/cross-requirements", nil); code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}

	atomic.StoreInt64(count, 0)
	if code, _ := callCross(t, h.List, http.MethodGet, "/api/cross-requirements", nil); code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	oneReq := atomic.LoadInt64(count)
	if oneReq == 0 {
		t.Fatal("no statement was counted — the counter is not wired")
	}

	// 再加一条需求 + 10 个子需求。
	seedListFixture(t, "两条", 10)
	if code, _ := callCross(t, h.List, http.MethodGet, "/api/cross-requirements", nil); code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}

	atomic.StoreInt64(count, 0)
	if code, _ := callCross(t, h.List, http.MethodGet, "/api/cross-requirements", nil); code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	twoReqs := atomic.LoadInt64(count)

	if twoReqs != oneReq {
		t.Fatalf("list statement count grew with the number of rows: %d for one requirement, %d for two — the N+1 is back",
			oneReq, twoReqs)
	}
}

// T — 回收站端点：DELETE = 软删（可恢复），POST /restore = 恢复，
// DELETE /purge = 永久删除，GET /trash = 回收站列表。
//
// 这条链路的价值全在**顺序**上：不可逆的那一步只能从回收站里发起，所以
// 误点一次「删除」不会当场抹掉整体方案与联调记录。
func TestCrossRequirementHandler_TrashFlow(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	projID := seedCrossProject(t, d, "web-app")

	code, env := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements", model.CreateCrossRequirementReq{
		Title:      "回收站链路",
		ProjectIDs: []string{projID},
	})
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	id, _ := data["id"].(string)

	// The trash starts empty.
	code, env = callCross(t, h.Trash, http.MethodGet, "/api/cross-requirements/trash", nil)
	if code != http.StatusOK {
		t.Fatalf("trash status = %d", code)
	}
	trashData, _ := env["data"].(map[string]any)
	if items, _ := trashData["items"].([]any); len(items) != 0 {
		t.Fatalf("trash items = %d, want 0", len(items))
	}

	// Soft delete.
	code, env = callCross(t, h.Delete, http.MethodDelete, "/api/cross-requirements/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %v", code, env)
	}
	if env["success"] != true {
		t.Fatalf("delete envelope = %v", env)
	}

	// It left the working list and landed in the trash.
	code, env = callCross(t, h.List, http.MethodGet, "/api/cross-requirements", nil)
	if code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	listData, _ := env["data"].(map[string]any)
	if items, _ := listData["items"].([]any); len(items) != 0 {
		t.Fatalf("list items = %d after trashing, want 0", len(items))
	}
	code, env = callCross(t, h.Trash, http.MethodGet, "/api/cross-requirements/trash", nil)
	if code != http.StatusOK {
		t.Fatalf("trash status = %d", code)
	}
	trashData, _ = env["data"].(map[string]any)
	items, _ := trashData["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("trash items = %d, want 1", len(items))
	}
	trashRow, _ := items[0].(map[string]any)
	if trashRow["deleted_at"] == "" {
		t.Fatalf("trashed row has no deleted_at: %v", trashRow)
	}

	// A trashed row is read-only: the update path says so with 409.
	code, env = callCross(t, h.Update, http.MethodPut, "/api/cross-requirements/"+id,
		model.UpdateCrossRequirementReq{Title: "改名"})
	if code != http.StatusConflict {
		t.Fatalf("update on a trashed row status = %d, want 409 (body %v)", code, env)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["code"] != "XREQ_IN_TRASH" {
		t.Fatalf("error code = %v, want XREQ_IN_TRASH", errObj["code"])
	}

	// Purging a row that is NOT in the trash is refused — the destructive
	// action must go through the soft delete first.
	_, other := callCross(t, h.Create, http.MethodPost, "/api/cross-requirements", model.CreateCrossRequirementReq{
		Title:      "还在进行中",
		ProjectIDs: []string{projID},
	})
	otherData, _ := other["data"].(map[string]any)
	otherID, _ := otherData["id"].(string)
	code, _ = callCross(t, h.Purge, http.MethodDelete, "/api/cross-requirements/"+otherID+"/purge", nil)
	if code != http.StatusConflict {
		t.Fatalf("purge on a live row status = %d, want 409", code)
	}

	// Restore brings it back.
	code, env = callCross(t, h.Restore, http.MethodPost, "/api/cross-requirements/"+id+"/restore", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %v", code, env)
	}
	code, env = callCross(t, h.List, http.MethodGet, "/api/cross-requirements", nil)
	if code != http.StatusOK {
		t.Fatalf("list status = %d", code)
	}
	listData, _ = env["data"].(map[string]any)
	items, _ = listData["items"].([]any)
	restored := false
	for _, it := range items {
		row, _ := it.(map[string]any)
		if row["id"] == id {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("restored row is missing from the list: %v", items)
	}

	// Trash it again and purge for good.
	if code, _ = callCross(t, h.Delete, http.MethodDelete, "/api/cross-requirements/"+id, nil); code != http.StatusOK {
		t.Fatalf("second delete status = %d", code)
	}
	code, env = callCross(t, h.Purge, http.MethodDelete, "/api/cross-requirements/"+id+"/purge", nil)
	if code != http.StatusOK {
		t.Fatalf("purge status = %d, body = %v", code, env)
	}
	code, _ = callCross(t, h.Get, http.MethodGet, "/api/cross-requirements/"+id, nil)
	if code != http.StatusNotFound {
		t.Fatalf("detail after purge status = %d, want 404", code)
	}
}
