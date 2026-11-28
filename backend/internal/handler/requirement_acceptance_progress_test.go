package handler

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
)

// The reconciliation rules of the acceptance check.
//
// They exist because the check is meant to be runnable at ANY moment: the
// user asks where the work stands while the children are still being
// developed. What they must never produce is a verdict that blames work in
// flight.

// acceptanceMember builds a member for the tests.
func acceptanceMember(id, title, status, stage, worktree string) prompt.AcceptanceMember {
	return prompt.AcceptanceMember{
		RequirementID: id,
		Title:         title,
		Status:        status,
		Stage:         stage,
		WorktreePath:  worktree,
	}
}

// TestAcceptanceStage pins the status → stage mapping.
//
// The stage is what the CHECK can see, so the interesting cases are the
// ones where the workflow's status and the visibility disagree.
func TestAcceptanceStage(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		worktree string
		want     string
	}{
		{"finished", "done", "", model.AcceptanceStageDone},
		{"archived is finished", "archived", "", model.AcceptanceStageDone},
		{"finished with a worktree", "done", "/tmp/wt", model.AcceptanceStageDone},
		{"developing with a worktree", "developing", "/tmp/wt", model.AcceptanceStageInProgress},
		{"developing without a worktree", "developing", "", model.AcceptanceStageUnavailable},
		{"not started", "draft", "", model.AcceptanceStageNotStarted},
		{"designed but not developed", "designed", "", model.AcceptanceStageNotStarted},
		{"a leftover worktree means code to read", "draft", "/tmp/wt", model.AcceptanceStageInProgress},
	}
	for _, c := range cases {
		if got := prompt.AcceptanceStage(c.status, c.worktree); got != c.want {
			t.Fatalf("%s: stage(%q, %q) = %q, want %q", c.name, c.status, c.worktree, got, c.want)
		}
	}
}

// TestApplyAcceptanceProgress_AllDelivered is the baseline: when every child
// finished, nothing about the verdict changes.
func TestApplyAcceptanceProgress_AllDelivered(t *testing.T) {
	members := []prompt.AcceptanceMember{
		acceptanceMember("c1", "子需求一", "done", model.AcceptanceStageDone, "/tmp/c1"),
		acceptanceMember("c2", "子需求二", "done", model.AcceptanceStageDone, "/tmp/c2"),
	}
	summary := model.AcceptanceSummary{
		Verdict: model.AcceptanceVerdictYes,
		Checks: []model.AcceptanceCheck{
			{Criterion: "分页参数", Status: model.AcceptanceVerdictYes, ChildID: "c1"},
		},
	}
	out := applyAcceptanceProgress(summary, members)

	if out.Verdict != model.AcceptanceVerdictYes {
		t.Fatalf("verdict = %q, want %q", out.Verdict, model.AcceptanceVerdictYes)
	}
	if out.Provisional {
		t.Fatalf("a fully delivered split must not be provisional")
	}
	if len(out.Pending) != 0 {
		t.Fatalf("pending = %+v, want empty", out.Pending)
	}
	if out.Checks[0].Status != model.AcceptanceVerdictYes {
		t.Fatalf("check status = %q, want %q", out.Checks[0].Status, model.AcceptanceVerdictYes)
	}
	if out.Checks[0].ChildState != model.AcceptanceStageDone {
		t.Fatalf("child state = %q, want %q", out.Checks[0].ChildState, model.AcceptanceStageDone)
	}
}

// TestApplyAcceptanceProgress_UnfinishedCapsTheVerdict is the core rule: a
// check that ran while a child was still being developed cannot certify the
// master as delivered.
func TestApplyAcceptanceProgress_UnfinishedCapsTheVerdict(t *testing.T) {
	members := []prompt.AcceptanceMember{
		acceptanceMember("c1", "已完成的子需求", "done", model.AcceptanceStageDone, "/tmp/c1"),
		acceptanceMember("c2", "还在开发的子需求", "developing", model.AcceptanceStageInProgress, "/tmp/c2"),
	}
	summary := model.AcceptanceSummary{
		Verdict: model.AcceptanceVerdictYes,
		Checks: []model.AcceptanceCheck{
			{Criterion: "分页参数", Status: model.AcceptanceVerdictYes, ChildID: "c1"},
		},
	}
	out := applyAcceptanceProgress(summary, members)

	if out.Verdict != model.AcceptanceVerdictPartial {
		t.Fatalf("verdict = %q, want %q — a child is still in flight", out.Verdict, model.AcceptanceVerdictPartial)
	}
	if !out.Provisional {
		t.Fatalf("the result must be marked provisional")
	}
	if len(out.Pending) != 1 || out.Pending[0].ChildID != "c2" {
		t.Fatalf("pending = %+v, want the in-flight child", out.Pending)
	}
	if out.Pending[0].Stage != model.AcceptanceStageInProgress {
		t.Fatalf("pending stage = %q, want %q", out.Pending[0].Stage, model.AcceptanceStageInProgress)
	}
}

// TestApplyAcceptanceProgress_InFlightWorkIsNotAFailure covers the blame:
// a point owned by a child that is still being developed is "not written
// yet", not "not implemented".
func TestApplyAcceptanceProgress_InFlightWorkIsNotAFailure(t *testing.T) {
	members := []prompt.AcceptanceMember{
		acceptanceMember("c1", "还在开发的子需求", "developing", model.AcceptanceStageInProgress, "/tmp/c1"),
	}
	summary := model.AcceptanceSummary{
		Verdict: model.AcceptanceVerdictNo,
		Checks: []model.AcceptanceCheck{
			{Criterion: "导出功能", Status: model.AcceptanceVerdictNo, ChildID: "c1", Evidence: "backend/export.go:10"},
		},
		Gaps: []model.AcceptanceGap{
			{Title: "导出功能", ChildID: "c1"},
		},
	}
	out := applyAcceptanceProgress(summary, members)

	if out.Checks[0].Status != model.AcceptanceVerdictPartial {
		t.Fatalf("check status = %q, want %q — the child is still developing",
			out.Checks[0].Status, model.AcceptanceVerdictPartial)
	}
	if out.Checks[0].ChildState != model.AcceptanceStageInProgress {
		t.Fatalf("child state = %q, want %q", out.Checks[0].ChildState, model.AcceptanceStageInProgress)
	}
	// Every failure was against work in flight, so the master is "not
	// finished", not "not met".
	if out.Verdict != model.AcceptanceVerdictPartial {
		t.Fatalf("verdict = %q, want %q", out.Verdict, model.AcceptanceVerdictPartial)
	}
	// The gap is kept (the user still wants to see what is open) but it is
	// labelled as work in flight.
	if len(out.Gaps) != 1 || out.Gaps[0].ChildState != model.AcceptanceStageInProgress {
		t.Fatalf("gap = %+v, want the in-flight stage recorded", out.Gaps)
	}
}

// TestApplyAcceptanceProgress_UnverifiableIsNotAFailure covers the child
// whose code is not on this machine: a failure against it is unverifiable,
// not a real gap.
func TestApplyAcceptanceProgress_UnverifiableIsNotAFailure(t *testing.T) {
	members := []prompt.AcceptanceMember{
		acceptanceMember("c1", "在别的机器上开发", "developing", model.AcceptanceStageUnavailable, ""),
		acceptanceMember("c2", "还没开工", "draft", model.AcceptanceStageNotStarted, ""),
	}
	summary := model.AcceptanceSummary{
		Verdict: model.AcceptanceVerdictNo,
		Checks: []model.AcceptanceCheck{
			{Criterion: "A", Status: model.AcceptanceVerdictNo, ChildID: "c1", Evidence: "backend/a.go:1"},
			{Criterion: "B", Status: model.AcceptanceVerdictNo, ChildID: "c2", Evidence: "backend/b.go:2"},
		},
	}
	out := applyAcceptanceProgress(summary, members)

	for i, c := range out.Checks {
		if c.Status != model.AcceptanceCheckUnknown {
			t.Fatalf("check %d status = %q, want %q — there is nothing to read",
				i, c.Status, model.AcceptanceCheckUnknown)
		}
	}
	if out.Verdict != model.AcceptanceVerdictPartial {
		t.Fatalf("verdict = %q, want %q", out.Verdict, model.AcceptanceVerdictPartial)
	}
	if len(out.Pending) != 2 {
		t.Fatalf("pending = %+v, want both children", out.Pending)
	}
}

// TestApplyAcceptanceProgress_FinalGapKeepsTheFailure is the other side of
// the coin: a gap against a FINISHED child is a real gap, and the verdict
// must stay 「未达成」.
func TestApplyAcceptanceProgress_FinalGapKeepsTheFailure(t *testing.T) {
	members := []prompt.AcceptanceMember{
		acceptanceMember("c1", "已完成但漏了", "done", model.AcceptanceStageDone, "/tmp/c1"),
		acceptanceMember("c2", "还在开发", "developing", model.AcceptanceStageInProgress, "/tmp/c2"),
	}
	summary := model.AcceptanceSummary{
		Verdict: model.AcceptanceVerdictNo,
		Checks: []model.AcceptanceCheck{
			{Criterion: "缺失的校验", Status: model.AcceptanceVerdictNo, ChildID: "c1", Evidence: "backend/a.go:1"},
			{Criterion: "还没写的导出", Status: model.AcceptanceVerdictNo, ChildID: "c2", Evidence: "backend/b.go:2"},
		},
	}
	out := applyAcceptanceProgress(summary, members)

	if out.Verdict != model.AcceptanceVerdictNo {
		t.Fatalf("verdict = %q, want %q — a finished child really missed this",
			out.Verdict, model.AcceptanceVerdictNo)
	}
	if out.Checks[1].Status != model.AcceptanceVerdictPartial {
		t.Fatalf("the in-flight check = %q, want %q", out.Checks[1].Status, model.AcceptanceVerdictPartial)
	}
	if out.Checks[0].Status != model.AcceptanceVerdictNo {
		t.Fatalf("the finished child's check = %q, want %q", out.Checks[0].Status, model.AcceptanceVerdictNo)
	}
}

// TestApplyAcceptanceProgress_UnknownOwnerIsUntouched covers a point that
// belongs to no child: the rules apply to a child's own points, and a point
// nobody owns is the model's call.
func TestApplyAcceptanceProgress_UnknownOwnerIsUntouched(t *testing.T) {
	members := []prompt.AcceptanceMember{
		acceptanceMember("c1", "还在开发", "developing", model.AcceptanceStageInProgress, "/tmp/c1"),
	}
	summary := model.AcceptanceSummary{
		Verdict: model.AcceptanceVerdictNo,
		Checks: []model.AcceptanceCheck{
			{Criterion: "没有归属的点", Status: model.AcceptanceVerdictNo, Evidence: "backend/a.go:1"},
		},
	}
	out := applyAcceptanceProgress(summary, members)

	if out.Checks[0].Status != model.AcceptanceVerdictNo {
		t.Fatalf("check status = %q, want it untouched", out.Checks[0].Status)
	}
	if out.Checks[0].ChildState != "" {
		t.Fatalf("child state = %q, want empty", out.Checks[0].ChildState)
	}
	if out.Verdict != model.AcceptanceVerdictNo {
		t.Fatalf("verdict = %q, want %q", out.Verdict, model.AcceptanceVerdictNo)
	}
}

// TestAcceptancePartialSplit_EndToEnd runs the whole path a check on a
// half-built split takes: the model's report → the parser → the
// reconciliation → the summary the panel renders.
//
// The model's report here is the one the rules exist for: it graded a child
// that is STILL BEING DEVELOPED as 「未通过」 and put it in the "not
// implemented" list, because the export feature is not written yet.
func TestAcceptancePartialSplit_EndToEnd(t *testing.T) {
	report := "## 总体结论\n部分达成 —— 报表导出还在开发中。\n\n" +
		"## 逐条验收判定\n" +
		"- 订单列表支持分页\n  - 判定：通过\n  - 依据：backend/api/orders.go:42\n  - 归属：订单接口分页参数\n" +
		"- 订单报表导出\n  - 判定：未通过\n  - 依据：backend/report/export.go:10\n  - 归属：报表导出\n\n" +
		"## 未实现清单\n- 订单报表导出 —— 归属报表导出\n\n" +
		"```json\n" +
		`{"verdict":"no","summary":"报表导出还在开发中",` +
		`"checks":[` +
		`{"criterion":"订单列表支持分页","status":"yes","evidence":"backend/api/orders.go:42","child_id":"req_c1","child_title":"订单接口分页参数"},` +
		`{"criterion":"订单报表导出","status":"no","evidence":"backend/report/export.go:10","child_id":"req_c2","child_title":"报表导出"}],` +
		`"gaps":[{"title":"订单报表导出","detail":"导出入口还没写","child_id":"req_c2","child_title":"报表导出"}],` +
		`"proposals":[]}` +
		"\n```"

	resultMD, summary, err := parseAcceptanceReport(report)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resultMD == "" {
		t.Fatalf("the Markdown report must survive")
	}

	members := []prompt.AcceptanceMember{
		acceptanceMember("req_c1", "订单接口分页参数", "done", model.AcceptanceStageDone, "/tmp/c1"),
		acceptanceMember("req_c2", "报表导出", "developing", model.AcceptanceStageInProgress, "/tmp/c2"),
	}
	out := applyAcceptanceProgress(summary, members)

	// The verdict may not blame the work in flight: it is 「部分达成」.
	if out.Verdict != model.AcceptanceVerdictPartial {
		t.Fatalf("verdict = %q, want %q", out.Verdict, model.AcceptanceVerdictPartial)
	}
	if !out.Provisional {
		t.Fatalf("the result must be marked provisional")
	}
	// The finished child's point keeps its verdict.
	if out.Checks[0].Status != model.AcceptanceVerdictYes {
		t.Fatalf("the delivered point = %q, want %q", out.Checks[0].Status, model.AcceptanceVerdictYes)
	}
	// The in-flight child's point is not a failure.
	if out.Checks[1].Status != model.AcceptanceVerdictPartial {
		t.Fatalf("the in-flight point = %q, want %q", out.Checks[1].Status, model.AcceptanceVerdictPartial)
	}
	// The pending child is listed, so the panel can say what is still open.
	if len(out.Pending) != 1 || out.Pending[0].ChildID != "req_c2" {
		t.Fatalf("pending = %+v, want the in-flight child", out.Pending)
	}
	// The gap is kept — the user still wants to see it — but labelled.
	if len(out.Gaps) != 1 || out.Gaps[0].ChildState != model.AcceptanceStageInProgress {
		t.Fatalf("gap = %+v, want the in-flight stage recorded", out.Gaps)
	}
}

// TestApplyAcceptanceProgress_DerivesTheStageWhenMissing covers a caller
// that did not fill the stage in: the rules must still hold.
func TestApplyAcceptanceProgress_DerivesTheStageWhenMissing(t *testing.T) {
	members := []prompt.AcceptanceMember{
		{RequirementID: "c1", Title: "还在开发", Status: "developing", WorktreePath: "/tmp/c1"},
	}
	summary := model.AcceptanceSummary{
		Verdict: model.AcceptanceVerdictYes,
		Checks: []model.AcceptanceCheck{
			{Criterion: "分页参数", Status: model.AcceptanceVerdictNo, ChildID: "c1", Evidence: "backend/a.go:1"},
		},
	}
	out := applyAcceptanceProgress(summary, members)

	if out.Checks[0].Status != model.AcceptanceVerdictPartial {
		t.Fatalf("check status = %q, want %q", out.Checks[0].Status, model.AcceptanceVerdictPartial)
	}
	if !out.Provisional || len(out.Pending) != 1 {
		t.Fatalf("the derived stage must make the result provisional: %+v", out)
	}
}
