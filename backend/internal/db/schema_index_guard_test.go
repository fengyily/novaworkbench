package db

import (
	"regexp"
	"strings"
	"testing"
)

// The canonical DDL runs **before** the ALTER list (see migrate()). On an
// existing database `CREATE TABLE IF NOT EXISTS` is a no-op — the table is
// already there, without the columns that arrive later through ALTER — so a
// `CREATE INDEX ... ON <alter-added column>` in the canonical DDL fails with
// "column does not exist" and the backend refuses to boot.
//
// This is not hypothetical: `cross_requirements.deleted_at` (the recycle-bin
// stamp) shipped with such an index and took the Postgres startup down with
// `SQLSTATE 42703`.
//
// The rule this guard pins: **a column that arrives via ALTER must not be
// indexed by the canonical DDL** — the same trap `requirements.
// cross_requirement_id` avoids by staying unindexed. Small tables do not need
// the index anyway.
func TestNoIndexOnAlterAddedColumn(t *testing.T) {
	for _, hit := range indexesOnAlterAddedColumns(canonicalSchema, strings.Join(alterColumns, "\n")) {
		t.Errorf("index %s covers %s.%s, which only arrives via ALTER — the CREATE INDEX runs before the ALTER and breaks startup on existing databases",
			hit.index, hit.table, hit.column)
	}
}

// alterAddedIndex is one index that covers a column the ALTER list adds.
type alterAddedIndex struct {
	index  string
	table  string
	column string
}

var (
	// ALTER TABLE <table> ADD COLUMN <column> — the column arrives after the
	// canonical DDL has already run.
	alterAddedColumnRe = regexp.MustCompile(`ALTER TABLE\s+(\w+)\s+ADD COLUMN\s+(\w+)`)
	// CREATE [UNIQUE] INDEX [IF NOT EXISTS] <name> ON <table>(<columns>) —
	// the canonical DDL's indexes, plain and unique, on one line or wrapped.
	canonicalIndexRe = regexp.MustCompile(`CREATE\s+(UNIQUE\s+)?INDEX(?:\s+IF\s+NOT\s+EXISTS)?\s+(\w+)\s+ON\s+(\w+)\s*\(([^)]*)\)`)
)

// indexesOnAlterAddedColumns returns every index the canonical DDL creates on
// a column that only exists once the ALTER list has run.
//
// Both inputs are plain SQL text so the checker itself can be tested against
// synthetic schemas — see TestIndexesOnAlterAddedColumns.
func indexesOnAlterAddedColumns(schema, alters string) []alterAddedIndex {
	altered := map[string]map[string]bool{} // table → column set
	for _, m := range alterAddedColumnRe.FindAllStringSubmatch(alters, -1) {
		if altered[m[1]] == nil {
			altered[m[1]] = map[string]bool{}
		}
		altered[m[1]][m[2]] = true
	}

	var hits []alterAddedIndex
	for _, m := range canonicalIndexRe.FindAllStringSubmatch(schema, -1) {
		index, table, columns := m[2], m[3], m[4]
		for _, column := range strings.Split(columns, ",") {
			column = strings.TrimSpace(column)
			if altered[table][column] {
				hits = append(hits, alterAddedIndex{index: index, table: table, column: column})
			}
		}
	}
	return hits
}

// TestIndexesOnAlterAddedColumns pins the checker itself: a guard that cannot
// be shown to fail is not a guard.
func TestIndexesOnAlterAddedColumns(t *testing.T) {
	alters := "ALTER TABLE cross_requirements ADD COLUMN deleted_at DATETIME"

	cases := []struct {
		name   string
		schema string
		want   int
	}{
		{
			name:   "plain index on an alter-added column",
			schema: "CREATE INDEX IF NOT EXISTS idx_xreq_deleted ON cross_requirements(deleted_at);",
			want:   1,
		},
		{
			name:   "unique index on an alter-added column",
			schema: "CREATE UNIQUE INDEX IF NOT EXISTS idx_xreq_deleted ON cross_requirements(deleted_at);",
			want:   1,
		},
		{
			name:   "unique index wrapped onto the next line",
			schema: "CREATE UNIQUE INDEX IF NOT EXISTS idx_xreq_deleted\n\tON cross_requirements(deleted_at);",
			want:   1,
		},
		{
			name:   "composite index with one alter-added column",
			schema: "CREATE INDEX IF NOT EXISTS idx_xreq_status ON cross_requirements(status, deleted_at);",
			want:   1,
		},
		{
			name:   "index on a column the create table owns",
			schema: "CREATE INDEX IF NOT EXISTS idx_xreq_status ON cross_requirements(status);",
			want:   0,
		},
		{
			name:   "index on another table's alter-added column",
			schema: "CREATE INDEX IF NOT EXISTS idx_other ON projects(deleted_at);",
			want:   0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := indexesOnAlterAddedColumns(tc.schema, alters)
			if len(hits) != tc.want {
				t.Fatalf("got %d hits, want %d: %+v", len(hits), tc.want, hits)
			}
			if tc.want == 1 && (hits[0].table != "cross_requirements" || hits[0].column != "deleted_at") {
				t.Errorf("hit names %s.%s, want cross_requirements.deleted_at", hits[0].table, hits[0].column)
			}
		})
	}
}
