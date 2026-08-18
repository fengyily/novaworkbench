package service

import (
	"context"
	"database/sql"

	"fmt"
	"github.com/novaworkbench/backend/internal/db"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

type ProjectService struct {
	db *db.DB
}

func NewProjectService(db *db.DB) *ProjectService {
	return &ProjectService{db: db}
}

func (s *ProjectService) List() ([]model.Project, error) {
	rows, err := s.db.Query(`SELECT id, name, local_path, remote_url, status, default_branch,
		project_type, claude_files, platform_type, platform_token_id, added_at, updated_at, last_scanned_at,
		deleted_at, deleted_dir
		FROM projects WHERE deleted_at IS NULL ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []model.Project
	for rows.Next() {
		var p model.Project
		err := rows.Scan(&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
			&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
			&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir)
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
		project_type, claude_files, platform_type, platform_token_id, added_at, updated_at, last_scanned_at,
		deleted_at, deleted_dir
		FROM projects WHERE id = ? AND deleted_at IS NULL`, id).Scan(
		&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
		&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
		&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("project not found")
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// getAny returns a project regardless of soft-delete state (used by Remove/Restore).
func (s *ProjectService) getAny(id string) (*model.Project, error) {
	var p model.Project
	err := s.db.QueryRow(`SELECT id, name, local_path, remote_url, status, default_branch,
		project_type, claude_files, platform_type, platform_token_id, added_at, updated_at, last_scanned_at,
		deleted_at, deleted_dir
		FROM projects WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
		&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
		&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("project not found")
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListTrash returns soft-deleted projects (deleted_at IS NOT NULL).
func (s *ProjectService) ListTrash() ([]model.Project, error) {
	rows, err := s.db.Query(`SELECT id, name, local_path, remote_url, status, default_branch,
		project_type, claude_files, platform_type, platform_token_id, added_at, updated_at, last_scanned_at,
		deleted_at, deleted_dir
		FROM projects WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []model.Project
	for rows.Next() {
		var p model.Project
		err := rows.Scan(&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
			&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
			&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
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

	// Check duplicate (exclude soft-deleted projects)
	var exists int
	s.db.QueryRow("SELECT COUNT(*) FROM projects WHERE local_path = ? AND deleted_at IS NULL", path).Scan(&exists)
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

// Remove soft-deletes a project. When deleteDir is true, it also physically
// removes the project directory after a workspace-path safety check. On any
// directory-removal failure the soft-delete is rolled back so the project
// remains in its pre-delete state.
func (s *ProjectService) Remove(id string, deleteDir bool) error {
	p, err := s.getAny(id)
	if err != nil {
		return err
	}

	now := time.Now()
	nowStr := now.UTC().Format(time.RFC3339)
	dirVal := 0
	if deleteDir {
		dirVal = 1
	}

	// 1. Soft-delete.
	if _, err := s.db.Exec(
		`UPDATE projects SET deleted_at = ?, deleted_dir = ?, updated_at = ? WHERE id = ?`,
		nowStr, dirVal, now, id); err != nil {
		return err
	}

	// 2. Optionally remove the directory.
	if deleteDir && p.LocalPath != "" {
		if err := s.validateWorkspacePath(p.LocalPath); err != nil {
			s.rollbackDelete(id)
			return err
		}
		if err := os.RemoveAll(p.LocalPath); err != nil {
			s.rollbackDelete(id)
			return fmt.Errorf("REMOVE_DIR_FAILED: %w", err)
		}
	}
	return nil
}

// rollbackDelete clears the soft-delete flags so a failed directory removal
// leaves the project record untouched.
func (s *ProjectService) rollbackDelete(id string) {
	s.db.Exec(`UPDATE projects SET deleted_at = NULL, deleted_dir = 0 WHERE id = ?`, id)
}

// validateWorkspacePath ensures the target path resolves to a location strictly
// inside $HOME/workspace (not the workspace root itself) so a malformed
// local_path can never trigger a destructive removal outside the workspace.
func (s *ProjectService) validateWorkspacePath(path string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("PATH_OUT_OF_WORKSPACE: cannot resolve home dir: %w", err)
	}
	root := filepath.Join(home, "workspace")

	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("PATH_OUT_OF_WORKSPACE: cannot resolve path: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		real = filepath.Clean(abs) // path may not exist yet; fall back to cleaned form
	}
	rootReal, rootErr := filepath.EvalSymlinks(root)
	if rootErr != nil {
		rootReal = filepath.Clean(root)
	}
	sep := string(filepath.Separator)
	if real == rootReal || !strings.HasPrefix(real, rootReal+sep) {
		return fmt.Errorf("PATH_OUT_OF_WORKSPACE: %s is not inside %s", real, rootReal)
	}
	return nil
}

// Restore re-clones a soft-deleted project's directory from its stored
// remote_url/default_branch and clears the soft-delete flags. It errors with
// NO_REMOTE / DIR_EXISTS / RESTORE_FAILED prefix for the handler to map to
// HTTP status codes.
func (s *ProjectService) Restore(id string) (*model.Project, error) {
	p, err := s.getAny(id)
	if err != nil {
		return nil, err
	}
	if p.RemoteURL == "" {
		return nil, fmt.Errorf("NO_REMOTE: project has no git remote URL, cannot auto-restore")
	}
	if p.LocalPath == "" {
		return nil, fmt.Errorf("NO_REMOTE: project has no local path to restore into")
	}
	if _, err := os.Stat(p.LocalPath); err == nil {
		return nil, fmt.Errorf("DIR_EXISTS: target directory already exists: %s", p.LocalPath)
	}

	args := []string{"clone", p.RemoteURL}
	if p.DefaultBranch != "" && p.DefaultBranch != "main" && p.DefaultBranch != "master" {
		args = append(args, "--branch", p.DefaultBranch)
	}
	args = append(args, p.LocalPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(p.LocalPath) // clean up a half-finished clone
		return nil, fmt.Errorf("RESTORE_FAILED: %s: %w", strings.TrimSpace(string(out)), err)
	}

	if _, err := s.db.Exec(
		`UPDATE projects SET deleted_at = NULL, deleted_dir = 0, updated_at = ? WHERE id = ?`,
		time.Now(), id); err != nil {
		return nil, err
	}
	return s.Get(id)
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
		"package.json":     "Node.js",
		"go.mod":           "Go",
		"Cargo.toml":       "Rust",
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
