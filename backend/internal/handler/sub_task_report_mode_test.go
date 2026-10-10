package handler

import (
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
