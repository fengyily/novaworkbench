package service

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
)

// knowledgeTestDB opens a fresh SQLite db in a temp dir (migrations applied by
// db.Init) and seeds one project + a few knowledge rows for ListForRequirement
// tests.
func knowledgeTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	if _, err := d.Exec("INSERT INTO projects (id, name, local_path, description) VALUES ('proj_1', 'Test', '/tmp/proj1', '')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	seed := []struct {
		id, title, content, category, source string
	}{
		{"kb_arch", "CLAUDE.md", "项目概览：这里描述了 graphql 服务架构与编码约定。", "architecture", "document"},
		{"kb_agents", "AGENTS.md", "开发代理说明。", "architecture", "document"},
		{"kb_struct", "Project Structure", "## Project Structure\n- backend\n- frontend", "architecture", "code"},
		{"kb_req", "已有需求：登录校验", "登录与权限相关的历史需求归档。", "requirement", "requirement"},
	}
	for _, r := range seed {
		if _, err := d.Exec(
			"INSERT INTO knowledge (id, project_id, title, content, category, source_type, is_reviewed, is_approved) VALUES (?, 'proj_1', ?, ?, ?, ?, 1, 1)",
			r.id, r.title, r.content, r.category, r.source); err != nil {
			t.Fatalf("seed knowledge %s: %v", r.id, err)
		}
	}
	return d
}

func TestListForRequirement(t *testing.T) {
	d := knowledgeTestDB(t)
	ks := NewKnowledgeService(d)

	t.Run("title term match", func(t *testing.T) {
		items, err := ks.ListForRequirement("proj_1", "GraphQL 服务设计", 10)
		if err != nil {
			t.Fatalf("ListForRequirement: %v", err)
		}
		// The "graphql" term matches kb_arch only; the remaining limit is
		// topped up with architecture documents, never the requirement row.
		if len(items) == 0 {
			t.Fatal("expected at least the graphql-matched entry")
		}
		if items[0].ID != "kb_arch" {
			t.Fatalf("first item = %s (want kb_arch, most recent match)", items[0].ID)
		}
		for _, k := range items {
			if k.ID == "kb_req" {
				t.Fatalf("requirement row leaked into result: %+v", k)
			}
		}
	})

	t.Run("matches title terms too", func(t *testing.T) {
		items, err := ks.ListForRequirement("proj_1", "登录需求", 10)
		if err != nil {
			t.Fatalf("ListForRequirement: %v", err)
		}
		if len(items) == 0 {
			t.Fatal("expected the 登录 entry for the 登录 keyword")
		}
		if items[0].ID != "kb_req" {
			t.Fatalf("first item = %s (want kb_req)", items[0].ID)
		}
	})

	t.Run("no match falls back to architecture", func(t *testing.T) {
		// A pure-ASCII title with no overlap with the seeded contents must not
		// match anything, so the result is exactly the architecture top-up.
		items, err := ks.ListForRequirement("proj_1", "Cross Platform Rendering", 10)
		if err != nil {
			t.Fatalf("ListForRequirement: %v", err)
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 architecture entries, got %d", len(items))
		}
		for _, k := range items {
			if k.Category != "architecture" {
				t.Fatalf("non-architecture entry in fallback: %+v", k)
			}
		}
	})

	t.Run("empty title returns architecture", func(t *testing.T) {
		items, err := ks.ListForRequirement("proj_1", "", 10)
		if err != nil {
			t.Fatalf("ListForRequirement: %v", err)
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 architecture entries, got %d", len(items))
		}
	})

	t.Run("limit applied", func(t *testing.T) {
		items, err := ks.ListForRequirement("proj_1", "GraphQL", 1)
		if err != nil {
			t.Fatalf("ListForRequirement: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("expected 1 item under limit=1, got %d", len(items))
		}
		if items[0].ID != "kb_arch" {
			t.Fatalf("first item = %s (want kb_arch)", items[0].ID)
		}
	})

	t.Run("unknown project returns empty", func(t *testing.T) {
		items, err := ks.ListForRequirement("proj_missing", "anything", 10)
		if err != nil {
			t.Fatalf("ListForRequirement: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("expected no items for unknown project, got %d", len(items))
		}
	})
}

// TestCreateFromPdf covers the end-to-end PDF → knowledge-row pipeline with
// mock extract + polish closures. The DB is real (knowledgeTestDB) so the
// INSERT and the post-insert Get go through the same code path the handler
// hits. Asserts: (1) source_type='pdf_upload', (2) category='document',
// (3) source_ref starts with "pdf:", (4) title contains the original
// filename, (5) content equals the polished markdown verbatim.
func TestCreateFromPdf(t *testing.T) {
	d := knowledgeTestDB(t)
	ks := NewKnowledgeService(d)

	// Mock raw extractor — ignores the path, returns a fixed string. Lets us
	// verify extract is called and the result is forwarded to polish.
	extract := func(path string) (string, error) {
		if path == "" {
			return "", errExtractEmpty
		}
		return "raw invoice text line 1\nraw invoice text line 2", nil
	}
	// Mock LLM polish — returns a markdown body with a heading. The
	// pdfKnowledgeTitle helper is expected to lift the first heading into
	// the knowledge.title field.
	polish := func(rawText, file string) (string, *llm.Usage, error) {
		if rawText == "" {
			return "", nil, errPolishEmpty
		}
		return "# Invoice 2026-01\n\n- item A\n- item B\n", &llm.Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
			Model:            "test-model",
		}, nil
	}

	const filename = "invoice-2026-01.pdf"
	k, usage, err := ks.CreateFromPdf(CreateKnowledgeFromPdfReq{
		ProjectID:  "proj_1",
		SourceFile: filename,
		FilePath:   "/tmp/fake.pdf",
	}, extract, polish)
	if err != nil {
		t.Fatalf("CreateFromPdf: %v", err)
	}
	if k == nil {
		t.Fatal("expected non-nil knowledge row")
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 {
		t.Fatalf("expected usage {10, 5, test-model}, got %+v", usage)
	}

	// (1) source_type
	if k.SourceType != SourceTypePdfUpload {
		t.Errorf("SourceType = %q, want %q", k.SourceType, SourceTypePdfUpload)
	}
	// (2) category
	if k.Category != CategoryDocument {
		t.Errorf("Category = %q, want %q", k.Category, CategoryDocument)
	}
	// (3) source_ref starts with "pdf:" and is sha256(filename)[:16]
	if !strings.HasPrefix(k.SourceRef, "pdf:") {
		t.Errorf("SourceRef %q missing 'pdf:' prefix", k.SourceRef)
	}
	wantRefSum := sha256.Sum256([]byte(filename))
	wantRef := "pdf:" + hex.EncodeToString(wantRefSum[:])[:16]
	if k.SourceRef != wantRef {
		t.Errorf("SourceRef = %q, want %q", k.SourceRef, wantRef)
	}
	// (4) title contains the original filename
	if !strings.Contains(k.Title, "Invoice") {
		t.Errorf("Title %q does not contain polish-extracted header", k.Title)
	}
	// (5) content equals polish output verbatim
	if k.Content != "# Invoice 2026-01\n\n- item A\n- item B\n" {
		t.Errorf("Content = %q, want polished markdown verbatim", k.Content)
	}
	if k.ProjectID != "proj_1" {
		t.Errorf("ProjectID = %q, want proj_1", k.ProjectID)
	}

	// error path: extract failure short-circuits before any DB write
	extErr := func(path string) (string, error) { return "", errExtractBoom }
	if _, _, err := ks.CreateFromPdf(CreateKnowledgeFromPdfReq{
		ProjectID: "proj_1", SourceFile: filename, FilePath: "/tmp/x.pdf",
	}, extErr, polish); err == nil {
		t.Error("expected error when extract returns an error")
	}
	// error path: polish failure also short-circuits
	badPolish := func(rawText, file string) (string, *llm.Usage, error) {
		return "", nil, errPolishBoom
	}
	if _, _, err := ks.CreateFromPdf(CreateKnowledgeFromPdfReq{
		ProjectID: "proj_1", SourceFile: filename, FilePath: "/tmp/x.pdf",
	}, extract, badPolish); err == nil {
		t.Error("expected error when polish returns an error")
	}
}

var (
	errExtractEmpty = stringErr("extract: empty path")
	errPolishEmpty  = stringErr("polish: empty raw text")
	errExtractBoom  = stringErr("extract: simulated failure")
	errPolishBoom   = stringErr("polish: simulated failure")
)

type stringErr string

func (e stringErr) Error() string { return string(e) }
