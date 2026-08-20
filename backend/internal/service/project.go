package service

import (
	"context"
	"database/sql"

	"fmt"
	"net/url"
	"regexp"
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
	db         *db.DB
	platforms  *PlatformTokenService
}

// ProjectRef is a lightweight project handle (id + path) used by callers that
// only need to locate the on-disk project (e.g. description backfill).
type ProjectRef struct {
	ID        string
	LocalPath string
}

func NewProjectService(db *db.DB, platforms *PlatformTokenService) *ProjectService {
	return &ProjectService{db: db, platforms: platforms}
}

func (s *ProjectService) List() ([]model.Project, error) {
	return s.ListForUser("", true)
}

// ListForUser returns the projects visible to userID. Admins (isAdmin=true)
// see every non-deleted project; non-admins see only projects assigned via the
// user_projects table. An empty userID with isAdmin=true (the historical
// call site / NOVA_AUTH_DISABLED bypass) returns all projects.
func (s *ProjectService) ListForUser(userID string, isAdmin bool) ([]model.Project, error) {
	q := `SELECT id, name, local_path, remote_url, status, default_branch,
		project_type, claude_files, platform_type, platform_token_id, added_at, updated_at, last_scanned_at,
		deleted_at, deleted_dir, description, description_manual, description_hash
		FROM projects`
	args := []any{}
	if !isAdmin || userID == "" {
		if userID == "" {
			// No user and not admin → nothing visible.
			return []model.Project{}, nil
		}
		q += ` WHERE deleted_at IS NULL AND id IN (SELECT project_id FROM user_projects WHERE user_id = ?)`
		args = append(args, userID)
	} else {
		q += ` WHERE deleted_at IS NULL`
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []model.Project
	for rows.Next() {
		var p model.Project
		err := rows.Scan(&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
			&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
			&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir, &p.Description, &p.DescriptionManual, &p.DescriptionHash)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

// CanAccess reports whether userID may access projectID. Admins see all; an
// empty userID with isAdmin=true (auth bypass) sees all.
func (s *ProjectService) CanAccess(userID string, isAdmin bool, projectID string) (bool, error) {
	if isAdmin || userID == "" {
		return true, nil
	}
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM user_projects WHERE user_id = ? AND project_id = ?`, userID, projectID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *ProjectService) Get(id string) (*model.Project, error) {
	var p model.Project
	err := s.db.QueryRow(`SELECT id, name, local_path, remote_url, status, default_branch,
		project_type, claude_files, platform_type, platform_token_id, added_at, updated_at, last_scanned_at,
		deleted_at, deleted_dir, description, description_manual, description_hash
		FROM projects WHERE id = ? AND deleted_at IS NULL`, id).Scan(
		&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
		&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
		&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir, &p.Description, &p.DescriptionManual, &p.DescriptionHash)
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
		deleted_at, deleted_dir, description, description_manual, description_hash
		FROM projects WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
		&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
		&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir, &p.Description, &p.DescriptionManual, &p.DescriptionHash)
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
		deleted_at, deleted_dir, description, description_manual, description_hash
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
			&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir, &p.Description, &p.DescriptionManual, &p.DescriptionHash)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

func (s *ProjectService) Add(req model.AddProjectRequest) (*model.Project, error) {
	path := req.LocalPath

	// Remote mode: no local path supplied yet — clone into the workspace using
	// the repo name derived from the remote URL.
	if path == "" && req.RemoteURL != "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, "workspace", repoName(req.RemoteURL))
	}
	if path == "" {
		return nil, fmt.Errorf("local_path is required")
	}

	// Expand ~
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[1:])
	}
	path, _ = filepath.Abs(path)

	// Validate the optional platform/token pairing before any disk work so
	// a bad token never leaves a half-cloned tree behind.
	tokenSecret, tokenPlatform, err := s.resolveCloneAuth(req.PlatformType, req.PlatformTokenID, req.RemoteURL)
	if err != nil {
		return nil, err
	}

	// Clone the remote into the target when it doesn't exist yet.
	if req.RemoteURL != "" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := cloneRepo(req.RemoteURL, req.Branch, path, tokenPlatform, tokenSecret); err != nil {
				return nil, err
			}
		}
	}

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

	// platform_type/platform_token_id may be empty (public repo, no token).
	// Persist them when supplied so PR review reuses the same credentials.
	_, err = s.db.Exec(`INSERT INTO projects (id, name, local_path, remote_url, status, project_type, claude_files,
			platform_type, platform_token_id, added_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?)`,
		id, name, path, req.RemoteURL, projectType, claudeFiles,
		req.PlatformType, req.PlatformTokenID, now, now)
	if err != nil {
		return nil, fmt.Errorf("failed to insert project: %w", err)
	}

	return s.Get(id)
}

// resolveCloneAuth validates the platform/token pair supplied with a remote
// add. Returns the raw token secret + its platform kind so cloneRepo can
// build an authenticated URL / ssh command. Errors are surfaced with stable
// prefixes the handler maps to HTTP status codes:
//   TOKEN_NOT_FOUND  — token ID missing or unknown
//   PLATFORM_MISMATCH — token platform doesn't match the user-supplied platform_type
//                       (and can't be inferred from the remote URL host)
//
// Both platformType and tokenID must be set together — passing one without
// the other is treated as TOKEN_NOT_FOUND to keep the contract explicit.
func (s *ProjectService) resolveCloneAuth(platformType, tokenID, remoteURL string) (string, string, error) {
	if tokenID == "" && platformType == "" {
		return "", "", nil
	}
	if tokenID == "" || platformType == "" {
		return "", "", fmt.Errorf("TOKEN_NOT_FOUND: token id and platform must be supplied together")
	}
	tok, err := s.platforms.Get(tokenID)
	if err != nil {
		return "", "", fmt.Errorf("TOKEN_NOT_FOUND: %w", err)
	}
	if tok.Platform != platformType {
		return "", "", fmt.Errorf("PLATFORM_MISMATCH: token is for %q, request asked for %q", tok.Platform, platformType)
	}
	// Defensive: if the URL host suggests a different platform than the
	// supplied token (e.g. github token + gitlab.com URL), refuse the clone
	// rather than silently push the wrong creds.
	if host, ok := urlHost(remoteURL); ok && hostPlatform(host) != "" && hostPlatform(host) != tok.Platform {
		return "", "", fmt.Errorf("PLATFORM_MISMATCH: remote host %q belongs to %q but token is for %q",
			host, hostPlatform(host), tok.Platform)
	}
	return tok.Token, tok.Platform, nil
}

// urlHost extracts the lowercased hostname from a git URL. Returns
// ("", false) for SSH-style scp-like URLs (git@github.com:foo/bar.git),
// where the "host" is buried in the userinfo and a different parser is
// needed — hostPlatform handles those too.
func urlHost(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	return strings.ToLower(u.Hostname()), true
}

// hostPlatform maps a git host to its platform kind. Empty string = unknown.
func hostPlatform(host string) string {
	switch host {
	case "github.com":
		return "github"
	case "gitlab.com":
		return "gitlab"
	}
	// Gitea is self-hosted — only detectable via the platform_tokens row's
	// base_url. The caller cross-checks that elsewhere.
	return ""
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

// repoName extracts the repository name from a git URL (git@host:owner/repo.git,
// https://host/owner/repo.git, or a bare path) so a remote add can clone into
// ~/workspace/<repo>.
func repoName(url string) string {
	s := strings.TrimSpace(url)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// cloneRepo clones remote into dest, optionally pinning a branch, with a 5m
// timeout. On failure it removes any half-finished clone.
//
// When tokenSecret is non-empty the URL is rewritten to embed credentials
// (HTTPS) or, for SSH URLs, the token is ignored and authentication relies
// on the user's ssh-agent / key — by design, since SSH doesn't take a token.
//
// To prevent the previous behavior — git prompting on STDIN for the host
// key or for credentials and blocking the whole handler — we always set:
//
//	GIT_TERMINAL_PROMPT=0          — never prompt on stdin; fail fast
//	GIT_SSH_COMMAND=...accept-new  — auto-accept first-time host keys,
//	                                 reject changed keys instead of hanging
//	                                 on stdin
//
// A redundant "yes\n" is piped into stdin as belt-and-suspenders.
func cloneRepo(remote, branch, dest, platform, tokenSecret string) error {
	cloneURL := injectCredentials(remote, platform, tokenSecret)

	args := []string{"clone"}
	if branch != "" && branch != "main" && branch != "master" {
		args = append(args, "--branch", branch)
	}
	args = append(args, cloneURL, dest)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)

	// Strip the userinfo from the URL before logging. injectCredentials
	// returns a URL with embedded creds; we still want a clean view in
	// stderr / debug logs in case git echoes it.
	cmd.Stdin = strings.NewReader("yes\n")
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o UserKnownHostsFile=/dev/null",
	)
	// Run with separate streams so a long stderr (e.g. "fatal: Authentication
	// failed") reaches us even if stdout is silent. CombinedOutput would
	// buffer both; we still concatenate for the error message.
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		os.RemoveAll(dest) // clean up a half-finished clone
		out := strings.TrimSpace(stdout.String() + stderr.String())
		// Drop any userinfo from URLs in the error so the token never leaks.
		out = redactUserinfo(out)
		if out == "" {
			out = err.Error()
		}
		return fmt.Errorf("%s — %w", out, err)
	}
	return nil
}

// injectCredentials returns a clone URL with the token embedded in the
// userinfo for HTTPS remotes. SSH / git@… remotes are returned unchanged —
// SSH auth is key-based, not token-based. An empty tokenSecret passes the
// URL through verbatim (public repo path).
func injectCredentials(remote, platform, tokenSecret string) string {
	if tokenSecret == "" {
		return remote
	}
	u, err := url.Parse(remote)
	if err != nil || u.Scheme == "" || (u.Scheme != "http" && u.Scheme != "https") {
		// git@…:owner/repo.git or any non-HTTP URL — leave alone.
		return remote
	}
	// Personal access tokens: drop any existing userinfo, set token as
	// username. GitHub PATs, GitLab PATs, and Gitea PATs all accept
	// "https://<token>@host/…" — the simplest portable form.
	u.User = url.UserPassword(tokenSecret, "")
	return u.String()
}

// redactUserinfo strips any https://user:token@host segments from s so an
// error message from git can be logged without leaking the token. It is a
// best-effort pass: it only touches the well-formed URL form.
var userinfoPattern = regexp.MustCompile(`([a-z][a-z0-9+\-.]*://)([^/\s:@]+):([^@\s/]+)@`)

func redactUserinfo(s string) string {
	return userinfoPattern.ReplaceAllString(s, "$1<redacted>@")
}

// Restore re-clones a soft-deleted project's directory from its stored
// remote_url/default_branch and clears the soft-delete flags. It errors with
// NO_REMOTE / DIR_EXISTS / RESTORE_FAILED / TOKEN_NOT_FOUND / PLATFORM_MISMATCH
// prefix for the handler to map to HTTP status codes.
//
// Re-cloning reuses the platform token that was stored on the project at
// add-time, so a private repo can be restored without re-entering creds.
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

	tokenSecret, tokenPlatform, err := s.resolveCloneAuth(p.PlatformType, p.PlatformTokenID, p.RemoteURL)
	if err != nil {
		return nil, err
	}

	if err := cloneRepo(p.RemoteURL, p.DefaultBranch, p.LocalPath, tokenPlatform, tokenSecret); err != nil {
		return nil, fmt.Errorf("RESTORE_FAILED: %w", err)
	}

	if _, err := s.db.Exec(
		`UPDATE projects SET deleted_at = NULL, deleted_dir = 0, updated_at = ? WHERE id = ?`,
		time.Now(), id); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *ProjectService) Dashboard() (*model.DashboardData, error) {
	return s.DashboardForUser("", true)
}

// DashboardForUser is the user-scoped dashboard: projects are limited to what
// userID can see (admin = all). Counts are derived from that same set so a
// non-admin never learns about projects they're not assigned to.
func (s *ProjectService) DashboardForUser(userID string, isAdmin bool) (*model.DashboardData, error) {
	projects, err := s.ListForUser(userID, isAdmin)
	if err != nil {
		return nil, err
	}

	visibleIDs := make(map[string]bool, len(projects))
	for _, p := range projects {
		visibleIDs[p.ID] = true
	}

	// Count active requirements across visible projects only.
	var activeReqs int
	if len(visibleIDs) > 0 {
		args := make([]any, 0, len(visibleIDs))
		placeholders := ""
		for id := range visibleIDs {
			if placeholders != "" {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, id)
		}
		// status values kept as the historical dashboard query (the lifecycle
		// names drift over releases; this is display-only).
		q := `SELECT COUNT(*) FROM requirements WHERE project_id IN (` + placeholders + `) AND status IN ('analysis','ready','in_progress')`
		s.db.QueryRow(q, args...).Scan(&activeReqs)
	}

	// Count pending reviews across visible projects only.
	var pendingReviews int
	if len(visibleIDs) > 0 {
		args := make([]any, 0, len(visibleIDs))
		placeholders := ""
		for id := range visibleIDs {
			if placeholders != "" {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, id)
		}
		q := `SELECT COUNT(*) FROM knowledge WHERE project_id IN (` + placeholders + `) AND is_reviewed = 0`
		s.db.QueryRow(q, args...).Scan(&pendingReviews)
	}

	// Count weekly commits (placeholder - needs git integration)
	weeklyCommits := 0

	return &model.DashboardData{
		TotalProjects:   len(projects),
		ActiveReqs:      activeReqs,
		PendingReviews:  pendingReviews,
		WeeklyCommits:   weeklyCommits,
		Projects:        projects,
		RecentActivity:  []model.ActivityItem{},
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

// UpdateDescription saves a manually-edited project description and locks it
// from automatic regeneration (description_manual=1). A manual edit always
// wins over the scanner's auto-regeneration.
func (s *ProjectService) UpdateDescription(id, desc string) error {
	res, err := s.db.Exec(
		`UPDATE projects SET description = ?, description_manual = 1, updated_at = ? WHERE id = ?`,
		desc, time.Now(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project not found: %s", id)
	}
	return nil
}

// SetAutoDescription writes an AI-generated description only when the row is
// not manually locked. Returns true when the row was updated (false = locked
// meanwhile, or the project vanished), so callers can distinguish a race-safe
// skip from a real write.
func (s *ProjectService) SetAutoDescription(id, desc, hash string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE projects SET description = ?, description_hash = ?, description_manual = 0, updated_at = ?
		 WHERE id = ? AND description_manual = 0`,
		desc, hash, time.Now(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ForceAutoDescription writes an AI-generated description and clears the manual
// lock unconditionally. Used by the explicit "regenerate" action, where the
// user asks to override their manual edit with a fresh AI summary.
func (s *ProjectService) ForceAutoDescription(id, desc, hash string) error {
	_, err := s.db.Exec(
		`UPDATE projects SET description = ?, description_hash = ?, description_manual = 0, updated_at = ?
		 WHERE id = ?`,
		desc, hash, time.Now(), id)
	return err
}

// DescriptionState returns the stored description, its manual-lock flag, and
// the SHA256 of the CLAUDE.md content the description was generated from.
func (s *ProjectService) DescriptionState(id string) (desc string, manual bool, hash string, err error) {
	err = s.db.QueryRow(
		`SELECT description, description_manual, description_hash FROM projects WHERE id = ?`, id).
		Scan(&desc, &manual, &hash)
	return
}

// ListProjectsNeedingDescription returns projects whose description is empty
// and not manually locked — candidates for the backfill endpoint.
func (s *ProjectService) ListProjectsNeedingDescription() ([]ProjectRef, error) {
	rows, err := s.db.Query(
		`SELECT id, local_path FROM projects WHERE description = '' AND description_manual = 0 AND deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectRef
	for rows.Next() {
		var r ProjectRef
		if err := rows.Scan(&r.ID, &r.LocalPath); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
