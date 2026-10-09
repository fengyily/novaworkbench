package handler

import (
	"context"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// gitCredSvc is the package-level handle to the service-layer credential
// service. main.go injects it via SetGitCredentialService at startup; when
// it's nil (e.g. unit tests that don't wire the dependency) the credential
// helpers below degrade to "no env, no-op cleanup" so call sites never
// panic and the host's ambient credentials stay in effect.
var gitCredSvc *service.GitCredentialService

// SetGitCredentialService injects the credential service used by the
// package-level helpers in this file. Called once from main.go after
// service.NewGitCredentialService returns; safe to call again to swap the
// service in tests.
func SetGitCredentialService(svc *service.GitCredentialService) {
	gitCredSvc = svc
}

// gitCredentialEnv resolves the project's platform token and returns the
// GIT_ASKPASS / GIT_TERMINAL_PROMPT env pairs (and a cleanup func that
// removes the temp askpass script) for any HTTPS git operation spawned by
// the caller.
//
// The committer identity env (GIT_AUTHOR_* / GIT_COMMITTER_*) is NOT
// included here — callers that need the identity should call
// lookupGitIdentity separately (the same way assembleGitCredEnv and the
// merge flow do). This split mirrors the service-layer contract: the
// credential service is only responsible for auth material; identity is a
// separate concern.
//
// Signature is preserved verbatim from the original in-file implementation
// so the existing call sites (sub_task_runner / merge / wizard_coding /
// wizard_orchestration) keep compiling without business-code changes.
func gitCredentialEnv(
	projectSvc *service.ProjectService,
	platformSvc *service.PlatformTokenService,
	reqRow *model.Requirement,
) ([]string, func()) {
	noop := func() {}
	if gitCredSvc == nil {
		return nil, noop
	}
	// The original function had no ctx parameter; pass a background ctx to
	// keep the call signature stable. GitCredentialService.BuildEnv does not
	// currently use ctx — it's reserved for a future timeout path.
	env, cleanup, _ := gitCredSvc.BuildEnvForRequirement(context.Background(), reqRow)
	return env, cleanup
}

// gitCredentialEnvForProject is the projectID-keyed twin of gitCredentialEnv.
// It exists so worktree.go (and any other handler that only has a projectID
// in scope) can inject the project's platform token without fabricating a
// placeholder *model.Requirement.
//
// The projectSvc / platformSvc parameters are accepted for API symmetry
// with gitCredentialEnv — the service layer reads them through its own
// dependency, so they're ignored here, but keeping them in the signature
// lets us swap to a different lookup path without touching call sites.
func gitCredentialEnvForProject(
	projectSvc *service.ProjectService,
	platformSvc *service.PlatformTokenService,
	projectID string,
) ([]string, func()) {
	noop := func() {}
	if gitCredSvc == nil {
		return nil, noop
	}
	env, cleanup, _ := gitCredSvc.BuildEnv(context.Background(), projectID)
	return env, cleanup
}
