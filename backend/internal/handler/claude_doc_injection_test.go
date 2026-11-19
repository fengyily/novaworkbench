package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
)

// fakeClaudeDocSvc is a claudeDocReader returning a fixed document.
type fakeClaudeDocSvc struct {
	doc *model.ProjectClaudeDoc
	err error
}

func (f fakeClaudeDocSvc) ClaudeDoc(projectID string) (*model.ProjectClaudeDoc, error) {
	return f.doc, f.err
}

// fakeClaudeDocSettings is a claudeDocSettingsReader returning a fixed policy.
type fakeClaudeDocSettings struct {
	cfg service.ClaudeDocInjectionConfig
}

func (f fakeClaudeDocSettings) ClaudeDocInjectionConfig() (service.ClaudeDocInjectionConfig, error) {
	return f.cfg, nil
}

// TestResolveClaudeDocInjectionNativeChannel pins the native path: the
// managed copy is materialized into a directory and the repository's own
// instruction files are excluded, so exactly ONE project document is in the
// context.
func TestResolveClaudeDocInjectionNativeChannel(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { service.SetClaudeDocMemoryRoot("") })
	service.SetClaudeDocMemoryRoot(filepath.Join(root, "memory"))

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("disk copy"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	inj := resolveClaudeDocInjection(
		fakeClaudeDocSvc{doc: &model.ProjectClaudeDoc{Content: "# 托管约定"}},
		fakeClaudeDocSettings{cfg: service.ClaudeDocInjectionConfig{
			InjectMode:       service.ClaudeDocInjectModeNative,
			ExcludeRepoDoc:   true,
			SystemPromptMode: service.ClaudeDocSystemPromptReplace,
		}},
		"proj_1", repo)

	if !inj.Native {
		t.Fatal("the injection did not resolve to the native channel")
	}
	if inj.MemDir == "" {
		t.Error("the native channel has no memory directory")
	}
	if !inj.ExcludeRepo {
		t.Error("the native channel must keep the repository's copy out of the CLI's load set")
	}
	// The prompt must NOT carry the document — it is in the CLI's channel.
	if inj.Block() != "" {
		t.Errorf("the native channel also produced a prompt block:\n%s", inj.Block())
	}
	// The materialized copy has to exist on disk.
	data, err := os.ReadFile(filepath.Join(inj.MemDir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read the materialized copy: %v", err)
	}
	if !strings.Contains(string(data), "托管约定") {
		t.Errorf("materialized content = %q, want the managed document", string(data))
	}
}

// TestResolveClaudeDocInjectionPromptMode pins the fallback: the document is
// spliced into the prompt and nothing is materialized or excluded.
func TestResolveClaudeDocInjectionPromptMode(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { service.SetClaudeDocMemoryRoot("") })
	service.SetClaudeDocMemoryRoot(filepath.Join(root, "memory"))

	inj := resolveClaudeDocInjection(
		fakeClaudeDocSvc{doc: &model.ProjectClaudeDoc{Content: "# 托管约定"}},
		fakeClaudeDocSettings{cfg: service.ClaudeDocInjectionConfig{
			InjectMode:       service.ClaudeDocInjectModePrompt,
			ExcludeRepoDoc:   false,
			SystemPromptMode: service.ClaudeDocSystemPromptReplace,
		}},
		"proj_1", t.TempDir())

	if inj.Native {
		t.Error("the prompt mode must not resolve to the native channel")
	}
	if inj.MemDir != "" {
		t.Errorf("the prompt mode materialized a copy at %q", inj.MemDir)
	}
	if inj.ExcludeRepo {
		t.Error("the prompt mode must not exclude the repository's copy")
	}
	block := inj.Block()
	if !strings.Contains(block, promptpkg.ProjectClaudeDocHeading) {
		t.Errorf("the prompt block is missing the heading:\n%s", block)
	}
	if !strings.Contains(block, "托管约定") {
		t.Errorf("the prompt block is missing the document body:\n%s", block)
	}
}

// TestResolveClaudeDocInjectionWithoutDocument pins the no-op: a project
// without a managed document changes nothing — the prompt stays
// byte-for-byte what it was, and no file is materialized.
func TestResolveClaudeDocInjectionWithoutDocument(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { service.SetClaudeDocMemoryRoot("") })
	service.SetClaudeDocMemoryRoot(filepath.Join(root, "memory"))

	for _, doc := range []*model.ProjectClaudeDoc{nil, {Content: "   \n\t "}} {
		inj := resolveClaudeDocInjection(
			fakeClaudeDocSvc{doc: doc},
			fakeClaudeDocSettings{cfg: service.DefaultClaudeDocInjectionConfig()},
			"proj_1", t.TempDir())
		if inj.Block() != "" {
			t.Errorf("a project without a document produced a prompt block:\n%s", inj.Block())
		}
		if inj.MemDir != "" {
			t.Errorf("a project without a document materialized a copy at %q", inj.MemDir)
		}
	}
}

// TestResolveClaudeDocInjectionNoProject pins the guard against an empty
// project id — without a project there is no document to inject.
func TestResolveClaudeDocInjectionNoProject(t *testing.T) {
	inj := resolveClaudeDocInjection(
		fakeClaudeDocSvc{doc: &model.ProjectClaudeDoc{Content: "# 约定"}},
		fakeClaudeDocSettings{cfg: service.DefaultClaudeDocInjectionConfig()},
		"", t.TempDir())
	if inj.Block() != "" || inj.MemDir != "" {
		t.Errorf("an empty project id produced an injection: %+v", inj)
	}
}

// TestClaudeDocInjectionApplyTo pins how the decision reaches the CLI.
//
// Native mode grants the memory directory and asks for the repository's
// copy to be excluded; prompt mode does neither (the document is in the
// prompt instead).
func TestClaudeDocInjectionApplyTo(t *testing.T) {
	inj := claudeDocInjection{
		Native:      true,
		MemDir:      "/tmp/nova/memory/proj_1",
		ExcludeRepo: true,
	}
	opts := llm.StreamOpts{WorkDir: "/tmp/nova/repo"}
	inj.ApplyTo(&opts)
	if opts.ClaudeMdDir != "/tmp/nova/memory/proj_1" {
		t.Errorf("ClaudeMdDir = %q, want the memory directory", opts.ClaudeMdDir)
	}
	if !opts.ExcludeRepoClaudeMd {
		t.Error("the repository's copy was not excluded")
	}

	promptOpts := llm.StreamOpts{WorkDir: "/tmp/nova/repo"}
	claudeDocInjection{Content: "# 约定"}.ApplyTo(&promptOpts)
	if promptOpts.ClaudeMdDir != "" {
		t.Errorf("the prompt mode granted a directory: %q", promptOpts.ClaudeMdDir)
	}
	if promptOpts.ExcludeRepoClaudeMd {
		t.Error("the prompt mode excluded the repository's copy")
	}
}

// TestFilterOutProjectConventionKnowledge pins the knowledge-block dedup.
//
// The scanner indexes the repository's own CLAUDE.md / AGENTS.md as
// knowledge rows. In native mode the managed copy is delivered through the
// CLI's channel and the repository's copy is excluded there — so keeping
// those rows would put the same document back into the prompt.
func TestFilterOutProjectConventionKnowledge(t *testing.T) {
	items := []model.Knowledge{
		{ID: "kb_1", SourceRef: "CLAUDE.md", Title: "CLAUDE.md"},
		{ID: "kb_2", SourceRef: "AGENTS.md", Title: "AGENTS.md"},
		{ID: "kb_3", SourceRef: "docs/api.md", Title: "API"},
		{ID: "kb_4", Title: "手写条目"},
	}
	got := filterOutProjectConventionKnowledge(items)
	if len(got) != 2 {
		t.Fatalf("filtered = %v, want 2 entries", got)
	}
	for _, k := range got {
		if k.SourceRef == "CLAUDE.md" || k.SourceRef == "AGENTS.md" {
			t.Errorf("the repository's convention document survived: %+v", k)
		}
	}
}
