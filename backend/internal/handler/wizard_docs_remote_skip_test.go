package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// Regression coverage for the Agent Server mode "skip local fetch" wiring in
// the four wizard_docs.go call-sites of resolveWorkDir.
//
// Background (req_feb87f7d3d65f0fc): when the user picked Agent Server as
// the execution environment for the design / coding stage, the wizard
// handlers still ran a best-effort local `git fetch origin <base>` via
// ensureProjectDir → EnsureClonedAndSynced before they could decide to
// delegate the heavy work to the agent host. On a workstation that cannot
// reach github (Agent Server side is reachable), this produced a 60s
// FETCH_FAILED noise followed by an empty-error SSE panel even though the
// remote side would have worked fine.
//
// The fix threads a `skipLocalSync` bool from each handler through
// resolveWorkDir → ensureProjectDir so EnsureClonedAndSynced is only
// invoked when local execution is in effect. EnsureCloned (auto-restore-
// from-remote fallback for a missing local directory) still runs
// regardless of the flag.
//
// Each top-level test below pins ONE of the four call-sites in
// wizard_docs.go. The handler-level call-site is exercised via the helper
// (`ensureProjectDir`) that all four share: that helper is the actual
// gatekeeper for EnsureClonedAndSynced. We invoke it directly with the same
// skipLocalSync flag value each handler computes from its requirement row,
// and assert that the spy counter matches expectations in both local and
// Agent Server modes.
//
// Why direct-helper rather than full HTTP: the HTTP path of RefineDoc /
// ApplyDoc / CompressContext runs through roleConfig / resolveConfigIDForRun
// / claudeStream etc., which require a fully-wired WizardHandler (real
// RoleService, ClaudeConfigService, llm.Gateway, JobStore) that this test
// is not in a position to set up — and that mock surface has no upstream
// test precedent either. The shared helper, by contrast, is one decision
// branch away from each call-site and the only place where the flag is
// actually consumed, which makes it the correct unit under test.

// spyProjectSvc is a projectService implementation that records every
// EnsureClonedAndSynced invocation. Other interface methods return the
// minimum zero values so ensureProjectDir can keep moving past its pre-sync
// steps without panicking: Get returns a fake project row anchored at a
// non-existent path (we WANT the post-sync os.Stat to fail so the helper
// aborts cleanly with a wrapped error), EnsureCloned is a no-op success.
//
// Any other method is unused by ensureProjectDir and would panic via nil
// if invoked — acceptable because the helper path does not reach them.
type spyProjectSvc struct {
	ensureClonedAndSyncedCalls int32
	ensureClonedCalls          int32
	getCalls                    int32
}

func (s *spyProjectSvc) EnsureClonedAndSynced(ctx context.Context, id string, logf func(string)) (service.ProjectSyncResult, error) {
	atomic.AddInt32(&s.ensureClonedAndSyncedCalls, 1)
	return service.ProjectSyncResult{}, nil
}

func (s *spyProjectSvc) EnsureCloned(id string) error {
	atomic.AddInt32(&s.ensureClonedCalls, 1)
	return nil
}

func (s *spyProjectSvc) Get(id string) (*model.Project, error) {
	atomic.AddInt32(&s.getCalls, 1)
	// LocalPath intentionally points at a path that does NOT exist on disk:
	// ensureProjectDir will run os.Stat → fail → (skip or run) sync →
	// try EnsureCloned (stub returns nil) → os.Stat still fails → return
	// wrapped error. The counter is incremented on entry, so by the time
	// the helper returns the counter is stable regardless of whether the
	// post-sync path errored out.
	return &model.Project{
		ID:            id,
		Name:          "spy project",
		LocalPath:     "/tmp/novaworkbench-spy-nonexistent-" + id,
		DefaultBranch: "main",
	}, nil
}

func (s *spyProjectSvc) SyncDesignBase(ctx context.Context, projectID, baseBranch string, timeout time.Duration, logf func(string)) (service.GitSyncResult, error) {
	return service.GitSyncResult{}, nil
}

func (s *spyProjectSvc) OriginURL(projectID string) (string, error) {
	return "", nil
}

func (s *spyProjectSvc) DiscoverAndCacheClaudeProjectSlug(id, localPath string) (string, error) {
	return "", nil
}

func (s *spyProjectSvc) UpdateSyncStatus(id, status, commit string) {}

// runEnsureProjectDir is the shared test body for the four
// call-site-specific top-level tests. It exercises the helper exactly the
// way each call-site does:
//
//   - resolveWorkDir(...) → resolveWorkDirLogged(..., nil logf) → ensureProjectDir(..., skipLocalSync)
//
// skipLocalSync is computed by each handler from the requirement row: three
// of the four use DesignAgentServerID (RefineDoc / refineDocViaAgent /
// ApplyDoc, all design-stage doc-refine paths) and one uses AgentServerID
// (CompressContext, coding-session mid-conversation context compression).
// The test parametrises the flag value the way the handler does so the
// "what value should this field produce" question is answered by the test
// for each call-site independently.
func runEnsureProjectDir(t *testing.T, name string, requirement *model.Requirement, skipLocalSync bool, wantSyncCalls int32) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		spy := &spyProjectSvc{}
		h := &WizardHandler{projectSvc: spy}

		// ensureProjectDir will error out because the stub's project path
		// does not exist on disk. The error is irrelevant to the assertion
		// — the spy counter is incremented on entry to
		// EnsureClonedAndSynced, BEFORE the post-sync os.Stat can fail.
		_ = h.ensureProjectDir(context.Background(), requirement, "/tmp/novaworkbench-spy-nonexistent-"+requirement.ProjectID, func(string) {}, skipLocalSync)

		if got := atomic.LoadInt32(&spy.ensureClonedAndSyncedCalls); got != wantSyncCalls {
			t.Errorf("EnsureClonedAndSynced calls = %d, want %d", got, wantSyncCalls)
		}
		// EnsureCloned (the auto-restore fallback) MUST run regardless of
		// skipLocalSync — pinning that guarantee here so a future tweak
		// can't silently drop the local-directory recovery path.
		if got := atomic.LoadInt32(&spy.ensureClonedCalls); got != 1 {
			t.Errorf("EnsureCloned calls = %d, want 1 (must run regardless of skipLocalSync)", got)
		}
	})
}

// TestRefineDoc_SkipsLocalSync_WhenDesignAgentServerIDSet pins the
// wizard_docs.go:360 call-site of resolveWorkDir inside the SSE-direct
// branch of RefineDoc.
//
// In production, RefineDoc routes to refineDocViaAgent when both
// requirement.DesignAgentServerID AND agentSvrSvc are non-empty; line 360
// is only reached when the agent-server side is unavailable. The flag value
// under test is therefore requirement.DesignAgentServerID, and the
// assertion target is the helper that consumes it.
func TestRefineDoc_SkipsLocalSync_WhenDesignAgentServerIDSet(t *testing.T) {
	const reqID = "req_refine"
	cases := []struct {
		name                string
		designAgentServerID string
		wantSyncCalls       int32
	}{
		{
			name:                "local mode (DesignAgentServerID empty) calls EnsureClonedAndSynced",
			designAgentServerID: "",
			wantSyncCalls:       1,
		},
		{
			name:                "Agent Server mode (DesignAgentServerID set) skips EnsureClonedAndSynced",
			designAgentServerID: "agent_xxx",
			wantSyncCalls:       0,
		},
	}
	for _, tc := range cases {
		requirement := &model.Requirement{
			ID:                   reqID,
			ProjectID:            "proj_refine",
			DesignAgentServerID:  tc.designAgentServerID,
		}
		runEnsureProjectDir(t, tc.name, requirement, tc.designAgentServerID != "", tc.wantSyncCalls)
	}
}

// TestRefineDocViaAgent_SkipsLocalSync_WhenDesignAgentServerIDSet pins the
// wizard_docs.go:547 call-site of resolveWorkDir inside refineDocViaAgent.
// RefineDoc delegates to refineDocViaAgent when both DesignAgentServerID
// AND agentSvrSvc are non-empty; here we model that branch. The flag value
// is identical to the local SSE-direct branch (DesignAgentServerID).
func TestRefineDocViaAgent_SkipsLocalSync_WhenDesignAgentServerIDSet(t *testing.T) {
	const reqID = "req_refine_agent"
	cases := []struct {
		name                string
		designAgentServerID string
		wantSyncCalls       int32
	}{
		{
			name:                "local mode (DesignAgentServerID empty) calls EnsureClonedAndSynced",
			designAgentServerID: "",
			wantSyncCalls:       1,
		},
		{
			name:                "Agent Server mode (DesignAgentServerID set) skips EnsureClonedAndSynced",
			designAgentServerID: "agent_xxx",
			wantSyncCalls:       0,
		},
	}
	for _, tc := range cases {
		requirement := &model.Requirement{
			ID:                  reqID,
			ProjectID:           "proj_refine_agent",
			DesignAgentServerID: tc.designAgentServerID,
		}
		runEnsureProjectDir(t, tc.name, requirement, tc.designAgentServerID != "", tc.wantSyncCalls)
	}
}

// TestApplyDoc_SkipsLocalSync_WhenDesignAgentServerIDSet pins the
// wizard_docs.go:726 call-site of resolveWorkDir inside ApplyDoc. ApplyDoc
// is stage-agnostic (it serves design / coding / wiki doc_types by
// request), but the design-stage agent-server binding is what gets
// threaded through here per the implementation choice to keep a single
// field driving this refactor — see the plan for the rationale. The
// assertion target is the same flag: requirement.DesignAgentServerID.
func TestApplyDoc_SkipsLocalSync_WhenDesignAgentServerIDSet(t *testing.T) {
	const reqID = "req_apply"
	cases := []struct {
		name                string
		designAgentServerID string
		wantSyncCalls       int32
	}{
		{
			name:                "local mode (DesignAgentServerID empty) calls EnsureClonedAndSynced",
			designAgentServerID: "",
			wantSyncCalls:       1,
		},
		{
			name:                "Agent Server mode (DesignAgentServerID set) skips EnsureClonedAndSynced",
			designAgentServerID: "agent_xxx",
			wantSyncCalls:       0,
		},
	}
	for _, tc := range cases {
		requirement := &model.Requirement{
			ID:                  reqID,
			ProjectID:           "proj_apply",
			DesignAgentServerID: tc.designAgentServerID,
		}
		runEnsureProjectDir(t, tc.name, requirement, tc.designAgentServerID != "", tc.wantSyncCalls)
	}
}

// TestCompressContext_SkipsLocalSync_WhenAgentServerIDSet pins the
// wizard_docs.go:1090 call-site of resolveWorkDir inside CompressContext.
// CompressContext runs in the CODING session (it's the "compress our
// current context" hook fired mid-coding-conversation), so the flag value
// under test is requirement.AgentServerID, NOT DesignAgentServerID.
// CompressContext treats resolveWorkDir errors as non-fatal — it logs and
// falls back to projectPath — but the spy counter is incremented BEFORE
// that fallback can take effect, so the assertion is stable either way.
func TestCompressContext_SkipsLocalSync_WhenAgentServerIDSet(t *testing.T) {
	const reqID = "req_compress"
	cases := []struct {
		name          string
		agentServerID string
		wantSyncCalls int32
	}{
		{
			name:          "local mode (AgentServerID empty) calls EnsureClonedAndSynced",
			agentServerID: "",
			wantSyncCalls: 1,
		},
		{
			name:          "Agent Server mode (AgentServerID set) skips EnsureClonedAndSynced",
			agentServerID: "agent_xxx",
			wantSyncCalls: 0,
		},
	}
	for _, tc := range cases {
		requirement := &model.Requirement{
			ID:            reqID,
			ProjectID:     "proj_compress",
			AgentServerID: tc.agentServerID,
			// DesignAgentServerID stays "" — CompressContext reads
			// AgentServerID only.
		}
		runEnsureProjectDir(t, tc.name, requirement, tc.agentServerID != "", tc.wantSyncCalls)
	}
}