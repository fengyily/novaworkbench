package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
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

// chatCompletion sends a single-turn chat completion to an OpenAI-compatible
// /chat/completions endpoint and returns the assistant's raw text. Shared by
// the lightweight LLM tasks that bypass the claude CLI for speed — none need
// tool use. baseURL is the API root (with or without trailing slash, with or
// without /v1); the path /chat/completions is appended. maxTokens caps the
// response; the caller picks a size appropriate to the task. A 30s timeout
// keeps the request fail-fast so the caller's fallback path is not stalled.
func chatCompletion(baseURL, apiKey, model, systemPrompt, userContent string, maxTokens int) (string, *Usage, error) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	if len(cr.Choices) == 0 || cr.Choices[0].Message.Content == "" {
		return "", nil, fmt.Errorf("llm http: empty response")
	}
	var usage *Usage
	if cr.Usage != nil {
		usage = &Usage{
			PromptTokens:     cr.Usage.PromptTokens,
			CompletionTokens: cr.Usage.CompletionTokens,
			Model:            model,
		}
	}
	return cr.Choices[0].Message.Content, usage, nil
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
	out, usage, err := chatCompletion(baseURL, apiKey, model, systemPrompt, content, 3072)
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
