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

// TestPushPRRuntimeModel pins the "提交 → 推送 → 创建 PR" child's model/config
// inheritance. The reported bug: with the active config being "配置B / b 模型",
// a requirement developed on "配置A / a 模型" dispatched its auto push+PR child
// on 配置B — the child inherited the model but not the gateway (and the
// sub_tasks.model / token_usage.claude_config_id pair recorded the mismatch).
func TestPushPRRuntimeModel(t *testing.T) {
	// A requirement last developed on 配置A / a 模型.
	req := &model.Requirement{ID: "req_1", DeveloperModel: "a 模型", DeveloperConfigID: "ccfg_a"}
	// A requirement with no recorded developer model (never developed).
	bare := &model.Requirement{ID: "req_2"}
	// A legacy row whose developer_model is the "no specific model" sentinel.
	sentinel := &model.Requirement{ID: "req_3", DeveloperModel: DefaultModelLabel}

	cases := []struct {
		name         string
		req          *model.Requirement
		explicit     string
		prModel      string
		prCfgID      string
		wantModel    string
		wantConfigID string
	}{
		{
			name: "inherits the parent requirement's model",
			req:  req, prModel: "b 模型", prCfgID: "ccfg_b",
			wantModel: "a 模型", wantConfigID: "", // runner resolves 配置A from the model
		},
		{
			name: "explicit pick wins over the parent",
			req:  req, explicit: "c 模型", prModel: "b 模型", prCfgID: "ccfg_b",
			wantModel: "c 模型", wantConfigID: "",
		},
		{
			name: "no developer model falls back to the pr_author pair",
			req:  bare, prModel: "b 模型", prCfgID: "ccfg_b",
			wantModel: "b 模型", wantConfigID: "ccfg_b",
		},
		{
			name: "sentinel developer model means no inheritance",
			req:  sentinel, prModel: "b 模型", prCfgID: "ccfg_b",
			wantModel: "b 模型", wantConfigID: "ccfg_b",
		},
		{
			name: "empty pr_author model drops its config too (runner derives both)",
			req:  bare, prModel: "", prCfgID: "ccfg_b",
			wantModel: "", wantConfigID: "",
		},
		{
			name: "nil requirement behaves like a bare row",
			req:  nil, prModel: "b 模型", prCfgID: "ccfg_b",
			wantModel: "b 模型", wantConfigID: "ccfg_b",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotModel, gotCfg := pushPRRuntimeModel(c.req, c.explicit, c.prModel, c.prCfgID)
			if gotModel != c.wantModel || gotCfg != c.wantConfigID {
				t.Fatalf("pushPRRuntimeModel(req=%v, explicit=%q, pr=(%q,%q)) = (%q,%q), want (%q,%q)",
					c.req, c.explicit, c.prModel, c.prCfgID, gotModel, gotCfg, c.wantModel, c.wantConfigID)
			}
		})
	}
}
