package prompt

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/novaworkbench/backend/internal/model"
)

func parentFixture() *model.Requirement {
	return &model.Requirement{
		ID:          "req_parent",
		ProjectID:   "proj_1",
		Title:       "主需求标题",
		Description: "主需求描述正文",
		DesignDocs:  "# 主方案\n\n第一步做 A。",
	}
}

// The block has two halves and both are load-bearing: the input half (so the
// architect can see the parent) and the output contract (so the parent content
// is distilled into this requirement's own plan, which is the only plan the
// coding / sub-task stages ever read). A block missing either half silently
// regresses to the bug this feature fixes.
func TestBuildParentContextBlock_InputAndContract(t *testing.T) {
	manifest := "## 📎 已附加文件\n\n### 1. spec.pdf  (1 KB)\n**位置**：`/root/spec.pdf`\n"
	out := BuildParentContextBlock(parentFixture(), manifest)

	for _, want := range []string{
		// input half
		"## 主需求参考（本需求是其子需求）",
		"主需求标题",
		"req_parent",
		"主需求描述正文",
		"# 主方案",
		"#### 主需求的附件",
		"spec.pdf",
		// output contract
		ParentContextHeading,
		"### 来源",
		"### 继承的约束与决策",
		"### 本子需求与主需求的分工边界",
		"### 相关附件",
		"严禁编造关联",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("parent block is missing %q\n---\n%s", want, out)
		}
	}

	// The attachment manifest heading must be rewritten: an un-rewritten
	// 「## 📎 已附加文件」 would collide with the sub-requirement's own
	// identically-named section and leave the model unable to tell whose
	// files are whose.
	if strings.Contains(out, "## 📎 已附加文件") {
		t.Errorf("attachment manifest heading was not rewritten for the parent block\n---\n%s", out)
	}
}

// The read-only disclaimer is the guard against the model's most likely
// misread here: a parent description that reads like a set of instructions
// ("不得…", "务必…") getting applied to the sub-requirement.
func TestBuildParentContextBlock_FramesParentAsReferenceOnly(t *testing.T) {
	out := BuildParentContextBlock(parentFixture(), "")
	if !strings.Contains(out, "仅供参考") || !strings.Contains(out, "以本需求为准") {
		t.Errorf("parent block must frame the parent as reference-only, got:\n%s", out)
	}
}

// No parent (or a half-populated one) must yield "" so the handler can append
// unconditionally and a top-level requirement's prompt stays byte-identical
// to today's — the no-regression guarantee for every existing requirement.
func TestBuildParentContextBlock_EmptyForNilParent(t *testing.T) {
	for name, p := range map[string]*model.Requirement{
		"nil":   nil,
		"blank": {ID: "  "},
	} {
		if got := BuildParentContextBlock(p, ""); got != "" {
			t.Errorf("%s parent: got %q, want empty", name, got)
		}
	}
}

// design_docs predating the plan-markdown era stored a JSON array, so an
// untouched row reads as 「[]」 rather than "". Both must render as the friendly
// "no design yet" sentence instead of leaking a bare JSON token into the prompt.
func TestBuildParentContextBlock_EmptyAndLegacyDesign(t *testing.T) {
	for name, design := range map[string]string{
		"empty":     "",
		"spaces":    "   \n\t ",
		"legacy []": "[]",
		"legacy {}": "{}",
	} {
		p := parentFixture()
		p.DesignDocs = design
		out := BuildParentContextBlock(p, "")
		if !strings.Contains(out, "（主需求尚无方案）") {
			t.Errorf("design=%q: want 「（主需求尚无方案）」, got:\n%s", name, out)
		}
		if strings.Contains(out, "[]") {
			t.Errorf("design=%q: raw legacy token leaked into the block:\n%s", name, out)
		}
	}
}

func TestBuildParentContextBlock_NoAttachments(t *testing.T) {
	out := BuildParentContextBlock(parentFixture(), "   ")
	if !strings.Contains(out, "（主需求无附件）") {
		t.Errorf("missing manifest must render 「（主需求无附件）」, got:\n%s", out)
	}
	if strings.Contains(out, parentAttachHeading+" \n") {
		t.Errorf("unexpected trailing space after heading")
	}
}

// Truncation is what bounds how much of a large parent plan can eat the design
// run's context budget — and it must cut on rune boundaries. A byte cut would
// split a 3-byte Chinese character and emit invalid UTF-8 into the prompt,
// which is exactly the corruption a naive len(s[:n]) would cause.
func TestBuildParentContextBlock_TruncatesOnRuneBoundary(t *testing.T) {
	p := parentFixture()
	p.Description = strings.Repeat("中", parentDescMaxRunes+500)
	p.DesignDocs = strings.Repeat("方", parentDesignMaxRunes+500)

	out := BuildParentContextBlock(p, "")

	if !utf8.ValidString(out) {
		t.Fatal("truncated block is not valid UTF-8 — rune boundary was not respected")
	}
	if !strings.Contains(out, "（已截断，完整内容见主需求）") {
		t.Errorf("truncation breadcrumb missing:\n%s", out[:min(400, len(out))])
	}
	// Both fields are present, so each must be cut exactly once.
	if got := strings.Count(out, "（已截断，完整内容见主需求）"); got != 2 {
		t.Errorf("want 2 truncation notices (description + design), got %d", got)
	}
	// The whole point of the cap: the block must not have grown to hold both
	// oversized fields in full.
	if len([]rune(out)) > parentDescMaxRunes+parentDesignMaxRunes+4000 {
		t.Errorf("block %d runes — caps not enforced", len([]rune(out)))
	}
}

// Short content must pass through untouched: the breadcrumb must not appear
// when nothing was actually cut.
func TestBuildParentContextBlock_ShortContentNotMarkedTruncated(t *testing.T) {
	out := BuildParentContextBlock(parentFixture(), "")
	if strings.Contains(out, "（已截断，完整内容见主需求）") {
		t.Errorf("short content must not be marked truncated:\n%s", out)
	}
}

// HasParentContextSection drives the append-fallback decision in
// finalizeArchitectRun. A false positive would silently drop the parent
// context, so these cases pin the strict side of the boundary.
func TestHasParentContextSection(t *testing.T) {
	tests := []struct {
		name string
		md   string
		want bool
	}{
		{
			name: "level-2 heading present",
			md:   "# 方案\n\n## 需求理解\n\n正文\n\n" + ParentContextHeading + "\n\n### 来源\n",
			want: true,
		},
		{
			name: "fallback appendix variant counts",
			md:   "# 方案\n\n" + parentContextFallbackHeading + "\n\n- 主需求标题：x\n",
			want: true,
		},
		{
			name: "no space after hashes",
			md:   "##主需求关联上下文\n\n正文\n",
			want: true,
		},
		{
			name: "absent",
			md:   "# 方案\n\n## 需求理解\n\n正文\n",
			want: false,
		},
		{
			name: "level-3 heading does not count",
			md:   "# 方案\n\n### 主需求关联上下文\n\n正文\n",
			want: false,
		},
		{
			name: "inside a fenced code block does not count",
			md:   "# 方案\n\n示例：\n\n```json\n{\n  \"section\": \"" + ParentContextHeading + "\"\n}\n```\n\n后续正文\n",
			want: false,
		},
		{
			name: "fenced block in the middle does not mask a later real heading",
			md:   "```\n" + ParentContextHeading + "\n```\n\n" + ParentContextHeading + "\n\n正文\n",
			want: true,
		},
		{
			name: "tilde fence is honoured too",
			md:   "~~~\n" + ParentContextHeading + "\n~~~\n\n正文\n",
			want: false,
		},
		{
			name: "empty",
			md:   "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasParentContextSection(tt.md); got != tt.want {
				t.Errorf("HasParentContextSection = %v, want %v\ninput:\n%s", got, tt.want, tt.md)
			}
		})
	}
}

// The fallback is the hard guarantee behind the soft prompt contract: even
// when the model ignores the contract, the persisted plan must carry *some*
// trace of the parent or downstream (which only reads this plan) contradicts it.
func TestBuildParentContextFallback(t *testing.T) {
	out := BuildParentContextFallback(parentFixture())
	for _, want := range []string{
		parentContextFallbackHeading,
		"主需求标题",
		"req_parent",
		"主方案",
		"未经提炼",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback is missing %q\n---\n%s", want, out)
		}
	}
	// Sticky: the appendix must itself satisfy the verifier, or every re-run
	// would append another one.
	if !HasParentContextSection(out) {
		t.Errorf("fallback appendix must satisfy HasParentContextSection:\n%s", out)
	}
}

func TestBuildParentContextFallback_NilParent(t *testing.T) {
	if got := BuildParentContextFallback(nil); got != "" {
		t.Errorf("nil parent: got %q, want empty", got)
	}
}

func TestBuildParentContextFallback_Truncates(t *testing.T) {
	p := parentFixture()
	p.DesignDocs = strings.Repeat("案", parentFallbackDesignMaxRunes+500)
	out := BuildParentContextFallback(p)
	if !utf8.ValidString(out) {
		t.Fatal("truncated fallback is not valid UTF-8")
	}
	if len([]rune(out)) > parentFallbackDesignMaxRunes+1000 {
		t.Errorf("fallback %d runes — cap not enforced", len([]rune(out)))
	}
}

// A parent with a description but no plan is common (parent still in
// analyst/design). Falling back to the description beats writing "既无方案也无描述".
func TestBuildParentContextFallback_DescriptionWhenNoDesign(t *testing.T) {
	p := parentFixture()
	p.DesignDocs = "[]"
	out := BuildParentContextFallback(p)
	if !strings.Contains(out, "主需求描述正文") {
		t.Errorf("fallback should fall back to the parent description:\n%s", out)
	}
}

func TestParentContextKeepRule(t *testing.T) {
	out := ParentContextKeepRule()
	if !strings.Contains(out, "保留") {
		t.Errorf("keep rule must instruct retention, got: %q", out)
	}
	if !strings.HasPrefix(out, "\n\n") {
		t.Errorf("keep rule must carry its own leading newlines so call sites can concatenate unconditionally")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
