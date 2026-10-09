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
