package prompt

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// ── 待决策事项的解析 ────────────────────────────────────────────────────────

// 设计契约里的「## 待决策事项」章节必须能解析成 chip 列表。
func TestExtractCrossPendingDecisions(t *testing.T) {
	md := `## 需求目标

统一订单接口。

## 跨项目接口契约

GET /api/v1/orders/{id}

## 待决策事项

- **接口默认超时**：订单查询接口的默认超时取 3s 还是 5s？3s 对齐现有网关，5s 给聚合查询留余量。
- **幂等键策略**：下单接口的幂等键用全局 UUID 还是「用户 ID + 业务单号」？
- 没有加粗的写法也能解析
- [ ] 待办形式的待决策点
- （无）

## 集成与验证计划

第一阶段联调。
`
	got := ExtractCrossPendingDecisions(md)
	if len(got) != 4 {
		t.Fatalf("expected 4 decisions, got %d: %+v", len(got), got)
	}
	if got[0].Title != "接口默认超时" {
		t.Fatalf("the bolded label must become the title, got %q", got[0].Title)
	}
	if !strings.Contains(got[0].Detail, "3s 还是 5s") {
		t.Fatalf("the detail must keep the whole line, got %q", got[0].Detail)
	}
	if got[1].Title != "幂等键策略" {
		t.Fatalf("the text before the colon must become the title, got %q", got[1].Title)
	}
	if got[2].Title != "没有加粗的写法也能解析" {
		t.Fatalf("a plain bullet must still parse, got %q", got[2].Title)
	}
	if got[3].Title != "待办形式的待决策点" {
		t.Fatalf("a checkbox item must parse without the checkbox, got %q", got[3].Title)
	}
}

// 「（无）」是「没有待决策」的占位词，不能变成一颗写着「无」的 chip。
func TestExtractCrossPendingDecisions_SkipsPlaceholders(t *testing.T) {
	md := "## 待决策事项\n\n（无）\n"
	if got := ExtractCrossPendingDecisions(md); len(got) != 0 {
		t.Fatalf("the placeholder must not produce a decision, got %+v", got)
	}
	md2 := "## 待决策事项\n\n无\n"
	if got := ExtractCrossPendingDecisions(md2); len(got) != 0 {
		t.Fatalf("the placeholder must not produce a decision, got %+v", got)
	}
}

// 没有这一节时返回空列表 —— 存量方案多半如此，这不是错误。
func TestExtractCrossPendingDecisions_NoSection(t *testing.T) {
	md := "## 需求目标\n\n做点事。\n\n## 硬性要求\n\n1. 中文。\n"
	got := ExtractCrossPendingDecisions(md)
	if got == nil {
		t.Fatalf("an absent section must yield an empty slice, not nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected no decisions, got %+v", got)
	}
}

// 标题匹配刻意宽松：存量方案的待决策点很可能在模型自选的标题下。
func TestExtractCrossPendingDecisions_LooseHeading(t *testing.T) {
	for _, heading := range []string{"## 待决策事项", "## 待确认事项", "### 待定", "## 开放问题", "## 待决策事项："} {
		md := heading + "\n\n- 某个点\n\n## 别的章节\n\n正文\n"
		got := ExtractCrossPendingDecisions(md)
		if len(got) != 1 {
			t.Fatalf("heading %q must be recognised, got %+v", heading, got)
		}
	}
}

// 三级标题也算 —— 模型偶尔会把待决策点写成子章节。
func TestExtractCrossPendingDecisions_Level3(t *testing.T) {
	md := "## 整体方案\n\n### 待决策事项\n\n- 某个点\n\n## 集成\n"
	got := ExtractCrossPendingDecisions(md)
	if len(got) != 1 || got[0].Title != "某个点" {
		t.Fatalf("a level-3 heading must be recognised, got %+v", got)
	}
}

// 条数封顶 —— 一份方案留五十个待决策点不是「留了待决策点」，是「没做决定」。
func TestExtractCrossPendingDecisions_CapsCount(t *testing.T) {
	var b strings.Builder
	b.WriteString("## 待决策事项\n\n")
	for i := 0; i < 50; i++ {
		b.WriteString("- 决策点\n")
	}
	got := ExtractCrossPendingDecisions(b.String())
	if len(got) != crossDecisionMaxCount {
		t.Fatalf("expected the list to be capped at %d, got %d", crossDecisionMaxCount, len(got))
	}
}

// ── 调整对话的提示词 ────────────────────────────────────────────────────────

func crossAdjustInput() CrossAdjustPromptInput {
	return CrossAdjustPromptInput{
		Title:       "跨仓库订单重构",
		Description: "让三个端共用一套订单接口",
		DesignDocs:  "## 跨项目接口契约\n\nGET /api/v1/orders/{id}\n",
		Projects:    crossTestProjects(),
		Conventions: []CrossProjectDoc{{ProjectName: "web-app", Content: "前端约定：组件必须写单测。"}},
		PendingDecisions: []model.CrossPendingDecision{
			{Title: "接口默认超时", Detail: "接口默认超时：3s 还是 5s？"},
		},
		Question: "接口超时取 3s 还是 5s？",
	}
}

// 全新会话的形态必须自带全部上下文 —— 原会话没了，提示词就是唯一的上下文。
func TestBuildCrossAdjustPrompt_FreshForm(t *testing.T) {
	in := crossAdjustInput()
	p := BuildCrossAdjustPrompt(in)

	for _, want := range []string{
		"跨仓库订单重构",
		"让三个端共用一套订单接口",
		"当前整体方案（原文）",
		"GET /api/v1/orders/{id}",
		"接口默认超时",
		"接口超时取 3s 还是 5s？",
		"前端约定：组件必须写单测。",
		"web-app",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("the self-contained prompt lost %q: %q", want, p)
		}
	}
}

// 续接形态是短提示词：会话里已经有方案上下文，重贴一遍纯属浪费。
func TestBuildCrossAdjustPrompt_ResumedForm(t *testing.T) {
	in := crossAdjustInput()
	in.Resumed = true
	in.History = []CrossAdjustTurn{{Question: "上一个问题", Answer: "上一个答案"}}
	p := BuildCrossAdjustPrompt(in)

	if !strings.Contains(p, "接口超时取 3s 还是 5s？") {
		t.Fatalf("the resumed prompt must carry the question: %q", p)
	}
	if strings.Contains(p, "当前整体方案（原文）") {
		t.Fatalf("the resumed prompt must not re-paste the design: %q", p)
	}
	if strings.Contains(p, "上一个答案") {
		t.Fatalf("the resumed prompt must not replay the history: %q", p)
	}
}

// 全新形态要带上此前的问答 —— 它就是「我们之前谈到哪了」的载体。
func TestBuildCrossAdjustPrompt_FreshFormCarriesHistory(t *testing.T) {
	in := crossAdjustInput()
	in.History = []CrossAdjustTurn{{Question: "上一个问题", Answer: "上一个答案"}}
	p := BuildCrossAdjustPrompt(in)
	if !strings.Contains(p, "上一个问题") || !strings.Contains(p, "上一个答案") {
		t.Fatalf("the self-contained prompt must replay the history: %q", p)
	}
}

// 回答契约必须要求那三段固定标题 —— 它们是「重新补全方案」能吃下结论的
// 唯一依据。
func TestBuildCrossAdjustPrompt_DemandsTheThreeSections(t *testing.T) {
	p := BuildCrossAdjustPrompt(crossAdjustInput())
	for _, want := range []string{"## 已确认结论", "## 待决策事项", "## 调整建议"} {
		if !strings.Contains(p, want) {
			t.Fatalf("the answering contract must demand %q: %q", want, p)
		}
	}
	if !strings.Contains(p, "不要输出 json 代码围栏块") {
		t.Fatalf("the answering contract must forbid json fences: %q", p)
	}
}

// ── 调整讨论记录 ────────────────────────────────────────────────────────────

func TestBuildCrossDiscussionBlock(t *testing.T) {
	turns := []CrossAdjustTurn{
		{
			Question: "接口超时取 3s 还是 5s？",
			Answer:   "## 已确认结论\n\n- 接口默认超时取 3s。\n\n## 待决策事项\n\n- 幂等键策略还没定。\n\n## 调整建议\n\n- 无",
		},
	}
	block := BuildCrossDiscussionBlock(turns)
	for _, want := range []string{
		"## 调整讨论记录",
		"接口超时取 3s 还是 5s？",
		"接口默认超时取 3s。",
		"幂等键策略还没定。",
		"只按「已确认结论」修订",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("the discussion block lost %q: %q", want, block)
		}
	}
}

// 没有讨论 → 空串 → 调用方一个字节都不写，提示词与「这个特性存在之前」逐字
// 相同。
func TestBuildCrossDiscussionBlock_Empty(t *testing.T) {
	if got := BuildCrossDiscussionBlock(nil); got != "" {
		t.Fatalf("an empty thread must produce an empty block, got %q", got)
	}
	// 一轮没有答案的对话同样不构成结论。
	turns := []CrossAdjustTurn{{Question: "问了但没人答", Answer: ""}}
	if got := BuildCrossDiscussionBlock(turns); got != "" {
		t.Fatalf("a turn without an answer must produce an empty block, got %q", got)
	}
}

// 讨论记录必须排在「上一版整体方案」之后 —— 模型先读旧版，再读对旧版的讨论。
func TestBuildCrossDesignPrompt_CarriesDiscussion(t *testing.T) {
	discussion := []CrossAdjustTurn{{
		Question: "接口超时取 3s 还是 5s？",
		Answer:   "## 已确认结论\n\n- 接口默认超时取 3s。",
	}}
	p := BuildCrossDesignPrompt("跨仓库订单重构", "让三个端共用一套订单接口",
		[]string{"接口契约可核对"}, crossTestProjects(), "## 上一版方案正文", nil, "", discussion)

	if !strings.Contains(p, "## 调整讨论记录") {
		t.Fatalf("the design prompt must carry the discussion: %q", p)
	}
	if strings.Index(p, "## 调整讨论记录") < strings.Index(p, "## 上一版整体方案") {
		t.Fatalf("the discussion must come after the previous design: %q", p)
	}
	if strings.Index(p, "## 调整讨论记录") > strings.Index(p, "## 跨项目接口契约") {
		t.Fatalf("the discussion must come before the output contract: %q", p)
	}
}

// 没有讨论时提示词里不能出现「调整讨论记录」——这条不变量让「普通的重新
// 生成」与「这个特性存在之前」完全一致。
func TestBuildCrossDesignPrompt_NoDiscussionLeavesPromptUntouched(t *testing.T) {
	p := BuildCrossDesignPrompt("跨仓库订单重构", "让三个端共用一套订单接口",
		[]string{"接口契约可核对"}, crossTestProjects(), "## 上一版方案正文", nil, "", nil)
	if strings.Contains(p, "调整讨论记录") {
		t.Fatalf("an empty discussion must not produce a section: %q", p)
	}
}

// 章节抽取器：从回答里取「已确认结论」/「待决策事项」两段。
func TestExtractCrossAnswerSections(t *testing.T) {
	answer := `回答开头。

## 已确认结论

- 接口默认超时取 3s。

## 待决策事项

- 幂等键还没定。

## 调整建议

- 无
`
	confirmed := ExtractCrossConfirmedConclusions(answer)
	if !strings.Contains(confirmed, "接口默认超时取 3s") {
		t.Fatalf("the confirmed section was not extracted: %q", confirmed)
	}
	if strings.Contains(confirmed, "幂等键") {
		t.Fatalf("the confirmed section must stop at the next heading: %q", confirmed)
	}
	open := ExtractCrossOpenDecisions(answer)
	if !strings.Contains(open, "幂等键还没定") {
		t.Fatalf("the open section was not extracted: %q", open)
	}
	if strings.Contains(open, "接口默认超时") {
		t.Fatalf("the open section must start at its own heading: %q", open)
	}
	// 没有该小节时返回空串（调用方退回原始问答）。
	if got := ExtractCrossConfirmedConclusions("没有小节的回答"); got != "" {
		t.Fatalf("an absent section must yield an empty string, got %q", got)
	}
}

// 契约里必须有「## 待决策事项」这一节 —— 防止有人手滑删掉。
func TestCrossDesignContract_HasPendingDecisionsSection(t *testing.T) {
	if !strings.Contains(CrossDesignContract, "## 待决策事项") {
		t.Fatalf("the design contract must ask for a 「待决策事项」 section")
	}
	if !strings.Contains(CrossDesignContract, "## 跨项目接口契约") {
		t.Fatalf("the design contract must keep the hard contract section")
	}
}
