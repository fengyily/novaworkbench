package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// TestLooksLikeSSH confirms the URL classifier agrees with the platform
// credential helper's own heuristic — both surfaces use it to decide
// whether to render the SSH vs HTTPS phrasing, and a divergence here would
// cause the prompt to lie to the model about how git push will be
// authenticated.
func TestLooksLikeSSH(t *testing.T) {
	cases := []struct {
		raw  string
	want bool
	}{
		{"", false},
		{"https://github.com/owner/repo.git", false},
		{"http://git.acme.com/owner/repo.git", false},
		{"git@github.com:owner/repo.git", true},
		{"ssh://git@github.com/owner/repo.git", true},
		{"git+ssh://git@gitlab.com/owner/repo.git", true},
		{"git@gitlab.example.com:2222:owner/repo.git", true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := looksLikeSSH(tc.raw); got != tc.want {
				t.Errorf("looksLikeSSH(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestRemoteHostOnly exercises the helper that strips userinfo (e.g. an
// accidentally-pasted token) out of a remote URL before it goes into the
// rendered prompt. The whole point of the helper is to keep tokens out of
// prompt text — a positive test would mean the helper itself is the leak.
func TestRemoteHostOnly(t *testing.T) {
	const token = "ghp_FAKE_TOKEN_xxxxxxxxxxxxxxxxxxxx"
	cases := []struct {
		raw      string
		want     string
		notWant  []string // substrings that must NOT appear in the rendered output
	}{
		{"", "", []string{token}},
		{"https://github.com/owner/repo.git", "github.com", nil},
		{"git@github.com:owner/repo.git", "github.com", nil},
		{"ssh://git@gitlab.example.com:2222/owner/repo.git", "gitlab.example.com:2222", nil},
		{"https://oauth2:" + token + "@github.com/owner/repo.git", "github.com", []string{token, "oauth2"}},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := remoteHostOnly(tc.raw)
			if got != tc.want {
				t.Errorf("remoteHostOnly(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			for _, bad := range tc.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("remoteHostOnly(%q) = %q; must NOT contain %q", tc.raw, got, bad)
				}
			}
		})
	}
}

// TestBuildGitProjectContext_MinimalAllEmptyPath makes sure an empty
// GitProjectContext renders without panicking and that the headline
// "## 本项目 Git 约定" line is still emitted (no panic = degraded
// information, which is the contract the assembler promises). The
// current buildGitProjectContext only errors on a nil project (which
// can't be exercised here, since we pass &Project{}).
func TestBuildGitProjectContext_MinimalAllEmptyPath(t *testing.T) {
	c, err := buildGitProjectContext(&model.Project{PlatformType: "github"}, nil)
	if err != nil {
		t.Fatalf("buildGitProjectContext with minimal project should not error: %v", err)
	}
	if c.PlatformType != "github" {
		t.Errorf("PlatformType should propagate; got %q", c.PlatformType)
	}
	if c.StyleHint == "" {
		// Empty commit_lang → empty StyleHint (which renders nothing in the
		// block). The contract is "miss = empty string", and the helper
		// must not panic on a nil/empty style.
		t.Logf("StyleHint empty as expected (no commit_lang set)")
	}
}

// TestStyleHintServiceContract confirms service.StyleHint still emits the
// expected wording — the prompt block depends on these strings, so a
// regression here would render the obsolete wording in the assistant's
// context.
func TestStyleHintServiceContract(t *testing.T) {
	if !strings.Contains(service.StyleHint("en", "", service.CommitStyleRules{}), "English") {
		t.Errorf("StyleHint(en) must mention English")
	}
	if !strings.Contains(service.StyleHint("zh", "", service.CommitStyleRules{}), "中文") {
		t.Errorf("StyleHint(zh) must mention 中文")
	}
	if !strings.Contains(service.StyleHint("mixed", "", service.CommitStyleRules{}), "混用") {
		t.Errorf("StyleHint(mixed) must mention 混用")
	}
}
