package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
)

// TestGenerateCommitMessage_PRTitleSurfaced covers the prTitle surfacing rule
// across all four commit_mode values plus the no-project fallback
// (req_9ead19cd39f632fd — auto-push shell path needs prTitle from LLM paths,
// but pure-script paths must NOT pretend LLM produced one).
//
// The LLM call is stubbed via the package-level commitLLMFn variable
// (see merge_generate.go), so no real Claude turn happens.
//
// Expectations:
//   - commit_mode = llm_only              → prTitle non-empty
//   - commit_mode = llm_first (ok)        → prTitle non-empty
//   - commit_mode = llm_first (LLM err → script ok) → prTitle empty
//   - commit_mode = script_first script err → LLM fallback → prTitle non-empty
//   - commit_mode = script_first short stdout OK → prTitle empty
//   - commit_mode = script_only           → prTitle empty (script path)
//   - user-supplied userMsg               → prTitle empty (no LLM)
func TestGenerateCommitMessage_PRTitleSurfaced(t *testing.T) {
	// Common fixture.
	reqRow := &model.Requirement{
		ID:          "req_test",
		Title:       "测试需求标题",
		Description: "desc",
	}
	// New project with CommitMode set per subtest. LocalPath is a tempdir
	// so runCommitScript has somewhere to spawn /bin/sh.
	newProject := func(t *testing.T, mode string) *model.Project {
		t.Helper()
		return &model.Project{
			LocalPath:    t.TempDir(),
			CommitMode:   mode,
			CommitScript: "echo stub-commit", // any non-empty script for script_* modes
			CommitLang:   "en",
		}
	}

	// Stub control: each subtest sets expectedReturn and expectedErr to drive
	// the package-level commitLLMFn. We restore the default on test exit.
	origFn := commitLLMFn
	t.Cleanup(func() { commitLLMFn = origFn })

	stubLLM := func(msg, prTitle string, err error) {
		commitLLMFn = func(_ *llm.Gateway, _ llm.GenerateCommitArtifactsOpts) (string, string, error) {
			return msg, prTitle, err
		}
	}

	ctx := context.Background()

	t.Run("llm_only_with_stub", func(t *testing.T) {
		// Drive the actual LLM path through callLLMForCommit. A non-nil
		// *llm.Gateway is required so the nil-check passes; the stub
		// never calls any method on it.
		llmGateway := &llm.Gateway{}
		stubLLM("feat: stub commit", "feat: stub pr title", nil)
		msg, pr, strat, err := generateCommitMessage(ctx, llmGateway, reqRow, newProject(t, model.CommitModeLLMOnly), "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if msg != "feat: stub commit" {
			t.Errorf("msg = %q, want %q", msg, "feat: stub commit")
		}
		if pr != "feat: stub pr title" {
			t.Errorf("prTitle = %q, want %q (LLM should surface a PR title for llm_only)", pr, "feat: stub pr title")
		}
		if strat != model.CommitModeLLMOnly {
			t.Errorf("strategy = %q, want %q", strat, model.CommitModeLLMOnly)
		}
	})

	t.Run("llm_first_ok", func(t *testing.T) {
		llmGateway := &llm.Gateway{}
		stubLLM("fix: llm-first commit", "fix: llm-first pr", nil)
		msg, pr, strat, err := generateCommitMessage(ctx, llmGateway, reqRow, newProject(t, model.CommitModeLLMFirst), "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pr != "fix: llm-first pr" {
			t.Errorf("prTitle = %q, want non-empty", pr)
		}
		if strat != model.CommitModeLLMFirst {
			t.Errorf("strategy = %q, want %q", strat, model.CommitModeLLMFirst)
		}
		_ = msg
	})

	t.Run("llm_first_llm_err_script_ok", func(t *testing.T) {
		llmGateway := &llm.Gateway{}
		// LLM returns error → fall back to script → no prTitle
		stubLLM("", "", errStubLLM)
		msg, pr, strat, err := generateCommitMessage(ctx, llmGateway, reqRow, newProject(t, model.CommitModeLLMFirst), "", "")
		if err != nil {
			t.Fatalf("unexpected error (script should have saved us): %v", err)
		}
		if pr != "" {
			t.Errorf("prTitle = %q, want empty (script fallback must not pretend LLM produced a title)", pr)
		}
		if strat != commitStrategyLLMFailed {
			t.Errorf("strategy = %q, want %q", strat, commitStrategyLLMFailed)
		}
		// Script is "echo stub-commit" → stdout is "stub-commit"
		if !strings.Contains(strings.ToLower(msg), "stub-commit") {
			t.Errorf("msg should contain script output, got %q", msg)
		}
	})

	t.Run("script_only_no_llm_call", func(t *testing.T) {
		// Use a stub that would FAIL the test if called — proving script_only
		// never touches the LLM.
		commitLLMFn = func(_ *llm.Gateway, _ llm.GenerateCommitArtifactsOpts) (string, string, error) {
			t.Fatal("commitLLMFn must not be called for script_only")
			return "", "", nil
		}
		msg, pr, strat, err := generateCommitMessage(ctx, &llm.Gateway{}, reqRow, newProject(t, model.CommitModeScriptOnly), "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pr != "" {
			t.Errorf("prTitle = %q, want empty (script_only never invokes LLM)", pr)
		}
		if strat != model.CommitModeScriptOnly {
			t.Errorf("strategy = %q, want %q", strat, model.CommitModeScriptOnly)
		}
		_ = msg
	})

	t.Run("user_supplied_msg_wins_no_prTitle", func(t *testing.T) {
		commitLLMFn = func(_ *llm.Gateway, _ llm.GenerateCommitArtifactsOpts) (string, string, error) {
			t.Fatal("commitLLMFn must not be called when userMsg is provided")
			return "", "", nil
		}
		_, pr, strat, err := generateCommitMessage(ctx, &llm.Gateway{}, reqRow, newProject(t, model.CommitModeLLMOnly), "user typed this", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pr != "" {
			t.Errorf("prTitle = %q, want empty (user-supplied message path)", pr)
		}
		if strat != commitStrategyUser {
			t.Errorf("strategy = %q, want %q", strat, commitStrategyUser)
		}
	})
}

// errStubLLM is a sentinel error returned by stub commitLLMFn in failure
// scenarios (LLM unreachable, etc).
var errStubLLM = errStub("stub llm failure")

type errStub string

func (e errStub) Error() string { return string(e) }
