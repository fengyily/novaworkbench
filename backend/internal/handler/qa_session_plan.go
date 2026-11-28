package handler

import (
	"strings"

	"github.com/novaworkbench/backend/internal/util"
)

// ── 「这一轮该跑在哪个会话里」的共享决策树 ──────────────────────────────────
//
// 两个功能问的是**同一个问题**：
//
//	单项目的阶段答疑（wizard_qa.go）—— 源会话是 requirements.design_session_id
//	  或 coding_session_id；
//	跨项目需求的方案调整（cross_requirement_adjust.go）—— 源会话是
//	  cross_requirements.design_session_id。
//
// 两者的 turn 模型不同（RequirementQATurn vs CrossRequirementQATurn），但
// 决策逻辑**必须一致**：两棵树 = 两套 bug，而且单项目那棵树已经被
// wizard_qa_test.go 的九个子测试钉死了。所以决策树写在这里一次，两个调用方
// 各自把自己的行翻译成 qaSessionSource。
//
// 纯函数：没有 I/O，所以整棵树可以被单测直接覆盖。

// qaSessionSource is the input of the session-plan decision tree.
//
// It is a flat struct (not a model type) on purpose: the two callers have
// different turn models but answer the same question.
type qaSessionSource struct {
	// StageSID is the production session to fork from — the requirement's
	// design_session_id / coding_session_id, or the cross requirement's
	// design_session_id. Empty = there is nothing to fork.
	StageSID string
	// ThreadSID is the thread's own session ("" = the thread has none yet).
	ThreadSID string
	// ThreadSourceSID is the snapshot of the stage session the thread forked
	// from. Empty = the last turn was degraded (fresh).
	ThreadSourceSID string
}

// resolveQASessionPlan decides how a new turn gets its context.
//
// 三分支：
//
//  1. resume —— 线程会话健在，且它 fork 的源仍是当前阶段会话 → 续接。
//  2. fork   —— 阶段会话变了（阶段被重跑 / 方案被重新生成）→ 从新会话重新
//     fork。旧轮次保留展示。
//  3. fresh  —— 既没有阶段会话也没有线程会话 → 全新会话，上下文贴进提示词。
//
// The "StageSID == ''" condition in branch 1 covers "the user compressed the
// stage session away"（单项目）或"方案没有会话 id"（跨项目）：the thread
// session itself is still alive, so the thread continues instead of
// needlessly re-opening a new segment.
//
// The "ThreadSourceSID == ''" condition covers a DEGRADED previous turn (the
// stage session was gone, so the turn ran fresh with the context pasted into
// the prompt): its session holds that context plus the earlier Q&A, so
// continuing it preserves the thread. Without this condition the very next
// question would re-fork from the stage session and silently drop the Q&A
// history the user just built up.
func resolveQASessionPlan(src qaSessionSource) qaSessionPlan {
	stageSID := strings.TrimSpace(src.StageSID)
	threadSID := strings.TrimSpace(src.ThreadSID)
	sourceSnapshot := strings.TrimSpace(src.ThreadSourceSID)

	if threadSID != "" && (sourceSnapshot == "" || stageSID == sourceSnapshot || stageSID == "") {
		return qaSessionPlan{
			Mode:      qaModeResume,
			ResumeSID: threadSID,
			// Carry the fork source forward. A resumed turn did NOT fork
			// again, so the thread's source session is still the one the
			// first turn forked from — dropping it would make the next
			// "did the stage get re-run?" comparison look like a mismatch
			// and trigger a needless re-fork.
			SourceSID:    sourceSnapshot,
			NewSessionID: threadSID,
			Resumed:      true,
		}
	}
	if stageSID != "" {
		return qaSessionPlan{
			Mode:         qaModeFork,
			SourceSID:    stageSID,
			NewSessionID: util.NewUUID(),
		}
	}
	return qaSessionPlan{
		Mode:         qaModeFresh,
		NewSessionID: util.NewUUID(),
	}
}
