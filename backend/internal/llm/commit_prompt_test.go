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
