package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestLooksSimpleTask covers the auto-heuristic. The motivating case is
// req_b19b2ccc9775809f's manual "好像没推送，请推送吧" — a single 8-rune
// Chinese sentence — which must fall on the brief path. The negative cases
// pin down the Markdown-signals branch (lists / headings / code fences) and
// the length / line-count branches.
func TestLooksSimpleTask(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		// positive
		{name: "motivating example (single Chinese sentence)", body: "好像没推送，请推送吧", want: true},
		{name: "single English sentence", body: "please push the branch", want: true},
		{name: "two short lines", body: "推一下\n先跑测试", want: true},
		{name: "blank-prefixed short body", body: "\n   \n好像没推送，请推送吧\n", want: true},
		{name: "empty", body: "", want: true},
		{name: "CRLF normalized", body: "好像没推送，请推送吧\r\n", want: true},

		// negative — long
		{name: "long single line", body: "请把整个 auth 模块从 session 模式重构成 JWT 模式，保留所有外部 API 兼容，迁移所有存量用户、加上双写回退、跑全量回归，确保线上无感知切换；如果有问题立刻回滚并把变更回退。", want: false},
		{name: "many short lines", body: "第一件事\n第二件事\n第三件事\n第四件事", want: false},

		// negative — markdown signals
		{name: "bullet list", body: "做三件事：\n- 第一\n- 第二\n- 第三", want: false},
		{name: "ordered list", body: "步骤：\n1. foo\n2. bar", want: false},
		{name: "heading", body: "## 重构鉴权", want: false},
		{name: "code fence", body: "修一下：\n```bash\ngit push\n```", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := looksSimpleTask(c.body)
			if got != c.want {
				t.Errorf("looksSimpleTask(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// TestResolveReportMode pins down the (mode, body) → (effective) table.
// "auto" runs the heuristic, explicit brief/full pass through, unknown values
// fall back to "auto" (so a future client typo can't break a row).
func TestResolveReportMode(t *testing.T) {
	short := "好像没推送，请推送吧"
	long := "请把整个 auth 模块从 session 模式重构成 JWT 模式，保留所有外部 API 兼容，迁移所有存量用户、加上双写回退、跑全量回归，确保线上无感知切换；如果有问题立刻回滚并把变更回退。"
	cases := []struct {
		mode string
		body string
		want string
	}{
		{mode: "auto", body: short, want: model.SubTaskReportModeBrief},
		{mode: "auto", body: long, want: model.SubTaskReportModeFull},
		{mode: "", body: short, want: model.SubTaskReportModeBrief},
		{mode: "", body: long, want: model.SubTaskReportModeFull},
		{mode: "brief", body: long, want: model.SubTaskReportModeBrief},
		{mode: "full", body: short, want: model.SubTaskReportModeFull},
		{mode: "garbage", body: short, want: model.SubTaskReportModeBrief},
		{mode: "garbage", body: long, want: model.SubTaskReportModeFull},
	}
	for _, c := range cases {
		label := c.mode + "/"
		if len(c.body) > 6 {
			label += c.body[:6]
		} else {
			label += c.body
		}
		t.Run(label, func(t *testing.T) {
			got := resolveReportMode(c.mode, c.body)
			if got != c.want {
				t.Errorf("resolveReportMode(%q, %q) = %q, want %q", c.mode, c.body, got, c.want)
			}
		})
	}
}

// TestExtractPRURL pins down the line-by-line first-URL extractor used by
// finishSubTask's push_pr happy path. The function must:
//   - return the first trimmed line starting with http:// or https://
//   - skip prose lines that contain URLs but aren't URL lines themselves
//   - return "" when no URL line is present (NOT fall back to the full text —
//     see push_pr_shell.go's extractURL for the contrast)
func TestExtractPRURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single https line",
			in:   "✅ PR 创建成功\nhttps://github.com/owner/repo/pull/42",
			want: "https://github.com/owner/repo/pull/42",
		},
		{
			name: "single http line",
			in:   "http://gitlab.example.com/owner/repo/-/merge_requests/7",
			want: "http://gitlab.example.com/owner/repo/-/merge_requests/7",
		},
		{
			name: "URL on a middle line, prose around it",
			in:   "已推送并创建 PR\n\n下面是链接：\nhttps://github.com/owner/repo/pull/42\n\n收工。",
			want: "https://github.com/owner/repo/pull/42",
		},
		{
			name: "URL with trailing whitespace gets trimmed",
			in:   "  https://github.com/owner/repo/pull/42  \n",
			want: "https://github.com/owner/repo/pull/42",
		},
		{
			name: "no URL — empty result (NOT full text fallback)",
			in:   "✅ PR 创建成功，但 Claude 没粘贴 URL",
			want: "",
		},
		{
			name: "URL embedded mid-line in prose — NOT matched (no full-text fallback)",
			in:   "详情见 https://github.com/owner/repo/pull/42 这个 PR。",
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "URL after code fence — still matched when on its own line",
			in:   "```\n$ gh pr create\n```\nhttps://github.com/owner/repo/pull/42",
			want: "https://github.com/owner/repo/pull/42",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractPRURL(c.in)
			if got != c.want {
				t.Errorf("extractPRURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestAppendPRLineToBody pins down the artifact-body trailing-line appender
// used by finishSubTask's push_pr happy path. The function must:
//   - append a "\n\n**PR**: <url>" line when url is non-empty AND the body
//     does NOT already contain it
//   - skip the append when url is empty
//   - skip the append when the body already contains url (avoid duplication)
//   - return the body untouched on either skip path
func TestAppendPRLineToBody(t *testing.T) {
	const url = "https://github.com/owner/repo/pull/42"
	cases := []struct {
		name string
		body string
		url  string
		want string
	}{
		{
			name: "URL appended with blank-line separator",
			body: "✅ PR 创建成功",
			url:  url,
			want: "✅ PR 创建成功\n\n**PR**: " + url,
		},
		{
			name: "empty URL — body untouched",
			body: "✅ PR 创建成功",
			url:  "",
			want: "✅ PR 创建成功",
		},
		{
			name: "URL already present — body untouched (no duplicate)",
			body: "✅ PR 创建成功\n\n链接：https://github.com/owner/repo/pull/42",
			url:  url,
			want: "✅ PR 创建成功\n\n链接：https://github.com/owner/repo/pull/42",
		},
		{
			name: "empty body + URL — just the appended line",
			body: "",
			url:  url,
			want: "\n\n**PR**: " + url,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := appendPRLineToBody(c.body, c.url)
			if got != c.want {
				t.Errorf("appendPRLineToBody(%q, %q) = %q, want %q", c.body, c.url, got, c.want)
			}
		})
	}
}

// TestPushPRHappyPathContract is a focused contract test for the push_pr
// happy-path event emission. It does NOT exercise finishSubTask end-to-end
// (that path needs DB + Claude subprocess + JobStore plumbing); instead it
// pins down the two contracts that DO drive the user-visible report:
//   1. extractPRURL picks the URL out of a realistic Claude finalResult
//      → that URL is what the SSE `pr_link` event carries (drives
//      applyMergeSignals → "创建 PR / 查看 PR" CTA on the merge panel)
//   2. appendPRLineToBody appends a trailing "**PR**: <url>" line ONLY when
//      the body doesn't already contain the URL (avoids duplication). The
//      contract is "body ends up containing the URL" — either via Claude's
//      own paste (idempotent no-op) or via our append (force-visible).
//
// Together these pin down the user-visible wire contract without standing
// up the DB / claude subprocess / JobStore.
func TestPushPRHappyPathContract(t *testing.T) {
	const url = "https://github.com/owner/repo/pull/42"

	// Realistic Claude finalResult: success prose + URL on its own line.
	replyWithURL := "✅ 已完成提交、推送、创建 PR。\n\n" + url

	// 1. SSE pr_link event content — the URL we surface to applyMergeSignals.
	if got := extractPRURL(replyWithURL); got != url {
		t.Fatalf("extractPRURL(%q) = %q, want %q", replyWithURL, got, url)
	}

	// 2a. artifact body — when Claude already pasted it inline, our append
	// must be idempotent (no duplicate "**PR**" line).
	body := appendPRLineToBody(replyWithURL, url)
	if body != replyWithURL {
		t.Fatalf("appendPRLineToBody must be idempotent when URL already present; got %q, want %q", body, replyWithURL)
	}
	if !strings.Contains(body, url) {
		t.Fatalf("artifact body must contain URL %q; got: %q", url, body)
	}

	// 2b. artifact body — when body somehow lost the URL (e.g. URL was
	// inside a Markdown fence that got stripped, or truncation clipped it),
	// appendPRLineToBody MUST restore it as a trailing "**PR**: <url>" line
	// so the SubTaskPanel MarkdownRender still surfaces it.
	bareBody := "✅ 已完成提交、推送、创建 PR。" // URL stripped / not pasted
	body = appendPRLineToBody(bareBody, url)
	if !strings.HasSuffix(body, "**PR**: "+url) {
		t.Fatalf("artifact body must end with '**PR**: %s' when URL missing from input; got: %q", url, body)
	}
	if !strings.Contains(body, url) {
		t.Fatalf("artifact body must contain URL %q; got: %q", url, body)
	}
}
