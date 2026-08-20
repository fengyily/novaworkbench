package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// UsageService records and aggregates LLM token consumption per claude CLI /
// HTTP LLM invocation. One row per invocation; callers aggregate by step /
// requirement / project. Review rows (requirement_id="") are never counted
// in requirement-level or project-level totals — only surfaced in the
// project review breakdown.
//
// Cost is NOT stored: each row records the claude_config_id (platform) that
// served the request, and prices are recomputed at query time from that
// config's current model entries, grouped by currency.
type UsageService struct {
	db *db.DB
}

func NewUsageService(database *db.DB) *UsageService {
	return &UsageService{db: database}
}

// Record persists one token-usage row. Best-effort by contract: callers wrap
// errors so a recording failure never breaks the claude turn.
func (s *UsageService) Record(u model.TokenUsage) error {
	if u.ID == "" {
		u.ID = util.NewID("tu")
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(
		"INSERT INTO token_usage (id,requirement_id,project_id,job_id,step,model,claude_config_id,currency,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,meta,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		u.ID, nullableString(u.RequirementID), u.ProjectID, u.JobID, u.Step, u.Model,
		u.ClaudeConfigID, u.Currency,
		u.InputTokens, u.OutputTokens, u.CacheCreationTokens, u.CacheReadTokens, u.Meta, u.CreatedAt)
	return err
}

// nullableString returns nil for an empty string so the column stores NULL
// (review rows have a NULL requirement_id, which the schema allows and which
// keeps NOT NULL-agnostic queries clean).
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// StepLabels maps a step code to its Chinese display label. Shared with the
// frontend's stepLabels; the API joins it onto StepUsage rows so the client
// doesn't have to maintain a parallel map.
var StepLabels = map[string]string{
	"requirement_create": "需求整理",
	"analyst_chat":        "需求分析",
	"architect_design":    "技术方案",
	"refine_doc":          "方案精炼",
	"apply_doc":           "方案应用",
	"coding":              "编码开发",
	"adjust_coding":       "追加调整",
	"continue_coding":     "继续开发",
	"developer_chat":      "开发讨论",
	"merge":               "合入解决",
	"review":              "代码审查",
}

func stepLabel(step string) string {
	if l, ok := StepLabels[step]; ok {
		return l
	}
	return step
}

// stepAgg folds the (step, model, config, currency) aggregation rows into one
// per-(step, model) entry, summing tokens and cost. It embeds costAgg for the
// token/cost bookkeeping and adds the label keys + invocation count.
type stepAgg struct {
	step, model string
	count       int
	costAgg
}

// StepsByRequirement returns per-step (and per-model) aggregates for one
// requirement. Cost is recomputed from the config's current unit prices, so
// grouping must include claude_config_id + currency (a step+model pair can span
// configs if the active platform changed between turns); the rows are then
// folded back into a single entry per (step, model).
func (s *UsageService) StepsByRequirement(reqID string) ([]model.StepUsage, error) {
	// Load price tables BEFORE the aggregation query (SQLite single-connection).
	tables := s.priceTablesBestEffort()

	rows, err := s.db.Query(
		"SELECT step, model, claude_config_id, currency, SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens), COUNT(*) FROM token_usage WHERE requirement_id=? GROUP BY step, model, claude_config_id, currency ORDER BY MIN(created_at)",
		reqID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	acc := map[string]*stepAgg{}
	var order []string
	for rows.Next() {
		var step, model, cid, currency string
		var in, out, cc, cr, count int
		if err := rows.Scan(&step, &model, &cid, &currency, &in, &out, &cc, &cr, &count); err != nil {
			return nil, err
		}
		key := step + "\x00" + model
		a, ok := acc[key]
		if !ok {
			a = &stepAgg{step: step, model: model}
			acc[key] = a
			order = append(order, key)
		}
		a.add(in, out, cc, cr)
		a.count += count
		a.addCost(currency, cid, model, in, out, cc, cr, tables)
	}

	items := make([]model.StepUsage, 0, len(acc))
	for _, key := range order {
		a := acc[key]
		items = append(items, model.StepUsage{
			Step:                a.step,
			Label:               stepLabel(a.step),
			Model:               a.model,
			InputTokens:         a.in,
			OutputTokens:        a.out,
			CacheCreationTokens: a.cc,
			CacheReadTokens:     a.cr,
			Count:               a.count,
			Costs:               a.done(),
		})
	}
	if items == nil {
		items = []model.StepUsage{}
	}
	return items, rows.Err()
}

// requirementTotals sums all non-review rows for a filter clause.
func (s *UsageService) totals(where string, args ...any) (model.UsageTotals, error) {
	var t model.UsageTotals
	err := s.db.QueryRow(
		"SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(cache_read_tokens),0) FROM token_usage WHERE "+where,
		args...).Scan(&t.InputTokens, &t.OutputTokens, &t.CacheCreationTokens, &t.CacheReadTokens)
	if err != nil {
		return t, err
	}
	t.Costs = s.costForRows(where, args)
	return t, nil
}

// RequirementSummary is the requirement-detail payload.
type RequirementSummary struct {
	ByStep []model.StepUsage  `json:"by_step"`
	Total  model.UsageTotals `json:"total"`
}

// RequirementSummary returns the per-step breakdown + total for a requirement.
func (s *UsageService) RequirementSummary(reqID string) (RequirementSummary, error) {
	steps, err := s.StepsByRequirement(reqID)
	if err != nil {
		return RequirementSummary{}, err
	}
	total, err := s.totals("requirement_id=?", reqID)
	if err != nil {
		return RequirementSummary{}, err
	}
	return RequirementSummary{ByStep: steps, Total: total}, nil
}

// RequirementsByProject returns per-requirement totals for a project,
// excluding review rows (which carry no requirement_id).
func (s *UsageService) RequirementsByProject(projID string) ([]model.ReqUsage, error) {
	// Load price tables BEFORE opening the aggregation query: SQLite runs with
	// MaxOpenConns=1, so a second query while rows is open would deadlock.
	tables := s.priceTablesBestEffort()

	rows, err := s.db.Query(
		"SELECT requirement_id, claude_config_id, currency, model, SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens) FROM token_usage WHERE project_id=? AND step!='review' AND requirement_id IS NOT NULL GROUP BY requirement_id, claude_config_id, currency, model ORDER BY SUM(input_tokens+output_tokens+cache_creation_tokens+cache_read_tokens) DESC",
		projID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	acc := map[string]*costAgg{}
	var order []string
	for rows.Next() {
		var rid, cid, currency, mn string
		var in, out, cc, cr int
		if err := rows.Scan(&rid, &cid, &currency, &mn, &in, &out, &cc, &cr); err != nil {
			return nil, err
		}
		a, ok := acc[rid]
		if !ok {
			a = &costAgg{}
			acc[rid] = a
			order = append(order, rid)
		}
		a.add(in, out, cc, cr)
		a.addCost(currency, cid, mn, in, out, cc, cr, tables)
	}

	items := make([]model.ReqUsage, 0, len(acc))
	for _, rid := range order {
		a := acc[rid]
		items = append(items, model.ReqUsage{
			RequirementID:       rid,
			InputTokens:         a.in,
			OutputTokens:        a.out,
			CacheCreationTokens: a.cc,
			CacheReadTokens:     a.cr,
			Costs:               a.done(),
		})
	}
	if items == nil {
		items = []model.ReqUsage{}
	}
	return items, rows.Err()
}

// ModelsByProject returns per-model token totals + cost for a project,
// excluding review rows.
func (s *UsageService) ModelsByProject(projID string) ([]model.ModelUsage, error) {
	// Load price tables BEFORE the aggregation query (SQLite single-connection).
	tables := s.priceTablesBestEffort()

	rows, err := s.db.Query(
		"SELECT model, claude_config_id, currency, SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens) FROM token_usage WHERE project_id=? AND step!='review' GROUP BY model, claude_config_id, currency ORDER BY SUM(input_tokens+output_tokens+cache_creation_tokens+cache_read_tokens) DESC",
		projID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	acc := map[string]*costAgg{}
	var order []string
	for rows.Next() {
		var mn, cid, currency string
		var in, out, cc, cr int
		if err := rows.Scan(&mn, &cid, &currency, &in, &out, &cc, &cr); err != nil {
			return nil, err
		}
		a, ok := acc[mn]
		if !ok {
			a = &costAgg{}
			acc[mn] = a
			order = append(order, mn)
		}
		a.add(in, out, cc, cr)
		a.addCost(currency, cid, mn, in, out, cc, cr, tables)
	}

	items := make([]model.ModelUsage, 0, len(acc))
	for _, m := range order {
		a := acc[m]
		items = append(items, model.ModelUsage{
			Model:               m,
			InputTokens:         a.in,
			OutputTokens:        a.out,
			CacheCreationTokens: a.cc,
			CacheReadTokens:     a.cr,
			Costs:               a.done(),
		})
	}
	if items == nil {
		items = []model.ModelUsage{}
	}
	return items, rows.Err()
}

// dateExpr returns a dialect-specific expression that renders created_at as a
// zero-padded "YYYY-MM-DD" string. SQLite stores DATETIME as TEXT so substr()
// suffices; MySQL and PostgreSQL store a real temporal type (DATETIME /
// TIMESTAMP) and need DATE_FORMAT / to_char. The result is always TEXT so it
// scans into a Go string and orders lexicographically (correct for ISO dates).
func (s *UsageService) dateExpr() string {
	switch s.db.Dialect() {
	case db.Postgres:
		return "to_char(created_at, 'YYYY-MM-DD')"
	case db.MySQL:
		return "DATE_FORMAT(created_at, '%Y-%m-%d')"
	default:
		return "substr(created_at,1,10)"
	}
}

// DailyByProject returns per-day token totals + cost for a project (YYYY-MM-DD,
// descending), excluding review rows.
func (s *UsageService) DailyByProject(projID string) ([]model.DailyUsage, error) {
	// Load price tables BEFORE the aggregation query (SQLite single-connection).
	tables := s.priceTablesBestEffort()
	dayExpr := s.dateExpr()

	rows, err := s.db.Query(
		"SELECT "+dayExpr+", claude_config_id, currency, model, SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens) FROM token_usage WHERE project_id=? AND step!='review' GROUP BY "+dayExpr+", claude_config_id, currency, model ORDER BY "+dayExpr+" DESC",
		projID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	acc := map[string]*costAgg{}
	var order []string
	for rows.Next() {
		var day, cid, currency, mn string
		var in, out, cc, cr int
		if err := rows.Scan(&day, &cid, &currency, &mn, &in, &out, &cc, &cr); err != nil {
			return nil, err
		}
		a, ok := acc[day]
		if !ok {
			a = &costAgg{}
			acc[day] = a
			order = append(order, day)
		}
		a.add(in, out, cc, cr)
		a.addCost(currency, cid, mn, in, out, cc, cr, tables)
	}

	items := make([]model.DailyUsage, 0, len(acc))
	for _, day := range order {
		a := acc[day]
		items = append(items, model.DailyUsage{
			Date:                day,
			InputTokens:         a.in,
			OutputTokens:        a.out,
			CacheCreationTokens: a.cc,
			CacheReadTokens:     a.cr,
			Costs:               a.done(),
		})
	}
	if items == nil {
		items = []model.DailyUsage{}
	}
	return items, rows.Err()
}

// ReviewRowsByProject returns the project-level review token records
// (requirement_id is NULL). Meta is parsed into PR fields for display.
func (s *UsageService) ReviewRowsByProject(projID string) ([]model.ReviewUsage, error) {
	rows, err := s.db.Query(
		"SELECT id, job_id, meta, input_tokens, output_tokens, created_at FROM token_usage WHERE project_id=? AND step='review' ORDER BY created_at DESC",
		projID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []model.ReviewUsage
	for rows.Next() {
		var ru model.ReviewUsage
		var meta string
		if err := rows.Scan(&ru.ID, &ru.JobID, &meta, &ru.InputTokens, &ru.OutputTokens, &ru.CreatedAt); err != nil {
			return nil, err
		}
		ru.PRNumber, ru.PRTitle, ru.Branch = parseReviewMeta(meta)
		items = append(items, ru)
	}
	if items == nil {
		items = []model.ReviewUsage{}
	}
	return items, rows.Err()
}

func parseReviewMeta(meta string) (prNumber int, prTitle, branch string) {
	if meta == "" {
		return
	}
	var m struct {
		PRNumber int    `json:"pr_number"`
		PRTitle  string `json:"pr_title"`
		Branch   string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(meta), &m); err == nil {
		return m.PRNumber, m.PRTitle, m.Branch
	}
	return
}

// ProjectSummary is the project-usage payload. Total excludes review rows;
// the review breakdown is reported separately.
type ProjectSummary struct {
	Total         model.UsageTotals   `json:"total"`
	ByRequirement []model.ReqUsage    `json:"by_requirement"`
	ByModel       []model.ModelUsage  `json:"by_model"`
	ByDay         []model.DailyUsage  `json:"by_day"`
	Review        []model.ReviewUsage `json:"review"`
}

// ProjectSummary returns the project total (excl review), per-requirement and
// per-model and per-day totals, and the review breakdown.
func (s *UsageService) ProjectSummary(projID string) (ProjectSummary, error) {
	total, err := s.totals("project_id=? AND step!='review'", projID)
	if err != nil {
		return ProjectSummary{}, err
	}
	byReq, err := s.RequirementsByProject(projID)
	if err != nil {
		return ProjectSummary{}, fmt.Errorf("by-requirement: %w", err)
	}
	byModel, err := s.ModelsByProject(projID)
	if err != nil {
		return ProjectSummary{}, fmt.Errorf("by-model: %w", err)
	}
	byDay, err := s.DailyByProject(projID)
	if err != nil {
		return ProjectSummary{}, fmt.Errorf("by-day: %w", err)
	}
	review, err := s.ReviewRowsByProject(projID)
	if err != nil {
		return ProjectSummary{}, fmt.Errorf("review: %w", err)
	}
	return ProjectSummary{Total: total, ByRequirement: byReq, ByModel: byModel, ByDay: byDay, Review: review}, nil
}

// ---- Cost recomputation -----------------------------------------------------------

// modelPrice holds one model's unit prices on a config, per million tokens.
type modelPrice struct {
	input  float64
	output float64
}

// configPriceTable is the price lookup for one Claude config (platform): its
// currency + per-model unit prices, sourced from the config's models column.
type configPriceTable struct {
	currency string
	byModel  map[string]modelPrice
}

// loadPriceTables loads every claude config's current model prices keyed by
// config id. A config that no longer exists is simply absent — rows pointing to
// it then produce 0 cost (currency falls back to the row's snapshot).
func (s *UsageService) loadPriceTables() (map[string]configPriceTable, error) {
	rows, err := s.db.Query("SELECT id, currency, models FROM claude_configs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make(map[string]configPriceTable)
	for rows.Next() {
		var id, currency, modelsJSON string
		if err := rows.Scan(&id, &currency, &modelsJSON); err != nil {
			return nil, err
		}
		t := configPriceTable{currency: currency, byModel: make(map[string]modelPrice)}
		for _, e := range DecodeModels(modelsJSON) {
			t.byModel[e.Model] = modelPrice{input: e.InputPrice, output: e.OutputPrice}
		}
		tables[id] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

// priceTablesBestEffort loads the price tables, degrading to an empty map on
// any error so pricing hiccups never break token aggregation (cost just reads 0).
func (s *UsageService) priceTablesBestEffort() map[string]configPriceTable {
	tables, err := s.loadPriceTables()
	if err != nil {
		return map[string]configPriceTable{}
	}
	return tables
}

// costForRows recomputes the recomputed cost for a filtered row set, grouped by
// currency. Since cost depends on (config, model), it aggregates per config+
// model first, then folds into per-currency totals. Best-effort: returns nil on
// any lookup failure.
func (s *UsageService) costForRows(where string, args []any) []model.CostItem {
	tables := s.priceTablesBestEffort()
	costByCur := map[string]float64{}
	rows, err := s.db.Query(
		"SELECT claude_config_id, currency, model, SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens) FROM token_usage WHERE "+where+" GROUP BY claude_config_id, currency, model",
		args...)
	if err != nil {
		return []model.CostItem{}
	}
	defer rows.Close()
	for rows.Next() {
		var cid, currency, mn string
		var in, out, cc, cr int
		if err := rows.Scan(&cid, &currency, &mn, &in, &out, &cc, &cr); err != nil {
			return []model.CostItem{}
		}
		accumulateCost(costByCur, currency, cid, mn, in, out, cc, cr, tables)
	}
	return costsToItems(costByCur)
}

// accumulateCost folds one aggregated row (already grouped by config+model) into
// a per-currency cost bucket. Prices come from the config's CURRENT entries
// ("设置后生效": edits recompute past rows), currency from the row snapshot
// (falling back to the config's currency for pre-pricing rows).
func accumulateCost(costByCur map[string]float64, rowCurrency, configID, modelName string, in, out, cc, cr int, tables map[string]configPriceTable) {
	cfg, ok := tables[configID]
	if !ok {
		return // config deleted → no prices to recompute against (cost 0)
	}
	p, ok := cfg.byModel[modelName]
	if !ok {
		return // model not priced on this config (cost 0)
	}
	if p.input == 0 && p.output == 0 {
		return // legacy string-array entry with no unit prices → produces nothing
	}
	currency := rowCurrency
	if currency == "" {
		currency = cfg.currency
	}
	if currency == "" {
		return // no currency to attribute cost under
	}
	costByCur[currency] += billedInput(in, cc, cr)*p.input + outputCost(out)*p.output
}

// billedInput is (input + cache_creation + cache_read) / 1e6 — cache reads and
// creations are billed as input tokens (mirrors the frontend usageTotalInput).
func billedInput(in, cc, cr int) float64 {
	return float64(in+cc+cr) / 1e6
}

func outputCost(out int) float64 {
	return float64(out) / 1e6
}

// costsToItems renders a per-currency cost map as a deterministically-ordered
// []CostItem (empty when no currency bucket was populated).
func costsToItems(costByCur map[string]float64) []model.CostItem {
	if len(costByCur) == 0 {
		return []model.CostItem{}
	}
	currencies := make([]string, 0, len(costByCur))
	for c := range costByCur {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	items := make([]model.CostItem, 0, len(currencies))
	for _, c := range currencies {
		items = append(items, model.CostItem{Currency: c, Amount: costByCur[c]})
	}
	return items
}

// costAgg accumulates tokens + per-currency costs across the (config, model)
// rows that fold into one output key (a requirement / model / day).
type costAgg struct {
	in, out, cc, cr int
	costs           map[string]float64
}

func (a *costAgg) add(in, out, cc, cr int) {
	a.in += in
	a.out += out
	a.cc += cc
	a.cr += cr
}

func (a *costAgg) addCost(rowCurrency, configID, modelName string, in, out, cc, cr int, tables map[string]configPriceTable) {
	if a.costs == nil {
		a.costs = map[string]float64{}
	}
	accumulateCost(a.costs, rowCurrency, configID, modelName, in, out, cc, cr, tables)
}

func (a *costAgg) done() []model.CostItem {
	if a.costs == nil {
		return []model.CostItem{}
	}
	return costsToItems(a.costs)
}