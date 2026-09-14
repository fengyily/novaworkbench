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
//	"📝 项目提交风格：中文"
//	"📝 项目提交风格：English"
//	"📝 项目提交风格：中英混用"
//
// 两者皆空时返回空字符串，调用方应自行决定是否渲染。
func StyleHint(lang, override string) string {
	switch ResolveCommitLang(lang, override) {
	case "zh":
		return "📝 项目提交风格：中文"
	case "en":
		return "📝 项目提交风格：English"
	case "mixed":
		return "📝 项目提交风格：中英混用"
	default:
		return ""
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
