package llm

// commitMessageSystemPrompt + commitMessageUserTemplate drive the
// "轻量级 LLM 生成 commit 信息" path used by
// handler.mergeGenerate.generateCommitMessage (and the legacy
// shell + LLM push sub-task). They run over the OpenAI-compatible
// HTTP channel via chatCompletion rather than spawning the claude
// CLI — the call needs no tool use and is on the critical path
// between "dev complete" and "git push", so a CLI spawn would cost
// seconds + a full session.
//
// The prompt is intentionally narrower than the wizard pipeline's
// commit-message prompt (see prompt.GitCommitConvention, which is
// appended to the sub-agent's prompt): this is a SINGLE-SHOT
// generation, not a multi-step developer agent. The output shape
// ({"commit_message": "..."}) keeps the JSON envelope simple so the
// decoder doesn't have to coerce fences the way the higher-level
// extractors do.

// commitMessageSystemPrompt instructs the model on *what kind of
// text* to produce. The "不添加任何 AI 署名" + "不臆造改动内容"
// rules are load-bearing — they're the last line of defence against
// AI attribution trailers leaking into the commit log and against
// hallucinated diffs being attributed to the wrong change. The
// language rule uses {{LANG}} as a token so the per-request user
// template can swap it without us hand-editing the system prompt
// per call.
const commitMessageSystemPrompt = `你是一名 git commit 信息生成助手。请基于下方给出的需求标题、描述与改动 diff，生成一条简洁、规范的 commit 信息（仅首行 — subject line，不要 body）。

{{LANG_RULE}}

输出格式严格为 JSON 对象：{"commit_message":"..."}，不要用 markdown 代码围栏包裹整体输出，不要任何前后缀解释。
commit_message 字面要求：
- 一行文本，不带 \n、不以 \n 结尾
- 不超过 72 个可见字符（中文按 1 字符计），尽量 < 50 字符
- 使用动词开头（添加 / 修复 / 调整 / 重构 / 优化 / 实现 / feat / fix / refactor / chore 等），避免无意义的统称（"update code" / "some changes"）
- 不要加句号、引号、emoji、破折号
- 描述本 diff 实际做了什么，不要泛泛而谈、不要臆造未出现的功能
- 严禁包含任何 AI 署名 / 协作者尾注：禁止出现 "🤖 Generated with Claude Code"、"Co-Authored-By: ..."、"Co-authored-by: ..." 等任何形式的 trailer
- 严禁在命令里写 token / 密码 / 用户名 — 凭据已由系统配置
仅输出该 JSON 对象，不要任何前后缀说明。`

// commitMessageLangRule is the {{LANG_RULE}} body that gets swapped in
// per request. mode == "zh" → 中文 commit；mode == "en" / "" / unknown
// → English commit. Anything else (mixed) is treated as English default
// to preserve parity with the legacy shell PR templates.
const commitMessageLangRuleZh = "目标语言：中文。commit_message 必须使用中文短语。"
const commitMessageLangRuleEn = "目标语言：English. The commit_message MUST be an English subject line."

// commitMessageUserTemplate renders the request body. {{TITLE}} /
// {{DESCRIPTION}} / {{DIFF}} are filled per call by
// GenerateCommitArtifacts / GenerateCommitMessage. {{DIFF}} is capped
// per caller (12,000 runes) before substitution so a huge diff can't
// push the prompt past the LLM's context window — the cap mirrors
// the gateway's Extract* extractors so call paths share a single
// budget policy.
const commitMessageUserTemplate = `## 需求标题
{{TITLE}}

## 需求描述
{{DESCRIPTION}}

## 改动 diff（已截断，必要时只看首尾）
` + "```" + `
{{DIFF}}
` + "```" + `

请基于上述信息生成 commit_message。`
