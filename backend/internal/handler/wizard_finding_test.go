package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

func TestParseFindingBullet(t *testing.T) {
	cases := []struct {
		name         string
		bullet       string
		wantTitle    string
		wantDetail   string
		wantSeverity string
	}{
		{
			name:         "contract shape",
			bullet:       "[冲突] 帧构造归属 —— protocol.c:871",
			wantTitle:    "帧构造归属",
			wantDetail:   "protocol.c:871",
			wantSeverity: service.FindingSeverityConflict,
		},
		{
			name:         "new fact",
			bullet:       "[新事实] 页地址池收窄到 1024",
			wantTitle:    "页地址池收窄到 1024",
			wantSeverity: service.FindingSeverityInfo,
		},
		{
			name:         "no bracket",
			bullet:       "SDK 的 holdAmounts 会夹取请求 —— SDK SetBatchParam.java:88",
			wantTitle:    "SDK 的 holdAmounts 会夹取请求",
			wantDetail:   "SDK SetBatchParam.java:88",
			wantSeverity: service.FindingSeverityInfo,
		},
		{
			name:         "deviation tag",
			bullet:       "[偏差] 字节布局由本子需求提供",
			wantTitle:    "字节布局由本子需求提供",
			wantSeverity: service.FindingSeverityDeviation,
		},
		{
			name:         "placeholder is not a finding",
			bullet:       "无",
			wantSeverity: service.FindingSeverityInfo,
		},
		{
			name:         "placeholder with punctuation",
			bullet:       "无。",
			wantSeverity: service.FindingSeverityInfo,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title, detail, severity := parseFindingBullet(c.bullet)
			if c.wantTitle == "" {
				if title != "" {
					t.Fatalf("title = %q, want empty", title)
				}
				return
			}
			if title != c.wantTitle {
				t.Errorf("title = %q, want %q", title, c.wantTitle)
			}
			if detail != c.wantDetail {
				t.Errorf("detail = %q, want %q", detail, c.wantDetail)
			}
			if severity != c.wantSeverity {
				t.Errorf("severity = %q, want %q", severity, c.wantSeverity)
			}
		})
	}
}

// The findings producer reads the 「与主需求的偏差」 section out of the
// stored plan. It must file one finding per bullet, against the master.
func TestRecordDesignFindings_FromPlanSection(t *testing.T) {
	d := newParentCtxDB(t)
	parentID, childID := seedParentChild(t, d, "# 主方案\n\n主需求方案正文。")
	reqSvc := service.NewRequirementService(d, nil)
	findingSvc := service.NewRequirementFindingService(d)
	jobs := store.NewJobStore(8)
	h := &WizardHandler{reqSvc: reqSvc, findingSvc: findingSvc, jobs: jobs}

	const plan = "# 技术方案\n\n## 2. 主需求关联上下文\n\n### 与主需求的偏差\n\n" +
		"- [冲突] 帧构造归属 —— protocol.c:871\n" +
		"- [新事实] 页地址池收窄到 1024\n\n## 9. 实施步骤\n\n正文。\n"

	n := h.recordDesignFindings(childID, &model.Requirement{ID: parentID, DesignDocs: "# 主方案"}, plan)
	if n != 2 {
		t.Fatalf("recorded = %d, want 2", n)
	}

	items, err := findingSvc.ListByParent(parentID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("findings = %d, want 2", len(items))
	}
	for _, f := range items {
		if f.RequirementID != childID {
			t.Errorf("finding %q attributed to %q, want %q", f.ID, f.RequirementID, childID)
		}
		if f.ParentID != parentID {
			t.Errorf("finding %q parent = %q, want %q", f.ID, f.ParentID, parentID)
		}
		if f.Status != service.FindingStatusPending {
			t.Errorf("finding %q status = %q, want pending", f.ID, f.Status)
		}
		if f.Stage != service.FindingStageDesign {
			t.Errorf("finding %q stage = %q, want design", f.ID, f.Stage)
		}
	}
}

// A re-run of the architect must not fill the master's queue with duplicates.
func TestRecordDesignFindings_UpsertOnRerun(t *testing.T) {
	d := newParentCtxDB(t)
	parentID, childID := seedParentChild(t, d, "# 主方案")
	reqSvc := service.NewRequirementService(d, nil)
	findingSvc := service.NewRequirementFindingService(d)
	jobs := store.NewJobStore(8)
	h := &WizardHandler{reqSvc: reqSvc, findingSvc: findingSvc, jobs: jobs}

	const plan = "# 技术方案\n\n### 与主需求的偏差\n\n- [冲突] 帧构造归属 —— protocol.c:871\n"
	parent := &model.Requirement{ID: parentID, DesignDocs: "# 主方案"}
	h.recordDesignFindings(childID, parent, plan)
	h.recordDesignFindings(childID, parent, plan)

	items, err := findingSvc.ListByParent(parentID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("findings = %d, want 1 (re-run must upsert)", len(items))
	}
}

// The master's review queue: accept → the finding becomes eligible for
// absorption into the master plan.
func TestFindingService_ReviewLifecycle(t *testing.T) {
	d := newParentCtxDB(t)
	parentID, childID := seedParentChild(t, d, "# 主方案")
	findingSvc := service.NewRequirementFindingService(d)

	err := findingSvc.Record(model.RequirementFinding{
		RequirementID: childID,
		ParentID:      parentID,
		Stage:         service.FindingStageDesign,
		Severity:      service.FindingSeverityConflict,
		Title:         "帧构造归属",
		Detail:        "protocol.c:871",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	items, _ := findingSvc.ListByParent(parentID)
	if len(items) != 1 {
		t.Fatalf("findings = %d, want 1", len(items))
	}
	fid := items[0].ID

	// A pending finding is not absorbable yet.
	unabsorbed, _ := findingSvc.ListUnabsorbed(parentID)
	if len(unabsorbed) != 0 {
		t.Fatalf("unabsorbed = %d, want 0 before acceptance", len(unabsorbed))
	}

	if err := findingSvc.SetStatus(fid, service.FindingStatusAccepted, "采纳", parentID); err != nil {
		t.Fatalf("set status: %v", err)
	}
	unabsorbed, _ = findingSvc.ListUnabsorbed(parentID)
	if len(unabsorbed) != 1 {
		t.Fatalf("unabsorbed = %d, want 1 after acceptance", len(unabsorbed))
	}

	// Absorbing marks it done; the sequence counter is monotonic.
	seq, err := findingSvc.NextAbsorbSeq(parentID)
	if err != nil {
		t.Fatalf("next seq: %v", err)
	}
	if _, err := findingSvc.MarkAbsorbed(parentID, seq, 3); err != nil {
		t.Fatalf("mark absorbed: %v", err)
	}
	unabsorbed, _ = findingSvc.ListUnabsorbed(parentID)
	if len(unabsorbed) != 0 {
		t.Fatalf("unabsorbed = %d, want 0 after absorption", len(unabsorbed))
	}
	seq2, _ := findingSvc.NextAbsorbSeq(parentID)
	if seq2 <= seq {
		t.Fatalf("absorb seq did not advance: %d → %d", seq, seq2)
	}
}

// The refine prompt must carry the accepted findings as a mandatory
// checklist — this is the "确认后收纳" step.
func TestFindingsAbsorbBlock(t *testing.T) {
	d := newParentCtxDB(t)
	parentID, childID := seedParentChild(t, d, "# 主方案")
	reqSvc := service.NewRequirementService(d, nil)
	findingSvc := service.NewRequirementFindingService(d)
	h := &WizardHandler{reqSvc: reqSvc, findingSvc: findingSvc}

	parent, err := reqSvc.Get(parentID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}

	// Nothing outstanding → no block (the common case stays byte-identical).
	if got := h.findingsAbsorbBlock(parent); got != "" {
		t.Fatalf("block = %q, want empty", got)
	}

	_ = findingSvc.Record(model.RequirementFinding{
		RequirementID: childID,
		ParentID:      parentID,
		Stage:         service.FindingStageCoding,
		Severity:      service.FindingSeverityDeviation,
		Title:         "帧构造归属",
		Detail:        "protocol.c:871",
	})
	items, _ := findingSvc.ListByParent(parentID)
	_ = findingSvc.SetStatus(items[0].ID, service.FindingStatusAccepted, "", parentID)

	got := h.findingsAbsorbBlock(parent)
	if got == "" {
		t.Fatal("block is empty after an accepted finding")
	}
	for _, want := range []string{"必须吸收的变更", "帧构造归属", "req_"} {
		if !strings.Contains(got, want) {
			t.Errorf("block is missing %q:\n%s", want, got)
		}
	}

	// A child requirement has no queue of its own.
	child, _ := reqSvc.Get(childID)
	if got := h.findingsAbsorbBlock(child); got != "" {
		t.Fatalf("a child must not get an absorb block, got %q", got)
	}
}
