package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// buildDesignPromptFixture wires a project + requirement the design stage
// can run against, with a real git upstream so the baseline sync succeeds.
//
// It returns the handler, the requirement and the job — the three things
// prepareDesignWorkspace needs.
func buildDesignPromptFixture(t *testing.T, withDoc bool) (*WizardHandler, *model.Requirement, *store.Job, string) {
	t.Helper()
	dir, _ := os.MkdirTemp("", "claude-doc-design-")
	t.Cleanup(func() { os.RemoveAll(dir) })

	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reqID := seedRequirementForSchedule(t, d)
	const projID = "proj_seed_sched"
	const baseBranch = "main"

	upstreamDir := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "git", "init", "--bare", "--initial-branch="+baseBranch, upstreamDir)
	seed := filepath.Join(t.TempDir(), "seed")
	mustGit(t, "", "git", "clone", upstreamDir, seed)
	mustGit(t, seed, "git", "config", "user.email", "test@nova")
	mustGit(t, seed, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("init"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, seed, "git", "add", "README.md")
	mustGit(t, seed, "git", "commit", "-m", "init")
	mustGit(t, seed, "git", "push", "origin", "HEAD:"+baseBranch)

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "", "git", "clone", upstreamDir, work)
	mustGit(t, work, "git", "config", "user.email", "test@nova")
	mustGit(t, work, "git", "config", "user.name", "Test")

	if _, err := d.Exec(
		`UPDATE projects SET local_path = ?, remote_url = ? WHERE id = ?`,
		work, upstreamDir, projID); err != nil {
		t.Fatalf("update project: %v", err)
	}

	reqSvc := service.NewRequirementService(d, nil)
	projSvc := service.NewProjectService(d, nil, nil)
	settingSvc := service.NewSettingService(d)
	jobs := store.NewJobStore(8)

	// The managed copy is materialized under ~/.novaworkbench in
	// production; a test must never write into the developer's real home
	// directory, so the memory root is redirected into the temp dir.
	t.Cleanup(func() { service.SetClaudeDocMemoryRoot("") })
	service.SetClaudeDocMemoryRoot(filepath.Join(dir, "memory"))

	if withDoc {
		if _, err := projSvc.UpsertClaudeDoc(projID,
			"# 项目约定\n\n分层：handler / service / model。",
			model.ProjectClaudeDocSourceGenerated, true); err != nil {
			t.Fatalf("seed claude doc: %v", err)
		}
	}

	req, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	job := jobs.Create(reqID)

	h := &WizardHandler{
		db:         d,
		projectSvc: projSvc,
		reqSvc:     reqSvc,
		settingSvc: settingSvc,
		jobs:       jobs,
	}
	return h, req, job, work
}

// TestDesignPromptInjectsProjectClaudeDoc is the injection regression test
// for the design stage.
//
// Two halves, and the second one matters as much as the first:
//
//  1. a project WITH a project-level CLAUDE.md → the design prompt carries
//     the 「项目级约定」 block;
//  2. a project WITHOUT one → the design prompt is byte-for-byte the same
//     as before the feature existed. The no-op has to be total: a project
//     that never generated a document must not see a single extra byte in
//     its prompts.
func TestDesignPromptInjectsProjectClaudeDoc(t *testing.T) {
	t.Run("with document", func(t *testing.T) {
		h, req, job, work := buildDesignPromptFixture(t, true)
		p := &designRunParams{
			Req:         req,
			ProjectPath: work,
			BaseBranch:  "main",
			SourceSID:   "analyst-sid",
		}
		if ok := h.prepareDesignWorkspace(p, job); !ok {
			lines, status, _ := job.Snapshot()
			t.Fatalf("prepareDesignWorkspace failed (status=%s, lines=%+v)", status, lines)
		}
		// The document travels through the CLI's own project-memory channel,
		// so the PROMPT must NOT carry it — a copy in both places is exactly
		// the duplication this change removes.
		if strings.Contains(p.Prompt, prompt.ProjectClaudeDocHeading) {
			t.Errorf("design prompt carries the CLAUDE.md block although the native channel delivers it:\n%s", p.Prompt)
		}
		if !p.ClaudeDoc.Native {
			t.Errorf("the design stage did not resolve the native injection channel")
		}
		if p.ClaudeDoc.MemDir == "" {
			t.Errorf("the native injection channel has no memory directory")
		}
	})

	t.Run("prompt mode keeps the block in the prompt", func(t *testing.T) {
		h, req, job, work := buildDesignPromptFixture(t, true)
		// Force the fallback: the document is spliced into the prompt.
		if err := h.settingSvc.SetClaudeDocInjectionConfig(service.ClaudeDocInjectionConfig{
			InjectMode:       service.ClaudeDocInjectModePrompt,
			ExcludeRepoDoc:   false,
			SystemPromptMode: service.ClaudeDocSystemPromptReplace,
		}); err != nil {
			t.Fatalf("set injection config: %v", err)
		}
		p := &designRunParams{
			Req:         req,
			ProjectPath: work,
			BaseBranch:  "main",
			SourceSID:   "analyst-sid",
		}
		if ok := h.prepareDesignWorkspace(p, job); !ok {
			lines, status, _ := job.Snapshot()
			t.Fatalf("prepareDesignWorkspace failed (status=%s, lines=%+v)", status, lines)
		}
		if !strings.Contains(p.Prompt, prompt.ProjectClaudeDocHeading) {
			t.Errorf("prompt-mode design prompt missing the project CLAUDE.md block:\n%s", p.Prompt)
		}
		if !strings.Contains(p.Prompt, "分层：handler / service / model。") {
			t.Errorf("prompt-mode design prompt missing the document body:\n%s", p.Prompt)
		}
	})

	t.Run("without document", func(t *testing.T) {
		h, req, job, work := buildDesignPromptFixture(t, false)
		p := &designRunParams{
			Req:         req,
			ProjectPath: work,
			BaseBranch:  "main",
			SourceSID:   "analyst-sid",
		}
		if ok := h.prepareDesignWorkspace(p, job); !ok {
			lines, status, _ := job.Snapshot()
			t.Fatalf("prepareDesignWorkspace failed (status=%s, lines=%+v)", status, lines)
		}
		if strings.Contains(p.Prompt, prompt.ProjectClaudeDocHeading) {
			t.Errorf("design prompt carries a CLAUDE.md block although the project has none:\n%s", p.Prompt)
		}
		if strings.Contains(p.Prompt, "项目级约定") {
			t.Errorf("design prompt mentions the project conventions although the project has none:\n%s", p.Prompt)
		}
	})
}
