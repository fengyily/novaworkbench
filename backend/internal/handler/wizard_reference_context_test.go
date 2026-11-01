package handler

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/store"
)

// insertReferenceRows inserts the project + owner + target rows and stores the
// reference through the service layer.
func insertReferenceRows(t *testing.T, h *WizardHandler, ownerID, targetID string) {
	t.Helper()
	if _, err := h.db.Exec(`INSERT OR IGNORE INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		"proj_ref_ctx", "RefCtx", "/tmp/refctx", "ready", "main"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := h.db.Exec(`INSERT INTO requirements (id, project_id, title, description, design_docs) VALUES (?, ?, ?, ?, ?)`,
		ownerID, "proj_ref_ctx", "本需求标题", "本需求描述正文", ""); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	if _, err := h.db.Exec(`INSERT INTO requirements (id, project_id, title, description, design_docs) VALUES (?, ?, ?, ?, ?)`,
		targetID, "proj_ref_ctx", "被引用需求标题", "被引用需求描述正文", "# 被引用方案\n\n被引用方案正文。"); err != nil {
		t.Fatalf("insert target: %v", err)
	}
	if _, err := h.reqSvc.SetReferences(ownerID, []model.RequirementReferenceInput{
		{RequirementID: targetID, Note: "复用其鉴权中间件"},
	}); err != nil {
		t.Fatalf("set references: %v", err)
	}
}

// The referenced-context block must land in the prompt on ALL THREE branches
// — the analyst-resume path, the fresh skip-analysis path, and the fresh path
// with a DesignContextSummary handoff prefix. The block is appended once
// after the branch, so this test is what pins that it really does cover all
// three.
func TestPrepareDesignWorkspace_ReferencedBlockOnEveryPromptBranch(t *testing.T) {
	d := newParentCtxDB(t)
	h, jobs, work := prepareDesignWorkspaceFixture(t, d)
	// The rows first (they create the project), then point the project at the
	// worktree — the hard sync gate reads the project's local_path.
	insertReferenceRows(t, h, "req_ref_owner", "req_ref_target")
	if _, err := h.db.Exec(`UPDATE projects SET local_path = ?, remote_url = ? WHERE id = ?`,
		work, filepath.Join(filepath.Dir(work), "origin.git"), "proj_ref_ctx"); err != nil {
		t.Fatalf("point project at workdir: %v", err)
	}

	block, refs := h.buildReferencedBlock(mustGetReq(t, h.reqSvc, "req_ref_owner"))
	if block == "" {
		t.Fatal("buildReferencedBlock returned empty for a well-formed reference")
	}
	if len(refs) != 1 {
		t.Fatalf("got %d resolved references, want 1", len(refs))
	}

	cases := []struct {
		name         string
		sourceSID    string
		skipAnalysis bool
		contextSum   string
		wantInPrompt []string
	}{
		{
			name:         "analyst resume",
			sourceSID:    "analyst-sid",
			wantInPrompt: []string{"切换到「架构师」角色", "基于我们刚才完成的需求分析对话"},
		},
		{
			name:         "fresh skip-analysis",
			skipAnalysis: true,
			wantInPrompt: []string{"## 需求描述", "## 项目上下文", "## 需求标题"},
		},
		{
			name:         "fresh skip-analysis with context summary",
			skipAnalysis: true,
			contextSum:   "之前的方案摘要",
			wantInPrompt: []string{"## 上下文压缩摘要", "之前的方案摘要"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mustGetReq(t, h.reqSvc, "req_ref_owner")
			req.DesignContextSummary = c.contextSum
			p := &designRunParams{
				Req:             req,
				ProjectPath:     work,
				BaseBranch:      "main",
				SourceSID:       c.sourceSID,
				SkipAnalysis:    c.skipAnalysis,
				ReferencedBlock: block,
				ReferencedRefs:  refs,
			}
			job := jobs.Create("req_ref_owner")
			if !h.prepareDesignWorkspace(p, job) {
				lines, status, _ := job.Snapshot()
				t.Fatalf("prepareDesignWorkspace failed (status=%s, lines=%+v)", status, lines)
			}
			for _, want := range c.wantInPrompt {
				if !strings.Contains(p.Prompt, want) {
					t.Errorf("prompt is missing branch marker %q", want)
				}
			}
			for _, want := range []string{
				"## 关联需求参考（本需求引用了其它需求）",
				"req_ref_target",
				"复用其鉴权中间件",
				"被引用需求描述正文",
				"# 被引用方案",
				promptpkg.ReferenceContextHeading,
				"### 继承的约束与决策",
			} {
				if !strings.Contains(p.Prompt, want) {
					t.Errorf("prompt is missing reference block content %q\n---\n%s", want, p.Prompt)
				}
			}
			// The regression guard: the referenced requirement's TITLE must not
			// reach the prompt. The UI still shows it; the model must not see it.
			if strings.Contains(p.Prompt, "被引用需求标题") {
				t.Errorf("prompt leaked the referenced requirement's title\n---\n%s", p.Prompt)
			}
		})
	}
}

// The no-references guarantee: a requirement without references must get a
// byte-identical prompt. The block is inserted into the single tail assembly
// point, not spliced into the branch bodies, so an empty ReferencedBlock can
// only ever add nothing.
func TestPrepareDesignWorkspace_NoReferencesPromptIsUnchanged(t *testing.T) {
	d := newParentCtxDB(t)
	h, jobs, work := prepareDesignWorkspaceFixture(t, d)
	// The rows first (they create the project), then point the project at the
	// worktree — the hard sync gate reads the project's local_path.
	insertReferenceRows(t, h, "req_ref_owner", "req_ref_target")
	if _, err := h.db.Exec(`UPDATE projects SET local_path = ?, remote_url = ? WHERE id = ?`,
		work, filepath.Join(filepath.Dir(work), "origin.git"), "proj_ref_ctx"); err != nil {
		t.Fatalf("point project at workdir: %v", err)
	}

	build := func(t *testing.T, block string) string {
		t.Helper()
		p := &designRunParams{
			Req:         mustGetReq(t, h.reqSvc, "req_ref_owner"),
			ProjectPath: work,
			BaseBranch:  "main",
			SourceSID:   "analyst-sid",
		}
		p.ReferencedBlock = block
		job := jobs.Create("req_ref_owner")
		if !h.prepareDesignWorkspace(p, job) {
			lines, status, _ := job.Snapshot()
			t.Fatalf("prepareDesignWorkspace failed (status=%s, lines=%+v)", status, lines)
		}
		return p.Prompt
	}

	noBlock := build(t, "")
	withBlock := build(t, "REFBLOCK")

	if strings.Contains(noBlock, "REFBLOCK") || strings.Contains(noBlock, "关联需求参考") {
		t.Fatalf("prompt without ReferencedBlock leaked reference content:\n%s", noBlock)
	}

	// Exact-byte proof: withBlock must be noBlock with the block spliced in
	// immediately before the kind-specific tail.
	tail := "\n\n" + promptpkg.ArchitectBlock("requirement", nil)
	idx := strings.Index(noBlock, tail)
	if idx < 0 {
		t.Fatalf("cannot locate the kind-specific tail in the baseline prompt:\n%s", noBlock)
	}
	if want := noBlock[:idx] + "\n\n" + "REFBLOCK" + tail; withBlock != want {
		t.Errorf("prompt assembly differs from baseline+block\nwant:\n%s\ngot:\n%s", want, withBlock)
	}
}

// A requirement with no references (or a degraded lookup) must yield an empty
// block — never an error, never a half-rendered block.
func TestBuildReferencedBlock_DegradesGracefully(t *testing.T) {
	d := newParentCtxDB(t)
	h, _, _ := prepareDesignWorkspaceFixture(t, d)

	t.Run("requirement without references", func(t *testing.T) {
		if _, err := h.db.Exec(`INSERT OR IGNORE INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
			"proj_ref_ctx2", "RefCtx2", "/tmp/refctx2", "ready", "main"); err != nil {
			t.Fatalf("insert project: %v", err)
		}
		if _, err := h.db.Exec(`INSERT INTO requirements (id, project_id, title) VALUES (?, ?, ?)`,
			"req_ref_none", "proj_ref_ctx2", "没有引用"); err != nil {
			t.Fatalf("insert requirement: %v", err)
		}
		block, refs := h.buildReferencedBlock(mustGetReq(t, h.reqSvc, "req_ref_none"))
		if block != "" || len(refs) != 0 {
			t.Fatalf("expected (empty, nil), got (%q, %+v)", block, refs)
		}
	})

	t.Run("nil requirement", func(t *testing.T) {
		block, refs := h.buildReferencedBlock(nil)
		if block != "" || len(refs) != 0 {
			t.Fatalf("expected (empty, nil), got (%q, %+v)", block, refs)
		}
	})
}

// Downstream reads ONLY design_docs, so the persisted plan is the last place
// the referenced context can be guaranteed.
func TestFinalizeArchitectRun_ReferencedContextPersistence(t *testing.T) {
	const withSection = "# 技术方案\n\n## 需求理解\n\n正文。\n\n## 关联需求上下文\n\n### 来源\n\n1. req_ref_target（复用其鉴权中间件）\n"
	const withoutSection = "# 技术方案\n\n## 需求理解\n\n正文。\n\n## 整体实现思路\n\n思路。\n"

	t.Run("section present is persisted verbatim", func(t *testing.T) {
		d := newParentCtxDB(t)
		h, _, _ := prepareDesignWorkspaceFixture(t, d)
		insertReferenceRows(t, h, "req_ref_owner", "req_ref_target")
		jobs := store.NewJobStore(8)
		h.jobs = jobs
		job := jobs.Create("req_ref_owner")

		h.finalizeArchitectRun(
			claudeStreamOutcome{planContent: withSection, sessionID: "sid"},
			&designRunParams{
				Req:             mustGetReq(t, h.reqSvc, "req_ref_owner"),
				ReferencedBlock: "some-block",
				ReferencedRefs: []model.RequirementReferenceContent{{
					RequirementReference: model.RequirementReference{RequirementID: "req_ref_target", Note: "复用其鉴权中间件"},
					DesignDocs:           "# 被引用方案",
				}},
			},
			job, nil, "", "sid", "req_ref_owner", "sonnet",
		)

		got := mustGetReq(t, h.reqSvc, "req_ref_owner")
		if got.DesignDocs != withSection {
			t.Errorf("plan must be persisted verbatim when the section is present\nwant:\n%s\ngot:\n%s", withSection, got.DesignDocs)
		}
		for _, l := range mustLines(t, job) {
			if strings.Contains(l, "已自动追加附录") {
				t.Errorf("no fallback warning expected, got: %s", l)
			}
		}
	})

	t.Run("section missing gets an appendix and a warning", func(t *testing.T) {
		d := newParentCtxDB(t)
		h, _, _ := prepareDesignWorkspaceFixture(t, d)
		insertReferenceRows(t, h, "req_ref_owner", "req_ref_target")
		jobs := store.NewJobStore(8)
		h.jobs = jobs
		job := jobs.Create("req_ref_owner")

		h.finalizeArchitectRun(
			claudeStreamOutcome{planContent: withoutSection, sessionID: "sid"},
			&designRunParams{
				Req:             mustGetReq(t, h.reqSvc, "req_ref_owner"),
				ReferencedBlock: "some-block",
				ReferencedRefs: []model.RequirementReferenceContent{{
					RequirementReference: model.RequirementReference{RequirementID: "req_ref_target", Note: "复用其鉴权中间件"},
					DesignDocs:           "# 被引用方案\n\n被引用方案正文。",
				}},
			},
			job, nil, "", "sid", "req_ref_owner", "sonnet",
		)

		got := mustGetReq(t, h.reqSvc, "req_ref_owner")
		if !strings.HasPrefix(got.DesignDocs, withoutSection) {
			t.Errorf("original plan must be preserved verbatim, got:\n%s", got.DesignDocs)
		}
		for _, want := range []string{
			"## 关联需求上下文（自动附录）",
			"req_ref_target",
			"复用其鉴权中间件",
			"被引用方案正文。",
			"未经提炼",
		} {
			if !strings.Contains(got.DesignDocs, want) {
				t.Errorf("persisted plan is missing appendix content %q\n---\n%s", want, got.DesignDocs)
			}
		}
		// The appendix must not leak the title either.
		if strings.Contains(got.DesignDocs, "被引用需求标题") {
			t.Errorf("appendix leaked the referenced requirement's title\n---\n%s", got.DesignDocs)
		}
		var warned bool
		for _, l := range mustLines(t, job) {
			if strings.Contains(l, "未包含「关联需求上下文」章节") && strings.Contains(l, "已自动追加附录") {
				warned = true
			}
		}
		if !warned {
			t.Errorf("expected a job-log warning about the missing section, got: %+v", mustLines(t, job))
		}
	})

	t.Run("no references leaves the plan untouched", func(t *testing.T) {
		d := newParentCtxDB(t)
		h, _, _ := prepareDesignWorkspaceFixture(t, d)
		insertReferenceRows(t, h, "req_ref_owner", "req_ref_target")
		jobs := store.NewJobStore(8)
		h.jobs = jobs
		job := jobs.Create("req_ref_owner")

		h.finalizeArchitectRun(
			claudeStreamOutcome{planContent: withoutSection, sessionID: "sid"},
			&designRunParams{Req: mustGetReq(t, h.reqSvc, "req_ref_owner")},
			job, nil, "", "sid", "req_ref_owner", "sonnet",
		)

		got := mustGetReq(t, h.reqSvc, "req_ref_owner")
		if got.DesignDocs != withoutSection {
			t.Errorf("plan must not be modified when there are no references\nwant:\n%s\ngot:\n%s", withoutSection, got.DesignDocs)
		}
	})
}

// Refine rewrites the doc from scratch every turn, and the distilled
// referenced section reads like context the conversation never discussed —
// exactly what a model prunes. The keep rule has to fire for a doc that
// actually carries the section, and stay silent otherwise.
func TestReferencedContextKeepRule(t *testing.T) {
	if got := referencedContextKeepRule("# 方案\n\n没有关联需求上下文。\n"); got != "" {
		t.Errorf("keep rule must stay silent for a doc without the section, got: %q", got)
	}
	got := referencedContextKeepRule("# 方案\n\n## 关联需求上下文\n\n### 来源\n")
	if !strings.Contains(got, "保留") {
		t.Errorf("keep rule must fire for a doc carrying the section, got: %q", got)
	}
	// The appendix variant counts too (stickiness).
	if got := referencedContextKeepRule("## 关联需求上下文（自动附录）\n"); got == "" {
		t.Error("keep rule must fire for the machine-written appendix too")
	}
}
