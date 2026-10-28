package handler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// seedParentChild inserts a master requirement + a child requirement hung off
// it, both in project proj_parent_ctx, and returns (parentID, childID). Kept
// as raw SQL (like seedRequirementForSchedule) so the test owns the exact row
// shape — in particular parent_id, which Create/SetParent would otherwise
// normalize away.
func seedParentChild(t *testing.T, d *db.DB, parentDesign string) (string, string) {
	t.Helper()
	const projID = "proj_parent_ctx"
	if _, err := d.Exec(`INSERT OR IGNORE INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "ParentCtx", "/tmp/parentctx", "ready", "main"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const parentID = "req_ctx_parent"
	const childID = "req_ctx_child"
	if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title, description, design_docs) VALUES (?, ?, ?, ?, ?)`,
		parentID, projID, "主需求标题", "主需求描述正文", parentDesign); err != nil {
		t.Fatalf("insert parent: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title, description, parent_id) VALUES (?, ?, ?, ?, ?)`,
		childID, projID, "子需求标题", "子需求描述正文", parentID); err != nil {
		t.Fatalf("insert child: %v", err)
	}
	return parentID, childID
}

// newParentCtxDB builds a throwaway sqlite DB. Every test in this file gets its
// own — the requirement service writes rows and we assert on them, so sharing
// would leak state between cases.
func newParentCtxDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// prepareDesignWorkspace needs a real git upstream + clone to satisfy the hard
// sync gate before it will build a prompt, so the prompt-content tests pay for
// a small repo fixture. Returns (handler, jobs, projectPath).
func prepareDesignWorkspaceFixture(t *testing.T, d *db.DB) (*WizardHandler, *store.JobStore, string) {
	t.Helper()
	const baseBranch = "main"
	upstreamDir := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch="+baseBranch, upstreamDir)
	seed := filepath.Join(t.TempDir(), "seed")
	mustGit(t, "", "git", "clone", upstreamDir, seed)
	mustGit(t, seed, "git", "config", "user.email", "test@nova")
	mustGit(t, seed, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("init"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, seed, "git", "add", "README.md")
	mustGit(t, seed, "git", "commit", "-m", "init")
	mustGit(t, seed, "git", "push", "origin", "HEAD:"+baseBranch)

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "", "git", "clone", upstreamDir, work)
	mustGit(t, work, "git", "config", "user.email", "test@nova")
	mustGit(t, work, "git", "config", "user.name", "Test")

	h := &WizardHandler{
		db:         d,
		projectSvc: service.NewProjectService(d, nil, nil),
		reqSvc:     service.NewRequirementService(d, nil),
		settingSvc: service.NewSettingService(d),
		jobs:       store.NewJobStore(8),
	}
	return h, h.jobs, work
}

// The parent block must land in the prompt on ALL THREE branches — the
// analyst-resume path, the fresh skip-analysis path, and the fresh path with a
// DesignContextSummary handoff prefix. Copying the append into each branch
// would guarantee drift, so prepareDesignWorkspace appends it once after the
// branch; this test is what pins that it really does cover all three.
func TestPrepareDesignWorkspace_ParentBlockOnEveryPromptBranch(t *testing.T) {
	d := newParentCtxDB(t)
	_, childID := seedParentChild(t, d, "# 主方案\n\n主需求第一步。")

	h, jobs, work := prepareDesignWorkspaceFixture(t, d)
	if _, err := h.db.Exec(`UPDATE projects SET local_path = ?, remote_url = ? WHERE id = ?`,
		work, filepath.Join(filepath.Dir(work), "origin.git"), "proj_parent_ctx"); err != nil {
		t.Fatalf("point project at workdir: %v", err)
	}
	h.projectSvc = service.NewProjectService(d, nil, nil)

	reqSvc := h.reqSvc
	parent, block, _ := h.buildParentBlock(context.Background(), mustGetReq(t, reqSvc, childID), "")
	if block == "" {
		t.Fatal("buildParentBlock returned empty for a well-formed parent link")
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
			req := mustGetReq(t, reqSvc, childID)
			req.DesignContextSummary = c.contextSum
			p := &designRunParams{
				Req:          req,
				ProjectPath:  work,
				BaseBranch:   "main",
				SourceSID:    c.sourceSID,
				SkipAnalysis: c.skipAnalysis,
				Parent:       parent,
				ParentBlock:  block,
			}
			job := jobs.Create(childID)
			if !h.prepareDesignWorkspace(p, job) {
				lines, status, _ := job.Snapshot()
				t.Fatalf("prepareDesignWorkspace failed (status=%s, lines=%+v)", status, lines)
			}
			for _, want := range c.wantInPrompt {
				if !strings.Contains(p.Prompt, want) {
					t.Errorf("prompt is missing branch marker %q", want)
				}
			}
			// Parent block: input half + output contract.
			for _, want := range []string{
				"## 主需求参考（本需求是其子需求）",
				"主需求标题",
				"req_ctx_parent",
				"主需求描述正文",
				"# 主方案",
				promptpkg.ParentContextHeading,
				"### 继承的约束与决策",
			} {
				if !strings.Contains(p.Prompt, want) {
					t.Errorf("prompt is missing parent block content %q\n---\n%s", want, p.Prompt)
				}
			}
		})
	}
}

// The no-parent guarantee: a top-level requirement's prompt must be exactly
// what it was before this feature — the block is inserted into the single tail
// assembly point, not spliced into the branch bodies, so an empty ParentBlock
// can only ever add nothing.
func TestPrepareDesignWorkspace_NoParentPromptIsUnchanged(t *testing.T) {
	d := newParentCtxDB(t)
	_, childID := seedParentChild(t, d, "# 主方案")
	h, jobs, work := prepareDesignWorkspaceFixture(t, d)
	if _, err := h.db.Exec(`UPDATE projects SET local_path = ?, remote_url = ? WHERE id = ?`,
		work, filepath.Join(filepath.Dir(work), "origin.git"), "proj_parent_ctx"); err != nil {
		t.Fatalf("point project at workdir: %v", err)
	}

	build := func(t *testing.T, block string) string {
		t.Helper()
		p := &designRunParams{
			Req:         mustGetReq(t, h.reqSvc, childID),
			ProjectPath: work,
			BaseBranch:  "main",
			SourceSID:   "analyst-sid",
			ParentBlock: block,
		}
		job := jobs.Create(childID)
		if !h.prepareDesignWorkspace(p, job) {
			lines, status, _ := job.Snapshot()
			t.Fatalf("prepareDesignWorkspace failed (status=%s, lines=%+v)", status, lines)
		}
		return p.Prompt
	}

	// Deliberately pass a NON-empty ParentBlock on the child row to prove the
	// field — not the requirement's parent_id — is the sole switch. A parent
	// context must never leak into a run that wasn't given one.
	noBlock := build(t, "")
	withBlock := build(t, "PARENTBLOCK")

	if strings.Contains(noBlock, "PARENTBLOCK") || strings.Contains(noBlock, "主需求参考") {
		t.Fatalf("prompt without ParentBlock leaked parent content:\n%s", noBlock)
	}

	// Exact-byte proof: withBlock must be noBlock with the block spliced in
	// immediately before the kind-specific tail.
	tail := "\n\n" + promptpkg.ArchitectBlock("requirement", nil)
	idx := strings.Index(noBlock, tail)
	if idx < 0 {
		t.Fatalf("cannot locate the kind-specific tail in the baseline prompt:\n%s", noBlock)
	}
	if want := noBlock[:idx] + "\n\n" + "PARENTBLOCK" + tail; withBlock != want {
		t.Errorf("prompt assembly differs from baseline+block\nwant:\n%s\ngot:\n%s", want, withBlock)
	}
}

// A missing parent, a cross-project parent, and a grandparent must each
// produce (nil, "") — never an error, and never a half-rendered block.
func TestBuildParentBlock_RejectsBrokenLinks(t *testing.T) {
	d := newParentCtxDB(t)
	_, childID := seedParentChild(t, d, "# 主方案")
	reqSvc := service.NewRequirementService(d, nil)
	h := &WizardHandler{reqSvc: reqSvc}

	t.Run("parent row does not exist", func(t *testing.T) {
		child := mustGetReq(t, reqSvc, childID)
		child.ParentID = "req_does_not_exist"
		parent, block, _ := h.buildParentBlock(context.Background(), child, "")
		if parent != nil || block != "" {
			t.Fatalf("deleted parent must degrade to (nil, \"\"), got (%v, %q)", parent != nil, block)
		}
	})

	t.Run("parent in a different project", func(t *testing.T) {
		if _, err := d.Exec(`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
			"proj_other", "Other", "/tmp/other", "ready", "main"); err != nil {
			t.Fatalf("insert other project: %v", err)
		}
		if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title) VALUES (?, ?, ?)`,
			"req_cross_proj", "proj_other", "跨项目主需求"); err != nil {
			t.Fatalf("insert cross-project parent: %v", err)
		}
		child := mustGetReq(t, reqSvc, childID)
		child.ParentID = "req_cross_proj"
		parent, block, _ := h.buildParentBlock(context.Background(), child, "")
		if parent != nil || block != "" {
			t.Fatalf("cross-project parent must degrade to (nil, \"\"), got (%v, %q)", parent != nil, block)
		}
	})

	t.Run("parent is itself a child", func(t *testing.T) {
		if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title, parent_id) VALUES (?, ?, ?, ?)`,
			"req_grandparent", "proj_parent_ctx", "祖父需求", "req_ctx_parent"); err != nil {
			t.Fatalf("insert grandparent: %v", err)
		}
		child := mustGetReq(t, reqSvc, childID)
		child.ParentID = "req_grandparent"
		parent, block, _ := h.buildParentBlock(context.Background(), child, "")
		if parent != nil || block != "" {
			t.Fatalf("grandparent must degrade to (nil, \"\") — nesting is one level, got (%v, %q)", parent != nil, block)
		}
	})

	t.Run("no parent id at all", func(t *testing.T) {
		child := mustGetReq(t, reqSvc, childID)
		child.ParentID = ""
		parent, block, _ := h.buildParentBlock(context.Background(), child, "")
		if parent != nil || block != "" {
			t.Fatalf("top-level requirement must yield (nil, \"\"), got (%v, %q)", parent != nil, block)
		}
	})
}

// Downstream reads ONLY design_docs. So the persisted plan is the last place
// the parent context can be guaranteed — these three cases pin the contract:
// section present → verbatim; section missing → appendix + a warning the user
// actually sees; no parent at all → the plan is never touched.
func TestFinalizeArchitectRun_ParentContextPersistence(t *testing.T) {
	const withSection = "# 技术方案\n\n## 需求理解\n\n正文。\n\n## 主需求关联上下文\n\n### 来源\n\n主需求标题 (req_ctx_parent)\n"
	const withoutSection = "# 技术方案\n\n## 需求理解\n\n正文。\n\n## 整体实现思路\n\n思路。\n"

	t.Run("section present is persisted verbatim", func(t *testing.T) {
		d := newParentCtxDB(t)
		_, childID := seedParentChild(t, d, "# 主方案\n\n主需求方案正文。")
		reqSvc := service.NewRequirementService(d, nil)
		jobs := store.NewJobStore(8)
		h := &WizardHandler{reqSvc: reqSvc, jobs: jobs}
		job := jobs.Create(childID)

		req := mustGetReq(t, reqSvc, childID)
		h.finalizeArchitectRun(
			claudeStreamOutcome{planContent: withSection, sessionID: "sid"},
			&designRunParams{
				Req:         req,
				ParentBlock: "some-block",
				Parent:      &model.Requirement{ID: "req_ctx_parent", Title: "主需求标题", DesignDocs: "# 主方案"},
			},
			job, nil, "", "sid", childID, "sonnet",
		)

		got := mustGetReq(t, reqSvc, childID)
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
		_, childID := seedParentChild(t, d, "# 主方案\n\n主需求方案正文。")
		reqSvc := service.NewRequirementService(d, nil)
		jobs := store.NewJobStore(8)
		h := &WizardHandler{reqSvc: reqSvc, jobs: jobs}
		job := jobs.Create(childID)

		req := mustGetReq(t, reqSvc, childID)
		h.finalizeArchitectRun(
			claudeStreamOutcome{planContent: withoutSection, sessionID: "sid"},
			&designRunParams{
				Req:         req,
				ParentBlock: "some-block",
				Parent:      &model.Requirement{ID: "req_ctx_parent", Title: "主需求标题", DesignDocs: "# 主方案\n\n主需求方案正文。"},
			},
			job, nil, "", "sid", childID, "sonnet",
		)

		got := mustGetReq(t, reqSvc, childID)
		if !strings.HasPrefix(got.DesignDocs, withoutSection) {
			t.Errorf("original plan must be preserved verbatim, got:\n%s", got.DesignDocs)
		}
		for _, want := range []string{
			"## 主需求关联上下文（自动附录）",
			"req_ctx_parent",
			"主需求方案正文。",
			"未经提炼",
		} {
			if !strings.Contains(got.DesignDocs, want) {
				t.Errorf("persisted plan is missing appendix content %q\n---\n%s", want, got.DesignDocs)
			}
		}
		// The user must be told, in the SSE panel, that the model skipped the
		// contracted section — otherwise a silently weaker plan reads as a
		// fully-distilled one.
		var warned bool
		for _, l := range mustLines(t, job) {
			if strings.Contains(l, "未包含「主需求关联上下文」章节") && strings.Contains(l, "已自动追加附录") {
				warned = true
			}
		}
		if !warned {
			t.Errorf("expected a job-log warning about the missing section, got: %+v", mustLines(t, job))
		}
	})

	t.Run("no parent leaves the plan untouched", func(t *testing.T) {
		d := newParentCtxDB(t)
		_, childID := seedParentChild(t, d, "# 主方案")
		reqSvc := service.NewRequirementService(d, nil)
		jobs := store.NewJobStore(8)
		h := &WizardHandler{reqSvc: reqSvc, jobs: jobs}
		job := jobs.Create(childID)

		req := mustGetReq(t, reqSvc, childID)
		// ParentBlock empty = top-level requirement (or a degraded parent
		// lookup). The plan must land byte-for-byte.
		h.finalizeArchitectRun(
			claudeStreamOutcome{planContent: withoutSection, sessionID: "sid"},
			&designRunParams{Req: req},
			job, nil, "", "sid", childID, "sonnet",
		)

		got := mustGetReq(t, reqSvc, childID)
		if got.DesignDocs != withoutSection {
			t.Errorf("top-level plan must not be modified\nwant:\n%s\ngot:\n%s", withoutSection, got.DesignDocs)
		}
	})
}

// Refine rewrites the doc from scratch every turn, and the distilled parent
// section reads like context the conversation never discussed — exactly what a
// model prunes. The keep rule has to fire for the sections that actually exist,
// and stay silent otherwise (instructing retention of an absent section just
// invites the model to invent one).
func TestParentContextKeepRule(t *testing.T) {
	d := newParentCtxDB(t)
	_, childID := seedParentChild(t, d, "")
	reqSvc := service.NewRequirementService(d, nil)
	req := mustGetReq(t, reqSvc, childID)

	t.Run("child with the section gets the rule", func(t *testing.T) {
		got := parentContextKeepRule(req, "# 方案\n\n"+promptpkg.ParentContextHeading+"\n\n### 来源\n")
		if !strings.Contains(got, "保留") {
			t.Errorf("want the retention rule, got %q", got)
		}
	})

	t.Run("fallback appendix variant gets the rule", func(t *testing.T) {
		got := parentContextKeepRule(req, "# 方案\n\n## 主需求关联上下文（自动附录）\n")
		if got == "" {
			t.Errorf("the machine-written appendix must also be sticky across refine")
		}
	})

	t.Run("child without the section gets nothing", func(t *testing.T) {
		if got := parentContextKeepRule(req, "# 方案\n\n## 需求理解\n"); got != "" {
			t.Errorf("absent section must not produce a keep rule, got %q", got)
		}
	})

	t.Run("top-level requirement gets nothing", func(t *testing.T) {
		top := *req
		top.ParentID = ""
		got := parentContextKeepRule(&top, "# 方案\n\n"+promptpkg.ParentContextHeading+"\n")
		if got != "" {
			t.Errorf("a requirement with no parent has no parent section to keep, got %q", got)
		}
	})

	t.Run("nil requirement is safe", func(t *testing.T) {
		if got := parentContextKeepRule(nil, "# 方案\n\n"+promptpkg.ParentContextHeading+"\n"); got != "" {
			t.Errorf("nil requirement must yield \"\", got %q", got)
		}
	})
}

func mustGetReq(t *testing.T, svc *service.RequirementService, id string) *model.Requirement {
	t.Helper()
	r, err := svc.Get(id)
	if err != nil {
		t.Fatalf("get requirement %s: %v", id, err)
	}
	return r
}

func mustLines(t *testing.T, job *store.Job) []string {
	t.Helper()
	raw, _, _ := job.Snapshot()
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		out = append(out, l.Type+": "+l.Content)
	}
	return out
}
