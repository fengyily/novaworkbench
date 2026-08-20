package handler

import "testing"

func TestInputToolPath(t *testing.T) {
	got := inputToolPath("Read", map[string]interface{}{"file_path": "/app/CLAUDE.md"})
	if got != "/app/CLAUDE.md" {
		t.Fatalf("Read => %q, want /app/CLAUDE.md", got)
	}
	if got := inputToolPath("Grep", map[string]interface{}{"pattern": "graphql"}); got != "graphql" {
		t.Fatalf("Grep => %q, want graphql", got)
	}
	if got := inputToolPath("Bash", map[string]interface{}{"command": "git status"}); got != "" {
		t.Fatalf("Bash => %q, want empty (command is too noisy)", got)
	}
	if got := inputToolPath("Read", nil); got != "" {
		t.Fatalf("nil input => %q, want empty", got)
	}
}

func TestEvaluateKnowledgeUsage(t *testing.T) {
	titles := []string{"CLAUDE.md", "AGENTS.md", "Project Structure"}
	toolFiles := []string{"/workspace/lib/.claude/worktree/CLAUDE.md", "cmd/server/main.go"}
	resultText := "本方案遵循了 CLAUDE.md 中描述的项目约定。"

	items, used := evaluateKnowledgeUsage(titles, toolFiles, resultText)
	if used != 1 {
		t.Fatalf("used = %d, want 1 (CLAUDE.md is the only one with a trace)", used)
	}
	byTitle := map[string]knowledgeUseItem{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	if !byTitle["CLAUDE.md"].Used {
		t.Fatal("CLAUDE.md should be marked used (tool basename + result mention)")
	}
	if byTitle["AGENTS.md"].Used {
		t.Fatal("AGENTS.md must NOT be marked used (no tool trace, no mention)")
	}
	if byTitle["Project Structure"].Used {
		t.Fatal("Project Structure must NOT be marked used (informational only)")
	}

	// Empty tool stack but the result text mentions the title → still used.
	items2, used2 := evaluateKnowledgeUsage(titles, nil, "阅读了 AGENTS.md 中的规则。")
	if used2 != 1 {
		t.Fatalf("mention-only used = %d, want 1", used2)
	}
	for _, it := range items2 {
		if it.Title == "AGENTS.md" && !it.Used {
			t.Fatal("AGENTS.md should be used via result-text mention")
		}
	}

	// No input at all → zero used.
	if _, used3 := evaluateKnowledgeUsage(nil, nil, "x"); used3 != 0 {
		t.Fatalf("empty titles used = %d, want 0", used3)
	}
}
