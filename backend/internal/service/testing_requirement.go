package service

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/novaworkbench/backend/internal/model"
)

// SourcePlatformAutoTest marks requirements created from a failing
// automated-test case, so the origin is distinguishable from a GitHub Issue
// import and can be filtered later.
const SourcePlatformAutoTest = "autotest"

// CreateFromTestCase turns a failing Playwright case into a local
// requirement, closing the loop from "the test found a bug" back into the
// normal 开发 pipeline.
//
// The requirement is created with skip-analysis=true and skip-design=false:
// the failing test already encodes a precise, verified reproduction, so the
// analyst stage would add nothing, but designing the fix is exactly what
// should happen next.
//
// Idempotency is handled by the CALLER (the handler checks case.RequirementID
// first and AttachRequirementID is a conditional UPDATE). This function is
// the single place the field values are assembled, so both callers cannot
// drift.
func (s *RequirementService) CreateFromTestCase(projectID string, tc *model.TestRunCase, run *model.TestRun) (string, error) {
	title := fmt.Sprintf("[自动化测试失败] %s", tc.Title)
	if len(title) > 255 {
		title = title[:255]
	}

	description := buildTestCaseDescription(tc, run)

	// source_data keeps the audit trail of what produced this requirement,
	// mirroring the issue-monitor's createRequirementFromIssue.
	payload, _ := json.Marshal(map[string]any{
		"autotest_case_id": tc.ID,
		"autotest_run_id":  run.ID,
		"file":             tc.File,
		"case_status":      tc.Status,
		"duration_ms":      tc.DurationMs,
		"error":            tc.Error,
		"attachments":      tc.AttachmentList,
	})

	req := model.CreateRequirementReq{
		ProjectID:   projectID,
		Title:       title,
		Description: description,
		Priority:    "high", // a failing automated test is a confirmed defect
		Kind:        KindIssue,
		// The failing test IS the analysis: it reproduces the defect against
		// the real app. Skip it; keep the design step.
		SkipAnalysis:   ptrBool(true),
		SkipDesign:     ptrBool(false),
		SkipOrganize:   ptrBool(true),
		SourcePlatform: SourcePlatformAutoTest,
		// Stable per case id, so the same failure can never be filed twice
		// even if the UI guard is bypassed.
		SourceExternalID: tc.ID,
		SourceState:      tc.Status,
		SourceData:       string(payload),
	}
	created, err := s.Create(req)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

// buildTestCaseDescription renders the failing case as a requirement body:
// the error, the spec file, and the run context needed to reproduce it.
func buildTestCaseDescription(tc *model.TestRunCase, run *model.TestRun) string {
	var b []byte
	appendLine := func(format string, args ...any) {
		b = append(b, []byte(fmt.Sprintf(format, args...))...)
		b = append(b, '\n')
	}

	appendLine("由 NovaWorkbench 自动化测试自动创建。\n")
	appendLine("- 测试文件：`%s`", tc.File)
	appendLine("- 用例标题：%s", tc.Title)
	appendLine("- 运行 ID：`%s`", run.ID)
	if !run.StartedAt.IsZero() {
		appendLine("- 运行时间：%s", run.StartedAt.Format("2006-01-02 15:04:05"))
	}
	if tc.Error != "" {
		appendLine("\n## 失败原因\n")
		appendLine("```\n%s\n```\n", tc.Error)
	}
	if len(tc.AttachmentList) > 0 {
		appendLine("\n## 证据文件\n")
		for _, att := range tc.AttachmentList {
			appendLine("- %s（`%s`）", att.Name, att.Path)
		}
	}
	appendLine("\n> 在「项目 → 自动化测试」中查看完整运行记录与截图/trace。")
	return string(b)
}

// findRequirementByTestCase looks up an existing requirement created from a
// case, so a caller that bypassed the UI guard can still detect the duplicate.
func (s *RequirementService) findRequirementByTestCase(projectID, caseID string) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT id FROM requirements
		 WHERE project_id = ? AND source_platform = ? AND source_external_id = ?
		 LIMIT 1`,
		projectID, SourcePlatformAutoTest, caseID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}
