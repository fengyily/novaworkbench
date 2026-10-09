package service

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
	"unicode"
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

// StyleHint 根据 DB 读取的 lang 与 UI 上的 override 返回展示文本。
// override 优先；lang 为空时返回空字符串。返回结果形如：
//
//	"📝 项目提交风格：中文 — 强制要求：commit 信息、PR 标题、PR 正文必须全部使用中文"
//	"📝 项目提交风格：English — 强制要求：commit messages, PR title, and PR body MUST all be in English"
//	"📝 项目提交风格：中英混用 — commit 信息、PR 标题、PR 正文允许中英文混用，与项目历史风格保持一致"
//
// 文案改成硬性指令（含"必须全部使用..."），用于在 prompt 末尾对 PRLangRules
// 进行二次强化。两者皆空时返回空字符串，调用方应自行决定是否渲染。
func StyleHint(lang, override string) string {
	switch ResolveCommitLang(lang, override) {
	case "zh":
		return "📝 项目提交风格：中文 — 强制要求：commit 信息、PR 标题、PR 正文必须全部使用中文"
	case "en":
		return "📝 项目提交风格：English — 强制要求：commit messages, PR title, and PR body MUST all be in English"
	case "mixed":
		return "📝 项目提交风格：中英混用 — commit 信息、PR 标题、PR 正文允许中英文混用，与项目历史风格保持一致"
	default:
		return ""
	}
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
func PRLangRules(lang string) string {
	switch ResolveCommitLang(lang, "") {
	case "en":
		return "## PR Summary Template (overrides pr_author role default)\n" +
			"- PR title: one sentence in English, no more than 80 characters, no Conventional Commits prefix (`feat:`, `fix:`, etc.).\n" +
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
			"  - Placeholder text for missing data: 无关联 / N/A / 无 — omit the section entirely instead.\n" +
			"- Keep it concise.\n"
	case "mixed":
		return "## PR 摘要模板（强制，覆盖 pr_author 角色默认中文规则）\n" +
			"- PR 标题：可用中文或英文，与项目既有提交历史风格保持一致（不超过 40 字，不要 Conventional Commits 前缀）。\n" +
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
			"  - 占位文本：无关联 / N/A / 无 —— 完全空白的 section 整个省略\n" +
			"- 简洁有重点。\n"
	default: // "" 或 "zh"：默认中文
		return "## PR 摘要要求\n" +
			"- PR 标题使用中文，一句话概括本次改动（不超过 40 字，不要以 `feat:` 等前缀开头）。\n" +
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
			"  - 占位文本：无关联 / N/A / 无 —— 完全空白的 section 整个省略\n" +
			"- 简洁有重点。\n"
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
// 函数名采用 `New<Struct>` 构造函数惯例，因为类型 PRShellTemplates 与
// 构造函数同名会与 Go 同包命名空间冲突。
func NewPRShellTemplates(lang string) PRShellTemplates {
	switch ResolveCommitLang(lang, "") {
	case "en":
		return PRShellTemplates{
			CommitMessageDefaultFmt:  "NovaWorkbench auto-push: update %s",
			PRTitleFallback:          "NovaWorkbench auto-push: %s",
			PRBodyWhatChangedHeading: "## What changed",
			PRBodyNotesHeading:       "## Notes",
			PRBodyNotesChecklist:     "- [ ] Self-tested\n- [ ] No debug code",
		}
	default: // "" / "zh" / "mixed" — 中文默认
		return PRShellTemplates{
			CommitMessageDefaultFmt:  "NovaWorkbench auto-push 更新 %s",
			PRTitleFallback:          "NovaWorkbench auto-push: %s",
			PRBodyWhatChangedHeading: "## 改了什么",
			PRBodyNotesHeading:       "## 备注",
			PRBodyNotesChecklist:     "- [ ] 自测通过\n- [ ] 无调试代码",
		}
	}
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
