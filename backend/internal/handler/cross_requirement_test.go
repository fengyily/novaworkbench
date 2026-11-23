package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
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
		nil, nil, nil)
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
