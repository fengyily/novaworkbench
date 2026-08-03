package model

import "time"

// WeeklyReport is one generated weekly report for a project. The Markdown
// content is produced by the claude CLI from git log + requirement data and
// persisted so the history survives restarts (jobs are in-memory only).
type WeeklyReport struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	PeriodStart string    `json:"period_start"` // YYYY-MM-DD
	PeriodEnd   string    `json:"period_end"`   // YYYY-MM-DD
	GitBranch   string    `json:"git_branch"`   // branch filter used; "" = all branches
	GitAuthor   string    `json:"git_author"`   // author filter used; "" = everyone
	Rule        string    `json:"rule"`         // rule template snapshot used for this run
	Content     string    `json:"content"`      // Markdown body
	Status      string    `json:"status"`       // done | error
	CreatedAt   time.Time `json:"created_at"`
}
