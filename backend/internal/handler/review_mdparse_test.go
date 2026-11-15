package handler

import (
	"strings"
	"testing"
)

// The body parser is the deterministic second source of a Review report's
// grading: when the model's trailing JSON contract block is missing, the
// report's own P0/P1/P3 sections have to carry the finding. These cases are
// the shapes models actually produce.

func TestParseMarkdownFindings_NewFormat(t *testing.T) {
	md := "## 总体评价\n\n整体尚可，但有一个必须修复的问题。\n\n" +
		"## P0 — 必须修复\n\n" +
		"**[P0] 除零 panic** — `util/x.go:5` — 这里直接做了除法而没有判零 — 建议：调用前补一个 `if b == 0` 判断\n\n" +
		"## P1 — 建议修复\n\n" +
		"**[P1] 命名不规范** — `api/handler.go:120` — 变量名用了缩写 — 建议：改成完整单词\n\n" +
		"## P3 — 优化项建议\n\n" +
		"**[P3] 缺少注释** — `api/handler.go:1` — 导出的函数没有文档注释 — 建议：补一段说明\n"

	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if len(findings) != 3 {
		t.Fatalf("len(findings) = %d, want 3: %+v", len(findings), findings)
	}

	p0 := findings[0]
	if p0.Severity != "P0" {
		t.Errorf("severity = %q, want P0", p0.Severity)
	}
	if p0.Title != "除零 panic" {
		t.Errorf("title = %q, want 除零 panic", p0.Title)
	}
	if p0.File != "util/x.go" || p0.Line != "5" {
		t.Errorf("location = %q:%q, want util/x.go:5", p0.File, p0.Line)
	}
	if !strings.Contains(p0.Detail, "没有判零") {
		t.Errorf("detail = %q, want the problem description", p0.Detail)
	}
	if !strings.Contains(p0.Suggestion, "补一个") {
		t.Errorf("suggestion = %q, want the 建议 tail", p0.Suggestion)
	}

	// The section heading is the authoritative bucket, and the [P1] inside
	// the title must not be mistaken for it.
	if findings[1].Severity != "P1" || findings[2].Severity != "P3" {
		t.Errorf("severities = %q/%q, want P1/P3", findings[1].Severity, findings[2].Severity)
	}
	if findings[1].Title != "命名不规范" {
		t.Errorf("title = %q, want the [P1] prefix stripped", findings[1].Title)
	}
}

// The OLD item shape (no severity prefix, no 建议 tail) must still parse —
// the parser has to read reports written before the format was pinned.
func TestParseMarkdownFindings_LegacyFormat(t *testing.T) {
	md := "## P1 — 建议修复\n\n" +
		"**命名** — `api/x.go:12`，这里命名不清晰。\n\n" +
		"**空指针** — `api/y.go:30`，这里没有判空。\n"

	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if len(findings) != 2 {
		t.Fatalf("len(findings) = %d, want 2: %+v", len(findings), findings)
	}
	if findings[0].Title != "命名" || findings[0].File != "api/x.go" || findings[0].Line != "12" {
		t.Errorf("first finding = %+v", findings[0])
	}
	if findings[1].Title != "空指针" || findings[1].File != "api/y.go" {
		t.Errorf("second finding = %+v", findings[1])
	}
	if findings[0].Severity != "P1" {
		t.Errorf("severity = %q, want P1", findings[0].Severity)
	}
}

// Heading variants: the bucket may be followed by anything.
func TestParseMarkdownFindings_HeadingVariants(t *testing.T) {
	cases := []string{
		"## P0 — 必须修复",
		"## P0：必须修复",
		"## P0 问题",
		"## P0",
		"## p1 建议修复",
	}
	for _, heading := range cases {
		md := heading + "\n\n**标题** — `a.go:1`，说明\n"
		findings, ok := parseMarkdownFindings(md)
		if !ok || len(findings) != 1 {
			t.Errorf("%q: parse failed (%v, %+v)", heading, ok, findings)
			continue
		}
		if findings[0].Title != "标题" {
			t.Errorf("%q: title = %q", heading, findings[0].Title)
		}
	}
}

// An empty section says "no problems here" and must produce nothing.
func TestParseMarkdownFindings_EmptySections(t *testing.T) {
	md := "## P0 — 必须修复\n\n无\n\n## P1 — 建议修复\n\nN/A\n\n## P3 — 优化项建议\n\n暂无\n"
	if findings, ok := parseMarkdownFindings(md); ok {
		t.Fatalf("empty sections must not produce findings, got %+v", findings)
	}
}

// P2 is not a bucket in this product — a section for it is ignored, exactly
// like the contract parser drops a P2 finding.
func TestParseMarkdownFindings_IgnoresP2(t *testing.T) {
	md := "## P2 — 一般问题\n\n**不该被收录** — `a.go:1`，说明\n\n## P1 — 建议修复\n\n**该被收录** — `b.go:2`，说明\n"
	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Title != "该被收录" {
		t.Fatalf("finding = %+v", findings[0])
	}
}

// A summary heading that names two buckets is not a severity section.
func TestParseMarkdownFindings_IgnoresSummaryHeadings(t *testing.T) {
	md := "## P0/P1 汇总\n\n**不该被收录** — `a.go:1`，说明\n"
	if findings, ok := parseMarkdownFindings(md); ok {
		t.Fatalf("a summary heading must not produce findings, got %+v", findings)
	}
}

// A report without any graded section yields nothing.
func TestParseMarkdownFindings_NoSections(t *testing.T) {
	md := "## 总体评价\n\n一切正常。\n\n## 优点\n\n结构清晰。\n"
	if findings, ok := parseMarkdownFindings(md); ok {
		t.Fatalf("no severity section must not produce findings, got %+v", findings)
	}
}

// Several items in one section keep their order.
func TestParseMarkdownFindings_KeepsOrder(t *testing.T) {
	md := "## P1 — 建议修复\n\n" +
		"- **第一** — `a.go:1`，说明一\n" +
		"- **第二** — `b.go:2`，说明二\n" +
		"- **第三** — `c.go:3`，说明三\n"
	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if len(findings) != 3 {
		t.Fatalf("len(findings) = %d, want 3", len(findings))
	}
	for i, want := range []string{"第一", "第二", "第三"} {
		if findings[i].Title != want {
			t.Errorf("findings[%d].Title = %q, want %q", i, findings[i].Title, want)
		}
	}
}

// A line number may be written in a separate code span after the path.
func TestParseMarkdownFindings_SeparateLineSpan(t *testing.T) {
	md := "## P0 — 必须修复\n\n**标题** — `api/x.go` `42`，这里有问题。\n"
	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if findings[0].File != "api/x.go" || findings[0].Line != "42" {
		t.Fatalf("location = %q:%q, want api/x.go:42", findings[0].File, findings[0].Line)
	}
}

// A line range in the path span is kept as-is.
func TestParseMarkdownFindings_LineRange(t *testing.T) {
	md := "## P1 — 建议修复\n\n**标题** — `api/x.go:12-20`，这里有问题。\n"
	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if findings[0].Line != "12-20" {
		t.Fatalf("line = %q, want 12-20", findings[0].Line)
	}
}

// A section written as paragraphs (no list markers) still parses.
func TestParseMarkdownFindings_ParagraphSection(t *testing.T) {
	md := "## P0 — 必须修复\n\n**标题** — `api/x.go:1`，这里有问题。\n\n**另一个标题** — `api/y.go:2`，这里也有问题。\n"
	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if len(findings) != 2 {
		t.Fatalf("len(findings) = %d, want 2: %+v", len(findings), findings)
	}
}

// A continuation line under a list item belongs to that item, not to a new
// one.
func TestParseMarkdownFindings_ContinuationLines(t *testing.T) {
	md := "## P1 — 建议修复\n\n" +
		"- **标题** — `api/x.go:1`，第一行说明\n" +
		"  第二行说明\n" +
		"- **另一个** — `api/y.go:2`，说明\n"
	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if len(findings) != 2 {
		t.Fatalf("len(findings) = %d, want 2: %+v", len(findings), findings)
	}
	if !strings.Contains(findings[0].Detail, "第二行说明") {
		t.Errorf("detail = %q, want the continuation line folded in", findings[0].Detail)
	}
}

// The detail must not carry the title or the location away with it.
func TestParseMarkdownFindings_DetailExcludesTitleAndLocation(t *testing.T) {
	md := "## P0 — 必须修复\n\n**[P0] 标题** — `api/x.go:9` — 问题描述 — 建议：改成这样\n"
	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if strings.Contains(findings[0].Detail, "标题") {
		t.Errorf("detail = %q, want the title stripped", findings[0].Detail)
	}
	if strings.Contains(findings[0].Detail, "api/x.go") {
		t.Errorf("detail = %q, want the location stripped", findings[0].Detail)
	}
	if findings[0].Suggestion != "改成这样" {
		t.Errorf("suggestion = %q, want 改成这样", findings[0].Suggestion)
	}
}

// A sub-heading (###) must not steal the severity of its section.
func TestParseMarkdownFindings_SubHeadingStaysInSection(t *testing.T) {
	md := "## P1 — 建议修复\n\n### 细节\n\n**标题** — `api/x.go:1`，说明\n"
	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1: %+v", len(findings), findings)
	}
	if findings[0].Severity != "P1" {
		t.Fatalf("severity = %q, want P1", findings[0].Severity)
	}
}

// The preamble before the first heading is never a finding.
func TestParseMarkdownFindings_PreambleIgnored(t *testing.T) {
	md := "这是一段前言，提到 P0 但不是小节。\n\n## P1 — 建议修复\n\n**标题** — `api/x.go:1`，说明\n"
	findings, ok := parseMarkdownFindings(md)
	if !ok {
		t.Fatal("parseMarkdownFindings returned not-ok")
	}
	if len(findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1: %+v", len(findings), findings)
	}
}
