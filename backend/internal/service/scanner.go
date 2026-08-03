package service

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/util"
)

type ScannerService struct {
	db *sql.DB
}

func NewScannerService(db *sql.DB) *ScannerService {
	return &ScannerService{db: db}
}

type ScanResult struct {
	ProjectID     string   `json:"project_id"`
	ProjectType   string   `json:"project_type"`
	ClaudeFiles   []string `json:"claude_files"`
	KnowledgeNew  int      `json:"knowledge_new"`
	KnowledgeUpd  int      `json:"knowledge_updated"`
	FilesScanned  int      `json:"files_scanned"`
	Duration      string   `json:"duration"`
}

func (s *ScannerService) Scan(projectID string) (*ScanResult, error) {
	start := time.Now()

	// Get project path
	var projectPath, projectType string
	err := s.db.QueryRow("SELECT local_path, project_type FROM projects WHERE id = ?", projectID).Scan(&projectPath, &projectType)
	if err != nil {
		return nil, err
	}

	result := &ScanResult{ProjectID: projectID}

	// Detect project type
	result.ProjectType = detectProjectTypeFromPath(projectPath)
	if result.ProjectType != "" {
		s.db.Exec("UPDATE projects SET project_type = ? WHERE id = ?", result.ProjectType, projectID)
	}

	// Scan for AI config files
	claudeFiles := []string{}
	for _, f := range []string{"CLAUDE.md", "AGENTS.md", ".cursorrules"} {
		if _, err := os.Stat(filepath.Join(projectPath, f)); err == nil {
			claudeFiles = append(claudeFiles, f)
		}
	}
	result.ClaudeFiles = claudeFiles

	claudeFilesJSON := "{" + func() string {
		parts := []string{}
		for _, f := range claudeFiles {
			parts = append(parts, `"`+f+`":"`+filepath.Join(projectPath, f)+`"`)
		}
		return strings.Join(parts, ",")
	}() + "}"
	s.db.Exec("UPDATE projects SET claude_files = ?, last_scanned_at = ? WHERE id = ?", claudeFilesJSON, time.Now(), projectID)

	// Backfill git remote URL / default branch if missing, so a soft-deleted
	// project can be restored later via `git clone`. Failures are non-fatal.
	s.backfillGitInfo(projectID, projectPath)

	// Index CLAUDE.md content as knowledge
	for _, f := range claudeFiles {
		fullPath := filepath.Join(projectPath, f)
		content, err := os.ReadFile(fullPath)
		if err != nil {
			continue
		}

		// Check if already indexed
		var exists int
		s.db.QueryRow("SELECT COUNT(*) FROM knowledge WHERE project_id = ? AND source_ref = ? AND source_type = 'document'",
			projectID, f).Scan(&exists)

		if exists > 0 {
			result.KnowledgeUpd++
			s.db.Exec("UPDATE knowledge SET content = ?, updated_at = ? WHERE project_id = ? AND source_ref = ? AND source_type = 'document'",
				string(content), time.Now(), projectID, f)
		} else {
			result.KnowledgeNew++
			id := util.NewID("kb")
			s.db.Exec(
				"INSERT INTO knowledge (id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at) VALUES (?,?,?,?,?,?,?,0,1,?,?)",
				id, projectID, f, string(content), "architecture", "document", f, time.Now(), time.Now())
		}
	}

	// Index basic project structure as knowledge
	if err := s.indexStructure(projectID, projectPath, &result.KnowledgeNew); err != nil {
		// non-fatal, continue
	}

	result.Duration = time.Since(start).Round(time.Millisecond).String()
	return result, nil
}

func (s *ScannerService) indexStructure(projectID, projectPath string, count *int) error {
	// Generate a simple project structure summary
	var summary strings.Builder
	summary.WriteString("## Project Structure\n\n")
	summary.WriteString("Path: " + projectPath + "\n\n")

	topEntries, err := os.ReadDir(projectPath)
	if err != nil {
		return err
	}

	summary.WriteString("### Top-level files and directories\n\n")
	for _, e := range topEntries {
		if strings.HasPrefix(e.Name(), ".") && e.Name() != ".gitignore" {
			continue
		}
		prefix := "📁"
		if !e.IsDir() {
			prefix = "📄"
		}
		summary.WriteString("- " + prefix + " " + e.Name() + "\n")
		*count++
	}

	// Check if already indexed
	var exists int
	s.db.QueryRow("SELECT COUNT(*) FROM knowledge WHERE project_id = ? AND category = 'architecture' AND source_type = 'code'",
		projectID).Scan(&exists)

	id := util.NewID("kb")
	now := time.Now()
	if exists > 0 {
		s.db.Exec("UPDATE knowledge SET content = ?, updated_at = ? WHERE project_id = ? AND category = 'architecture' AND source_type = 'code'",
			summary.String(), now, projectID)
	} else {
		s.db.Exec(
			"INSERT INTO knowledge (id, project_id, title, content, category, source_type, source_ref, is_reviewed, is_approved, created_at, updated_at) VALUES (?,?,?,?,?,?,?,0,1,?,?)",
			id, projectID, "Project Structure", summary.String(), "architecture", "code", projectPath, now, now)
		*count++
	}

	return nil
}

func detectProjectTypeFromPath(path string) string {
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

// backfillGitInfo records the project's origin remote URL and current branch
// if either is missing in the DB, so the project can be re-cloned on restore.
// Errors are ignored — this is a best-effort enhancement.
func (s *ScannerService) backfillGitInfo(projectID, projectPath string) {
	var remoteURL, branch string
	s.db.QueryRow("SELECT remote_url, default_branch FROM projects WHERE id = ?", projectID).Scan(&remoteURL, &branch)

	if remoteURL == "" {
		if out, err := exec.Command("git", "-C", projectPath, "remote", "get-url", "origin").Output(); err == nil {
			remoteURL = strings.TrimSpace(string(out))
		}
	}
	if branch == "" {
		if out, err := exec.Command("git", "-C", projectPath, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
			branch = strings.TrimSpace(string(out))
		}
	}

	if remoteURL != "" || branch != "" {
		s.db.Exec(`UPDATE projects SET
			remote_url = COALESCE(NULLIF(remote_url, ''), ?),
			default_branch = COALESCE(NULLIF(default_branch, ''), ?),
			updated_at = ?
			WHERE id = ?`,
			remoteURL, branch, time.Now(), projectID)
	}
}
