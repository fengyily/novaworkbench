package handler

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
)

// reviewClaudeDocRig is a ReviewHandler over a project that may or may not
// have a project-level CLAUDE.md.
type reviewClaudeDocRig struct {
	h       *ReviewHandler
	project *model.Project
	rv      *model.CodeReview
}

func newReviewClaudeDocRig(t *testing.T, withDoc bool) *reviewClaudeDocRig {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	projSvc := service.NewProjectService(d, nil, nil)
	const projID = "proj_review_claude_doc"
	localPath := t.TempDir()
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?)`,
		projID, "review doc project", localPath); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	project, err := projSvc.Get(projID)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if withDoc {
		if _, err := projSvc.UpsertClaudeDoc(projID,
			"# 项目约定\n\n分层：handler / service / model。",
			model.ProjectClaudeDocSourceGenerated, true); err != nil {
			t.Fatalf("seed claude doc: %v", err)
		}
	}
	return &reviewClaudeDocRig{
		h:       &ReviewHandler{projectSvc: projSvc},
		project: project,
		rv: &model.CodeReview{
			ID:         "crv_review_claude_doc",
			ProjectID:  projID,
			ReviewType: model.ReviewTypeBranch,
			Branch:     "feat/x",
			BaseBranch: "main",
		},
	}
}

// TestReviewTaskPromptCarriesProjectClaudeDoc is the regression guard for
// the project-level Review.
//
// The injected block promises that 代码审查 follows the project's
// conventions, and the checklist even asks whether the change matches
// 「与项目既有约定是否一致」. A reviewer that never sees the conventions
// grades the diff against rules it has no access to — while the
// requirement-level Review (which runs through the sub-task runner) did see
// them. The two review paths must not disagree.
func TestReviewTaskPromptCarriesProjectClaudeDoc(t *testing.T) {
	t.Run("with document", func(t *testing.T) {
		rig := newReviewClaudeDocRig(t, true)
		// The prompt-mode injection: with no settings service wired the
		// resolution falls back to the shipped default (native), so the test
		// asks for the prompt-mode block explicitly.
		inj := rig.h.projectClaudeDoc(rig.rv.ProjectID, rig.project.LocalPath)
		inj.Native = false
		got := rig.h.buildReviewTaskPrompt(rig.rv, rig.project, "origin/feat/x", "origin/main", false, inj)
		if !strings.Contains(got, prompt.ProjectClaudeDocHeading) {
			t.Errorf("review prompt missing the project CLAUDE.md block:\n%s", got)
		}
		if !strings.Contains(got, "分层：handler / service / model。") {
			t.Errorf("review prompt missing the document body:\n%s", got)
		}
		// The conventions are CONTEXT for the review, not another contract:
		// they have to sit before the output contract, otherwise the model
		// reads them as a fourth thing it has to satisfy.
		if strings.Index(got, prompt.ProjectClaudeDocHeading) > strings.Index(got, "## 输出契约") {
			t.Errorf("the conventions come after the output contract:\n%s", got)
		}
	})

	t.Run("without document", func(t *testing.T) {
		rig := newReviewClaudeDocRig(t, false)
		// The prompt-mode injection: with no settings service wired the
		// resolution falls back to the shipped default (native), so the test
		// asks for the prompt-mode block explicitly.
		inj := rig.h.projectClaudeDoc(rig.rv.ProjectID, rig.project.LocalPath)
		inj.Native = false
		got := rig.h.buildReviewTaskPrompt(rig.rv, rig.project, "origin/feat/x", "origin/main", false, inj)
		if strings.Contains(got, prompt.ProjectClaudeDocHeading) {
			t.Errorf("review prompt carries a CLAUDE.md block although the project has none:\n%s", got)
		}
		// Byte-for-byte: the prompt of a project without a document must be
		// exactly the one from before the feature existed.
		want := prompt.BuildReviewPrompt(prompt.ReviewPromptInput{
			ReviewType:     rig.rv.ReviewType,
			Branch:         rig.rv.Branch,
			BaseBranch:     rig.rv.BaseBranch,
			HeadRef:        "origin/feat/x",
			BaseRef:        "origin/main",
			HasAttachments: false,
		})
		if got != want {
			t.Errorf("review prompt changed for a project without a document:\ngot  %q\nwant %q", got, want)
		}
	})
}

// TestReviewTaskPromptWithoutProjectService is the nil-safety guard: a
// ReviewHandler without a wired project service must still build a prompt
// instead of panicking.
func TestReviewTaskPromptWithoutProjectService(t *testing.T) {
	h := &ReviewHandler{}
	rv := &model.CodeReview{ID: "crv_x", ProjectID: "proj_x", ReviewType: model.ReviewTypePR}
	got := h.buildReviewTaskPrompt(rv, nil, "origin/feat/x", "origin/main", false, claudeDocInjection{})
	if got == "" {
		t.Fatal("buildReviewTaskPrompt returned an empty prompt")
	}
	if strings.Contains(got, prompt.ProjectClaudeDocHeading) {
		t.Errorf("prompt carries a CLAUDE.md block although no service is wired:\n%s", got)
	}
}

// TestRegeneratePromptCarriesProjectClaudeDoc pins the same rule for the
// 「基于追问对话重新生成」 flow: the regenerated report REPLACES the old one
// in front of the user, so it must be graded against the same conventions.
func TestRegeneratePromptCarriesProjectClaudeDoc(t *testing.T) {
	rig := newReviewClaudeDocRig(t, true)
	inj := rig.h.projectClaudeDoc(rig.rv.ProjectID, rig.project.LocalPath)
	inj.Native = false
	got := rig.h.regeneratePrompt(rig.rv, rig.project, nil, false, false, inj)
	if !strings.Contains(got, prompt.ProjectClaudeDocHeading) {
		t.Errorf("regenerate prompt missing the project CLAUDE.md block:\n%s", got)
	}

	rig2 := newReviewClaudeDocRig(t, false)
	inj2 := rig2.h.projectClaudeDoc(rig2.rv.ProjectID, rig2.project.LocalPath)
	inj2.Native = false
	got2 := rig2.h.regeneratePrompt(rig2.rv, rig2.project, nil, false, false, inj2)
	if strings.Contains(got2, prompt.ProjectClaudeDocHeading) {
		t.Errorf("regenerate prompt carries a CLAUDE.md block although the project has none:\n%s", got2)
	}
}
