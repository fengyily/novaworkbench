package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/service"
)

// TestCreatePRShell_BodyShape exercises the pure buildPRShellBody helper
// across all 4 commit_lang values (en / zh / mixed / "") and asserts the
// 2-section shape (改了什么 + 备注) plus the 8 forbidden substrings the
// user explicitly asked to remove (req_9ead19cd39f632fd).
//
// The helper is called from createPRShell; testing it directly avoids
// having to mock exec.Command (gh/glab/tea) for the PR creation step.
func TestCreatePRShell_BodyShape(t *testing.T) {
	fakeLog := "abc1234 fix: foo\ndef5678 feat: bar\n"
	fakeDiff := "file.go | 2 +-\n1 file changed, 1 insertion(+), 1 deletion(-)\n"

	cases := []struct {
		lang      string
		wantHead  string
		wantCheck string
	}{
		{"en", "## What changed", "- [ ] Self-tested"},
		{"zh", "## 改了什么", "- [ ] 自测通过"},
		{"mixed", "## 改了什么", "- [ ] 自测通过"},
		{"", "## 改了什么", "- [ ] 自测通过"},
	}

	// Substrings the user explicitly asked to NEVER appear in PR title or body
	// (req_9ead19cd39f632fd). Each case must contain zero of these.
	forbidden := []string{
		"由 NovaWorkbench auto-push 自动创建",
		"Auto-created by NovaWorkbench auto-push",
		"🤖 Generated with Claude Code",
		"Co-Authored-By: Claude",
		"co-authored-by",
		"## 关联 Issue",
		"## 怎么验证",
		"## 检查清单",
	}

	for _, c := range cases {
		tmpl := service.NewPRShellTemplates(c.lang)
		got := buildPRShellBody(tmpl, fakeLog, fakeDiff)

		// 必要 section + log + diff 都在
		if !strings.Contains(got, c.wantHead) {
			t.Errorf("[%s] section heading %q missing in body:\n%s", c.lang, c.wantHead, got)
		}
		if !strings.Contains(got, c.wantCheck) {
			t.Errorf("[%s] checklist %q missing in body:\n%s", c.lang, c.wantCheck, got)
		}
		if !strings.Contains(got, fakeLog) {
			t.Errorf("[%s] log content missing in body:\n%s", c.lang, got)
		}
		if !strings.Contains(got, fakeDiff) {
			t.Errorf("[%s] diff content missing in body:\n%s", c.lang, got)
		}

		// 禁词全不在
		for _, bad := range forbidden {
			if strings.Contains(got, bad) {
				t.Errorf("[%s] forbidden substring %q present in body:\n%s", c.lang, bad, got)
			}
		}

		// 5-section 模板里剩余 3 个 section（关联 Issue / 怎么验证 / 检查清单）
		// shell 路径不输出（无上下文），确认它们的检查项字符串不在 body 里。
		// 注意：## 关联 Issue 已经在 forbidden 里；同时确认 zh 版 checklist 不在 en
		// body 里、en 版 checklist 不在 zh body 里。
		if c.lang == "en" {
			if strings.Contains(got, "## 改了什么") || strings.Contains(got, "## 备注") {
				t.Errorf("[en] 中文 section heading should not appear:\n%s", got)
			}
			if strings.Contains(got, "自测通过") || strings.Contains(got, "无调试代码") {
				t.Errorf("[en] 中文 checklist should not appear:\n%s", got)
			}
		} else {
			if strings.Contains(got, "## What changed") || strings.Contains(got, "## Notes") {
				t.Errorf("[%s] English section heading should not appear:\n%s", c.lang, got)
			}
			if strings.Contains(got, "Self-tested") || strings.Contains(got, "No debug code") {
				t.Errorf("[%s] English checklist should not appear:\n%s", c.lang, got)
			}
		}
	}
}

// TestResolvePRTitle pins the title priority used by createPRShell. The
// regression it guards: req_3e7f5c152254672f had prTitle == "" and the
// English-style project got a Chinese PR title because reqRow.Title was
// used before the (already commit_lang-compliant) commit subject.
func TestResolvePRTitle(t *testing.T) {
	en := service.NewPRShellTemplates("en")

	cases := []struct {
		name          string
		prTitle       string
		commitMessage string
		reqTitle      string
		want          string
	}{
		{
			name:          "llm PR title wins",
			prTitle:       "Lead the stats homepage with an all-time attack total",
			commitMessage: "feat: add attack total",
			reqTitle:      "首页显示攻击总数",
			want:          "Lead the stats homepage with an all-time attack total",
		},
		{
			name:          "empty prTitle falls back to commit subject",
			commitMessage: "xdplog-report: add all-time attack total",
			reqTitle:      "XDP 数据统计后首页显示的内容",
			want:          "xdplog-report: add all-time attack total",
		},
		{
			name:          "conventional prefix stripped from commit subject",
			commitMessage: "feat(stats): add all-time attack total",
			reqTitle:      "首页显示攻击总数",
			want:          "add all-time attack total",
		},
		{
			name:     "requirement title used only when nothing else exists",
			reqTitle: "首页显示攻击总数",
			want:     "首页显示攻击总数",
		},
		{
			name: "localized template fallback is last",
			want: "NovaWorkbench auto-push: feat/req_abc",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolvePRTitle(c.prTitle, c.commitMessage, c.reqTitle, "feat/req_abc", en)
			if got != c.want {
				t.Errorf("resolvePRTitle() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStripConventionalCommitPrefix(t *testing.T) {
	cases := map[string]string{
		"feat: add stats":              "add stats",
		"fix(scope): repair":           "repair",
		"chore!: drop node 18":         "drop node 18",
		"refactor(a/b)!: split parser": "split parser",
		"feat(stats): add total":       "add total",
		"stats: add total":             "stats: add total", // not a known type
		"Phase: two":                   "Phase: two",       // free-form title survives
		"no colon here":                "no colon here",
		"feat:":                        "feat:",
		"feat(scope: malformed":        "feat(scope: malformed",
	}
	for in, want := range cases {
		if got := stripConventionalCommitPrefix(in); got != want {
			t.Errorf("stripConventionalCommitPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
