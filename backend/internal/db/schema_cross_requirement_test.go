package db

import (
	"regexp"
	"strings"
	"testing"
)

// TestCrossRequirementSchema pins the DDL of the cross-project requirement
// tables across the three dialects.
//
// The thing that can silently break: MySQL cannot index a bare TEXT column,
// so every column that carries a UNIQUE constraint or an index
// (cross_requirement_id / phase_id) must be rewritten to VARCHAR(64) —
// otherwise `CREATE TABLE` fails on MySQL and the backend refuses to boot.
//
// requirements.cross_requirement_id is deliberately NOT indexed: on a legacy
// MySQL database the column arrives through ALTER TABLE (still TEXT), and
// indexing it would raise "BLOB/TEXT column used in key specification".
func TestCrossRequirementSchema(t *testing.T) {
	mysql := fixupSchema(MySQL, canonicalSchema)

	for _, table := range []string{
		"cross_requirements",
		"cross_requirement_projects",
		"cross_requirement_phases",
		"cross_requirement_phase_items",
	} {
		if !strings.Contains(canonicalSchema, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("canonical schema missing the %s table", table)
		}
		if !strings.Contains(mysql, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("mysql schema missing the %s table", table)
		}
	}

	// The indexed / unique columns must be VARCHAR on MySQL. The whitespace
	// between the column name and the type is preserved by the rewrite, so
	// the check is whitespace-insensitive.
	for _, c := range []string{
		`cross_requirement_id\s+VARCHAR\(64\)`,
		`phase_id\s+VARCHAR\(64\)`,
	} {
		if !regexp.MustCompile(c).MatchString(mysql) {
			t.Errorf("mysql schema missing %q", c)
		}
	}
	if strings.Contains(mysql, "cross_requirement_id TEXT") {
		t.Error("mysql schema still indexes a bare TEXT cross_requirement_id")
	}
	if strings.Contains(mysql, "phase_id TEXT") {
		t.Error("mysql schema still indexes a bare TEXT phase_id")
	}

	// The requirements column arrives through alterColumns (append-only).
	found := false
	for _, stmt := range alterColumns {
		if strings.Contains(stmt, "ADD COLUMN cross_requirement_id") {
			found = true
			break
		}
	}
	if !found {
		t.Error("alterColumns does not add requirements.cross_requirement_id")
	}

	// split_batch_id carries the 「一个跨项目需求只能拆一次」不变量: it is
	// claimed atomically by the split confirm path. It must exist in the
	// canonical DDL and be added to older databases through alterColumns.
	if !strings.Contains(canonicalSchema, "split_batch_id      TEXT NOT NULL DEFAULT ''") {
		t.Error("canonical schema missing cross_requirements.split_batch_id")
	}
	found = false
	for _, stmt := range alterColumns {
		if strings.Contains(stmt, "ALTER TABLE cross_requirements ADD COLUMN split_batch_id") {
			found = true
			break
		}
	}
	if !found {
		t.Error("alterColumns does not add cross_requirements.split_batch_id")
	}
}

// TestCrossRequirementMigrateOrder pins the copy order: the cross-requirement
// tables travel with their parent before their children, so a cross-dialect
// migration does not drop the mapping / phase rows.
func TestCrossRequirementMigrateOrder(t *testing.T) {
	pos := map[string]int{}
	for i, table := range copyOrder {
		pos[table] = i
	}
	for _, table := range []string{
		"cross_requirements",
		"cross_requirement_projects",
		"cross_requirement_phases",
		"cross_requirement_phase_items",
	} {
		if _, ok := pos[table]; !ok {
			t.Errorf("copyOrder is missing %s", table)
		}
	}
	if pos["cross_requirements"] > pos["cross_requirement_projects"] {
		t.Error("cross_requirement_projects must be copied after cross_requirements")
	}
	if pos["cross_requirement_phases"] > pos["cross_requirement_phase_items"] {
		t.Error("cross_requirement_phase_items must be copied after cross_requirement_phases")
	}
}
