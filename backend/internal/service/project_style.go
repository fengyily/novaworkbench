package service

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/novaworkbench/backend/internal/model"
)

// DetectCommitLanguage 扫描项目最近 200 条 commit message，统计中文 commit
// 占比，返回推断的语言与置信度。中日韩字符通过 unicode.Han 类判断（等价于
// 正则表达式 \p{Han}）。
//
// 返回约定：
//   - 非 git 仓库 / 无 commit / git 不可用 → ("", 0, nil)
//   - 至少 1 条 commit → lang 为 "zh" 或 "en"，confidence 为对应比例 [0,1]
//   - 子命令执行失败（非 git 仓库）→ ("", 0, nil)，不向上抛错
//
// git 命令设有 5s 超时；输入输出通过 bytes.Buffer 捕获，避免 spawn 子 shell。
// 函数无全局状态，可独立测试。
func DetectCommitLanguage(projectDir string) (string, float64, error) {
	if _, err := exec.LookPath("git"); err != nil {
		// git 不在 PATH 中：无法推断，按"无 commit"语义返回空，不视为错误
		return "", 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", projectDir, "log", "--pretty=format:%s", "-n", "200")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// 非 git 目录、空仓库、命令异常等都视为无 commit
		return "", 0, nil
	}
	raw := strings.TrimSpace(stdout.String())
	if raw == "" {
		return "", 0, nil
	}
	lines := strings.Split(raw, "\n")
	zh, en := 0, 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if containsHan(line) {
			zh++
		} else {
			en++
		}
	}
	total := zh + en
	if total == 0 {
		return "", 0, nil
	}
	if zh >= en {
		return "zh", float64(zh) / float64(total), nil
	}
	return "en", float64(en) / float64(total), nil
}

// containsHan 报告 s 中是否包含任何中日韩字符（unicode.Han 类）。
func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// CommitStyleRules 是「预设 + 自定义文本 + 语言」一次性渲染的结果。
// 项目概览的「提交风格」卡片回答的是两个正交的问题：用什么语言写
// （commit_lang_override → StyleHint 的语言那一行）、写成什么形状
// （本结构的三个字段）。渲染在这里收口，所有注入点只消费产物、不再各自
// 拼字符串，于是「预览 = 实际注入」有单一来源。
//
// 三个字段为空串的语义统一是「不注入这一段」。未配置的项目
// （commit_style_preset == ""）拿到的是全空结构，注入后的 prompt 与
// 本功能上线前逐字相同 —— 这条零回归保证由 TestBuildCommitStyleRules_*
// 的空预设用例钉住。
type CommitStyleRules struct {
	// Commit 是 commit message 规范，注入 prompt.GitProjectContext.StyleHint。
	Commit string
	// PRTitle 是 PR 标题规范，替换 PRLangRules 模板里写死的「不要前缀」那行。
	PRTitle string
	// PRBody 是 PR 正文规范，作为独立小节追加在 PRLangRules 的禁止项之后。
	PRBody string
	// Example 只给前端预览用（"fix(model): ..."），不注入任何 prompt ——
	// 展示一行例子比让模型读一整段规则更能帮用户确认「是不是我要的」。
	Example string
	// Conventional 标记本结构来自 conventional 预设（而不是 custom 的
	// 自由文本）。它存在的理由是 NewPRShellTemplates 需要判断「规范是否
	// 强制 type 前缀」，而这个信息没法从 Commit/PRTitle 两段文本里可靠地
	// 反推 —— 用户完全可能自己写一段要求 `feat:` 前缀的 custom 文本，
	// 那是另一回事，不能和内置预设混为一谈。
	Conventional bool
}

// SingleShotRule renders the convention block for the single-shot
// llm.GenerateCommitArtifacts channel, which produces commit_message AND
// pr_title in one call and has no separate PR section to hang them off —
// so the PR title rule has to ride along inside the same block. Empty when
// the project has no preset (the gateway substitutes its own fallback).
//
// PRBody is deliberately excluded: that channel emits a title + a subject,
// never a body, so shipping body rules would be noise in the prompt.
func (r CommitStyleRules) SingleShotRule() string {
	parts := make([]string, 0, 2)
	if c := strings.TrimSpace(r.Commit); c != "" {
		parts = append(parts, c)
	}
	if t := strings.TrimSpace(r.PRTitle); t != "" {
		parts = append(parts, "- PR 标题："+t)
	}
	return strings.Join(parts, "\n")
}

// customStyleSupplementHeading 是用户在补充规范文本框里写的内容被包进
// prompt 时的标题。固定措辞，方便用户在生成的 commit 里反查这段规则
// 是项目配置来的而不是模型自作主张。
const customStyleSupplementHeading = "## 项目补充规范"

// BuildCommitStyleRules 把项目上的 preset + custom + 语言渲染成注入用文本。
//
//	preset == "" / none → 全空结构（不注入任何规范，等价于本功能上线前）
//	preset == conventional → 按 lang 本地化的 Conventional Commits 三段
//	preset == custom → 用户的自定义文本原样成为规范
//
// custom 文本非空时会被**追加**在预设三段之后 —— 用户选 conventional 又
// 想补一条自己的规则，是常见组合；反过来（选 custom 又想保留预设）没有
// 意义，所以 custom 预设直接用原文，不拼预设。
//
// 空 custom + preset == custom 返回全空结构：service.SetCommitStyle 会在
// 落库前把这种组合拦成 400，这里再兜一层，是为了让 GET 预览端点（纯函数、
// 不碰 DB）对任意入参都返回可渲染的结果而不是半截文本。
func BuildCommitStyleRules(preset, custom, lang string) CommitStyleRules {
	preset = strings.TrimSpace(preset)
	custom = strings.TrimSpace(custom)
	// 长度上限与 service.SetCommitStyle 的校验同源（model.CommitStyleCustomMaxLen）。
	// 这里再截一次是给预览端点用的：它不落库，所以不能假设入参已校验过，
	// 而超长文本会把整个 prompt 撑爆。
	if runes := []rune(custom); len(runes) > model.CommitStyleCustomMaxLen {
		custom = string(runes[:model.CommitStyleCustomMaxLen])
	}

	switch preset {
	case "", model.CommitStylePresetNone:
		return CommitStyleRules{}

	case model.CommitStylePresetConventional:
		r := conventionalCommitStyleRules(lang)
		if custom != "" {
			r.Commit += "\n" + customStyleSupplement(custom)
		}
		return r

	case model.CommitStylePresetCustom:
		if custom == "" {
			return CommitStyleRules{}
		}
		// 用户原文只在 prompt 里出现**一次**：PR 侧不复述它，而是让
		// PRTitle 指一句方向、把全文放进 PRBody。这样
		// "PR 标题遵循项目规范" + 紧跟着的规范全文既自洽（不依赖
		// StyleHint 段是否同时出现，generatePRSummary 就只带 PR 侧），
		// 又不会把同一段 4000 字的文本在一条 prompt 里塞两遍。
		return CommitStyleRules{
			Commit:  customStyleSupplement(custom),
			PRTitle: "遵循下方「项目提交规范」中的 PR 标题要求",
			PRBody:  customStyleSupplement(custom),
		}

	default:
		// 未知 preset（老库里被手改的行 / 未来下线某个预设）：按未配置
		// 处理，比注入一段未知来源的文本安全。
		return CommitStyleRules{}
	}
}

// customStyleSupplement 包装用户补充规范，让它在 prompt 里成为可辨识的一节。
func customStyleSupplement(custom string) string {
	return customStyleSupplementHeading + "\n" + custom
}

// conventionalCommitStyleRules 渲染 Conventional Commits 预设的三段规范。
// type 清单来自 model.CommitStyleTypes（后端校验与前端说明共用同一份），
// 示例按 lang 切换 —— 示例本身就是「形状 + 语言」的一次演示，比抽象描述
// 更能让模型一次做对。
func conventionalCommitStyleRules(lang string) CommitStyleRules {
	types := strings.Join(model.CommitStyleTypes, " / ")
	// 长度提示：conventional 的 subject 上限是 72 字符，但这个数字是
	// 规范的一部分，必须和 commit_prompt.go 里的字面要求一致，否则模型
	// 会拿到两个互相打架的上限。
	if ResolveCommitLang(lang, "") == "en" {
		return CommitStyleRules{
			Commit: "## 项目提交规范（Conventional Commits）\n" +
				"- Format: `type(scope): subject`. type must be one of: " + types + ".\n" +
				"- scope is optional; use it only when the change clearly belongs to one area (e.g. `model`, `api`, `ui`).\n" +
				"- subject: imperative mood, lowercase, no trailing period, 72 characters max.\n" +
				"- One commit = one logical change; don't mix unrelated edits under one type.\n" +
				"- Example: `fix(model): guard against empty input`",
			PRTitle: "use the same `type(scope): subject` format as commit messages",
			PRBody: "## Project PR body rules (Conventional Commits)\n" +
				"- Markdown. Make the summary, the concrete changes, and the verification steps easy to locate — omit a part entirely when you have nothing for it, never leave an empty heading.\n" +
				"- When you can confidently identify an issue being closed, end the body with `Closes #123`.\n" +
				"- Do not restate the commit subject verbatim in the body.",
			Example:      "fix(model): guard against empty input",
			Conventional: true,
		}
	}
	return CommitStyleRules{
		Commit: "## 项目提交规范（Conventional Commits）\n" +
			"- 格式：`type(scope): subject`，type 取自：" + types + "。\n" +
			"- scope 可选，仅当改动明确属于某一模块时使用（如 `model`、`api`、`ui`）。\n" +
			"- subject：祈使句、小写开头、不带句号，不超过 72 个可见字符。\n" +
			"- 一次提交只做一件事，不要把无关改动混在同一个 type 下。\n" +
			"- 示例：`fix(model): 修复用户输入为空时的崩溃`",
		PRTitle: "与 commit 保持同一格式 `type(scope): subject`",
		PRBody: "## 项目 PR 正文规范（Conventional Commits）\n" +
			"- 使用 Markdown，让概述、具体改动、验证方式易于定位；某部分没有内容就整段省略，不要留空标题。\n" +
			"- 能明确识别到要关闭的 Issue 时，正文末尾附 `Closes #123`。\n" +
			"- 不要把 commit subject 原样复制到正文里。",
		Example:      "fix(model): 修复用户输入为空时的崩溃",
		Conventional: true,
	}
}

// StyleHint 根据 DB 读取的 lang 与 UI 上的 override 返回展示文本。
// override 优先；lang 为空时返回空字符串。返回结果形如：
//
//	"📝 项目提交风格：中文 — 强制要求：commit 信息、PR 标题、PR 正文必须全部使用中文"
//	"📝 项目提交风格：English — 强制要求：commit messages, PR title, and PR body MUST all be in English"
//	"📝 项目提交风格：中英混用 — commit 信息、PR 标题、PR 正文允许中英文混用，与项目历史风格保持一致"
//
// style 是 BuildCommitStyleRules 的产物（语言之外的「格式规范」维度）。
// 非空时把 commit 规范段追加在语言那一行之后 —— 语言回答"用什么语言写"，
// 规范回答"写成什么形状"（如 Conventional Commits 的 `type(scope): subject`）。
// 两者拼在同一条 hint 里，是因为 prompt 包刻意不依赖 service：调用方把整段
// 渲染好传进来，prompt.GitProjectContext 只做拼接。
//
// 文案改成硬性指令（含"必须全部使用..."），用于在 prompt 末尾对 PRLangRules
// 进行二次强化。语言与 style 都为空时返回空字符串，调用方应自行决定是否渲染。
func StyleHint(lang, override string, style CommitStyleRules) string {
	var hint string
	switch ResolveCommitLang(lang, override) {
	case "zh":
		hint = "📝 项目提交风格：中文 — 强制要求：commit 信息、PR 标题、PR 正文必须全部使用中文"
	case "en":
		hint = "📝 项目提交风格：English — 强制要求：commit messages, PR title, and PR body MUST all be in English"
	case "mixed":
		hint = "📝 项目提交风格：中英混用 — commit 信息、PR 标题、PR 正文允许中英文混用，与项目历史风格保持一致"
	default:
		hint = ""
	}
	if s := strings.TrimSpace(style.Commit); s != "" {
		if hint == "" {
			// 语言未确定但规范已配置：仍然要把规范送到 prompt，否则选了
			// conventional 的项目在 commit_lang 检测失败时会静默丢掉规范。
			hint = "📝 项目提交风格：\n" + s
		} else {
			hint += "\n" + s
		}
	}
	return hint
}

// PRLangRules 返回"PR 摘要语言规则"片段，注入到 buildPushSubTaskPrompt 与
// generatePRSummary 的 user prompt 中。该片段强于 pr_author 角色 system
// prompt 中的硬编码指令；空值返回默认中文规则块，与未检测到语言时的现有
// 行为一致（零回归）。
//
// 模板策略：自由 section。用户列出的 5 个 section 只是建议，LLM 按实际
// 增删；只有有真实内容时才写对应 section。
//
// 显式禁止项（PR title + body 都不可含）：
//   - AI 署名 trailer：🤖 Generated with Claude Code、Co-Authored-By: Claude <...>、
//     Co-authored-by: ... 等
//   - auto-push 描述：Auto-created by NovaWorkbench auto-push、由 NovaWorkbench
//     auto-push 自动创建 等
//   - 占位文本：无关联 / N/A / 无 —— 完全空白的 section 整段省略
//
// 三种返回值：en / mixed / "" (zh) —— 与 ResolveCommitLang 对齐。
//
// style 是 BuildCommitStyleRules 的产物，负责「PR 标题 / 正文规范」这一维。
// 它**替换**掉模板里原本写死的「不要 Conventional Commits 前缀」那一行 ——
// 旧写法是一条无条件否定式硬规则，与 conventional 预设直接冲突（同时给
// 模型「要加前缀」和「不要加前缀」两条指令，行为不可预测）。现在这一行的
// 内容由 style.PRTitle 决定：未配置 → 保留原否定式措辞（逐字不变，零回归）；
// 配了 conventional → 换成正向的 `type(scope): subject` 格式要求。
func PRLangRules(lang string, style CommitStyleRules) string {
	// 标题格式一行：未配置规范时沿用历史措辞（各语言各自的否定式表达）。
	// 这些字符串是"零回归基线"，改动它们会让所有未配置项目的 prompt 变形。
	zhNoPrefix := "- PR 标题使用中文，一句话概括本次改动（不超过 40 字，不要以 `feat:` 等前缀开头）。"
	mixedNoPrefix := "- PR 标题：可用中文或英文，与项目既有提交历史风格保持一致（不超过 40 字，不要 Conventional Commits 前缀）。"
	enNoPrefix := "- PR title: one sentence in English, no more than 80 characters, no Conventional Commits prefix (`feat:`, `fix:`, etc.)."

	zhTitle, mixedTitle, enTitle := zhNoPrefix, mixedNoPrefix, enNoPrefix
	if t := strings.TrimSpace(style.PRTitle); t != "" {
		// 规范已配置 → 用正向格式要求替换掉否定式措辞。
		zhTitle = "- PR 标题：" + t
		mixedTitle = "- PR 标题：" + t
		enTitle = "- PR title: " + t
	}

	switch ResolveCommitLang(lang, "") {
	case "en":
		out := "## PR Summary Template (overrides pr_author role default)\n" +
			enTitle + "\n" +
			"- PR body: Markdown in English. Use your judgment to include or omit sections based on what's actually relevant. The user listed 5 reference sections below; treat them as suggestions, not requirements — only include a section when you have real content for it:\n" +
			"  - `## What changed`: concrete description of code changes (files, behaviors, why). Include when there are code changes to describe.\n" +
			"  - `## Related issue`: ONLY when you can confidently identify a closing issue, format as `Closes #123`. Omit entirely if there is no closing issue.\n" +
			"  - `## How to verify`: test steps / commands the reviewer can run. Omit if no test/verification is applicable.\n" +
			"  - `## Checklist`: any test coverage / manual verification you performed. Omit if you did none.\n" +
			"  - `## Notes`: free-form reviewer notes. When you include this section, end it with these two fixed items (do not change wording):\n" +
			"    - [ ] Self-tested\n" +
			"    - [ ] No debug code\n" +
			"- Strictly FORBIDDEN in PR title or body:\n" +
			"  - AI attribution trailers: 🤖 Generated with Claude Code, Co-Authored-By: Claude <...>, Co-authored-by: ..., etc.\n" +
			"  - Auto-push notices: \"Auto-created by NovaWorkbench auto-push\", \"由 NovaWorkbench auto-push 自动创建\", etc.\n" +
			"  - Placeholder text for missing data: 无关联 / N/A / 无 — omit the section entirely instead.\n"
		// PR 正文规范：conventional / custom 预设追加一段，保留上面「自由
		// section」基线不变 —— 用户选了预设也不该丢掉兜底小节建议。
		if b := strings.TrimSpace(style.PRBody); b != "" {
			out += "\n" + b + "\n"
		}
		return out + "- Keep it concise.\n"
	case "mixed":
		out := "## PR 摘要模板（强制，覆盖 pr_author 角色默认中文规则）\n" +
			mixedTitle + "\n" +
			"- PR 正文：Markdown，中英混用。section 按实际判断增删 —— 用户列出的 5 个参考 section 只是建议，不是强制；只有当有真实内容时才写：\n" +
			"  - `## 改了什么`：具体说明代码改动（文件、行为、原因）。\n" +
			"  - `## 关联 Issue`：仅当能明确识别到要关闭的 issue 时输出，格式为 `Closes #123`，否则整个 section 省略。\n" +
			"  - `## 怎么验证`：审阅者可执行的测试步骤 / 命令。\n" +
			"  - `## 检查清单`：你实际做的测试 / 验证。\n" +
			"  - `## 备注`：自由文本 reviewer 备注。如包含此 section，末尾固定附以下两个检查项（不要修改措辞）：\n" +
			"    - [ ] 自测通过\n" +
			"    - [ ] 无调试代码\n" +
			"- 严格禁止出现在 PR 标题或正文：\n" +
			"  - AI 署名 trailer：🤖 Generated with Claude Code、Co-Authored-By: Claude <...>、Co-authored-by: ... 等\n" +
			"  - auto-push 描述：「由 NovaWorkbench auto-push 自动创建」「Auto-created by NovaWorkbench auto-push」等\n" +
			"  - 占位文本：无关联 / N/A / 无 —— 完全空白的 section 整个省略\n"
		if b := strings.TrimSpace(style.PRBody); b != "" {
			out += "\n" + b + "\n"
		}
		return out + "- 简洁有重点。\n"
	default: // "" 或 "zh"：默认中文
		out := "## PR 摘要要求\n" +
			zhTitle + "\n" +
			"- PR 正文使用 Markdown。section 按实际判断增删 —— 用户列出的 5 个参考 section 只是建议，不是强制；只有当有真实内容时才写：\n" +
			"  - `## 改了什么`：具体说明代码改动（文件、行为、原因）。\n" +
			"  - `## 关联 Issue`：仅当能明确识别到要关闭的 issue 时输出，格式为 `Closes #123`，否则整个 section 省略。\n" +
			"  - `## 怎么验证`：审阅者可执行的测试步骤 / 命令。\n" +
			"  - `## 检查清单`：你实际做的测试 / 验证。\n" +
			"  - `## 备注`：自由文本 reviewer 备注。如包含此 section，末尾固定附以下两个检查项（不要修改措辞）：\n" +
			"    - [ ] 自测通过\n" +
			"    - [ ] 无调试代码\n" +
			"- 严格禁止出现在 PR 标题或正文：\n" +
			"  - AI 署名 trailer：🤖 Generated with Claude Code、Co-Authored-By: Claude <...>、Co-authored-by: ... 等\n" +
			"  - auto-push 描述：「由 NovaWorkbench auto-push 自动创建」「Auto-created by NovaWorkbench auto-push」等\n" +
			"  - 占位文本：无关联 / N/A / 无 —— 完全空白的 section 整个省略\n"
		if b := strings.TrimSpace(style.PRBody); b != "" {
			out += "\n" + b + "\n"
		}
		return out + "- 简洁有重点。\n"
	}
}

// DetectTextLanguage 检测单个字符串的主导语言。
//
//   - CJK 字符数 > ASCII 字母数 * 1.5 → "zh"
//   - ASCII 字母数 > CJK 字符数 * 1.5 → "en"
//   - 比例相近 → "mixed"
//   - 空白 / 无 CJK 也无字母 → ""
//
// 与 DetectCommitLanguage（多行 commit 扫描）语义对齐，但接受单个字符串，
// 用于判定单条标题 / PR 文案的语言。
func DetectTextLanguage(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var han, asciiAlpha int
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			han++
		} else if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			asciiAlpha++
		}
	}
	total := han + asciiAlpha
	if total == 0 {
		return ""
	}
	if float64(han) > float64(asciiAlpha)*1.5 {
		return "zh"
	}
	if float64(asciiAlpha) > float64(han)*1.5 {
		return "en"
	}
	return "mixed"
}

// ShouldRegenerateForLang 判定 commitLang 与 title 标题是否语言冲突、
// 是否需要走 LLM 重新生成文案。
//
// 规则：
//   - commitLang 与 DetectTextLanguage(title) 都非空 / 非 mixed 且互不等 → true
//     （例如 "en" 项目 + 中文标题；"zh" 项目 + 英文标题）
//   - commitLang == "mixed" → false（项目本就允许中英混用）
//   - 其它（commitLang 空 / 标题为空 / mixed）→ false
func ShouldRegenerateForLang(commitLang, title string) bool {
	if commitLang == "" || commitLang == "mixed" {
		return false
	}
	titleLang := DetectTextLanguage(title)
	if titleLang == "" || titleLang == "mixed" {
		return false
	}
	return commitLang != titleLang
}

// PRShellTemplates 集中提供「本地 shell 自动推送」路径所需的本地化模板
// 字符串。这些是 git/gh 用的直接文案（不是给 LLM 看的指令），所以不能
// 复用 PRLangRules / StyleHint。
//
// shell 路径是确定性的（不调 LLM），只能填 5-section 模板的 2 个子集：
//   - ## 改了什么 / ## What changed → PRBodyWhatChangedHeading（commits + diff stat）
//   - ## 备注 / ## Notes            → PRBodyNotesHeading + PRBodyNotesChecklist
//
// 其余 3 个 section（关联 Issue / 怎么验证 / 检查清单）shell 路径无 issue
// 上下文、无验证步骤、无 checklist 内容，整段省略。这是数据驱动的"自由
// section"——shell 路径只输出它有数据的 section。完全空白的 section
// 整段省略，PR body 不含 AI 署名 / auto-push 描述。
//
//   - "en"    → 英文 commit fallback + 英文 PR 标题回退 + 英文 PR 正文。
//   - "" / "zh" / "mixed" → 默认中文。
type PRShellTemplates struct {
	CommitMessageDefaultFmt  string // e.g. "NovaWorkbench auto-push: update %s"
	PRTitleFallback          string // e.g. "NovaWorkbench auto-push: %s"
	PRBodyWhatChangedHeading string // e.g. "## What changed" / "## 改了什么"
	PRBodyNotesHeading       string // e.g. "## Notes" / "## 备注"
	PRBodyNotesChecklist     string // 多行字符串：en "- [ ] Self-tested\n- [ ] No debug code" / zh "- [ ] 自测通过\n- [ ] 无调试代码"
}

// NewPRShellTemplates 根据 lang 返回本地化模板。分支解析走 ResolveCommitLang：
//
//	"en" → 英文模板
//	"" / "zh" / "mixed" → 中文模板
//
// style 来自 BuildCommitStyleRules。选了 conventional 预设时，
// CommitMessageDefaultFmt 改成 `chore: %s` —— 这条 shell 路径是**无 LLM 的
// 确定性 fallback**，从分支名推不出这次改动到底是 feat 还是 fix，所以只用
// `chore` 这个语义上恒成立的 type（"维护性改动"兜底），而不是编一个看着
// 具体实则瞎猜的 type。
//
// PRTitleFallback 则**保持不加前缀**：PR 标题的 type 只能来自 LLM 语义判断
// （GenerateCommitArtifacts 的 pr_title），fallback 是 LLM 没给出标题时的
// 最后一道兜底，此时给标题加前缀等于凭空断言一次改动性质。该路径只在
// pr_title 为空时才触发，属低频。
//
// 函数名采用 `New<Struct>` 构造函数惯例，因为类型 PRShellTemplates 与
// 构造函数同名会与 Go 同包命名空间冲突。
func NewPRShellTemplates(lang string, style CommitStyleRules) PRShellTemplates {
	var tmpl PRShellTemplates
	switch ResolveCommitLang(lang, "") {
	case "en":
		tmpl = PRShellTemplates{
			CommitMessageDefaultFmt:  "NovaWorkbench auto-push: update %s",
			PRTitleFallback:          "NovaWorkbench auto-push: %s",
			PRBodyWhatChangedHeading: "## What changed",
			PRBodyNotesHeading:       "## Notes",
			PRBodyNotesChecklist:     "- [ ] Self-tested\n- [ ] No debug code",
		}
	default: // "" / "zh" / "mixed" — 中文默认
		tmpl = PRShellTemplates{
			CommitMessageDefaultFmt:  "NovaWorkbench auto-push 更新 %s",
			PRTitleFallback:          "NovaWorkbench auto-push: %s",
			PRBodyWhatChangedHeading: "## 改了什么",
			PRBodyNotesHeading:       "## 备注",
			PRBodyNotesChecklist:     "- [ ] 自测通过\n- [ ] 无调试代码",
		}
	}
	if style.Conventional {
		tmpl.CommitMessageDefaultFmt = "chore: %s"
	}
	return tmpl
}

// ResolveCommitLang 决策项目提交风格的最终值。优先级：
//  1. 用户覆盖值（override 非空）
//  2. 自动检测值（stored）
//  3. 两者皆空时返回空字符串（调用方按"未确定风格"处理）
func ResolveCommitLang(stored, override string) string {
	if override != "" {
		return override
	}
	return stored
}
