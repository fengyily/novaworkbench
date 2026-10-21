package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// The automated-testing tables are the first schema addition that carries a
// large block of `--` comments, and migrate() splits canonicalSchema on a
// bare ";" — so a semicolon inside a comment silently truncates the CREATE.
// These tests exist to catch that class of bug, not just "the table exists".

func openTempSQLite(t *testing.T) *DB {
	t.Helper()
	d, err := Init(Config{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestTestingTablesExist(t *testing.T) {
	d := openTempSQLite(t)

	tables := []string{"test_environments", "test_plans", "test_runs", "test_run_cases"}
	for _, tbl := range tables {
		var n int
		if err := d.QueryRow(
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", tbl,
		).Scan(&n); err != nil {
			t.Fatalf("query sqlite_master for %s: %v", tbl, err)
		}
		if n != 1 {
			t.Errorf("table %s missing", tbl)
		}
	}

	indexes := []string{
		"idx_test_envs_project", "uq_test_envs_name",
		"idx_test_plans_env", "idx_test_plans_project_env", "idx_test_plans_req",
		"idx_test_runs_env", "idx_test_runs_project_env",
		"idx_test_run_cases_run",
	}
	for _, idx := range indexes {
		var n int
		if err := d.QueryRow(
			"SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", idx,
		).Scan(&n); err != nil {
			t.Fatalf("query sqlite_master for %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("index %s missing", idx)
		}
	}
}

// A truncated CREATE TABLE still produces a row in sqlite_master for some
// dialects, but it will be missing its trailing columns. Assert the LAST
// column of each new table exists — that is exactly what a semicolon-in-a-
// comment bug eats.
func TestTestingTablesHaveTrailingColumns(t *testing.T) {
	d := openTempSQLite(t)
	cases := map[string]string{
		"test_environments": "retention_runs",
		"test_plans":        "last_job_id",
		"test_runs":         "duration_ms",
		"test_run_cases":    "requirement_id",
	}
	for tbl, col := range cases {
		var n int
		err := d.QueryRow(
			"SELECT count(*) FROM pragma_table_info(?) WHERE name=?", tbl, col,
		).Scan(&n)
		if err != nil {
			t.Fatalf("pragma_table_info(%s): %v", tbl, err)
		}
		if n != 1 {
			t.Errorf("%s is missing its trailing column %q — a CREATE was probably truncated by a semicolon inside a -- comment", tbl, col)
		}
	}
}

// Re-running migrate() must be a no-op (the server migrates on every boot),
// and it must not error on the new tables.
func TestMigrateIdempotentWithTestingTables(t *testing.T) {
	d := openTempSQLite(t)
	if err := migrate(d); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := migrate(d); err != nil {
		t.Fatalf("third migrate: %v", err)
	}
}

// The unique index on (project_id, name) is what stops a user creating two
// environments called "dev" — exercise it directly.
func TestTestEnvsUniqueNamePerProject(t *testing.T) {
	d := openTempSQLite(t)
	ins := `INSERT INTO test_environments (id, project_id, name) VALUES (?, ?, ?)`
	if _, err := d.Exec(ins, "tenv_1", "proj_1", "dev"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := d.Exec(ins, "tenv_2", "proj_1", "dev"); err == nil {
		t.Error("expected a unique violation for a duplicate (project_id, name)")
	}
	// Same name in a DIFFERENT project must be allowed.
	if _, err := d.Exec(ins, "tenv_3", "proj_2", "dev"); err != nil {
		t.Errorf("same name in another project must be allowed: %v", err)
	}
}

// Defaults must be usable without the service layer filling them in: a bare
// INSERT should land as pending/chromium/20 with empty JSON arrays.
func TestTestEnvDefaults(t *testing.T) {
	d := openTempSQLite(t)
	if _, err := d.Exec(
		`INSERT INTO test_environments (id, project_id, name) VALUES ('tenv_d', 'proj_d', 'defaults')`,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var status, browsers, seed string
	var retention int
	err := d.QueryRow(
		`SELECT init_status, browsers, seed_test, retention_runs FROM test_environments WHERE id = 'tenv_d'`,
	).Scan(&status, &browsers, &seed, &retention)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "pending" || browsers != "chromium" || seed != "" || retention != 20 {
		t.Errorf("defaults = %q/%q/%q/%d, want pending/chromium/\"\"/20", status, browsers, seed, retention)
	}

	if _, err := d.Exec(
		`INSERT INTO test_run_cases (id, run_id) VALUES ('trc_d', 'trun_d')`,
	); err != nil {
		t.Fatalf("insert case: %v", err)
	}
	var cs, att, iu string
	var issueNum int
	if err := d.QueryRow(
		`SELECT status, attachments, issue_url, issue_number FROM test_run_cases WHERE id = 'trc_d'`,
	).Scan(&cs, &att, &iu, &issueNum); err != nil {
		t.Fatalf("select case: %v", err)
	}
	if cs != "passed" || att != "[]" || iu != "" || issueNum != 0 {
		t.Errorf("case defaults = %q/%q/%q/%d, want passed/[]/\"\"/0", cs, att, iu, issueNum)
	}
}

// copyOrder drives `nova -migrate`. The four testing tables must appear
// parents-before-children, otherwise the FK-constrained INSERTs fail on a
// strict dialect (SQLite does not enforce FKs by default, so this would
// otherwise pass unnoticed until someone runs the Postgres migration).
func TestCopyOrderTestingTablesAreParentFirst(t *testing.T) {
	idx := map[string]int{}
	for i, tbl := range copyOrder {
		idx[tbl] = i
	}
	order := []string{"test_environments", "test_plans", "test_runs", "test_run_cases"}
	for _, tbl := range order {
		if _, ok := idx[tbl]; !ok {
			t.Errorf("copyOrder is missing %q", tbl)
		}
	}
	if len(order) > 1 && idx[order[0]] >= idx[order[3]] {
		t.Error("test_environments must be copied before test_run_cases")
	}
	if idx["test_environments"] >= idx["test_runs"] {
		t.Error("test_environments must be copied before test_runs")
	}
	if idx["test_runs"] >= idx["test_run_cases"] {
		t.Error("test_runs must be copied before test_run_cases")
	}
}

// MySQL cannot index TEXT columns, so fixupSchema rewrites a fixed list of
// column names to VARCHAR. env_id and run_id are FK/index targets in the new
// tables — if they were missing from that list, `go run` against MySQL would
// fail at CREATE INDEX time with a dialect error that SQLite never surfaces.
func TestFixupConvertsTestingFKColumnsForMySQL(t *testing.T) {
	out := fixupSchema(MySQL, canonicalSchema)
	for _, col := range []string{"env_id", "run_id", "project_id", "requirement_id"} {
		if strings.Contains(out, col+" ") && !strings.Contains(out, col+" VARCHAR(") {
			// Only assert for columns that actually appear in the testing
			// block; requirement_id has its own dedicated VARCHAR rewrite.
			if strings.Contains(out, col+" TEXT") {
				t.Errorf("column %q survived fixupSchema as TEXT — MySQL will refuse to index it", col)
			}
		}
	}
	// MySQL has no CREATE INDEX IF NOT EXISTS.
	if strings.Contains(out, "CREATE INDEX IF NOT EXISTS") {
		t.Error("MySQL fixup must strip CREATE INDEX IF NOT EXISTS")
	}
	// Postgres maps DATETIME to TIMESTAMP.
	pg := fixupSchema(Postgres, canonicalSchema)
	if strings.Contains(pg, "DATETIME") {
		t.Error("Postgres fixup must map DATETIME to TIMESTAMP")
	}
}
