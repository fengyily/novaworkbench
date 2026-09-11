package service

import (
	"strings"
	"testing"
)

// TestSanitizeDesignDoc exercises the pure sanitizer that strips a single
// outer ```markdown``` (or any-lang) code fence from a Claude-generated
// design_doc payload before it is persisted into requirements.design_docs.
//
// The frontend's parseDesign runs the same regex as a defense in depth, so
// any change here must be reflected there too.

func TestSanitizeDesignDoc(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty unchanged", "", ""},
		{"plain markdown unchanged", "# 标题\n## 子节\n- 列表项", "# 标题\n## 子节\n- 列表项"},
		{"legacy JSON object unchanged",
			`{"overview":"概览","files":["a.go"],"steps":["b"],"model_changes":"无","risks":[]}`,
			`{"overview":"概览","files":["a.go"],"steps":["b"],"model_changes":"无","risks":[]}`,
		},
		{"legacy JSON array unchanged",
			`["# 方案\n## 详情"]`,
			`["# 方案\n## 详情"]`,
		},
		{"fenced markdown stripped",
			"```markdown\n# 真方案\n## 详情\n- a\n- b\n```",
			"# 真方案\n## 详情\n- a\n- b\n",
		},
		{"fenced no lang stripped",
			"```\n# 真方案\n## 详情\n```",
			"# 真方案\n## 详情\n",
		},
		{"double-wrapped (3-fence over 3-fence) stripped to innermost body",
			"```markdown\n```markdown\n# 内层\n```\n```",
			"# 内层\n",
		},
		{"inner code blocks preserved",
			"```markdown\n# 标题\n例子：\n```js\nconsole.log(1)\n```\n继续\n```",
			"# 标题\n例子：\n```js\nconsole.log(1)\n```\n继续\n",
		},
		{"leading preamble before fence — fence stripped, preamble dropped",
			"Here's the updated plan:\n```markdown\n# 真方案\n## 详情\n```",
			"# 真方案\n## 详情\n",
		},
		{"plain markdown plus leading preamble — preserved unchanged",
			"Here's the updated plan:\n# 真方案\n## 详情",
			"Here's the updated plan:\n# 真方案\n## 详情",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeDesignDoc(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeDesignDoc mismatch\n  got:  %q\n  want: %q", got, tc.want)
			}
		})
	}
}

// TestSanitizeDesignDoc_RealReq8801102ab514293d replays the exact bytes
// fetched from the live postgres DB for requirement req_8801102ab514293d
// (the bug report). The stored value is wrapped in an outer ```markdown
// fence. After sanitisation the result must start with the "# 修复：..."
// heading (not a code fence) so ReactMarkdown can render headings/lists.
//
// To regenerate the embedded snapshot, dump the row with:
//   PGPASSWORD=nova psql -h 127.0.0.1 -p 5434 -U postgres -d novaworkbench \
//     -Atc "SELECT design_docs FROM requirements WHERE id='req_8801102ab514293d';"
const realBuggySample = "" +
	"```markdown\n" +
	"# 修复：Agent Server 远端 `git commit` 触发 GPG 签名失败（non-interactive）\n" +
	"\n" +
	"## Context\n" +
	"\n" +
	"问题现象通过 Agent Server 远端执行 Claude 开发任务时……\n" +
	"（截断 — 完整内容 ~20 KB 在生产库）\n" +
	"```\n"

func TestSanitizeDesignDoc_RealReq8801102ab514293d(t *testing.T) {
	got := sanitizeDesignDoc(realBuggySample)
	if strings.HasPrefix(got, "```") {
		t.Fatalf("outer fence not stripped, output starts with %q", got[:min(20, len(got))])
	}
	if !strings.HasPrefix(got, "# 修复") {
		t.Fatalf("expected sanitised output to start with '# 修复', got %q", got[:min(40, len(got))])
	}
	if strings.HasSuffix(strings.TrimRight(got, "\n"), "```") {
		t.Fatalf("outer fence not stripped, output ends with ```")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}