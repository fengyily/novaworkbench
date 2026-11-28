package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestParseAcceptanceReport_ContractBlock covers the happy path: the
// trailing JSON block carries the whole structured result.
func TestParseAcceptanceReport_ContractBlock(t *testing.T) {
	raw := `# 验收核对报告

## 总体结论
部分达成。

## 逐条验收判定
- 分页参数已支持
  - 判定：通过
  - 依据：backend/handler/orders.go:120
  - 归属：子需求①

## 未实现清单
- 缺少 total 字段 —— 归属子需求②

## 建议再拆的子需求
- 补充 total 字段
  - 目标：响应里带上总数
  - 验收标准：响应含 total 字段
  - 建议前置：子需求①

` + "```json" + `
{
  "verdict": "partial",
  "summary": "分页参数已实现，total 字段缺失",
  "checks": [
    {"criterion": "GET /api/orders 支持 page / size", "status": "yes", "evidence": "backend/handler/orders.go:120", "child_id": "req_c1", "child_title": "子需求①"},
    {"criterion": "响应含 total 字段", "status": "no", "evidence": "backend/model/order.go:44 缺少该字段", "child_id": "req_c2", "child_title": "子需求②"}
  ],
  "gaps": [
    {"title": "缺少 total 字段", "detail": "响应体里没有 total", "child_id": "req_c2", "child_title": "子需求②"}
  ],
  "proposals": [
    {"title": "补充 total 字段", "goal": "响应里带上总数", "acceptance": ["响应含 total 字段"], "depends_on": ["子需求①"]}
  ]
}
` + "```"

	md, summary, err := parseAcceptanceReport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Contains(md, "```json") {
		t.Fatalf("the JSON block must be stripped from the markdown:\n%s", md)
	}
	if !strings.Contains(md, "## 总体结论") {
		t.Fatalf("markdown lost its sections:\n%s", md)
	}
	if summary.Verdict != model.AcceptanceVerdictPartial {
		t.Fatalf("verdict = %q, want partial", summary.Verdict)
	}
	if summary.Summary == "" {
		t.Fatalf("summary is empty")
	}
	if len(summary.Checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(summary.Checks))
	}
	if summary.Checks[0].Criterion != "GET /api/orders 支持 page / size" {
		t.Fatalf("check[0].criterion = %q", summary.Checks[0].Criterion)
	}
	if summary.Checks[0].ChildID != "req_c1" || summary.Checks[0].ChildTitle != "子需求①" {
		t.Fatalf("check[0] child = %q / %q", summary.Checks[0].ChildID, summary.Checks[0].ChildTitle)
	}
	if summary.Checks[1].Status != model.AcceptanceVerdictNo {
		t.Fatalf("check[1].status = %q, want no", summary.Checks[1].Status)
	}
	if len(summary.Gaps) != 1 || summary.Gaps[0].Title != "缺少 total 字段" {
		t.Fatalf("gaps = %+v", summary.Gaps)
	}
	if len(summary.Proposals) != 1 || summary.Proposals[0].Title != "补充 total 字段" {
		t.Fatalf("proposals = %+v", summary.Proposals)
	}
	if len(summary.Proposals[0].Acceptance) != 1 {
		t.Fatalf("proposal acceptance = %+v", summary.Proposals[0].Acceptance)
	}
}

// TestParseAcceptanceReport_NoContractBlock covers the second degradation
// level: no JSON block at all — the summary is rebuilt from the report's
// own Markdown sections.
func TestParseAcceptanceReport_NoContractBlock(t *testing.T) {
	raw := `## 总体结论
部分达成：分页参数有了，total 字段还没有。

## 逐条验收判定
- GET /api/orders 支持 page / size
  - 判定：通过
  - 依据：backend/handler/orders.go:120
  - 归属：子需求①
- 响应含 total 字段
  - 判定：未通过
  - 依据：backend/model/order.go:44
  - 归属：子需求②

## 未实现清单
- 缺少 total 字段 —— 归属子需求②

## 建议再拆的子需求
- 补充 total 字段
  - 目标：响应里带上总数
  - 验收标准：响应含 total 字段
`
	md, summary, err := parseAcceptanceReport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(md, "## 总体结论") {
		t.Fatalf("markdown lost its sections:\n%s", md)
	}
	if summary.Verdict != model.AcceptanceVerdictPartial {
		t.Fatalf("verdict = %q, want partial", summary.Verdict)
	}
	if len(summary.Checks) != 2 {
		t.Fatalf("checks = %d, want 2: %+v", len(summary.Checks), summary.Checks)
	}
	if summary.Checks[0].Status != model.AcceptanceVerdictYes {
		t.Fatalf("check[0].status = %q, want yes", summary.Checks[0].Status)
	}
	if summary.Checks[1].Status != model.AcceptanceVerdictNo {
		t.Fatalf("check[1].status = %q, want no", summary.Checks[1].Status)
	}
	if summary.Checks[0].Evidence != "backend/handler/orders.go:120" {
		t.Fatalf("check[0].evidence = %q", summary.Checks[0].Evidence)
	}
	if len(summary.Gaps) != 1 {
		t.Fatalf("gaps = %+v", summary.Gaps)
	}
	if len(summary.Proposals) != 1 {
		t.Fatalf("proposals = %+v", summary.Proposals)
	}
}

// TestParseAcceptanceReport_BrokenContractBlock covers the case where the
// model emitted a fenced block that is not valid JSON: the run must NOT
// panic, and the Markdown report must survive.
func TestParseAcceptanceReport_BrokenContractBlock(t *testing.T) {
	raw := "## 总体结论\n部分达成。\n\n```json\n{not json at all\n```\n"
	md, _, err := parseAcceptanceReport(raw)
	if err == nil {
		t.Fatalf("a broken JSON block must be reported as an error")
	}
	if !strings.Contains(md, "## 总体结论") {
		t.Fatalf("markdown lost its sections:\n%s", md)
	}
}

// TestParseAcceptanceReport_TypeDrift covers the models' habit of writing
// numbers where strings belong. It must not panic and must not lose the
// other fields.
func TestParseAcceptanceReport_TypeDrift(t *testing.T) {
	raw := "```json\n" + `{
  "verdict": 42,
  "summary": 7,
  "checks": [
    {"criterion": 1, "status": "yes", "evidence": "a.go:1", "child_id": 9},
    {"criterion": "第二条", "status": ["x"], "evidence": 3.5}
  ],
  "gaps": [{"title": 1, "detail": 2}],
  "proposals": [{"title": 3, "acceptance": [1, "两条"], "depends_on": 4}]
}` + "\n```\n"
	md, summary, err := parseAcceptanceReport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if md != "" {
		t.Fatalf("the markdown part should be empty here, got %q", md)
	}
	// An out-of-vocabulary verdict is cleared, not shown as one of the three.
	if summary.Verdict != "" {
		t.Fatalf("verdict = %q, want empty (42 is not a verdict)", summary.Verdict)
	}
	if summary.Summary != "7" {
		t.Fatalf("summary = %q, want the numeric form rendered as text", summary.Summary)
	}
	if len(summary.Checks) != 2 {
		t.Fatalf("checks = %d, want 2: %+v", len(summary.Checks), summary.Checks)
	}
	if summary.Checks[0].Criterion != "1" {
		t.Fatalf("check[0].criterion = %q", summary.Checks[0].Criterion)
	}
	if summary.Checks[0].ChildID != "9" {
		t.Fatalf("check[0].child_id = %q", summary.Checks[0].ChildID)
	}
	if summary.Checks[1].Status != model.AcceptanceCheckUnknown {
		t.Fatalf("check[1].status = %q, want unknown", summary.Checks[1].Status)
	}
	if len(summary.Gaps) != 1 || summary.Gaps[0].Title != "1" {
		t.Fatalf("gaps = %+v", summary.Gaps)
	}
	if len(summary.Proposals) != 1 {
		t.Fatalf("proposals = %+v", summary.Proposals)
	}
	if len(summary.Proposals[0].Acceptance) != 2 {
		t.Fatalf("proposal acceptance = %+v", summary.Proposals[0].Acceptance)
	}
}

// TestParseAcceptanceReport_HollowEvidenceDowngradesToUnknown is the guard
// against a confident, unfounded "通过": an evidence that says nothing
// checkable cannot back a pass.
func TestParseAcceptanceReport_HollowEvidenceDowngradesToUnknown(t *testing.T) {
	raw := "```json\n" + `{
  "verdict": "yes",
  "checks": [
    {"criterion": "分页参数", "status": "yes", "evidence": "已确认"},
    {"criterion": "total 字段", "status": "yes", "evidence": "backend/order.go:12"}
  ]
}` + "\n```\n"
	_, summary, err := parseAcceptanceReport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(summary.Checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(summary.Checks))
	}
	if summary.Checks[0].Status != model.AcceptanceCheckUnknown {
		t.Fatalf("check[0].status = %q, want unknown (hollow evidence)", summary.Checks[0].Status)
	}
	if summary.Checks[1].Status != model.AcceptanceVerdictYes {
		t.Fatalf("check[1].status = %q, want yes (real evidence)", summary.Checks[1].Status)
	}
}

// TestParseAcceptanceReport_MissingSections covers a contract block that
// only answers the verdict: the other sections are simply empty.
func TestParseAcceptanceReport_MissingSections(t *testing.T) {
	raw := "```json\n" + `{"verdict":"yes","summary":"都做到了"}` + "\n```\n"
	_, summary, err := parseAcceptanceReport(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if summary.Verdict != model.AcceptanceVerdictYes {
		t.Fatalf("verdict = %q", summary.Verdict)
	}
	if len(summary.Checks) != 0 || len(summary.Gaps) != 0 || len(summary.Proposals) != 0 {
		t.Fatalf("missing sections must stay empty: %+v", summary)
	}
}

// TestParseAcceptanceReport_EmptyInput covers the degenerate input.
func TestParseAcceptanceReport_EmptyInput(t *testing.T) {
	if _, _, err := parseAcceptanceReport("   \n  "); err == nil {
		t.Fatalf("an empty report must be an error")
	}
}

// TestParseAcceptanceReport_NeverPanics fuzzes the parser with the shapes
// models actually produce. The parser's contract is "never panic".
func TestParseAcceptanceReport_NeverPanics(t *testing.T) {
	inputs := []string{
		"```json\n```\n",
		"```json\nnull\n```\n",
		"```json\n[]\n```\n",
		"```json\n{\"checks\": 5}\n```\n",
		"```json\n{\"checks\": [1,2,3]}\n```\n",
		"```json\n{\"gaps\": [{}], \"proposals\": [{}]}\n```\n",
		"## 总体结论\n",
		"## 逐条验收判定\n- \n- 判定：\n",
		"```json\n{\"verdict\": {\"a\":1}}\n```\n",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("parser panicked on %q: %v", in, r)
				}
			}()
			_, _, _ = parseAcceptanceReport(in)
		}()
	}
}

// TestVerdictFromChecks pins the derivation rules of the overall verdict.
func TestVerdictFromChecks(t *testing.T) {
	cases := []struct {
		name   string
		checks []model.AcceptanceCheck
		want   string
	}{
		{"no checks", nil, ""},
		{"all yes", []model.AcceptanceCheck{{Status: model.AcceptanceVerdictYes}}, model.AcceptanceVerdictYes},
		{"one no wins", []model.AcceptanceCheck{
			{Status: model.AcceptanceVerdictYes}, {Status: model.AcceptanceVerdictNo},
		}, model.AcceptanceVerdictNo},
		{"partial", []model.AcceptanceCheck{
			{Status: model.AcceptanceVerdictYes}, {Status: model.AcceptanceVerdictPartial},
		}, model.AcceptanceVerdictPartial},
		{"unknown alone certifies nothing", []model.AcceptanceCheck{
			{Status: model.AcceptanceCheckUnknown},
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verdictFromChecks(tc.checks); got != tc.want {
				t.Fatalf("verdictFromChecks = %q, want %q", got, tc.want)
			}
		})
	}
}
