package handler

import (
	"fmt"
	"regexp"

	"github.com/novaworkbench/backend/internal/llm"
)

// Failure classification for claude CLI failures.
//
// The claude CLI aborts a turn whose reply exceeds the model's per-response
// output limit and reports it in the terminal "result" event:
//
//	API Error: Claude's response exceeded the 128000 output token maximum.
//	To configure this behavior, set the CLAUDE_CODE_MAX_OUTPUT_TOKENS environment variable.
//
// That text is the ONLY stable signal: the CLI's internal telemetry name
// (tengu_max_tokens_reached) never reaches us, and the event carries no
// dedicated field for it. Classifying by text therefore works across CLI
// versions and across third-party gateways that relay the same message.
//
// We deliberately do NOT expose a CLAUDE_CODE_MAX_OUTPUT_TOKENS knob. That env
// can only make the CLI pick a DIFFERENT limit: raising it past the model's
// real ceiling gets the request rejected upstream ("max_tokens too large"),
// lowering it truncates even earlier. The limit the CLI reports IS the model's
// real ceiling, so the switch would be pure misdirection. The fix for this
// failure mode is to write less in one turn (see prompt.OutputBudgetClause)
// and to resume the session when it happens anyway.
const (
	// FailureKindOutputTokenLimit: the model filled its per-response output
	// budget and the reply was truncated mid-flight. The turn did not
	// complete; the work already written to the working tree survives.
	FailureKindOutputTokenLimit = "output-token-limit"
	// FailureKindContextWindow: the session's context window is exhausted.
	FailureKindContextWindow = "context-window-exhausted"
)

var (
	// The CLI's own wording, verbatim (both the streaming and the
	// non-streaming variant share the "exceeded the N output token maximum"
	// fragment; the non-streaming one continues with "for a request sent
	// without streaming").
	reOutputTokenLimit = regexp.MustCompile(`(?i)exceeded the\s+(\d+)\s+output token maximum`)
	// The CLI's context-window wording.
	reContextWindow = regexp.MustCompile(`(?i)has reached its context window limit`)
)

// classifyClaudeFailure recognises the CLI failure texts we know how to act
// on. It returns ("", 0) for anything else — the caller then passes the
// original message through untouched.
//
// limitTokens carries the N the CLI reported (e.g. 128000) so the user-facing
// hint can name the actual ceiling instead of guessing.
func classifyClaudeFailure(msg string) (kind string, limitTokens int) {
	if msg == "" {
		return "", 0
	}
	if m := reOutputTokenLimit.FindStringSubmatch(msg); m != nil {
		// The output-limit class wins over the context-window class: it is
		// the one we can auto-resume, and a message can mention both.
		kind = FailureKindOutputTokenLimit
		fmt.Sscanf(m[1], "%d", &limitTokens)
		return kind, limitTokens
	}
	if reContextWindow.MatchString(msg) {
		return FailureKindContextWindow, 0
	}
	return "", 0
}

// claudeFailureHint returns the Chinese, actionable guidance shown to the
// user for a known failure class. Unknown kinds get "" — the original English
// message is then shown as-is.
func claudeFailureHint(kind string, limitTokens int) string {
	switch kind {
	case FailureKindOutputTokenLimit:
		limit := "模型单次输出上限"
		if limitTokens > 0 {
			limit = fmt.Sprintf("模型单次输出上限（%d tokens）", limitTokens)
		}
		return "模型这一轮的输出达到了" + limit + "被截断，任务未完成。已写入工作区的改动不会丢失。" +
			"平台已在原会话上自动续跑；若仍失败，建议把需求拆分为子任务，或让模型分多次、小步修改文件。"
	case FailureKindContextWindow:
		return "本次会话的上下文窗口已用尽。请先「压缩上下文」再继续，或把大需求拆分为子任务分次执行。"
	}
	return ""
}

// outputLimitResumeOpts rewrites a coding run's options into the shape that
// resumes the SAME conversation instead of starting a new one.
//
// The four rewritten fields:
//
//   - SessionID / Resume / Fork / ForkSessionID: continue the session that
//     just got truncated. Fork is OFF on purpose — a fork would create a new
//     session and lose the truncated turn's context, which is exactly the
//     context the model needs to continue.
//   - SystemPrompt: CLEARED. This mirrors the production-proven "继续开发"
//     path (ContinueCoding's continueOpts): the coding session was forked
//     from the design/analysis session and already carries the developer
//     persona in its context. Injecting --system-prompt again on resume
//     REPLACES the persona rather than adding to it, which would drop the
//     role's instructions.
//
// Every other field (Model / Effort / ClaudeConfigID / WorkDir / ExtraEnv /
// AddDirs / ClaudeMdDir / ClaudeMdExcludes / ExcludeRepoClaudeMd /
// AppendSystemPrompt / DisallowedTools / PermissionMode /
// OverrideSettingSources / IdleTimeout / HardTimeout) is carried over
// unchanged — the resumed run must execute under the same conditions as the
// run it continues.
func outputLimitResumeOpts(base llm.StreamOpts, sessionID string, resumePrompt string) llm.StreamOpts {
	out := base
	out.SessionID = sessionID
	out.Resume = true
	out.Fork = false
	out.ForkSessionID = ""
	out.SystemPrompt = ""
	out.Prompt = resumePrompt
	return out
}

// outputLimitResumePrompt is the prompt sent with the resumed run. It has two
// jobs: tell the model the previous reply was cut short (so it checks the
// working tree instead of redoing finished work), and re-impose the
// "write in small pieces" constraint that the truncated turn violated.
func outputLimitResumePrompt() string {
	return `上一次回复因模型单次输出达到上限而被截断，任务尚未完成。
请先检查工作区当前状态（已完成的改动不要重复做），然后从中断处继续完成剩余工作。
为避免再次触发输出上限，请遵守：
- 单次 Write / Edit 的新增内容控制在 300 行以内；大文件分段写入（先写骨架，再逐段 Edit 追加）。
- 优先用 Edit 做小步改动，避免一次性重写整个文件。
- 不要在回复正文里粘贴文件全文或大段代码。
最后用中文总结本次完成的内容。`
}

// outputLimitAutoResumeWarning is the log line shown when the platform
// resumes a truncated coding turn on its own.
func outputLimitAutoResumeWarning(attempt, max int) string {
	return fmt.Sprintf("🔄 模型单次输出达到上限被截断，正在原会话上自动续跑（第 %d/%d 次）…", attempt, max)
}

// outputLimitStillFailedHint is the log line appended when the resumed run
// hit the same wall. It points at the NEXT level of intervention — splitting
// the requirement — because another resume would just burn tokens.
func outputLimitStillFailedHint() string {
	return "⚠️ 自动续跑后仍未完成：建议把需求拆分为子任务分次执行，或在「设置 → Claude 配置」调高「输出上限保护」的自动续跑次数。" +
		"（注意：调大模型自身的输出上限对本次问题无效，根因是一次写入过大。）"
}
