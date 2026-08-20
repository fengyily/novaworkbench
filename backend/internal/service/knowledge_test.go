package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/db"
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
