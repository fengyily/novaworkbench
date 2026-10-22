package service

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestBuildCommitStyleRules_ZeroRegression pins the promise that makes this
// feature safe to ship: a project that never configured a preset renders an
// EMPTY rule set, so every prompt it sees is byte-identical to the one from
// before the two new columns existed. Both "" (the ALTER default every legacy
// row lands on) and the explicit "none" must produce that empty struct —
// "" and "none" differ only in what they MEAN to the user, never in output.
func TestBuildCommitStyleRules_ZeroRegression(t *testing.T) {
	for _, preset := range []string{"", model.CommitStylePresetNone} {
		for _, lang := range []string{"", "zh", "en", "mixed"} {
			got := BuildCommitStyleRules(preset, "leftover text", lang)
			if got.Commit != "" || got.PRTitle != "" || got.PRBody != "" || got.Example != "" || got.Conventional {
				t.Errorf("preset=%q lang=%q must render empty rules, got %+v", preset, lang, got)
			}
			// An unconfigured project must also contribute nothing to the
			// single-shot LLM channel, which is what falls back to the
			// hardcoded "no prefix" wording.
			if got.SingleShotRule() != "" {
				t.Errorf("preset=%q lang=%q: SingleShotRule must be empty, got %q", preset, lang, got.SingleShotRule())
			}
		}
	}
}

// TestBuildCommitStyleRules_Conventional is the snapshot test for the
// recommended preset: every one of the four fields must be populated, the
// rendered type list must come from model.CommitStyleTypes (the same list the
// docs show the user), and the example must localize with the project
// language — the example IS the "shape + language" demo the user judges the
// preset by.
func TestBuildCommitStyleRules_Conventional(t *testing.T) {
	zh := BuildCommitStyleRules(model.CommitStylePresetConventional, "", "zh")
	en := BuildCommitStyleRules(model.CommitStylePresetConventional, "", "en")

	if !zh.Conventional || !en.Conventional {
		t.Fatal("conventional preset must set the Conventional flag (NewPRShellTemplates keys off it)")
	}
	for _, field := range []struct{ name, got string }{
		{"zh.Commit", zh.Commit}, {"zh.PRTitle", zh.PRTitle}, {"zh.PRBody", zh.PRBody},
		{"en.Commit", en.Commit}, {"en.PRTitle", en.PRTitle}, {"en.PRBody", en.PRBody},
	} {
		if strings.TrimSpace(field.got) == "" {
			t.Errorf("conventional preset: %s must not be empty", field.name)
		}
	}
	for _, ty := range model.CommitStyleTypes {
		if !strings.Contains(zh.Commit, ty) {
			t.Errorf("conventional Commit rules must list type %q", ty)
		}
	}
	// The requirement names this exact example; keep it pinned.
	if zh.Example != "fix(model): 修复用户输入为空时的崩溃" {
		t.Errorf("zh example drifted: %q", zh.Example)
	}
	if en.Example != "fix(model): guard against empty input" {
		t.Errorf("en example drifted: %q", en.Example)
	}
	if !strings.Contains(en.Commit, "fix(model): guard against empty input") {
		t.Error("en commit rules should carry the en example so the model sees the shape AND the language")
	}
	// PR title must reuse the commit format, per the preset's contract.
	if !strings.Contains(zh.PRTitle, "type(scope): subject") {
		t.Errorf("zh PRTitle must require the conventional format, got %q", zh.PRTitle)
	}
	if !strings.Contains(en.PRTitle, "type(scope): subject") {
		t.Errorf("en PRTitle must require the conventional format, got %q", en.PRTitle)
	}
}

// TestBuildCommitStyleRules_Custom pins the long-tail path: the user's own
// text becomes the rule, verbatim and language-agnostic. It must NOT pick up
// the conventional preset's content (choosing custom means "these are my
// rules", not "conventional plus more").
func TestBuildCommitStyleRules_Custom(t *testing.T) {
	const rules = "# 允许的 type: feat / fix\nscope 用模块名"
	got := BuildCommitStyleRules(model.CommitStylePresetCustom, rules, "en")

	if !strings.Contains(got.Commit, rules) || !strings.Contains(got.PRBody, rules) {
		t.Errorf("custom rules text must reach both the commit and PR-body blocks, got %+v", got)
	}
	if strings.Contains(got.Commit, "Conventional Commits") {
		t.Error("custom preset must not splice in the conventional preset")
	}
	if got.Conventional {
		t.Error("custom preset must not claim the Conventional flag — the shell fallback would fabricate a `chore:` prefix")
	}
	if !strings.Contains(got.PRTitle, "项目提交规范") {
		t.Errorf("custom PRTitle should point at the supplement section, got %q", got.PRTitle)
	}
	// The user's text must appear exactly once per prompt, not once per field.
	if n := strings.Count(got.SingleShotRule(), rules); n != 1 {
		t.Errorf("SingleShotRule repeats the user text %d times; a long supplement would blow the prompt budget", n)
	}
}

// TestBuildCommitStyleRules_ConventionalPlusSupplement covers the common combo
// — pick a preset AND append a project-specific rule — which is exactly why
// the supplement textarea is rendered for every preset rather than only for
// "custom".
func TestBuildCommitStyleRules_ConventionalPlusSupplement(t *testing.T) {
	const extra = "DB migration files go in backend/internal/db/migrations/"
	got := BuildCommitStyleRules(model.CommitStylePresetConventional, extra, "zh")

	if !strings.Contains(got.Commit, "type(scope): subject") {
		t.Error("supplement must not displace the preset's own rules")
	}
	if !strings.Contains(got.Commit, extra) {
		t.Error("supplement must be appended to the commit rules")
	}
	if !strings.Contains(got.Commit, customStyleSupplementHeading) {
		t.Errorf("supplement should be wrapped in a recognisable heading %q so a reviewer can tell it came from project config", customStyleSupplementHeading)
	}
}

// TestBuildCommitStyleRules_EdgeCases pins the degenerate inputs the preview
// endpoint can receive (it validates nothing — it's a pure render over
// whatever the user has typed so far).
func TestBuildCommitStyleRules_EdgeCases(t *testing.T) {
	// custom preset with nothing typed yet must degrade to "no rules", not
	// render a heading with an empty body under it.
	if got := BuildCommitStyleRules(model.CommitStylePresetCustom, "   ", "zh"); got.Commit != "" {
		t.Errorf("blank custom text must render empty, got %q", got.Commit)
	}
	// An unknown preset (row hand-edited in SQL, or a preset retired in a
	// future release) must degrade to "no rules" rather than inject text of
	// unknown provenance.
	if got := BuildCommitStyleRules("banana", "some rule", "zh"); got.Commit != "" {
		t.Errorf("unknown preset must render empty, got %q", got.Commit)
	}
	// Oversized supplement is clamped here because the preview endpoint never
	// goes through SetCommitStyle's validation.
	huge := strings.Repeat("x", model.CommitStyleCustomMaxLen+500)
	got := BuildCommitStyleRules(model.CommitStylePresetCustom, huge, "en")
	if n := len([]rune(got.Commit)); n > model.CommitStyleCustomMaxLen+len(customStyleSupplementHeading)+2 {
		t.Errorf("oversized supplement must be clamped to the cap, got %d runes", n)
	}
	// Surrounding whitespace is a user-typing artifact, not content.
	if got := BuildCommitStyleRules("  conventional  ", "  ", "en"); got.Commit == "" {
		t.Error("preset should be trimmed before dispatch")
	}
}

// TestPRLangRules_NoPrefixRuleFollowsPreset is the regression guard for the
// defect that motivated this feature: three hardcoded "不要 Conventional
// Commits 前缀" lines used to contradict a project that had opted into the
// conventional preset, handing the model two opposite instructions at once.
//
// Unconfigured → the negation stays (byte-identical legacy wording).
// Configured → the negation is REPLACED by the positive format rule.
func TestPRLangRules_NoPrefixRuleFollowsPreset(t *testing.T) {
	conventional := BuildCommitStyleRules(model.CommitStylePresetConventional, "", "zh")
	noStyle := CommitStyleRules{}

	// Each language's legacy negation, kept verbatim when unconfigured and
	// replaced when a preset is configured.
	legacyNegations := map[string]string{
		"zh":    "不要以 `feat:` 等前缀开头",
		"mixed": "不要 Conventional Commits 前缀",
		"en":    "no Conventional Commits prefix",
	}
	for lang, negation := range legacyNegations {
		legacy := PRLangRules(lang, noStyle)
		configured := PRLangRules(lang, conventional)

		if !strings.Contains(legacy, negation) {
			t.Errorf("lang=%s: unconfigured block must keep its legacy no-prefix wording %q", lang, negation)
		}
		if strings.Contains(configured, negation) {
			t.Errorf("lang=%s: configured block still carries the contradictory no-prefix rule:\n%s", lang, configured)
		}
		if !strings.Contains(configured, "type(scope): subject") {
			t.Errorf("lang=%s: configured block must require the conventional title format:\n%s", lang, configured)
		}
		// The rest of the template (sections, prohibitions, checklists) is
		// shared baseline — a preset must never delete those.
		for _, keep := range []string{"Closes #123", "🤖 Generated with Claude Code", "Self-tested", "自测通过"} {
			if strings.Contains(legacy, keep) && !strings.Contains(configured, keep) {
				t.Errorf("lang=%s: preset dropped baseline text %q", lang, keep)
			}
		}
	}
}

// TestPRShellTemplates_ConventionalFallback pins the deliberate asymmetry in
// the deterministic (no-LLM) shell path: the commit fallback gains a
// `chore:` prefix because "chore" is semantically always true, while the PR
// title fallback stays bare because a title prefix would assert a change
// type nobody determined.
func TestPRShellTemplates_ConventionalFallback(t *testing.T) {
	conventional := BuildCommitStyleRules(model.CommitStylePresetConventional, "", "en")
	custom := BuildCommitStyleRules(model.CommitStylePresetCustom, "feat: 用 feat", "en")

	withPreset := NewPRShellTemplates("en", conventional)
	withoutPreset := NewPRShellTemplates("en", CommitStyleRules{})
	withCustom := NewPRShellTemplates("en", custom)

	if withPreset.CommitMessageDefaultFmt != "chore: %s" {
		t.Errorf("conventional commit fallback = %q, want %q", withPreset.CommitMessageDefaultFmt, "chore: %s")
	}
	if withoutPreset.CommitMessageDefaultFmt == "chore: %s" {
		t.Error("unconfigured project must keep the legacy commit fallback")
	}
	if withCustom.CommitMessageDefaultFmt == "chore: %s" {
		t.Error("custom free-text rules must not trigger the conventional `chore:` fallback")
	}
	// PR title fallback: never prefixed, in every case.
	for name, tmpl := range map[string]PRShellTemplates{
		"conventional": withPreset, "none": withoutPreset, "custom": withCustom,
	} {
		if strings.Contains(tmpl.PRTitleFallback, ":") && !strings.Contains(tmpl.PRTitleFallback, "auto-push") {
			t.Errorf("%s: PR title fallback must stay unprefixed, got %q", name, tmpl.PRTitleFallback)
		}
	}
}

// TestStyleHint_CarriesCommitRules pins the merge of the two style
// dimensions into the single line prompt.GitProjectContext renders: language
// answers "which language", the preset answers "which shape".
func TestStyleHint_CarriesCommitRules(t *testing.T) {
	conventional := BuildCommitStyleRules(model.CommitStylePresetConventional, "", "zh")

	withLang := StyleHint("zh", "", conventional)
	if !strings.Contains(withLang, "中文") {
		t.Errorf("language line lost: %q", withLang)
	}
	if !strings.Contains(withLang, "type(scope): subject") {
		t.Errorf("commit rules not carried into the hint: %q", withLang)
	}

	// No preset → the hint is exactly the legacy single line (zero regression).
	legacy := StyleHint("zh", "", CommitStyleRules{})
	if strings.Count(legacy, "\n") != 0 {
		t.Errorf("unconfigured hint must stay one line, got %q", legacy)
	}

	// Language undetected but preset configured → still ship the rules.
	// Dropping them would silently disable a preset on any project whose
	// commit-language detection came back empty.
	langless := StyleHint("", "", conventional)
	if !strings.Contains(langless, "type(scope): subject") {
		t.Errorf("preset must survive an undetected language: %q", langless)
	}
	if StyleHint("zh", "", CommitStyleRules{}) == langless {
		t.Error("a configured preset must change the hint even when the language is unresolved")
	}
}

// TestSetCommitStyle_Validation covers the four rejection branches the
// handler maps to 400. Each one exists because a bad value would otherwise
// persist and produce a prompt nobody can trace back to a UI mistake.
func TestSetCommitStyle_Validation(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_style", "style", "/tmp/proj_style")
	svc := NewProjectService(d, nil, nil)

	cases := []struct {
		name          string
		lang, preset  string
		custom        string
		wantErrPrefix string
	}{
		{"bad lang", "de", model.CommitStylePresetNone, "", "INVALID_LANG"},
		{"bad preset", "zh", "banana", "", "INVALID_PRESET"},
		{"custom without text", "zh", model.CommitStylePresetCustom, "   ", "INVALID_CUSTOM"},
		{"custom too long", "zh", model.CommitStylePresetCustom, strings.Repeat("x", model.CommitStyleCustomMaxLen+1), "INVALID_CUSTOM"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.SetCommitStyle("proj_style", c.lang, c.preset, c.custom)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.HasPrefix(err.Error(), c.wantErrPrefix) {
				t.Errorf("err = %q, want prefix %q", err.Error(), c.wantErrPrefix)
			}
		})
	}
}

// TestSetCommitStyle_PersistRoundTrip verifies the write path end to end,
// including the two behaviours that are easy to get wrong: a missing project
// must be distinguishable from a rejected value (404 vs 400), and the
// returned row must actually carry the new columns (they have to be in the
// SELECT list, or every prompt-building path would silently read "").
func TestSetCommitStyle_PersistRoundTrip(t *testing.T) {
	d := newTestDB(t)
	seedProject(t, d, "proj_style", "style", "/tmp/proj_style")
	svc := NewProjectService(d, nil, nil)

	got, err := svc.SetCommitStyle("proj_style", "zh", model.CommitStylePresetConventional, "补充一条")
	if err != nil {
		t.Fatalf("SetCommitStyle: %v", err)
	}
	if got.CommitStylePreset != model.CommitStylePresetConventional {
		t.Errorf("preset = %q, want conventional", got.CommitStylePreset)
	}
	if got.CommitStyleCustom != "补充一条" {
		t.Errorf("custom = %q", got.CommitStyleCustom)
	}
	if got.CommitLangOverride != "zh" || got.CommitLangSource != "user" {
		t.Errorf("language pin not applied: override=%q source=%q", got.CommitLangOverride, got.CommitLangSource)
	}

	// A second read (Get) must return the same values — this is what proves
	// the columns are in the SELECT list rather than only in the UPDATE.
	reread, err := svc.Get("proj_style")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reread.CommitStylePreset != model.CommitStylePresetConventional || reread.CommitStyleCustom != "补充一条" {
		t.Errorf("Get did not return the new columns: %+v", reread)
	}

	// Listing must carry them too (ListForUser has its own column list).
	// A non-admin user only sees projects granted through user_projects.
	if _, err := d.Exec(
		`INSERT INTO users (id, username, password_hash) VALUES ('usr_style', 'style', 'x')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO user_projects (user_id, project_id) VALUES ('usr_style', 'proj_style')`); err != nil {
		t.Fatalf("seed user_projects: %v", err)
	}
	list, err := svc.ListForUser("usr_style", false)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(list) != 1 || list[0].CommitStylePreset != model.CommitStylePresetConventional {
		t.Errorf("ListForUser lost the new columns: %+v", list)
	}

	// Unknown id → "project not found", which the handler maps to 404.
	if _, err := svc.SetCommitStyle("proj_missing", "zh", model.CommitStylePresetNone, ""); err == nil ||
		!strings.HasPrefix(err.Error(), "project not found") {
		t.Errorf("unknown project err = %v, want 'project not found'", err)
	}

	// Empty preset normalises to the explicit "none" so the column only ever
	// holds a deliberate choice going forward.
	cleared, err := svc.SetCommitStyle("proj_style", "", "", "")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.CommitStylePreset != model.CommitStylePresetNone {
		t.Errorf("cleared preset = %q, want none", cleared.CommitStylePreset)
	}
	if cleared.CommitLangSource != "auto" {
		t.Errorf("clearing the language pin should reset source to auto, got %q", cleared.CommitLangSource)
	}
}

// TestIsValidCommitStylePreset pins the whitelist the service validates
// against. Empty is excluded on purpose: "" is the never-configured state
// produced by the ALTER default, never by a write.
func TestIsValidCommitStylePreset(t *testing.T) {
	for _, v := range []string{model.CommitStylePresetNone, model.CommitStylePresetConventional, model.CommitStylePresetCustom} {
		if !model.IsValidCommitStylePreset(v) {
			t.Errorf("%q must be valid", v)
		}
	}
	for _, v := range []string{"", " ", "Conventional", "feat", "conventional "} {
		if model.IsValidCommitStylePreset(v) {
			t.Errorf("%q must be rejected", v)
		}
	}
}
