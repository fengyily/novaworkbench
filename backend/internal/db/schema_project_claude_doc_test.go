package db

import (
	"strings"
	"testing"
)

// TestProjectClaudeDocsSchema pins the DDL of the project-level CLAUDE.md
// table across the three dialects.
//
// The two things that can silently break:
//   - MySQL cannot index a bare TEXT column, so project_id must be
//     rewritten to VARCHAR(64) (it carries a UNIQUE constraint);
//   - MySQL's TEXT tops out at 64KB and a CLAUDE.md can be bigger, so
//     md_content must become MEDIUMTEXT — and MEDIUMTEXT also forbids a
//     literal default, which the expression-form rewrite has to cover.
func TestProjectClaudeDocsSchema(t *testing.T) {
	mysql := fixupSchema(MySQL, canonicalSchema)
	pg := fixupSchema(Postgres, canonicalSchema)

	// SQLite keeps the canonical DDL as-is.
	if !strings.Contains(canonicalSchema, "CREATE TABLE IF NOT EXISTS project_claude_docs") {
		t.Fatal("canonical schema missing the project_claude_docs table")
	}
	if !strings.Contains(canonicalSchema, "project_id TEXT NOT NULL UNIQUE") {
		t.Error("canonical schema missing the project_id UNIQUE constraint")
	}

	for _, c := range []string{
		"project_id VARCHAR(64) NOT NULL UNIQUE",
		"md_content MEDIUMTEXT NOT NULL DEFAULT ('')",
	} {
		if !strings.Contains(mysql, c) {
			t.Errorf("mysql schema missing %q", c)
		}
	}
	if strings.Contains(mysql, "md_content TEXT") {
		t.Error("mysql schema still has a 64KB-capped md_content")
	}

	// PostgreSQL: TEXT is unbounded, only DATETIME → TIMESTAMP applies.
	if !strings.Contains(pg, "md_content TEXT NOT NULL DEFAULT ''") {
		t.Error("pg schema missing the md_content column")
	}
	if !strings.Contains(pg, "project_id TEXT NOT NULL UNIQUE") {
		t.Error("pg schema missing the project_id UNIQUE constraint")
	}
	if strings.Contains(pg, "md_content MEDIUMTEXT") {
		t.Error("pg schema contains a MySQL-only type")
	}
}

// TestProjectClaudeDocRevisionsSchema pins the version-history table and
// the manual-edit guard column.
//
// The history table is what makes a destructive replace recoverable, and
// the guard column is what makes the replace ask first — both are part of
// the contract, so both are pinned here (a fresh install gets them from
// the canonical DDL, an existing one from the ALTERs in alterColumns).
func TestProjectClaudeDocRevisionsSchema(t *testing.T) {
	mysql := fixupSchema(MySQL, canonicalSchema)
	pg := fixupSchema(Postgres, canonicalSchema)

	for _, s := range []struct {
		name string
		ddl  string
	}{
		{"canonical", canonicalSchema},
		{"mysql", mysql},
		{"postgres", pg},
	} {
		if !strings.Contains(s.ddl, "CREATE TABLE IF NOT EXISTS project_claude_doc_revisions") {
			t.Errorf("%s schema missing the project_claude_doc_revisions table", s.name)
		}
		if !strings.Contains(s.ddl, "manual_edited_at") {
			t.Errorf("%s schema missing the manual_edited_at column", s.name)
		}
	}
	if !strings.Contains(mysql, "md_content MEDIUMTEXT NOT NULL DEFAULT ('')") {
		t.Error("mysql schema missing the MEDIUMTEXT md_content of the history table")
	}
	// The guard column must be added to an EXISTING database too — the
	// table was shipped before the column existed.
	found := false
	for _, stmt := range alterColumns {
		if strings.Contains(stmt, "project_claude_docs") && strings.Contains(stmt, "manual_edited_at") {
			found = true
			break
		}
	}
	if !found {
		t.Error("alterColumns missing the manual_edited_at ALTER for existing databases")
	}
}
