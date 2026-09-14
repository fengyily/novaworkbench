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
