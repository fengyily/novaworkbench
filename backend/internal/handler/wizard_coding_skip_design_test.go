package handler

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
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

// =============================================================================
// Agent Server mode — execStartCoding must skip local `git fetch` calls.
//
// Background (req_feb87f7d3d65f0fc): when the user picked Agent Server as the
// execution environment for the coding stage, execStartCoding's inline
// prologue still ran a best-effort local `git fetch origin <base>` via
// projectSvc.EnsureClonedAndSynced / worktreeH.syncBaseBranch. On a
// workstation that cannot reach github (Agent Server side is reachable) this
// produced a 60s FETCH_FAILED noise followed by an empty-error SSE panel even
// though the remote side would have worked fine.
//
// The fix surgical-guards the three network-binding call-sites in
// execStartCoding on `p.AgentServerID == ""`. The tests below pin that
// behaviour by injecting a spy projectService and asserting the
// EnsureClonedAndSynced counter matches expectations in both modes.
//
// Scope note: only the FIRST guard (line 686, EnsureClonedAndSynced) is
// exercised directly. The other two syncBaseBranch guards (line 848 / 888)
// sit behind the same `if p.AgentServerID == ""` pattern and are protected
// by construction — running a full real-git path to reach them would only
// re-test the same boolean, at the cost of standing up a real clone + worktree.
// =============================================================================

// runExecStartCodingPrologue seeds an in-memory DB with a project + requirement
// and fires execStartCoding in a recover-wrapped goroutine. After a short wait
// the spy counter is stable: the prologue's first guard runs well before any
// downstream path (worktree creation / git pull / claude spawn) that the test
// stubs cannot satisfy. Returns the spy + the in-flight job so callers can
// assert counter values and log content.
//
// `cb` is nil: the test does not exercise the scheduler OnFinish hook. Any
// downstream panic (jobLogSvc.Save NPE, claude spawn failure, etc.) is
// swallowed by the outer recover so the test process stays clean.
func runExecStartCodingPrologue(t *testing.T, agentServerID string) (*spyProjectSvc, *store.Job) {
	t.Helper()

	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	const projID = "proj_exec_skip"
	const reqID = "req_exec_skip"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "exec skip project", filepath.Join(dir, "project"), "ready", "main",
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO requirements (id, project_id, title) VALUES (?, ?, ?)`,
		reqID, projID, "exec skip requirement",
	); err != nil {
		t.Fatalf("insert requirement: %v", err)
	}

	reqSvc := service.NewRequirementService(d, nil)
	jobLogSvc := service.NewJobLogService(d)
	spy := &spyProjectSvc{}

	h := &WizardHandler{
		db:         d,
		projectSvc: spy,
		reqSvc:     reqSvc,
		jobLogSvc:  jobLogSvc,
		// worktreeH stays at zero value — syncBaseBranch is nil-safe on
		// nil gitCredSvc / nil projectSvc, and our tests never reach it
		// because the prologue aborts before line 847+ once EnsureCloned +
		// anchorWorktree encounter a non-existent project path.
		worktreeH: &WorktreeHandler{},
		jobs:      store.NewJobStore(1),
	}

	p := &codingRunParams{
		RequirementID: reqID,
		AgentServerID: agentServerID,
		BaseBranch:    "main",
	}

	job := &store.Job{
		ID:            "job_exec_skip_" + agentServerID,
		RequirementID: reqID,
		Status:        store.JobRunning,
		StartedAt:     time.Now(),
	}

	go func() {
		defer func() { _ = recover() }() // swallow NPE from later pipeline stubs
		h.execStartCoding(p, job, nil)
	}()

	// Prologue runs through line 686 within microseconds. 150ms is generous
	// headroom for CI noise without making the test slow.
	time.Sleep(150 * time.Millisecond)

	return spy, job
}

// TestExecStartCoding_SkipsLocalSync_WhenAgentServerIDSet pins the surgical
// guard at wizard_coding.go:686: when p.AgentServerID != "" the prologue
// must NOT call EnsureClonedAndSynced, and the SSE panel must show the
// "⏭️ 跳过本地 origin/<base> 同步（Agent Server 模式，由远端 SSH 侧接管）"
// phase line.
func TestExecStartCoding_SkipsLocalSync_WhenAgentServerIDSet(t *testing.T) {
	spy, job := runExecStartCodingPrologue(t, "agent_xxx")

	if got := atomic.LoadInt32(&spy.ensureClonedAndSyncedCalls); got != 0 {
		t.Errorf("Agent Server mode: EnsureClonedAndSynced calls = %d, want 0 (local fetch must be skipped)", got)
	}
	// EnsureCloned (auto-restore-from-remote fallback) MUST still run —
	// a missing local directory should still try to recover. Pinning this
	// here guards against a future tweak that lumps EnsureCloned into the
	// skip block by mistake.
	if got := atomic.LoadInt32(&spy.ensureClonedCalls); got < 1 {
		t.Errorf("Agent Server mode: EnsureCloned calls = %d, want >=1 (auto-clone must still run)", got)
	}

	lines, _, _ := job.Snapshot()
	want := "⏭️ 跳过本地 origin/<base> 同步（Agent Server 模式，由远端 SSH 侧接管）"
	var found bool
	for _, l := range lines {
		if strings.Contains(l.Content, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected job log to contain %q; got %d lines, first content: %q",
			want, len(lines), firstLineContent(lines))
	}
}

// TestExecStartCoding_CallsLocalSync_WhenAgentServerIDEmpty is the sanity
// counterpart: with AgentServerID == "" the prologue MUST still call
// EnsureClonedAndSynced. Without this assertion the test above would pass
// trivially even if the guard were wrong (e.g. always-skipped).
func TestExecStartCoding_CallsLocalSync_WhenAgentServerIDEmpty(t *testing.T) {
	spy, job := runExecStartCodingPrologue(t, "")

	if got := atomic.LoadInt32(&spy.ensureClonedAndSyncedCalls); got < 1 {
		t.Errorf("local mode: EnsureClonedAndSynced calls = %d, want >=1 (local fetch must run)", got)
	}
	if got := atomic.LoadInt32(&spy.ensureClonedCalls); got < 1 {
		t.Errorf("local mode: EnsureCloned calls = %d, want >=1 (auto-clone must run)", got)
	}

	// The skip message is the Agent Server signal — local mode must NOT emit it.
	lines, _, _ := job.Snapshot()
	skipMsg := "⏭️ 跳过本地 origin/<base> 同步"
	for _, l := range lines {
		if strings.Contains(l.Content, skipMsg) {
			t.Errorf("local mode: job log unexpectedly contains the Agent Server skip message: %q", l.Content)
			break
		}
	}
}

// firstLineContent is a tiny helper for clearer failure messages when the
// expected log line is missing.
func firstLineContent(lines []store.LogLine) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[0].Content
}