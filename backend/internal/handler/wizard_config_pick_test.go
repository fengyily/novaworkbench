package handler

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// cfg builds a config row with the given model ids, in List order.
func cfg(id string, models ...string) model.ClaudeConfig {
	entries := make([]model.ModelEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, model.ModelEntry{Model: m})
	}
	return model.ClaudeConfig{ID: id, Models: entries}
}

// TestPickConfigForModelFrom pins the sub-task / re-split config resolution
// chain. The reported bug: a requirement developed on "Claude Code /
// claude-ops-4.8" produced sub-tasks that ran against the developer role's
// gateway (MinMax) — and, when the config dropdown was re-hydrated, against the
// global active config (DeepSeek) — because only the MODEL was inherited while
// the config came from the role/active binding.
func TestPickConfigForModelFrom(t *testing.T) {
	// Claude Code is neither active nor role-bound here, mirroring the bug
	// report: only the model→config lookup can find it.
	configs := []model.ClaudeConfig{
		cfg("ccfg_deepseek", "deepseek-v4-pro", "deepseek-flash"), // active
		cfg("ccfg_minmax", "MiniMax-M3", "MiniMax-M2.7"),
		cfg("ccfg_claude", "claude-opus-4-8", "claude-opus-5"),
	}
	const (
		deepseek = "ccfg_deepseek"
		minmax   = "ccfg_minmax"
		claude   = "ccfg_claude"
	)

	cases := []struct {
		name       string
		model      string
		candidates []string
		want       string
	}{
		{
			name:       "legacy requirement: inherited model, no persisted config",
			model:      "claude-opus-4-8",
			candidates: []string{"", minmax, "role_executor_cfg"},
			want:       claude,
		},
		{
			name:       "persisted parent config wins when it owns the model",
			model:      "claude-opus-4-8",
			candidates: []string{claude, minmax, ""},
			want:       claude,
		},
		{
			name:       "model owned by the developer role binding needs no lookup",
			model:      "MiniMax-M3",
			candidates: []string{"", minmax, "role_executor_cfg"},
			want:       minmax,
		},
		{
			name:       "stale persisted config loses to the model's real owner",
			model:      "claude-opus-4-8",
			candidates: []string{deepseek, minmax, ""},
			want:       claude,
		},
		{
			name:       "empty model keeps the first non-empty binding",
			model:      "",
			candidates: []string{"", minmax, "role_executor_cfg"},
			want:       minmax,
		},
		{
			name:       "unknown model keeps the caller's binding (never the active default)",
			model:      "some-removed-model",
			candidates: []string{"", minmax, "role_executor_cfg"},
			want:       minmax,
		},
		{
			name:       "nothing to resolve stays empty so llm falls back to active",
			model:      "some-removed-model",
			candidates: []string{"", ""},
			want:       "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pickConfigForModelFrom(configs, c.model, c.candidates...)
			if got != c.want {
				t.Fatalf("pickConfigForModelFrom(model=%q, candidates=%v) = %q, want %q",
					c.model, c.candidates, got, c.want)
			}
		})
	}
}

// TestPickConfigForModelFromNoConfigs guards the degraded path (empty or
// unreadable config table): the caller's binding still wins over "no config",
// so a DB hiccup can't silently reroute a run to the active gateway.
func TestPickConfigForModelFromNoConfigs(t *testing.T) {
	if got := pickConfigForModelFrom(nil, "claude-opus-4-8", "", "ccfg_minmax"); got != "ccfg_minmax" {
		t.Fatalf("no configs: got %q, want the caller's binding", got)
	}
	if got := pickConfigForModelFrom(nil, "claude-opus-4-8"); got != "" {
		t.Fatalf("no configs, no candidates: got %q, want \"\"", got)
	}
}
