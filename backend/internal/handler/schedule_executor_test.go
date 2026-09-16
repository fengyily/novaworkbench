package handler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/scheduler"
	"github.com/novaworkbench/backend/internal/service"
)

// TestRunScheduledCoding_StatusTransition pins the fix for
// req_30080193f1c95255 where a `succeeded` scheduled coding task left
// requirements.status stuck at 'designed' because the case "designed"
// branch's UpdateStatus('developing') was silently skipped under certain
// timing windows (succeeded schedule + designed requirement + done
// sub_tasks). The fix (schedule_executor.go:85-117) hoists the
// UpdateStatus call BEFORE the switch, so both `designed` and
// `draft+skip_design` paths write the transition unconditionally.
//
// Three cases covered:
//
//	case A: req.Status='designed'                  → UpdateStatus('developing') called
//	case B: req.Status='draft' && SkipDesign=true  → UpdateStatus('developing') called
//	case C: req.Status='draft' && SkipDesign=false → REJECTED, UpdateStatus NOT called
//
// The test stops at the dispatchFromCtx boundary by passing
// context.Background() — RunScheduledCoding returns "scheduler ctx not
// provided" right after the status gate for cases A/B, and "未生成技术方案"
// before the gate for case C. Either way we observe the side effect on
// requirements.status to verify whether UpdateStatus fired.
func TestRunScheduledCoding_StatusTransition(t *testing.T) {
	dir, err := os.MkdirTemp("", "sched-exec-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	defer os.RemoveAll(dir)
	cfg := db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")}
	d, err := db.Init(cfg)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	reqSvc := service.NewRequirementService(d)
	schedSvc := service.NewScheduledTaskService(d)

	// WizardHandler with ONLY reqSvc populated. The happy path of
	// RunScheduledCoding would call h.RunScheduledCoding (which needs
	// jobs / llmGateway / etc.), but we never reach that point:
	// context.Background() has no SchedCtxValue, so dispatchFromCtx
	// fails with "scheduler ctx not provided" right after the status
	// gate for cases A/B. WizardHandler fields beyond reqSvc stay nil
	// and are never touched.
	h := &WizardHandler{reqSvc: reqSvc}
	exec := NewScheduledExecutor(h, schedSvc)

	// Seed three requirements, one per case. FK chain: requirements
	// references projects; project row must exist.
	seedRequirementForExec(t, d, "req_designed", "designed", false)
	seedRequirementForExec(t, d, "req_draft_skip", "draft", true)
	seedRequirementForExec(t, d, "req_draft_noskip", "draft", false)

	ctx := context.Background()

	t.Run("case A designed → UpdateStatus developing fires", func(t *testing.T) {
		_, err := exec.RunScheduledCoding(ctx, scheduler.CodingParams{
			RequirementID: "req_designed",
		})
		// After the status gate passes, dispatchFromCtx fails because the
		// ctx carries no SchedCtxValue. That error proves we got past
		// the gate. The actual contract being asserted is the DB side
		// effect below.
		if err == nil {
			t.Fatalf("expected dispatch error (no SchedCtxValue); got nil")
		}
		if strings.Contains(err.Error(), "未生成技术方案") {
			t.Fatalf("case A: status gate rejected the row; err = %v", err)
		}
		req, gerr := reqSvc.Get("req_designed")
		if gerr != nil {
			t.Fatalf("get req after exec: %v", gerr)
		}
		if req.Status != "developing" {
			t.Fatalf("case A: requirements.status = %q, want %q (UpdateStatus did not fire)",
				req.Status, "developing")
		}
	})

	t.Run("case B draft + skip_design=true → UpdateStatus developing fires", func(t *testing.T) {
		_, err := exec.RunScheduledCoding(ctx, scheduler.CodingParams{
			RequirementID: "req_draft_skip",
		})
		if err == nil {
			t.Fatalf("expected dispatch error (no SchedCtxValue); got nil")
		}
		if strings.Contains(err.Error(), "未生成技术方案") {
			t.Fatalf("case B: status gate rejected the row (skip_design should bypass); err = %v", err)
		}
		req, gerr := reqSvc.Get("req_draft_skip")
		if gerr != nil {
			t.Fatalf("get req after exec: %v", gerr)
		}
		if req.Status != "developing" {
			t.Fatalf("case B: requirements.status = %q, want %q (UpdateStatus did not fire on skip-design path)",
				req.Status, "developing")
		}
	})

	t.Run("case C draft + skip_design=false → REJECTED, UpdateStatus NOT called", func(t *testing.T) {
		_, err := exec.RunScheduledCoding(ctx, scheduler.CodingParams{
			RequirementID: "req_draft_noskip",
		})
		if err == nil {
			t.Fatalf("expected status-gate error; got nil")
		}
		if !strings.Contains(err.Error(), "未生成技术方案") {
			t.Fatalf("case C: expected error to mention '未生成技术方案', got %v", err)
		}
		// Critical: the row MUST NOT have been mutated. UpdateStatus must
		// only fire on the two legal-entry paths (designed /
		// draft+skip_design); the unconditional write that fired on
		// case C would be a regression.
		req, gerr := reqSvc.Get("req_draft_noskip")
		if gerr != nil {
			t.Fatalf("get req after exec: %v", gerr)
		}
		if req.Status != "draft" {
			t.Fatalf("case C: requirements.status = %q, want %q (UpdateStatus must not fire on the rejected path)",
				req.Status, "draft")
		}
	})
}

// seedRequirementForExec inserts the bare project + requirement pair
// RunScheduledCoding's pre-gate lookup needs, with explicit status /
// skip_design overrides so each test case can sit at the lifecycle
// boundary it asserts on. Mirrors the helper in
// schedule_create_list_test.go (seedRequirementForSchedule) but
// parameterises the two fields that matter for the post-mortem.
func seedRequirementForExec(t *testing.T, d *db.DB, id, status string, skipDesign bool) {
	t.Helper()
	projID := "proj_" + id
	skipInt := 0
	if skipDesign {
		skipInt = 1
	}
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "Seed", "/tmp/seed_"+id, "ready", "main",
	); err != nil {
		t.Fatalf("insert project for %s: %v", id, err)
	}
	if _, err := d.Exec(
		`INSERT INTO requirements (id, project_id, title, status, skip_design) VALUES (?, ?, ?, ?, ?)`,
		id, projID, "Seed req", status, skipInt,
	); err != nil {
		t.Fatalf("insert requirement %s: %v", id, err)
	}
}