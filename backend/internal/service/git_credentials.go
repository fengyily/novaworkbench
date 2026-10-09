package service

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// GitCredentialService owns the askpass / identity injection protocol used
// for HTTPS git operations. It is a port of the original helper that lived
// in internal/handler/git_credentials.go — the logic is byte-for-byte the
// same, just relocated to the service layer so the wizard's sync helpers
// (syncBaseBranchService, SyncToOriginBase) can reuse it without reaching
// back into the handler package.
//
// Contract (mirrors the original gitCredentialEnv):
//
//   - BuildEnv / BuildEnvForRequirement NEVER panic, NEVER echo the token
//     into the log stream, and NEVER propagate credential-lookup misses as
//     errors. Any soft failure (project missing, token missing, SSH remote,
//     temp-file create failed) degrades to "empty env + no-op cleanup +
//     nil error" so the caller can simply skip credential injection and
//     fall back to the host's ambient credentials.
//   - ctx is currently unused but reserved for a future timeout path; the
//     parameter is already in the signature so callers don't need to change
//     when that lands.
//   - The returned cleanup func MUST be deferred by the caller. It removes
//     the temp askpass script and is safe to call multiple times.
//   - The env slice is meant to be appended AFTER os.Environ() (the service
//     intentionally does not pre-merge with the host env — that's the
//     caller's responsibility since the caller also owns the os/exec.Cmd).
type GitCredentialService struct {
	db *db.DB
}

// NewGitCredentialService constructs a service backed by the given *db.DB.
// The DB is used for direct platform_tokens / projects lookups so the
// service stays decoupled from ProjectService / PlatformTokenService
// (which would create a cycle through the wizard handler today).
func NewGitCredentialService(db *db.DB) *GitCredentialService {
	return &GitCredentialService{db: db}
}

// BuildEnv resolves the project's platform token and returns:
//
//   - env: "KEY=VALUE" pairs to layer onto the *exec.Cmd.Env (in addition
//     to os.Environ()). Always includes "GIT_TERMINAL_PROMPT=0" when a
//     credential was actually wired up; the askpass script path is also
//     appended under "GIT_ASKPASS=...".
//   - cleanup: a function that removes the temp askpass script. MUST be
//     deferred by the caller. Idempotent and safe to call on the no-op
//     path (returns a no-op func).
//   - err: reserved for hard infrastructure errors. Credential-lookup
//     misses are NOT errors — they return (nil, noop, nil).
func (s *GitCredentialService) BuildEnv(ctx context.Context, projectID string) ([]string, func(), error) {
	noop := func() {}
	_ = ctx // reserved; see type docstring.
	if s == nil || s.db == nil {
		return nil, noop, nil
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, noop, nil
	}

	project, tok, err := s.lookupProjectToken(projectID)
	if err != nil {
		// lookupProjectToken already logged the precise reason. Degrade
		// silently to ambient fallback.
		return nil, noop, nil
	}
	if project == nil || tok == nil {
		return nil, noop, nil
	}
	if strings.TrimSpace(project.RemoteURL) == "" {
		return nil, noop, nil
	}
	if isSSHRemoteURL(project.RemoteURL) {
		// SSH auth is key-based; tokens are irrelevant here.
		return nil, noop, nil
	}

	// HTTPS path. Build (username, password) the same way the remote
	// injection path does so the askpass answer is equivalent to what the
	// remote URL would have put into the origin basic-auth userinfo.
	username, password := askpassCredentials(tok)
	if username == "" || password == "" {
		return nil, noop, nil
	}

	scriptPath, cleanup, writeErr := writeAskpassScript(username, password)
	if writeErr != nil {
		log.Printf("[git-credentials] askpass script setup failed: %v", writeErr)
		return nil, noop, nil
	}

	env := []string{
		"GIT_ASKPASS=" + scriptPath,
		"GIT_TERMINAL_PROMPT=0",
	}
	return env, cleanup, nil
}

// BuildEnvForRequirement is a thin convenience wrapper around BuildEnv that
// pulls the projectID off a *model.Requirement. The reqRow is allowed to be
// nil (returns no-op); an empty ProjectID is treated identically.
func (s *GitCredentialService) BuildEnvForRequirement(ctx context.Context, reqRow *model.Requirement) ([]string, func(), error) {
	noop := func() {}
	if reqRow == nil {
		return nil, noop, nil
	}
	return s.BuildEnv(ctx, reqRow.ProjectID)
}

// lookupProjectToken resolves a projectID to its (project, platform_token)
// pair in a single SQL hop. Soft-deleted projects are skipped (matching
// service.ProjectService.Get's `deleted_at IS NULL` filter). The two
// returns are independent: a project may exist without a platform token
// configured (project.PlatformTokenID == "") — in that case we return
// (project, nil, nil) so the caller can decide whether to abort, but the
// current BuildEnv path treats that case as a no-op.
func (s *GitCredentialService) lookupProjectToken(projectID string) (*model.Project, *model.PlatformToken, error) {
	// First hop: project row. We need the project even when the platform
	// token is missing so the caller can inspect RemoteURL for the SSH /
	// HTTPS decision.
	var (
		project  model.Project
		tokenID  sql.NullString
		token    sql.NullString
		platform sql.NullString
		baseURL  sql.NullString
		gitUser  sql.NullString
		gitEmail sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT p.id, p.remote_url, p.platform_token_id,
		        t.token, t.platform, t.base_url, t.git_user_name, t.git_user_email
		 FROM projects p
		 LEFT JOIN platform_tokens t ON t.id = p.platform_token_id
		 WHERE p.id = ? AND p.deleted_at IS NULL`, projectID,
	).Scan(
		&project.ID, &project.RemoteURL, &tokenID,
		&token, &platform, &baseURL, &gitUser, &gitEmail,
	)
	if err == sql.ErrNoRows {
		log.Printf("[git-credentials] project lookup failed for %s: not found", projectID)
		return nil, nil, nil
	}
	if err != nil {
		log.Printf("[git-credentials] project lookup failed for %s: %v", projectID, err)
		return nil, nil, err
	}
	if !tokenID.Valid || strings.TrimSpace(tokenID.String) == "" {
		// Project exists but has no platform token bound. Caller will
		// degrade to ambient fallback.
		return &project, nil, nil
	}
	if !token.Valid || strings.TrimSpace(token.String) == "" {
		// Token row was deleted out from under us. Caller will degrade.
		log.Printf("[git-credentials] platform token %s has empty token value", tokenID.String)
		return &project, nil, nil
	}

	tok := &model.PlatformToken{
		ID:          tokenID.String,
		Platform:    platform.String,
		BaseURL:     baseURL.String,
		Token:       token.String,
		GitUserName: gitUser.String,
		GitUserEmail: gitEmail.String,
	}
	return &project, tok, nil
}

// askpassCredentials mirrors injectCredentials (service/project.go) for the
// HTTPS basic-auth userinfo split. github / gitea use the token as the
// username with empty password; gitlab prefers `<git_user_name>:<token>`
// and falls back to `oauth2:<token>` when no username is set.
func askpassCredentials(tok *model.PlatformToken) (string, string) {
	if tok == nil {
		return "", ""
	}
	switch tok.Platform {
	case "gitlab":
		if tok.GitUserName != "" {
			return tok.GitUserName, tok.Token
		}
		return "oauth2", tok.Token
	default:
		// github / gitea / unknown-HTTPS: emit the token on both the username
		// and password fields. The askpass script writes the password to
		// stdout, so making the password the token means the script outputs
		// the token (which is also the basic-auth username). git's askpass
		// protocol matches the username supplied via the URL/remote against
		// the credential answer in any order, so this works for both git's
		// "Username for ..." and "Password for ..." prompts.
		return tok.Token, tok.Token
	}
}

// writeAskpassScript creates a 0700-permission shell script under the OS
// temp dir that prints the given password when invoked. git calls this
// script via GIT_ASKPASS and reads stdout as the credential.
//
// We use `printf '%s'` (not `echo`) so backslash / -n / -e sequences inside
// the token don't get mangled by the shell, and we single-quote-escape the
// password so embedded `$` / backticks / `'` / `\` are passed through
// literally.
func writeAskpassScript(username, password string) (string, func(), error) {
	noop := func() {}
	// We emit the password on stdout; git's askpass protocol treats stdout
	// as the credential answer (any username provided via the prompt is
	// matched by git). For github / gitea the username IS the token; for
	// gitlab the username is set on the URL/remote separately, so the
	// askpass answer is just the password.
	_ = username // currently unused on stdout; reserved for future multi-line askpass

	f, err := os.CreateTemp("", "nova-askpass-*.sh")
	if err != nil {
		return "", noop, err
	}
	scriptPath := f.Name()

	// Single-quote escape: replace ' with '\'' (close, escaped, reopen).
	// This is the standard POSIX-safe form and is immune to expansion of
	// $, `, \, ", and most other shell metacharacters. The replacement is
	// the 4-character sequence `'\'` `''` — written as a raw string the
	// backticks contain the literal 4 bytes: single-quote, backslash,
	// single-quote, single-quote. (A 6-char raw like `'\'\''` would emit
	// an extra backslash that the shell re-interprets as another escape,
	// producing two single quotes on stdout instead of one.)
	escaped := strings.ReplaceAll(password, "'", `'\''`)
	content := "#!/bin/sh\nprintf '%s' '" + escaped + "'\n"
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(scriptPath)
		return "", noop, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(scriptPath)
		return "", noop, err
	}
	if err := os.Chmod(scriptPath, 0o700); err != nil {
		_ = os.Remove(scriptPath)
		return "", noop, err
	}

	cleanup := func() { _ = os.Remove(scriptPath) }
	return scriptPath, cleanup, nil
}

// isSSHRemoteURL returns true when raw looks like an SSH-style remote URL.
// Three forms are recognised: `ssh://...`, `git+ssh://...`, and the
// scp-like `user@host:path` (where the path segment contains no `/`).
// URLs that already carry a scheme other than `ssh*` are always treated
// as non-SSH regardless of any `@` in the userinfo.
func isSSHRemoteURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "ssh://") || strings.HasPrefix(s, "git+ssh://") {
		return true
	}
	// scp-like `user@host:path`. If the URL already has a scheme, it isn't
	// this form regardless of @.
	if strings.Contains(s, "://") {
		return false
	}
	if i := strings.Index(s, "@"); i > 0 {
		rest := s[i+1:]
		if j := strings.Index(rest, ":"); j > 0 && !strings.Contains(rest[:j], "/") {
			return true
		}
	}
	return false
}
