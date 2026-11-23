package service

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/dbtest"
	"github.com/novaworkbench/backend/internal/model"
)

// ── 语句计数用的连接池 ──────────────────────────────────────────────────────
//
// 这套计数装置的唯一目的：把「查询数与行数无关」这条性质**测出来**，而不是
// 靠人读代码确认。没有它，一次重构（哪怕只是把循环挪个地方）就能把 N+1 悄悄
// 放回来，而测试照样全绿。计数装置本身在 internal/dbtest 里（handler 侧的
// 列表页测试也要用同一套）。

// newCountingServiceTestDB builds a service test DB that counts statements.
//
// The counting pool replaces the migration pool AFTER the schema is in place,
// so only the statements the test itself triggers are counted.
func newCountingServiceTestDB(t *testing.T) (*db.DB, *int64) {
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
	return d, count
}

// seedAggregateFixture builds one cross requirement with `children` children
// spread over two projects, chained by cross-project dependencies.
func seedAggregateFixture(t *testing.T, d *db.DB, title string, children int) (string, []string) {
	t.Helper()
	projSvc := NewProjectService(d, nil, nil)
	reqSvc := NewRequirementService(d, nil)
	svc := NewCrossRequirementService(d)

	p1 := seedCrossProject(t, projSvc, "web-app-"+title)
	p2 := seedCrossProject(t, projSvc, "api-svc-"+title)

	x, err := svc.Create(model.CreateCrossRequirementReq{
		Title:      title,
		ProjectIDs: []string{p1.ID, p2.ID},
	})
	if err != nil {
		t.Fatalf("create %s: %v", title, err)
	}

	cands := make([]model.CrossRequirementSplitCandidate, 0, children)
	for i := 0; i < children; i++ {
		projectID := p1.ID
		if i%2 == 1 {
			projectID = p2.ID
		}
		cand := model.CrossRequirementSplitCandidate{
			Title:     title + "-子需求",
			ProjectID: projectID,
			Order:     i + 1,
		}
		// 依赖链：每个子需求依赖前一个（跨项目）。
		if i > 0 {
			cand.DependsOn = []int{i}
		}
		cands = append(cands, cand)
	}
	rows, err := svc.ConfirmSplit(x.ID, cands)
	if err != nil {
		t.Fatalf("confirm split %s: %v", title, err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	_ = reqSvc
	return x.ID, ids
}

// TestCrossRequirementAggregate_QueryCountIsConstant 是这次 N+1 修复的回归
// 测试。
//
// 修复前：列表页对每条跨项目需求跑一次聚合，而聚合内部又为**每个子需求**单跑
// 一次前置依赖查询 —— 20 条需求 × 10 个子需求 ≈ 200+ 条语句。
//
// 修复后：整页的语句数是**常数** —— 与需求条数、子需求个数都无关。
func TestCrossRequirementAggregate_QueryCountIsConstant(t *testing.T) {
	d, count := newCountingServiceTestDB(t)
	svc := NewCrossRequirementService(d)

	x1, _ := seedAggregateFixture(t, d, "一条", 10)

	// 一条需求、10 个子需求。
	atomic.StoreInt64(count, 0)
	if _, err := svc.LoadAggregate([]string{x1}); err != nil {
		t.Fatalf("load aggregate: %v", err)
	}
	oneReq := atomic.LoadInt64(count)
	if oneReq == 0 {
		t.Fatal("no statement was counted — the counter is not wired")
	}

	// 再加一条需求、10 个子需求（依赖链跨两个项目）。
	x2, _ := seedAggregateFixture(t, d, "两条", 10)

	atomic.StoreInt64(count, 0)
	if _, err := svc.LoadAggregate([]string{x1, x2}); err != nil {
		t.Fatalf("load aggregate: %v", err)
	}
	twoReqs := atomic.LoadInt64(count)

	if twoReqs != oneReq {
		t.Fatalf("statement count grew with the number of rows: %d for one requirement, %d for two — the aggregate is N+1 again",
			oneReq, twoReqs)
	}
	// 聚合固定跑 5 条：参与项目 / 子需求 / 前置依赖边 / 阶段 / 阶段成员。
	if oneReq != 5 {
		t.Fatalf("aggregate ran %d statements, want 5 (projects / children / edges / phases / phase items)", oneReq)
	}

	// ListChildren 也必须是**常数**条语句 —— 「为每个子需求单查一次前置依赖」
	// 正是被换掉的那一段（修复前是 1 + 子需求数 条）。
	atomic.StoreInt64(count, 0)
	if _, err := svc.ListChildren(x1); err != nil {
		t.Fatalf("list children: %v", err)
	}
	childrenOne := atomic.LoadInt64(count)

	atomic.StoreInt64(count, 0)
	if _, err := svc.ListChildren(x2); err != nil {
		t.Fatalf("list children: %v", err)
	}
	childrenTwo := atomic.LoadInt64(count)

	if childrenOne != childrenTwo {
		t.Fatalf("ListChildren statement count grew with the number of children: %d then %d — the per-child lookup is back",
			childrenOne, childrenTwo)
	}
	if childrenOne > 4 {
		t.Fatalf("ListChildren ran %d statements for 10 children, want a constant <= 4", childrenOne)
	}
}

// TestCrossRequirementAggregate_MatchesSingleId 断言批量聚合与逐条读取的结果
// 完全一致 —— 批量只是把同样的查询合并了，没有改任何判定规则。
func TestCrossRequirementAggregate_MatchesSingleId(t *testing.T) {
	d, _ := newCountingServiceTestDB(t)
	svc := NewCrossRequirementService(d)

	x1, _ := seedAggregateFixture(t, d, "甲", 4)
	x2, _ := seedAggregateFixture(t, d, "乙", 3)

	agg, err := svc.LoadAggregate([]string{x1, x2})
	if err != nil {
		t.Fatalf("load aggregate: %v", err)
	}

	for _, id := range []string{x1, x2} {
		children, err := svc.ListChildren(id)
		if err != nil {
			t.Fatalf("list children: %v", err)
		}
		got := agg.Children[id]
		if len(got) != len(children) {
			t.Fatalf("children for %s: batch %d, single %d", id, len(got), len(children))
		}
		for i := range got {
			if got[i].ID != children[i].ID {
				t.Fatalf("child %d: batch %q, single %q", i, got[i].ID, children[i].ID)
			}
			if len(got[i].DependsOn) != len(children[i].DependsOn) {
				t.Fatalf("child %s depends_on: batch %v, single %v", got[i].ID, got[i].DependsOn, children[i].DependsOn)
			}
			if len(got[i].BlockedBy) != len(children[i].BlockedBy) {
				t.Fatalf("child %s blocked_by: batch %v, single %v", got[i].ID, got[i].BlockedBy, children[i].BlockedBy)
			}
		}

		progress, err := svc.Progress(id)
		if err != nil {
			t.Fatalf("progress: %v", err)
		}
		batch := agg.Progress[id]
		if batch.Total != progress.Total || batch.Done != progress.Done ||
			batch.Blocked != progress.Blocked || batch.Percent != progress.Percent {
			t.Fatalf("progress for %s: batch %+v, single %+v", id, batch, progress)
		}
		if len(batch.StatusCounts) != len(progress.StatusCounts) {
			t.Fatalf("status counts for %s: batch %v, single %v", id, batch.StatusCounts, progress.StatusCounts)
		}
		for status, n := range progress.StatusCounts {
			if batch.StatusCounts[status] != n {
				t.Fatalf("status %s for %s: batch %d, single %d", status, id, batch.StatusCounts[status], n)
			}
		}

		phases, err := svc.ListPhases(id)
		if err != nil {
			t.Fatalf("list phases: %v", err)
		}
		batchPhases := agg.Phases[id]
		if len(batchPhases) != len(phases) {
			t.Fatalf("phases for %s: batch %d, single %d", id, len(batchPhases), len(phases))
		}
		for i := range batchPhases {
			if batchPhases[i].ID != phases[i].ID || batchPhases[i].Status != phases[i].Status {
				t.Fatalf("phase %d for %s: batch %+v, single %+v", i, id, batchPhases[i], phases[i])
			}
			if len(batchPhases[i].Items) != len(phases[i].Items) {
				t.Fatalf("phase %s items: batch %d, single %d", batchPhases[i].ID,
					len(batchPhases[i].Items), len(phases[i].Items))
			}
		}
	}
}
