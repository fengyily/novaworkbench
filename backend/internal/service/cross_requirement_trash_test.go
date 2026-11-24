package service

import (
	"errors"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// 跨项目需求「回收站」的语义测试。
//
// 删除是**两步**的：移进回收站（软删，deleted_at 打戳）→ 彻底删除（purge，
// 行与关联一起消失）。这两步之间的状态是本文件的主题：
//
//   - 回收站里的行**只读** —— 写路径一律被拒；
//   - 回收站里的行**状态冻结** —— 读路径不再重算它；
//   - 恢复**不是回滚** —— 恢复后状态按当前事实重算。
//
// 为什么要这么绕：跨项目需求被拆过之后，整体方案 / 联调阶段 / 跨项目依赖是
// **不可再生产**的东西。子需求里的代码还在，但「三个仓库怎么对齐」的上下文
// 丢了就是丢了 —— 所以删除必须可恢复，而恢复必须给出**当下**的正确状态，
// 而不是把一条过时的快照倒回列表。

// helperCrossWithChild builds a cross requirement with one sub-requirement.
//
// 它**不**把行移进回收站 —— 回收站是每个测试自己的一步，这样每个测试都能
// 在「进回收站之前」先摆好它要检查的前置状态。
func helperCrossWithChild(t *testing.T) (*CrossRequirementService, *RequirementService, *ProjectService, *model.CrossRequirement, []model.CrossRequirementChild) {
	t.Helper()
	svc, reqSvc, projSvc := newCrossFixture(t)
	proj := seedCrossProject(t, projSvc, "web-app")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "待回收",
		ProjectIDs: []string{proj.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "接口定义", ProjectID: proj.ID, Order: 1},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}
	return svc, reqSvc, projSvc, x, children
}

// T — 移进回收站：行从「进行中」列表消失，出现在回收站列表里，子需求的关联
// 原样保留（恢复要能零成本接回去）。
func TestCrossRequirement_MoveToTrashHidesFromList(t *testing.T) {
	svc, reqSvc, _, x, children := helperCrossWithChild(t)
	if err := svc.MoveToTrash(x.ID); err != nil {
		t.Fatalf("move to trash: %v", err)
	}

	items, err := svc.List("", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, item := range items {
		if item.ID == x.ID {
			t.Fatal("trashed row still shows up in the working list")
		}
	}

	trash, err := svc.ListTrash()
	if err != nil {
		t.Fatalf("list trash: %v", err)
	}
	if len(trash) != 1 || trash[0].ID != x.ID {
		t.Fatalf("trash = %d rows, want the trashed one", len(trash))
	}
	if trash[0].DeletedAt == "" {
		t.Fatal("trashed row has no deleted_at stamp")
	}

	// The link survives — that is what makes the restore cheap.
	for _, c := range children {
		row, gErr := reqSvc.Get(c.ID)
		if gErr != nil {
			t.Fatalf("child %s must survive: %v", c.ID, gErr)
		}
		if row.CrossRequirementID != x.ID {
			t.Fatalf("child %s link = %q, want %q", c.ID, row.CrossRequirementID, x.ID)
		}
	}

	// The row is still readable — the detail page shows the trash banner.
	got, err := svc.Get(x.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DeletedAt == "" {
		t.Fatal("get returned a row without the trash stamp")
	}
}

// T — 恢复：行回到「进行中」列表，阶段 / 参与项目 / 子需求关联全都还在。
func TestCrossRequirement_RestoreBringsBackEverything(t *testing.T) {
	svc, _, _, x, _ := helperCrossWithChild(t)

	if _, err := svc.CreatePhase(x.ID, model.CrossRequirementPhaseReq{
		Name:           "阶段一",
		RequirementIDs: nil,
	}); err != nil {
		t.Fatalf("create phase: %v", err)
	}
	if err := svc.MoveToTrash(x.ID); err != nil {
		t.Fatalf("move to trash: %v", err)
	}

	if err := svc.Restore(x.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}

	items, err := svc.List("", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, item := range items {
		if item.ID == x.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("restored row is still missing from the working list")
	}

	trash, err := svc.ListTrash()
	if err != nil {
		t.Fatalf("list trash: %v", err)
	}
	if len(trash) != 0 {
		t.Fatalf("trash = %d rows after restore, want 0", len(trash))
	}

	projects, err := svc.ListProjects(x.ID)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %d after restore, want 1", len(projects))
	}
	phases, err := svc.ListPhases(x.ID)
	if err != nil {
		t.Fatalf("list phases: %v", err)
	}
	if len(phases) != 1 {
		t.Fatalf("phases = %d after restore, want 1", len(phases))
	}
}

// T — 回收站里的行只读：所有写路径一律被拒。
//
// 少了这道闸门，用户会在垃圾堆里干活，而作业跑完会把结果写回一条已经被
// 「删掉」的行 —— 状态从此错乱。
func TestCrossRequirement_TrashedRowIsReadOnly(t *testing.T) {
	svc, _, projSvc, x, children := helperCrossWithChild(t)
	if err := svc.MoveToTrash(x.ID); err != nil {
		t.Fatalf("move to trash: %v", err)
	}
	proj := seedCrossProject(t, projSvc, "api-svc")

	checks := []struct {
		name string
		run  func() error
	}{
		{"update", func() error {
			return svc.Update(x.ID, model.UpdateCrossRequirementReq{Title: "改名"})
		}},
		{"set status", func() error {
			return svc.SetStatus(x.ID, model.XReqStatusDone)
		}},
		{"set projects", func() error {
			return svc.SetProjects(x.ID, []model.CrossRequirementProjectInput{{ProjectID: proj.ID}})
		}},
		{"save design", func() error {
			return svc.SaveDesign(x.ID, "# 方案", "", "")
		}},
		{"create phase", func() error {
			_, err := svc.CreatePhase(x.ID, model.CrossRequirementPhaseReq{Name: "阶段二"})
			return err
		}},
		{"update phase", func() error {
			_, err := svc.UpdatePhase(x.ID, "xph_missing", model.UpdateCrossRequirementPhaseReq{Name: "改名"})
			return err
		}},
		{"delete phase", func() error {
			return svc.DeletePhase(x.ID, "xph_missing")
		}},
		{"confirm split", func() error {
			_, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
				{Title: "新任务", ProjectID: proj.ID, Order: 1},
			})
			return err
		}},
	}
	for _, c := range checks {
		err := c.run()
		if !errors.Is(err, ErrCrossRequirementInTrash) {
			t.Fatalf("%s on a trashed row: err = %v, want ErrCrossRequirementInTrash", c.name, err)
		}
	}

	// The children are untouched by the rejected writes.
	for _, c := range children {
		var crossID string
		if err := svc.db.QueryRow(`SELECT cross_requirement_id FROM requirements WHERE id = ?`, c.ID).Scan(&crossID); err != nil {
			t.Fatalf("read child %s: %v", c.ID, err)
		}
		if crossID != x.ID {
			t.Fatalf("child %s link = %q, want %q", c.ID, crossID, x.ID)
		}
	}
}

// T — 彻底删除只对回收站里的行开放（NOT_IN_TRASH 闸门）。
//
// 误点在「进行中」的行上点「彻底删除」时，应该被挡下来并引导用户先走软删。
func TestCrossRequirement_PurgeRequiresTrash(t *testing.T) {
	svc, _, projSvc, _, _ := helperCrossWithChild(t)

	live, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "还在进行中",
		ProjectIDs: []string{seedCrossProject(t, projSvc, "web-app").ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Purge(live.ID); !errors.Is(err, ErrCrossRequirementNotInTrash) {
		t.Fatalf("purge on a live row: err = %v, want ErrCrossRequirementNotInTrash", err)
	}
	// The row is still there.
	if _, err := svc.Get(live.ID); err != nil {
		t.Fatalf("live row must survive a rejected purge: %v", err)
	}
}

// T — 恢复后状态按**当前事实**重算，而不是回到被删那一刻的值。
//
// 子需求在跨项目需求躺在回收站期间被做完了 —— 恢复回来时它必须显示「已完成」，
// 否则用户看到的是一条过时的快照。
func TestCrossRequirement_RestoreRecomputesDerivedStatus(t *testing.T) {
	svc, reqSvc, projSvc := newCrossFixture(t)
	proj := seedCrossProject(t, projSvc, "web-app")

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      "状态重算",
		ProjectIDs: []string{proj.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	children, err := svc.ConfirmSplit(x.ID, []model.CrossRequirementSplitCandidate{
		{Title: "接口定义", ProjectID: proj.ID, Order: 1},
	})
	if err != nil {
		t.Fatalf("confirm split: %v", err)
	}

	// The child finishes while the parent sits in the trash.
	if err := svc.MoveToTrash(x.ID); err != nil {
		t.Fatalf("move to trash: %v", err)
	}
	markDone(t, reqSvc, children[0].ID)

	// While trashed, the status is frozen — the read path must not recompute it.
	detail, err := svc.DetailFresh(x.ID)
	if err != nil {
		t.Fatalf("detail while trashed: %v", err)
	}
	if detail.Status == model.XReqStatusDone {
		t.Fatal("trashed row's status was recomputed — it must stay frozen")
	}

	// Restore → the derivation takes over again and sees the finished child.
	if err := svc.Restore(x.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	detail, err = svc.DetailFresh(x.ID)
	if err != nil {
		t.Fatalf("detail after restore: %v", err)
	}
	if detail.Status != model.XReqStatusDone {
		t.Fatalf("status after restore = %q, want %q", detail.Status, model.XReqStatusDone)
	}
}
