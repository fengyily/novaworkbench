package prompt

import (
	"strings"
	"testing"
)

// TestBuildReviewPromptProjectContext pins where the project-level
// CLAUDE.md lands in the review prompt.
//
// Two things matter:
//   - it is rendered at all (a reviewer that never sees the project's
//     conventions cannot grade 「与项目既有约定是否一致」);
//   - it lands BEFORE the output contract. The conventions are context for
//     the review, not a fourth contract the model has to satisfy — and the
//     contract block ends with an explicit "nothing after the json block"
//     rule, which a trailing conventions section would contradict.
func TestBuildReviewPromptProjectContext(t *testing.T) {
	const block = "## 项目级约定\n\n分层：handler / service / model。"

	out := BuildReviewPrompt(ReviewPromptInput{
		ReviewType:     "branch",
		Branch:         "feat/x",
		BaseBranch:     "main",
		HeadRef:        "origin/feat/x",
		BaseRef:        "origin/main",
		ProjectContext: block,
	})
	if !strings.Contains(out, block) {
		t.Errorf("review prompt missing the project context:\n%s", out)
	}
	if strings.Index(out, block) > strings.Index(out, "## 输出契约") {
		t.Errorf("the project context comes after the output contract:\n%s", out)
	}

	// Without it the prompt is unchanged — the requirement-level Review
	// passes no ProjectContext (it gets the same block through the
	// sub-task prompt) and must keep its own prompt byte-for-byte.
	plain := BuildReviewPrompt(ReviewPromptInput{
		ReviewType: "branch",
		Branch:     "feat/x",
		BaseBranch: "main",
		HeadRef:    "origin/feat/x",
		BaseRef:    "origin/main",
	})
	if strings.Contains(plain, "项目级约定") {
		t.Errorf("review prompt mentions the project conventions although none was passed:\n%s", plain)
	}
}

// TestBuildRegeneratePromptProjectContext is the same rule for the
// regeneration: the regenerated report replaces the old one in front of the
// user, so it must be graded against the same conventions.
func TestBuildRegeneratePromptProjectContext(t *testing.T) {
	const block = "## 项目级约定\n\n分层：handler / service / model。"

	out := BuildRegeneratePrompt(RegeneratePromptInput{
		ReviewType:     "branch",
		Branch:         "feat/x",
		BaseBranch:     "main",
		ProjectContext: block,
	})
	if !strings.Contains(out, block) {
		t.Errorf("regenerate prompt missing the project context:\n%s", out)
	}
	if strings.Index(out, block) > strings.Index(out, "## 输出契约") {
		t.Errorf("the project context comes after the output contract:\n%s", out)
	}

	plain := BuildRegeneratePrompt(RegeneratePromptInput{
		ReviewType: "branch",
		Branch:     "feat/x",
		BaseBranch: "main",
	})
	if strings.Contains(plain, "项目级约定") {
		t.Errorf("regenerate prompt mentions the project conventions although none was passed:\n%s", plain)
	}
}
