package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderSkillMD_FrontmatterFirst(t *testing.T) {
	sk := MentionedSkill{Slug: "go-conventions", Description: "Go style rules", Content: "Always run gofmt.\n"}
	got := string(RenderSkillMD(sk))
	if !strings.HasPrefix(got, "---\n") {
		t.Fatalf("SKILL.md must start with '---\\n' so frontmatter parses; got prefix %q", got[:min(16, len(got))])
	}
	if !strings.Contains(got, "name: go-conventions") {
		t.Errorf("missing name frontmatter: %s", got)
	}
	if !strings.Contains(got, "description: Go style rules") {
		t.Errorf("missing description frontmatter: %s", got)
	}
	if !strings.HasSuffix(got, "Always run gofmt.\n") {
		t.Errorf("body not preserved / not newline-terminated: %q", got)
	}
}

func TestRenderSkillMD_EmptyDescription(t *testing.T) {
	sk := MentionedSkill{Slug: "minimal", Content: "body"}
	got := string(RenderSkillMD(sk))
	if strings.Contains(got, "description:") {
		t.Errorf("empty description should omit the field: %s", got)
	}
}

func TestMaterializeSkillFiles_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	skills := []MentionedSkill{
		{Slug: "a", Description: "desc a", Content: "body a"},
		{Slug: "b", Description: "desc b", Content: "body b"},
	}
	if err := MaterializeSkillFiles(dir, skills); err != nil {
		t.Fatalf("MaterializeSkillFiles: %v", err)
	}
	for _, sk := range skills {
		path := filepath.Join(dir, ".claude", "skills", sk.Slug, "SKILL.md")
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("skill %s not written: %v", sk.Slug, err)
		}
		if !strings.HasPrefix(string(got), "---\n") {
			t.Errorf("skill %s: frontmatter not first", sk.Slug)
		}
		if !strings.Contains(string(got), sk.Content) {
			t.Errorf("skill %s: body missing", sk.Slug)
		}
	}
}

func TestMaterializeSkillFiles_IdempotentOverwrite(t *testing.T) {
	dir := t.TempDir()
	sk := MentionedSkill{Slug: "x", Description: "v1", Content: "v1 body"}
	_ = MaterializeSkillFiles(dir, []MentionedSkill{sk})
	sk.Description = "v2"
	sk.Content = "v2 body"
	if err := MaterializeSkillFiles(dir, []MentionedSkill{sk}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, ".claude", "skills", "x", "SKILL.md"))
	if !strings.Contains(string(got), "v2 body") || !strings.Contains(string(got), "v2") {
		t.Errorf("overwrite did not update content: %s", got)
	}
}

func TestMaterializeSkillFiles_EmptyInputs(t *testing.T) {
	if err := MaterializeSkillFiles("", []MentionedSkill{{Slug: "x", Content: "y"}}); err != nil {
		t.Errorf("empty workDir should be a no-op, got %v", err)
	}
	if err := MaterializeSkillFiles(t.TempDir(), nil); err != nil {
		t.Errorf("nil skills should be a no-op, got %v", err)
	}
}

func TestTranslateAtToSlash(t *testing.T) {
	cases := []struct {
		name string
		text string
		slugs []string
		want string
	}{
		{"known slug", "use @go-conventions here", []string{"go-conventions"}, "use /go-conventions here"},
		{"unknown preserved", "see @filename and @go-conventions", []string{"go-conventions"}, "see @filename and /go-conventions"},
		{"no slugs no change", "@a @b", nil, "@a @b"},
		{"empty text", "", []string{"x"}, ""},
		{"boundary slug", "@x@y", []string{"x", "y"}, "/x/y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TranslateAtToSlash(c.text, c.slugs); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestSkillRefBlock(t *testing.T) {
	if got := SkillRefBlock(nil); got != "" {
		t.Errorf("nil should give empty block, got %q", got)
	}
	got := SkillRefBlock([]MentionedSkill{{Slug: "s", Description: "d"}})
	if !strings.Contains(got, "/s") || !strings.Contains(got, "d") {
		t.Errorf("block missing /slug or description: %s", got)
	}
}

func TestDecomposeSkillPropagation(t *testing.T) {
	if got := DecomposeSkillPropagation(nil); got != "" {
		t.Errorf("nil should give empty, got %q", got)
	}
	got := DecomposeSkillPropagation([]MentionedSkill{{Slug: "s", Description: "d"}})
	if !strings.Contains(got, "/slug") {
		t.Errorf("propagation should mention /slug: %s", got)
	}
}

func TestApplyMentionedSkills(t *testing.T) {
	dir := t.TempDir()
	in := "do work @go-conventions now"
	got := ApplyMentionedSkills(in, dir, []MentionedSkill{{Slug: "go-conventions", Description: "rules", Content: "body"}})
	if !strings.Contains(got, "/go-conventions") {
		t.Errorf("@slug not translated to /slug: %s", got)
	}
	if strings.Contains(got, "@go-conventions") {
		t.Errorf("@slug should be gone after translate: %s", got)
	}
	// file materialized
	if _, err := os.Stat(filepath.Join(dir, ".claude", "skills", "go-conventions", "SKILL.md")); err != nil {
		t.Errorf("skill file not materialized: %v", err)
	}
	// no skills -> unchanged
	if got := ApplyMentionedSkills("plain", dir, nil); got != "plain" {
		t.Errorf("no skills should leave prompt unchanged, got %q", got)
	}
}
