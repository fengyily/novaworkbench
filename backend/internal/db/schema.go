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
	kind TEXT NOT NULL DEFAULT 'requirement',
	acceptance_criteria TEXT DEFAULT '[]',
	design_docs TEXT DEFAULT '[]',
	conversation_ids TEXT DEFAULT '[]',
	assigned_to TEXT DEFAULT '',
	created_by TEXT DEFAULT 'user',
	source_requirement_id TEXT NOT NULL DEFAULT '',
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

CREATE TABLE IF NOT EXISTS coding_chats (
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
	finished_at    TEXT NOT NULL DEFAULT '',
	log             TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_job_logs_req ON job_logs(requirement_id);

CREATE TABLE IF NOT EXISTS token_usage (
	id                      TEXT PRIMARY KEY,
	requirement_id          TEXT,
	project_id              TEXT NOT NULL,
	job_id                  TEXT NOT NULL DEFAULT '',
	step                    TEXT NOT NULL,
	model                   TEXT NOT NULL DEFAULT '',
	input_tokens            INTEGER NOT NULL DEFAULT 0,
	output_tokens           INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens   INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens       INTEGER NOT NULL DEFAULT 0,
	meta                    TEXT NOT NULL DEFAULT '',
	created_at              DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_usage_req    ON token_usage(requirement_id);
CREATE INDEX IF NOT EXISTS idx_usage_proj   ON token_usage(project_id);
CREATE INDEX IF NOT EXISTS idx_usage_created ON token_usage(created_at);

-- User-Role-Permission (RBAC) tables. NOTE: the existing roles table above
-- stores AI personas (analyst/architect/developer/reviewer) -- it is a DIFFERENT
-- concept from the RBAC role definitions here, which live in acl_roles to
-- avoid table-name / UI confusion.

CREATE TABLE IF NOT EXISTS users (
	id             TEXT PRIMARY KEY,
	username       TEXT NOT NULL UNIQUE,
	display_name   TEXT NOT NULL DEFAULT '',
	password_hash  TEXT NOT NULL DEFAULT '',
	status         TEXT NOT NULL DEFAULT 'active',
	is_admin       INTEGER NOT NULL DEFAULT 0,
	last_login_at  DATETIME,
	created_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at     DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_users_username ON users(username);

CREATE TABLE IF NOT EXISTS acl_roles (
	id          TEXT PRIMARY KEY,
	key         TEXT NOT NULL UNIQUE,
	name        TEXT NOT NULL,
	description TEXT DEFAULT '',
	is_builtin  INTEGER NOT NULL DEFAULT 0,
	sort_order  INTEGER NOT NULL DEFAULT 0,
	enabled     INTEGER NOT NULL DEFAULT 1,
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS permissions (
	id          TEXT PRIMARY KEY,
	key         TEXT NOT NULL UNIQUE,
	name        TEXT NOT NULL,
	module      TEXT NOT NULL DEFAULT '',
	description TEXT DEFAULT '',
	sort_order  INTEGER NOT NULL DEFAULT 0,
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS acl_role_permissions (
	role_id       TEXT NOT NULL,
	permission_id TEXT NOT NULL,
	PRIMARY KEY (role_id, permission_id),
	FOREIGN KEY (role_id) REFERENCES acl_roles(id) ON DELETE CASCADE,
	FOREIGN KEY (permission_id) REFERENCES permissions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS acl_user_roles (
	user_id    TEXT NOT NULL,
	role_id    TEXT NOT NULL,
	PRIMARY KEY (user_id, role_id),
	FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
	FOREIGN KEY (role_id) REFERENCES acl_roles(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS user_projects (
	user_id    TEXT NOT NULL,
	project_id TEXT NOT NULL,
	assigned_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_id, project_id),
	FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
	FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_user_projects_user ON user_projects(user_id);

CREATE TABLE IF NOT EXISTS sessions (
	token       TEXT PRIMARY KEY,
	user_id     TEXT NOT NULL,
	expires_at  DATETIME NOT NULL,
	created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);

CREATE TABLE IF NOT EXISTS skills (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	slug        TEXT NOT NULL UNIQUE,
	content     TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	source_url  TEXT NOT NULL DEFAULT '',
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Agent servers: remote Linux/macOS execution targets. The credential
-- (auth_value) is stored as an AES-256-GCM ciphertext (base64, nonce embedded)
-- by internal/secret and never returned in API responses. status is updated
-- by the Check goroutine and check_result holds the last human-readable summary.
-- Schema added 2026-09 by Agent-Server feature.
CREATE TABLE IF NOT EXISTS agent_servers (
	id              TEXT PRIMARY KEY,
	name            TEXT NOT NULL,
	host            TEXT NOT NULL,
	port            INTEGER NOT NULL DEFAULT 22,
	username        TEXT NOT NULL DEFAULT 'root',
	auth_type       TEXT NOT NULL DEFAULT 'key',
	auth_value      TEXT NOT NULL DEFAULT '',
	auth_value_algo TEXT NOT NULL DEFAULT 'aes-gcm',
	status          TEXT NOT NULL DEFAULT 'unknown',
	last_check_at   DATETIME,
	check_result    TEXT NOT NULL DEFAULT '',
	created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_agent_servers_status ON agent_servers(status);

-- Sub-task: a manually-triggered child agent under a requirement's developing
-- stage. Each sub-task forks the requirement's coding_session_id (or
-- design_session_id as fallback) so every child agent shares the main agent's
-- context — project files, design docs, prior conversation — but runs in its
-- own claude process with its own session id. Artifact holds the final
-- Markdown report the child agent produced, persisted on completion so the
-- history survives JobStore ring-buffer eviction and server restarts.
-- Lifecycle: pending → running → done | error. Status mirrors JobStore job
-- status so the UI can show the same spinner / error chip pattern.
CREATE TABLE IF NOT EXISTS sub_tasks (
	id                 TEXT PRIMARY KEY,
	requirement_id     TEXT NOT NULL,
	title              TEXT NOT NULL DEFAULT '',
	prompt             TEXT NOT NULL DEFAULT '',
	status             TEXT NOT NULL DEFAULT 'pending',
	session_id         TEXT NOT NULL DEFAULT '',
	source_session_id  TEXT NOT NULL DEFAULT '',
	job_id             TEXT NOT NULL DEFAULT '',
	artifact           TEXT NOT NULL DEFAULT '',
	model              TEXT NOT NULL DEFAULT '',
	input_tokens       INTEGER NOT NULL DEFAULT 0,
	output_tokens      INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
	cost_cents         INTEGER NOT NULL DEFAULT 0,
	duration_seconds   INTEGER NOT NULL DEFAULT 0,
	created_at         DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at         DATETIME DEFAULT CURRENT_TIMESTAMP,
	completed_at       DATETIME,
	FOREIGN KEY (requirement_id) REFERENCES requirements(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_sub_tasks_req ON sub_tasks(requirement_id);
`

// alterColumns adds columns to older databases. ALTER TABLE fails when the
// column already exists — migrate ignores the dialect-specific "duplicate
// column" error.
var alterColumns = []string{
	`ALTER TABLE projects ADD COLUMN platform_type TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE projects ADD COLUMN platform_token_id TEXT NOT NULL DEFAULT ''`,
	// install_job_id: persisted JobStore job id for the running install on an
	// agent server, so a page refresh can reconnect to its SSE stream and
	// replay history (same pattern as requirements.analysis_job_id / apply_job_id).
	// Cleared by runInstall's defer on Finish so a stale id never lingers
	// after the job is gone (matters because JobStore is in-memory and can
	// evict the job on backend restart while the DB column still holds it).
	`ALTER TABLE agent_servers ADD COLUMN install_job_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN analysis_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN design_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN design_job_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN coding_session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN analysis_job_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN apply_job_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN skip_analysis INTEGER NOT NULL DEFAULT 1`,
	// skip_design: "直接开发" — when true, the requirement skips the analyst AND
	// architect stages entirely and goes straight to coding (draft → developing).
	// Default 0 because the full pipeline stays the default flow.
	`ALTER TABLE requirements ADD COLUMN skip_design INTEGER NOT NULL DEFAULT 0`,
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
	// Model pricing is bound to each Claude config (platform): the models
	// column carries per-model input/output unit prices, and currency is the
	// accounting unit for that platform (USD/CNY; empty = not yet set).
	`ALTER TABLE claude_configs ADD COLUMN currency TEXT NOT NULL DEFAULT ''`,
	// Cost attribution: every token_usage row records which config (platform)
	// served the request and a currency snapshot, so cost can be recomputed
	// from that config's (possibly later-edited) unit prices. The price itself
	// is NOT snapshotted — edits take effect retroactively ("设置后生效").
	`ALTER TABLE token_usage ADD COLUMN claude_config_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE token_usage ADD COLUMN currency TEXT NOT NULL DEFAULT ''`,
	// Git commit identity bound to each platform token (so Docker runs without
	// a mounted ~/.gitconfig can still commit as the right account). Empty =
	// fall back to git's normal config lookup (host ~/.gitconfig etc.).
	`ALTER TABLE platform_tokens ADD COLUMN git_user_name  TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE platform_tokens ADD COLUMN git_user_email TEXT NOT NULL DEFAULT ''`,
	// Requirement kind: broadens "需求" into three top-level categories — issue
	// (a defect/bug report), requirement (a planned feature, the legacy default),
	// idea (an exploratory note). The wizard uses it to inject kind-specific
	// prompt context blocks (see internal/prompt/kind_blocks.go) and the
	// frontend uses it to drive UX affordances (badge, CTA visibility). Default
	// 'requirement' keeps historical rows on the legacy flow without any data
	// backfill. Validated in service.RequirementService; no CHECK constraint so
	// the column behaves like the existing status/priority TEXT columns.
	`ALTER TABLE requirements ADD COLUMN kind TEXT NOT NULL DEFAULT 'requirement'`,
	// Source traceability for promoted / split-off requirements. Set when an
	// idea (or another kind) is summarized into a brand-new requirement via the
	// "总结转需求" action. The original row keeps its own kind (so discussions
	// aren't mutated); only the new requirement carries this pointer. Empty =
	// no parent requirement.
	`ALTER TABLE requirements ADD COLUMN source_requirement_id TEXT NOT NULL DEFAULT ''`,
	// Per-stage context compression (analyst / architect / coding). When the
	// user clicks "压缩上下文", the wizard handler asks Claude to summarize the
	// current session via --resume and stores the result here, then clears the
	// matching *_session_id so the next turn starts a fresh session with the
	// summary prepended to its first prompt. compressed_at lets the UI show
	// "已压缩 N 分钟前" without recomputing from the summary text. Empty string
	// / NULL = stage not yet compressed.
	`ALTER TABLE requirements ADD COLUMN analyst_context_summary TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN analyst_compressed_at   DATETIME`,
	`ALTER TABLE requirements ADD COLUMN design_context_summary  TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN design_compressed_at    DATETIME`,
	`ALTER TABLE requirements ADD COLUMN coding_context_summary  TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE requirements ADD COLUMN coding_compressed_at    DATETIME`,
	// Session-level context-usage snapshots (analyst / design / coding). The
	// wizard's runClaudeStream writes a JSON blob here at the end of every
	// claude turn (same point it emits the `usage` SSE event), keyed by session:
	// {"analyst_chat":{...},"architect_design":{...},"coding":{...}}. The
	// frontend seeds its usage bars from this on load so the bar survives page
	// refresh / panel collapse instead of dropping to 0%. One JSON column
	// (read-modify-write in a tx) beats three columns and keeps the live
	// telemetry out of the compression-summary columns above. Empty = no
	// snapshot yet.
	`ALTER TABLE requirements ADD COLUMN usage_snapshots TEXT NOT NULL DEFAULT ''`,
	// Agent server table: covered in canonicalSchema; nothing further to alter.
	// Project ↔ Claude session slug: claude CLI stores per-session JSONL under
	// $NOVA_CLAUDE_HOME/projects/<slug>/, where <slug> is an encoding of the
	// working directory path. The remote-coding path (handler/wizard.go
	// runRemoteCoding) needs the same slug on the remote server so SFTP
	// session sync lands in the right directory and `--resume <session_id>`
	// finds the jsonl. Cached here on first local execution by scanning the
	// local projects dir; empty = not yet discovered.
	`ALTER TABLE projects ADD COLUMN claude_project_slug TEXT NOT NULL DEFAULT ''`,
	// coding_plan: the developer main-agent's "task breakdown" Markdown,
	// produced on the start-coding turn and refreshed whenever the user asks
	// the main agent to re-plan. Empty = main agent hasn't emitted one yet, or
	// the user is using the developer stage in the legacy single-process mode.
	// Surfaced by the SubTaskPanel as the parent plan every child task forks
	// from; persisted on completion so a server restart / JobStore eviction
	// doesn't lose the breakdown.
	`ALTER TABLE requirements ADD COLUMN coding_plan TEXT NOT NULL DEFAULT ''`,
	// Per-sub-task token usage (mirrors token_usage per-row columns but stays
	// inline so a child agent's cost lives next to its artifact without a
	// second SELECT against token_usage). input_tokens / output_tokens are the
	// terminal result-event counts; cache_* tokens help the UI display the
	// "缓存命中率" badge alongside the cost. cost_cents is the resolved cost
	// (config's per-model unit price × tokens, same formula the dashboard
	// uses), written best-effort on completion — empty when the model has no
	// pricing configured yet. duration_seconds records wall-clock from
	// MarkRunning to Finish so the SubTaskCard header can show "耗时 2m15s".
	`ALTER TABLE sub_tasks ADD COLUMN input_tokens         INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sub_tasks ADD COLUMN output_tokens        INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sub_tasks ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sub_tasks ADD COLUMN cache_read_tokens     INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sub_tasks ADD COLUMN cost_cents            INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE sub_tasks ADD COLUMN duration_seconds      INTEGER NOT NULL DEFAULT 0`,
}

var (
	// MySQL cannot index TEXT columns (needs a key length), so every column
	// that is a PK/UNIQUE/index target becomes a VARCHAR. Column definitions
	// all start at the beginning of a line in the canonical schema.
	mysqlIndexedCol   = regexp.MustCompile(`(?m)^(\s*)(id|job_id|project_id|requirement_id|session_id|type|status|username|role_id|permission_id|user_id|token)(\s+)TEXT\b`)
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

	// alterColumns are SQLite-flavored; run them through the same per-dialect
	// fixup as the canonical schema so DATETIME→TIMESTAMP on Postgres and the
	// MySQL TEXT-default / VARCHAR translations apply here too. (The MySQL
	// block below used to do only the TEXT-default piece; fixupSchema is a
	// superset and also covers the indexed-column / `key` regexes, which are
	// harmless no-matches on these statements.)
	alters := make([]string, len(alterColumns))
	for i, stmt := range alterColumns {
		alters[i] = fixupSchema(d.dialect, stmt)
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

	// Data migration: upgrade the developer role's default system_prompt to
	// the "统筹协调" framing introduced when sub-task collaboration shipped.
	// SeedDefaults is per-key upsert, so a role_developer row written by an
	// older build keeps its original prompt forever. We match the old prompt
	// by a stable substring (the first user-visible sentence is unique enough
	// for a fingerprint) and replace it with the new framing. Idempotent:
	// re-running after the upgrade is a no-op because the substring won't
	// match anymore.
	//
	// Identifier quoting: `key` is reserved in MySQL and a quoted identifier
	// in Postgres/SQLite. db.Ident handles all three dialects — never use
	// raw backticks here (Postgres rejects them with "syntax error at or
	// near `=`" on the line that follows the SET clause).
	if _, err := d.Exec(`UPDATE roles SET system_prompt = ?
		WHERE `+d.Ident("key")+` = 'developer'
		  AND system_prompt LIKE ?
		  AND system_prompt NOT LIKE ?`,
		`你是一位资深软件工程师，担任本需求的开发**统筹协调者**。

工作方式：
- 先读取项目中的相关文件，理解现有代码结构与已确定的技术方案。
- **不要直接编写项目代码**——所有具体实现工作由子Agent完成。
- 分析当前情况后，给出可执行的子任务分解清单（每条包含：标题 + 具体提示词），方便用户据此创建子任务。
- 若用户已经在子任务中执行了某些工作，请基于子任务产物评估进度，并提示下一步建议的子任务。
- 输出 Markdown 任务分解表，便于用户复制粘贴。
- 用中文沟通。
- 最后务必包含一段「等待用户创建子任务」的明确提示。`,
		"%正在实现一个需求%",
		"%统筹协调%",
	); err != nil {
		return err
	}

	return nil
}
