package handler

import (
	"strings"
	"testing"
)

// The trailing-JSON extractor is the one piece of the review pipeline that
// has to survive an uncooperative model. These cases mirror the real failure
// modes: braces in the prose (R2), a missing contract block (R1), and more
// than one JSON block in the reply.

func TestExtractTrailingJSON_FencedBlock(t *testing.T) {
	report := "## 总体评价\n\n建议修复后合入。\n\n```json\n{\"goal_met\":\"partial\",\"findings\":[]}\n```\n"
	jsonPart, md := extractTrailingJSON(report)
	if jsonPart != `{"goal_met":"partial","findings":[]}` {
		t.Fatalf("jsonPart = %q", jsonPart)
	}
	if strings.Contains(md, "goal_met") {
		t.Fatalf("markdown still carries the contract block: %q", md)
	}
	if !strings.Contains(md, "总体评价") {
		t.Fatalf("markdown lost the report body: %q", md)
	}
}

// Prose braces are exactly what wizard_stream.go's extractJSON (which scans
// from the FIRST '{') would trip over, so this is the regression that
// justifies a separate extractor.
func TestExtractTrailingJSON_IgnoresProseBraces(t *testing.T) {
	report := "## P0 — 必须修复\n\n" +
		"**空指针** — `a.go:12`，这里 `cfg := Config{Retries: 3}` 没有判空。\n\n" +
		"```go\nif m := map[string]int{\"a\": 1}; len(m) > 0 {}\n```\n\n" +
		"```json\n{\"goal_met\":\"no\",\"findings\":[{\"severity\":\"P0\",\"file\":\"a.go\",\"line\":\"12\",\"title\":\"空指针\",\"detail\":\"d\",\"suggestion\":\"s\"}]}\n```"
	jsonPart, md := extractTrailingJSON(report)
	findings, goalMet, ok := parseFindings(jsonPart)
	if !ok {
		t.Fatalf("parse failed for %q", jsonPart)
	}
	if goalMet != "no" {
		t.Fatalf("goalMet = %q, want no", goalMet)
	}
	if len(findings) != 1 || findings[0].File != "a.go" {
		t.Fatalf("findings = %+v", findings)
	}
	if !strings.Contains(md, "Config{Retries: 3}") {
		t.Fatalf("prose brace snippet was eaten: %q", md)
	}
}

// Multiple JSON blocks: the LAST one is the contract (an earlier one is
// typically an illustrative snippet inside a finding's suggestion).
func TestExtractTrailingJSON_PicksLastBlock(t *testing.T) {
	report := "示例配置：\n\n```json\n{\"timeout\": 30}\n```\n\n" +
		"## P1 — 建议修复\n\n无\n\n" +
		"```json\n{\"goal_met\":\"yes\",\"findings\":[]}\n```"
	jsonPart, md := extractTrailingJSON(report)
	if !strings.Contains(jsonPart, "goal_met") {
		t.Fatalf("picked the wrong block: %q", jsonPart)
	}
	if !strings.Contains(md, `{"timeout": 30}`) {
		t.Fatalf("earlier snippet should survive in the markdown: %q", md)
	}
}

// No contract block at all — the whole report stays as markdown and the
// caller degrades to plain rendering.
func TestExtractTrailingJSON_NoBlock(t *testing.T) {
	report := "## 总体评价\n\n没有发现问题。代码结构 {清晰}。\n"
	jsonPart, md := extractTrailingJSON(report)
	if jsonPart != "" {
		t.Fatalf("expected no json, got %q", jsonPart)
	}
	if md != report {
		t.Fatalf("markdown should be untouched, got %q", md)
	}
	if _, _, ok := parseFindings(jsonPart); ok {
		t.Fatal("parseFindings should report not-ok for an empty block")
	}
}

// A bare (unfenced) trailing object is the documented fallback.
func TestExtractTrailingJSON_UnfencedTrailingObject(t *testing.T) {
	report := "## 总体评价\n\n可以合入。\n\n{\"goal_met\":\"yes\",\"findings\":[]}"
	jsonPart, md := extractTrailingJSON(report)
	if jsonPart != `{"goal_met":"yes","findings":[]}` {
		t.Fatalf("jsonPart = %q", jsonPart)
	}
	if strings.Contains(md, "goal_met") {
		t.Fatalf("markdown still carries the object: %q", md)
	}
}

func TestParseFindings_DropsUnknownSeverityAndCounts(t *testing.T) {
	raw := `{"goal_met":"PARTIAL","findings":[
		{"severity":"P0","title":"a"},
		{"severity":"p1","title":"b"},
		{"severity":"P2","title":"ignored"},
		{"severity":"P3","title":"c"},
		{"severity":"P1","title":"d"}
	]}`
	findings, goalMet, ok := parseFindings(raw)
	if !ok {
		t.Fatal("parse failed")
	}
	if goalMet != "partial" {
		t.Fatalf("goalMet = %q, want partial (case-normalised)", goalMet)
	}
	if len(findings) != 4 {
		t.Fatalf("len(findings) = %d, want 4 (P2 dropped)", len(findings))
	}
	p0, p1, p3 := countBySeverity(findings)
	if p0 != 1 || p1 != 2 || p3 != 1 {
		t.Fatalf("counts = (%d,%d,%d), want (1,2,1)", p0, p1, p3)
	}
}

func TestParseFindings_Malformed(t *testing.T) {
	if _, _, ok := parseFindings(`{"goal_met":`); ok {
		t.Fatal("truncated JSON should not parse")
	}
	// Valid JSON but no recognised verdict → empty goalMet, still ok.
	findings, goalMet, ok := parseFindings(`{"goal_met":"maybe","findings":[]}`)
	if !ok {
		t.Fatal("valid JSON should parse")
	}
	if goalMet != "" {
		t.Fatalf("goalMet = %q, want empty for an unknown verdict", goalMet)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %+v", findings)
	}
}

// buildReviewOutcome is the glue between the stream tail and the persisted
// row. The two branches that matter are "model honoured the contract" and
// "model didn't" — the latter must stay a success, not an error.

func TestBuildReviewOutcome_ContractHonoured(t *testing.T) {
	report := "## 总体评价\n\n建议修复 P0 后合入。\n\n## P0 — 必须修复\n\n**除零 panic** — `util/x.go:5`\n\n" +
		"```json\n{\"goal_met\":\"partial\",\"findings\":[" +
		"{\"severity\":\"P0\",\"file\":\"util/x.go\",\"line\":\"5\",\"title\":\"除零 panic\",\"detail\":\"d\",\"suggestion\":\"s\"}," +
		"{\"severity\":\"P3\",\"file\":\"util/x.go\",\"line\":\"\",\"title\":\"补注释\",\"detail\":\"d\",\"suggestion\":\"s\"}]}\n```"

	out := buildReviewOutcome("done", report)
	if out.Status != "done" {
		t.Fatalf("status = %q", out.Status)
	}
	if out.GoalMet != "partial" {
		t.Fatalf("goalMet = %q", out.GoalMet)
	}
	if out.P0 != 1 || out.P1 != 0 || out.P3 != 1 {
		t.Fatalf("counts = (%d,%d,%d), want (1,0,1)", out.P0, out.P1, out.P3)
	}
	if strings.Contains(out.ResultMD, "goal_met") {
		t.Fatalf("result_md should have the contract block stripped: %q", out.ResultMD)
	}
	if !strings.Contains(out.ResultMD, "总体评价") {
		t.Fatalf("result_md lost the report: %q", out.ResultMD)
	}
	if !strings.Contains(out.FindingsJSON, "除零 panic") {
		t.Fatalf("findings_json = %q", out.FindingsJSON)
	}
}

// R1: a model that ignores the contract must still produce a usable row —
// the full report as Markdown, no findings, zero counts.
func TestBuildReviewOutcome_DegradesWithoutContract(t *testing.T) {
	report := "## 总体评价\n\n没有发现阻塞问题。\n\n## P1 — 建议修复\n\n**命名** — `a.go`"
	out := buildReviewOutcome("done", report)
	if out.ResultMD != report {
		t.Fatalf("result_md should be the untouched report, got %q", out.ResultMD)
	}
	if out.FindingsJSON != "" || out.P0 != 0 || out.P1 != 0 || out.P3 != 0 || out.GoalMet != "" {
		t.Fatalf("degraded row should be empty/zero, got %+v", out)
	}
}

// Malformed JSON in the fence is the same degraded case, not a crash.
func TestBuildReviewOutcome_MalformedContract(t *testing.T) {
	report := "## 总体评价\n\n还行。\n\n```json\n{\"goal_met\": \"yes\", \"findings\": [\n```"
	out := buildReviewOutcome("done", report)
	if out.FindingsJSON != "" || out.P0+out.P1+out.P3 != 0 {
		t.Fatalf("malformed contract should degrade, got %+v", out)
	}
	if !strings.Contains(out.ResultMD, "总体评价") {
		t.Fatalf("result_md lost the report: %q", out.ResultMD)
	}
}

// An empty findings array is a valid contract result — zero counts but a
// stripped report and a real goal_met verdict.
func TestBuildReviewOutcome_EmptyFindings(t *testing.T) {
	report := "## 总体评价\n\n可以直接合入。\n\n```json\n{\"goal_met\":\"yes\",\"findings\":[]}\n```"
	out := buildReviewOutcome("done", report)
	if out.GoalMet != "yes" {
		t.Fatalf("goalMet = %q", out.GoalMet)
	}
	if out.FindingsJSON != "[]" {
		t.Fatalf("findings_json = %q, want []", out.FindingsJSON)
	}
	if strings.Contains(out.ResultMD, "goal_met") {
		t.Fatalf("contract block not stripped: %q", out.ResultMD)
	}
}

// The error path carries the failure message as the report body so the UI
// has something to show.
func TestBuildReviewOutcome_ErrorStatus(t *testing.T) {
	out := buildReviewOutcome("error", `无法解析对比分支: 分支 "x" 在本地与 origin 上都不存在`)
	if out.Status != "error" {
		t.Fatalf("status = %q", out.Status)
	}
	if !strings.Contains(out.ResultMD, "都不存在") {
		t.Fatalf("result_md = %q", out.ResultMD)
	}
}
