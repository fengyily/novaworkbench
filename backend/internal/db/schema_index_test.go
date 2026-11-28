package db

import (
	"regexp"
	"strings"
	"testing"
)

// TestCanonicalIndexNeverCoversAlterAddedColumn is a **class-level** guard,
// not a per-column one.
//
// (The name differs from the sibling guard `TestNoIndexOnAlterAddedColumn`
// in schema_index_guard_test.go on purpose — the same rule is pinned twice
// in the tree right now, and two files declaring the same test function
// would not compile once both land in one branch.)
//
// The trap: `migrate()` runs the canonical DDL **before** the ALTER list.
// On an existing database `CREATE TABLE IF NOT EXISTS` is a no-op — the table
// is already there, without the columns that were added later — so a
// `CREATE INDEX ... ON <alter-added column>` in the canonical DDL fails with
// "column does not exist" and the backend refuses to boot.
//
// This is not hypothetical: `cross_requirements.deleted_at` (the recycle-bin
// stamp) shipped with such an index and took the Postgres startup down with
// `SQLSTATE 42703`.
//
// The fix pattern (see `requirements.cross_requirement_id`, which has been
// unindexed for the same reason) is: **a column that arrives via ALTER must
// not be indexed by the canonical DDL**. Small tables don't need the index
// anyway.
func TestCanonicalIndexNeverCoversAlterAddedColumn(t *testing.T) {
	// Columns that only exist after the ALTER list ran.
	alterRe := regexp.MustCompile(`ALTER TABLE (\w+) ADD COLUMN (\w+)`)
	altered := map[string]map[string]bool{} // table → column set
	for _, m := range alterRe.FindAllStringSubmatch(alterColumnsText(), -1) {
		if altered[m[1]] == nil {
			altered[m[1]] = map[string]bool{}
		}
		altered[m[1]][m[2]] = true
	}

	// Every index the canonical DDL creates.
	idxRe := regexp.MustCompile(`CREATE INDEX IF NOT EXISTS (\w+) ON (\w+)\(([^)]*)\)`)
	for _, m := range idxRe.FindAllStringSubmatch(canonicalSchema, -1) {
		idxName, table, cols := m[1], m[2], m[3]
		for _, col := range strings.Split(cols, ",") {
			col = strings.TrimSpace(col)
			if altered[table][col] {
				t.Errorf("index %s covers %s.%s, which only arrives via ALTER — "+
					"the CREATE INDEX runs before the ALTER and breaks startup on "+
					"existing databases", idxName, table, col)
			}
		}
	}
}

// alterColumnsText returns the raw source of the alterColumns list.
//
// The list is a []string of SQL statements; the guard needs the statement
// text, not the parsed values, so it re-reads the source via the runtime
// value of the slice.
func alterColumnsText() string {
	return strings.Join(alterColumns, "\n")
}
