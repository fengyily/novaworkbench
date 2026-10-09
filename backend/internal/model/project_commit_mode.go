package model

// Commit-mode constants drive the "完成开发 → 提交推送" pipeline's
// 4-way strategy for sourcing the commit message. The mode is stored on
// the projects row (model.Project.CommitMode) and read by
// handler.mergeGenerate to decide whether to run the user's script, ask
// the LLM, or chain the two with a fallback. The values are mirrored on
// the frontend (frontend/src/pages/ProjectDetail.tsx), so changing them
// here is a coordinated change.
//
//   - CommitModeScriptOnly  — run the project's commit_script and use its
//                             stdout verbatim as the commit message. The
//                             LLM is never called. Failures (non-zero
//                             exit / empty stdout / script timeout)
//                             surface a hard error and abort the merge.
//   - CommitModeLLMOnly     — ask the OpenAI-compatible HTTP LLM
//                             channel to generate a commit message from
//                             the requirement title + description +
//                             project commit_lang. This was the only
//                             strategy before mode-aware routing landed
//                             and is the persisted default.
//   - CommitModeScriptFirst — run the user's script first. If the stdout
//                             language doesn't match the project's
//                             effective commit_lang (via
//                             service.ResolveCommitLang), fall back to
//                             LLM. Stdout under 10 chars is accepted
//                             without language detection (too short to
//                             reliably classify).
//   - CommitModeLLMFirst    — ask the LLM first. On any LLM error
//                             (timeout / API status / empty / JSON
//                             decode error), fall back to the user's
//                             script. Script failure is itself an
//                             error.
//
// IsValidCommitMode returns true iff v is one of the four constants
// above. Used by service.ProjectService.SetCommitPushConfig to validate
// user input before persisting.
const (
	CommitModeScriptOnly  = "script_only"
	CommitModeLLMOnly     = "llm_only"
	CommitModeScriptFirst = "script_first"
	CommitModeLLMFirst    = "llm_first"
)

// IsValidCommitMode reports whether v is one of the four recognised
// commit-mode strategy strings. Empty string is rejected — the handler
// treats unset as "use the persisted default" so callers must pass
// CommitModeLLMOnly (or a non-default) explicitly to swap behaviour.
// Used by service.ProjectService.SetCommitPushConfig and the handler-
// side error path so the user sees a useful 400 message instead of an
// opaque UPDATE_FAILED.
func IsValidCommitMode(v string) bool {
	switch v {
	case CommitModeScriptOnly, CommitModeLLMOnly, CommitModeScriptFirst, CommitModeLLMFirst:
		return true
	}
	return false
}
