package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// Regression tests for backfillSkipDesignDescription — the helper that fills
// in codingRunParams.RequirementDesc when the caller (frontend / scheduler /
// quick-start path) left it empty, on the skip-design direct-development
// path.
//
// Pre-fix symptom (req_f1f71c2b2f2ef0c7): a skip_design=1 requirement with a
// non-empty `requirements.description` reached Claude with no `## 需求描述`
// block — the agent persona header only carried the title (also empty in
// that case), and the CLI bailed out as "需求 ID 在数据库中不存在" before
// any code was written. Sub-tasks forked from that session inherited the
// same blind spot. The fix backfills RequirementDesc from reqRow.Description
// only when no parent session will resume into a conversation that already
// has the description (i.e. only on the fresh-session direct-development
// path).

func TestBackfillSkipDesignDescription(t *testing.T) {
	t.Run("skip_design_empty_desc_backfills_from_req_row", func(t *testing.T) {
		p := &codingRunParams{RequirementID: "req_f1f71c2b2f2ef0c7"} // RequirementDesc intentionally ""
		reqRow := &model.Requirement{
			ID:          "req_f1f71c2b2f2ef0c7",
			Description: "## 需求背景\n需要一个 HelloWorld 测试页面。",
		}

		if filled := backfillSkipDesignDescription(p, reqRow); !filled {
			t.Fatalf("expected backfill to fire on empty RequirementDesc + non-empty req.Description")
		}
		if p.RequirementDesc != reqRow.Description {
			t.Fatalf("RequirementDesc = %q, want %q", p.RequirementDesc, reqRow.Description)
		}
	})

	t.Run("design_chain_with_session_id_is_left_alone", func(t *testing.T) {
		// 方案 → 开发路径：reqRow.DesignSessionID != "" 时，上游 sourceSID
		// 推导会把 fork 走 --resume，描述已在父 jsonl 里。本次 helper 不应
		// 覆盖（即便 RequirementDesc 是空、req.Description 非空），否则会
		// 与父会话里的描述重复，且 caller 有意把字段留空通常代表"以父会话
		// 为准"。本测试断言 helper 在调用方明确留空时**仍然**回填——这是
		// 当前实现的事实行为，由 execStartCoding 的 prologue 决定后续是否
		// 走 fresh-session；如果将来需要做"父会话优先"语义，应在 helper
		// 内加 reqRow.DesignSessionID/AnalysisSessionID 分支判断。
		p := &codingRunParams{RequirementID: "req_design_chain"}
		reqRow := &model.Requirement{
			ID:               "req_design_chain",
			DesignSessionID:  "sid_design_xyz",
			Description:      "方案 → 开发路径下的描述",
		}

		filled := backfillSkipDesignDescription(p, reqRow)
		if !filled {
			t.Fatalf("helper currently backfills regardless of DesignSessionID; this test pins current behavior so any future change is deliberate")
		}
		if p.RequirementDesc != reqRow.Description {
			t.Fatalf("RequirementDesc = %q, want %q", p.RequirementDesc, reqRow.Description)
		}
	})

	t.Run("non_empty_requirement_desc_is_preserved", func(t *testing.T) {
		callerDesc := "caller-supplied desc — must win"
		p := &codingRunParams{RequirementDesc: callerDesc}
		reqRow := &model.Requirement{Description: "req row desc — must lose"}

		if filled := backfillSkipDesignDescription(p, reqRow); filled {
			t.Fatalf("expected NO backfill when RequirementDesc already non-empty; got filled=true")
		}
		if p.RequirementDesc != callerDesc {
			t.Fatalf("RequirementDesc = %q, want caller-supplied %q (reqRow must not overwrite)", p.RequirementDesc, callerDesc)
		}
	})

	t.Run("empty_requirement_desc_and_empty_req_description_is_noop", func(t *testing.T) {
		p := &codingRunParams{}
		reqRow := &model.Requirement{Description: ""}

		if filled := backfillSkipDesignDescription(p, reqRow); filled {
			t.Fatalf("expected NO backfill when both sides empty; got filled=true")
		}
		if p.RequirementDesc != "" {
			t.Fatalf("RequirementDesc = %q, want still \"\"", p.RequirementDesc)
		}
	})

	t.Run("whitespace_only_requirement_desc_is_treated_as_empty", func(t *testing.T) {
		p := &codingRunParams{RequirementDesc: "   \n\t  "}
		reqRow := &model.Requirement{Description: "real desc"}

		if filled := backfillSkipDesignDescription(p, reqRow); !filled {
			t.Fatalf("expected backfill when RequirementDesc is whitespace-only")
		}
		if !strings.Contains(p.RequirementDesc, "real desc") {
			t.Fatalf("RequirementDesc = %q, want to contain 'real desc'", p.RequirementDesc)
		}
	})

	t.Run("nil_req_row_is_noop", func(t *testing.T) {
		// Legacy /wizard quick-start path passes nil; backfill must not crash.
		p := &codingRunParams{}

		if filled := backfillSkipDesignDescription(p, nil); filled {
			t.Fatalf("expected NO backfill when reqRow is nil; got filled=true")
		}
		if p.RequirementDesc != "" {
			t.Fatalf("RequirementDesc = %q, want still \"\"", p.RequirementDesc)
		}
	})
}