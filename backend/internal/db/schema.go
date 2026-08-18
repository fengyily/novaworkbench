package db

import (
	"fmt"
	"regexp"
	"strings"
)

// canonicalSchema is the single source of truth for the database schema,
// written in SQLite-flavored DDL. Dialect fixups (see fixupSchema) translate
// it for MySQL / PostgreSQL at migration time — keep this block portable:
// no driver-specific functions, only TEXT/INTEGER/DATETIME column types.
const canonicalSchema = `
CREATE TABLE IF NOT EXISTS projects (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	local_path TEXT NOT NULL UNIQUE,
	remote_url TEXT DEFAULT '',
	status TEXT DEFAULT 'active',
	default_branch TEXT DEFAULT 'main',
	project_type TEXT DEFAULT '',
	claude_files TEXT DEFAULT '{}',
	added_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	last_scanned_at DATETIME,
	description TEXT NOT NULL DEFAULT '',
	description_manual INTEGER NOT NULL DEFAULT 0,
	description_hash TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS memories (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'business_context',
	title TEXT DEFAULT '',
	content TEXT NOT NULL,
	source TEXT DEFAULT 'user_input',
	file_path TEXT DEFAULT '',
	tags TEXT DEFAULT '[]',
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	valid_until DATETIME,
	FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS requirements (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL,
	title TEXT NOT NULL,
	description TEXT DEFAULT '',
	status TEXT DEFAULT 'draft',
	priority TEXT DEFAULT 'medium',
	acceptance_criteria TEXT DEFAULT '[]',
	design_docs TEXT DEFAULT '[]',
	conversation_ids TEXT DEFAULT '[]',
	assigned_to TEXT DEFAULT '',
	sprint TEXT DEFAULT '',
	created_by TEXT DEFAULT 'user',
	branch_name TEXT NOT NULL DEFAULT '',
	worktree_path TEXT NOT NULL DEFAULT '',
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	completed_at DATETIME,
	FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS knowledge (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL,
	title TEXT NOT NULL,
	content TEXT NOT NULL,
	category TEXT DEFAULT '',
	source_type TEXT DEFAULT 'user_defined',
	source_ref TEXT DEFAULT '',
	is_reviewed INTEGER DEFAULT 0,
	is_approved INTEGER DEFAULT 1,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS conversations (
	id TEXT PRIMARY KEY,
	project_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	role TEXT NOT NULL,
	content TEXT NOT NULL,
	context_snapshot TEXT DEFAULT '',
	tokens_used INTEGER DEFAULT 0,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_memories_project ON memories(project_id);
CREATE INDEX IF NOT EXISTS idx_memories_type ON memories(type);
CREATE INDEX IF NOT EXISTS idx_req_project ON requirements(project_id);
CREATE INDEX IF NOT EXISTS idx_req_status ON requirements(status);
CREATE INDEX IF NOT EXISTS idx_knowledge_project ON knowledge(project_id);
CREATE INDEX IF NOT EXISTS idx_conv_project ON conversations(project_id);
CREATE INDEX IF NOT EXISTS idx_conv_session ON conversations(session_id);

CREATE TABLE IF NOT EXISTS refinement_chats (
	requirement_id TEXT PRIMARY KEY,
	messages TEXT DEFAULT '[]',
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (requirement_id) REFERENCES requirements(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS project_run_configs (
	id           TEXT PRIMARY KEY,
	project_id   TEXT NOT NULL,
	name         TEXT NOT NULL DEFAULT 'default',
	work_dir     TEXT NOT NULL DEFAULT '',
	compose_file TEXT NOT NULL DEFAULT 'docker-compose.yml',
	extra_args   TEXT NOT NULL DEFAULT '[]',
	env_overrides TEXT NOT NULL DEFAULT '{}',
	is_default   INTEGER NOT NULL DEFAULT 1,
	created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_run_cfg_project ON project_run_configs(project_id);

CREATE TABLE IF NOT EXISTS platform_tokens (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	platform   TEXT NOT NULL,
	base_url   TEXT NOT NULL DEFAULT '',
	token      TEXT NOT NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS roles (
	id            TEXT PRIMARY KEY,
	key           TEXT NOT NULL UNIQUE,
	name          TEXT NOT NULL,
	description   TEXT DEFAULT '',
	system_prompt TEXT NOT NULL DEFAULT '',
	model         TEXT NOT NULL DEFAULT '',
	sort_order    INTEGER NOT NULL DEFAULT 0,
	enabled       INTEGER NOT NULL DEFAULT 1,
	created_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at    DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL DEFAULT '',
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS claude_configs (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL DEFAULT '',
	base_url      TEXT NOT NULL DEFAULT '',
	auth_token    TEXT NOT NULL DEFAULT '',
	models        TEXT NOT NULL DEFAULT '[]',
	default_model TEXT NOT NULL DEFAULT '',
	is_active     INTEGER NOT NULL DEFAULT 0,
	created_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at    DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS weekly_reports (
	id           TEXT PRIMARY KEY,
	project_id   TEXT NOT NULL,
	period_start TEXT NOT NULL,
	period_end   TEXT NOT NULL,
	rule         TEXT NOT NULL DEFAULT '',
	content      TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL DEFAULT 'done',
	created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_weekly_reports_project ON weekly_reports(project_id);

CREATE TABLE IF NOT EXISTS job_logs (
	job_id          TEXT PRIMARY KEY,
	requirement_id  TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL DEFAULT '',
	exit_code       INTEGER NOT NULL DEFAULT 0,
	started_at      TEXT NOT NULL DEFAULT '',
	finished_at     TEXT NOT NULL DEFAULT '',
	log             TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_job_logs_req ON job_logs(requirement_id);
`

// alterColumns adds columns to older databases. ALTER TABLE fails when the
// column already exists — migrate ignores the dialect-specific "duplicate
// column" error.
var alterColumns = []string{
	`ALTER TABLE projects ADD COLUMN platform_type TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE projects ADD COLUMN platform_token_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN analysis_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN design_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN design_job_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN coding_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN analysis_job_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN apply_job_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN skip_analysis INTEGER NOT NULL DEFAULT 1`,
	// Git worktree isolation for parallel development: each requirement's dev
	// branch + worktree path so adjust-coding and the merge step can locate the
	// isolated working tree instead of the shared project checkout.
	`ALTER TABLE requirements ADD COLUMN branch_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN worktree_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE weekly_reports ADD COLUMN git_branch TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE weekly_reports ADD COLUMN git_author TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE projects ADD COLUMN deleted_at TEXT`,
	`ALTER TABLE projects ADD COLUMN deleted_dir INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE projects ADD COLUMN description TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE projects ADD COLUMN description_manual INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE projects ADD COLUMN description_hash TEXT NOT NULL DEFAULT ''`,
	// Per-stage execution model: the effective model name (--model value or
	// CLI default) actually dispatched to the claude CLI for each wizard stage.
	// Written only on the success path so a failed run never clobbers the last
	// good record. Empty = stage not yet run (or run before this column
	// existed); the UI hides the badge for empty values.
	`ALTER TABLE requirements ADD COLUMN analyst_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN architect_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN developer_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN reviewer_model TEXT NOT NULL DEFAULT ''`,
	// Review jobs are project-level (not bound to a requirement), so the review
	// model is persisted on the durable job log instead of the requirement row.
	`ALTER TABLE job_logs ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
}

var (
	// MySQL cannot index TEXT columns (needs a key length), so every column
	// that is a PK/UNIQUE/index target becomes a VARCHAR. Column definitions
	// all start at the beginning of a line in the canonical schema.
	mysqlIndexedCol   = regexp.MustCompile(`(?m)^(\s*)(id|job_id|project_id|requirement_id|session_id|type|status)(\s+)TEXT\b`)
	mysqlLocalPathCol = regexp.MustCompile(`(?m)^(\s*)local_path(\s+)TEXT\b`)
	// `key` is a reserved word in MySQL — quote it in DDL. (Queries go
	// through DB.Ident.)
	mysqlKeyCol = regexp.MustCompile(`(?m)^(\s*)key(\s+)TEXT\b`)
	// MySQL forbids plain literal defaults on TEXT; the expression form
	// DEFAULT ('...') is allowed since 8.0.13.
	mysqlTextDefault = regexp.MustCompile(`\bTEXT(\s+NOT NULL)?\s+DEFAULT\s+('[^']*')`)
)

// fixupSchema translates the canonical SQLite-flavored DDL for the dialect.
func fixupSchema(dialect Dialect, schema string) string {
	switch dialect {
	case Postgres:
		return strings.ReplaceAll(schema, "DATETIME", "TIMESTAMP")
	case MySQL:
		s := mysqlIndexedCol.ReplaceAllString(schema, "${1}${2}${3}VARCHAR(64)")
		s = mysqlLocalPathCol.ReplaceAllString(s, "${1}local_path${2}VARCHAR(512)")
		s = mysqlKeyCol.ReplaceAllString(s, "${1}`key`${2}VARCHAR(191)")
		s = mysqlTextDefault.ReplaceAllString(s, "TEXT$1 DEFAULT ($2)")
		// MySQL has no CREATE INDEX IF NOT EXISTS — drop the clause and rely
		// on ignoring "Duplicate key name" (1061) on subsequent boots.
		s = strings.ReplaceAll(s, "CREATE INDEX IF NOT EXISTS", "CREATE INDEX")
		return s
	default:
		return schema
	}
}

// isIgnorableDDLError reports whether err is the expected "already exists"
// noise from re-running idempotent DDL on each boot.
func isIgnorableDDLError(stmt string, err error) bool {
	msg := strings.ToLower(err.Error())
	upper := strings.ToUpper(strings.TrimSpace(stmt))
	if strings.HasPrefix(upper, "ALTER TABLE") {
		// sqlite: "duplicate column name: x"; mysql: "Duplicate column name";
		// postgres: `column "x" of relation "y" already exists`
		return strings.Contains(msg, "duplicate column") || strings.Contains(msg, "already exists")
	}
	if strings.HasPrefix(upper, "CREATE INDEX") {
		// MySQL has no CREATE INDEX IF NOT EXISTS — "Duplicate key name" (1061).
		return strings.Contains(msg, "duplicate key name")
	}
	return false
}

func execStatements(d *DB, stmts []string) error {
	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := d.Exec(stmt); err != nil {
			if isIgnorableDDLError(stmt, err) {
				continue
			}
			preview := stmt
			if len(preview) > 80 {
				preview = preview[:80] + "..."
			}
			return fmt.Errorf("%w\n  statement: %s", err, preview)
		}
	}
	return nil
}

// migrate creates the schema (idempotently) and applies ad-hoc column
// additions + data migrations.
func migrate(d *DB) error {
	schema := fixupSchema(d.dialect, canonicalSchema)
	if err := execStatements(d, strings.Split(schema, ";")); err != nil {
		return err
	}

	alters := make([]string, len(alterColumns))
	copy(alters, alterColumns)
	if d.dialect == MySQL {
		for i, stmt := range alters {
			alters[i] = mysqlTextDefault.ReplaceAllString(stmt, "TEXT$1 DEFAULT ($2)")
		}
	}
	if err := execStatements(d, alters); err != nil {
		return err
	}

	// Data migration: the "analyzed" status was removed from the lifecycle
	// (the analyst-complete finalization step was deleted). Map leftover
	// "analyzed" rows to "analyzing". Idempotent: a no-op after the first run.
	if _, err := d.Exec("UPDATE requirements SET status='analyzing' WHERE status='analyzed'"); err != nil {
		return err
	}

	return nil
}
