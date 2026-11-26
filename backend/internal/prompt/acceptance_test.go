package prompt

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestBuildAcceptancePrompt_ContainsContractAndMembers pins the prompt's
// shape: the master's contract, every child's implementation directory and
// the output contract must all be present — a missing piece would silently
// change what the model is asked to judge.
func TestBuildAcceptancePrompt_ContainsContractAndMembers(t *testing.T) {
	master := AcceptanceContractInput{
		Title:              "给订单列表加分页",
		Description:        "订单列表要支持按创建时间倒序的分页。",
		AcceptanceCriteria: []string{"GET /api/orders 支持 page / size 参数", "响应含 total 字段"},
		DesignDocs:         "## 技术方案\n分页用 offset/limit。",
		WorktreePath:       "/tmp/master-wt",
	}
	members := []AcceptanceMember{
		{
			RequirementID:      "req_child1",
			Title:              "订单接口分页参数",
			Status:             "done",
			Description:        "## 目标\n实现分页参数\n\n## 验收标准\n- page / size 参数",
			AcceptanceCriteria: []string{"page / size 参数"},
			WorktreePath:       "/tmp/child1-wt",
		},
		{
			RequirementID:      "req_child2",
			Title:              "响应体补 total",
			Status:             "developing",
			Description:        "## 目标\n补 total 字段",
			AcceptanceCriteria: []string{"响应含 total 字段"},
			WorktreePath:       "/tmp/child2-wt",
			DependsOn:          []string{"订单接口分页参数"},
			DependsOnUnmet:     true,
		},
	}
	out := BuildAcceptancePrompt(master, members, "")

	mustContain(t, out, "给订单列表加分页")
	mustContain(t, out, "GET /api/orders 支持 page / size 参数")
	mustContain(t, out, "响应含 total 字段")
	mustContain(t, out, "## 技术方案")
	mustContain(t, out, "req_child1")
	mustContain(t, out, "/tmp/child1-wt")
	mustContain(t, out, "核对以这个目录里的代码为准")
	mustContain(t, out, "req_child2")
	mustContain(t, out, "/tmp/child2-wt")
	mustContain(t, out, "/tmp/master-wt")
	// The output contract: the Markdown sections and the JSON tail.
	mustContain(t, out, "## 总体结论")
	mustContain(t, out, "## 逐条验收判定")
	mustContain(t, out, "## 未实现清单")
	mustContain(t, out, "## 建议再拆的子需求")
	mustContain(t, out, "```json")
	mustContain(t, out, `"verdict":"yes|partial|no"`)
	// The unmet dependency must be flagged.
	mustContain(t, out, "前置尚未完成")
}

// TestBuildAcceptancePrompt_MissingWorktreeSaysUnverifiable covers the
// gap that would produce a WRONG verdict: a child that never entered
// development has no implementation directory. If the prompt silently
// omitted it, the model would grade the base branch and report it as
// verified.
func TestBuildAcceptancePrompt_MissingWorktreeSaysUnverifiable(t *testing.T) {
	out := BuildAcceptancePrompt(
		AcceptanceContractInput{Title: "主需求"},
		[]AcceptanceMember{{RequirementID: "req_c1", Title: "还没开工的子需求", Status: "draft"}},
		"",
	)
	mustContain(t, out, "req_c1")
	mustContain(t, out, "尚未进入开发")
	mustContain(t, out, "无法验证")
	if strings.Contains(out, "核对以这个目录里的代码为准") {
		t.Fatalf("a child without a worktree must not be given an implementation directory")
	}
}

// TestBuildAcceptancePrompt_NoMembersStatesIt covers the empty-children
// case: the prompt must say so rather than render an empty section.
func TestBuildAcceptancePrompt_NoMembersStatesIt(t *testing.T) {
	out := BuildAcceptancePrompt(AcceptanceContractInput{Title: "主需求"}, nil, "")
	mustContain(t, out, "没有拆分子需求")
}

// TestBuildAcceptancePrompt_ConventionsAreIncluded checks that the project
// conventions reach the prompt — the verdict is judged against the
// project's own rules.
func TestBuildAcceptancePrompt_ConventionsAreIncluded(t *testing.T) {
	out := BuildAcceptancePrompt(AcceptanceContractInput{Title: "主需求"}, nil, "## 项目级约定\n分层：handler / service。")
	mustContain(t, out, "分层：handler / service。")
}

// TestBuildAcceptancePrompt_TruncatesLongInput covers the prompt budget:
// an over-long description must be truncated, and the truncation must not
// break a multi-byte character.
func TestBuildAcceptancePrompt_TruncatesLongInput(t *testing.T) {
	long := strings.Repeat("验", 8000) // 8000 runes = 24000 bytes
	out := BuildAcceptancePrompt(AcceptanceContractInput{
		Title:       "主需求",
		Description: long,
		DesignDocs:  long,
	}, []AcceptanceMember{{RequirementID: "req_c1", Title: "子需求", Description: long}}, "")

	mustContain(t, out, "已截断")
	if strings.Contains(out, "�") {
		t.Fatalf("truncation produced a replacement character (broken UTF-8)")
	}
	// The prompt must stay far below the raw input size.
	if len(out) > len(long)*3 {
		t.Fatalf("prompt is %d bytes — truncation did not kick in", len(out))
	}
}

// TestAcceptanceSummaryText pins the job summary line.
func TestAcceptanceSummaryText(t *testing.T) {
	got := AcceptanceSummaryText(3, 0)
	if !strings.Contains(got, "3") {
		t.Fatalf("summary %q does not mention the member count", got)
	}
	// A check that ran while children were unfinished says so: the verdict
	// is a snapshot, not a sign-off.
	pending := AcceptanceSummaryText(3, 2)
	if !strings.Contains(pending, "阶段性结论") {
		t.Fatalf("summary %q does not mark the result as provisional", pending)
	}
}

// TestBuildAcceptancePrompt_InProgressChildIsNotAFailure covers the case
// the whole partial-state feature exists for: a check that runs while a
// child is still being developed.
//
// The prompt must tell the model that the directory holds a PARTIAL
// snapshot, because without that the model reads "the code is not there" as
// "the child never implemented it" — and reports work in flight as a gap.
func TestBuildAcceptancePrompt_InProgressChildIsNotAFailure(t *testing.T) {
	out := BuildAcceptancePrompt(
		AcceptanceContractInput{Title: "主需求"},
		[]AcceptanceMember{{
			RequirementID: "req_c1",
			Title:         "还在开发的子需求",
			Status:        "developing",
			Stage:         model.AcceptanceStageInProgress,
			WorktreePath:  "/tmp/child1-wt",
		}},
		"",
	)
	mustContain(t, out, "开发中")
	mustContain(t, out, "阶段性")
	mustContain(t, out, "不要**写进「未实现清单」")
	// The snapshot framing: the verdict may not certify an unfinished split.
	mustContain(t, out, "## 本次核对的时点")
	mustContain(t, out, "部分达成")
}

// TestBuildAcceptancePrompt_SnapshotSectionOnlyWhenPending pins the
// "when" section: it is rendered only while something is still in flight.
// With every child delivered it would be noise.
func TestBuildAcceptancePrompt_SnapshotSectionOnlyWhenPending(t *testing.T) {
	allDone := BuildAcceptancePrompt(
		AcceptanceContractInput{Title: "主需求"},
		[]AcceptanceMember{{
			RequirementID: "req_c1", Title: "子需求", Status: "done",
			Stage: model.AcceptanceStageDone, WorktreePath: "/tmp/c1",
		}},
		"",
	)
	if strings.Contains(allDone, "## 本次核对的时点") {
		t.Fatalf("a fully delivered split must not render the snapshot section")
	}

	pending := BuildAcceptancePrompt(
		AcceptanceContractInput{Title: "主需求"},
		[]AcceptanceMember{{
			RequirementID: "req_c1", Title: "子需求", Status: "developing",
			Stage: model.AcceptanceStageInProgress, WorktreePath: "/tmp/c1",
		}},
		"",
	)
	mustContain(t, pending, "## 本次核对的时点")
	mustContain(t, pending, "还没写」不等于「未实现")
}

// TestBuildAcceptancePrompt_NotStartedChildStaysUnverifiable keeps the
// pre-existing discipline intact: a child that never started has nothing to
// read, so its points are unverifiable — never a failure.
func TestBuildAcceptancePrompt_NotStartedChildStaysUnverifiable(t *testing.T) {
	out := BuildAcceptancePrompt(
		AcceptanceContractInput{Title: "主需求"},
		[]AcceptanceMember{{
			RequirementID: "req_c1", Title: "还没开工的子需求", Status: "draft",
			Stage: model.AcceptanceStageNotStarted,
		}},
		"",
	)
	mustContain(t, out, "尚未进入开发")
	mustContain(t, out, "无法验证")
	if strings.Contains(out, "核对以这个目录里的代码为准") {
		t.Fatalf("a child without a worktree must not be given an implementation directory")
	}
}

// TestBuildAcceptancePrompt_DevelopingWithoutDirectoryIsDistinct covers the
// fourth state: the child IS being developed, but its code is not on this
// machine. Saying "尚未进入开发" there would be a lie.
func TestBuildAcceptancePrompt_DevelopingWithoutDirectoryIsDistinct(t *testing.T) {
	out := BuildAcceptancePrompt(
		AcceptanceContractInput{Title: "主需求"},
		[]AcceptanceMember{{
			RequirementID: "req_c1", Title: "在别的机器上开发的子需求", Status: "developing",
			Stage: model.AcceptanceStageUnavailable,
		}},
		"",
	)
	mustContain(t, out, "仍在开发中")
	mustContain(t, out, "无法验证")
	if strings.Contains(out, "尚未进入开发") {
		t.Fatalf("a developing child must not be reported as not started")
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("prompt does not contain %q\n---\n%s", needle, haystack)
	}
}
