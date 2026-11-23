package prompt

import (
	"strings"
	"testing"
)

// 跨项目作业的「项目约定」section：每个参与仓库一节、标题带仓库名。
func TestBuildCrossProjectConventionSection(t *testing.T) {
	section := BuildCrossProjectConventionSection([]CrossProjectDoc{
		{ProjectName: "web-app", Content: "前端约定：组件必须写单测。"},
		{ProjectName: "api-svc", Content: "后端约定：接口必须有错误码。", Managed: true, DiskExists: true},
	})
	if section == "" {
		t.Fatal("section is empty")
	}
	if !strings.Contains(section, "### web-app") || !strings.Contains(section, "### api-svc") {
		t.Fatalf("section does not label the repositories:\n%s", section)
	}
	if !strings.Contains(section, "前端约定：组件必须写单测。") {
		t.Fatalf("section lost the first convention:\n%s", section)
	}
	if !strings.Contains(section, "后端约定：接口必须有错误码。") {
		t.Fatalf("section lost the second convention:\n%s", section)
	}
	// 受管副本要说清它是权威版本，并消解与仓库自带副本的冲突。
	if !strings.Contains(section, "以本块为准") {
		t.Fatalf("section does not disambiguate the managed copy:\n%s", section)
	}
}

// 没有正文就不出节 —— 一个空标题会让模型以为漏了一份约定。
func TestBuildCrossProjectConventionSection_Empty(t *testing.T) {
	if got := BuildCrossProjectConventionSection(nil); got != "" {
		t.Fatalf("section = %q, want empty", got)
	}
	if got := BuildCrossProjectConventionSection([]CrossProjectDoc{{ProjectName: "web-app"}}); got != "" {
		t.Fatalf("section = %q, want empty (the repository has no convention)", got)
	}
}

// 过长的约定按 rune 截断 —— 字节截断会把一个中文字切成半个字。
func TestTruncateCrossClaudeDoc(t *testing.T) {
	long := strings.Repeat("约定内容", 5000)
	got := truncateCrossClaudeDoc(long)
	if len(got) >= len(long) {
		t.Fatalf("truncation did not shorten the document (%d → %d)", len(long), len(got))
	}
	if !strings.Contains(got, crossClaudeDocTruncMarker) {
		t.Fatalf("truncated document has no marker: %q", got[len(got)-40:])
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(got, crossClaudeDocTruncMarker)) {
		t.Fatal("truncation did not keep the head of the document")
	}
}
