package service

import (
	"strings"
	"testing"
)

// TestSanitizeMermaidBlocks pins the three known-broken patterns from
// req_b37e6f385d371b7e and a handful of safe controls. Each broken case
// uses the actual offending text from the live wiki_docs; each safe
// case pins the no-op behavior so future regressions don't accidentally
// rewrite good input.
//
// Test cases mirror what /tmp/mermaid_fix.mjs (a small Node script
// importing the project's bundled mermaid 11.13.0) round-trips through
// mermaid.parse(): the sanitizer is correct iff mermaid accepts the
// result.
func TestSanitizeMermaidBlocks(t *testing.T) {
	// Real offending fragment from block 6 of req_b37e6f385d371b7e.
	const block6Line = "  Walk[walk map/slice<br/>phiFields + nameFields → \"[redacted]\"<br/>neverRedacted 跳过]"
	// Real offending fragment from block 2.
	const block2Line = "      McpEp[/mcp<br/>mcp.Server]"
	// Real offending fragment from block 3 (sequenceDiagram with ';').
	const block3Line = "    ApiMcp->>ApiMcp: uc.ApproveAuthorization: scope = 用户勾选 ∩ 客户端请求 ∩ 租户允许; sites = 请求 ∩ 用户角色"

	cases := []struct {
		name  string
		input string
		want  string // exact expected output; "" means "input unchanged"
	}{
		{
			name: "block-2-parallelogram-with-slash becomes quoted square",
			input: "```mermaid\nflowchart TD\n" + block2Line + "\n```\n",
			want: "```mermaid\nflowchart TD\n      McpEp[/mcp<br/>mcp.Server/]\n```\n",
		},
		{
			name: "block-3-semicolon-in-message becomes full-width semicolon",
			input: "```mermaid\nsequenceDiagram\n  A->>B: foo\n" + block3Line + "\n```\n",
			want: "```mermaid\nsequenceDiagram\n  A->>B: foo\n    ApiMcp->>ApiMcp: uc.ApproveAuthorization: scope = 用户勾选 ∩ 客户端请求 ∩ 租户允许； sites = 请求 ∩ 用户角色\n```\n",
		},
		{
			name: "block-6-quoted-bracket-in-square-label wraps in outer quotes",
			input: "```mermaid\nflowchart TD\n" + block6Line + "\n```\n",
			want: "```mermaid\nflowchart TD\n  Walk[\"walk map/slice<br/>phiFields + nameFields → [redacted]<br/>neverRedacted 跳过\"]\n```\n",
		},
		{
			name:  "harmless parallelogram is left alone",
			input: "```mermaid\nflowchart TD\n  A[/foo<br/>bar/]\n```\n",
			want:  "```mermaid\nflowchart TD\n  A[/foo<br/>bar/]\n```\n",
		},
		{
			name:  "harmless square label is left alone",
			input: "```mermaid\nflowchart TD\n  A[Walk → redacted]\n```\n",
			want:  "```mermaid\nflowchart TD\n  A[Walk → redacted]\n```\n",
		},
		{
			name:  "square label with parens gets wrapped in quotes",
			input: "```mermaid\nflowchart TD\n  X[renderKeep(n)<br/>binary search]\n```\n",
			want:  "```mermaid\nflowchart TD\n  X[\"renderKeep(n)<br/>binary search\"]\n```\n",
		},
		{
			name:  "square label with curly braces gets wrapped in quotes",
			input: "```mermaid\nflowchart TD\n  X[a{b}c]\n```\n",
			want:  "```mermaid\nflowchart TD\n  X[\"a{b}c\"]\n```\n",
		},
		{
			name:  "harmless sequenceDiagram is left alone",
			input: "```mermaid\nsequenceDiagram\n  A->>B: hello world\n  C->>D: foo, bar\n```\n",
			want:  "```mermaid\nsequenceDiagram\n  A->>B: hello world\n  C->>D: foo, bar\n```\n",
		},
		{
			name:  "non-mermaid code blocks are left alone",
			input: "```go\nfunc foo() { ; /* ; */ }\n```\n",
			want:  "```go\nfunc foo() { ; /* ; */ }\n```\n",
		},
		{
			name:  "stateDiagram-v2 semicolon in transition text becomes full-width",
			input: "```mermaid\nstateDiagram-v2\n  Active --> Active: foo; bar\n```\n",
			want: "```mermaid\nstateDiagram-v2\n  Active --> Active: foo； bar\n```\n",
		},
		{
			name:  "no mermaid block at all returns input unchanged",
			input: "# heading\n\nplain prose\n",
			want:  "# heading\n\nplain prose\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizeMermaidBlocks(c.input)
			if got != c.want {
				t.Errorf("SanitizeMermaidBlocks mismatch\n--- input ---\n%s\n--- got ---\n%s\n--- want ---\n%s", c.input, got, c.want)
			}
		})
	}
}

// TestSanitizeMermaidBlocks_PreservesUnknownDiagramTypes ensures we don't
// accidentally rewrite diagrams whose syntax we haven't validated (pie,
// gantt, er, class, journey). These either never appeared in production
// or have a separate lexer; rewriting them is a regression risk.
func TestSanitizeMermaidBlocks_PreservesUnknownDiagramTypes(t *testing.T) {
	cases := []string{
		"```mermaid\nerDiagram\n  CUSTOMER ||--o{ ORDER : places\n```\n",
		"```mermaid\npie title Pets\n  \"Dogs\" : 386\n  \"Cats\" : 85\n```\n",
		"```mermaid\nclassDiagram\n  class Foo\n```\n",
		"```mermaid\ngantt\n  title A\n  section A\n  Task :a1, 2026-01-01, 1d\n```\n",
	}
	for _, in := range cases {
		t.Run(in[:strings.Index(in, "\n")], func(t *testing.T) {
			if got := SanitizeMermaidBlocks(in); got != in {
				t.Errorf("unexpected rewrite\n--- input ---\n%s\n--- got ---\n%s", in, got)
			}
		})
	}
}