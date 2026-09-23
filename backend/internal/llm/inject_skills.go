package llm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MentionedSkill carries the fields needed to materialize a Claude Code skill
// file and render a /slug reference in a prompt:
//   - Slug: the invocation name and the on-disk directory name (<dir>/SKILL.md)
//   - Description: a short summary used as SKILL.md frontmatter — Claude keeps
//     descriptions in context and uses them to decide when to auto-invoke
//   - Content: the full SKILL.md body, loaded into context only when /slug is
//     expanded (or Claude invokes the Skill tool)
type MentionedSkill struct {
	Slug        string
	Description string
	Content     string
}

// atMentionRe matches @<slug> tokens. A slug may contain letters, digits,
// hyphens, and underscores. Shared by TranslateAtToSlash and the handler-side
// parseAtMentions (which re-declares it for the wizard package — kept in sync).
var atMentionRe = regexp.MustCompile(`@([A-Za-z0-9_-]+)`)

// BuildSkillsBlock renders the skill content as a prompt section that Claude
// will read as direct instructions. This is the LEGACY full-content injection
// path, retained for the analyst stage (which runs before the coding worktree
// exists, so there is nowhere to materialize a SKILL.md file). Design / coding
// / sub-task stages use MaterializeSkillFiles + SkillRefBlock + TranslateAtToSlash
// instead — see inject_skills.go doc comment.
func BuildSkillsBlock(skills []MentionedSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 本次任务适用的 Skills\n\n")
	b.WriteString("以下 Skill 指导原则**必须**在本次任务中遵循和应用：\n\n")
	for _, sk := range skills {
		b.WriteString("### @")
		b.WriteString(sk.Slug)
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(sk.Content))
		b.WriteString("\n\n---\n\n")
	}
	return b.String()
}

// SkillRefBlock renders a LIGHTWEIGHT reference section listing the skills that
// have been materialized to the worktree, telling Claude to invoke them via
// /slug (which Claude Code expands before the model sees it, loading the full
// body on demand). Replaces the full-content dump of BuildSkillsBlock for
// design / coding / sub-task stages: only the descriptions sit in context until
// a skill is actually invoked.
func SkillRefBlock(skills []MentionedSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 本次任务适用的 Skills\n\n")
	b.WriteString("以下 Skill 已在本工作区就绪，请在执行相关工作时调用以获取指导（Claude 会展开 /slug 加载完整内容）：\n\n")
	for _, sk := range skills {
		b.WriteString("- /")
		b.WriteString(sk.Slug)
		if desc := strings.TrimSpace(sk.Description); desc != "" {
			b.WriteString("：")
			b.WriteString(desc)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// SlugsOf returns the slug list from a MentionedSkill slice, for passing to
// TranslateAtToSlash.
func SlugsOf(skills []MentionedSkill) []string {
	out := make([]string, 0, len(skills))
	for _, s := range skills {
		out = append(out, s.Slug)
	}
	return out
}

// ApplyMentionedSkills is the shared prompt-rewrite used by the design / coding
// / docs / sub-task stages. It materializes the skills to workDir as SKILL.md
// files, prepends a /slug reference block, and translates @<slug> tokens in the
// prompt to /<slug>. Best-effort: a materialize error is returned but callers
// log it rather than fail the run. Returns the updated prompt.
func ApplyMentionedSkills(prompt, workDir string, skills []MentionedSkill) string {
	if len(skills) == 0 {
		return prompt
	}
	if err := MaterializeSkillFiles(workDir, skills); err != nil {
		// Caller logs; keep going so the /slug references still work if the
		// files happen to already exist from a prior run.
		fmt.Printf("[skills] best-effort materialize failed: %v\n", err)
	}
	if rb := SkillRefBlock(skills); rb != "" {
		prompt = rb + prompt
	}
	return TranslateAtToSlash(prompt, SlugsOf(skills))
}

// DecomposeSkillPropagation returns an instruction appended to the task-split
// (decompose) prompt when @slug-mentioned skills exist. It tells the main
// agent to carry /slug references into each sub-task prompt it writes so child
// agents inherit the skill — the user's accepted "考验主任务理解意图" path.
// Empty when no skills are mentioned.
func DecomposeSkillPropagation(skills []MentionedSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## 子任务 skill 传播\n")
	b.WriteString("以上 skill 已在本工作区就绪。你在拆分出的每个子任务 prompt 中，对相关 skill 以 /slug 形式引用，使子 Agent 执行时能调用对应 skill 获取指导——不要把 skill 全文复制进子任务 prompt。\n")
	return b.String()
}

// TranslateAtToSlash rewrites @<slug> tokens that match a known skill slug into
// /<slug> invocations. Claude Code treats @path as a file reference (it does
// NOT load a skill) while /slug is the skill-invocation syntax that gets
// expanded before the model sees the prompt. Tokens that don't match a known
// slug are left untouched so non-skill @-mentions (e.g. @filename) survive.
func TranslateAtToSlash(text string, slugs []string) string {
	if len(slugs) == 0 || text == "" {
		return text
	}
	known := make(map[string]bool, len(slugs))
	for _, s := range slugs {
		known[s] = true
	}
	return atMentionRe.ReplaceAllStringFunc(text, func(tok string) string {
		sub := atMentionRe.FindStringSubmatch(tok)
		if len(sub) == 2 && known[sub[1]] {
			return "/" + sub[1]
		}
		return tok
	})
}

// MaterializeSkillFiles writes each mentioned skill as a real Claude Code
// project skill at <workDir>/.claude/skills/<slug>/SKILL.md so that Claude
// auto-discovers it (and forked sub-task sessions in the same worktree inherit
// it via CWD-based discovery). The file starts with YAML frontmatter (--- must
// be the first line or Claude Code won't parse it): name + description, then
// the full body. Idempotent — re-writing overwrites. Errors are returned but
// callers treat them as best-effort (a missing skill file degrades to "skill
// not invoked" rather than failing the run).
func MaterializeSkillFiles(workDir string, skills []MentionedSkill) error {
	if workDir == "" || len(skills) == 0 {
		return nil
	}
	root := filepath.Join(workDir, ".claude", "skills")
	var firstErr error
	for _, sk := range skills {
		slug := strings.TrimSpace(sk.Slug)
		if slug == "" {
			continue
		}
		dir := filepath.Join(root, slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("mkdir %s: %w", dir, err)
			}
			continue
		}
		path := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(path, RenderSkillMD(sk), 0o644); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("write %s: %w", path, err)
			}
		}
	}
	return firstErr
}

// RenderSkillMD builds the SKILL.md file body for one skill: YAML frontmatter
// (name + description) followed by the full content. The leading "---" must be
// the first byte or Claude Code won't parse the frontmatter. Shared by the
// local (MaterializeSkillFiles) and remote (SFTP) materializers.
func RenderSkillMD(sk MentionedSkill) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: ")
	b.WriteString(sanitizeYAMLScalar(sk.Slug))
	b.WriteString("\n")
	if desc := strings.TrimSpace(sk.Description); desc != "" {
		b.WriteString("description: ")
		b.WriteString(sanitizeYAMLScalar(desc))
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(sk.Content))
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// sanitizeYAMLScalar quotes a frontmatter value if it contains characters that
// would break plain-scalar parsing (colon, hash, leading/trailing space, quotes,
// newlines). Keeps the common case (simple slug / short description) unquoted
// for readability.
func sanitizeYAMLScalar(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#\n\"'") || s != strings.TrimSpace(s) {
		// Double-quote, escaping inner double-quotes.
		return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
	}
	return s
}
