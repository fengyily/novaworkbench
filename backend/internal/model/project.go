package model

import "time"

type Project struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	LocalPath       string     `json:"local_path"`
	RemoteURL       string     `json:"remote_url"`
	Status          string     `json:"status"`
	DefaultBranch   string     `json:"default_branch"`
	ProjectType     string     `json:"project_type"`
	ClaudeFiles     string     `json:"claude_files"`
	PlatformType    string     `json:"platform_type"`
	PlatformTokenID string     `json:"platform_token_id"`
	AddedAt         time.Time  `json:"added_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LastScannedAt   *time.Time `json:"last_scanned_at,omitempty"`
	DeletedAt       *string    `json:"deleted_at,omitempty"`
	DeletedDir      int        `json:"deleted_dir"`
}

type AddProjectRequest struct {
	LocalPath string `json:"local_path"`
	RemoteURL string `json:"remote_url"`
	InitGit   bool   `json:"init_git"`
}

type DashboardData struct {
	TotalProjects   int            `json:"total_projects"`
	ActiveReqs      int            `json:"active_requirements"`
	PendingReviews  int            `json:"pending_reviews"`
	WeeklyCommits   int            `json:"weekly_commits"`
	Projects        []Project      `json:"projects"`
	RecentActivity  []ActivityItem `json:"recent_activity"`
}

type ActivityItem struct {
	ProjectName string `json:"project_name"`
	ProjectID   string `json:"project_id"`
	Action      string `json:"action"`
	Detail      string `json:"detail"`
	Timestamp   string `json:"timestamp"`
}
