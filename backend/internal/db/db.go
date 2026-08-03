package db

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

func Init(dataPath string) (*sql.DB, error) {
	// Expand ~ in path
	if strings.HasPrefix(dataPath, "~") {
		home, _ := os.UserHomeDir()
		dataPath = filepath.Join(home, dataPath[1:])
	}

	dir := filepath.Dir(dataPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dataPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1) // SQLite single-writer
	db.SetMaxIdleConns(1)

	if err := migrate(db); err != nil {
		return nil, err
	}

	log.Printf("Database initialized: %s", dataPath)
	return db, nil
}

func migrate(db *sql.DB) error {
	schema := `
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
		last_scanned_at DATETIME
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

	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// Add columns to projects that may not exist on older DBs.
	// ALTER TABLE fails with "duplicate column name" if the column already exists — ignore that error.
	alterCols := []string{
		`ALTER TABLE projects ADD COLUMN platform_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN platform_token_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requirements ADD COLUMN analysis_session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requirements ADD COLUMN design_session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requirements ADD COLUMN design_job_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requirements ADD COLUMN coding_session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requirements ADD COLUMN analysis_job_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE weekly_reports ADD COLUMN git_branch TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE weekly_reports ADD COLUMN git_author TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN deleted_at TEXT`,
		`ALTER TABLE projects ADD COLUMN deleted_dir INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range alterCols {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}

	// Data migration: the "analyzed" status was removed from the lifecycle (the
	// analyst-complete finalization step was deleted). Map any存量 "analyzed" rows
	// to "analyzing" — the new model treats the analyst chat as happening during
	// "analyzing", and the user proceeds directly to architect-design. Idempotent:
	// after the first run no rows match, so it's a no-op.
	if _, err := db.Exec("UPDATE requirements SET status='analyzing' WHERE status='analyzed'"); err != nil {
		return err
	}

	return nil
}
