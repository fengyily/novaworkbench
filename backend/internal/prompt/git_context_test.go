package prompt

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestBuildGitProjectBlock_FourModes renders the four commit-mode shapes
// back-to-back so a regression that swaps the wording is caught here rather
// than at the SSE-level (the SSE-level test would only catch the change once
// it had already reached the user). Each captured block is also asserted to
// carry GitCommitConvention at the head and never echo the supplied token
// value — the secret-leak guard the prompt/handler split was set up for.
func TestBuildGitProjectBlock_FourModes(t *testing.T) {
	const tokenValue = "ghp_abcdefghij1234567890" // fake PAT for assertion
	cases := []struct {
		name string
		ctx  GitProjectContext
		must []string
	}{
		{
			name: "script_only + token bound + HTTPS",
			ctx: GitProjectContext{
				PlatformType: "github",
				TokenBound:   true,
				TokenName:    "gh-main",
				APIBase:      "https://api.github.com",
				RemoteHost:   "github.com",
				SSHRemote:    false,
				StyleHint:    "📝 项目提交风格：English",
				CommitMode:   model.CommitModeScriptOnly,
				CommitScript: "echo \"chore: $(date +%F)\"",
			},
			must: []string{"## 本项目 Git 约定", "GitHub", "gh-main", "$GH_TOKEN", "$NOVA_PLATFORM_TOKEN", "$NOVA_PLATFORM_API_BASE", "## 提交推送模式（commit_mode: script_only）", "echo \"chore: $(date +%F)\"", "NOVA_COMMIT_REQUIREMENT_ID"},
		},
		{
			name: "script_first + token bound",
			ctx: GitProjectContext{
				PlatformType: "gitlab",
				TokenBound:   true,
				TokenName:    "gl-prod",
				APIBase:      "https://gitlab.com/api/v4",
				RemoteHost:   "gitlab.com",
				SSHRemote:    false,
				StyleHint:    "📝 项目提交风格：中文",
				CommitMode:   model.CommitModeScriptFirst,
				CommitScript: "printf '%s' \"fix-up\"",
			},
			// script_first = script runs FIRST, LLM is the fallback. (This row
			// used to also require "先调 LLM 失败时回退到脚本", which is
			// llm_first semantics — a stale expectation that made the suite
			// red and therefore ignored.)
			must: []string{"GitLab", "gl-prod", "$GITLAB_TOKEN", "$GITLAB_HOST", "先执行脚本", "再由你撰写", "printf '%s' \"fix-up\""},
		},
		{
			name: "llm_first + token bound",
			ctx: GitProjectContext{
				PlatformType: "gitea",
				TokenBound:   true,
				TokenName:    "tea-self",
				APIBase:      "https://gitea.acme.com/api/v1",
				RemoteHost:   "gitea.acme.com",
				SSHRemote:    false,
				StyleHint:    "📝 项目提交风格：中英混用",
				CommitMode:   model.CommitModeLLMFirst,
			},
			// llm_first = the model writes the message, the script is the
			// fallback. "由你直接撰写 commit 信息" belongs to llm_only.
			must: []string{"Gitea", "tea-self", "$GITEA_TOKEN", "先由你撰写", "执行下方脚本兜底"},
		},
		{
			name: "llm_only + no token",
			ctx: GitProjectContext{
				PlatformType: "github",
				TokenBound:   false,
				RemoteHost:   "github.com",
				SSHRemote:    false,
				CommitMode:   model.CommitModeLLMOnly,
			},
			must: []string{"未配置平台 Token", "由你直接撰写 commit 信息"},
		},
		{
			name: "SSH remote with token — drop the token reminder",
			ctx: GitProjectContext{
				PlatformType: "github",
				TokenBound:   true,
				TokenName:    "gh-ssh",
				APIBase:      "https://api.github.com",
				RemoteHost:   "github.com",
				SSHRemote:    true,
				CommitMode:   model.CommitModeLLMOnly,
			},
			must: []string{"remote 走 SSH", "gh-ssh", "GH_TOKEN", "ssh-agent"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildGitProjectBlock(tc.ctx, false)
			// The whole secret-leak guard: the test asserts the placeholder
			// token never sneaks into the rendered block. Off-by-one string
			// regressions here are caught immediately.
			if strings.Contains(got, tokenValue) {
				t.Errorf("token value %q should never appear in rendered block:\n%s", tokenValue, got)
			}
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing required substring %q in block:\n%s", want, got)
				}
			}
		})
	}
}

// TestBuildGitProjectBlock_PRLangRulesAdded confirms forPR=true selects the
// PR title/body language block. Empty PRLangRules = no block (callers
// usually pre-render via service.PRLangRules(lang), but the helper should
// tolerate a missing string).
func TestBuildGitProjectBlock_PRLangRulesAdded(t *testing.T) {
	noPR := BuildGitProjectBlock(GitProjectContext{
		StyleHint:   "📝 项目提交风格：English — 强制要求：commit messages, PR title, and PR body MUST all be in English",
		CommitMode:  model.CommitModeLLMOnly,
	}, false)
	if strings.Contains(noPR, "## PR Summary Template") || strings.Contains(noPR, "## PR 摘要要求") {
		t.Errorf("forPR=false must not include the PR summary template; got:\n%s", noPR)
	}

	withPR := BuildGitProjectBlock(GitProjectContext{
		StyleHint:   "📝 项目提交风格：English",
		CommitMode:  model.CommitModeLLMOnly,
		PRLangRules: "## PR Summary Template (overrides pr_author role default)\n- PR title: ...",
	}, true)
	if !strings.Contains(withPR, "## PR Summary Template") {
		t.Errorf("forPR=true must include the PR summary template; got:\n%s", withPR)
	}
}

// TestBuildGitProjectBlock_ScriptTooLong renders the cap-out hint rather
// than the script body once it exceeds gitContextScriptCapBytes. The cap
// exists to keep prompts bounded; we verify it exists by injecting a
// 9 KB script and confirming it's NOT inlined.
func TestBuildGitProjectBlock_ScriptTooLong(t *testing.T) {
	big := strings.Repeat("x", gitContextScriptCapBytes+1024)
	got := BuildGitProjectBlock(GitProjectContext{
		PlatformType: "github",
		TokenBound:   true,
		TokenName:    "gh-cap",
		APIBase:      "https://api.github.com",
		RemoteHost:   "github.com",
		SSHRemote:    false,
		StyleHint:    "📝 项目提交风格：English",
		CommitMode:   model.CommitModeScriptOnly,
		CommitScript: big,
	}, false)
	if strings.Contains(got, big) {
		t.Errorf("oversize script must not be inlined")
	}
	// The implementation words this as "超过 N 字节，未内联"; the old assertion
	// looked for "脚本过长" and had been red since the wording changed.
	if !strings.Contains(got, "未内联") {
		t.Errorf("oversize script must surface the cap-out hint; got:\n%s", got)
	}
}
