package prompt

import (
	"strings"
	"testing"
)

// The verifier used to be a plain HasPrefix on the heading text, which failed
// for the single most common way a model writes a heading: with an ordinal in
// front of it. The production symptom was a plan carrying the distilled
// section AND a raw machine appendix saying "the model did not write it".
//
// These tests pin the two behaviours that fix it:
//   - a numbered heading counts as authored;
//   - the machine appendix does NOT count as authored, and is strippable.

func TestMatchContextHeading_Authored(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"plain", "## 主需求关联上下文"},
		{"ascii ordinal", "## 2. 主需求关联上下文"},
		{"ascii ordinal no space", "## 2.主需求关联上下文"},
		{"ideographic comma ordinal", "## 2、主需求关联上下文"},
		{"paren ordinal", "## (2) 主需求关联上下文"},
		{"fullwidth paren ordinal", "## （2）主需求关联上下文"},
		{"chinese numeral ordinal", "## 二、主需求关联上下文"},
		{"trailing colon", "## 主需求关联上下文："},
		{"trailing fullwidth colon", "## 主需求关联上下文："},
		{"indented", "   ## 主需求关联上下文"},
		{"numbered two digits", "## 12. 主需求关联上下文"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MatchContextHeading(c.line, ParentContextTitle); got != ContextHeadingAuthored {
				t.Fatalf("MatchContextHeading(%q) = %v, want Authored", c.line, got)
			}
		})
	}
}

func TestMatchContextHeading_NotAuthored(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"level 3", "### 主需求关联上下文"},
		{"level 4", "#### 主需求关联上下文"},
		{"different section", "## 主需求相关背景"},
		{"prefix is not a word", "## 主需求关联上下文XYZ"},
		{"appendix", "## 主需求关联上下文（自动附录）"},
		{"not a heading", "主需求关联上下文"},
		{"hash in text", "##hashtag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MatchContextHeading(c.line, ParentContextTitle); got == ContextHeadingAuthored {
				t.Fatalf("MatchContextHeading(%q) = Authored, want not-authored", c.line)
			}
		})
	}
}

func TestMatchContextHeading_Appendix(t *testing.T) {
	got := MatchContextHeading("## 主需求关联上下文（自动附录）", ParentContextTitle)
	if got != ContextHeadingAppendix {
		t.Fatalf("got %v, want Appendix", got)
	}
	got = MatchContextHeading("## 关联需求上下文（自动附录）", ReferenceContextTitle)
	if got != ContextHeadingAppendix {
		t.Fatalf("referenced: got %v, want Appendix", got)
	}
}

// The regression that motivated the whole change: the model wrote a numbered
// heading, the verifier said "not written", and the machine appendix was
// appended on top of it.
func TestHasAuthoredParentContextSection_NumberedHeading(t *testing.T) {
	md := "# 技术方案\n\n## 1. 需求理解\n\n正文。\n\n## 2. 主需求关联上下文\n\n### 来源\n\n主需求。\n"
	if !HasAuthoredParentContextSection(md) {
		t.Fatal("numbered heading must count as authored")
	}
	if !HasParentContextSection(md) {
		t.Fatal("the any-form predicate must also accept it")
	}
}

func TestHasAuthoredParentContextSection_AppendixDoesNotCount(t *testing.T) {
	md := "# 技术方案\n\n## 主需求关联上下文（自动附录）\n\n原文摘要。\n"
	if HasAuthoredParentContextSection(md) {
		t.Fatal("the machine appendix must not count as authored")
	}
	if !HasParentContextSection(md) {
		t.Fatal("the any-form predicate must accept the appendix")
	}
}

func TestHasParentContextSection_FenceAware(t *testing.T) {
	md := "# 技术方案\n\n```markdown\n## 主需求关联上下文\n```\n"
	if HasAuthoredParentContextSection(md) {
		t.Fatal("a heading quoted inside a fence documents the contract, it does not satisfy it")
	}
}

// StripContextAppendix must remove the WHOLE section, otherwise the
// appendix's subsections get orphaned under whatever heading follows.
func TestStripContextAppendix_RemovesWholeSection(t *testing.T) {
	md := "# 技术方案\n\n## 主需求关联上下文\n\n模型写的。\n\n" +
		"## 主需求关联上下文（自动附录）\n\n### 主需求方案摘要（截断）\n\n原文……\n\n" +
		"## 9. 实施步骤\n\n正文。\n"
	out := StripContextAppendixParent(md)
	if strings.Contains(out, "（自动附录）") {
		t.Fatalf("appendix heading survived:\n%s", out)
	}
	if strings.Contains(out, "原文……") {
		t.Fatalf("appendix body survived:\n%s", out)
	}
	if !strings.Contains(out, "## 9. 实施步骤") {
		t.Fatalf("the following section was removed:\n%s", out)
	}
	if !strings.Contains(out, "## 主需求关联上下文\n\n模型写的。") {
		t.Fatalf("the authored section was removed:\n%s", out)
	}
}

func TestStripContextAppendix_Noop(t *testing.T) {
	md := "# 技术方案\n\n## 主需求关联上下文\n\n模型写的。\n"
	if out := StripContextAppendixParent(md); out != md {
		t.Fatalf("strip changed a document without an appendix:\n%q", out)
	}
	if out := StripContextAppendixParent(""); out != "" {
		t.Fatalf("strip of empty = %q", out)
	}
}

// Re-running the architect over a plan that already carries an appendix must
// not leave two of them behind.
func TestStripThenAppend_KeepsExactlyOneAppendix(t *testing.T) {
	md := "# 技术方案\n\n## 主需求关联上下文（自动附录）\n\n旧附录。\n"
	md = StripContextAppendixParent(md)
	if strings.Contains(md, "（自动附录）") {
		t.Fatalf("appendix survived the strip:\n%s", md)
	}
	md += "\n\n## 主需求关联上下文（自动附录）\n\n新附录。\n"
	md = StripContextAppendixParent(md)
	md += "\n\n## 主需求关联上下文（自动附录）\n\n新附录。\n"
	if n := strings.Count(md, "## 主需求关联上下文（自动附录）"); n != 1 {
		t.Fatalf("appendix count = %d, want 1:\n%s", n, md)
	}
}

func TestNormalizeContextHeading(t *testing.T) {
	cases := map[string]string{
		"2. 主需求关联上下文":        "主需求关联上下文",
		"2、主需求关联上下文":        "主需求关联上下文",
		"（2）主需求关联上下文":      "主需求关联上下文",
		"二、主需求关联上下文":        "主需求关联上下文",
		"主需求关联上下文：":          "主需求关联上下文",
		"  主需求关联上下文  ":       "主需求关联上下文",
		"主需求关联上下文（自动附录）": "主需求关联上下文（自动附录）",
		"十进制":                    "十进制",
	}
	for in, want := range cases {
		if got := NormalizeContextHeading(in); got != want {
			t.Errorf("NormalizeContextHeading(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasAuthoredReferencedContextSection_NumberedHeading(t *testing.T) {
	md := "# 技术方案\n\n## 3. 关联需求上下文\n\n### 来源\n\n- 关联需求 1\n"
	if !HasAuthoredReferencedContextSection(md) {
		t.Fatal("numbered heading must count as authored")
	}
}
