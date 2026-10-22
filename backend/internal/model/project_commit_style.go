package model

// Commit-style preset constants select which *convention* the project wants
// applied to generated commit messages / PR titles / PR bodies, on top of
// the language preference stored in commit_lang_override (zh / en / mixed).
// The preset lives on the projects row (model.Project.CommitStylePreset),
// is written by service.ProjectService.SetCommitStyle from the
// 「提交风格」card on /projects/:id, and is consumed — always after being
// rendered into text — by service.BuildCommitStyleRules.
//
// Four states, three constants:
//
//   - Empty string. Never configured — what every row created before this
//     feature existed holds, because the column's ALTER default is the empty
//     string. It renders to an EMPTY CommitStyleRules, so the injected prompt
//     is byte-identical to the pre-feature behaviour. Do not treat it as a
//     meaningful selection; that is what CommitStylePresetNone is for.
//
//   - CommitStylePresetNone. The user explicitly turned the preset off. It
//     renders identically to the empty string (no rules are injected); it
//     exists so the UI dropdown can offer "don't enforce anything" as a real
//     choice, letting the user disable a previously-selected preset without
//     having to clear the column.
//
//   - CommitStylePresetConventional. Conventional Commits — the most widely
//     adopted convention, and the preset recommended in the UI.
//
//   - CommitStylePresetCustom. The user's own free-text rules, stored in
//     CommitStyleCustom and used verbatim.
//
// Note the layering: the preset describes the SHAPE of a commit
// (`type(scope): subject`), the language describes the LANGUAGE it is written
// in. Both are rendered into the same prompt block by
// service.BuildCommitStyleRules.
const (
	CommitStylePresetNone         = "none"
	CommitStylePresetConventional = "conventional"
	CommitStylePresetCustom       = "custom"
)

// CommitStyleCustomMaxLen bounds the free-text rules column. The text is
// injected verbatim into every commit/PR generation prompt, so an
// unbounded value would let one project blow the prompt budget for every
// path. Enforced by service.ProjectService.SetCommitStyle (400
// INVALID_CUSTOM) — kept here so the limit lives next to the preset
// constants it protects, and so the handler's preview endpoint can apply
// the same cap without importing service.
const CommitStyleCustomMaxLen = 4000

// IsValidCommitStylePreset reports whether v may be persisted as
// CommitStylePreset. Empty string is NOT valid here — it is the
// never-configured state and is only produced by the ALTER default, never
// by a write; service.SetCommitStyle normalises the UI's "" dropdown value
// to CommitStylePresetNone before validating. Used by
// service.ProjectService.SetCommitStyle so a bad value surfaces as a 400
// with a useful message instead of an opaque UPDATE_FAILED.
func IsValidCommitStylePreset(v string) bool {
	switch v {
	case CommitStylePresetNone, CommitStylePresetConventional, CommitStylePresetCustom:
		return true
	}
	return false
}

// CommitStyleTypes is the canonical list of Conventional Commits types.
// service.BuildCommitStyleRules renders it into the prompt so the model
// picks from a closed set instead of inventing type names, and the
// frontend reads the same list (via GET /api/projects/commit-style/
// preview) so the documented set and the enforced set can never drift.
var CommitStyleTypes = []string{
	"feat",     // new user-facing capability
	"fix",      // bug fix
	"docs",     // documentation only
	"style",    // formatting / whitespace, no behaviour change
	"refactor", // internal restructuring, no behaviour change
	"perf",     // performance work
	"test",     // tests only
	"build",    // build system / dependencies / packaging
	"ci",       // CI configuration and pipelines
	"chore",    // maintenance that fits none of the above
	"revert",   // reverts a previous commit
}
