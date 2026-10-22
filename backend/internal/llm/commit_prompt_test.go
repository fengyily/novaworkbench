package llm

import (
	"strings"
	"testing"
)

// TestCommitArtifactsPromptRequestsPRTitle pins the prompt contract the
// local shell push path relies on: GenerateCommitArtifacts must ask for
// pr_title, and the per-language rule must cover both outputs. A prompt
// that only mentions commit_message leaves PRTitle empty, which made
// createPRShell fall back to the (Chinese) requirement title on an
// English-style project (req_3e7f5c152254672f).
func TestCommitArtifactsPromptRequestsPRTitle(t *testing.T) {
	if !strings.Contains(commitMessageSystemPrompt, `"pr_title":"..."`) {
		t.Errorf("system prompt JSON shape must include pr_title, got:\n%s", commitMessageSystemPrompt)
	}
	if !strings.Contains(commitMessageSystemPrompt, "pr_title 字面要求") {
		t.Errorf("system prompt must state the pr_title requirements")
	}

	rules := map[string]string{
		"en": commitMessageLangRuleEn,
		"zh": commitMessageLangRuleZh,
	}
	for lang, rule := range rules {
		if !strings.Contains(rule, "commit_message") || !strings.Contains(rule, "pr_title") {
			t.Errorf("[%s] language rule must cover both fields, got %q", lang, rule)
		}
	}

	if !strings.Contains(commitMessageUserTemplate, "pr_title") {
		t.Errorf("user template must ask for pr_title, got:\n%s", commitMessageUserTemplate)
	}
}

// TestCommitPromptStyleRuleSubstitution covers the placeholder wiring for
// the project 「提交规范」 (Conventional Commits / custom rules).
//
// The system prompt used to carry a hardcoded "不要 Conventional Commits
// 前缀" line, which contradicted any project that had opted into the
// conventional preset — the model received both "add a prefix" and "never add
// a prefix" in the same prompt. The line now lives in a {{STYLE_RULE}}
// section so the project's own rules replace it.
//
// This test pins the substitution contract GenerateCommitArtifacts performs
// (NewReplacer over both tokens) without needing an LLM: it applies the same
// substitution the gateway does and inspects the result.
func TestCommitPromptStyleRuleSubstitution(t *testing.T) {
	if !strings.Contains(commitMessageSystemPrompt, "{{STYLE_RULE}}") {
		t.Fatal("system prompt must expose a {{STYLE_RULE}} placeholder")
	}
	// Neither placeholder may survive into the rendered prompt, in either
	// configuration — an unsubstituted {{STYLE_RULE}} would reach the model
	// as literal text.
	render := func(style string) string {
		return strings.NewReplacer(
			"{{LANG_RULE}}", commitMessageLangRuleZh,
			"{{STYLE_RULE}}", style,
		).Replace(commitMessageSystemPrompt)
	}

	configured := render("## 项目提交规范（Conventional Commits）\n- 格式：`type(scope): subject`")
	if strings.Contains(configured, "{{") {
		t.Errorf("placeholders left unsubstituted:\n%s", configured)
	}
	if !strings.Contains(configured, "type(scope): subject") {
		t.Errorf("configured rules must reach the prompt:\n%s", configured)
	}
	// The old hardcoded negation must be gone from the template entirely —
	// it is what made the conventional preset unsatisfiable.
	for _, negation := range []string{"不要 Conventional Commits 前缀", "不要以 `feat:` 等前缀开头"} {
		if strings.Contains(commitMessageSystemPrompt, negation) {
			t.Errorf("system prompt still hardcodes %q; it must live in {{STYLE_RULE}} instead", negation)
		}
	}

	// Unconfigured project → the fallback must still state the no-prefix rule
	// explicitly. Models add feat:/fix: unprompted, so dropping the section
	// silently would change behaviour for every existing project.
	legacy := render(commitMessageStyleRuleFallback)
	if !strings.Contains(legacy, "不加任何前缀") {
		t.Errorf("fallback rule must preserve the explicit no-prefix instruction:\n%s", legacy)
	}
	if !strings.Contains(legacy, commitMessageLangRuleZh) {
		t.Errorf("one replacer pass must substitute both tokens:\n%s", legacy)
	}
}
