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
	// ClaudeProjectSlug is the directory name Claude CLI assigned to this
	// project under ~/.claude/projects/<slug>/. Cached on first local
	// start-coding (DiscoverAndCacheClaudeProjectSlug); read by the Agent
	// Server path (handler/wizard.go runRemoteCoding) to map the local
	// session dir to the remote cwd's slug for SFTP session sync. Empty
	// until discovered.
	ClaudeProjectSlug string `json:"claude_project_slug,omitempty"`
	// Commit language preference driving the "提交/PR 遵循项目历史风格"
	// pipeline. CommitLang is the auto-detected dominant language of the
	// project's git history (zh / en / mixed / ""). CommitLangOverride is
	// the user-pinned value and always wins over CommitLang when non-empty.
	// CommitLangSource records who set CommitLang ("auto" via scanner,
	// "manual" via override API, "" never written). CommitLangUpdatedAt is
	// the last successful write timestamp. Consumed by the wizard push
	// sub-task prompt, the pr_author role, and the frontend style chip.
	CommitLang         string     `json:"commit_lang,omitempty"`
	CommitLangOverride string     `json:"commit_lang_override,omitempty"`
	CommitLangSource   string     `json:"commit_lang_source,omitempty"`
	CommitLangUpdatedAt *time.Time `json:"commit_lang_updated_at,omitempty"`
	// Repo-sync trail for the "方案设计前同步仓库到工作目录" wizard prologue.
	// LastSyncedAt is the timestamp of the most recent clone/fetch attempt
	// (best-effort — failure still stamps the column). LastSyncedCommit is the
	// short SHA of origin/<defaultBranch> after the last successful sync
	// (empty until first sync). SyncStatus is the terminal state of the most
	// recent sync attempt: 'idle' = never synced, 'ok' = success, 'error' =
	// last attempt failed (see service.SyncStatusIdle/OK/Error constants).
	// Surfaced via GET /api/projects/{id} so the ProjectDetail badge and the
	// architect-design "24h stale" hint can read it without a new endpoint.
	LastSyncedAt     *time.Time `json:"last_synced_at,omitempty"`
	LastSyncedCommit string     `json:"last_synced_commit"`
	SyncStatus       string     `json:"sync_status"` // idle | ok | error
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
