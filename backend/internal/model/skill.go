package model

import "time"

// Skill is a Claude Code skill file (agents/<slug>.md) managed by NovaWorkbench.
// Before each claude CLI invocation the enabled skills are written into the
// project's .claude/agents/ directory so they are available regardless of the
// active --setting-sources value.
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
