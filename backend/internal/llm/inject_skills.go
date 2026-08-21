package llm

import "strings"

// BuildSkillsBlock renders the skill content as a prompt section that Claude
// will read as direct instructions. This is more reliable than writing agent
// files and hoping Claude invokes them — the instructions land directly in
// context and are applied without any tool call.
func BuildSkillsBlock(skills []struct{ Slug, Content string }) string {
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
