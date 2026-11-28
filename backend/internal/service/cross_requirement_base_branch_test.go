package service

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// 「参与项目 → 基础分支」的落库与继承。
//
// 跨项目需求里各仓库常处在不同的发布线上（A 在 develop、B 在
// release/2026q1），所以基础分支必须**逐项目**指定，并且拆分时落到该项目的
// 子需求上。

// T — 创建时逐项目指定基础分支。
func TestCrossRequirement_CreateWithPerProjectBaseBranch(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title: "跨仓库订单重构",
		Projects: []model.CrossRequirementProjectInput{
			{ProjectID: p1.ID, Role: "前端", BaseBranch: "develop"},
			{ProjectID: p2.ID, Role: "后端", BaseBranch: "release/2026q1"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	projects, err := svc.ListProjects(x.ID)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("projects = %d, want 2", len(projects))
	}
	want := map[string]string{p1.ID: "develop", p2.ID: "release/2026q1"}
	for _, p := range projects {
		if p.BaseBranch != want[p.ProjectID] {
			t.Fatalf("project %s base_branch = %q, want %q", p.ProjectID, p.BaseBranch, want[p.ProjectID])
		}
	}
}

// T — 非法基础分支名被拒（它会进 git 命令，脏值必须挡住）。
func TestCrossRequirement_RejectsInvalidBaseBranch(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")

	bad := []string{"has space", "has~tilde", "..", "-leading", "trailing/", "x.lock"}
	for _, name := range bad {
		if _, err := svc.Create(model.CreateCrossRequirementReq{
			Title:    "非法基础分支",
			Projects: []model.CrossRequirementProjectInput{{ProjectID: p1.ID, BaseBranch: name}},
		}); err == nil {
			t.Fatalf("base branch %q was accepted", name)
		}
	}

	// 合法值通过。
	if _, err := svc.Create(model.CreateCrossRequirementReq{
		Title:    "合法基础分支",
		Projects: []model.CrossRequirementProjectInput{{ProjectID: p1.ID, BaseBranch: "release/2026q1"}},
	}); err != nil {
		t.Fatalf("valid base branch rejected: %v", err)
	}
}

// T — SetProjects 换掉基础分支后，已经拆出来的子需求跟着换。
func TestCrossRequirement_SetProjectsUpdatesInheritedChildBase(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:    "换基础分支",
		Projects: []model.CrossRequirementProjectInput{{ProjectID: p1.ID, BaseBranch: "develop"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "前端改造", ProjectID: p1.ID, Order: 1},
	}); err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	children, err := svc.ListChildren(x.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	child, gErr := reqSvc.Get(children[0].ID)
	if gErr != nil {
		t.Fatalf("get child: %v", gErr)
	}
	if child.BaseBranch != "develop" {
		t.Fatalf("child base_branch = %q, want develop", child.BaseBranch)
	}
	if child.BaseBranchSource != model.BaseSourceCrossProject {
		t.Fatalf("child base_branch_source = %q, want %q", child.BaseBranchSource, model.BaseSourceCrossProject)
	}

	// 换基础分支 → 子需求跟着换。
	if err := svc.SetProjects(x.ID, []model.CrossRequirementProjectInput{
		{ProjectID: p1.ID, BaseBranch: "release/2026q1"},
	}); err != nil {
		t.Fatalf("set projects: %v", err)
	}
	child, gErr = reqSvc.Get(children[0].ID)
	if gErr != nil {
		t.Fatalf("get child: %v", gErr)
	}
	if child.BaseBranch != "release/2026q1" {
		t.Fatalf("child base_branch = %q, want release/2026q1", child.BaseBranch)
	}
}

// T — 用户在子需求上自己选过基础分支的，不被跨项目的基础分支覆盖。
func TestCrossRequirement_SetProjectsKeepsExplicitChildBase(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:    "显式选择优先",
		Projects: []model.CrossRequirementProjectInput{{ProjectID: p1.ID, BaseBranch: "develop"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "前端改造", ProjectID: p1.ID, Order: 1},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	// 用户在子需求上显式选了基础分支。
	if err := reqSvc.UpdateBaseBranchWithSource(children[0].ID, "hotfix", model.BaseSourceExplicit); err != nil {
		t.Fatalf("stamp explicit base: %v", err)
	}

	if err := svc.SetProjects(x.ID, []model.CrossRequirementProjectInput{
		{ProjectID: p1.ID, BaseBranch: "release/2026q1"},
	}); err != nil {
		t.Fatalf("set projects: %v", err)
	}
	child, gErr := reqSvc.Get(children[0].ID)
	if gErr != nil {
		t.Fatalf("get child: %v", gErr)
	}
	if child.BaseBranch != "hotfix" {
		t.Fatalf("child base_branch = %q, want hotfix (explicit wins)", child.BaseBranch)
	}
	if child.BaseBranchSource != model.BaseSourceExplicit {
		t.Fatalf("child base_branch_source = %q, want %q", child.BaseBranchSource, model.BaseSourceExplicit)
	}
}

// T — 没指定基础分支的项目：子需求的 base 留空，解析链落到项目默认分支。
func TestCrossRequirement_ChildWithoutBaseBranchFallsBackToProjectDefault(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "默认基础分支",
		ProjectIDs: []string{p1.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "前端改造", ProjectID: p1.ID, Order: 1},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	child, gErr := reqSvc.Get(children[0].ID)
	if gErr != nil {
		t.Fatalf("get child: %v", gErr)
	}
	if child.BaseBranch != "" || child.BaseBranchSource != "" {
		t.Fatalf("child base = %q/%q, want empty/empty", child.BaseBranch, child.BaseBranchSource)
	}
	base, source := ResolveAutoBase(child, "main", "")
	if base != "main" || source != model.BaseSourceProject {
		t.Fatalf("resolved base = %q/%q, want main/%q", base, source, model.BaseSourceProject)
	}
}

// T — 解析链：跨项目继承的基础分支压过项目默认分支，但让位给用户显式选择。
func TestResolveAutoBase_CrossProjectSource(t *testing.T) {
	req := &model.Requirement{
		BaseBranch:       "release/2026q1",
		BaseBranchSource: model.BaseSourceCrossProject,
	}
	base, source := ResolveAutoBase(req, "main", "")
	if base != "release/2026q1" || source != model.BaseSourceCrossProject {
		t.Fatalf("base = %q/%q, want release/2026q1/%q", base, source, model.BaseSourceCrossProject)
	}

	// 用户显式选过的 —— 优先。
	req.BaseBranchSource = model.BaseSourceExplicit
	req.BaseBranch = "hotfix"
	base, source = ResolveAutoBase(req, "main", "")
	if base != "hotfix" || source != model.BaseSourceExplicit {
		t.Fatalf("base = %q/%q, want hotfix/%q", base, source, model.BaseSourceExplicit)
	}
}

// T — 拆分后不同项目的子需求拿到各自的基础分支。
func TestCrossSplit_PerProjectBaseBranchLandsOnChildren(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")
	p2 := seedCrossProject(t, projSvc, "api-svc")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title: "逐项目基础分支",
		Projects: []model.CrossRequirementProjectInput{
			{ProjectID: p1.ID, BaseBranch: "develop"},
			{ProjectID: p2.ID, BaseBranch: "release/2026q1"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "前端改造", ProjectID: p1.ID, Order: 1},
		{Title: "订单服务", ProjectID: p2.ID, Order: 2},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	want := map[string]string{p1.ID: "develop", p2.ID: "release/2026q1"}
	for _, c := range children {
		row, gErr := reqSvc.Get(c.ID)
		if gErr != nil {
			t.Fatalf("get child %s: %v", c.ID, gErr)
		}
		if row.BaseBranch != want[row.ProjectID] {
			t.Fatalf("child %s base_branch = %q, want %q", c.ID, row.BaseBranch, want[row.ProjectID])
		}
	}
}

// T — 详情载荷带上基础分支（UI 的项目矩阵要显示它）。
func TestCrossRequirement_DetailCarriesBaseBranch(t *testing.T) {
	svc, _, projSvc := newCrossFixture(t)
	p1 := seedCrossProject(t, projSvc, "web-app")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:    "详情带基础分支",
		Projects: []model.CrossRequirementProjectInput{{ProjectID: p1.ID, BaseBranch: "develop"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	detail, err := svc.Detail(x.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.Projects) != 1 {
		t.Fatalf("detail projects = %d, want 1", len(detail.Projects))
	}
	if detail.Projects[0].BaseBranch != "develop" {
		t.Fatalf("detail base_branch = %q, want develop", detail.Projects[0].BaseBranch)
	}
	if !strings.Contains(detail.Projects[0].ProjectName, "web-app") {
		t.Fatalf("detail project name = %q", detail.Projects[0].ProjectName)
	}
}
