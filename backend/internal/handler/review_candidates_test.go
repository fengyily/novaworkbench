package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestSplitRequirementDescription pins the split between "what the change
// must achieve" (the acceptance points, offered as chips) and "why it
// exists" (the prose, pre-filled into the goal box).
//
// The split matters for two reasons:
//   - the two halves must not be shown twice (the goal box would otherwise
//     repeat every acceptance point the chips already offer);
//   - a chip that is really prose is noise the user has to dismiss.
func TestSplitRequirementDescription(t *testing.T) {
	desc := strings.Join([]string{
		"给列表接口加分页。",
		"",
		"需求背景：列表接口一次返回太多数据。",
		"",
		"验收标准：",
		"- 支持 page 与 pageSize 参数",
		"- 默认 pageSize 为 20",
		"1. 必须校验 pageSize 不超过 100",
		"",
		"返回体需要包含 total 字段",
		"",
		"其它说明：这一段不应该被当成验收点。",
	}, "\n")

	criteria, prose := splitRequirementDescription(desc)

	titles := make([]string, 0, len(criteria))
	for _, c := range criteria {
		titles = append(titles, c.Title)
	}
	joined := strings.Join(titles, " | ")

	for _, want := range []string{
		"支持 page 与 pageSize 参数",
		"默认 pageSize 为 20",
		"必须校验 pageSize 不超过 100",
		"返回体需要包含 total 字段",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing criterion %q in %q", want, joined)
		}
	}
	// The background line mentions "返回" — prose, not an acceptance point.
	if strings.Contains(joined, "需求背景") {
		t.Fatalf("the background line leaked into the criteria: %q", joined)
	}
	// "不应该" is a negation, not a modal verb.
	if strings.Contains(joined, "其它说明") {
		t.Fatalf("a negated sentence leaked into the criteria: %q", joined)
	}
	// The prose keeps the context the goal box is pre-filled with.
	if !strings.Contains(prose, "需求背景") {
		t.Fatalf("prose lost the background line:\n%s", prose)
	}
	if strings.Contains(prose, "支持 page 与 pageSize 参数") {
		t.Fatalf("prose must not repeat the criteria:\n%s", prose)
	}
}

// TestSplitRequirementDescription_Empty makes sure an empty description
// yields empty halves rather than a nil-vs-empty surprise at the call site.
func TestSplitRequirementDescription_Empty(t *testing.T) {
	criteria, prose := splitRequirementDescription("")
	if len(criteria) != 0 {
		t.Fatalf("criteria = %v, want empty", criteria)
	}
	if prose != "" {
		t.Fatalf("prose = %q, want empty", prose)
	}
}

// TestBuildReviewGoal checks the pre-filled goal: the requirement's title
// plus the prose half of its description.
func TestBuildReviewGoal(t *testing.T) {
	req := &model.Requirement{
		Title:       "列表分页",
		Description: "给列表接口加分页。",
	}
	goal := buildReviewGoal(req, "给列表接口加分页。")
	if !strings.Contains(goal, "列表分页") {
		t.Fatalf("goal lost the title:\n%s", goal)
	}
	if !strings.Contains(goal, "给列表接口加分页") {
		t.Fatalf("goal lost the prose:\n%s", goal)
	}

	// A requirement with no description still gets its title as the goal.
	only := buildReviewGoal(&model.Requirement{Title: "只有标题"}, "")
	if !strings.Contains(only, "只有标题") {
		t.Fatalf("goal = %q, want the title", only)
	}
}

// TestParseGoVetFindings pins the parser: a `go vet` diagnostic becomes a
// chip with the message as the title and file:line as the detail.
//
// The "vet: " prefix is the interesting case — `go vet` writes
// "vet: ./main.go:4:2: undefined: x" when a package fails to BUILD, and
// without stripping it the file path ends up inside the message.
func TestParseGoVetFindings(t *testing.T) {
	out := strings.Join([]string{
		"vet: ./main.go:4:2: undefined: undefinedFunc",
		"# github.com/novaworkbench/backend/internal/service",
		"./service.go:12:1: some other problem",
		"",
	}, "\n")

	findings := parseGoVetFindings(out)
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %#v", len(findings), findings)
	}
	if findings[0].Title != "undefined: undefinedFunc" {
		t.Fatalf("title = %q, want the message alone", findings[0].Title)
	}
	if findings[0].Detail != "main.go:4:2" {
		t.Fatalf("detail = %q, want main.go:4:2", findings[0].Detail)
	}
	if findings[1].Detail != "service.go:12:1" {
		t.Fatalf("detail = %q, want service.go:12:1", findings[1].Detail)
	}
	for _, f := range findings {
		if f.Kind != ReviewCandidateDefect {
			t.Fatalf("kind = %q, want %q", f.Kind, ReviewCandidateDefect)
		}
	}
}

// TestParseTscFindings pins the TypeScript-side parser.
func TestParseTscFindings(t *testing.T) {
	out := strings.Join([]string{
		"src/app.ts(12,5): error TS2345: Argument of type 'string' is not assignable.",
		"src/app.ts(20,1): error TS2304: Cannot find name 'foo'.",
		"",
	}, "\n")

	findings := parseTscFindings(out)
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %#v", len(findings), findings)
	}
	if findings[0].Detail != "src/app.ts:12" {
		t.Fatalf("detail = %q, want src/app.ts:12", findings[0].Detail)
	}
	if !strings.Contains(findings[0].Title, "TS2345") {
		t.Fatalf("title = %q, want the TS code in it", findings[0].Title)
	}
}
