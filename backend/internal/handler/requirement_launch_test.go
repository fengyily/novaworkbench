package handler

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// TestResolveLaunchTaskType pins the create-time 启动计划 mapping table —
// the one piece of genuinely new decision logic this feature adds. It must
// stay in lockstep with the two hard gates it exists to pre-empt:
//
//	wizard_architect.go NO_SESSION  — a fresh requirement with
//	  skip_analysis=false has no analyst session to fork, so the architect
//	  stage cannot run immediately.
//	gateCodingEntry                 — a draft requirement can only enter
//	  coding when skip_design is set.
//
// 3 flows × 2 modes = 6 cases, plus the kind=idea rejection for both modes.
func TestResolveLaunchTaskType(t *testing.T) {
	cases := []struct {
		name         string
		req          *model.Requirement
		mode         string
		wantTaskType string
		wantStatus   int
		wantCode     string
	}{
		// flow=direct (skip_analysis=true, skip_design=true) → 仅开发
		{
			name:         "direct immediate → coding",
			req:          &model.Requirement{Kind: "requirement", SkipAnalysis: true, SkipDesign: true},
			mode:         model.LaunchModeImmediate,
			wantTaskType: model.SchedTypeCoding,
		},
		{
			name:         "direct scheduled → coding",
			req:          &model.Requirement{Kind: "requirement", SkipAnalysis: true, SkipDesign: true},
			mode:         model.LaunchModeScheduled,
			wantTaskType: model.SchedTypeCoding,
		},
		// flow=skip-analysis (skip_analysis=true, skip_design=false) → 方案+开发
		{
			name:         "skip-analysis immediate → design_and_coding",
			req:          &model.Requirement{Kind: "requirement", SkipAnalysis: true},
			mode:         model.LaunchModeImmediate,
			wantTaskType: model.SchedTypeDesignCoding,
		},
		{
			name:         "skip-analysis scheduled → design_and_coding",
			req:          &model.Requirement{Kind: "requirement", SkipAnalysis: true},
			mode:         model.LaunchModeScheduled,
			wantTaskType: model.SchedTypeDesignCoding,
		},
		// flow=full (both flags false): immediate would hit NO_SESSION, so we
		// reject it up front with an actionable code. Scheduled is allowed on
		// purpose — the timer fires later, by which time the user may well
		// have finished the analyst chat.
		{
			name:       "full immediate → rejected (would hit NO_SESSION)",
			req:        &model.Requirement{Kind: "requirement"},
			mode:       model.LaunchModeImmediate,
			wantStatus: 400,
			wantCode:   launchErrNeedsAnalysis,
		},
		{
			name:         "full scheduled → design_and_coding",
			req:          &model.Requirement{Kind: "requirement"},
			mode:         model.LaunchModeScheduled,
			wantTaskType: model.SchedTypeDesignCoding,
		},
		// kind=idea is discussion-first and never developable, regardless of
		// mode or skip flags.
		{
			name:       "idea immediate → rejected",
			req:        &model.Requirement{Kind: service.KindIdea, SkipAnalysis: true, SkipDesign: true},
			mode:       model.LaunchModeImmediate,
			wantStatus: 400,
			wantCode:   launchErrIdea,
		},
		{
			name:       "idea scheduled → rejected",
			req:        &model.Requirement{Kind: service.KindIdea, SkipAnalysis: true, SkipDesign: true},
			mode:       model.LaunchModeScheduled,
			wantStatus: 400,
			wantCode:   launchErrIdea,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, af := resolveLaunchTaskType(c.req, c.mode)
			if c.wantCode != "" {
				if af == nil {
					t.Fatalf("expected failure %s, got task type %q", c.wantCode, got)
				}
				if af.Code != c.wantCode || af.Status != c.wantStatus {
					t.Fatalf("expected %d/%s, got %d/%s (%s)", c.wantStatus, c.wantCode, af.Status, af.Code, af.Msg)
				}
				if got != "" {
					t.Fatalf("failure must not return a task type, got %q", got)
				}
				return
			}
			if af != nil {
				t.Fatalf("unexpected failure %d/%s: %s", af.Status, af.Code, af.Msg)
			}
			if got != c.wantTaskType {
				t.Fatalf("task type = %q, want %q", got, c.wantTaskType)
			}
		})
	}
}

// TestDispatchLaunchRejectsBadMode ensures an unknown / missing launch.mode is
// refused before ANY dispatch work happens — the handler's wizard + schedule
// dependencies are nil here, so reaching either branch would panic (or return
// the "服务未初始化" 500) instead of the expected 400.
func TestDispatchLaunchRejectsBadMode(t *testing.T) {
	h := &RequirementHandler{}
	req := &model.Requirement{ID: "req_x", Kind: "requirement", SkipAnalysis: true, SkipDesign: true}
	for _, mode := range []string{"", "now", "IMMEDIATE"} {
		got, _, af := h.dispatchLaunch(req, &model.LaunchSpec{Mode: mode})
		if af == nil {
			t.Fatalf("mode %q: expected failure, got mode=%q", mode, got)
		}
		if af.Status != 400 || af.Code != launchErrInvalidMode {
			t.Fatalf("mode %q: expected 400/%s, got %d/%s", mode, launchErrInvalidMode, af.Status, af.Code)
		}
	}
}

// TestDispatchLaunchNilSpecIsNoop — an omitted `launch` key must leave the
// create path byte-for-byte identical to the pre-feature behavior.
func TestDispatchLaunchNilSpecIsNoop(t *testing.T) {
	h := &RequirementHandler{}
	mode, schedID, af := h.dispatchLaunch(&model.Requirement{ID: "req_x"}, nil)
	if af != nil || mode != "" || schedID != "" {
		t.Fatalf("nil spec must be a no-op, got mode=%q sched=%q af=%v", mode, schedID, af)
	}
}
