package handler

import (
	"strings"
	"testing"
)

// TestExtractSubtasksPayload_Fenced covers the happy path — agent emits a
// ```json … ``` fence and ends with the [SUBTASKS_READY] sentinel.
func TestExtractSubtasksPayload_Fenced(t *testing.T) {
	text := `# 拆分计划

| # | 子任务 | 说明 |
|---|--------|------|
| 1 | 登录接口 | POST /api/login |
| 2 | bcrypt | 加密密码 |

` + "```json\n" + `{"subtasks":[
  {"title":"登录接口","prompt":"实现 POST /api/login"},
  {"title":"bcrypt","prompt":"用 bcrypt 加密用户密码"}
]}
` + "```" + `
[SUBTASKS_READY]`
	payload, ok := extractSubtasksPayload(text)
	if !ok {
		t.Fatal("expected ok=true for fenced payload + sentinel")
	}
	if len(payload.Subtasks) != 2 {
		t.Fatalf("expected 2 subtasks, got %d", len(payload.Subtasks))
	}
	if payload.Subtasks[0].Title != "登录接口" {
		t.Fatalf("first title=%q", payload.Subtasks[0].Title)
	}
	if !strings.Contains(payload.Subtasks[1].Prompt, "bcrypt") {
		t.Fatalf("second prompt=%q", payload.Subtasks[1].Prompt)
	}
}

// TestExtractSubtasksPayload_PromptEmbedsCodeFence is the req_9d24ef181a5ad5c4
// regression: a subtask prompt contained a "\n```css\n…\n```\n" snippet inside
// its JSON string value. The old "first ``` after the fence" cut truncated the
// payload mid-JSON, the unmarshal failed, and the caller silently degraded to
// the Markdown table heuristic — which dispatched a single garbage child built
// from the table's header row instead of the two real subtasks.
func TestExtractSubtasksPayload_PromptEmbedsCodeFence(t *testing.T) {
	text := "## 任务拆分\n\n" +
		"| # | 子任务 | 说明 |\n|---|--------|------|\n" +
		"| 1 | 调列宽 | 改 CSS |\n| 2 | 验证 | 跑 lint |\n\n" +
		"```json\n" +
		"{\n  \"subtasks\": [\n" +
		"    {\n      \"title\": \"调整列宽\",\n" +
		"      \"prompt\": \"建议新增：\\n   ```css\\n   .req-table tbody td:first-child { white-space: nowrap; }\\n   ```\\n   注意不要覆盖响应式规则。\"\n    },\n" +
		"    {\n      \"title\": \"lint + 构建验证\",\n      \"prompt\": \"跑 npm run lint 和 npm run build\"\n    }\n  ]\n}\n" +
		"```\n\n[SUBTASKS_READY]"
	payload, ok := extractSubtasksPayload(text)
	if !ok {
		t.Fatal("expected ok=true when a prompt embeds an inner code fence")
	}
	if len(payload.Subtasks) != 2 {
		t.Fatalf("expected 2 subtasks, got %d", len(payload.Subtasks))
	}
	if payload.Subtasks[0].Title != "调整列宽" {
		t.Errorf("first title=%q", payload.Subtasks[0].Title)
	}
	if !strings.Contains(payload.Subtasks[0].Prompt, "white-space: nowrap") {
		t.Errorf("first prompt lost its embedded css block: %q", payload.Subtasks[0].Prompt)
	}
	if payload.Subtasks[1].Title != "lint + 构建验证" {
		t.Errorf("second title=%q", payload.Subtasks[1].Title)
	}
}

// TestExtractSubtasksPayload_NoSentinel returns nil + false when the agent
// answered a normal question without committing to a decomposition.
func TestExtractSubtasksPayload_NoSentinel(t *testing.T) {
	text := `# 拆分计划

我建议拆成 3 块。

` + "```json\n" + `{"subtasks":[{"title":"x","prompt":"y"}]}
` + "```"
	if _, ok := extractSubtasksPayload(text); ok {
		t.Fatal("expected ok=false without [SUBTASKS_READY] sentinel")
	}
}

// TestExtractSubtasksPayload_NoFence exercises the brace-match fallback so
// agents that omit the ```json fence still parse correctly.
func TestExtractSubtasksPayload_NoFence(t *testing.T) {
	text := `# 拆分计划

` + `{"subtasks":[{"title":"a","prompt":"do A"},{"title":"b","prompt":"do B"}]}

[SUBTASKS_READY]`
	payload, ok := extractSubtasksPayload(text)
	if !ok {
		t.Fatal("expected ok=true for brace-fallback")
	}
	if len(payload.Subtasks) != 2 {
		t.Fatalf("expected 2 subtasks, got %d", len(payload.Subtasks))
	}
}

// TestExtractSubtasksPayload_DropsEmpty ensures malformed entries (empty
// prompt) don't spawn a no-op child agent. A single valid entry survives.
func TestExtractSubtasksPayload_DropsEmpty(t *testing.T) {
	text := "```json\n{\"subtasks\":[" +
		"{\"title\":\"valid\",\"prompt\":\"do a\"}," +
		"{\"title\":\"empty\",\"prompt\":\"\"}," +
		"{\"title\":\"also empty\",\"prompt\":\"   \"}" +
		"]}\n```\n[SUBTASKS_READY]"
	payload, ok := extractSubtasksPayload(text)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(payload.Subtasks) != 1 {
		t.Fatalf("expected only the valid entry to survive, got %d", len(payload.Subtasks))
	}
	if payload.Subtasks[0].Title != "valid" {
		t.Fatalf("survived entry title=%q", payload.Subtasks[0].Title)
	}
}

// TestExtractSubtasksPayload_AllEmpty ensures a JSON block whose every
// subtask has an empty prompt returns nil + false (no children created).
func TestExtractSubtasksPayload_AllEmpty(t *testing.T) {
	text := "```json\n{\"subtasks\":[{\"title\":\"x\",\"prompt\":\"\"}]}\n```\n[SUBTASKS_READY]"
	if _, ok := extractSubtasksPayload(text); ok {
		t.Fatal("expected ok=false when every prompt is empty")
	}
}

// TestExtractSubtasksPayload_BadJSON: malformed JSON in the fence returns
// nil + false rather than spawning zero children (callers fall back to
// rendering the chat reply as-is).
func TestExtractSubtasksPayload_BadJSON(t *testing.T) {
	text := "```json\n{\"subtasks\":[THIS IS NOT JSON]}\n```\n[SUBTASKS_READY]"
	if _, ok := extractSubtasksPayload(text); ok {
		t.Fatal("expected ok=false for malformed JSON")
	}
}

// TestExtractSubtasksPayload_DefaultTitle ensures an entry with empty
// title gets a 40-char truncated preview of its prompt so the SubTaskPanel
// always has something to render.
func TestExtractSubtasksPayload_DefaultTitle(t *testing.T) {
	longPrompt := strings.Repeat("x", 100)
	text := "```json\n{\"subtasks\":[{\"title\":\"\",\"prompt\":\"" + longPrompt + "\"}]}\n```\n[SUBTASKS_READY]"
	payload, ok := extractSubtasksPayload(text)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(payload.Subtasks[0].Title) == 0 || len(payload.Subtasks[0].Title) > 50 {
		t.Fatalf("expected default title preview 1-50 chars, got %d", len(payload.Subtasks[0].Title))
	}
	if !strings.HasSuffix(payload.Subtasks[0].Title, "…") {
		t.Fatalf("expected truncated title to end with ellipsis, got %q", payload.Subtasks[0].Title)
	}
}

// ---------------------------------------------------------------------------
// Markdown fallback tests (extractSubtasksFromMarkdown).
//
// Exercises the permissive parser that fires when the main agent emits a
// task breakdown table but forgets the JSON fence or [SUBTASKS_READY]
// sentinel. Real users hit this case whenever Claude Code answers "开始开发"
// with a Markdown list only — we want those children to still auto-dispatch
// so the UX promise ("主Agent 拆分 → 后端自动派发") holds even when the agent
// doesn't follow the structured format perfectly.
// ---------------------------------------------------------------------------

func TestExtractSubtasksFromMarkdown_DashColon(t *testing.T) {
	text := `## 任务分解

- 修复登录空指针：定位 LoginHandler 第 42 行未判空 username 的分支
- 添加单元测试：在 internal/auth/login_test.go 新增 3 个用例
- 更新 README：在登录章节补充错误码说明

等待用户确认。`
	p := extractSubtasksFromMarkdown(text)
	if p == nil || len(p.Subtasks) != 3 {
		t.Fatalf("expected 3 subtasks, got %v", p)
	}
	if p.Subtasks[0].Title != "修复登录空指针" {
		t.Errorf("first title=%q", p.Subtasks[0].Title)
	}
	if !strings.Contains(p.Subtasks[0].Prompt, "LoginHandler") {
		t.Errorf("first prompt=%q", p.Subtasks[0].Prompt)
	}
}

func TestExtractSubtasksFromMarkdown_NumberedFullWidthColon(t *testing.T) {
	text := `## 子任务清单

1. 修改 user 表 schema：新增 avatar_url 字段
2. 实现上传接口：在 /api/users/{id}/avatar 支持 multipart
3. 前端组件：在用户设置页加头像上传按钮`
	p := extractSubtasksFromMarkdown(text)
	if p == nil || len(p.Subtasks) != 3 {
		t.Fatalf("expected 3 subtasks, got %v", p)
	}
	if p.Subtasks[0].Title != "修改 user 表 schema" {
		t.Errorf("first title=%q", p.Subtasks[0].Title)
	}
}

func TestExtractSubtasksFromMarkdown_BoldEmDash(t *testing.T) {
	text := `## 任务清单

- **修复登录 bug** — 在 auth/login.go 中增加空 username 防御
- **补单测** — 新增 internal/auth/login_test.go 覆盖 3 个 case

其他内容略。`
	p := extractSubtasksFromMarkdown(text)
	if p == nil || len(p.Subtasks) != 2 {
		t.Fatalf("expected 2 subtasks, got %v", p)
	}
	if p.Subtasks[0].Title != "修复登录 bug" {
		t.Errorf("title=%q (expected bold stripped)", p.Subtasks[0].Title)
	}
}

func TestExtractSubtasksFromMarkdown_StopsAtNextSibling(t *testing.T) {
	text := `## 任务分解

- 修复 A
- 修复 B

## 其他说明

这些是其他内容。`
	p := extractSubtasksFromMarkdown(text)
	if p == nil || len(p.Subtasks) != 2 {
		t.Fatalf("expected 2 subtasks (heading stops at next heading), got %v", p)
	}
}

func TestExtractSubtasksFromMarkdown_SingleBulletSkipped(t *testing.T) {
	// A single bullet is usually prose, not a real plan — we don't want to
	// auto-spawn a child for "就这一步".
	text := `## 任务分解

- 就这一步

别的没了。`
	if p := extractSubtasksFromMarkdown(text); p != nil {
		t.Fatalf("expected nil for single bullet, got %v", p)
	}
}

func TestExtractSubtasksFromMarkdown_NoHeading(t *testing.T) {
	if p := extractSubtasksFromMarkdown("一些普通的输出"); p != nil {
		t.Fatalf("expected nil without ## 任务分解, got %v", p)
	}
}

func TestExtractSubtasksFromMarkdown_TableHeadersFiltered(t *testing.T) {
	text := `## 任务分解

| 子任务 | 说明 |
|---|---|
| 实现缓存层 | 在 service/cache 包内新增 Redis 客户端封装 |
| 增加指标 | 通过 OpenTelemetry 暴露命中率 |

继续。`
	p := extractSubtasksFromMarkdown(text)
	if p == nil || len(p.Subtasks) != 2 {
		t.Fatalf("expected 2 subtasks (table headers filtered), got %v", p)
	}
	if p.Subtasks[0].Title != "实现缓存层" {
		t.Errorf("first title=%q (expected column-1 title)", p.Subtasks[0].Title)
	}
	if !strings.Contains(p.Subtasks[1].Prompt, "OpenTelemetry") {
		t.Errorf("second prompt=%q", p.Subtasks[1].Prompt)
	}
}

// TestExtractSubtasksFromMarkdown_IndexColumnTable is the second half of the
// req_9d24ef181a5ad5c4 regression: the fallback parser must skip the header
// row of a "| # | 子任务 | 涉及文件 | …" table (title "#" is not a task) and
// read data-row titles from the SECOND column, not the row index.
func TestExtractSubtasksFromMarkdown_IndexColumnTable(t *testing.T) {
	text := `## 任务拆分

| # | 子任务 | 涉及文件 | 关键改动 | 产物 |
|---|--------|---------|---------|------|
| 1 | 调整 RequirementsList 类型列宽度 | RequirementsList.tsx | 加 width: 70 | 修改后的 TSX |
| 2 | 前端 lint + 构建验证 | frontend/ | 跑 npm run lint | 成功日志 |

完成。`
	p := extractSubtasksFromMarkdown(text)
	if p == nil || len(p.Subtasks) != 2 {
		t.Fatalf("expected 2 subtasks (index column shifted), got %v", p)
	}
	if p.Subtasks[0].Title != "调整 RequirementsList 类型列宽度" {
		t.Errorf("first title=%q (expected 2nd column, not the row index)", p.Subtasks[0].Title)
	}
	if !strings.Contains(p.Subtasks[0].Prompt, "RequirementsList.tsx") {
		t.Errorf("first prompt=%q (expected remaining cells joined)", p.Subtasks[0].Prompt)
	}
	if p.Subtasks[1].Title != "前端 lint + 构建验证" {
		t.Errorf("second title=%q", p.Subtasks[1].Title)
	}
	for _, st := range p.Subtasks {
		if st.Title == "#" || st.Title == "1" || st.Title == "2" {
			t.Errorf("index/header leaked into subtask title: %q", st.Title)
		}
	}
}

// TestExtractSubtasksPayload_StillStrict guards the refactor: when the JSON
// path produces a payload, the markdown fallback MUST NOT run. This is
// already enforced by the sentinel-gated signature but the test makes the
// intent explicit so a future refactor can't accidentally widen the parse.
func TestExtractSubtasksPayload_StillStrict(t *testing.T) {
	text := "```json\n{\"subtasks\":[{\"title\":\"only-json\",\"prompt\":\"p\"}]}\n```\n[SUBTASKS_READY]"
	p, ok := extractSubtasksPayload(text)
	if !ok || len(p.Subtasks) != 1 || p.Subtasks[0].Title != "only-json" {
		t.Fatalf("JSON path regressed: %+v ok=%v", p, ok)
	}
}

// TestDeveloperDecomposePrompt_AlwaysTriggers guards the fix for
// req_04acb22d06fe3525: the fresh-session (skip-design) StartCoding path used
// to send a bare "## title\n\n desc" with no decomposition trigger, so the
// developer role never emitted [SUBTASKS_READY] and no children were
// dispatched. Both the fresh and fork paths now build the prompt via
// developerDecomposePrompt, which MUST always carry the "开始开发" trigger
// and the [SUBTASKS_READY] sentinel instruction — regardless of the lead-in
// line that distinguishes the two paths.
func TestDeveloperDecomposePrompt_AlwaysTriggers(t *testing.T) {
	cases := map[string]string{
		"fresh (skip-design)": "请先读取项目中的相关文件理解现有代码结构与需求上下文，然后立即完成**任务拆分**：\n",
		"fork (has-design)":   "基于已完成的需求分析与技术方案，请立即完成**任务拆分**：\n",
	}
	for name, leadIn := range cases {
		t.Run(name, func(t *testing.T) {
			p := developerDecomposePrompt("优化需求列表类型列宽度", leadIn, "/tmp/workdir")
			// The trigger phrase the developer system prompt keys its
			// decomposition + sentinel emission on.
			if !strings.Contains(p, "开始开发") {
				t.Errorf("prompt missing 开始开发 trigger:\n%s", p)
			}
			// The Write-tool primary channel: the prompt must name the exact
			// subtasks.json path the backend captures from the Write tool_use.
			if !strings.Contains(p, "/tmp/workdir/.novaworkbench/subtasks.json") {
				t.Errorf("prompt missing Write-tool subtasks.json path:\n%s", p)
			}
			// The sentinel instruction — without it the agent never emits
			// [SUBTASKS_READY] and tryAutoOrchestrate dispatches nothing.
			if !strings.Contains(p, "[SUBTASKS_READY]") {
				t.Errorf("prompt missing [SUBTASKS_READY] sentinel instruction:\n%s", p)
			}
			// The lead-in must appear so the two paths still differ in
			// context framing (read-files-first vs reference-prior-design).
			if !strings.Contains(p, leadIn) {
				t.Errorf("prompt missing lead-in:\n%s", p)
			}
			// Title is interpolated.
			if !strings.Contains(p, "优化需求列表类型列宽度") {
				t.Errorf("prompt missing requirement title:\n%s", p)
			}
		})
	}
}

