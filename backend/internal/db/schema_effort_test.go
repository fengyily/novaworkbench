package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestEffortColumnsMigrateIdempotently pins the migration contract for the
// reasoning-effort columns.
//
// The five columns are added through the append-only `alterColumns` list,
// which runs on EVERY boot. That means the ALTER must be safe to run twice
// (the second run hits "duplicate column" and has to be swallowed), and it
// must produce a column that a fresh install also ends up with — the
// canonical DDL deliberately does NOT carry them, so `alterColumns` is the
// only source of truth for them.
//
// A column that exists after the first migrate but not after the second
// (or vice versa) would make the feature work on a fresh install and break
// on an existing one, or the other way round.
func TestEffortColumnsMigrateIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effort.db")
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

	checks := []struct {
		table string
		col   string
	}{
		{"requirements", "analyst_effort"},
		{"requirements", "architect_effort"},
		{"requirements", "developer_effort"},
		{"sub_tasks", "effort"},
		{"code_reviews", "effort"},
	}
	for _, c := range checks {
		var n int
		row := d.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", c.table, c.col)
		if err := row.Scan(&n); err != nil {
			t.Fatalf("pragma_table_info(%s): %v", c.table, err)
		}
		if n != 1 {
			t.Errorf("%s.%s: column not present after migrate", c.table, c.col)
		}
	}
}

// TestEffortColumnsDefaultToEmpty pins the DEFAULT '' on the new columns.
//
// The columns are NOT NULL, and every read path treats "" as "no tier
// recorded" — the same shape the sibling *_model columns use. A column
// that defaulted to NULL would make every scan fail (NOT NULL) or force
// every read path to handle a null.
func TestEffortColumnsDefaultToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effort-default.db")
	d, err := Init(Config{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer d.Close()

	// A row with the effort columns left unspecified must read back "".
	var got string
	if err := d.QueryRow(`SELECT architect_effort FROM requirements LIMIT 1`).Scan(&got); err != nil {
		// No requirement rows yet — the column's DEFAULT is what we want
		// to pin, and reading it needs a row. Fall through to the schema
		// check below.
		got = ""
	}
	if got != "" {
		t.Errorf("architect_effort default = %q, want \"\"", got)
	}

	var dflt string
	row := d.QueryRow("SELECT dflt_value FROM pragma_table_info('requirements') WHERE name = ?", "architect_effort")
	if err := row.Scan(&dflt); err != nil {
		t.Fatalf("pragma_table_info(requirements.architect_effort): %v", err)
	}
	if dflt != "''" {
		t.Errorf("requirements.architect_effort default = %q, want %q", dflt, "''")
	}
}

// TestEffortColumnsMySQLDialect pins the MySQL rewrite of the new ALTERs.
//
// MySQL rejects a literal DEFAULT on a TEXT column, so fixupSchema rewrites
// `TEXT NOT NULL DEFAULT ''` into the expression form `DEFAULT ('')`. The
// five new columns use exactly that shape, so they must be covered by the
// rewrite — otherwise the MySQL migration fails outright on boot.
func TestEffortColumnsMySQLDialect(t *testing.T) {
	// The ALTERs live in the append-only alterColumns list, NOT in
	// canonicalSchema — and migrate() runs each of them through
	// fixupSchema individually (see schema.go's migrate()).
	var s string
	for _, stmt := range alterColumns {
		s += fixupSchema(MySQL, stmt) + "\n"
	}
	for _, want := range []string{
		"ALTER TABLE requirements ADD COLUMN analyst_effort TEXT NOT NULL DEFAULT ('')",
		"ALTER TABLE requirements ADD COLUMN architect_effort TEXT NOT NULL DEFAULT ('')",
		"ALTER TABLE requirements ADD COLUMN developer_effort TEXT NOT NULL DEFAULT ('')",
		"ALTER TABLE sub_tasks ADD COLUMN effort TEXT NOT NULL DEFAULT ('')",
		"ALTER TABLE code_reviews ADD COLUMN effort TEXT NOT NULL DEFAULT ('')",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("mysql schema missing %q", want)
		}
	}
}

// TestEffortColumnsPostgresDialect pins the Postgres rewrite.
//
// Postgres has no TEXT-vs-VARCHAR problem for these columns (they are not
// indexed), and DATETIME doesn't appear in them — so the only requirement
// is that the statements survive the dialect rewrite verbatim.
func TestEffortColumnsPostgresDialect(t *testing.T) {
	var s string
	for _, stmt := range alterColumns {
		s += fixupSchema(Postgres, stmt) + "\n"
	}
	for _, want := range []string{
		"ALTER TABLE requirements ADD COLUMN analyst_effort TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE requirements ADD COLUMN architect_effort TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE requirements ADD COLUMN developer_effort TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE sub_tasks ADD COLUMN effort TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE code_reviews ADD COLUMN effort TEXT NOT NULL DEFAULT ''",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("pg schema missing %q", want)
		}
	}
}
