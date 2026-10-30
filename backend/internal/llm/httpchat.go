package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// formatAndTitleSystemPrompt reorganizes the user's raw, free-form requirement
// text into a structured Markdown document AND distills a concise title, in a
// single LLM round — merging what used to be two separate chat completions so
// the title and body stay consistent and the content is transmitted once. It
// must not invent scope or technical detail — only reorganize what the user
// wrote. The model returns a JSON object so the title and Markdown can be split
// reliably without delimiter parsing.
const formatAndTitleSystemPrompt = `你是一位需求分析师。请把用户给出的需求描述整理成结构清晰的 Markdown 文档，并提炼一个简洁的需求标题。
以 JSON 对象输出，格式严格为：{"title":"...","markdown":"..."}，不要用代码块围栏包裹整体输出。
title：不超过20个汉字（或15个英文单词）；不要加标点符号、引号或换行；不要任何额外说明或前缀。
markdown：纯 Markdown 正文；按以下结构组织，缺信息的章节直接省略，不要编造内容：## 需求背景、## 目标、## 功能要点（用列表）、## 验收标准（用列表）、## 备注。
保留用户表达的关键信息与术语，不要扩大或缩小需求范围，不要臆测技术实现方案。
只输出该 JSON 对象，不要任何前后缀说明。`

// formatAndTitleIssueSystemPrompt distills an Issue (bug report) into a
// bug-shaped Markdown: 现象描述 / 复现步骤 / 期望行为 / 实际行为. No
// 验收标准 / 背景 / 功能要点 — Issues aren't features.
const formatAndTitleIssueSystemPrompt = `你是一位 Bug 分析助手。请把用户给出的问题描述整理成结构清晰的 Markdown 文档，并提炼一个简洁的问题标题。
以 JSON 对象输出，格式严格为：{"title":"...","markdown":"..."}，不要用代码块围栏包裹整体输出。
title：不超过20个汉字（或15个英文单词），用动词+宾语概括（如"修复 X 页面 500 错"）；不要加标点符号、引号或换行；不要任何额外说明或前缀。
markdown：纯 Markdown 正文；按以下结构组织，缺信息的章节直接省略，不要编造内容：## 现象描述、## 复现步骤（用有序列表）、## 期望行为、## 实际行为、## 备注。
保留用户给出的报错信息、堆栈、日志片段（如有）；不要臆测根因、不要扩大问题范围、不要补写功能需求或验收标准。
只输出该 JSON 对象，不要任何前后缀说明。`

// formatAndTitleIdeaSystemPrompt distills an Idea (exploratory note) into a
// lightweight shape: 灵感来源 / 初步设想 / 待回答的关键问题. Never a 验收标准
// — Ideas aren't yet committed features.
const formatAndTitleIdeaSystemPrompt = `你是一位想法记录助手。请把用户给出的灵感或想法整理成结构清晰的 Markdown 文档，并提炼一个简洁的想法标题。
以 JSON 对象输出，格式严格为：{"title":"...","markdown":"..."}，不要用代码块围栏包裹整体输出。
title：不超过20个汉字（或15个英文单词），一句话表达核心想法；不要加标点符号、引号或换行；不要任何额外说明或前缀。
markdown：纯 Markdown 正文；按以下结构组织，缺信息的章节直接省略，不要编造内容：## 灵感来源、## 初步设想、## 待回答的关键问题（用列表）、## 备注。
不要求结构化需求模板，不要写"验收标准"或"功能要点"这种已经确定要做的章节；保留用户的发散与不确定性。
只输出该 JSON 对象，不要任何前后缀说明。`

// summarizeIdeaToRequirementPrompt converts a finished idea-discussion thread
// (initial idea + multi-turn analyst chat + acceptance_criteria list) into a
// fully-fledged requirement draft. The output must follow the SAME shape as
// the standard requirement template (背景/目标/功能要点/验收标准) so the new
// requirement can drop straight into the 3-stage pipeline. Distinguish the
// "what was decided in the discussion" from speculation; if the discussion
// didn't actually commit a feature (still pure exploration), return empty
// markdown so the caller can refuse rather than fabricate a feature.
const summarizeIdeaToRequirementPrompt = `你是一位资深需求分析师。用户给出一段「想法」及其与 AI 助手的完整讨论记录，请把讨论中**已经达成共识**、**明确要实现**的部分提炼成一份可开发的需求文档（Markdown）。
以 JSON 对象输出，格式严格为：{"title":"...","markdown":"...","acceptance_criteria":["...","..."]}，不要用代码块围栏包裹整体输出。
title：不超过20个汉字（或15个英文单词），动词+宾语概括（如"实现 X 模块的 Y 能力"）；不要加标点符号、引号或换行；不要任何额外说明或前缀。
markdown：纯 Markdown 正文；按以下结构组织，缺信息的章节直接省略，不要编造内容：## 需求背景（缘起）、## 目标、## 功能要点（用列表）、## 备注。
acceptance_criteria：数组，每项一句话，描述"如何验证这个需求做完了"，不要列功能点。
重要约束：
- 只总结讨论中已经确定要做的内容，不要把"待回答的问题"、"备选思路"、"未确认的担忧"误当成需求条目。
- 如果整段讨论还停留在发散阶段（用户最终没有决定要做什么），把 markdown 设为空字符串 ""，title 设为"（未达成共识）"，acceptance_criteria 设为[]，让前端可以拒绝这次总结并保留原想法。
- 不要补写讨论中从未出现过的功能，不要臆测技术实现方案。
只输出该 JSON 对象，不要任何前后缀说明。`

// extractStepsSystemPrompt converts the planner persona's plan-mode
// implementation-steps Markdown into the sub-task envelope the orchestration
// layer already speaks. The envelope key is deliberately "subtasks" (not
// "steps") so the caller can reuse the existing decodeSubtasksPayload /
// normalizePayload parsers rather than growing a second, drifting one.
//
// The "self-contained prompt" rule is the load-bearing instruction here: each
// dispatched child agent forks the planning session but is given ONLY its own
// prompt as the -p message, so a step that says "同上一步" is unexecutable.
const extractStepsSystemPrompt = `你是一位软件项目调度助手。用户会给你一份「实施步骤计划」的 Markdown，请把其中的步骤列表提取成结构化的子任务列表。
只输出一个 JSON 对象，不要输出任何其他文字、解释或 markdown 代码围栏。
格式严格为：{"subtasks":[{"title":"<子任务标题>","prompt":"<执行该步骤的完整提示词>"}]}
规则：
- 计划里的每一个编号步骤对应一个子任务，保持原有顺序；序号本身（1、2、#）不是标题。
- title：一句话概括该步骤要做的事，不超过 40 个字。
- prompt 必须自包含：执行该步骤的子 Agent 看不到这份计划、看不到其他步骤，只能看到你写的 prompt。因此要把该步骤的目标、涉及文件路径、具体改动内容、产物形式、验收点都完整写进 prompt，必要的上下文宁可重复也不要用「见上一步」「同方案所述」这类指代。
- 不要合并步骤，也不要把一个步骤拆成多个；不要新增计划里没有的步骤。
- 计划里的前言、背景、风险分析等非步骤章节不要变成子任务。
- 如果这份文本里确实没有任何可执行的步骤列表，输出 {"subtasks":[]}。
只输出该 JSON 对象，不要任何前后缀说明。`

// extractReportKnowledgePrompt distills a development report (主 Agent 汇总报告
// or 子任务报告) into a reusable knowledge-base entry. The output is
// {title, markdown} only — caller appends a source-of-origin footer and a
// knowledge row gets persisted. Output must be JSON without code fences; the
// "no fabrication" rule keeps AI hallucination out of the next requirement's
// prompt (the knowledge base is injected into analyst / architect / coding
// stages verbatim, so garbage in = garbage out).
const extractReportKnowledgePrompt = `你是 NovaWorkbench 的知识库整理助手。
用户会给一份开发报告（主 Agent 汇总报告或子任务报告）的原始 Markdown。请提炼为可复用的项目知识条目。
只输出一个 JSON 对象，不要输出任何其他文字、解释或 markdown 代码围栏。
格式严格为：{"title":"<不超过 30 字的知识标题>","markdown":"<提炼后的 Markdown 正文>"}
提炼规则：
- 保留：实现了什么能力、关键文件与模块路径（绝对路径或 repo 相对路径）、技术决策与取舍理由、踩到的坑 / 约束 / 后续注意事项、对外接口或配置变更。
- 丢弃：进度播报、重试 / 失败过程、token 用量、"我将要…" 这类过程性叙述、纯寒暄、上下无关的代码块。
- 禁止臆造报告里没有的信息；如报告没有任何可沉淀内容，markdown 返回空串，title 返回一个描述性短句。
- 输出纯 Markdown，建议章节：## 实现概述 / ## 涉及文件 / ## 关键决策 / ## 注意事项。
只输出该 JSON 对象，不要任何前后缀说明。`

// pdfToMarkdownPrompt asks the LLM to clean up raw text extracted from a PDF
// (often noisy — page numbers, repeated headers/footers, hyphenated line
// breaks, glued tables) into structured Markdown suitable for the project
// knowledge base. The source filename is passed in the user message as a
// hint; the model returns a single Markdown document (no JSON envelope, no
// surrounding ```markdown fence) so the caller can persist the string
// verbatim into knowledge.content.
//
// Hard bans:
//   - mermaid / flowchart / diagram code blocks (knowledge rows are read as
//     documents, not architecture diagrams; the diagram flow owns its own
//     knowledge rows).
//   - any wrapping code fence around the whole response (MarkdownRender
//     would render the fence literally).
//   - fabrication: the model must only reorganize what was in the source —
//     no invented sections, no inferred technical detail. If the source is
//     empty / meaningless, return an empty string so the caller can
//     surface a clean parse failure instead of writing hallucinated content.
const pdfToMarkdownPrompt = `你是 NovaWorkbench 的 PDF 文档整理助手。
用户会给出一份从 PDF 抽取出的原始文本（可能含断行噪声、页码、重复页眉、表格粘连），以及原始文件名。请整理为结构清晰、便于检索的 Markdown 文档。

输出要求：
- 直接输出 Markdown 正文，不要用任何代码围栏（三个反引号加语言名、或裸的三个反引号）包裹整体输出；不要输出 mermaid 等图表代码块；不要任何前后缀说明。
- 以一个 # 一级标题开头；标题尽量反映文档主题（必要时参考原始文件名）。
- 合理使用 ## 二级标题、列表、必要时用 Markdown 表格语法还原原始文本中的表格。
- 清理：删除页码（如「第 1 页」「Page 2/10」）、重复的页眉页脚、与正文无关的导航/版权噪声；合并被错误断行的句子。
- 保留：原始章节结构、关键技术术语、API 端点、文件路径、引用与数据。
- 如果原始文本几乎为空或没有可整理的内容，直接输出空字符串（不要凑字）。`

// generateDiagramPrompt asks the LLM to pick the BEST Mermaid diagram type
// for the user's input and emit strict Mermaid source. Mirrors
// extractReportKnowledgePrompt's JSON-in-prompt shape (parser does
// stripJSONFences + json.Unmarshal). The kind field is a suggestion from the
// caller when known (e.g. the user picked "流程图" in the UI); when empty the
// model is free to pick. The mermaid field is the raw Mermaid source WITHOUT
// any ```mermaid fence — it's substituted into the knowledge row verbatim so
// MarkdownRender can render it inline.
//
// The "no fabrication" rule from extractReportKnowledgePrompt applies: don't
// invent components / endpoints / contracts the user didn't mention.
const generateDiagramPrompt = `你是 NovaWorkbench 的架构可视化助手。
用户会给你一段文字（需求描述、知识条目正文、项目结构片段、个人笔记…）。
请判断最适合表达其内容的 Mermaid 图类型,并输出一张图。

只输出一个 JSON 对象,不要任何前后缀文字、解释或 markdown 代码围栏。

严格输出格式:
{"kind":"flowchart|sequenceDiagram|stateDiagram-v2|classDiagram|erDiagram|gantt","title":"<不超过 30 字的中文标题>","mermaid":"<纯 Mermaid 源码,不要任何三反引号围栏>","description":"<一两句话说明这张图表达了什么>"}

规则:
- flowchart 用 TD 或 LR;节点 id 用字母数字,中文标签用 ["中文"] 包裹;避免空格与连字符在 id 内。
- sequenceDiagram 必须先声明 participant,箭头方向 ->>(实线)/-->>(虚线);不要在同一图里混中英 id。
- stateDiagram-v2 用 [*] 表示起止。
- erDiagram 关系用 ||..|| / }o--o{ 等标准符号。
- gantt / classDiagram 同理,优先采用最简洁能表达意图的语法。
- 关键文件路径、API 端点等放在外侧 description 字段,不要塞进 mermaid 节点文字里。
- 如果用户输入实在无法可视化(完全无结构,或纯寒暄),mermaid 字段返回空串,title 返回一个说明性短句,kind 留空。
只输出该 JSON 对象,不要任何前后缀说明。`

// generateDiagramResult is the JSON shape generateDiagramPrompt produces.
// Fields match the prompt's four keys exactly so json.Unmarshal passes without
// custom names. Title / Mermaid / Description are trimmed by the caller.
type generateDiagramResult struct {
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Mermaid     string `json:"mermaid"`
	Description string `json:"description"`
}

// summarizeIdeaToRequirementResult is the JSON shape summarizeIdeaToRequirementPrompt
// produces. The service treats an empty Markdown as "discussion didn't converge"
// and refuses to create a new requirement.
type summarizeIdeaToRequirementResult struct {
	Title              string   `json:"title"`
	Markdown           string   `json:"markdown"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
}

// formatAndTitleResult is the JSON shape the model is asked to emit for the
// combined format-and-title task.
type formatAndTitleResult struct {
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
}

// chatRequest is the OpenAI-compatible chat completions request body. Model
// uses omitempty so a self-hosted gateway with a default model can omit it.
type chatRequest struct {
	Model       string         `json:"model,omitempty"`
	Messages    []chatMessage  `json:"messages"`
	Temperature float64        `json:"temperature"`
	MaxTokens   int            `json:"max_tokens"`
	Stream      bool           `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the minimal slice of the OpenAI-compatible response needed
// to extract the assistant text and token usage.
//
// ReasoningContent is the DeepSeek-R1 / o1-style field that some gateways
// return INSTEAD of (or alongside) Content when the model is in reasoning
// mode. We don't render it to the user, but on an empty Content we treat it
// as a fallback so a reasoning-enabled model that finished thinking but
// emitted an empty final answer is surfaced with a useful diagnostic instead
// of a bare "empty response" — the model's reasoning trace is logged for
// postmortem but NOT returned to the caller (would be noise to the user).
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
		} `json:"message"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage,omitempty"`
}

// chatUsage is the OpenAI-compatible usage object. Some self-hosted gateways
// omit it, so it is a pointer (nil → unreported).
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Usage is the token consumption of one LLM round over the direct HTTP
// channel. The claude CLI stream path reads usage directly from the result
// event instead, so this type is only for the HTTP-bypass tasks.
type Usage struct {
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Model            string `json:"model"`
}

// salvageMarkdownFromReasoning pulls a polished markdown answer out of a
// reasoning model's `reasoning_content` when `content` came back empty.
//
// Why this exists: reasoning models (DeepSeek-R1, DeepSeek-V3 with thinking
// enabled, o1, etc.) sometimes return an empty `content` field while their
// `reasoning_content` actually carries the final answer. Three patterns
// show up in the wild:
//
//   1. Gateway serialization: the response puts the entire assistant turn
//      in `reasoning_content` and leaves `content` empty. Common with
//      self-hosted gateways that don't know how to split.
//   2. max_tokens exhausted during reasoning: the budget runs out before
//      the model can start its final-answer section in `content`, so the
//      markdown ends up appended to `reasoning_content` instead.
//   3. Model style: a few reasoning models write the answer inline with
//      the reasoning trace (often after a "Final Answer:" or similar
//      transition marker) and never emit a separate `content` field.
//
// All three leave the user staring at a 500 ("empty response") on a PDF
// upload that LITERALLY worked — the markdown is there, just in the
// wrong field. This helper rescues the markdown when the structure is
// clearly an actual document (heading + body) rather than pure scratchpad
// prose.
//
// Heuristics, in order:
//
//   - Look for explicit transition markers ("Final Answer:", "Markdown
//     Output:", etc.). Take the LAST occurrence so the model mentioning
//     the word "Answer" mid-thinking doesn't trip a false salvage.
//   - Fallback: find the LAST markdown heading at start of line. Pure
//     reasoning traces rarely contain "# Title"-style structure; the
//     model's actual answer almost always does.
//   - In both branches, validate the candidate has at least one extra
//     structural element (list, table, code fence, or another heading)
//     so we don't accidentally surface scratchpad prose as the user's
//     knowledge row.
//
// Returns "" when nothing matches; the caller then surfaces the original
// "empty response" diagnostic so the operator can decide whether to
// switch models or check the gateway config.
func salvageMarkdownFromReasoning(reasoning string) string {
	reasoning = strings.TrimSpace(reasoning)
	if reasoning == "" {
		return ""
	}
	// Explicit transition markers. We always pick the LAST occurrence in
	// the text so a model that mentions "Answer" mid-thinking doesn't
	// trip a false salvage.
	transitions := []string{
		"\n**Final Answer:**",
		"\n**Final Answer**:",
		"\nFinal Answer:",
		"\nfinal answer:",
		"\nMarkdown Output:",
		"\nMarkdown document:",
		"\nHere is the markdown:",
		"\nHere's the markdown:",
		"\nAnswer:",
	}
	bestStart := -1
	bestMarker := ""
	for _, marker := range transitions {
		if idx := strings.LastIndex(reasoning, marker); idx > bestStart {
			bestStart = idx
			bestMarker = marker
		}
	}
	if bestStart >= 0 {
		// Skip past the marker itself (e.g. "\nFinal Answer:") and any
		// leading whitespace/blank lines before the actual answer body
		// starts. The reasoning trace often has a blank line between the
		// marker and the answer for readability — that's normal.
		after := reasoning[bestStart+len(bestMarker):]
		candidate := strings.TrimLeft(after, " \t\r\n")
		candidate = strings.TrimSpace(candidate)
		if hasMarkdownStructure(candidate) {
			return candidate
		}
	}
	// Fallback: the LAST markdown heading at start of line. We try the
	// deeper levels first (`###`, `##`, `#`) so a sub-heading inside the
	// answer wins over a thinking-trace "## Step 1" marker higher up.
	for _, prefix := range []string{"\n# ", "\n## ", "\n### "} {
		if idx := strings.LastIndex(reasoning, prefix); idx >= 0 {
			candidate := strings.TrimSpace(reasoning[idx+1:])
			if hasMarkdownStructure(candidate) {
				return candidate
			}
		}
	}
	return ""
}

// hasMarkdownStructure is a cheap heuristic: returns true if s looks like
// an actual markdown document (heading + at least one other structural
// element — another heading, a list, a table, or a code fence) rather
// than a single line of prose. Reasoning traces are mostly prose, so
// this filter is what prevents us from salvaging scratchpad chatter.
func hasMarkdownStructure(s string) bool {
	if s == "" {
		return false
	}
	// Must contain at least one newline — a single line is just prose.
	if !strings.Contains(s, "\n") {
		return false
	}
	// Must contain at least one structural element beyond the leading
	// heading. Any of these count.
	structural := []string{
		"\n# ", "\n## ", "\n### ", "\n#### ",
		"\n- ", "\n* ", "\n+ ",
		"\n1. ", "\n2. ", "\n3. ",
		"\n| ",
		"\n```",
		"\n> ",
	}
	for _, marker := range structural {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// HTTP LLM channel timeouts.
//
// The channel serves two very different workloads and one cap cannot serve
// both:
//
//   - light  — title/description distillation (3072 max_tokens), commit + PR
//     title (512): short input, short output.
//   - heavy  — plan → sub-task step extraction (24000-rune input, 8192
//     max_tokens), report → knowledge, PDF → Markdown, idea → requirement,
//     mermaid diagram: minutes on a reasoning model or a self-hosted gateway.
//
// The channel originally shipped with ONE hard-coded 30s cap, tuned for the
// light workloads, and every later caller inherited it. Step extraction that
// needs 40s therefore died on EVERY attempt with
// "llm http: read response: context deadline exceeded", and the split path
// degraded to a single coarse child.
//
// Both defaults are overridable per deployment (Go duration strings):
//
//	NOVA_LLM_HTTP_TIMEOUT        — light calls  (default 30s)
//	NOVA_LLM_HTTP_HEAVY_TIMEOUT  — heavy calls  (default 5m)
const (
	httpChatTimeoutLight = 30 * time.Second
	httpChatTimeoutHeavy = 5 * time.Minute

	envHTTPTimeout      = "NOVA_LLM_HTTP_TIMEOUT"
	envHTTPHeavyTimeout = "NOVA_LLM_HTTP_HEAVY_TIMEOUT"
)

// httpChatTimeout resolves one channel timeout: env override first, then the
// package default. An unparsable or non-positive value is logged and ignored
// — a typo in the environment must never disable the timeout entirely.
func httpChatTimeout(env string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("[llm http] ignoring invalid %s=%q (want a Go duration like 90s / 5m)", env, raw)
		return def
	}
	return d
}

// Package vars (not consts) so tests can pin a timeout without sleeping.
var (
	lightChatTimeoutFn = func() time.Duration { return httpChatTimeout(envHTTPTimeout, httpChatTimeoutLight) }
	heavyChatTimeoutFn = func() time.Duration { return httpChatTimeout(envHTTPHeavyTimeout, httpChatTimeoutHeavy) }
)

// LightChatTimeout returns the resolved timeout for light-weight calls on the
// direct HTTP LLM channel (title/description distillation, commit + PR title).
func LightChatTimeout() time.Duration { return lightChatTimeoutFn() }

// HeavyChatTimeout returns the resolved timeout for heavy calls on the direct
// HTTP LLM channel (plan → sub-task step extraction, report → knowledge,
// PDF → Markdown, idea → requirement, mermaid diagram).
//
// It is also the budgeting unit for the split path's retry loop, so raising
// the per-call timeout cannot multiply the worst-case wait.
func HeavyChatTimeout() time.Duration { return heavyChatTimeoutFn() }

// chatCompletion sends a single-turn chat completion to an OpenAI-compatible
// /chat/completions endpoint and returns the assistant's raw text. Shared by
// the lightweight LLM tasks that bypass the claude CLI for speed — none need
// tool use. baseURL is the API root (with or without trailing slash, with or
// without /v1); the path /chat/completions is appended. maxTokens caps the
// response; the caller picks a size appropriate to the task.
//
// timeout bounds the whole request (connect → body read). The caller passes
// it explicitly because the channel serves two very different workloads: a
// title distillation wants to fail fast, while a plan → sub-task extraction
// ships 24k runes and asks for 8192 tokens and needs minutes. Passing the
// wrong tier makes the call either stall the fallback path or die
// deterministically mid-body ("context deadline exceeded"). A non-positive
// timeout falls back to LightChatTimeout() so a forgotten argument can never
// disable the bound entirely.
func chatCompletion(baseURL, apiKey, model, systemPrompt, userContent string, maxTokens int, timeout time.Duration) (string, *Usage, error) {
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"

	body := chatRequest{
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature: 0,
		MaxTokens:   maxTokens,
		Stream:      false,
	}
	if model != "" {
		body.Model = model
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", nil, fmt.Errorf("llm http: marshal request: %w", err)
	}

	if timeout <= 0 {
		timeout = LightChatTimeout()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", nil, fmt.Errorf("llm http: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("llm http: request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap, defensive
	if err != nil {
		return "", nil, fmt.Errorf("llm http: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Truncate body to keep the error readable; it should never contain the
		// api key (the key is sent in the Authorization header, not the body).
		snippet := string(raw)
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return "", nil, fmt.Errorf("llm http: status %d: %s", resp.StatusCode, strings.TrimSpace(snippet))
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", nil, fmt.Errorf("llm http: decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		// No choices at all — most often a model that doesn't support
		// /chat/completions (e.g. embeddings-only) or a gateway bug. Log the
		// raw body (truncated) so the operator can tell which.
		snippet := string(raw)
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		log.Printf("[llm http] /chat/completions returned no choices: model=%q body=%s", model, snippet)
		return "", nil, fmt.Errorf("llm http: empty response (no choices, body=%s)", strings.TrimSpace(snippet))
	}
	content := cr.Choices[0].Message.Content
	reasoning := cr.Choices[0].Message.ReasoningContent
	if content == "" {
		// The LLM emitted an empty final answer. Three known causes, handled
		// in order of how actionable they are:
		//
		//   1. Reasoning models (DeepSeek-R1 / V3, o1, etc.) that finished
		//      their chain-of-thought AND emitted the final answer — but the
		//      gateway put the answer into `reasoning_content` instead of
		//      `content` (or max_tokens exhausted during reasoning so the
		//      `content` buffer is empty while `reasoning_content` carries
		//      the markdown). Try to salvage the polished markdown so the
		//      upload doesn't hard-fail.
		//   2. Reasoning model that thought but never produced a final
		//      answer — `reasoning_content` is pure scratchpad prose with
		//      no markdown structure. Salvage returns empty; we surface
		//      the diagnostic so the operator can switch to a non-reasoning
		//      model.
		//   3. Safety refusal / output filter that swallowed the response.
		//      Same diagnostic: log the raw body so the operator can spot
		//      refusal markers.
		if salvaged := salvageMarkdownFromReasoning(reasoning); salvaged != "" {
			log.Printf("[llm http] /chat/completions returned empty content but reasoning_content "+
				"had a markdown answer — salvaging: model=%q reasoning_len=%d salvaged_len=%d",
				model, len(reasoning), len(salvaged))
			content = salvaged
		} else {
			snippet := string(raw)
			if len(snippet) > 256 {
				snippet = snippet[:256]
			}
			log.Printf("[llm http] /chat/completions returned empty content: model=%q reasoning_len=%d body=%s",
				model, len(reasoning), snippet)
			return "", nil, fmt.Errorf("llm http: empty response (model returned empty content; "+
				"if using a reasoning model like DeepSeek-R1, switch to a non-reasoning model — "+
				"raw body=%s)", strings.TrimSpace(snippet))
		}
	}
	var usage *Usage
	if cr.Usage != nil {
		usage = &Usage{
			PromptTokens:     cr.Usage.PromptTokens,
			CompletionTokens: cr.Usage.CompletionTokens,
			Model:            model,
		}
	}
	return content, usage, nil
}

// formatAndTitleViaHTTP reorganizes the user's raw requirement content into
// structured Markdown AND distills a concise title in a single LLM round via
// an OpenAI-compatible /chat/completions endpoint (e.g. DeepSeek). Bypasses
// the claude CLI — neither task needs tool use. Merging the two halves into
// one request keeps title and body consistent and transmits the content once.
// The kind argument selects a kind-shaped system prompt so an Issue
// description becomes a bug-report scaffold, an Idea becomes a lightweight
// exploratory note, and a Requirement keeps the legacy four-section layout.
// Empty kind falls back to the legacy prompt. On failure the caller falls back
// (raw content for Markdown, first line for title) so requirement creation
// never fails just because this is unavailable.
func formatAndTitleViaHTTP(baseURL, apiKey, model, content, kind string) (markdown, title string, usage *Usage, err error) {
	var systemPrompt string
	switch kind {
	case "issue":
		systemPrompt = formatAndTitleIssueSystemPrompt
	case "idea":
		systemPrompt = formatAndTitleIdeaSystemPrompt
	default:
		systemPrompt = formatAndTitleSystemPrompt
	}
	out, usage, err := chatCompletion(baseURL, apiKey, model, systemPrompt, content, 3072, LightChatTimeout())
	if err != nil {
		return "", "", nil, err
	}
	var res formatAndTitleResult
	if jerr := json.Unmarshal([]byte(stripJSONFences(out)), &res); jerr != nil {
		return "", "", usage, fmt.Errorf("llm http: decode format+title json: %w", jerr)
	}
	// Strip any surrounding quotes / whitespace the model may add despite
	// instructions (same post-processing as the legacy title path).
	res.Title = strings.Trim(res.Title, "\"'` \n\r\t")
	if strings.TrimSpace(res.Markdown) == "" {
		return "", "", usage, fmt.Errorf("llm http: empty markdown")
	}
	if res.Title == "" {
		return "", "", usage, fmt.Errorf("llm http: empty title")
	}
	return res.Markdown, res.Title, usage, nil
}
