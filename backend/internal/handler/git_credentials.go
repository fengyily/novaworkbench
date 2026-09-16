package handler

import (
	"log"
	"os"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// gitCredentialEnv resolves the project's committer identity + platform token
// and returns (a) env pairs to layer onto the claude subprocess and (b) a
// cleanup func removing the temp askpass script. Empty env + no-op cleanup
// when the project has no HTTPS remote / no token (preserves ambient-cred
// behaviour).
//
// Identity env (GIT_AUTHOR_* / GIT_COMMITTER_*) is always returned when the
// platform token has name/email — these are safe (no secrets), so callers
// don't need to gate them.
//
// Credential env (GIT_ASKPASS / GIT_TERMINAL_PROMPT) is only returned for
// HTTPS remotes with a non-empty token. The askpass script writes the token
// to git's stdin on demand; the token never appears in the -p prompt, in
// shell history, or in `.git/config`. The script is created with 0700 perms
// and the returned cleanup MUST be deferred by the caller to remove it.
//
// On any failure (project lookup, token lookup, temp file create/chmod) we
// log a precise warning and degrade to "no credential env, ambient fallback"
// — we never panic and never echo the token into the log stream.
func gitCredentialEnv(
	projectSvc *service.ProjectService,
	platformSvc *service.PlatformTokenService,
	reqRow *model.Requirement,
) ([]string, func()) {
	noop := func() {}

	// Identity first — safe to always attempt. Reuses lookupGitIdentity so
	// the merge flow (-c user.name=...) and this env-injection path agree.
	var env []string
	if name, email := lookupGitIdentity(projectSvc, platformSvc, reqRow); name != "" || email != "" {
		if name != "" {
			env = append(env, "GIT_AUTHOR_NAME="+name, "GIT_COMMITTER_NAME="+name)
		}
		if email != "" {
			env = append(env, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL="+email)
		}
	}

	// Below: credential injection. Each early-out must not lose the identity
	// env we already accumulated; we just append more or return what we have.
	if projectSvc == nil || platformSvc == nil || reqRow == nil {
		return env, noop
	}
	project, err := projectSvc.Get(reqRow.ProjectID)
	if err != nil || project == nil {
		if err != nil {
			log.Printf("[git-credentials] project lookup failed for %s: %v", reqRow.ProjectID, err)
		}
		return env, noop
	}
	if project.PlatformTokenID == "" || strings.TrimSpace(project.RemoteURL) == "" {
		return env, noop
	}
	tok, err := platformSvc.Get(project.PlatformTokenID)
	if err != nil || tok == nil || tok.Token == "" {
		if err != nil {
			log.Printf("[git-credentials] platform token lookup failed for %s: %v", project.PlatformTokenID, err)
		}
		return env, noop
	}
	if isSSHRemoteURL(project.RemoteURL) {
		// SSH auth is key-based; tokens are irrelevant here.
		return env, noop
	}

	// HTTPS path. Build (username, password) the same way injectCredentials
	// does so the askpass answer is equivalent to what the remote path would
	// have put into the origin URL.
	username, password := askpassCredentials(tok)
	if username == "" || password == "" {
		return env, noop
	}

	scriptPath, cleanup, writeErr := writeAskpassScript(username, password)
	if writeErr != nil {
		log.Printf("[git-credentials] askpass script setup failed: %v", writeErr)
		return env, noop
	}

	env = append(env,
		"GIT_ASKPASS="+scriptPath,
		"GIT_TERMINAL_PROMPT=0",
	)
	return env, cleanup
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
		// github / gitea / unknown-HTTPS: token-as-username, empty password.
		// git's askpass protocol only requires the password field; some
		// platforms accept the token in either slot, so we leave password
		// empty and let askpass still emit the token as username fallback.
		return tok.Token, ""
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
	// $, `, \, ", and most other shell metacharacters.
	escaped := strings.ReplaceAll(password, "'", `'\'\''`)
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

// isSSHRemoteURL is a local duplicate of service.isSSHRemote (which is
// unexported). The check is small and stable, so a verbatim copy is cheaper
// than promoting the original. Kept in lockstep with service/project.go.
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