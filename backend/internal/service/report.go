package service

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// DefaultWeeklyReportRule is the built-in generation rule template. It is
// returned when a project has no custom rule saved; users can edit and save a
// per-project override (stored in the settings table).
const DefaultWeeklyReportRule = `请按以下结构撰写周报（Markdown）：

## 本周进展
按工作主题归纳 git 提交，说明完成了什么（不要逐条罗列提交，要提炼成工作项）。

## 需求动态
本周新增 / 完成 / 推进中的需求变更。

## 数据统计
提交数、涉及贡献者、需求状态数量概览（表格形式）。

## 下周计划
基于本周未完成的工作和进行中的需求，推测下周重点。

## 风险与阻塞
从提交和需求状态中识别潜在风险；没有则写"无"。`

// CompactWeeklyReportRule is the built-in "简洁" preset: progress only, no
// other sections. Exposed via the rule-presets endpoint so the UI can fill
// the rule editor with one click.
const CompactWeeklyReportRule = `只输出一个「## 本周进展」板块：按工作主题归纳 git 提交的改动，每项 1-2 句话说明完成了什么。不要输出数据统计、需求动态、下周计划、风险等任何其它板块，不要输出额外说明。`

// RulePresets returns the named built-in rule templates offered in the UI.
func RulePresets() map[string]string {
	return map[string]string{
		"standard": DefaultWeeklyReportRule,
		"compact":  CompactWeeklyReportRule,
	}
}

type ReportService struct {
	db *sql.DB
}

func NewReportService(db *sql.DB) *ReportService { return &ReportService{db: db} }

// ruleKey is the settings-table key holding a project's custom rule template.
func ruleKey(projectID string) string { return "weekly_report_rule_" + projectID }

// GetRule returns the project's custom rule template, or the built-in default
// when none is saved (or the saved one is blank).
func (s *ReportService) GetRule(projectID string) string {
	var v string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", ruleKey(projectID)).Scan(&v)
	if err != nil || strings.TrimSpace(v) == "" {
		return DefaultWeeklyReportRule
	}
	return v
}

// SaveRule persists a custom rule template for the project.
func (s *ReportService) SaveRule(projectID, rule string) error {
	_, err := s.db.Exec(
		"INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = ?, updated_at = ?",
		ruleKey(projectID), rule, time.Now(), rule, time.Now())
	return err
}

func (s *ReportService) List(projectID string) ([]model.WeeklyReport, error) {
	rows, err := s.db.Query(
		"SELECT id, project_id, period_start, period_end, git_branch, git_author, rule, content, status, created_at FROM weekly_reports WHERE project_id = ? ORDER BY created_at DESC",
		projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []model.WeeklyReport
	for rows.Next() {
		var r model.WeeklyReport
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.PeriodStart, &r.PeriodEnd, &r.GitBranch, &r.GitAuthor, &r.Rule, &r.Content, &r.Status, &r.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	if items == nil {
		items = []model.WeeklyReport{}
	}
	return items, nil
}

func (s *ReportService) Get(id string) (*model.WeeklyReport, error) {
	var r model.WeeklyReport
	err := s.db.QueryRow(
		"SELECT id, project_id, period_start, period_end, git_branch, git_author, rule, content, status, created_at FROM weekly_reports WHERE id = ?", id).
		Scan(&r.ID, &r.ProjectID, &r.PeriodStart, &r.PeriodEnd, &r.GitBranch, &r.GitAuthor, &r.Rule, &r.Content, &r.Status, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("weekly report not found")
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *ReportService) Create(projectID, periodStart, periodEnd, gitBranch, gitAuthor, rule, content, status string) (*model.WeeklyReport, error) {
	id := util.NewID("rpt")
	_, err := s.db.Exec(
		"INSERT INTO weekly_reports (id, project_id, period_start, period_end, git_branch, git_author, rule, content, status, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
		id, projectID, periodStart, periodEnd, gitBranch, gitAuthor, rule, content, status, time.Now())
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *ReportService) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM weekly_reports WHERE id = ?", id)
	return err
}

// WeeklyRequirementStats aggregates requirement activity for the project since
// `since` into a Chinese text block ready to embed in the report prompt.
// Returns the text block and the number of requirements that had any activity.
func (s *ReportService) WeeklyRequirementStats(projectID string, since time.Time) (string, int, error) {
	type reqRow struct {
		title  string
		status string
	}
	fetch := func(where string, args ...interface{}) ([]reqRow, error) {
		rows, err := s.db.Query(
			"SELECT title, status FROM requirements WHERE project_id = ? AND status != 'archived' AND "+where+" ORDER BY updated_at DESC LIMIT 50",
			append([]interface{}{projectID}, args...)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []reqRow
		for rows.Next() {
			var r reqRow
			if err := rows.Scan(&r.title, &r.status); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	}

	created, err := fetch("created_at >= ?", since)
	if err != nil {
		return "", 0, err
	}
	completed, err := fetch("completed_at >= ?", since)
	if err != nil {
		return "", 0, err
	}
	// Updated this week but neither created nor completed this week — these are
	// the "in-flight" requirements that moved forward (analysis/design/coding).
	updated, err := fetch("updated_at >= ? AND created_at < ? AND (completed_at IS NULL OR completed_at < ?)", since, since, since)
	if err != nil {
		return "", 0, err
	}

	var activeCount int
	s.db.QueryRow(
		"SELECT COUNT(*) FROM requirements WHERE project_id = ? AND status IN ('draft','analyzing','designing','designed','developing')",
		projectID).Scan(&activeCount)

	statusLabel := map[string]string{
		"draft": "草稿", "analyzing": "需求分析中",
		"designing": "方案设计中", "designed": "方案完成", "developing": "开发中", "done": "开发完成",
	}
	listLines := func(rows []reqRow) string {
		if len(rows) == 0 {
			return "（无）"
		}
		var b strings.Builder
		for _, r := range rows {
			label := r.status
			if l, ok := statusLabel[r.status]; ok {
				label = l
			}
			fmt.Fprintf(&b, "- %s（%s）\n", r.title, label)
		}
		return strings.TrimRight(b.String(), "\n")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "本周新创建（%d）：\n%s\n\n", len(created), listLines(created))
	fmt.Fprintf(&b, "本周完成（%d）：\n%s\n\n", len(completed), listLines(completed))
	fmt.Fprintf(&b, "本周有推进（%d）：\n%s\n\n", len(updated), listLines(updated))
	fmt.Fprintf(&b, "当前进行中需求总数：%d", activeCount)

	activity := len(created) + len(completed) + len(updated)
	return b.String(), activity, nil
}
