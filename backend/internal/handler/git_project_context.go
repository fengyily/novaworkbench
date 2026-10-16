// Assembler for the project-level git context — bridges the project's
// stored Git config (commit_mode / commit_script / commit_lang /
// platform_type / platform_token_id / remote_url) and the prompt.GitProjectContext
// shape prompt.BuildGitProjectBlock consumes. Two functions:
//
//   - gitProjectBlock  → returns the full "本项目 Git 约定" block (Chinese
//     Markdown, append-after-GitCommitConvention shape), built fresh from
//     the DB each time. Any error path degrades to "only GitCommitConvention
//     is returned" so a broken project row never aborts a coding run (the
//     pipeline's "配置坏了不阻塞" contract — same one assembleGitCredEnv
//     and lookupGitIdentity already implement on the env side).
//
//   - platformRunEnv    → returns the "KEY=VALUE" pairs to splice into the
//     child process env: NOVA_PLATFORM_TYPE, NOVA_PLATFORM_API_BASE,
//     NOVA_PLATFORM_TOKEN (+ platform-native aliases like GH_TOKEN), and
//     the three NOVA_COMMIT_* vars the project's commit_script expects.
//     SSH remote or unbound token → only the NOVA_COMMIT_* triple is
//     returned so a Shell script can still rely on its env contract.
//
// Call sites: execStartCoding / AdjustCoding / ContinueCoding (wizard_coding.go),
// Run / ExecuteOrchestratedChild / buildPushSubTaskPrompt (sub_task_runner.go /
// wizard_orchestration.go / merge.go), and the two wizard_remote.go
// BuildRemoteEnvPairsWithConfig calls (so platformRunEnv's values reach the
// remote worker through the same --settings env block).
//
// Token safety: the function returns the token value (it's env, not a
// prompt), but every caller of platformRunEnv is then responsible for
// pushing the result through job.SetSecrets before launching the claude
// child so the log redaction in store.Job.Append blanks the secret.

package handler

import (
	"log"
	"net/url"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
)

// gitProjectBlock returns the per-project Git context block. The shape:
//
//	prompt.GitCommitConvention + "\n" + prompt.BuildGitProjectBlock(c, forPR)
//
// (preceded by nothing / succeeded by a trailing newline so the concatenation
// with whatever the caller already had in the task prompt stays clean).
//
// reqRow may be nil (the wizard quick-start path has no requirement row); in
// that case we read the project row by ID. projectSvc / platformSvc may be
// nil for tests / legacy deployments — every step below degrades to empty,
// and the function still returns at least GitCommitConvention so callers
// never see an empty tail block.
//
// forPR selects whether PRLangRules is included (the auto / manual push+PR
// prompt; not the generic coding prompt). Pass false for coding / sub-task
// development prompts where the agent never writes a PR body.
//
// Failure policy (matches lookupGitIdentity / assembleGitCredEnv):
//
//   - project row missing → returns GitCommitConvention unchanged.
//   - platform token row missing (deleted out from under the project) →
//     render the "no token" wording instead of the canonical "凭据已注入"
//     wording, so the prompt stays truthful.
//   - remote_url unparseable / SSH-ambiguous → falls back to assume SSH so
//     we never render "Token 已绑定但 git push 用 GIT_ASKPASS" on an actual
//     SSH remote (the symptom this guard exists to prevent).
//   - style hint / commit mode empty / script too long → graceful omissions.
//
// The function never panics. Any unexpected DB error is logged and the
// "only GitCommitConvention" path is taken instead.
func gitProjectBlock(
	projectSvc *service.ProjectService,
	platformSvc *service.PlatformTokenService,
	reqRow *model.Requirement,
	forPR bool,
) string {
	if projectSvc == nil || reqRow == nil {
		return promptpkg.GitCommitConvention
	}
	proj, err := projectSvc.Get(reqRow.ProjectID)
	if err != nil || proj == nil {
		return promptpkg.GitCommitConvention
	}

	ctx, err := buildGitProjectContext(proj, platformSvc)
	if err != nil {
		log.Printf("[git-context] %s: build context failed: %v — falling back to GitCommitConvention only", reqRow.ID, err)
		return promptpkg.GitCommitConvention
	}

	block := promptpkg.BuildGitProjectBlock(ctx, forPR)
	// Always keep GitCommitConvention at the top — the per-project block is
	// "in addition to" the universal no-AI-signature / no-typed-credentials
	// rule, never a replacement. Concat with a blank line so downstream
	// parsers that look for "## Git 提交与推送规范" stay happy.
	return promptpkg.GitCommitConvention + "\n" + block
}

// platformRunEnv returns the env pairs to splice into the child process /
// remote --settings block. Honors the same "miss = no-op" contract as
// gitCredentialEnv / assembleGitCredEnv: an SSH remote or unbound token
// still gets the NOVA_COMMIT_* triple (so a project's commit_script can
// rely on the contract even when token injection is skipped).
//
// The returned slice may be empty (unbound project, no script context);
// callers should append unconditionally. Tokens with non-empty value MUST
// be pushed through job.SetSecrets before the subprocess spawns.
//
// reqRow / project may be nil; missing → empty slice.
//
// workDir / projectPath are passed in for the NOVA_COMMIT_* triple. When
// both are empty the vars still render so a project's script can rely on
// its contract; empty values mean "未解析" but the script still exits
// cleanly.
func platformRunEnv(
	projectSvc *service.ProjectService,
	platformSvc *service.PlatformTokenService,
	reqRow *model.Requirement,
	workDir string,
	projectPath string,
) []string {
	if projectSvc == nil || reqRow == nil {
		return nil
	}
	proj, err := projectSvc.Get(reqRow.ProjectID)
	if err != nil || proj == nil {
		return nil
	}

	var out []string

	// Token-bound paths get the platform env (and the platform-native
	// aliases that gh / glab / tea read automatically). Unbound / SSH paths
	// skip these so the prompt doesn't lie about "已注入凭据".
	if proj.PlatformTokenID != "" && platformSvc != nil && !looksLikeSSH(proj.RemoteURL) {
		tok, terr := platformSvc.Get(proj.PlatformTokenID)
		if terr == nil && tok != nil {
			out = append(out,
				"NOVA_PLATFORM_TYPE="+tok.Platform,
				"NOVA_PLATFORM_API_BASE="+apiBaseURL(tok),
				"NOVA_PLATFORM_TOKEN="+tok.Token,
			)
			// Platform-native aliases that `gh` / `glab` / `tea` / curl read
			// without extra flags. README-bound tool recommendations in
			// GitProjectContext.PlatformType apply.
			switch tok.Platform {
			case "github":
				out = append(out, "GH_TOKEN="+tok.Token)
				// GitHub Enterprise: GH_HOST switches `gh api` / `gh repo view`
				// to the enterprise host. Only when BaseURL is set and not the
				// dotcom default.
				if host := hostFromBaseURL(tok.BaseURL); host != "" && host != "api.github.com" {
					out = append(out, "GH_HOST="+host)
				}
			case "gitlab":
				out = append(out, "GITLAB_TOKEN="+tok.Token)
				if host := hostFromBaseURL(tok.BaseURL); host != "" {
					out = append(out, "GITLAB_HOST="+host)
				}
			case "gitea":
				out = append(out, "GITEA_TOKEN="+tok.Token)
				if host := hostFromBaseURL(tok.BaseURL); host != "" {
					out = append(out, "GITEA_HOST="+host)
				}
			}
		}
		// Token deleted / unbound → no platform env, prompt will downgrade
		// to "no token available" wording. No error — same "miss = no-op"
		// contract as lookupGitIdentity.
	}

	// NOVA_COMMIT_* triple — always emitted (even on SSH / unbound), so the
	// project's commit_script can rely on a stable contract.
	if workDir != "" || projectPath != "" {
		out = append(out,
			"NOVA_COMMIT_REQUIREMENT_ID="+reqRow.ID,
			"NOVA_COMMIT_PROJECT_DIR="+projectPath,
			"NOVA_COMMIT_WORKTREE="+workDir,
		)
	} else {
		// Even with no workdir, expose the reqID so a script that just
		// needs to stamp an ID can do so.
		out = append(out, "NOVA_COMMIT_REQUIREMENT_ID="+reqRow.ID)
	}

	return out
}

// buildGitProjectContext turns a loaded Project row + (optional) token row
// into a fully-populated prompt.GitProjectContext. Errors come from the
// underlying Get / token row lookup; the caller decides whether to degrade.
func buildGitProjectContext(proj *model.Project, platformSvc *service.PlatformTokenService) (promptpkg.GitProjectContext, error) {
	if proj == nil {
		return promptpkg.GitProjectContext{}, errMissingProject
	}
	ctx := promptpkg.GitProjectContext{
		PlatformType: proj.PlatformType,
		RemoteHost:   remoteHostOnly(proj.RemoteURL),
		SSHRemote:    looksLikeSSH(proj.RemoteURL),
		CommitMode:   proj.CommitMode,
		CommitScript: proj.CommitScript,
	}
	commitLang := service.ResolveCommitLang(proj.CommitLang, proj.CommitLangOverride)
	ctx.StyleHint = service.StyleHint(commitLang, "")
	ctx.PRLangRules = service.PRLangRules(commitLang)

	if proj.PlatformTokenID != "" && platformSvc != nil {
		tok, terr := platformSvc.Get(proj.PlatformTokenID)
		if terr == nil && tok != nil {
			ctx.TokenBound = true
			ctx.TokenName = tok.Name
			ctx.APIBase = apiBaseURL(tok)
		}
		// Token lookup failure (deleted / decrypt error): TokenBound stays
		// false so the prompt renders "未配置 / 失效" — never pretend the
		// secret is available when Get returned an error.
	}
	return ctx, nil
}

// remoteHostOnly returns just the host:port of a remote URL, with any
// userinfo stripped. Empty when the URL is empty / unparseable / SSH-form
// ("git@github.com:owner/repo"). Used to talk about "the remote" in the
// prompt without ever accidentally leaking a token-bearing userinfo.
func remoteHostOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// SSH scp-like form: git@github.com:owner/repo.git → return the host only.
	if i := strings.Index(raw, "@"); i > 0 && !strings.Contains(raw, "://") {
		rest := raw[i+1:]
		if j := strings.Index(rest, ":"); j > 0 && !strings.Contains(rest[:j], "/") {
			return rest[:j]
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// looksLikeSSH reports whether the URL is git/scp-style SSH. The same
// heuristic GitCredentialService.BuildEnv uses ("ssh://" / "git@host:"")
// so a project that gets askpass skipped also gets the SSH wording in
// its prompt. Keeping both paths on the same predicate is what stops the
// "askpass-less push on an SSH remote" prompt from being accidentally
// contradictory.
func looksLikeSSH(raw string) bool {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "ssh://") || strings.HasPrefix(s, "git+ssh://") {
		return true
	}
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

// apiBaseURL returns the canonical REST API base for a token. Mirrors the
// service-layer resolveGitHubAPIBase / GitLab / Gitea logic — the helper
// re-derives the host from BaseURL rather than reaching across the service
// boundary because callers here only need the URL string for the prompt
// (no auth calls).
func apiBaseURL(tok *model.PlatformToken) string {
	if tok == nil {
		return ""
	}
	switch tok.Platform {
	case "github":
		host := hostFromBaseURL(tok.BaseURL)
		if host == "" || host == "api.github.com" {
			return "https://api.github.com"
		}
		// GitHub Enterprise: BaseURL is the web URL like https://github.acme.com
		// → api host is the same host on /api/v3.
		return strings.TrimRight(host, "/") + "/api/v3"
	case "gitlab":
		return strings.TrimRight(strings.TrimSpace(tok.BaseURL), "/") + "/api/v4"
	case "gitea":
		return strings.TrimRight(strings.TrimSpace(tok.BaseURL), "/") + "/api/v1"
	default:
		return strings.TrimSpace(tok.BaseURL)
	}
}

// hostFromBaseURL strips scheme / path off a base URL and returns just the
// host. Used to populate GH_HOST / GITLAB_HOST / GITEA_HOST aliases.
func hostFromBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// errMissingProject is returned by buildGitProjectContext when a caller
// passes a nil project. Kept as a sentinel so the caller can log a single
// "block skipped" line instead of distinguishing errMissProject from
// errEmptyLocalPath.
var errMissingProject = errStr("project row is nil")

type errStr string

func (e errStr) Error() string { return string(e) }
