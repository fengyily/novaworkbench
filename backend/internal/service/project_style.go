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
// prompt 中的"中文/改动概述..."等硬编码指令；空值返回默认中文块，与未检测
// 到语言时的现有行为一致（零回归）。
//
// 三种返回值：
//   - en    — 强制英文 PR 标题 + Markdown 正文（## Summary / Changes / Key
//     Files / How Verified）
//   - mixed — 中英混用，与项目历史风格一致
//   - "" / "zh" — 默认中文块（与旧硬编码指令同语义）
func PRLangRules(lang string) string {
	switch ResolveCommitLang(lang, "") {
	case "en":
		return "## PR 摘要语言规则（强制，覆盖 pr_author 角色默认中文规则）\n" +
			"- PR title: one sentence in English, no more than 80 characters, no Conventional Commits prefix (`feat:`, `fix:`, etc.).\n" +
			"- PR body: Markdown in English, organized as `## Summary / ## Changes / ## Key Files / ## How Verified`. Keep it concise.\n"
	case "mixed":
		return "## PR 摘要语言规则（强制，覆盖 pr_author 角色默认中文规则）\n" +
			"- PR 标题：可用中文或英文，与项目既有提交历史风格保持一致（不超过 40 字，不要 Conventional Commits 前缀）。\n" +
			"- PR 正文：Markdown，结构按「改动概述 / 主要变更 / 关键文件 / 验证方式」组织，可中英混用。\n"
	default: // "" 或 "zh"：保持现有中文默认行为
		return "## PR 摘要要求\n" +
			"- PR 标题使用中文，一句话概括本次改动（不超过 40 字，不要以 `feat:` 等前缀开头）。\n" +
			"- PR 正文使用 Markdown，按「改动概述 / 主要变更 / 关键文件 / 验证方式」组织，简洁有重点。\n"
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
//   - "en"            → 英文 commit fallback + 英文 PR 标题回退 + 英文
//                       PR 正文段落与 section 标题。
//   - "" / "zh" /
//     "mixed"         → 默认中文，与现有硬编码内容同语义（零回归）。
type PRShellTemplates struct {
	CommitMessageDefaultFmt string // e.g. "NovaWorkbench auto-push: update %s"
	PRTitleFallback         string // e.g. "NovaWorkbench auto-push: %s"
	PRBodyIntroFmt          string // e.g. "Auto-created by ... (based on requirement %s)."
	PRBodySummaryHeading    string // e.g. "## Summary"
	PRBodyCommitsHeading    string // e.g. "## Commits"
	PRBodyStatsHeading      string // e.g. "## Diff Stats"
}

// NewPRShellTemplates 根据 lang 返回本地化模板。分支解析走 ResolveCommitLang：
//
//	"en" → 英文模板
//	"" / "zh" / "mixed" → 中文模板（与现有硬编码内容同语义，零回归）
//
// 函数名采用 `New<Struct>` 构造函数惯例，因为类型 PRShellTemplates 与
// 构造函数同名会与 Go 同包命名空间冲突。
func NewPRShellTemplates(lang string) PRShellTemplates {
	switch ResolveCommitLang(lang, "") {
	case "en":
		return PRShellTemplates{
			CommitMessageDefaultFmt: "NovaWorkbench auto-push: update %s",
			PRTitleFallback:         "NovaWorkbench auto-push: %s",
			PRBodyIntroFmt:          "Auto-created by NovaWorkbench auto-push (based on requirement %s).",
			PRBodySummaryHeading:    "## Summary",
			PRBodyCommitsHeading:    "## Commits",
			PRBodyStatsHeading:      "## Diff Stats",
		}
	default: // "" / "zh" / "mixed" — 中文默认，保持现有硬编码内容
		return PRShellTemplates{
			CommitMessageDefaultFmt: "NovaWorkbench auto-push 更新 %s",
			PRTitleFallback:         "NovaWorkbench auto-push: %s",
			PRBodyIntroFmt:          "由 NovaWorkbench auto-push 自动创建（基于需求 %s）。",
			PRBodySummaryHeading:    "## 改动概述",
			PRBodyCommitsHeading:    "## 提交",
			PRBodyStatsHeading:      "## 变更统计",
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
