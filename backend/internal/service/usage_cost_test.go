package service

import (
	"reflect"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// usageTestDB opens a fresh SQLite db in a temp dir (migrations already applied
// by db.Init) for usage/cost integration tests.
func usageTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	return d
}

func TestDecodeModels(t *testing.T) {
	// Legacy string-array format converts to 0-priced entries.
	got := DecodeModels(`["claude-sonnet-4-5","claude-haiku-4-5"]`)
	want := []model.ModelEntry{
		{Model: "claude-sonnet-4-5"},
		{Model: "claude-haiku-4-5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy decode = %+v, want %+v", got, want)
	}

	// Object-array with input/output prices only (pre-cache-read-price era).
	got = DecodeModels(`[{"model":"claude-sonnet-4-5","input_price":3,"output_price":15}]`)
	want = []model.ModelEntry{{Model: "claude-sonnet-4-5", InputPrice: 3, OutputPrice: 15}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries decode = %+v, want %+v", got, want)
	}

	// Object-array with the new cache_read_price field populated.
	got = DecodeModels(`[{"model":"claude-sonnet-4-5","input_price":3,"output_price":15,"cache_read_price":0.3}]`)
	want = []model.ModelEntry{{Model: "claude-sonnet-4-5", InputPrice: 3, OutputPrice: 15, CacheReadPrice: 0.3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries with cache_read_price decode = %+v, want %+v", got, want)
	}

	// Empty / malformed → empty non-nil slice.
	if got := DecodeModels(`"null"`); len(got) != 0 {
		t.Fatalf("null got %+v, want empty", got)
	}
	if got := DecodeModels(`not-json`); len(got) != 0 {
		t.Fatalf("garbage got %+v, want empty", got)
	}
}

func TestUsageCostRecompute(t *testing.T) {
	d := usageTestDB(t)
	us := NewUsageService(d)

	// Seed one config with prices (CNY) and one legacy string-array config.
	cfg := []struct{ id, currency, models string }{
		{"ccfg_priced", "CNY", `[{"model":"claude-sonnet-4-5","input_price":3,"output_price":15}]`},
		{"ccfg_legacy", "USD", `["claude-haiku-4-5"]`}, // string legacy → 0 prices (no cost)
	}
	for _, c := range cfg {
		if _, err := d.Exec(
			"INSERT INTO claude_configs (id,name,base_url,auth_token,models,default_model,currency,is_active,created_at,updated_at) VALUES (?,?,'','',?,'',?,0,?,?)",
			c.id, c.id, c.models, c.currency, time.Now(), time.Now(),
		); err != nil {
			t.Fatalf("seed config %s: %v", c.id, err)
		}
	}

	now := time.Now()
	rows := []model.TokenUsage{
		// Requirement A on the priced config: 1M input + 0.5M output.
		{RequirementID: "req_a", ProjectID: "proj_1", Model: "claude-sonnet-4-5", ClaudeConfigID: "ccfg_priced", Currency: "CNY",
			InputTokens: 1_000_000, OutputTokens: 500_000, CreatedAt: now},
		// Requirement B on the same priced config: 1M cache read. cache_read_price
		// is unset on this config, so cache reads fall back to the input_price
		// (3) — preserving the pre-feature legacy behavior.
		{RequirementID: "req_b", ProjectID: "proj_1", Model: "claude-sonnet-4-5", ClaudeConfigID: "ccfg_priced", Currency: "CNY",
			CacheReadTokens: 1_000_000, CreatedAt: now.Add(2 * time.Hour)},
		// Pre-pricing row (currency empty → falls back to config CNY).
		{RequirementID: "req_c", ProjectID: "proj_1", Model: "claude-sonnet-4-5", ClaudeConfigID: "ccfg_priced", Currency: "",
			InputTokens: 1_000_000, CreatedAt: now.Add(24 * time.Hour)},
		// Row on the legacy (unpriced) config → cost 0.
		{RequirementID: "req_c", ProjectID: "proj_1", Model: "claude-haiku-4-5", ClaudeConfigID: "ccfg_legacy", Currency: "USD",
			OutputTokens: 500_000, CreatedAt: now.Add(24 * time.Hour)},
	}
	for _, u := range rows {
		if err := us.Record(u); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// Expected costs (per million tokens):
	//   row A: in 1M*3 + out 0.5M*15 = 3 + 7.5 = 10.5
	//   row B: cache_read 1M*3 (falls back to input_price) = 3
	//   row C(first): 1M*3 = 3 ; row C(second): unpriced → 0
	// Project total = 16.5 CNY (CNY), 0 USD (unpriced rows produce nothing).
	wantTotal := []model.CostItem{{Currency: "CNY", Amount: 16.5}}

	summary, err := us.ProjectSummary("proj_1")
	if err != nil {
		t.Fatalf("ProjectSummary: %v", err)
	}
	if !reflect.DeepEqual(summary.Total.Costs, wantTotal) {
		t.Fatalf("total costs = %+v, want %+v", summary.Total.Costs, wantTotal)
	}

	// Per-model: claude-sonnet-4-5 → CNY 16.5 ; claude-haiku-4-5 → no costs.
	var sonnet *model.ModelUsage
	for i := range summary.ByModel {
		if summary.ByModel[i].Model == "claude-sonnet-4-5" {
			sonnet = &summary.ByModel[i]
		}
	}
	if sonnet == nil || !reflect.DeepEqual(sonnet.Costs, wantTotal) {
		t.Fatalf("by-model sonnet costs = %+v, want %+v", sonnet, wantTotal)
	}

	// Per-requirement: req_a=10.5, req_b=3 (cache_read falls back to input_price), req_c=3+0.
	byReq := map[string][]model.CostItem{}
	for _, r := range summary.ByRequirement {
		byReq[r.RequirementID] = r.Costs
	}
	for rid, wantAmt := range map[string]float64{"req_a": 10.5, "req_b": 3, "req_c": 3} {
		if len(byReq[rid]) != 1 || byReq[rid][0].Amount != wantAmt {
			t.Fatalf("req %s costs = %+v, want CNY %.2f", rid, byReq[rid], wantAmt)
		}
	}

	// Per-day: rows A+B land on `now`'s date, rows C on the next day.
	if got := len(summary.ByDay); got != 2 {
		t.Fatalf("ByDay len = %d, want 2 (two distinct dates)", got)
	}
	if summary.ByDay[0].Date <= summary.ByDay[1].Date {
		t.Fatalf("ByDay not descending: %+v", summary.ByDay)
	}

	// Edit a price → cost recomputes ("设置后生效").
	if _, err := d.Exec(
		`UPDATE claude_configs SET models=? WHERE id='ccfg_priced'`,
		`[{"model":"claude-sonnet-4-5","input_price":6,"output_price":15}]`,
	); err != nil {
		t.Fatalf("update price: %v", err)
	}
	summary2, err := us.ProjectSummary("proj_1")
	if err != nil {
		t.Fatalf("ProjectSummary: %v", err)
	}
	// After the edit: A: 1M×6 + 0.5M×15 = 13.5 ; B: cache_read 1M×6 = 6 ; C: 1M×6 = 6 → 25.5
	want2 := []model.CostItem{{Currency: "CNY", Amount: 25.5}}
	if !reflect.DeepEqual(summary2.Total.Costs, want2) {
		t.Fatalf("after price edit total = %+v, want %+v", summary2.Total.Costs, want2)
	}
}

// TestUsageCostCacheReadPrice exercises the cache_read_price field end-to-end.
// When the operator sets cache_read_price, cache_read rows price against that
// rate instead of the (default) input_price; cache_creation continues to
// bill at input_price (no separate rate yet).
func TestUsageCostCacheReadPrice(t *testing.T) {
	d := usageTestDB(t)
	us := NewUsageService(d)

	// input_price=3, output_price=15, cache_read_price=0.3 (1/10 of input).
	if _, err := d.Exec(
		"INSERT INTO claude_configs (id,name,base_url,auth_token,models,default_model,currency,is_active,created_at,updated_at) VALUES (?,?,'','',?,'claude-sonnet-4-5','CNY',0,?,?)",
		"ccfg_cache", "ccfg_cache",
		`[{"model":"claude-sonnet-4-5","input_price":3,"output_price":15,"cache_read_price":0.3}]`,
		time.Now(), time.Now(),
	); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	now := time.Now()
	rows := []model.TokenUsage{
		// Fresh input: 1M tokens × input_price 3 = 3.
		{RequirementID: "req_x", ProjectID: "proj_1", Model: "claude-sonnet-4-5", ClaudeConfigID: "ccfg_cache", Currency: "CNY",
			InputTokens: 1_000_000, CreatedAt: now},
		// Cache creation: 1M tokens × input_price 3 (cache_creation bills at
		// the input rate, no separate rate yet).
		{RequirementID: "req_x", ProjectID: "proj_1", Model: "claude-sonnet-4-5", ClaudeConfigID: "ccfg_cache", Currency: "CNY",
			CacheCreationTokens: 1_000_000, CreatedAt: now.Add(time.Minute)},
		// Cache read: 1M tokens × cache_read_price 0.3 = 0.3 — the new feature.
		{RequirementID: "req_x", ProjectID: "proj_1", Model: "claude-sonnet-4-5", ClaudeConfigID: "ccfg_cache", Currency: "CNY",
			CacheReadTokens: 1_000_000, CreatedAt: now.Add(2 * time.Minute)},
		// Output: 0.5M × output_price 15 = 7.5.
		{RequirementID: "req_x", ProjectID: "proj_1", Model: "claude-sonnet-4-5", ClaudeConfigID: "ccfg_cache", Currency: "CNY",
			OutputTokens: 500_000, CreatedAt: now.Add(3 * time.Minute)},
	}
	for _, u := range rows {
		if err := us.Record(u); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// Expected: 3 + 3 + 0.3 + 7.5 = 13.8 CNY.
	wantTotal := []model.CostItem{{Currency: "CNY", Amount: 13.8}}

	summary, err := us.ProjectSummary("proj_1")
	if err != nil {
		t.Fatalf("ProjectSummary: %v", err)
	}
	if !reflect.DeepEqual(summary.Total.Costs, wantTotal) {
		t.Fatalf("cache_read_price total = %+v, want %+v", summary.Total.Costs, wantTotal)
	}

	// Edit only cache_read_price → historical cache_read rows re-price.
	if _, err := d.Exec(
		`UPDATE claude_configs SET models=? WHERE id='ccfg_cache'`,
		`[{"model":"claude-sonnet-4-5","input_price":3,"output_price":15,"cache_read_price":0.6}]`,
	); err != nil {
		t.Fatalf("update cache_read_price: %v", err)
	}
	summary2, err := us.ProjectSummary("proj_1")
	if err != nil {
		t.Fatalf("ProjectSummary: %v", err)
	}
	// Only the cache_read row's cost changed: 0.3 → 0.6 → +0.3, total 14.1.
	want2 := []model.CostItem{{Currency: "CNY", Amount: 14.1}}
	if !reflect.DeepEqual(summary2.Total.Costs, want2) {
		t.Fatalf("after cache_read_price edit total = %+v, want %+v", summary2.Total.Costs, want2)
	}
}