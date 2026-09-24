package handler

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/novaworkbench/backend/internal/service"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
)

// Remote counterpart of git_credentials.go.
//
// The local path re-derives the project's platform token on EVERY run (see
// gitCredentialEnv → GIT_ASKPASS), so rotating the token in 设置 → 平台 Token
// takes effect immediately. The Agent-server path used to embed the token
// exactly once — in the `git clone <originURL>` that creates
// /tmp/nova-agent/<projectID>/base — and never touched it again: the clone is
// skipped whenever that scratch directory already exists, so the `.git/config`
// there keeps whatever credential was current when the directory was first
// created. A project re-pointed from GitLab to GitHub (or whose PAT was simply
// rotated) then pushes with a stale, wrong-platform token, which is exactly the
// `glpat-` against github.com failure this file fixes.
//
// Secondary source of wrong credentials: the agent host's own credential
// helper / ~/.git-credentials, which Nova never provisioned and cannot audit.
// When we do have a project token we disable the helper at the repo level so
// the origin userinfo is the only credential in play.

// buildRemoteOriginScript renders the shell body that re-points the scratch
// repo's `origin` at originURL. Split out from ensureRemoteOrigin (and shaped
// like buildRemoteSyncScript) so the quoting can be unit-tested without SSH.
//
// disableHostHelper controls the `credential.helper ""` line: we only sever
// the agent host's ambient credentials when Nova has a token of its own to
// offer. Without one, the host helper is the only thing that could make a
// push work, so cutting it would be a strict regression.
func buildRemoteOriginScript(baseRepo, originURL string, disableHostHelper bool) string {
	qRepo := shellQuoteSingle(baseRepo)
	qURL := shellQuoteSingle(originURL)
	lines := []string{
		"#!/bin/sh",
		"set -eu",
	}
	if disableHostHelper {
		// An empty helper value resets the inherited (global / system) helper
		// list for this repo, so ~/.git-credentials can no longer supply a
		// token for the origin host.
		lines = append(lines, "git -C "+qRepo+" config --local credential.helper ''")
	}
	lines = append(lines,
		// `remote get-url` exists since git 2.7; on older git it fails and we
		// fall through to `remote add`, which is the correct branch when there
		// genuinely is no origin.
		"if git -C "+qRepo+" remote get-url origin >/dev/null 2>&1; then",
		"  git -C "+qRepo+" remote set-url origin "+qURL,
		"else",
		"  git -C "+qRepo+" remote add origin "+qURL,
		"fi",
		// The URL we just wrote carries the token in cleartext (same as what
		// `git clone <url-with-token>` has always left behind); tighten the
		// file so other accounts on the agent host cannot read it.
		"chmod 600 "+shellQuoteSingle(baseRepo+"/.git/config")+" 2>/dev/null || true",
	)
	return strings.Join(lines, "\n") + "\n"
}

// ensureRemoteOrigin re-points the agent host's scratch repo at the project's
// CURRENT platform credentials before every remote run. Callers invoke it
// right after the (possibly skipped) `git clone` and before the sync script,
// so both a fresh clone and a months-old scratch directory end up with the
// same, current origin.
//
// authed=false means the project has no usable HTTPS token (no
// platform_token_id, or an SSH remote). We still normalise the URL, but we
// leave the host's own credentials intact and warn — read-only / public-repo
// runs are legitimate, so this must not abort the run.
//
// Errors are returned rather than fatal: the caller downgrades them to a job
// warning, matching provisionRemoteGitIdentity's contract.
func ensureRemoteOrigin(ctx context.Context, client *gossh.Client, job *store.Job, baseRepo, originURL string, authed bool) error {
	if client == nil || baseRepo == "" || strings.TrimSpace(originURL) == "" {
		return nil
	}
	if !authed && job != nil {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 项目未绑定平台 Token（或 remote 为 SSH），Agent 服务器上的 git push 将依赖主机自身凭据，很可能失败"})
	}
	var out bytes.Buffer
	exit, err := client.RunScript(ctx, buildRemoteOriginScript(baseRepo, originURL, authed), "git-origin", nil, &out)
	if exit != 0 || err != nil {
		return fmt.Errorf("刷新 Agent 服务器 origin 凭据失败（exit=%d）：%s%s",
			exit, redactRemoteScriptOutput(strings.TrimSpace(out.String())), errString(err))
	}
	if job != nil {
		job.Append(store.LogLine{Type: "message", Content: "🔑 已按项目平台 Token 刷新 Agent 服务器 origin 凭据: " + redactOriginForLog(originURL)})
	}
	return nil
}

// remoteOriginAuthed reports whether Nova has an HTTPS token it can inject
// into the remote origin URL for this project. Mirrors the gate
// gitCredentialEnv applies on the local path (token present + non-SSH remote)
// so both surfaces agree on when a credential is actually in play.
func remoteOriginAuthed(projectSvc *service.ProjectService, projectID string) bool {
	if projectSvc == nil || projectID == "" {
		return false
	}
	p, err := projectSvc.Get(projectID)
	if err != nil || p == nil {
		return false
	}
	return p.PlatformTokenID != "" && !isSSHRemoteURL(p.RemoteURL)
}

// remoteURLUserinfo matches the `scheme://userinfo@` prefix of any URL in a
// blob of text. Unlike service.userinfoPattern it does not require a
// `user:password` split, because github / gitea remotes carry the token as a
// bare username (`https://<token>@github.com/...`).
var remoteURLUserinfo = regexp.MustCompile(`([a-z][a-z0-9+\-.]*://)[^/\s@]+@`)

// redactRemoteScriptOutput strips credentials from every URL in git's output
// before it reaches a job log. redactOriginForLog handles a single, well-formed
// origin URL; script output can echo the URL several times (and in either
// userinfo shape), so error paths go through this instead.
func redactRemoteScriptOutput(s string) string {
	return remoteURLUserinfo.ReplaceAllString(s, "$1<redacted>@")
}
