package prompt

import (
	"strings"
	"testing"
)

// crossTestProjects — a small participating project set.
func crossTestProjects() []CrossProjectInput {
	return []CrossProjectInput{
		{ID: "proj_web", Name: "web-app", Role: "前端"},
		{ID: "proj_api", Name: "api-svc", Role: "后端"},
		{ID: "proj_mob", Name: "mobile", Role: "移动端"},
	}
}

// 候选解析：项目名 → 项目 id；未知项目名保持未解析；序号重编号为 1..N。
func TestNormalizeCrossSplitCandidates(t *testing.T) {
	raw := `下面是我的拆分说明。

` + "```json" + `
{
  "tasks": [
    {
      "title": "接口定义",
      "description": "定义订单接口",
      "scope": ["api/handler/order.go"],
      "acceptance": ["GET /api/v1/orders/{id} 返回 amount"],
      "boundary": "提供接口契约",
      "project": "web-app",
      "order": 1,
      "depends_on": []
    },
    {
      "title": "订单服务",
      "description": "实现订单服务",
      "scope": [],
      "acceptance": [],
      "boundary": "消费接口契约",
      "project": "api-svc",
      "order": 5,
      "depends_on": [1]
    },
    {
      "title": "幻觉子需求",
      "description": "写到一个不存在的仓库",
      "project": "not-a-project",
      "order": 9,
      "depends_on": [5]
    }
  ]
}
` + "```"

	cands := NormalizeCrossSplitCandidates(raw, crossTestProjects())
	if len(cands) != 3 {
		t.Fatalf("candidates = %d, want 3", len(cands))
	}
	if cands[0].ProjectID != "proj_web" {
		t.Fatalf("cands[0].ProjectID = %q, want proj_web", cands[0].ProjectID)
	}
	if cands[1].ProjectID != "proj_api" {
		t.Fatalf("cands[1].ProjectID = %q, want proj_api", cands[1].ProjectID)
	}
	// An unknown project name stays unresolved — the service rejects the whole
	// batch at confirm time instead of guessing.
	if cands[2].ProjectID != "" {
		t.Fatalf("cands[2].ProjectID = %q, want empty (unknown project)", cands[2].ProjectID)
	}
	if cands[2].Project != "not-a-project" {
		t.Fatalf("cands[2].Project = %q, want the model's own wording", cands[2].Project)
	}

	// The ordinals are renumbered into a contiguous 1..N space.
	for i, c := range cands {
		if c.Order != i+1 {
			t.Fatalf("cands[%d].Order = %d, want %d", i, c.Order, i+1)
		}
	}
	// depends_on is translated into the new space: the model's 1 (old order 1)
	// stays 1; the model's 5 (old order 5 → new order 2) becomes 2.
	if len(cands[1].DependsOn) != 1 || cands[1].DependsOn[0] != 1 {
		t.Fatalf("cands[1].DependsOn = %v, want [1]", cands[1].DependsOn)
	}
	if len(cands[2].DependsOn) != 1 || cands[2].DependsOn[0] != 2 {
		t.Fatalf("cands[2].DependsOn = %v, want [2]", cands[2].DependsOn)
	}
	// An empty acceptance list gets a placeholder so the row is editable.
	if len(cands[1].Acceptance) == 0 {
		t.Fatal("cands[1].Acceptance is empty, want a placeholder")
	}
}

// 无标题的候选被丢弃；无法解析的载荷返回空列表（不是 nil）。
func TestNormalizeCrossSplitCandidates_Edges(t *testing.T) {
	if got := NormalizeCrossSplitCandidates("", crossTestProjects()); len(got) != 0 {
		t.Fatalf("empty payload → %d candidates, want 0", len(got))
	}
	if got := NormalizeCrossSplitCandidates("not json at all", crossTestProjects()); len(got) != 0 {
		t.Fatalf("garbage payload → %d candidates, want 0", len(got))
	}
	raw := `{"tasks":[{"title":"  ","description":"x"},{"title":"好的","project":"web-app"}]}`
	got := NormalizeCrossSplitCandidates(raw, crossTestProjects())
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1 (the untitled one must be dropped)", len(got))
	}
	if got[0].Title != "好的" {
		t.Fatalf("title = %q", got[0].Title)
	}
	// A missing order is derived from the array position.
	if got[0].Order != 1 {
		t.Fatalf("order = %d, want 1", got[0].Order)
	}
}

// 项目名解析：先按 id，再按名称（忽略大小写）。
func TestResolveCrossProjectID(t *testing.T) {
	projects := crossTestProjects()
	if got := ResolveCrossProjectID("proj_api", projects); got != "proj_api" {
		t.Fatalf("by id = %q", got)
	}
	if got := ResolveCrossProjectID("web-app", projects); got != "proj_web" {
		t.Fatalf("by name = %q", got)
	}
	if got := ResolveCrossProjectID("API-SVC", projects); got != "proj_api" {
		t.Fatalf("case-insensitive name = %q", got)
	}
	if got := ResolveCrossProjectID("nope", projects); got != "" {
		t.Fatalf("unknown = %q, want empty", got)
	}
}

// 「跨项目接口契约」章节的检测与抽取。
func TestHasCrossContractSection(t *testing.T) {
	if HasCrossContractSection("## 需求目标\n\n随便写点\n") {
		t.Fatal("a document without the contract section was accepted")
	}
	doc := "## 需求目标\n\n目标\n\n## 跨项目接口契约\n\nGET /api/x\n\n## 各项目落地清单\n\n清单\n"
	if !HasCrossContractSection(doc) {
		t.Fatal("the contract section was not detected")
	}
	// A colon after the heading must not break the detection.
	if !HasCrossContractSection("## 跨项目接口契约：\n\nGET /api/x\n") {
		t.Fatal("the contract section with a trailing colon was not detected")
	}
	section := ExtractCrossContractSection(doc)
	if !strings.Contains(section, "GET /api/x") {
		t.Fatalf("section = %q, want the contract body", section)
	}
	if strings.Contains(section, "各项目落地清单") {
		t.Fatalf("section leaked the next heading: %q", section)
	}
}

// 上下文块必须带上契约与参与项目。
func TestBuildCrossContextBlock(t *testing.T) {
	design := "## 需求目标\n\n统一登录\n\n## 跨项目接口契约\n\nPOST /api/v1/auth/login\n\n## 各项目落地清单\n\n清单内容\n"
	block := BuildCrossContextBlock(design, []CrossProjectRef{
		{ID: "proj_web", Name: "web-app", Role: "前端"},
		{ID: "proj_api", Name: "api-svc", Role: "后端"},
	}, "订单服务")
	if block == "" {
		t.Fatal("block is empty")
	}
	if !strings.Contains(block, "POST /api/v1/auth/login") {
		t.Fatalf("block lost the contract: %q", block)
	}
	if !strings.Contains(block, "web-app") || !strings.Contains(block, "api-svc") {
		t.Fatalf("block lost the project list: %q", block)
	}
	// The summary keeps the rest of the design.
	if !strings.Contains(block, "统一登录") {
		t.Fatalf("block lost the overview: %q", block)
	}
}

// 没有整体方案时上下文块为空 —— 调用方的行为与功能上线前逐字一致。
func TestBuildCrossContextBlock_Empty(t *testing.T) {
	if got := BuildCrossContextBlock("", nil, "标题"); got != "" {
		t.Fatalf("block = %q, want empty", got)
	}
}

// 整体方案的 prompt 必须带上参与项目与契约要求。
func TestBuildCrossDesignPrompt(t *testing.T) {
	p := BuildCrossDesignPrompt("跨仓库订单重构", "让三个端共用一套订单接口",
		[]string{"接口契约可核对"}, crossTestProjects(), "", nil, "", nil)
	if !strings.Contains(p, "跨仓库订单重构") {
		t.Fatalf("prompt lost the title: %q", p)
	}
	if !strings.Contains(p, "web-app") || !strings.Contains(p, "api-svc") {
		t.Fatalf("prompt lost the project list: %q", p)
	}
	if !strings.Contains(p, "跨项目接口契约") {
		t.Fatalf("prompt lost the contract requirement: %q", p)
	}
}

// 整体方案的 prompt 必须带上参与仓库的项目约定 —— 约定不到达模型，各仓库就会
// 各自按自己的习惯实现，跨仓库的接口对不上。
func TestBuildCrossDesignPrompt_CarriesConventions(t *testing.T) {
	p := BuildCrossDesignPrompt("跨仓库订单重构", "让三个端共用一套订单接口",
		[]string{"接口契约可核对"}, crossTestProjects(), "", []CrossProjectDoc{
			{ProjectName: "web-app", Content: "前端约定：组件必须写单测。"},
		}, "", nil)
	if !strings.Contains(p, "前端约定：组件必须写单测。") {
		t.Fatalf("prompt lost the project convention: %q", p)
	}
	// 约定在输出契约**之前**：模型先读约定，再读契约。
	if strings.Index(p, "前端约定") > strings.Index(p, "跨项目接口契约") {
		t.Fatalf("the convention section must come before the output contract: %q", p)
	}
}

// 整体方案的 prompt 必须带上用户上传的附件 —— 用户在需求里附了什么，AI 就该
// 看到什么。附件块在输出契约**之前**：模型先读附件，再读契约。
func TestBuildCrossDesignPrompt_CarriesAttachments(t *testing.T) {
	manifest := "## 📎 已附加文件\n\n### 1. err.log  (1.0 KB)\n**位置**：`/root/.novaworkbench/attachments/xreq_1/att_a__ab12__err.log`\n"
	p := BuildCrossDesignPrompt("跨仓库订单重构", "让三个端共用一套订单接口",
		[]string{"接口契约可核对"}, crossTestProjects(), "", nil, manifest, nil)
	if !strings.Contains(p, "已附加文件") || !strings.Contains(p, "err.log") {
		t.Fatalf("prompt lost the attachment manifest: %q", p)
	}
	if !strings.Contains(p, "att_a__ab12__err.log") {
		t.Fatalf("prompt lost the attachment path: %q", p)
	}
	if strings.Index(p, "已附加文件") > strings.Index(p, "跨项目接口契约") {
		t.Fatalf("the attachment section must come before the output contract: %q", p)
	}
}

// 没有附件时提示词必须逐字节不变 —— 空清单一个字节都不写，这样已有的
// 期望值不需要为「附件这个特性存在」而改。
func TestBuildCrossDesignPrompt_NoAttachments(t *testing.T) {
	withEmpty := BuildCrossDesignPrompt("跨仓库订单重构", "让三个端共用一套订单接口",
		[]string{"接口契约可核对"}, crossTestProjects(), "", nil, "", nil)
	if strings.Contains(withEmpty, "已附加文件") {
		t.Fatalf("an empty manifest must not produce an attachment section: %q", withEmpty)
	}
}

// 拆分与阶段验证的 prompt 同样要带上附件。
func TestBuildCrossSplitAndPhasePrompts_CarryAttachments(t *testing.T) {
	manifest := "## 📎 已附加文件\n\n### 1. err.log  (1.0 KB)\n"

	split := BuildCrossSplitPrompt("跨仓库订单重构", "## 整体方案", crossTestProjects(), nil, manifest)
	if !strings.Contains(split, "err.log") {
		t.Fatalf("split prompt lost the attachment manifest: %q", split)
	}

	phase := BuildCrossPhasePrompt("联调", "前后端打通", nil, "## 整体方案", nil, manifest)
	if !strings.Contains(phase, "err.log") {
		t.Fatalf("phase prompt lost the attachment manifest: %q", phase)
	}
}

// 上下文块带上每个参与项目的基础分支 —— 执行 Agent 必须知道自己那条线的
// 基线，否则会按错误的版本假设写代码。
func TestBuildCrossContextBlock_CarriesBaseBranch(t *testing.T) {
	block := BuildCrossContextBlock("", []CrossProjectRef{
		{ID: "proj_web", Name: "web-app", Role: "前端", BaseBranch: "develop"},
		{ID: "proj_api", Name: "api-svc", Role: "后端", BaseBranch: "release/2026q1"},
		{ID: "proj_mob", Name: "mobile", Role: "移动端"},
	}, "订单服务")
	if !strings.Contains(block, "develop") || !strings.Contains(block, "release/2026q1") {
		t.Fatalf("block lost the base branches: %q", block)
	}
	// 没指定基础分支的项目不写空标签。
	if strings.Count(block, "基础分支") != 2 {
		t.Fatalf("block mentions the base branch %d times, want 2: %q", strings.Count(block, "基础分支"), block)
	}
}
