package handler

import (
	"strings"
	"testing"
)

// TestContinuePromptHasNoReauditWording pins the prompt fix for the
// "反复在回答" symptom.
//
// The OLD prompt asked the child to "先检查当前代码与工作区状态，判断哪些部分已完成、哪些未完成或需要修复" — i.e. it ORDERED a re-audit of the previous attempt. The model then re-derived the same conclusion over and over (14 of 19 assistant blocks in the recorded session carried the same sentence).
//
// The new prompt must forbid the audit and the repetition outright.
func TestContinuePromptHasNoReauditWording(t *testing.T) {
	// The wording that used to make the model re-audit its own work.
	forbidden := []string{
		"判断哪些部分已完成",
		"哪些未完成",
		"请先检查当前代码与工作区状态",
	}
	for _, f := range forbidden {
		if strings.Contains(continueSubTaskPrompt, f) {
			t.Errorf("continueSubTaskPrompt still contains the re-audit wording %q", f)
		}
	}

	// The replacement must be explicit: no restating, no re-auditing.
	for _, required := range []string{
		"不要复述",
		"不要重新审计",
		"不要重写",
	} {
		if !strings.Contains(continueSubTaskPrompt, required) {
			t.Errorf("continueSubTaskPrompt is missing the %q constraint", required)
		}
	}

	// It must still ask for a summary — the artifact reader depends on it.
	if !strings.Contains(continueSubTaskPrompt, "中文") {
		t.Error("continueSubTaskPrompt lost the Chinese-summary instruction")
	}
}

// TestContinuePromptAsksForTheRemainingWork pins the positive half of the
// rewrite: the child is told to work on what is LEFT, not to re-derive what
// was done.
func TestContinuePromptAsksForTheRemainingWork(t *testing.T) {
	for _, required := range []string{
		"尚未完成",
		"已经完成的部分保持原样",
	} {
		if !strings.Contains(continueSubTaskPrompt, required) {
			t.Errorf("continueSubTaskPrompt is missing %q", required)
		}
	}
}
