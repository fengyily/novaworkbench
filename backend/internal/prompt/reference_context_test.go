package prompt

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/novaworkbench/backend/internal/model"
)

func refFixture() model.RequirementReferenceContent {
	return model.RequirementReferenceContent{
		RequirementReference: model.RequirementReference{
			RequirementID: "req_ref_1",
			Note:          "复用其鉴权中间件",
			Origin:        model.ReferenceOriginDirect,
			// Title / Status / Kind / ProjectName are display-only fields. The
			// title in particular must never reach the prompt — the title is a
			// human label, the description already carries the full meaning,
			// and one fewer anchor is one fewer thing the model can over-read.
			Title:       "支付风控需求标题",
			Status:      "done",
			Kind:        "requirement",
			ProjectID:   "proj_2",
			ProjectName: "支付项目",
		},
		Description: "被引用需求的描述正文",
		DesignDocs:  "# 既有方案\n\n第一步做 A。",
	}
}

// The block has two halves and both are load-bearing: the input half (so the
// architect can see the referenced requirement) and the output contract (so the
// referenced content is distilled into this requirement's own plan, which is
// the only plan the coding / sub-task stages ever read).
func TestBuildReferencedContextBlock_InputAndContract(t *testing.T) {
	out := BuildReferencedContextBlock([]model.RequirementReferenceContent{refFixture()})

	for _, want := range []string{
		// input half
		"## 关联需求参考（本需求引用了其它需求）",
		"### 关联需求 1",
		"req_ref_1",
		"复用其鉴权中间件",
		"本需求直接引用",
		"#### 需求描述",
		"被引用需求的描述正文",
		"#### 技术方案",
		"# 既有方案",
		// output contract
		ReferenceContextHeading,
		"### 来源",
		"### 继承的约束与决策",
		"### 本需求与关联需求的分工边界",
		"严禁编造关联",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reference block is missing %q\n---\n%s", want, out)
		}
	}
}

// The regression guard for the whole feature: the referenced requirement's
// TITLE must not appear in the prompt. Titles are for humans; the description
// carries the semantics. If this ever regresses, the model gets an extra
// anchor it can over-interpret — and the "只注入描述 + 方案" contract with the
// requirement owner is broken.
func TestBuildReferencedContextBlock_OmitsTitle(t *testing.T) {
	ref := refFixture()
	out := BuildReferencedContextBlock([]model.RequirementReferenceContent{ref})

	if strings.Contains(out, ref.Title) {
		t.Errorf("reference block leaked the referenced requirement's title %q\n---\n%s", ref.Title, out)
	}
	// The display-only siblings must not leak either.
	for _, leak := range []string{ref.Status, ref.Kind, ref.ProjectName, ref.ProjectID} {
		if leak != "" && strings.Contains(out, leak) {
			t.Errorf("reference block leaked display-only field %q\n---\n%s", leak, out)
		}
	}
	// …while the content the feature IS about must be there.
	if !strings.Contains(out, ref.Description) || !strings.Contains(out, ref.DesignDocs) {
		t.Errorf("reference block lost the description / plan\n---\n%s", out)
	}
}

// The read-only disclaimer is the guard against the model's most likely
// misread: a referenced requirement's description that reads like a set of
// instructions ("不得…", "务必…") getting applied to this requirement.
func TestBuildReferencedContextBlock_FramesReferencesAsReadOnly(t *testing.T) {
	out := BuildReferencedContextBlock([]model.RequirementReferenceContent{refFixture()})
	if !strings.Contains(out, "仅供参考") || !strings.Contains(out, "以本需求为准") {
		t.Errorf("reference block must frame the references as reference-only, got:\n%s", out)
	}
}

// No references must yield "" so the handler can append unconditionally and a
// requirement without references keeps a byte-identical prompt.
func TestBuildReferencedContextBlock_EmptyForNoReferences(t *testing.T) {
	if got := BuildReferencedContextBlock(nil); got != "" {
		t.Errorf("nil references: got %q, want empty", got)
	}
	if got := BuildReferencedContextBlock([]model.RequirementReferenceContent{}); got != "" {
		t.Errorf("empty references: got %q, want empty", got)
	}
	// An entry without an id is not a reference.
	if got := BuildReferencedContextBlock([]model.RequirementReferenceContent{{}}); got != "" {
		t.Errorf("blank-id reference: got %q, want empty", got)
	}
}

// Truncation is rune-based: a byte cut would split a Chinese character and
// emit invalid UTF-8 into the prompt.
func TestBuildReferencedContextBlock_TruncatesOnRuneBoundary(t *testing.T) {
	desc := strings.Repeat("描", 5000)
	ref := refFixture()
	ref.Description = desc
	out := BuildReferencedContextBlock([]model.RequirementReferenceContent{ref})

	if !utf8.ValidString(out) {
		t.Fatal("reference block is not valid UTF-8 after truncation")
	}
	if strings.Contains(out, strings.Repeat("描", 3000)) {
		t.Error("description was not truncated to the 2K rune cap")
	}
	if !strings.Contains(out, "（已截断）") {
		t.Error("truncated content must carry a visible breadcrumb")
	}
}

// A requirement written before the plan-markdown era stores design_docs as a
// JSON array. Rendering that verbatim would put a stray "[]" in the prompt.
func TestBuildReferencedContextBlock_LegacyEmptyDesign(t *testing.T) {
	ref := refFixture()
	ref.DesignDocs = "[]"
	out := BuildReferencedContextBlock([]model.RequirementReferenceContent{ref})
	if !strings.Contains(out, "（该需求尚无方案）") {
		t.Errorf("legacy empty design must render as 「尚无方案」\n---\n%s", out)
	}
	if strings.Contains(out, "#### 技术方案\n[]") {
		t.Errorf("legacy [] leaked into the prompt\n---\n%s", out)
	}
}

// The origin marker tells the model whether the user picked the reference
// directly or whether it was inherited from the master requirement.
func TestBuildReferencedContextBlock_MarksOrigin(t *testing.T) {
	direct := refFixture()
	inherited := refFixture()
	inherited.RequirementID = "req_ref_2"
	inherited.Origin = model.ReferenceOriginParent

	out := BuildReferencedContextBlock([]model.RequirementReferenceContent{direct, inherited})
	if !strings.Contains(out, "本需求直接引用") {
		t.Errorf("direct origin label missing\n---\n%s", out)
	}
	if !strings.Contains(out, "继承自主需求") {
		t.Errorf("inherited origin label missing\n---\n%s", out)
	}
	if !strings.Contains(out, "### 关联需求 2") {
		t.Errorf("entries must be numbered so the contract can point at them\n---\n%s", out)
	}
}

// A reference without a note must not render an empty 「引用原因：」 line —
// it reads like a field the model failed to fill.
func TestBuildReferencedContextBlock_OmitsEmptyNoteLine(t *testing.T) {
	ref := refFixture()
	ref.Note = ""
	out := BuildReferencedContextBlock([]model.RequirementReferenceContent{ref})
	if strings.Contains(out, "引用原因：\n") {
		t.Errorf("empty note must not render a note line\n---\n%s", out)
	}
}

// The whole-block budget keeps one fat reference from starving the others.
func TestBuildReferencedContextBlock_RespectsBlockBudget(t *testing.T) {
	fat := refFixture()
	fat.Description = strings.Repeat("描", 4000)
	fat.DesignDocs = strings.Repeat("案", 8000)
	var refs []model.RequirementReferenceContent
	for i := 0; i < 6; i++ {
		r := fat
		r.RequirementID = "req_ref_" + string(rune('a'+i))
		refs = append(refs, r)
	}

	out := BuildReferencedContextBlock(refs)
	if !strings.Contains(out, "未展开") {
		t.Errorf("dropped entries must be announced\n---\n%s", out)
	}
	// The rendered material stays inside the budget (the contract text on top
	// of it is fixed-size).
	if len([]rune(out)) > refBlockMaxRunes+8192 {
		t.Errorf("block blew past its budget: %d runes", len([]rune(out)))
	}
}

// HasReferencedContextSection drives the append-fallback decision, so it must
// not be fooled by a fenced code block that merely quotes the heading, and it
// must accept the machine-written appendix (otherwise every re-run would
// append another appendix).
func TestHasReferencedContextSection(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want bool
	}{
		{"plain", "## 关联需求上下文\n\n正文", true},
		{"with body", "## 关联需求上下文\n\n### 来源\n", true},
		{"appendix", "## 关联需求上下文（自动附录）\n\n正文", true},
		{"level 3", "### 关联需求上下文\n\n正文", false},
		{"level 4", "#### 关联需求上下文\n\n正文", false},
		// No space after the hashes still counts — same rule as the sibling
		// parent-context check, so the two verifiers can't disagree about how
		// a heading is spelled.
		{"no space", "##关联需求上下文\n\n正文", true},
		{"inside json fence", "```json\n{\"section\": \"## 关联需求上下文\"}\n```\n", false},
		{"inside markdown fence", "```markdown\n## 关联需求上下文\n```\n", false},
		{"inside tilde fence", "~~~\n## 关联需求上下文\n~~~\n", false},
		{"after fence closes", "```\ncode\n```\n\n## 关联需求上下文\n", true},
		{"absent", "# 方案\n\n正文", false},
		// The parent-context heading must not satisfy this one — the two
		// sections are independent contracts.
		{"parent heading", "## 主需求关联上下文\n\n正文", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasReferencedContextSection(c.md); got != c.want {
				t.Errorf("HasReferencedContextSection(%q) = %v, want %v", c.md, got, c.want)
			}
		})
	}
}

// The fallback appendix must carry the reference id + reason so the reader
// can trace it, and — like the input block — never the title.
func TestBuildReferencedContextFallback(t *testing.T) {
	ref := refFixture()
	out := BuildReferencedContextFallback([]model.RequirementReferenceContent{ref})

	for _, want := range []string{
		"## 关联需求上下文（自动附录）",
		"req_ref_1",
		"复用其鉴权中间件",
		"### 关联需求 1",
		"第一步做 A。",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback appendix is missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, ref.Title) {
		t.Errorf("fallback appendix leaked the title %q\n---\n%s", ref.Title, out)
	}
	// Stickiness: the appendix satisfies the section check, so refine / apply
	// won't append another one on the next pass.
	if !HasReferencedContextSection(out) {
		t.Error("fallback appendix must satisfy HasReferencedContextSection (stickiness)")
	}
	if got := BuildReferencedContextFallback(nil); got != "" {
		t.Errorf("nil references: got %q, want empty", got)
	}
}

// A referenced requirement with no plan falls back to its description so the
// appendix is never a bare heading.
func TestBuildReferencedContextFallback_FallsBackToDescription(t *testing.T) {
	ref := refFixture()
	ref.DesignDocs = ""
	out := BuildReferencedContextFallback([]model.RequirementReferenceContent{ref})
	if !strings.Contains(out, "被引用需求的描述正文") {
		t.Errorf("fallback appendix must fall back to the description\n---\n%s", out)
	}
}

// The keep rule is concatenated unconditionally by the refine / apply call
// sites, so it must carry its own leading newlines.
func TestReferencedContextKeepRule_LeadingNewlines(t *testing.T) {
	rule := ReferencedContextKeepRule()
	if !strings.HasPrefix(rule, "\n\n") {
		t.Errorf("keep rule must start with two newlines, got %q", rule[:min(8, len(rule))])
	}
	for _, want := range []string{"保留", "关联需求上下文"} {
		if !strings.Contains(rule, want) {
			t.Errorf("keep rule is missing %q: %s", want, rule)
		}
	}
}
