package service

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDetectCommitLanguage_Smoke creates temp git repos with known content and
// verifies DetectCommitLanguage reports the expected (lang, confidence).
func TestDetectCommitLanguage_Smoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	mkRepo := func(t *testing.T, subjects []string) string {
		t.Helper()
		dir := t.TempDir()
		mustRun(t, dir, "init", "-q")
		mustRun(t, dir, "config", "user.email", "test@example.com")
		mustRun(t, dir, "config", "user.name", "Test")
		mustRun(t, dir, "config", "commit.gpgsign", "false")
		if _, err := exec.LookPath("git"); err != nil {
			t.Fatal(err)
		}
		for _, s := range subjects {
			mustRun(t, dir, "commit", "--allow-empty", "-q", "-m", s)
		}
		return dir
	}

	t.Run("empty_repo", func(t *testing.T) {
		dir := mkRepo(t, nil)
		lang, conf, err := DetectCommitLanguage(dir)
		if err != nil || lang != "" || conf != 0 {
			t.Fatalf("want (\"\", 0, nil), got (%q, %v, %v)", lang, conf, err)
		}
	})

	t.Run("all_chinese", func(t *testing.T) {
		dir := mkRepo(t, []string{
			"修复登录校验逻辑",
			"新增需求：支持 OAuth 登录",
			"重构：抽离用户服务",
			"修复 issue #123 关于中文 commit 解析",
		})
		lang, conf, err := DetectCommitLanguage(dir)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if lang != "zh" || conf != 1.0 {
			t.Fatalf("want (\"zh\", 1.0), got (%q, %v)", lang, conf)
		}
	})

	t.Run("all_english", func(t *testing.T) {
		dir := mkRepo(t, []string{
			"fix: login validation",
			"feat: add OAuth support",
			"refactor: extract user service",
			"docs: update README",
		})
		lang, conf, err := DetectCommitLanguage(dir)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if lang != "en" || conf != 1.0 {
			t.Fatalf("want (\"en\", 1.0), got (%q, %v)", lang, conf)
		}
	})

	t.Run("majority_zh", func(t *testing.T) {
		dir := mkRepo(t, []string{
			"修复 bug",
			"新增 feature",
			"重构模块",
			"chore: update deps", // 1 english
		})
		lang, conf, err := DetectCommitLanguage(dir)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if lang != "zh" {
			t.Fatalf("want \"zh\", got %q", lang)
		}
		if conf < 0.74 || conf > 0.76 {
			t.Fatalf("confidence out of range, got %v", conf)
		}
	})

	t.Run("majority_en", func(t *testing.T) {
		dir := mkRepo(t, []string{
			"fix: bug",
			"feat: thing",
			"docs: doc",
			"修复小问题", // 1 chinese
		})
		lang, conf, err := DetectCommitLanguage(dir)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if lang != "en" {
			t.Fatalf("want \"en\", got %q", lang)
		}
		if conf < 0.74 || conf > 0.76 {
			t.Fatalf("confidence out of range, got %v", conf)
		}
	})

	t.Run("not_a_git_repo", func(t *testing.T) {
		dir := t.TempDir()
		lang, conf, err := DetectCommitLanguage(dir)
		if err != nil || lang != "" || conf != 0 {
			t.Fatalf("want (\"\", 0, nil), got (%q, %v, %v)", lang, conf, err)
		}
	})
}

func TestStyleHint(t *testing.T) {
	cases := []struct {
		name             string
		lang, override   string
		wantContains     string
		wantEmpty        bool
	}{
		{"zh_default", "zh", "", "中文", false},
		{"en_default", "en", "", "English", false},
		{"override_beats_stored", "zh", "en", "English", false},
		{"override_with_empty_stored", "", "zh", "中文", false},
		{"both_empty", "", "", "", true},
		{"mixed", "mixed", "", "中英混用", false},
		{"unknown_lang", "klingon", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StyleHint(c.lang, c.override)
			if c.wantEmpty {
				if got != "" {
					t.Fatalf("want empty, got %q", got)
				}
				return
			}
			if !strings.Contains(got, c.wantContains) {
				t.Fatalf("want contains %q, got %q", c.wantContains, got)
			}
			if !strings.HasPrefix(got, "📝") {
				t.Fatalf("want emoji prefix, got %q", got)
			}
		})
	}
}

func TestResolveCommitLang(t *testing.T) {
	cases := []struct {
		stored, override, want string
	}{
		{"", "", ""},
		{"zh", "", "zh"},
		{"", "en", "en"},
		{"zh", "en", "en"},
		{"en", "zh", "zh"},
		{"mixed", "", "mixed"},
		{"", "mixed", "mixed"},
	}
	for _, c := range cases {
		got := ResolveCommitLang(c.stored, c.override)
		if got != c.want {
			t.Errorf("ResolveCommitLang(%q, %q) = %q, want %q", c.stored, c.override, got, c.want)
		}
	}
}

// TestPRLangRules asserts the four language branches of PRLangRules:
//   - en    → English title + Summary/Changes/Key Files/How Verified block
//   - mixed → 中英混用, with both Chinese keywords
//   - zh    → 改动概述 / 主要变更 / 关键文件 / 验证方式 中文块
//   - ""    → identical to zh (zero regression for unresolved style)
func TestPRLangRules(t *testing.T) {
	en := PRLangRules("en")
	if !strings.Contains(en, "English") {
		t.Fatalf("en block must contain English, got: %q", en)
	}
	for _, kw := range []string{"Summary", "Changes", "Key Files", "How Verified"} {
		if !strings.Contains(en, kw) {
			t.Fatalf("en block must contain %q, got: %q", kw, en)
		}
	}
	if strings.Contains(en, "改动概述") {
		t.Fatalf("en block must not contain 中文 schema 改动概述, got: %q", en)
	}

	mixed := PRLangRules("mixed")
	if !strings.Contains(mixed, "中英混用") {
		t.Fatalf("mixed block must contain 中英混用, got: %q", mixed)
	}
	if !strings.Contains(mixed, "改动概述") {
		t.Fatalf("mixed block must keep 中文 schema 改动概述, got: %q", mixed)
	}

	zh := PRLangRules("zh")
	if !strings.Contains(zh, "中文") || !strings.Contains(zh, "改动概述") {
		t.Fatalf("zh block must contain 中文/改动概述, got: %q", zh)
	}
	if !strings.Contains(zh, "PR 摘要要求") {
		t.Fatalf("zh block must keep legacy heading PR 摘要要求 (zero regression), got: %q", zh)
	}

	empty := PRLangRules("")
	if empty != zh {
		t.Fatalf("empty block must equal zh block (zero regression), got: %q vs %q", empty, zh)
	}
}

// mustRun is a helper that runs a git command in dir and fails the test on
// error, surfacing stderr for easier debugging.
func mustRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestDetectTextLanguage covers the single-string language detector used by
// the shell auto-push path to decide whether to LLM-regenerate commit / PR
// titles. The detector must distinguish zh / en / mixed and treat pure
// digits/whitespace as empty. Expected values are derived from the actual
// algorithm (CJK vs ASCII alpha count with a 1.5x dominance threshold):
//
//   - 15 ASCII + 4 Han ("Fix login bug with 用户认证") -> "en" (15 > 4*1.5)
//   - 4 ASCII + 4 Han ("feat: 新增功能") -> "mixed" (neither side > 1.5x the other)
func TestDetectTextLanguage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello world", "en"},
		{"你好世界", "zh"},
		{"Fix login bug with 用户认证", "en"},
		{"", ""},
		{"1234567890", ""},
		{"feat: 新增功能", "mixed"},
	}
	for _, c := range cases {
		if got := DetectTextLanguage(c.in); got != c.want {
			t.Errorf("DetectTextLanguage(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestShouldRegenerateForLang covers the conflict rule used by
// execPushPRShell to decide whether to invoke the LLM fallback. The matrix
// verifies both the obvious mismatch (en project + zh title, zh project +
// en title) and the no-op cases (mixed / empty / matching language).
func TestShouldRegenerateForLang(t *testing.T) {
	cases := []struct {
		commitLang, title string
		want              bool
	}{
		{"en", "你好世界", true},
		{"zh", "hello world", true},
		{"en", "hello world", false},
		{"zh", "你好世界", false},
		{"mixed", "hello world", false},
		{"mixed", "你好世界", false},
		{"", "hello world", false},
		{"en", "", false},
		{"en", "Fix login", false},
	}
	for _, c := range cases {
		if got := ShouldRegenerateForLang(c.commitLang, c.title); got != c.want {
			t.Errorf("ShouldRegenerateForLang(%q, %q) = %v, want %v", c.commitLang, c.title, got, c.want)
		}
	}
}

// TestPRShellTemplates guards the localized text used by the shell-path PR
// body. The "en" branch must produce English section headings + intro; the
// zero-regression branches ("" / "zh" / "mixed") must keep the existing
// hardcoded Chinese strings verbatim.
func TestPRShellTemplates(t *testing.T) {
	en := NewPRShellTemplates("en")
	if en.PRBodySummaryHeading != "## Summary" {
		t.Errorf("en.PRBodySummaryHeading = %q, want %q", en.PRBodySummaryHeading, "## Summary")
	}
	if en.PRBodyStatsHeading != "## Diff Stats" {
		t.Errorf("en.PRBodyStatsHeading = %q, want %q", en.PRBodyStatsHeading, "## Diff Stats")
	}
	if en.PRBodyIntroFmt != "Auto-created by NovaWorkbench auto-push (based on requirement %s)." {
		t.Errorf("en.PRBodyIntroFmt = %q, want %q", en.PRBodyIntroFmt,
			"Auto-created by NovaWorkbench auto-push (based on requirement %s).")
	}

	for _, lang := range []string{"", "zh", "mixed"} {
		got := NewPRShellTemplates(lang)
		if got.PRBodySummaryHeading != "## 改动概述" {
			t.Errorf("lang=%q PRBodySummaryHeading = %q, want 中文零回归 %q", lang, got.PRBodySummaryHeading, "## 改动概述")
		}
		if got.PRBodyIntroFmt != "由 NovaWorkbench auto-push 自动创建（基于需求 %s）。" {
			t.Errorf("lang=%q PRBodyIntroFmt = %q, want 中文零回归 %q", lang, got.PRBodyIntroFmt,
				"由 NovaWorkbench auto-push 自动创建（基于需求 %s）。")
		}
	}
}
