package service

import (
	"context"
	"database/sql"

	"fmt"
	"io"
	"log"
	"net/http"
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

// SyncStatus* constants drive the projects.sync_status column (see schema.go's
// last_synced_* ALTERs). The wizard's EnsureClonedAndSynced stamps one of these
// after every clone/fetch attempt; the value is exposed via GET /api/projects/{id}
// so the ProjectDetail badge and the architect-design "24h stale" hint can read
// it without a new endpoint. New rows default to SyncStatusIdle via the column
// DEFAULT clause, so legacy / pre-migration projects render as "待同步" without
// a backfill.
const (
	SyncStatusIdle  = "idle"  // project has never been synced (clone or fetch)
	SyncStatusOK    = "ok"    // last sync attempt succeeded (cloned or fetched)
	SyncStatusError = "error" // last sync attempt failed; wizard fell back to local snapshot
)

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
		deleted_at, deleted_dir, description, description_manual, description_hash, claude_project_slug,
		commit_lang, commit_lang_override, commit_lang_source, commit_lang_updated_at,
		last_synced_at, last_synced_commit, sync_status,
		requirement_count, issue_count, idea_count
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
			&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir, &p.Description, &p.DescriptionManual, &p.DescriptionHash,
			&p.ClaudeProjectSlug,
			&p.CommitLang, &p.CommitLangOverride, &p.CommitLangSource, &p.CommitLangUpdatedAt,
			&p.LastSyncedAt, &p.LastSyncedCommit, &p.SyncStatus,
		&p.RequirementCount, &p.IssueCount, &p.IdeaCount)
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
		deleted_at, deleted_dir, description, description_manual, description_hash, claude_project_slug,
		commit_lang, commit_lang_override, commit_lang_source, commit_lang_updated_at,
		last_synced_at, last_synced_commit, sync_status,
		requirement_count, issue_count, idea_count
		FROM projects WHERE id = ? AND deleted_at IS NULL`, id).Scan(
		&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
		&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
		&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir, &p.Description, &p.DescriptionManual, &p.DescriptionHash,
		&p.ClaudeProjectSlug,
		&p.CommitLang, &p.CommitLangOverride, &p.CommitLangSource, &p.CommitLangUpdatedAt,
		&p.LastSyncedAt, &p.LastSyncedCommit, &p.SyncStatus,
		&p.RequirementCount, &p.IssueCount, &p.IdeaCount)
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
		deleted_at, deleted_dir, description, description_manual, description_hash, claude_project_slug,
		commit_lang, commit_lang_override, commit_lang_source, commit_lang_updated_at,
		last_synced_at, last_synced_commit, sync_status,
		requirement_count, issue_count, idea_count
		FROM projects WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &p.LocalPath, &p.RemoteURL, &p.Status,
		&p.DefaultBranch, &p.ProjectType, &p.ClaudeFiles, &p.PlatformType, &p.PlatformTokenID,
		&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir, &p.Description, &p.DescriptionManual, &p.DescriptionHash,
		&p.ClaudeProjectSlug,
		&p.CommitLang, &p.CommitLangOverride, &p.CommitLangSource, &p.CommitLangUpdatedAt,
		&p.LastSyncedAt, &p.LastSyncedCommit, &p.SyncStatus,
		&p.RequirementCount, &p.IssueCount, &p.IdeaCount)
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
		deleted_at, deleted_dir, description, description_manual, description_hash, claude_project_slug,
		commit_lang, commit_lang_override, commit_lang_source, commit_lang_updated_at,
		last_synced_at, last_synced_commit, sync_status,
		requirement_count, issue_count, idea_count
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
			&p.AddedAt, &p.UpdatedAt, &p.LastScannedAt, &p.DeletedAt, &p.DeletedDir, &p.Description, &p.DescriptionManual, &p.DescriptionHash,
			&p.ClaudeProjectSlug,
			&p.CommitLang, &p.CommitLangOverride, &p.CommitLangSource, &p.CommitLangUpdatedAt,
			&p.LastSyncedAt, &p.LastSyncedCommit, &p.SyncStatus,
		&p.RequirementCount, &p.IssueCount, &p.IdeaCount)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

func (s *ProjectService) Add(req model.AddProjectRequest) (*model.Project, error) {
	log.Printf("[gitlab-debug] service.Add entry: remote_url=%q platform_type=%q platform_token_id=%q branch=%q",
		req.RemoteURL, req.PlatformType, req.PlatformTokenID, req.Branch)
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
	tokenSecret, tokenPlatform, tokenBaseURL, tokenGitUserName, err := s.resolveCloneAuth(req.PlatformType, req.PlatformTokenID, req.RemoteURL)
	if err != nil {
		return nil, err
	}

	// Pre-clone token validation: GitLab's git HTTP basic auth and the
	// `/api/v4/user` endpoint use different auth paths, so a token that's
	// valid for the API may still be rejected on `git clone` (e.g. missing
	// `read_repository` scope, or the GitLab admin disabled basic auth on
	// the git endpoints). Validate up-front and return a specific error
	// instead of the cryptic "HTTP Basic: Access denied" from git.
	if req.RemoteURL != "" && tokenSecret != "" && tokenPlatform == "gitlab" {
		if vErr := validateGitLabToken(tokenBaseURL, tokenSecret, req.RemoteURL); vErr != nil {
			return nil, vErr
		}
	}

	// Clone the remote into the target when it doesn't exist yet.
	if req.RemoteURL != "" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := cloneRepo(req.RemoteURL, req.Branch, path, tokenPlatform, tokenSecret, tokenGitUserName); err != nil {
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
				return nil, wrapGitError("git init", string(out), err)
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
func (s *ProjectService) resolveCloneAuth(platformType, tokenID, remoteURL string) (string, string, string, string, error) {
	host, _ := urlHost(remoteURL)
	log.Printf("[gitlab-debug] resolveCloneAuth in: platform_type=%q token_id=%q remote_host=%q",
		platformType, tokenID, host)
	if tokenID == "" && platformType == "" {
		log.Printf("[gitlab-debug] resolveCloneAuth early-return: no token requested (public-repo path)")
		return "", "", "", "", nil
	}
	if tokenID == "" || platformType == "" {
		return "", "", "", "", fmt.Errorf("TOKEN_NOT_FOUND: token id and platform must be supplied together")
	}
	tok, err := s.platforms.Get(tokenID)
	if err != nil {
		log.Printf("[gitlab-debug] resolveCloneAuth platforms.Get(%q) err=%v", tokenID, err)
		return "", "", "", "", fmt.Errorf("TOKEN_NOT_FOUND: %w", err)
	}
	if tok.Platform != platformType {
		return "", "", "", "", fmt.Errorf("PLATFORM_MISMATCH: token is for %q, request asked for %q", tok.Platform, platformType)
	}
	// Defensive: if the URL host suggests a different platform than the
	// supplied token (e.g. github token + gitlab.com URL), refuse the clone
	// rather than silently push the wrong creds.
	if host != "" && hostPlatform(host) != "" && hostPlatform(host) != tok.Platform {
		return "", "", "", "", fmt.Errorf("PLATFORM_MISMATCH: remote host %q belongs to %q but token is for %q",
			host, hostPlatform(host), tok.Platform)
	}
	log.Printf("[gitlab-debug] resolveCloneAuth out: token=%s platform=%q base_url=%q git_user_name=%q",
		redactToken(tok.Token), tok.Platform, tok.BaseURL, tok.GitUserName)
	return tok.Token, tok.Platform, tok.BaseURL, tok.GitUserName, nil
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

// isSSHRemote reports whether raw is an SSH-form git URL that requires the
// `ssh` binary on PATH (i.e. `git` would invoke `GIT_SSH_COMMAND=ssh …`).
// Both the explicit `ssh://` scheme and the scp-like `user@host:path` form
// count; plain https/http URLs do not, even though they may also use a
// token via injectCredentials.
func isSSHRemote(raw string) bool {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "ssh://") || strings.HasPrefix(s, "git+ssh://") {
		return true
	}
	// scp-like form: `user@host:path`. url.Parse places the whole thing in
	// Path for this shape, so the URL-based scheme check misses it. Fall
	// back to a regex — `[user@]host:path` with no scheme prefix.
	if strings.Contains(s, "://") {
		return false // explicit non-ssh scheme wins
	}
	if i := strings.Index(s, "@"); i > 0 {
		rest := s[i+1:]
		if j := strings.Index(rest, ":"); j > 0 && !strings.Contains(rest[:j], "/") {
			return true
		}
	}
	return false
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
func cloneRepo(remote, branch, dest, platform, tokenSecret, gitUserName string) error {
	log.Printf("[gitlab-debug] cloneRepo in: platform=%q token=%s git_user_name=%q dest=%q branch=%q",
		platform, redactToken(tokenSecret), gitUserName, dest, branch)
	cloneURL := injectCredentials(remote, platform, tokenSecret, gitUserName)
	log.Printf("[gitlab-debug] cloneRepo final URL: %s", redactUserinfo(cloneURL))

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
		// `git` missing on PATH (e.g. minimal container image) — surface a
		// actionable hint instead of the raw `executable file not found`.
		if isMissingExecutable(err) {
			return fmt.Errorf("git 未安装或不在 PATH 中，无法克隆仓库 %s：请在服务端安装 git（如 Alpine: apk add git）", redactUserinfo(remote))
		}
		// SSH remotes on a minimal image: `git` is there but `ssh` is not,
		// and GIT_SSH_COMMAND=ssh … fails with `ssh: not found`. Detect that
		// pattern and tell the user what to install (Alpine: openssh-client).
		if isSSHRemote(remote) && strings.Contains(out, "ssh:") && (strings.Contains(out, "not found") || strings.Contains(out, "No such file")) {
			return fmt.Errorf("ssh 未安装或不在 PATH 中，无法克隆 SSH 仓库 %s：请在服务端安装 openssh 客户端（如 Alpine: apk add openssh-client），或将远程地址改为 https://", redactUserinfo(remote))
		}
		if out == "" {
			out = err.Error()
		}
		return fmt.Errorf("%s — %w", out, err)
	}
	return nil
}

// isMissingExecutable reports whether err is the Go runtime's "executable
// file not found in $PATH" error. exec.Command returns *exec.Error wrapping
// fs.ErrNotExist with this exact message when LookPath fails. Matching the
// string avoids depending on Go's internal error types.
func isMissingExecutable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "executable file not found in $PATH")
}

// validateGitLabToken probes the GitLab instance with the supplied token
// before attempting a `git clone`. Two checks run in order:
//
//  1. GET {baseURL}/api/v4/user — confirms the token is recognised at all
//     (catches: wrong / expired / revoked tokens, wrong base URL).
//  2. GET {baseURL}/api/v4/projects/{path} — confirms the token can see the
//     specific project the user wants to clone (catches: missing
//     `read_repository` scope, project access not granted, project
//     doesn't exist on this instance).
//
// Both endpoints are authenticated with the `PRIVATE-TOKEN` header (not
// HTTP basic), so they exercise a different code path from `git clone`'s
// HTTP basic auth. A token that passes these checks but still gets
// "HTTP Basic: Access denied" on `git clone` indicates the GitLab admin
// has disabled basic auth on the git endpoints — the returned error
// message points the user at that, instead of leaving them guessing.
//
// All errors are prefixed with `TOKEN_INVALID:` so the handler can map
// them to a dedicated HTTP status. The token is never echoed back; URLs
// are passed through redactUserinfo.
func validateGitLabToken(baseURL, token, remoteURL string) error {
	if baseURL == "" {
		return fmt.Errorf("TOKEN_INVALID: GitLab base_url 未配置 — 请在「平台 Token」中重新保存 videocut-Gitlab，并填写 base_url（如 http://172.20.210.36）")
	}
	apiBase := strings.TrimRight(baseURL, "/") + "/api/v4"
	client := &http.Client{Timeout: 10 * time.Second}

	// 1. /user — basic token validity.
	req, _ := http.NewRequest(http.MethodGet, apiBase+"/user", nil)
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("TOKEN_INVALID: 无法连接 GitLab %s — %w", redactUserinfo(baseURL), err)
	}
	userBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("TOKEN_INVALID: GitLab 拒绝此 token (HTTP %d) — token 可能过期、被撤销或权限不足。请在 %s/-/user_settings/personal_access_tokens 重新生成 token，并确保勾选 api 和 read_repository (或 write_repository) 权限",
			resp.StatusCode, redactUserinfo(baseURL))
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("TOKEN_INVALID: GitLab /user 响应异常 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(userBody)))
	}

	// 2. /projects/{path} — project-level access. Skipped if we can't
	//    derive a project path from the remote URL.
	projPath := extractGitLabProjectPath(remoteURL)
	if projPath == "" {
		return nil
	}
	projURL := apiBase + "/projects/" + url.PathEscape(projPath)
	req2, _ := http.NewRequest(http.MethodGet, projURL, nil)
	req2.Header.Set("PRIVATE-TOKEN", token)
	resp2, err := client.Do(req2)
	if err != nil {
		return fmt.Errorf("TOKEN_INVALID: 无法连接 GitLab 验证项目访问 — %w", err)
	}
	projBody, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	switch {
	case resp2.StatusCode == http.StatusUnauthorized, resp2.StatusCode == http.StatusForbidden:
		return fmt.Errorf("TOKEN_INVALID: token 鉴权通过，但无法访问项目 %s (HTTP %d) — token 没有该项目的 read_repository 权限，或 token 已被吊销对该项目的访问。请在 GitLab 项目设置 → Members 确认 token 对应用户已被授予 Reporter 及以上角色",
			projPath, resp2.StatusCode)
	case resp2.StatusCode == http.StatusNotFound:
		return fmt.Errorf("TOKEN_INVALID: 项目 %s 在 GitLab 上不存在，或 token 没有该项目访问权限 (HTTP 404)。请确认 URL 拼写或为 token 授予项目访问权", projPath)
	case resp2.StatusCode != http.StatusOK:
		return fmt.Errorf("TOKEN_INVALID: GitLab /projects 响应异常 (HTTP %d): %s", resp2.StatusCode, strings.TrimSpace(string(projBody)))
	}
	return nil
}

// extractGitLabProjectPath turns a clone URL into the GitLab project
// path. Examples:
//
//	http://gitlab.example.com/team2/videocutbot.git  → "team2/videocutbot"
//	https://gitlab.example.com/group/sub/proj.git    → "group/sub/proj"
//	git@gitlab.example.com:team2/videocutbot.git     → "team2/videocutbot"
//
// Returns "" when no path can be derived.
func extractGitLabProjectPath(remoteURL string) string {
	s := strings.TrimSpace(remoteURL)
	// Strip scp-like `user@host:` prefix for SSH URLs.
	if i := strings.Index(s, "@"); i > 0 {
		rest := s[i+1:]
		if j := strings.Index(rest, ":"); j > 0 && !strings.Contains(rest[:j], "/") {
			s = rest[j+1:]
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Path == "" {
		return ""
	}
	p := strings.TrimPrefix(u.Path, "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimSuffix(p, "/")
	return p
}

// wrapGitError returns a clear error when git is missing on PATH, otherwise
// falls back to the previous "git <op> failed: <out> — <err>" format. Used
// by Add's auto-init path where there is no captured git stderr.
func wrapGitError(op, out string, err error) error {
	if isMissingExecutable(err) {
		return fmt.Errorf("git 未安装或不在 PATH 中，无法执行 %s：请在服务端安装 git（如 Alpine: apk add git）", op)
	}
	return fmt.Errorf("%s failed: %s — %w", op, out, err)
}

// injectCredentials returns a clone URL with the token embedded in the
// userinfo for HTTPS remotes. SSH / git@… remotes are returned unchanged —
// SSH auth is key-based, not token-based. An empty tokenSecret passes the
// URL through verbatim (public repo path).
func injectCredentials(remote, platform, tokenSecret, gitUserName string) string {
	if tokenSecret == "" {
		log.Printf("[gitlab-debug] injectCredentials: empty token, returning raw URL host=%q",
			hostOnlyForLog(remote))
		return remote
	}
	u, err := url.Parse(remote)
	if err != nil || u.Scheme == "" || (u.Scheme != "http" && u.Scheme != "https") {
		// git@…:owner/repo.git or any non-HTTP URL — leave alone.
		log.Printf("[gitlab-debug] injectCredentials: non-http URL host=%q scheme=%q, pass-through",
			u.Host, u.Scheme)
		return remote
	}
	// Drop any existing userinfo before re-attaching our own; otherwise the
	// rewritten URL would carry both the original and injected credentials.
	u.User = nil
	// Build the userinfo. url.UserPassword(username, password) keeps the
	// structural ":" between user and password un-percent-encoded, so the
	// wire form is `http://<user>:<password>@host/...` — which is what
	// GitLab expects (it reads the colon-separated form for HTTP basic).
	//
	// Platform-specific user/password split:
	//   - github / gitea:  user=<token>, password=""  → "https://<token>@host/..."
	//   - gitlab + username: user=<gitUserName>, password=<token>
	//                       → "http://fengyi:glpat-xxx@host/..." (works on self-hosted
	//                         GitLab that rejects the bare `oauth2:` prefix)
	//   - gitlab without username: user="oauth2", password=<token>
	//                       → "http://oauth2:<token>@host/..." (the GitLab-docs form
	//                         for PAT auth; preserved as a fallback if git_user_name
	//                         is empty in the platform_tokens row)
	user := tokenSecret
	password := ""
	switch platform {
	case "gitlab":
		if gitUserName != "" {
			user = gitUserName
			password = tokenSecret
		} else {
			user = "oauth2"
			password = tokenSecret
		}
	}
	u.User = url.UserPassword(user, password)
	out := u.String()
	log.Printf("[gitlab-debug] injectCredentials out: platform=%q token=%s host=%s",
		platform, redactToken(tokenSecret), redactUserinfo(out))
	return out
}

// hostOnlyForLog extracts just the host portion of a raw URL for logging,
// tolerating malformed input.
func hostOnlyForLog(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	return u.Host
}

// redactUserinfo strips any https://user:token@host segments from s so an
// error message from git can be logged without leaking the token. It is a
// best-effort pass: it only touches the well-formed URL form.
var userinfoPattern = regexp.MustCompile(`([a-z][a-z0-9+\-.]*://)([^/\s:@]+):([^@\s/]+)@`)

func redactUserinfo(s string) string {
	return userinfoPattern.ReplaceAllString(s, "$1<redacted>@")
}

// redactToken returns a safe-to-log form of a token secret: first 4 + "***" +
// last 4 chars. Short / empty tokens collapse to a sentinel so we never leak
// the full value into the log stream.
func redactToken(s string) string {
	if s == "" {
		return "<empty>"
	}
	if len(s) <= 8 {
		return "<short>"
	}
	return s[:4] + "***" + s[len(s)-4:]
}

// OriginURL returns the project's git remote_url with the platform token
// embedded in userinfo (HTTPS-only). Used by the Agent-server remote path
// (handler/wizard.go runRemoteCoding) so a remote `git clone` can pull the
// repo without the user configuring any credentials on the remote host.
//
// Returns:
//   - ("", NO_REMOTE) when the project has no remote_url
//   - (url, nil)         for the HTTPS-injected case
//   - (raw, nil)         for SSH remotes or projects without a token (pass-through)
func (s *ProjectService) OriginURL(projectID string) (string, error) {
	p, err := s.Get(projectID)
	if err != nil {
		return "", err
	}
	if p.RemoteURL == "" {
		return "", fmt.Errorf("NO_REMOTE: project %s has no remote_url configured", projectID)
	}
	tok, plat, _, gitUserName, err := s.resolveCloneAuth(p.PlatformType, p.PlatformTokenID, p.RemoteURL)
	if err != nil {
		// TOKEN_NOT_FOUND / PLATFORM_MISMATCH: propagate but caller may choose
		// to fall back to the raw URL for a public repo.
		return "", err
	}
	return injectCredentials(p.RemoteURL, plat, tok, gitUserName), nil
}

// FirstOriginURLForProbe returns the first non-soft-deleted project's
// remote_url (with platform credentials injected) so a per-server check job
// can probe git remote reachability without needing project context itself.
//
// "Check" is server-scoped, not project-scoped, so we pick any project with
// a configured remote_url — the probe is just a connectivity smoke test, not
// a clone. Returns ("", nil) when no project has a remote configured (the
// caller should treat that as "nothing to probe" rather than an error).
//
// Callers MUST redact the returned URL via handler.redactOriginForLog before
// surfacing it in any user-visible log line — the token is embedded in
// userinfo and would otherwise leak.
func (s *ProjectService) FirstOriginURLForProbe() (string, error) {
	var (
		id, remoteURL, platformType, platformTokenID string
	)
	err := s.db.QueryRow(
		`SELECT id, remote_url, platform_type, platform_token_id
		   FROM projects
		  WHERE deleted_at IS NULL AND remote_url != ''
		  ORDER BY updated_at DESC
		  LIMIT 1`,
	).Scan(&id, &remoteURL, &platformType, &platformTokenID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Reuse the same credential-injection path as OriginURL — a project
	// configured without a token (e.g. public repo) falls through with the
	// raw URL, which is fine for an `ls-remote` probe.
	tok, plat, _, gitUserName, cerr := s.resolveCloneAuth(platformType, platformTokenID, remoteURL)
	if cerr != nil {
		// TOKEN_NOT_FOUND / PLATFORM_MISMATCH → fall back to the raw URL so a
		// public repo still probes cleanly. This mirrors OriginURL's
		// "propagate but caller may fall back" semantics, adapted for the
		// probe use-case where any URL is better than nothing.
		return remoteURL, nil
	}
	return injectCredentials(remoteURL, plat, tok, gitUserName), nil
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

	tokenSecret, tokenPlatform, _, tokenGitUserName, err := s.resolveCloneAuth(p.PlatformType, p.PlatformTokenID, p.RemoteURL)
	if err != nil {
		return nil, err
	}

	if err := cloneRepo(p.RemoteURL, p.DefaultBranch, p.LocalPath, tokenPlatform, tokenSecret, tokenGitUserName); err != nil {
		return nil, fmt.Errorf("RESTORE_FAILED: %w", err)
	}

	if _, err := s.db.Exec(
		`UPDATE projects SET deleted_at = NULL, deleted_dir = 0, updated_at = ? WHERE id = ?`,
		time.Now(), id); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// EnsureCloned makes sure the project's working directory exists on disk.
// This is the recovery path for Docker environments where a container
// rebuild / fresh workspace mount leaves a previously-added project's
// directory absent: without it the next git operation fails with "非 git
// 仓库" or "git checkout 失败" — the wizard's reported error.
//
// Behaviour:
//   - Directory present → no-op (also a no-op when the path exists but is
//     not a git repo; we deliberately do NOT wipe a populated tree, since
//     removing user work would be far worse than a failed checkout).
//   - Directory missing + RemoteURL empty → error (no remote to restore).
//   - Directory missing + RemoteURL set  → re-clone into LocalPath using the
//     stored platform token + DefaultBranch.
//
// Re-cloning reuses the platform token that was stored on the project at
// add-time, so a private repo can be restored without re-entering creds.
func (s *ProjectService) EnsureCloned(id string) error {
	p, err := s.Get(id)
	if err != nil {
		return err
	}
	if p.LocalPath == "" {
		return fmt.Errorf("project has no local_path")
	}
	switch _, statErr := os.Stat(p.LocalPath); {
	case statErr == nil:
		return nil
	case !os.IsNotExist(statErr):
		return fmt.Errorf("stat project dir: %w", statErr)
	}
	// Directory is missing — only safe to recreate when the project has a
	// stored remote we can clone from.
	if p.RemoteURL == "" {
		return fmt.Errorf("project directory missing and no remote URL to re-clone: %s", p.LocalPath)
	}
	tokenSecret, tokenPlatform, _, tokenGitUserName, err := s.resolveCloneAuth(p.PlatformType, p.PlatformTokenID, p.RemoteURL)
	if err != nil {
		return err
	}
	branch := p.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	if err := cloneRepo(p.RemoteURL, branch, p.LocalPath, tokenPlatform, tokenSecret, tokenGitUserName); err != nil {
		return fmt.Errorf("re-clone from %s: %w", redactUserinfo(p.RemoteURL), err)
	}
	return nil
}

// ProjectSyncResult reports the outcome of EnsureClonedAndSynced so the
// caller (handler wizard stages) can tailor user-facing phase events
// ("cloned" vs "fetched" vs "skipped") without re-inspecting the project.
//
// All fields are best-effort metadata — the function never returns a fatal
// error for git failures; it stamps sync_status=error and lets the wizard
// continue with the local snapshot.
type ProjectSyncResult struct {
	Cloned  bool   // true when this call performed a `git clone`
	Fetched bool   // true when this call performed a `git fetch origin <base>`
	OldSHA  string // HEAD before fetch (empty when Cloned=true or Skipped=true)
	NewSHA  string // origin/<defaultBranch> SHA after fetch (or HEAD after clone)
	Skipped bool   // true when the project has no remote_url → sync_status stays idle
	Err     error  // non-nil when the last git step failed (network, auth, timeout)
}

// fetchTimeout caps the inner `git fetch origin <base>` so a stalled network
// or an unreachable remote cannot hang a wizard Job. 30s mirrors
// handler/worktree.go's syncBaseBranch timeout.
const fetchTimeout = 30 * time.Second

// syncBaseBranchService is the service-layer twin of handler/worktree.go's
// syncBaseBranch. It fetches the project's main branch into the local repo
// so any worktree branched afterwards starts from the latest upstream SHA.
//
// Returns the pre-fetch HEAD and the post-fetch origin/<baseBranch> SHA so
// the caller can surface them as user-visible phase lines. logf may be nil.
//
// Best-effort by design: no remote / offline / missing ref / fetch failure
// all surface as a non-nil error wrapped with the trimmed stderr — the caller
// (EnsureClonedAndSynced) decides whether to swallow or propagate.
func syncBaseBranchService(ctx context.Context, projectPath, baseBranch string, logf func(string)) (oldSHA, newSHA string, err error) {
	if projectPath == "" || baseBranch == "" {
		return "", "", nil
	}
	// Skip non-git checkouts silently (the wizard falls back to the legacy
	// in-place strategy via resolveWorkDir → EnsureWorktreeLogged).
	if _, gerr := gitRunPlain(projectPath, "rev-parse", "--is-inside-work-tree"); gerr != nil {
		return "", "", nil
	}
	if _, gerr := gitRunPlain(projectPath, "remote", "get-url", "origin"); gerr != nil {
		if logf != nil {
			logf("ℹ️ 项目未配置 origin 远程，跳过主分支同步")
		}
		return "", "", nil
	}
	// Capture the pre-fetch HEAD so callers can diff "what changed".
	if head, herr := gitRunPlain(projectPath, "rev-parse", "HEAD"); herr == nil {
		oldSHA = strings.TrimSpace(head)
	}
	// GIT_TERMINAL_PROMPT=0 disables interactive auth (private repos with
	// missing credentials would otherwise hang forever).
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(fctx, "git", "-C", projectPath, "fetch", "origin", baseBranch)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		return oldSHA, "", fmt.Errorf("git fetch origin %s: %s: %w", baseBranch, strings.TrimSpace(stderr.String()), runErr)
	}
	if tip, terr := gitRunPlain(projectPath, "rev-parse", "--verify", "--quiet", "origin/"+baseBranch); terr == nil {
		newSHA = strings.TrimSpace(tip)
	}
	if logf != nil {
		logf("🔄 已同步 origin/" + baseBranch)
		if newSHA != "" {
			if msg, terr := gitRunPlain(projectPath, "log", "-1", "origin/"+baseBranch,
				"--format=%h %s (%an, %ad)", "--date=format:%Y-%m-%d %H:%M"); terr == nil {
				if msg = strings.TrimSpace(msg); msg != "" {
					logf("📌 origin/" + baseBranch + " 最新提交: " + msg)
				}
			}
		}
	}
	return oldSHA, newSHA, nil
}

// gitRunPlain is a thin wrapper over exec.Command("git", ...) for read-only
// subcommands used by syncBaseBranchService. It deliberately does NOT take a
// timeout — callers wrap with their own context when needed.
func gitRunPlain(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// EnsureClonedAndSynced is the wizard prologue for every architect / coding
// / analyst stage: it makes sure the project's local checkout is either
// cloned from origin (when missing) or freshly fetched from origin/<base>
// (when present), and persists the outcome onto projects.sync_status so the
// UI can show "已同步 / 同步失败 / 待同步".
//
// Behaviour matrix:
//
//	remote missing  + dir missing   →  fatal "no remote to re-clone" (matches EnsureCloned)
//	remote missing  + dir present   →  Skipped=true, sync_status stays idle
//	remote present  + dir missing   →  EnsureCloned → Cloned=true, sync_status=ok
//	remote present  + dir present   →  fetch → Fetched=true, sync_status=ok (or error on failure)
//
// Sync failures (network / auth / timeout) are LOGGED and stamped as
// sync_status=error but DO NOT return a non-nil error: the wizard continues
// with the local snapshot so an offline user can still produce a design
// against the last-known code. ctx is used to bound the inner fetch.
func (s *ProjectService) EnsureClonedAndSynced(ctx context.Context, id string, logf func(string)) (ProjectSyncResult, error) {
	var res ProjectSyncResult
	p, err := s.Get(id)
	if err != nil {
		return res, err
	}
	if p.LocalPath == "" {
		return res, fmt.Errorf("project has no local_path")
	}
	now := time.Now()
	stat, statErr := os.Stat(p.LocalPath)
	switch {
	case statErr == nil && !stat.IsDir():
		return res, fmt.Errorf("project path is not a directory: %s", p.LocalPath)
	case statErr != nil && !os.IsNotExist(statErr):
		return res, fmt.Errorf("stat project dir: %w", statErr)
	}

	// Case 1: project has no remote → nothing to sync against. Leave
	// sync_status untouched so a future edit (setting remote_url) starts
	// from idle, and skip both clone and fetch.
	if p.RemoteURL == "" {
		res.Skipped = true
		if logf != nil {
			logf("ℹ️ 项目未配置 remote_url，跳过仓库同步")
		}
		return res, nil
	}

	// Case 2: directory missing → reuse EnsureCloned's clone path. Treat its
	// failure as sync_status=error (the wizard still proceeds to the legacy
	// "project directory not found" error via resolveWorkDirLogged, so the
	// failure is surfaced higher up).
	if statErr != nil {
		if logf != nil {
			logf("🔄 同步仓库到最新版本…（首次 clone）")
		}
		if cerr := s.EnsureCloned(id); cerr != nil {
			res.Err = cerr
			if logf != nil {
				logf("⚠️ " + cerr.Error())
			}
			_ = s.updateSyncStatus(id, SyncStatusError, "", now)
			return res, nil // not fatal — caller decides
		}
		res.Cloned = true
		if head, herr := gitRunPlain(p.LocalPath, "rev-parse", "HEAD"); herr == nil {
			res.NewSHA = strings.TrimSpace(head)
		}
		if logf != nil {
			logf("✅ 仓库已同步（克隆）")
		}
		_ = s.updateSyncStatus(id, SyncStatusOK, res.NewSHA, now)
		return res, nil
	}

	// Case 3: directory present → fetch origin/<base>. Best-effort.
	if logf != nil {
		logf("🔄 同步仓库到最新版本…")
	}
	oldSHA, newSHA, ferr := syncBaseBranchService(ctx, p.LocalPath, p.DefaultBranch, logf)
	if ferr != nil {
		res.Err = ferr
		if logf != nil {
			logf("⚠️ " + ferr.Error())
		}
		_ = s.updateSyncStatus(id, SyncStatusError, "", now)
		return res, nil // not fatal
	}
	res.Fetched = true
	res.OldSHA = oldSHA
	res.NewSHA = newSHA
	if logf != nil {
		logf("✅ 已同步" + shortSHATag(newSHA))
	}
	_ = s.updateSyncStatus(id, SyncStatusOK, newSHA, now)
	return res, nil
}

// shortSHATag renders a 7-char commit tag for SSE phase lines (" 已同步到 abc1234").
// Empty input → empty string so the caller can chain without branching.
func shortSHATag(sha string) string {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return ""
	}
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return " 到 " + sha
}

// updateSyncStatus stamps last_synced_at / last_synced_commit / sync_status
// + updated_at on a project row. Errors are swallowed by callers (the wizard
// must continue even when the audit row write fails) — only logged here for
// ops visibility.
func (s *ProjectService) updateSyncStatus(id, status, commit string, when time.Time) error {
	_, err := s.db.Exec(
		`UPDATE projects SET sync_status = ?, last_synced_commit = ?, last_synced_at = ?, updated_at = ? WHERE id = ?`,
		status, commit, when, when, id,
	)
	if err != nil {
		log.Printf("[project-sync] update %s status=%s commit=%s: %v", id, status, commit, err)
	}
	return err
}

// UpdateSyncStatus is the public counterpart of updateSyncStatus. The agent-
// server SSH path runs `git fetch origin <base>` inside originTransport
// (handler/wizard_remote.go) and needs to stamp the local projects.sync_status
// from outside the service package — exposes a thin wrapper that fills in the
// timestamp and delegates to the private method so the only DB-write code
// path stays in one spot. Errors are still swallowed at the private layer so a
// failed audit row write never aborts the wizard.
func (s *ProjectService) UpdateSyncStatus(id, status, commit string) {
	_ = s.updateSyncStatus(id, status, commit, time.Now())
}

// Purge permanently removes a soft-deleted project and its on-disk
// directory. Errors are prefixed with NOT_IN_TRASH / PROJECT_NOT_FOUND /
// REMOVE_DIR_FAILED / PURGE_FAILED so the handler can map them to HTTP
// status codes.
//
// Only projects in the trash (deleted_at IS NOT NULL) can be purged. This
// is a defensive gate: a user who hits Purge on an active row by mistake
// should be redirected through the soft-delete modal first.
//
// Child rows in knowledge / memories / requirements / conversations /
// project_run_configs / weekly_reports / user_projects / refinement_chats
// are removed by ON DELETE CASCADE on the projects FK. token_usage has a
// project_id column but no FK (append-only billing log), so we clean it up
// explicitly to keep the table consistent with the project set.
func (s *ProjectService) Purge(id string) error {
	p, err := s.getAny(id)
	if err != nil {
		return err
	}
	if p.DeletedAt == nil {
		return fmt.Errorf("NOT_IN_TRASH: project %s is not in the trash; soft-delete it first", id)
	}

	// Remove the directory when we still own it on disk. deleted_dir == 0
	// means Remove() left it alone; == 1 means Remove() already wiped it.
	// If the path has been re-created since (e.g. user cloned back
	// manually), validateWorkspacePath guards against a runaway removal.
	if p.LocalPath != "" && p.DeletedDir == 0 {
		if _, err := os.Stat(p.LocalPath); err == nil {
			if err := s.validateWorkspacePath(p.LocalPath); err != nil {
				return err
			}
			if err := os.RemoveAll(p.LocalPath); err != nil {
				return fmt.Errorf("REMOVE_DIR_FAILED: %w", err)
			}
		}
	}

	// Drop the billing-log rows first. token_usage has no FK to projects,
	// so the cascade below wouldn't touch them.
	if _, err := s.db.Exec(`DELETE FROM token_usage WHERE project_id = ?`, id); err != nil {
		return fmt.Errorf("PURGE_FAILED: token_usage cleanup: %w", err)
	}

	// CASCADE deletes everything else (memories, requirements, knowledge,
	// conversations, project_run_configs, weekly_reports, user_projects,
	// refinement_chats). Token-usage cleanup already done.
	if _, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id); err != nil {
		return fmt.Errorf("PURGE_FAILED: %w", err)
	}
	return nil
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

// UpdateBasicInfo updates the user-editable basic fields of a project:
// display name, remote URL, project type, local filesystem path, and the
// project's configured main branch (default_branch). Empty/whitespace-only
// inputs are normalized to "" before write so the caller can rely on the
// same canonical form that's stored at Add time.
//
// name is required (the unique-by-local_path primary identifier is the
// path, so name can repeat — but a blank name is rejected to keep the
// project list readable).
//
// local_path is also required (it's the UNIQUE NOT NULL column); when it
// changes, the new path must point to an existing directory or the update
// is rejected with INVALID_LOCAL_PATH so a typo can't silently orphan the
// project. The new path must also not collide with another active
// project (DUPLICATE_LOCAL_PATH) since projects.local_path has a UNIQUE
// constraint that a hand-edited value must continue to satisfy.
//
// project_type, remote_url, and default_branch are free-form strings and
// accept empty values. An empty default_branch is intentionally allowed:
// the wizard pipeline falls back to "main" when no branch is configured,
// so a user who wants the legacy behaviour just clears the field. Errors
// are prefixed with stable codes the handler maps to HTTP status codes
// (PROJECT_NOT_FOUND / INVALID_NAME / INVALID_LOCAL_PATH /
// DUPLICATE_LOCAL_PATH).
func (s *ProjectService) UpdateBasicInfo(id, name, remoteURL, projectType, localPath, defaultBranch string) error {
	name = strings.TrimSpace(name)
	remoteURL = strings.TrimSpace(remoteURL)
	projectType = strings.TrimSpace(projectType)
	localPath = strings.TrimSpace(localPath)
	defaultBranch = strings.TrimSpace(defaultBranch)

	if name == "" {
		return fmt.Errorf("INVALID_NAME: name is required")
	}
	if localPath == "" {
		return fmt.Errorf("INVALID_LOCAL_PATH: local_path is required")
	}

	// Expand ~ and resolve to an absolute, symlink-cleaned path so the
	// duplicate check (and downstream `cd "$local_path"`) operates on the
	// canonical form the rest of the code expects.
	if strings.HasPrefix(localPath, "~") {
		home, _ := os.UserHomeDir()
		localPath = filepath.Join(home, localPath[1:])
	}
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("INVALID_LOCAL_PATH: cannot resolve path: %w", err)
	}
	if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
		return fmt.Errorf("INVALID_LOCAL_PATH: path does not exist or is not a directory: %s", abs)
	}

	// Load the current row so we can detect a real no-op (avoid
	// touching updated_at on an unchanged edit) and validate uniqueness
	// against the correct scope.
	current, err := s.Get(id)
	if err != nil {
		return err
	}

	if abs != current.LocalPath {
		var dup int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM projects WHERE local_path = ? AND id != ? AND deleted_at IS NULL`,
			abs, id,
		).Scan(&dup); err != nil {
			return fmt.Errorf("DUPLICATE_LOCAL_PATH: %w", err)
		}
		if dup > 0 {
			return fmt.Errorf("DUPLICATE_LOCAL_PATH: another project already uses %s", abs)
		}
	}

	res, err := s.db.Exec(
		`UPDATE projects SET name = ?, remote_url = ?, project_type = ?, local_path = ?, default_branch = ?, updated_at = ?
		 WHERE id = ?`,
		name, remoteURL, projectType, abs, defaultBranch, time.Now(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project not found: %s", id)
	}
	// Cascade: when the project's local_path actually moved (abs != old
	// LocalPath), every requirement's persisted worktree_path now refers to
	// the OLD worktreeRoot, which no longer corresponds to the new
	// local_path. Without this cascade, the next coding/adjust/continue call
	// would either land in a stale directory of the previous host (if still
	// on disk) or fail the WorktreePathMatches drift guard on every entry
	// point. Clearing the columns lets anchorWorktree rebuild a fresh
	// worktree under the new root on the next wizard call.
	//
	// Scope: only requirements of THIS project (project_id = id), and only
	// rows that still carry a non-empty worktree_path/branch_name (avoids
	// a write that hits 0 rows for projects with no prior development).
	// Errors here are logged and swallowed — a partial cascade is better
	// than failing the whole PATCH (the drift guard will catch any leftover
	// rows on the next entry point anyway).
	if abs != current.LocalPath {
		if _, cerr := s.db.Exec(
			`UPDATE requirements SET branch_name = '', worktree_path = '', updated_at = ?
			 WHERE project_id = ? AND (worktree_path != '' OR branch_name != '')`,
			time.Now(), id); cerr != nil {
			log.Printf("[project] %s: cascade-clear requirements worktree failed after local_path move %q → %q: %v",
				id, current.LocalPath, abs, cerr)
		}
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

// UpdateClaudeProjectSlug persists the claude CLI session slug assigned to
// this project. Called by DiscoverAndCacheClaudeProjectSlug on first
// discovery so subsequent reads avoid the on-disk scan.
//
// The slug is whatever directory name Claude CLI created under
// ~/.claude/projects/ when it first ran with this project as CWD (e.g.
// "-Users-f1--novaworkbench-worktrees-novaworkbench-req_xxx"). It is
// project-scoped: a single repo checked out at multiple paths gets one
// canonical slug (the first path Claude was invoked from).
func (s *ProjectService) UpdateClaudeProjectSlug(id, slug string) error {
	if id == "" || slug == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE projects SET claude_project_slug = ?, updated_at = ? WHERE id = ?`,
		slug, time.Now(), id)
	return err
}

// DiscoverAndCacheClaudeProjectSlug scans <claudeSessionHome>/projects/ for
// a subdirectory whose decoded slug matches localPath. Returns ("", nil)
// when no match is found — that is normal for a brand-new project that has
// never been run through Claude locally yet (the Agent Server path would
// also be in this state until the first start-coding locally).
//
// Fallback semantics: when NO slug on disk matches localPath (e.g. the
// project was never coded locally so the directory never existed), the
// function falls back to EncodeClaudeSlug(localPath). The SFTP upload step
// in runRemoteCoding creates the remote dir under this slug; if the local
// CLI later writes to that slug too, the next upload picks it up
// transparently. We persist the fallback slug via UpdateClaudeProjectSlug
// only when we found a real on-disk match — caching a never-written slug
// would just guarantee a stale row on the next read.
//
// On a match the slug is persisted to projects.claude_project_slug via
// UpdateClaudeProjectSlug. A write error is logged but does not fail the
// read — the caller can still use the returned slug for this request.
func (s *ProjectService) DiscoverAndCacheClaudeProjectSlug(id, localPath string) (string, error) {
	if id == "" || localPath == "" {
		return "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	claudeHome := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeHome == "" {
		claudeHome = os.Getenv("NOVA_CLAUDE_HOME")
	}
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}
	root := filepath.Join(claudeHome, "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return util.EncodeClaudeSlug(localPath), nil
		}
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if util.MatchSlugToPath(e.Name(), localPath) {
			if uerr := s.UpdateClaudeProjectSlug(id, e.Name()); uerr != nil {
				log.Printf("[project] cache claude_project_slug %s: %v", id, uerr)
			}
			return e.Name(), nil
		}
	}
	// No on-disk match: return the encoded slug as a best-effort fallback so
	// the SFTP uploader has a directory name to write to. We deliberately do
	// NOT persist this fallback — it might never exist on disk, and caching
	// it would just guarantee a stale row.
	return util.EncodeClaudeSlug(localPath), nil
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

// SetCommitLangOverride persists the user's pinned language override for
// commits / push / PR in this project. Pass override="" to clear the pin and
// revert to the auto-detected value (or the default "en" when nothing has
// been detected yet). When the override is non-empty, commit_lang_source is
// flipped to "user"; when cleared, it reverts to "auto" so the UI badge
// matches the resolved effective value. Returns the refreshed project row
// (handlers can echo it directly to the client).
//
// Validated values: "" / "zh" / "en" / "mixed". Anything else returns
// INVALID_OVERRIDE so the handler can map to 400 BAD_REQUEST without
// scraping the message.
func (s *ProjectService) SetCommitLangOverride(id, override string) (*model.Project, error) {
	override = strings.TrimSpace(override)
	if override != "" && override != "zh" && override != "en" && override != "mixed" {
		return nil, fmt.Errorf("INVALID_OVERRIDE: override must be empty, zh, en, or mixed")
	}
	now := time.Now()
	// Effective source tracks where the *currently effective* language value
	// comes from: "user" when override is set, "auto" when the override is
	// cleared and we fall back to whatever the scanner last wrote.
	source := "auto"
	if override != "" {
		source = "user"
	}
	res, err := s.db.Exec(
		`UPDATE projects SET commit_lang_override = ?, commit_lang_source = ?, commit_lang_updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		override, source, now, id)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, fmt.Errorf("project not found: %s", id)
	}
	return s.Get(id)
}

// ClearCommitLangOverride removes any user-pinned language override; the
// effective language falls back to the auto-detected value (or "en" if
// nothing has been detected yet). Thin wrapper around SetCommitLangOverride
// for callers that prefer the explicit semantic.
func (s *ProjectService) ClearCommitLangOverride(id string) (*model.Project, error) {
	return s.SetCommitLangOverride(id, "")
}
