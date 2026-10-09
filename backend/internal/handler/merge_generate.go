package handler

// merge_generate.go — the new "commit-message sourcing" layer inserted
// into the merge / push pipeline. Three handlers (MergeHandler.Push,
// MergeHandler.LocalMerge, WizardHandler.autoPushPR) used to accept
// the user's body.commit_message verbatim and otherwise fall back to
// git's default (dev branch / requirement title). The requirement
// "项目提交推送的模式" introduces four strategy strings per project
// (script_only / llm_only / script_first / llm_first), and this file
// is the single entry point all three handlers call.
//
// Public surface of this file is the (h *MergeHandler) methods
// `generateCommitMessage` (strategy dispatch) and (used internally
// only) `runCommitScript`. The strategy result tuple is
// (message, sourceString, error) where sourceString is the literal
// mode that produced the message (or "user" when the user typed one
// inline) — that string is what the caller writes to the JobLog so
// the UI can render "提交信息：策略 X → 已自动生成" without any extra
// round-trip.
//
// Notes for future maintainers:
//   - Don't add ANY LLM call to merge_generate.go that doesn't go
//     through llm.Gateway.GenerateCommitMessage. The push sub-task's
//     own prompt path (buildPushSubTaskPrompt) still generates its
//     own commit text via Claude — that's a separate concern, not
//     this layer's responsibility.
//   - The script runner is intentionally single-shot. A multi-pass
//     runner that re-runs on conflict would silently mutate the
//     user's intent across attempts. If a future requirement asks
//     for retry-on-conflict, lift it out into a separate helper
//     rather than extending runCommitScript in place.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// commitStrategyUser is the canonical literal used in JobLog + UI
// notifications when the user typed a commit_message inline on the
// push/merge request. Keeping it a constant (not the literal "user")
// means a future rename / i18n switch only touches one place.
const commitStrategyUser = "user"

// commitStrategyScriptLangMiss is the literal used when script_first
// mode produced output whose language didn't match the project's
// effective commit_lang and we fell back to LLM. Surfaced in JobLog
// so users see "脚本输出语言不对 → 已自动走 LLM" in the timeline.
const commitStrategyScriptLangMiss = "script_first+llm_fallback"

// commitStrategyLLMFailed is the literal used when llm_first mode's
// first attempt failed (timeout / API / parse / empty) and we
// recovered by running the user's script. Mirrors the above so the
// UI renders both fallback directions uniformly.
const commitStrategyLLMFailed = "llm_first+script_fallback"

// scriptMinLengthForLangDetect is the threshold below which stdout
// from the user's commit_script is accepted as-is, regardless of
// detected language. Below this length the CJK↔ASCII ratio test is
// too noisy to trust — a 3-character commit like "fix" or "修复"
// would flip-flop modes across runs.
const scriptMinLengthForLangDetect = 10

// scriptTimeoutDefault is the default cap for `runCommitScript`. The
// helper reads NOVA_COMMIT_SCRIPT_TIMEOUT at call time so operators
// can bump it without recompiling (e.g. when a project legitimately
// needs a slow analysis script).
const scriptTimeoutDefault = 30 * time.Second

// scriptTimeout returns the configured cap for `runCommitScript`,
// honouring the NOVA_COMMIT_SCRIPT_TIMEOUT env override. Falls
// back to scriptTimeoutDefault on parse failure so a malformed
// value never silently disables the timeout.
func scriptTimeout() time.Duration {
	v := strings.TrimSpace(envCommitScriptTimeout())
	if v == "" {
		return scriptTimeoutDefault
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("[commit-script] NOVA_COMMIT_SCRIPT_TIMEOUT=%q invalid (%v), using default %s", v, err, scriptTimeoutDefault)
		return scriptTimeoutDefault
	}
	return d
}

// envCommitScriptTimeout reads NOVA_COMMIT_SCRIPT_TIMEOUT. Indirected
// through a var so tests can swap it; production always reads the
// real os.Getenv.
var envCommitScriptTimeout = func() string { return os.Getenv("NOVA_COMMIT_SCRIPT_TIMEOUT") }

// runCommitScript executes the user-configured commit_message shell
// script in a fixed CWD (reqRow.WorktreePath when set, otherwise the
// project local path) and returns the trimmed stdout. The script is
// run under /bin/sh, so it's NOT a portable shebang — instead, the
// same multi-line string the user wrote in the ProjectDetail UI is
// interpreted once by sh. NOVA_COMMIT_REQUIREMENT_ID +
// NOVA_COMMIT_DIFF_PATH + NOVA_COMMIT_PROJECT_DIR envs are injected
// so the script can address them without hard-coding paths.
//
// Agent-server projects are not yet supported in v0.5.x (the script
// would need to run on the remote host). The caller passes
// useWorktree=true when WorktreePath is set so we honour it; if the
// caller lies, sh's own CWD semantics will surface the wrong path.
//
// On failure (non-zero exit / timeout / empty stdout after strip)
// the error carries the trimmed stderr so JobLog lines have
// something useful to show.
func runCommitScript(ctx context.Context, dir, script, reqID, projectDir string, useWorktree bool, allowEmpty bool) (string, error) {
	if script == "" {
		return "", fmt.Errorf("commit_script is empty")
	}
	if dir == "" {
		// Fall back to projectDir so a request without a worktree
		// (or with an unset WorktreePath) still has somewhere
		// sensible to run.
		if useWorktree && projectDir != "" {
			dir = projectDir
		} else {
			return "", fmt.Errorf("commit_script: no working directory available")
		}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, scriptTimeout())
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "/bin/sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(
		cmd.Environ(),
		"NOVA_COMMIT_REQUIREMENT_ID="+reqID,
		"NOVA_COMMIT_PROJECT_DIR="+projectDir,
		"NOVA_COMMIT_WORKTREE="+boolToOneZero(useWorktree),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Distinguish timeout (so the user knows the script was the
		// problem, not the LLM) from generic non-zero exit.
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("commit_script timeout after %s", scriptTimeout())
		}
		return "", fmt.Errorf("commit_script failed: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" && !allowEmpty {
		return "", fmt.Errorf("commit_script produced empty stdout")
	}
	return out, nil
}

// boolToOneZero renders a boolean as the env-friendly "1" / "0".
func boolToOneZero(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// generateCommitMessage is the strategy dispatcher. It is the
// single entry point that Push / LocalMerge / autoPushPR call when
// the user has not provided a manual commit_message (caller already
// short-circuits on userMsg != "" before reaching here). It honours
// the project's commit_mode (set via /api/projects/{id}/commit-push-
// config) and applies the four-mode contract:
//
//   - script_only  : run the script; never call LLM. Fail / empty /
//     short-with-wrong-language all become hard errors.
//   - llm_only     : ask LLM. (Same as the pre-strategy behaviour.)
//   - script_first : run the script first. If the stdout language
//     doesn't match the project's effective commit_lang AND the
//     output is at least scriptMinLengthForLangDetect chars, fall
//     back to LLM. Otherwise accept the script output.
//   - llm_first    : ask LLM first. If it returns an error of any
//     kind (timeout / API / parse / empty), fall back to running
//     the script.
//
// The "strategy" return string is one of the literals the JobLog /
// UI consumes. It's used by the three handler call sites to render
// "提交信息：策略 X → 已自动生成" without the UI having to know the
// `project` may be nil (the caller passed an unloaded row); in
// that case the mode falls back to CommitModeLLMOnly so behaviour
// is exactly the legacy default. We never invent defaults
// silently for script_* modes — those require a config check.
//
// Free function (not a MergeHandler method) so WizardHandler.autoPushPR
// can call the same dispatcher — keeping the strategy logic in one
// place is the whole point of this layer. The llm parameter is the
// shared gateway; nil means LLM modes hard-fail (which is the
// existing behaviour for the legacy code path).
func generateCommitMessage(
	ctx context.Context,
	llmGateway *llm.Gateway,
	reqRow *model.Requirement,
	project *model.Project,
	userMsg string,
	diff string,
) (msg string, strategy string, err error) {
	// User-supplied message always wins (this is the only path
	// bypassing the strategy: the user typed a commit_message
	// in the merge modal).
	if strings.TrimSpace(userMsg) != "" {
		return userMsg, commitStrategyUser, nil
	}
	if reqRow == nil {
		// Defensive: should never happen (Push / LocalMerge /
		// autoPushPR all validate reqRow first), but failing loud
		// here is better than silently dropping the commit.
		return "", "", fmt.Errorf("generateCommitMessage: missing requirement row")
	}

	mode := model.CommitModeLLMOnly
	commitLang := ""
	script := ""
	if project != nil {
		if m := strings.TrimSpace(project.CommitMode); m != "" && model.IsValidCommitMode(m) {
			mode = m
		}
		commitLang = service.ResolveCommitLang(project.CommitLang, project.CommitLangOverride)
		script = project.CommitScript
	}

	// No script configured + a script_* mode → fail loud. The
	// handler has already validated this on /commit-push-config,
	// so reaching here means somebody mutated the row out-of-band
	// (manual SQL?) or the project row was loaded from a snapshot
	// pre-dating the UI. Either way, an opaque "script not found"
	// beats silently switching modes.
	if (mode == model.CommitModeScriptOnly || mode == model.CommitModeScriptFirst) && script == "" {
		return "", "", fmt.Errorf("commit_script_required: mode=%s but the project's commit_script is empty", mode)
	}

	workDir := reqRow.WorktreePath
	if workDir == "" && project != nil {
		workDir = project.LocalPath
	}
	projectDir := ""
	if project != nil {
		projectDir = project.LocalPath
	}
	useWorktree := reqRow.WorktreePath != ""

	// Reusable script runner — both script_first and llm_first end
	// up calling it via different control flows.
	runScript := func() (string, error) {
		return runCommitScript(ctx, workDir, script, reqRow.ID, projectDir, useWorktree, false)
	}

	switch mode {
	case model.CommitModeScriptOnly:
		out, serr := runScript()
		if serr != nil {
			return "", "", serr
		}
		return out, model.CommitModeScriptOnly, nil

	case model.CommitModeLLMOnly:
		out, gerr := callLLMForCommit(ctx, llmGateway, reqRow, commitLang, diff)
		if gerr != nil {
			return "", "", gerr
		}
		return out, model.CommitModeLLMOnly, nil

	case model.CommitModeScriptFirst:
		out, serr := runScript()
		if serr != nil {
			// Script failed AND we're in script_first mode → fall
			// back to LLM (the user said "prefer script", but a
			// broken script shouldn't block the merge entirely).
			log.Printf("[commit-mode] %s: script_first script failed (%v), falling back to LLM", reqRow.ID, serr)
			out2, gerr := callLLMForCommit(ctx, llmGateway, reqRow, commitLang, diff)
			if gerr != nil {
				return "", "", fmt.Errorf("script (%v) and LLM (%w) both failed", serr, gerr)
			}
			return out2, commitStrategyScriptLangMiss, nil
		}
		// Short stdout: accept it without language detection
		// (the threshold exists exactly so messages like "fix"
		// don't bounce into LLM mode).
		if len([]rune(out)) < scriptMinLengthForLangDetect {
			return out, model.CommitModeScriptFirst, nil
		}
		// Long enough to detect language. If it matches the
		// project's commit_lang, accept; otherwise fall back.
		scriptLang := service.DetectTextLanguage(out)
		if commitLang != "" && commitLang != "mixed" && scriptLang != "" && scriptLang != "mixed" && scriptLang != commitLang {
			log.Printf("[commit-mode] %s: script_first output lang=%q != commit_lang=%q, falling back to LLM",
				reqRow.ID, scriptLang, commitLang)
			out2, gerr := callLLMForCommit(ctx, llmGateway, reqRow, commitLang, diff)
			if gerr != nil {
				return "", "", fmt.Errorf("script lang mismatch and LLM fallback failed: %w", gerr)
			}
			return out2, commitStrategyScriptLangMiss, nil
		}
		return out, model.CommitModeScriptFirst, nil

	case model.CommitModeLLMFirst:
		out, gerr := callLLMForCommit(ctx, llmGateway, reqRow, commitLang, diff)
		if gerr == nil {
			return out, model.CommitModeLLMFirst, nil
		}
		log.Printf("[commit-mode] %s: llm_first LLM call failed (%v), falling back to script", reqRow.ID, gerr)
		out2, serr := runScript()
		if serr != nil {
			return "", "", fmt.Errorf("LLM (%v) and script (%w) both failed", gerr, serr)
		}
		return out2, commitStrategyLLMFailed, nil
	}

	// Unreachable (model.IsValidCommitMode guards the switch
	// above), but defensive: a mode that falls through becomes
	// llm_only rather than panicking.
	out, gerr := callLLMForCommit(ctx, llmGateway, reqRow, commitLang, diff)
	if gerr != nil {
		return "", "", gerr
	}
	return out, model.CommitModeLLMOnly, nil
}

// callLLMForCommit is a thin wrapper over llm.Gateway that picks the
// right model + config pair for a commit-message call. Splits the
// LLM-flavoured branches out of generateCommitMessage so the
// strategy switch stays readable. Currently there is no project-
// level model override for the commit-message role, so we use the
// gateway default; a future "commit.author role" model override
// would slot in here.
func callLLMForCommit(
	ctx context.Context,
	llmGateway *llm.Gateway,
	reqRow *model.Requirement,
	commitLang string,
	diff string,
) (string, error) {
	if llmGateway == nil {
		return "", fmt.Errorf("llm gateway not wired")
	}
	desc := ""
	if reqRow != nil {
		desc = reqRow.Description
	}
	title := ""
	if reqRow != nil {
		title = reqRow.Title
	}
	out, err := llmGateway.GenerateCommitMessage(llm.GenerateCommitArtifactsOpts{
		ReqTitle:       title,
		ReqDescription: desc,
		CommitLang:     commitLang,
		Diff:           diff,
	})
	if err != nil {
		return "", err
	}
	_ = ctx // reserved: future cancellation wiring for abortable SSE
	return out, nil
}
