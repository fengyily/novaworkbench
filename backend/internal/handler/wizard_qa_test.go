package handler

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestResolveQASession pins the whole decision tree. This function decides
// WHICH session a Q&A turn runs in — and therefore whether the answer is
// grounded in the real design conversation (fork) or in a pasted copy of it
// (fresh). Getting a branch wrong silently degrades every answer, so each
// branch is nailed down here.
func TestResolveQASession(t *testing.T) {
	const (
		stageA = "11111111-1111-4111-8111-111111111111"
		stageB = "22222222-2222-4222-8222-222222222222"
		qaA    = "33333333-3333-4333-8333-333333333333"
	)

	t.Run("first question forks off the stage session", func(t *testing.T) {
		p := resolveQASession(stageA, nil)
		if p.Mode != qaModeFork {
			t.Fatalf("mode = %q, want fork", p.Mode)
		}
		if p.SourceSID != stageA {
			t.Fatalf("source = %q, want %q", p.SourceSID, stageA)
		}
		if p.NewSessionID == "" {
			t.Fatal("a fork must mint a new session id")
		}
		if p.NewSessionID == stageA {
			t.Fatal("the forked session must NOT reuse the stage session id")
		}
		if p.Resumed {
			t.Fatal("a fork has no context yet — the prompt must be the full form")
		}
	})

	t.Run("follow-up resumes the thread's QA session", func(t *testing.T) {
		p := resolveQASession(stageA, &model.RequirementQATurn{
			SessionID:       qaA,
			SourceSessionID: stageA,
		})
		if p.Mode != qaModeResume {
			t.Fatalf("mode = %q, want resume", p.Mode)
		}
		if p.ResumeSID != qaA {
			t.Fatalf("resume sid = %q, want %q", p.ResumeSID, qaA)
		}
		if !p.Resumed {
			t.Fatal("a resumed turn must use the short prompt")
		}
		// The fork source is carried forward — the thread still belongs to
		// the stage session it was forked from.
		if p.SourceSID != stageA {
			t.Fatalf("source = %q, want the carried-forward %q", p.SourceSID, stageA)
		}
	})

	t.Run("stage re-run re-forks from the new session", func(t *testing.T) {
		// The user re-ran the design stage → stage_session_id changed. The
		// old thread context is stale, so the next question forks again.
		p := resolveQASession(stageB, &model.RequirementQATurn{
			SessionID:       qaA,
			SourceSessionID: stageA,
		})
		if p.Mode != qaModeFork {
			t.Fatalf("mode = %q, want fork", p.Mode)
		}
		if p.SourceSID != stageB {
			t.Fatalf("source = %q, want the new stage session %q", p.SourceSID, stageB)
		}
		if p.Resumed {
			t.Fatal("a re-fork must use the full prompt")
		}
	})

	t.Run("compressed stage session keeps the thread alive", func(t *testing.T) {
		// The user compressed the stage session away (design_session_id is
		// now empty) but the QA session itself is fine. Continuing it keeps
		// the thread instead of needlessly opening a new segment.
		p := resolveQASession("", &model.RequirementQATurn{
			SessionID:       qaA,
			SourceSessionID: stageA,
		})
		if p.Mode != qaModeResume {
			t.Fatalf("mode = %q, want resume", p.Mode)
		}
		if p.ResumeSID != qaA {
			t.Fatalf("resume sid = %q, want %q", p.ResumeSID, qaA)
		}
	})

	t.Run("degraded turn keeps its own session", func(t *testing.T) {
		// The previous turn degraded to a fresh session (source_session_id
		// was cleared). Its session holds the pasted context + the Q&A so
		// far, so the thread continues there rather than re-forking and
		// dropping the Q&A history.
		p := resolveQASession(stageA, &model.RequirementQATurn{
			SessionID:       qaA,
			SourceSessionID: "",
			Resumed:         false,
		})
		if p.Mode != qaModeResume {
			t.Fatalf("mode = %q, want resume", p.Mode)
		}
		if p.SourceSID != "" {
			t.Fatalf("source = %q, want empty (the previous turn was degraded)", p.SourceSID)
		}
	})

	t.Run("no stage session and no thread starts fresh", func(t *testing.T) {
		p := resolveQASession("", nil)
		if p.Mode != qaModeFresh {
			t.Fatalf("mode = %q, want fresh", p.Mode)
		}
		if p.NewSessionID == "" {
			t.Fatal("a fresh run must mint a session id")
		}
		if p.Resumed {
			t.Fatal("a fresh run has no context — the prompt must be the full form")
		}
	})

	t.Run("turn without a session id falls back to fork", func(t *testing.T) {
		// A turn whose session id never landed (the CLI refused the
		// pre-minted id) cannot be resumed.
		p := resolveQASession(stageA, &model.RequirementQATurn{
			SessionID:       "",
			SourceSessionID: stageA,
		})
		if p.Mode != qaModeFork {
			t.Fatalf("mode = %q, want fork", p.Mode)
		}
	})

	t.Run("blank stage session id is treated as absent", func(t *testing.T) {
		p := resolveQASession("   ", nil)
		if p.Mode != qaModeFresh {
			t.Fatalf("mode = %q, want fresh", p.Mode)
		}
	})
}

// TestQaStageHasArtifact guards the "该阶段还没有产物" gate: asking about a
// stage that never ran would send the model off to improvise.
func TestQaStageHasArtifact(t *testing.T) {
	empty := &model.Requirement{}
	if qaStageHasArtifact(empty, model.QAStageDesign, "") {
		t.Fatal("an empty requirement has no design artifact")
	}
	if qaStageHasArtifact(empty, model.QAStageCoding, "") {
		t.Fatal("an empty requirement has no coding artifact")
	}
	// A live stage session is an artifact on its own.
	if !qaStageHasArtifact(empty, model.QAStageDesign, "sess-1") {
		t.Fatal("a design session id is an artifact")
	}
	withDocs := &model.Requirement{DesignDocs: "# 方案"}
	if !qaStageHasArtifact(withDocs, model.QAStageDesign, "") {
		t.Fatal("a design document is an artifact")
	}
	if qaStageHasArtifact(withDocs, model.QAStageCoding, "") {
		t.Fatal("a design document alone is not a coding artifact")
	}
	withSummary := &model.Requirement{CodingContextSummary: "已改 …"}
	if !qaStageHasArtifact(withSummary, model.QAStageCoding, "") {
		t.Fatal("a coding context summary is an artifact")
	}
}
