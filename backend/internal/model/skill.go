package model

import "time"

// Skill is a Claude Code skill file managed by NovaWorkbench. When a
// requirement's text @mentions a skill by slug, the wizard materializes it as a
// real SKILL.md under <worktree>/.claude/skills/<slug>/ (local worktree write,
// or SFTP for Agent-Server runs) and references it in the prompt via /slug,
// which Claude Code expands to load the full body on demand. Forked sub-task
// sessions in the same worktree inherit the file via CWD-based discovery.
type Skill struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Content     string    `json:"content"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	SourceURL   string    `json:"source_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type CreateSkillReq struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Content     string `json:"content"`
	Description string `json:"description"`
	SourceURL   string `json:"source_url"`
	// Enabled controls the initial enabled state. nil is treated as true so
	// existing market installs keep enabling the skill by default; command
	// integration passes false so the skill can be @mentioned but not auto-loaded.
	Enabled *bool `json:"enabled"`
}

type UpdateSkillReq struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Content     string `json:"content"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

// MarketSkill is a skill entry from a remote registry manifest.
type MarketSkill struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Content     string `json:"content"`
	SourceURL   string `json:"source_url"`
}

// SkillMarket is a curated skill market entry shown in the UI.
type SkillMarket struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RepoURL     string `json:"repo_url"`
}
