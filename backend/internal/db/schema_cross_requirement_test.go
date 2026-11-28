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
		"cross_requirement_bases",
		"cross_requirement_qa_turns",
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

// TestCrossRequirementBasesSchema pins the baseline ledger's DDL.
//
// It is the one cross-requirement table whose UNIQUE constraint spans two
// columns (cross_requirement_id + project_id). MySQL cannot put a UNIQUE on
// a bare TEXT column, so BOTH columns must be rewritten to VARCHAR(64) —
// otherwise `CREATE TABLE` fails on MySQL and the backend refuses to boot.
func TestCrossRequirementBasesSchema(t *testing.T) {
	mysql := fixupSchema(MySQL, canonicalSchema)

	start := strings.Index(mysql, "CREATE TABLE IF NOT EXISTS cross_requirement_bases")
	if start < 0 {
		t.Fatal("mysql schema missing the cross_requirement_bases table")
	}
	end := strings.Index(mysql[start:], ");")
	if end < 0 {
		t.Fatal("cross_requirement_bases DDL is unterminated")
	}
	ddl := mysql[start : start+end+2]

	for _, c := range []string{
		`cross_requirement_id\s+VARCHAR\(64\)`,
		`project_id\s+VARCHAR\(64\)`,
	} {
		if !regexp.MustCompile(c).MatchString(ddl) {
			t.Errorf("cross_requirement_bases DDL missing %q:\n%s", c, ddl)
		}
	}
	if strings.Contains(ddl, "cross_requirement_id TEXT") || strings.Contains(ddl, "project_id TEXT") {
		t.Errorf("cross_requirement_bases still indexes a bare TEXT column:\n%s", ddl)
	}
	if !regexp.MustCompile(`UNIQUE\(\s*cross_requirement_id\s*,\s*project_id\s*\)`).MatchString(ddl) {
		t.Errorf("cross_requirement_bases lost its per-project uniqueness:\n%s", ddl)
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
		"cross_requirement_bases",
		"cross_requirement_attachments",
		"cross_requirement_qa_turns",
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
	if pos["cross_requirements"] > pos["cross_requirement_bases"] {
		t.Error("cross_requirement_bases must be copied after cross_requirements")
	}
	if pos["cross_requirements"] > pos["cross_requirement_attachments"] {
		t.Error("cross_requirement_attachments must be copied after cross_requirements")
	}
	if pos["cross_requirements"] > pos["cross_requirement_qa_turns"] {
		t.Error("cross_requirement_qa_turns must be copied after cross_requirements")
	}
}

// TestCrossRequirementAttachmentsSchema pins the attachment table's DDL.
//
// It is the third owner table driven by service.AttachmentService (requirement
// / code review / cross requirement), so its column shape must match the
// other two exactly — AttachmentService runs the SAME SQL against it with only
// the table + owner column swapped at construction time. A column that drifts
// (say a renamed "description") would blow up at query time, not at boot.
//
// The MySQL rewrite matters for the same reason as everywhere else: the
// indexed columns (cross_requirement_id) must become VARCHAR(64) or
// `CREATE TABLE` fails and the backend refuses to boot.
func TestCrossRequirementAttachmentsSchema(t *testing.T) {
	mysql := fixupSchema(MySQL, canonicalSchema)

	start := strings.Index(mysql, "CREATE TABLE IF NOT EXISTS cross_requirement_attachments")
	if start < 0 {
		t.Fatal("mysql schema missing the cross_requirement_attachments table")
	}
	end := strings.Index(mysql[start:], ");")
	if end < 0 {
		t.Fatal("cross_requirement_attachments DDL is unterminated")
	}
	ddl := mysql[start : start+end+2]

	if !regexp.MustCompile(`cross_requirement_id\s+VARCHAR\(64\)`).MatchString(ddl) {
		t.Errorf("cross_requirement_attachments DDL missing VARCHAR(64) owner column:\n%s", ddl)
	}
	if strings.Contains(ddl, "cross_requirement_id TEXT") {
		t.Errorf("cross_requirement_attachments still indexes a bare TEXT column:\n%s", ddl)
	}
	// The (owner, sha256) uniqueness lives in a separate CREATE UNIQUE INDEX
	// statement — same shape as requirement_attachments / code_review_attachments.
	// It is what makes a re-upload of the same bytes reuse the same row
	// instead of piling up duplicates.
	if !strings.Contains(mysql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_uniq_attach_xreq_sha") {
		t.Error("mysql schema missing the cross_requirement_attachments unique index")
	}
	if !regexp.MustCompile(`ON\s+cross_requirement_attachments\s*\(\s*cross_requirement_id\s*,\s*sha256\s*\)`).MatchString(mysql) {
		t.Error("cross_requirement_attachments lost its (owner, sha256) uniqueness")
	}
	// Column-for-column the same set as code_review_attachments — the two
	// tables are driven by the same service, so a missing column is a bug
	// waiting to fire on the first upload.
	for _, col := range []string{
		"id", "cross_requirement_id", "filename", "stored_path", "mime_type",
		"size_bytes", "sha256", "description", "manifest_text", "uploaded_by",
		"uploaded_at",
	} {
		if !regexp.MustCompile(`(?m)^\s*` + col + `\s`).MatchString(ddl) {
			t.Errorf("cross_requirement_attachments DDL missing the %q column:\n%s", col, ddl)
		}
	}
}

// TestCrossRequirementTrashColumn pins the recycle-bin column.
//
// The cross-project requirement's delete is two-step (move to the trash →
// purge), so the row needs a "deleted_at" stamp that is **orthogonal to the
// lifecycle status**: status says where the work is (draft → … → done /
// archived), deleted_at says whether the row still shows up in the list.
//
// Two things must hold:
//   - the column is in the canonical DDL (fresh databases get it), and
//   - the ALTER exists (older databases get it) — the two are complementary,
//     not alternatives.
//
// It is DATETIME rather than TEXT so that MySQL can index it without the
// mysqlIndexedCol rewrite (which would turn it into VARCHAR(64)); a
// timestamp fits in 64 chars, but the index on it is not even required for
// correctness.
func TestCrossRequirementTrashColumn(t *testing.T) {
	if !strings.Contains(canonicalSchema, "deleted_at          DATETIME") {
		t.Error("canonical schema missing cross_requirements.deleted_at")
	}
	found := false
	for _, stmt := range alterColumns {
		if strings.Contains(stmt, "ALTER TABLE cross_requirements ADD COLUMN deleted_at") {
			found = true
			break
		}
	}
	if !found {
		t.Error("alterColumns does not add cross_requirements.deleted_at")
	}
	// Postgres renames DATETIME → TIMESTAMP; the column must survive.
	pg := fixupSchema(Postgres, canonicalSchema)
	if !strings.Contains(pg, "deleted_at          TIMESTAMP") {
		t.Error("postgres schema missing cross_requirements.deleted_at")
	}
}
