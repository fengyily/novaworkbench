package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// The requirement_findings table and the requirements.parent_design_hash column
// carry the child → master 「实施发现」 channel. Both are created through the
// canonical schema / the append-only alterColumns list, so they have to exist
// on a fresh install AND on an existing one, and the migration must be safe
// to run twice (it runs on EVERY boot).
//
// The MySQL rewrite matters most here: MySQL cannot index a TEXT column, and
// the table indexes parent_id — so parent_id must be in the mysqlIndexedCol
// whitelist or CREATE TABLE fails on boot.

func TestRequirementFindingsTableMigratesIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.db")
	d, err := Init(Config{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer d.Close()

	// Init already ran migrate() once. Run it again — the duplicate-object
	// errors must be swallowed.
	if err := migrate(d); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	cols := map[string]bool{}
	rows, err := d.Query("SELECT name FROM pragma_table_info('requirement_findings')")
	if err != nil {
		t.Fatalf("pragma_table_info(requirement_findings): %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[n] = true
	}
	for _, want := range []string{
		"id", "requirement_id", "parent_id", "stage", "severity", "status",
		"title", "detail", "resolution", "source_section", "parent_design_hash",
		"absorbed_seq", "absorbed_at_rev", "created_at", "updated_at", "reviewed_at",
	} {
		if !cols[want] {
			t.Errorf("requirement_findings.%s: column missing", want)
		}
	}

	// parent_design_hash on requirements.
	var n int
	if err := d.QueryRow("SELECT COUNT(1) FROM pragma_table_info('requirements') WHERE name = ?", "parent_design_hash").Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(requirements.parent_design_hash): %v", err)
	}
	if n != 1 {
		t.Fatal("requirements.parent_design_hash: column not present after migrate")
	}
	// The default must be '' — "never designed against a parent", NOT NULL.
	var dflt string
	if err := d.QueryRow("SELECT dflt_value FROM pragma_table_info('requirements') WHERE name = 'parent_design_hash'").Scan(&dflt); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	if dflt != "''" {
		t.Errorf("requirements.parent_design_hash default = %q, want %q", dflt, "''")
	}
}

// MySQL cannot index a TEXT column. The table indexes parent_id, so
// parent_id has to be in the mysqlIndexedCol whitelist — otherwise CREATE
// TABLE fails on boot and the whole app is down.
func TestRequirementFindingsMySQLDialect(t *testing.T) {
	var s string
	for _, stmt := range alterColumns {
		s += fixupSchema(MySQL, stmt) + "\n"
	}
	want := "ALTER TABLE requirements ADD COLUMN parent_design_hash TEXT NOT NULL DEFAULT ('')"
	if !strings.Contains(s, want) {
		t.Errorf("mysql schema missing %q", want)
	}
	if !mysqlIndexedCol.MatchString("parent_id TEXT") {
		t.Error("mysqlIndexedCol must whitelist parent_id — requirement_findings indexes it")
	}
}

// Postgres needs no TEXT-vs-VARCHAR rewrite for an unindexed column, but the
// indexed parent_id is rewritten to VARCHAR there.
func TestRequirementFindingsPostgresDialect(t *testing.T) {
	var s string
	for _, stmt := range alterColumns {
		s += fixupSchema(Postgres, stmt) + "\n"
	}
	want := "ALTER TABLE requirements ADD COLUMN parent_design_hash TEXT NOT NULL DEFAULT ''"
	if !strings.Contains(s, want) {
		t.Errorf("pg schema missing %q", want)
	}
}
