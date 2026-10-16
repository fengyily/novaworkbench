// Block rendering for the project-level git context — the "本项目 Git 约定"
// prompt block that is appended right after GitCommitConvention on every
// prompt that may drive the claude child to run `git commit` / `git push` /
// `gh pr create`. Holds NO token value (by design — see the comment on
// GitProjectContext.PlatformType / the absence of a Token field), so the
// block is safe to inline in any task prompt.
//
// Kept in package prompt (alongside git_convention.go) so that any caller
// already pulling in GitCommitConvention can pull this too without adding
// new imports. The function takes plain strings / ints — callers render
// `service.StyleHint(lang, override)` / `service.PRLangRules(lang)` upstream
// before passing them in, so the prompt package stays free of any
// `service` / `model` dependency and can't be involved in import cycles.

package prompt

import (
	"fmt"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// gitContextScriptCapBytes is the maximum size (in bytes) of the inline
// commit_script that BuildGitProjectBlock will embed into the prompt. Past
// 8 KB the script starts to dominate prompt volume without adding reliability
// (the model's actual instruction-compliance budget is already saturated by
// GitCommitConvention + the project-context block). When the script exceeds
// the cap we render a "script too long" hint instead of the script text so
// the model doesn't get confused by partial script bodies.
const gitContextScriptCapBytes = 8 * 1024

// GitProjectContext carries everything BuildGitProjectBlock needs to render
// the per-project block. The struct deliberately holds NO token value —
// callers pass the token name (label) but never the secret itself, so a
// regression that forwards this struct into a log / a prompt can never leak
// the secret. The actual token bytes live in platform_tokens.token and are
// injected into the child process env via platformRunEnv (handler package).
//
// Field semantics:
//   - PlatformType — "github" / "gitlab" / "gitea" / "". Empty means the
//     project has no platform configured; the renderer treats this as
//     "no Token-bound automation available" and falls back to a manual /
//     compare-URL wording.
//   - TokenBound — true when a platform_tokens row is actually linked. False
//     either because the project has no platform_token_id or because the
//     token row was deleted out from under the project.
//   - TokenName — platform_tokens.name. Display only, never used for auth.
//   - APIBase — platform_tokens.base_url (or "api.github.com" for the
//     github-com default). Used to populate the $NOVA_PLATFORM_API_BASE
//     reference in the prompt. Empty treated as "omit the env-var line".
//   - RemoteHost — parsed from project.remote_url with userinfo stripped.
//     Empty means no remote URL. Used to choose "SSH" vs "HTTPS" phrasing.
//   - SSHRemote — true iff RemoteHost is reachable via SSH. When true the
//     renderer drops token-related instructions because SSH auth depends
//     on the host's ssh-agent / config, not a Nova-injected token.
//   - StyleHint / PRLangRules — pre-rendered strings (see service.StyleHint
//     / service.PRLangRules). Pass empty strings to skip the section.
//   - CommitMode — model.CommitMode* four-value constant. Empty treated as
//     "model.CommitModeLLMOnly" so the renderer omits the script section.
//   - CommitScript — the raw shell snippet the user configured under "提交
//         推送模式". Only rendered inline for the three non-llm_only modes
//     and only when it fits under gitContextScriptCapBytes.
type GitProjectContext struct {
	PlatformType string
	TokenBound   bool
	TokenName    string
	APIBase      string
	RemoteHost   string
	SSHRemote    bool
	StyleHint    string
	PRLangRules  string
	CommitMode   string
	CommitScript string
}

// BuildGitProjectBlock renders the per-project "本项目 Git 约定" block. The
// returned string is meant to be appended to a task prompt immediately after
// GitCommitConvention (the existing "禁 AI 署名 + 凭据不手写" hard rules) so
// every coding / sub-task / push sub-agent sees the same project-level
// expectations.
//
// Output shape (Chinese): three sections, each headed with a `##` line:
//
//	## 本项目 Git 约定
//	### 平台与凭据
//	<wording depends on TokenBound / SSHRemote>
//	### 提交风格
//	<StyleHint body or omitted when empty>
//	[...PRLangRules body when forPR=true]
//	### 提交推送模式（commit_mode: <value>）
//	<wording depends on CommitMode; embeds the script for the three
//	 non-llm_only modes>
//
// forPR toggles the extra PR-only language rules block (PRLangRules already
// rendered by the caller). Commit-only prompts pass false so the prompt
// stays tight.
//
// The function never panics on missing data: an entirely empty GitProjectContext
// returns a minimal block containing just the heading + a "未配置" note, so
// a panic-prone upstream panic can't turn a misconfigured DB into a denial
// of service.
func BuildGitProjectBlock(c GitProjectContext, forPR bool) string {
	var b strings.Builder
	b.WriteString("\n\n## 本项目 Git 约定\n")
	b.WriteString("### 平台与凭据\n")
	b.WriteString(renderPlatformSection(c))

	// Style section — only when the caller pre-rendered a StyleHint. The
	// string is already a single-line instruction (service.StyleHint), so we
	// emit it verbatim without further wrapping.
	if hint := strings.TrimSpace(c.StyleHint); hint != "" {
		b.WriteString("\n\n### 提交风格\n")
		b.WriteString(hint)
		b.WriteString("\n")
	}

	// PR language rules (PR-title / body wording) — only for prompts that
	// actually create a PR. forPR=false (manual coding / sub-task) skips
	// this so a development prompt doesn't carry rules the agent won't use.
	if forPR {
		if rules := strings.TrimSpace(c.PRLangRules); rules != "" {
			b.WriteString("\n")
			b.WriteString(rules)
			b.WriteString("\n")
		}
	}

	// Commit-mode section — last so it lives next to the script body. Empty
	// CommitMode falls back to llm_only (the historical default), matching
	// merge_generate.go's "未配置策略" branch.
	mode := c.CommitMode
	if mode == "" {
		mode = model.CommitModeLLMOnly
	}
	b.WriteString("\n\n### 提交推送模式（commit_mode: ")
	b.WriteString(mode)
	b.WriteString("）\n")
	b.WriteString(renderCommitModeSection(c, mode))

	return b.String()
}

// renderPlatformSection picks the wording for the "平台与凭据" subsection.
// Three shapes:
//   - TokenBound && !SSHRemote → the canonical "已注入环境" wording, listing
//     $NOVA_PLATFORM_TOKEN + platform aliases.
//   - TokenBound && SSHRemote → "你已绑定 Token 但 remote 是 SSH，Token
//     仅用于平台 REST API，git push 走 ssh-agent 凭据"
//   - !TokenBound             → "未配置 / 凭据或权限问题请报告并停止".
//
// Returns a single paragraph (no leading newline — the caller owns spacing).
// The prompt explicitly forbids echoing / cat-ing the variable values; that
// line is present in every shape that has a TokenBound so the leak vector
// stays in front of the model.
func renderPlatformSection(c GitProjectContext) string {
	switch {
	case !c.TokenBound:
		return "- 未配置平台 Token / Token 已失效。git push 与 REST API 调用都没有可用凭据，" +
			"**遇认证失败请报告并停止，不要尝试在命令中手输凭据、不要伪造提交者身份**。"
	case c.SSHRemote:
		// Token is wired into the env for REST API use, but git push on the
		// SSH remote itself depends on the agent host's ssh-agent / ~/.ssh/config.
		// The model must not surface the token to push; if ssh auth fails we
		// want a clean "权限问题" report, not improvised credentials.
		return fmt.Sprintf(
			"- 平台：%s；已绑定 Token「%s」。但项目 remote 走 SSH（%s）—— %s / API 调用可使用 `$NOVA_PLATFORM_TOKEN`（%s）调用，git push 由 SSH 凭据（ssh-agent / ~/.ssh/config）提供。**严禁 echo / cat / 重定向到文件 / 在命令行明文粘贴该变量，也不得写进 commit 信息或 PR 正文**；SSH 认证失败请报告并停止，不要尝试手工输入凭据。",
			displayPlatform(c.PlatformType), c.TokenName, hostOrEmpty(c.RemoteHost),
			platformAliasList(c.PlatformType), envBaseRef(c.APIBase),
		)
	default:
		return fmt.Sprintf(
			"- 平台：%s；已绑定 Token「%s」。本次运行的凭据已注入环境：git push / fetch 走 GIT_ASKPASS 自动认证，直接 `git push origin <分支>` 即可；需要调用 `%s` 或平台 REST API 时，Token 在环境变量 `$NOVA_PLATFORM_TOKEN`（%s），API 地址在 `%s`。**严禁 echo / cat / 重定向到文件 / 在命令行明文粘贴该变量的值，也不得写进 commit 信息或 PR 正文**。",
			displayPlatform(c.PlatformType), c.TokenName,
			platformCLIName(c.PlatformType), platformAliasList(c.PlatformType), envBaseRef(c.APIBase),
		)
	}
}

// renderCommitModeSection writes the three possible commit-mode bodies. The
// three non-llm_only modes inline the user-configured shell snippet inside a
// ```sh fence (when it fits under the 8 KB cap; otherwise a "脚本过长" hint
// replaces it). llm_only deliberately omits the script body so a project
// that picked "always LLM" doesn't leak it.
func renderCommitModeSection(c GitProjectContext, mode string) string {
	switch mode {
	case model.CommitModeScriptOnly:
		return renderScriptOnlySection(c)
	case model.CommitModeScriptFirst:
		return renderScriptWithRole(c, "先执行脚本：脚本 stdout 作为 commit 信息；脚本 stdout 为空 / 退出非零 / 输出语言明显与项目风格不符时，再由你撰写。", "脚本失败时**不要**自行编造信息。")
	case model.CommitModeLLMFirst:
		return renderScriptWithRole(c, "先由你撰写；撰写失败（超时 / API 错误 / 思路卡壳）时，执行下方脚本兜底，脚本 stdout 作为 commit 信息。", "脚本失败时**不要**自行编造信息。")
	case model.CommitModeLLMOnly:
		fallthrough
	default:
		return "- 由你直接撰写 commit 信息。注意遵循上面的「提交风格」约束。"
	}
}

// renderScriptOnlySection is the script_only variant: the model MUST run the
// script and use its stdout verbatim.
func renderScriptOnlySection(c GitProjectContext) string {
	return "- **必须**先执行下方脚本（`sh -c` 在当前工作目录运行），把脚本 stdout 原样作为 commit 信息，**不得自行改写**。脚本失败 → 立即停止并报告，**不要**自行编造 commit 信息。\n" +
		"- 工作目录已注入：`NOVA_COMMIT_REQUIREMENT_ID` / `NOVA_COMMIT_PROJECT_DIR` / `NOVA_COMMIT_WORKTREE`（只读，给脚本用）。\n" +
		renderScriptBody(c.CommitScript)
}

// renderScriptWithRole is the shared body for script_first / llm_first:
// both render the script body, then layer on mode-specific wording.
func renderScriptWithRole(c GitProjectContext, lead, fallback string) string {
	return "- " + lead + "\n" +
		"- 工作目录已注入：`NOVA_COMMIT_REQUIREMENT_ID` / `NOVA_COMMIT_PROJECT_DIR` / `NOVA_COMMIT_WORKTREE`（只读，给脚本用）。\n" +
		"- " + fallback + "\n" +
		renderScriptBody(c.CommitScript)
}

// renderScriptBody returns either the script wrapped in a ```sh fence, or a
// one-line hint when the script exceeds the size cap (or is empty).
func renderScriptBody(script string) string {
	script = strings.TrimRight(script, "\n")
	switch {
	case script == "":
		return "- （项目未填写 commit_script；该模式实际等同于 llm_only。）"
	case len(script) > gitContextScriptCapBytes:
		return fmt.Sprintf("- （项目 commit_script 超过 %d 字节，未内联；请向用户确认 commit 信息后再提交。）", gitContextScriptCapBytes)
	default:
		return "- 脚本正文（sh -c 在工作目录执行）：\n\n```sh\n" + script + "\n```"
	}
}

// displayPlatform returns a friendly Chinese / English platform label. Empty
// stays empty so the renderer can detect "unconfigured" cleanly.
func displayPlatform(p string) string {
	switch p {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "gitea":
		return "Gitea"
	default:
		return "（未配置）"
	}
}

// platformAliasList returns the conventional platform-specific CLI / env var
// names. Used inside the credential reminder so the model knows which tool
// to reach for first.
func platformAliasList(p string) string {
	switch p {
	case "github":
		return "GitHub 另有 `$GH_TOKEN`，gh CLI 用它自动认证"
	case "gitlab":
		return "GitLab 另有 `$GITLAB_TOKEN` 与 `$GITLAB_HOST`，glab / curl 用它自动认证"
	case "gitea":
		return "Gitea 另有 `$GITEA_TOKEN`，tea CLI 用它自动认证"
	default:
		return "平台原生别名未配置"
	}
}

// platformCLIName returns the conventional CLI name for the platform
// (used inside the "如需调用 X" hint).
func platformCLIName(p string) string {
	switch p {
	case "github":
		return "gh"
	case "gitlab":
		return "glab"
	case "gitea":
		return "tea"
	default:
		return "平台 CLI"
	}
}

// hostOrEmpty returns the host string verbatim. Empty stays empty so a
// "(未配置 remote)" sentence can be assembled upstream if needed.
func hostOrEmpty(h string) string {
	if strings.TrimSpace(h) == "" {
		return "remote 未配置"
	}
	return h
}

// envBaseRef returns "in `$NOVA_PLATFORM_API_BASE`" or a fallback when the
// base URL is empty. We never inline the actual URL here (it's already
// injected via env); the prompt only needs to point the model at the
// variable name.
func envBaseRef(apiBase string) string {
	if strings.TrimSpace(apiBase) == "" {
		return "API 地址未配置，必要时使用平台默认值"
	}
	return "API 地址在 `$NOVA_PLATFORM_API_BASE`"
}
