package service

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// T2 — 拆分确认：3 个候选分别落 A/B/C 三个项目。
func TestCrossSplit_ConfirmCreatesPerProjectChildren(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
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

	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "接口定义", ProjectID: p1.ID, Order: 1},
		{Title: "订单服务", ProjectID: p2.ID, Order: 2},
		{Title: "客户端适配", ProjectID: p3.ID, Order: 3},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("children = %d, want 3", len(children))
	}

	wantProjects := []string{p1.ID, p2.ID, p3.ID}
	for i, c := range children {
		row, gErr := reqSvc.Get(c.ID)
		if gErr != nil {
			t.Fatalf("get child %d: %v", i, gErr)
		}
		if row.ProjectID != wantProjects[i] {
			t.Fatalf("child %d project = %q, want %q", i, row.ProjectID, wantProjects[i])
		}
		if row.CrossRequirementID != x.ID {
			t.Fatalf("child %d cross_requirement_id = %q, want %q", i, row.CrossRequirementID, x.ID)
		}
		// The children are PEERS — there is no master row, so parent_id stays
		// empty (the hierarchy edge means "same project, one level").
		if row.ParentID != "" {
			t.Fatalf("child %d parent_id = %q, want empty", i, row.ParentID)
		}
		if row.SplitOrder != i+1 {
			t.Fatalf("child %d split_order = %d, want %d", i, row.SplitOrder, i+1)
		}
	}
}

// T3 — 候选项目不在参与集合内：整批零写入。
func TestCrossSplit_RejectsOutOfScopeProject(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	other := seedCrossProject(t, projSvc, "not-participant")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "越界校验",
		ProjectIDs: []string{p1.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err = svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "接口定义", ProjectID: p1.ID, Order: 1},
		{Title: "越界子需求", ProjectID: other.ID, Order: 2},
	})
	if err == nil {
		t.Fatal("out-of-scope project accepted, want an error")
	}
	if !strings.Contains(err.Error(), "not part of the cross requirement") {
		t.Fatalf("error = %v, want the out-of-scope error", err)
	}

	// Nothing was written — the batch is atomic.
	children, err := svc.ListChildren(x.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("children = %d, want 0 (the batch must roll back entirely)", len(children))
	}
}

// T4 — 跨项目依赖边写入并被 UnmetDependencies 看到。
func TestCrossSplit_CrossProjectDependency(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "跨项目前置",
		ProjectIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "接口定义", ProjectID: p1.ID, Order: 1},
		{Title: "对接", ProjectID: p2.ID, Order: 2, DependsOn: []int{1}},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}

	// The dependent (project B) waits for the prerequisite (project A).
	unmet, err := reqSvc.UnmetDependencies(children[1].ID)
	if err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if len(unmet) != 1 || unmet[0].ID != children[0].ID {
		t.Fatalf("unmet = %+v, want the project-A prerequisite", unmet)
	}
	// The prerequisite itself has no unmet prerequisites.
	unmet, err = reqSvc.UnmetDependencies(children[0].ID)
	if err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if len(unmet) != 0 {
		t.Fatalf("unmet = %+v, want none", unmet)
	}

	// The dependency shows up in the cross-requirement view, flagged as a
	// cross-project edge.
	deps, err := svc.ListDependencies(x.ID)
	if err != nil {
		t.Fatalf("list dependencies: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("dependencies = %d, want 1", len(deps))
	}
	if !deps[0].CrossProject {
		t.Fatal("dependency not flagged as cross-project")
	}
	if deps[0].Met {
		t.Fatal("dependency reported as met although the prerequisite is not done")
	}

	// The dependent child is reported as blocked.
	listed, err := svc.ListChildren(x.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(listed[1].BlockedBy) != 1 || listed[1].BlockedBy[0] != children[0].ID {
		t.Fatalf("blocked by = %+v, want the project-A prerequisite", listed[1].BlockedBy)
	}
}

// T5 — 依赖成环：整批失败，无残留行。
func TestCrossSplit_RejectsDependencyCycle(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "成环校验",
		ProjectIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A depends on B and B depends on A.
	_, err = svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "A", ProjectID: p1.ID, Order: 1, DependsOn: []int{2}},
		{Title: "B", ProjectID: p2.ID, Order: 2, DependsOn: []int{1}},
	})
	if err == nil {
		t.Fatal("cyclic dependency accepted, want an error")
	}
	children, err := svc.ListChildren(x.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("children = %d, want 0 (the batch must roll back entirely)", len(children))
	}
}

// T13 — 标题 / 描述里的引号不破坏 SQL（走占位符）。
func TestCrossSplit_QuoteInTitleIsSafe(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "引号校验",
		ProjectIDs: []string{p1.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "O'Brien's 接口", Description: "it's a test; DROP TABLE requirements;--",
			ProjectID: p1.ID, Order: 1},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	row, err := reqSvc.Get(children[0].ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if row.Title != "O'Brien's 接口" {
		t.Fatalf("title = %q", row.Title)
	}
	if !strings.Contains(row.Description, "DROP TABLE requirements") {
		t.Fatalf("description = %q, want the original text", row.Description)
	}
}

// 子需求的正文必须自包含：跨项目契约写进了 description（子需求的执行 Agent
// 看不到父实体的方案）。
func TestCrossSplit_ChildDescriptionCarriesContract(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "契约持久化",
		ProjectIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	design := "## 需求目标\n\n统一订单模型\n\n## 跨项目接口契约\n\nGET /api/v1/orders/{id} 返回 {id, status, amount}\n"
	if err := svc.SaveDesign(x.ID, design, "", ""); err != nil {
		t.Fatalf("save design: %v", err)
	}

	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "订单服务", ProjectID: p2.ID, Order: 1},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	row, err := reqSvc.Get(children[0].ID)
	if err != nil {
		t.Fatalf("get child: %v", err)
	}
	if !strings.Contains(row.Description, "GET /api/v1/orders/{id}") {
		t.Fatalf("description does not carry the contract: %q", row.Description)
	}
	if !strings.Contains(row.Description, "跨项目上下文") {
		t.Fatalf("description does not carry the cross-project block: %q", row.Description)
	}
}
