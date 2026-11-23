package service

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// ── helpers ────────────────────────────────────────────────────────────────

// seedCrossProject inserts a project row the cross-requirement mapping can
// point at.
//
// The row is written directly instead of going through ProjectService.Add —
// that path insists on a real git checkout (it runs `git init` / clones), and
// the cross-requirement feature never touches the checkout in these tests.
func seedCrossProject(t *testing.T, svc *ProjectService, name string) *model.Project {
	t.Helper()
	id := util.NewID("proj")
	now := time.Now()
	_, err := svc.db.Exec(
		`INSERT INTO projects (id,name,local_path,status,added_at,updated_at) VALUES (?,?,?, 'active', ?, ?)`,
		id, name, filepath.Join(t.TempDir(), name), now, now)
	if err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
	proj, err := svc.Get(id)
	if err != nil {
		t.Fatalf("read project %s: %v", name, err)
	}
	return proj
}

// markDone drives a requirement to the "done" state through the real status
// machine (draft → developing → done).
func markDone(t *testing.T, svc *RequirementService, id string) {
	t.Helper()
	if _, err := svc.UpdateStatus(id, "developing"); err != nil {
		t.Fatalf("mark developing %s: %v", id, err)
	}
	if _, err := svc.UpdateStatus(id, "done"); err != nil {
		t.Fatalf("mark done %s: %v", id, err)
	}
}

// newCrossFixture builds a service trio over a throwaway database.
func newCrossFixture(t *testing.T) (*CrossRequirementService, *RequirementService, *ProjectService) {
	t.Helper()
	database := newServiceTestDB(t)
	projSvc := NewProjectService(database, nil, nil)
	reqSvc := NewRequirementService(database, nil)
	return NewCrossRequirementService(database), reqSvc, projSvc
}

// T1 — 创建 XReq 并设置 3 个参与项目。
func TestCrossRequirement_CreateWithProjects(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")
	p3 := seedCrossProject(t, projSvc, "mobile")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "跨仓库订单重构",
		ProjectIDs: []string{p1.ID, p2.ID, p3.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if x.Status != model.XReqStatusDraft {
		t.Fatalf("status = %q, want draft", x.Status)
	}

	projects, err := svc.ListProjects(x.ID)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 3 {
		t.Fatalf("projects = %d, want 3", len(projects))
	}
	if projects[0].ProjectID != p1.ID || projects[2].ProjectID != p3.ID {
		t.Fatalf("project order not preserved: %+v", projects)
	}
	// The display name is resolved on read.
	if projects[0].ProjectName != "web-app" {
		t.Fatalf("project name = %q, want web-app", projects[0].ProjectName)
	}

	// 重复添加同一项目被拒（UNIQUE 约束 / 服务层去重）。
	if err := svc.SetProjects(x.ID, []model.CrossRequirementProjectInput{
		{ProjectID: p1.ID}, {ProjectID: p1.ID},
	}); err == nil {
		t.Fatal("duplicate project accepted, want an error")
	}
}

// T1b — 没有参与项目的跨项目需求不被接受。
func TestCrossRequirement_CreateWithoutProjects(t *testing.T) {
	svc, _, _ := newCrossFixture(t)
	if _, err := svc.Create(model.CreateCrossRequirementReq{Title: "无项目"}); err == nil {
		t.Fatal("create without projects accepted, want an error")
	}
}

// T7 — 进度聚合。
func TestCrossRequirement_Progress(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "统一登录",
		ProjectIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "接口定义", ProjectID: p1.ID, Order: 1},
		{Title: "订单服务", ProjectID: p2.ID, Order: 2},
		{Title: "客户端适配", ProjectID: p2.ID, Order: 3},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("children = %d, want 3", len(children))
	}

	progress, err := svc.Progress(x.ID)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if progress.Total != 3 || progress.Done != 0 || progress.Percent != 0 {
		t.Fatalf("progress = %+v, want total=3 done=0 percent=0", progress)
	}
	if len(progress.Projects) != 2 {
		t.Fatalf("progress projects = %d, want 2", len(progress.Projects))
	}
	for _, p := range progress.Projects {
		switch p.ProjectID {
		case p1.ID:
			if p.Total != 1 {
				t.Fatalf("p1 total = %d, want 1", p.Total)
			}
		case p2.ID:
			if p.Total != 2 {
				t.Fatalf("p2 total = %d, want 2", p.Total)
			}
		default:
			t.Fatalf("unexpected project %s", p.ProjectID)
		}
	}

	// 完成其中一个子需求 → 进度实时反映。
	row, err := reqSvc.Get(children[0].ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	markDone(t, reqSvc, row.ID)
	progress, err = svc.Progress(x.ID)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if progress.Done != 1 || progress.Percent != 33 {
		t.Fatalf("progress = %+v, want done=1 percent=33", progress)
	}
}

// T8 — 阶段就绪派生：成员全部 done → ready；否则 pending；passed 不被覆盖。
func TestCrossRequirement_PhaseReadiness(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "接口打通",
		ProjectIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "接口定义", ProjectID: p1.ID, Order: 1},
		{Title: "订单服务", ProjectID: p2.ID, Order: 2},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}

	phase, err := svc.CreatePhase(x.ID, model.CrossRequirementPhaseReq{
		Name:           "阶段一：接口打通",
		RequirementIDs: []string{children[0].ID, children[1].ID},
	})
	if err != nil {
		t.Fatalf("create phase: %v", err)
	}
	if phase.Status != model.PhaseStatusPending {
		t.Fatalf("phase status = %q, want pending", phase.Status)
	}

	for _, c := range children {
		markDone(t, reqSvc, c.ID)
	}
	phase, err = svc.GetPhase(x.ID, phase.ID)
	if err != nil {
		t.Fatalf("get phase: %v", err)
	}
	if phase.Status != model.PhaseStatusReady {
		t.Fatalf("phase status = %q, want ready", phase.Status)
	}

	// 人工判定通过后，刷新不能把它改回 pending/ready。
	if _, err := svc.UpdatePhase(x.ID, phase.ID, model.UpdateCrossRequirementPhaseReq{
		Status: model.PhaseStatusPassed,
	}); err != nil {
		t.Fatalf("update phase: %v", err)
	}
	phase, err = svc.GetPhase(x.ID, phase.ID)
	if err != nil {
		t.Fatalf("get phase: %v", err)
	}
	if phase.Status != model.PhaseStatusPassed {
		t.Fatalf("phase status = %q, want passed", phase.Status)
	}
}

// T9 — 删除 XReq：子需求保留且 cross_requirement_id 置空。
func TestCrossRequirement_DeleteDetachesChildren(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "待删除",
		ProjectIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "接口定义", ProjectID: p1.ID, Order: 1},
		{Title: "订单服务", ProjectID: p2.ID, Order: 2},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	if _, err := svc.CreatePhase(x.ID, model.CrossRequirementPhaseReq{
		Name:           "阶段一",
		RequirementIDs: []string{children[0].ID},
	}); err != nil {
		t.Fatalf("create phase: %v", err)
	}

	if err := svc.Delete(x.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The children survive — only the link is severed.
	for _, c := range children {
		row, gErr := reqSvc.Get(c.ID)
		if gErr != nil {
			t.Fatalf("child %s must survive the delete: %v", c.ID, gErr)
		}
		if row.CrossRequirementID != "" {
			t.Fatalf("child %s still linked to %q", c.ID, row.CrossRequirementID)
		}
	}
	// The mapping / phases / phase items are gone.
	projects, err := svc.ListProjects(x.ID)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects = %d, want 0", len(projects))
	}
	phases, err := svc.ListPhases(x.ID)
	if err != nil {
		t.Fatalf("list phases: %v", err)
	}
	if len(phases) != 0 {
		t.Fatalf("phases = %d, want 0", len(phases))
	}
}

// T10 — 删除子需求时清理阶段成员行。
func TestCrossRequirement_DeleteChildCleansPhaseItems(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "阶段清理",
		ProjectIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "接口定义", ProjectID: p1.ID, Order: 1},
		{Title: "订单服务", ProjectID: p2.ID, Order: 2},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	phase, err := svc.CreatePhase(x.ID, model.CrossRequirementPhaseReq{
		Name:           "阶段一",
		RequirementIDs: []string{children[0].ID, children[1].ID},
	})
	if err != nil {
		t.Fatalf("create phase: %v", err)
	}

	if err := reqSvc.Delete(children[0].ID); err != nil {
		t.Fatalf("delete child: %v", err)
	}
	phase, err = svc.GetPhase(x.ID, phase.ID)
	if err != nil {
		t.Fatalf("get phase: %v", err)
	}
	if len(phase.Items) != 1 {
		t.Fatalf("phase items = %d, want 1", len(phase.Items))
	}
	if phase.Items[0].RequirementID != children[1].ID {
		t.Fatalf("phase item = %q, want %q", phase.Items[0].RequirementID, children[1].ID)
	}
}

// T11 — 删除项目（purge）时清理 cross_requirement_projects。
func TestCrossRequirement_PurgeProjectCleansMapping(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "项目清理",
		ProjectIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Purge only accepts a project that sits in the trash.
	if err := projSvc.Remove(p1.ID, false); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if err := projSvc.Purge(p1.ID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	projects, err := svc.ListProjects(x.ID)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(projects))
	}
	if projects[0].ProjectID != p2.ID {
		t.Fatalf("remaining project = %q, want %q", projects[0].ProjectID, p2.ID)
	}
}

// 阶段成员必须属于本跨项目需求。
func TestCrossRequirement_PhaseRejectsForeignChild(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x1, err := svc.Create(model.CreateCrossRequirementReq{Title: "A", ProjectIDs: []string{p1.ID}})
	if err != nil {
		t.Fatalf("create x1: %v", err)
	}
	x2, err := svc.Create(model.CreateCrossRequirementReq{Title: "B", ProjectIDs: []string{p2.ID}})
	if err != nil {
		t.Fatalf("create x2: %v", err)
	}
	foreign, err := svc.ConfirmSplit(x2.ID, []model.CrossRequirementSplitCandidate{
		{Title: "别的需求的子需求", ProjectID: p2.ID, Order: 1},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	if _, err := svc.CreatePhase(x1.ID, model.CrossRequirementPhaseReq{
		Name:           "阶段一",
		RequirementIDs: []string{foreign[0].ID},
	}); err == nil {
		t.Fatal("foreign child accepted as a phase member, want an error")
	}
}

// 整体方案保存后，状态推进到「已设计」。
func TestCrossRequirement_SaveDesignMovesStatus(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	x, err := svc.Create(model.CreateCrossRequirementReq{Title: "方案", ProjectIDs: []string{p1.ID}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.SaveDesign(x.ID, "## 需求目标\n\n整体方案\n\n## 跨项目接口契约\n\nGET /api/x\n", "", ""); err != nil {
		t.Fatalf("save design: %v", err)
	}
	if err := svc.RefreshDerivedStatus(x.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	row, err := svc.Get(x.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.Status != model.XReqStatusDesigned {
		t.Fatalf("status = %q, want designed", row.Status)
	}
	md, err := svc.DesignMarkdown(x.ID)
	if err != nil {
		t.Fatalf("markdown: %v", err)
	}
	if !strings.Contains(md, "跨项目接口契约") {
		t.Fatalf("markdown lost the contract section: %q", md)
	}
}

// RefreshDerivedStatus 必须是幂等的 —— 状态没变时不产生写。
func TestCrossRequirement_RefreshIsIdempotent(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	x, err := svc.Create(model.CreateCrossRequirementReq{Title: "幂等", ProjectIDs: []string{p1.ID}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := svc.Get(x.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := svc.RefreshDerivedStatus(x.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	after, err := svc.Get(x.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("refresh wrote a row although the status did not change (%q → %q)",
			before.UpdatedAt, after.UpdatedAt)
	}
}
