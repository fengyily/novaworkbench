package service

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

type ProjectService struct {
	db *sql.DB
}

func NewProjectService(db *sql.DB) *ProjectService {
	return &ProjectService{db: db}
}

func (s *ProjectService) List() ([]model.Project, error) {
	rows, err := s.db.Query(`SELECT id, name, local_path, remote_url, status, default_branch,
		project_type, claude_files, platform_type, platform_token_id, added_at, updated_at, last_scanned_at
		FROM projects ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []model.Project
	for rows.Next() {
		var p model.Project
		err := rows.Scan(&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
			&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
			&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

func (s *ProjectService) Get(id string) (*model.Project, error) {
	var p model.Project
	err := s.db.QueryRow(`SELECT id, name, local_path, remote_url, status, default_branch,
		project_type, claude_files, platform_type, platform_token_id, added_at, updated_at, last_scanned_at
		FROM projects WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
		&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
		&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("project not found")
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *ProjectService) Add(req model.AddProjectRequest) (*model.Project, error) {
	path := req.LocalPath
	if path == "" {
		return nil, fmt.Errorf("local_path is required")
	}

	// Expand ~
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[1:])
	}
	path, _ = filepath.Abs(path)

	// Validate path exists
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("path does not exist: %s", path)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", path)
	}

	// Check for git repo — auto-init if requested
	gitDir := filepath.Join(path, ".git")
	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		if req.InitGit {
			cmd := exec.Command("git", "init")
			cmd.Dir = path
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git init failed: %s — %w", string(out), err)
			}
		} else {
			return nil, fmt.Errorf("not a git repository: %s (only git repos are supported)", path)
		}
	}

	// Check duplicate
	var exists int
	s.db.QueryRow("SELECT COUNT(*) FROM projects WHERE local_path = ?", path).Scan(&exists)
	if exists > 0 {
		return nil, fmt.Errorf("project already added: %s", path)
	}

	// Detect project type
	projectType := detectProjectType(path)
	name := filepath.Base(path)

	// Detect AI config files
	claudeFiles := detectClaudeFiles(path)

	id := util.NewID("proj")
	now := time.Now()

	_, err = s.db.Exec(`INSERT INTO projects (id, name, local_path, remote_url, status, project_type, claude_files, added_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?)`,
		id, name, path, req.RemoteURL, projectType, claudeFiles, now, now)
	if err != nil {
		return nil, fmt.Errorf("failed to insert project: %w", err)
	}

	return s.Get(id)
}

func (s *ProjectService) Remove(id string, purge bool) error {
	if purge {
		p, err := s.Get(id)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(p.LocalPath); err != nil {
			return fmt.Errorf("failed to purge project files: %w", err)
		}
	}

	_, err := s.db.Exec("DELETE FROM projects WHERE id = ?", id)
	return err
}

func (s *ProjectService) Dashboard() (*model.DashboardData, error) {
	projects, err := s.List()
	if err != nil {
		return nil, err
	}

	// Count active requirements
	var activeReqs int
	s.db.QueryRow("SELECT COUNT(*) FROM requirements WHERE status IN ('analysis','ready','in_progress')").Scan(&activeReqs)

	// Count pending reviews
	var pendingReviews int
	s.db.QueryRow("SELECT COUNT(*) FROM knowledge WHERE is_reviewed = 0").Scan(&pendingReviews)

	// Count weekly commits (placeholder - needs git integration)
	weeklyCommits := 0

	return &model.DashboardData{
		TotalProjects:  len(projects),
		ActiveReqs:     activeReqs,
		PendingReviews: pendingReviews,
		WeeklyCommits:  weeklyCommits,
		Projects:       projects,
		RecentActivity: []model.ActivityItem{},
	}, nil
}

func detectProjectType(path string) string {
	indicators := map[string]string{
		"package.json": "Node.js",
		"go.mod":       "Go",
		"Cargo.toml":   "Rust",
		"requirements.txt": "Python",
		"pyproject.toml":   "Python",
		"pom.xml":          "Java/Maven",
		"build.gradle":     "Java/Gradle",
	}
	for file, ptype := range indicators {
		if _, err := os.Stat(filepath.Join(path, file)); err == nil {
			return ptype
		}
	}
	return "Unknown"
}

func detectClaudeFiles(path string) string {
	files := []string{}
	for _, f := range []string{"CLAUDE.md", "AGENTS.md", ".cursorrules"} {
		if _, err := os.Stat(filepath.Join(path, f)); err == nil {
			files = append(files, f)
		}
	}
	if len(files) > 0 {
		return `{"root":"` + strings.Join(files, `","`) + `"}`
	}
	return "{}"
}

// UpdatePlatformConfig sets the platform type and token ID for a project.
func (s *ProjectService) UpdatePlatformConfig(id, platformType, tokenID string) error {
	res, err := s.db.Exec(
		`UPDATE projects SET platform_type = ?, platform_token_id = ?, updated_at = ? WHERE id = ?`,
		platformType, tokenID, time.Now(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project not found: %s", id)
	}
	return nil
}
