package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// findings_source records how a Review report's structured findings were
// obtained ("contract" / "markdown" / "llm"). It is added through the
// append-only `alterColumns` list, which runs on EVERY boot — so the ALTER
// must be safe to run twice, must exist on a fresh install too, and must
// default to '' (the "no structured findings" state) on a row that never
// set it.
//
// A column that existed on a fresh install but not on an existing one (or
// the other way round) would make the chip show on one database and not on
// the other.
func TestFindingsSourceColumnMigratesIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings-source.db")
	d, err := Init(Config{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer d.Close()

	// Init already ran migrate() once. Run it again — the duplicate-column
	// errors must be swallowed.
	if err := migrate(d); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var n int
	row := d.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", "code_reviews", "findings_source")
	if err := row.Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(code_reviews.findings_source): %v", err)
	}
	if n != 1 {
		t.Fatalf("code_reviews.findings_source: column not present after migrate")
	}

	// The DEFAULT must be '' — a NULL would break every scan (NOT NULL) and
	// would mean "unknown" instead of "not graded".
	var dflt string
	row = d.QueryRow("SELECT dflt_value FROM pragma_table_info('code_reviews') WHERE name = 'findings_source'")
	if err := row.Scan(&dflt); err != nil {
		t.Fatalf("pragma_table_info(code_reviews.findings_source): %v", err)
	}
	if dflt != "''" {
		t.Errorf("code_reviews.findings_source default = %q, want %q", dflt, "''")
	}
}

// The MySQL rewrite: MySQL rejects a literal DEFAULT on a TEXT column, so
// fixupSchema turns `TEXT NOT NULL DEFAULT ''` into the expression form
// `DEFAULT ('')`. Without the rewrite the MySQL migration fails on boot.
func TestFindingsSourceColumnMySQLDialect(t *testing.T) {
	var s string
	for _, stmt := range alterColumns {
		s += fixupSchema(MySQL, stmt) + "\n"
	}
	want := "ALTER TABLE code_reviews ADD COLUMN findings_source TEXT NOT NULL DEFAULT ('')"
	if !strings.Contains(s, want) {
		t.Errorf("mysql schema missing %q", want)
	}
}

// Postgres has no TEXT-vs-VARCHAR problem here (the column is not
// indexed), so the statement only has to survive the rewrite verbatim.
func TestFindingsSourceColumnPostgresDialect(t *testing.T) {
	var s string
	for _, stmt := range alterColumns {
		s += fixupSchema(Postgres, stmt) + "\n"
	}
	want := "ALTER TABLE code_reviews ADD COLUMN findings_source TEXT NOT NULL DEFAULT ''"
	if !strings.Contains(s, want) {
		t.Errorf("pg schema missing %q", want)
	}
}
