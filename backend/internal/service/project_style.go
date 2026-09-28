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
