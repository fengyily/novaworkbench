package handler

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// TestResolveLaunchTaskType pins the flow → scheduled_tasks.task_type
// mapping documented in requirements.design_docs ("flow → 启动类型映射").
// This is the single source of truth shared by Create's launch-spec
// dispatch (requirement.go) and the frontend's "启动计划" block — any
// divergence here will mis-route a fresh requirement into the wrong
// stage handler, so all 3 flows × 2 modes are pinned plus the two
// intentionally-rejected combinations (idea / full + immediate).
//
// "full + scheduled" is intentionally allowed (returns
// design_and_coding): the user may complete the analyst stage before
// the scheduled tick fires, and the scheduled-task failure surface
// already handles the NO_SESSION landing. The frontend surfaces this
// in a hint under the "定时执行" radio when flow=="full".
func TestResolveLaunchTaskType(t *testing.T) {
	cases := []struct {
		name         string
		req          *model.Requirement
		mode         string
		wantTaskType string
		wantErrCode  string // empty when no error expected
	}{
		{
			name: "direct flow + immediate → coding",
			req: &model.Requirement{
				Kind:        service.KindRequirement,
				SkipAnalysis: true,
				SkipDesign:   true,
			},
			mode:         "immediate",
			wantTaskType: model.SchedTypeCoding,
		},
		{
			name: "direct flow + scheduled → coding",
			req: &model.Requirement{
				Kind:        service.KindRequirement,
				SkipAnalysis: true,
				SkipDesign:   true,
			},
			mode:         "scheduled",
			wantTaskType: model.SchedTypeCoding,
		},
		{
			name: "skip-analysis flow + immediate → design_and_coding",
			req: &model.Requirement{
				Kind:        service.KindRequirement,
				SkipAnalysis: true,
				SkipDesign:   false,
			},
			mode:         "immediate",
			wantTaskType: model.SchedTypeDesignCoding,
		},
		{
			name: "skip-analysis flow + scheduled → design_and_coding",
			req: &model.Requirement{
				Kind:        service.KindRequirement,
				SkipAnalysis: true,
				SkipDesign:   false,
			},
			mode:         "scheduled",
			wantTaskType: model.SchedTypeDesignCoding,
		},
		{
			name: "full flow + immediate → 400 LAUNCH_NEEDS_ANALYSIS",
			req: &model.Requirement{
				Kind:        service.KindRequirement,
				SkipAnalysis: false,
				SkipDesign:   false,
			},
			mode:        "immediate",
			wantErrCode: launchErrNeedsAnalysis,
		},
		{
			name: "full flow + scheduled → design_and_coding (intentionally allowed)",
			req: &model.Requirement{
				Kind:        service.KindRequirement,
				SkipAnalysis: false,
				SkipDesign:   false,
			},
			mode:         "scheduled",
			wantTaskType: model.SchedTypeDesignCoding,
		},
		{
			name: "idea kind + immediate → 400 LAUNCH_NOT_ALLOWED",
			req: &model.Requirement{
				Kind:        service.KindIdea,
				SkipAnalysis: true,
				SkipDesign:   true,
			},
			mode:        "immediate",
			wantErrCode: launchErrNotAllowed,
		},
		{
			name: "idea kind + scheduled → 400 LAUNCH_NOT_ALLOWED",
			req: &model.Requirement{
				Kind:        service.KindIdea,
				SkipAnalysis: false,
				SkipDesign:   false,
			},
			mode:        "scheduled",
			wantErrCode: launchErrNotAllowed,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotTaskType, af := resolveLaunchTaskType(c.req, c.mode)
			if c.wantErrCode != "" {
				if af == nil {
					t.Fatalf("expected apiFailure code=%q, got nil", c.wantErrCode)
				}
				if af.Code != c.wantErrCode {
					t.Fatalf("apiFailure code = %q, want %q (msg=%q)", af.Code, c.wantErrCode, af.Msg)
				}
				if af.Status != 400 {
					t.Fatalf("apiFailure status = %d, want 400 (code=%q)", af.Status, af.Code)
				}
				if gotTaskType != "" {
					t.Fatalf("expected empty taskType on error, got %q", gotTaskType)
				}
				return
			}
			if af != nil {
				t.Fatalf("unexpected apiFailure: code=%q msg=%q", af.Code, af.Msg)
			}
			if gotTaskType != c.wantTaskType {
				t.Fatalf("taskType = %q, want %q", gotTaskType, c.wantTaskType)
			}
		})
	}
}

// TestResolveLaunchTaskTypeNilReq guards the defensive nil check at the
// top of resolveLaunchTaskType. The dispatch caller (requirement.go's
// Create) cannot reach this path with a nil req in practice — Create
// inserts the row first and only then calls dispatchLaunch — but the
// function is exported-by-symbolic-name to the dispatch helpers, so a
// future refactor that moves the call earlier MUST keep this guard.
func TestResolveLaunchTaskTypeNilReq(t *testing.T) {
	gotTaskType, af := resolveLaunchTaskType(nil, "immediate")
	if af == nil || af.Code != "INVALID" {
		t.Fatalf("nil req: expected apiFailure code=INVALID, got taskType=%q af=%v", gotTaskType, af)
	}
	if gotTaskType != "" {
		t.Fatalf("nil req: expected empty taskType, got %q", gotTaskType)
	}
}