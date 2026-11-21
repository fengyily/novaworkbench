package prompt

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

func TestExtractSectionBody(t *testing.T) {
	md := "# 技术方案\n\n" +
		"## 2. 主需求关联上下文\n\n" +
		"### 来源\n\n主需求。\n\n" +
		"### 与主需求的偏差\n\n" +
		"- [冲突] 帧构造归属 —— protocol.c:871\n" +
		"- [新事实] 页地址池收窄到 1024\n\n" +
		"## 9. 实施步骤\n\n正文。\n"
	body := ExtractSectionBody(md, FindingsSourceSection)
	if body == "" {
		t.Fatal("section body not found")
	}
	if got := len(ExtractSectionBullets(md, FindingsSourceSection)); got != 2 {
		t.Fatalf("bullet count = %d, want 2 (body: %q)", got, body)
	}
}

func TestExtractSectionBody_StopsAtSameLevel(t *testing.T) {
	md := "### 与主需求的偏差\n\n- 一条\n\n## 下一节\n\n- 不该被收进来\n"
	if got := len(ExtractSectionBullets(md, FindingsSourceSection)); got != 1 {
		t.Fatalf("bullet count = %d, want 1", got)
	}
}

func TestExtractSectionBody_Missing(t *testing.T) {
	if got := ExtractSectionBody("# 方案\n\n正文。\n", FindingsSourceSection); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := ExtractSectionBullets("", FindingsSourceSection); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestExtractSectionBullets_IgnoresProse(t *testing.T) {
	md := "### 与主需求的偏差\n\n这里是一段说明。\n\n- 一条\n\n* 另一条\n\n1. 第三条\n"
	got := ExtractSectionBullets(md, FindingsSourceSection)
	if len(got) != 3 {
		t.Fatalf("bullet count = %d, want 3: %v", len(got), got)
	}
}

func TestExtractSectionBullets_FenceAware(t *testing.T) {
	md := "### 与主需求的偏差\n\n```markdown\n- 文档里的示例\n```\n\n- 真正的发现\n"
	got := ExtractSectionBullets(md, FindingsSourceSection)
	if len(got) != 1 {
		t.Fatalf("bullet count = %d, want 1: %v", len(got), got)
	}
}

func TestBuildFindingsAbsorbBlock_Empty(t *testing.T) {
	if got := BuildFindingsAbsorbBlock(nil); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestBuildFindingsAbsorbBlock_RendersChecklist(t *testing.T) {
	block := BuildFindingsAbsorbBlock([]model.RequirementFinding{
		{RequirementID: "req_child", Stage: "design", Title: "帧构造归属", Detail: "protocol.c:871"},
	})
	if block == "" {
		t.Fatal("block is empty")
	}
	for _, want := range []string{
		"必须吸收的变更",
		"req_child",
		"帧构造归属",
		"protocol.c:871",
		FindingsAbsorbHeading,
	} {
		if !contains(block, want) {
			t.Fatalf("block is missing %q:\n%s", want, block)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
