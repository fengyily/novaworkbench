package service

import (
	"encoding/json"
	"fmt"
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
		"INSERT INTO token_usage (id,requirement_id,project_id,job_id,step,model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,meta,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		u.ID, nullableString(u.RequirementID), u.ProjectID, u.JobID, u.Step, u.Model,
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

// StepsByRequirement returns per-step aggregates for one requirement.
func (s *UsageService) StepsByRequirement(reqID string) ([]model.StepUsage, error) {
	rows, err := s.db.Query(
		"SELECT step, SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens), COUNT(*) FROM token_usage WHERE requirement_id=? GROUP BY step ORDER BY MIN(created_at)",
		reqID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []model.StepUsage
	for rows.Next() {
		var su model.StepUsage
		if err := rows.Scan(&su.Step, &su.InputTokens, &su.OutputTokens, &su.CacheCreationTokens, &su.CacheReadTokens, &su.Count); err != nil {
			return nil, err
		}
		su.Label = stepLabel(su.Step)
		items = append(items, su)
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
	return t, err
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
	rows, err := s.db.Query(
		"SELECT requirement_id, SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens) FROM token_usage WHERE project_id=? AND step!='review' AND requirement_id IS NOT NULL GROUP BY requirement_id ORDER BY SUM(input_tokens+output_tokens+cache_creation_tokens+cache_read_tokens) DESC",
		projID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []model.ReqUsage
	for rows.Next() {
		var r model.ReqUsage
		if err := rows.Scan(&r.RequirementID, &r.InputTokens, &r.OutputTokens, &r.CacheCreationTokens, &r.CacheReadTokens); err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	if items == nil {
		items = []model.ReqUsage{}
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
	Total         model.UsageTotals `json:"total"`
	ByRequirement []model.ReqUsage  `json:"by_requirement"`
	Review        []model.ReviewUsage `json:"review"`
}

// ProjectSummary returns the project total (excl review), per-requirement
// totals, and the review breakdown.
func (s *UsageService) ProjectSummary(projID string) (ProjectSummary, error) {
	total, err := s.totals("project_id=? AND step!='review'", projID)
	if err != nil {
		return ProjectSummary{}, err
	}
	byReq, err := s.RequirementsByProject(projID)
	if err != nil {
		return ProjectSummary{}, fmt.Errorf("by-requirement: %w", err)
	}
	review, err := s.ReviewRowsByProject(projID)
	if err != nil {
		return ProjectSummary{}, fmt.Errorf("review: %w", err)
	}
	return ProjectSummary{Total: total, ByRequirement: byReq, Review: review}, nil
}
