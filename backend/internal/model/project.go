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
	Description      string     `json:"description"`
	DescriptionManual bool      `json:"description_manual"`
	DescriptionHash  string     `json:"-"`
}

// AddProjectRequest is the body of POST /api/projects.
//
// In remote mode (RemoteURL set, LocalPath empty) the server clones the
// repository into ~/workspace/<repo-name>. To authenticate to a private
// remote, supply PlatformType + PlatformTokenID — the same token record
// created under 设置 → 平台 Token. The token is consumed for the clone only;
// subsequent git operations inside the worktree rely on the credentials
// git caches in the local repo config.
type AddProjectRequest struct {
	LocalPath       string `json:"local_path"`
	RemoteURL       string `json:"remote_url"`
	InitGit         bool   `json:"init_git"`
	// Optional clone --branch (ignored in local mode).
	Branch string `json:"branch,omitempty"`
	// "github" | "gitlab" | "gitea" — required when PlatformTokenID is set.
	PlatformType string `json:"platform_type,omitempty"`
	// tok_xxx — when set, used to authenticate the clone and persisted on
	// the project row for later PR review.
	PlatformTokenID string `json:"platform_token_id,omitempty"`
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
