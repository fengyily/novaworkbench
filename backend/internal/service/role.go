package service

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

type RoleService struct{ db *db.DB }

func NewRoleService(db *db.DB) *RoleService { return &RoleService{db: db} }

// roleColumns is the SELECT column list for roles, with `key` quoted for the
// active dialect (reserved word in MySQL).
func (s *RoleService) roleColumns() string {
	return "id, " + s.db.Ident("key") + ", name, description, system_prompt, model, claude_config_id, sort_order, enabled, created_at, updated_at"
}

// SeedDefaults inserts any built-in role whose key is not yet present.
// Idempotent and per-key (not whole-table), so roles added in later releases
// (e.g. reviewer) are backfilled into existing databases on startup.
//
// IMPORTANT: SeedDefaults does NOT update an existing role's system_prompt.
// Once a user has customized a role (or once we shipped a previous version of
// the built-in prompt), SeedDefaults leaves it alone. Role prompt upgrades
// that MUST reach existing databases — i.e. changes that flip the role's
// behaviour rather than just clarify wording — go through MigrateDeveloperRole
// (or a similarly-named release-specific migrator) instead, which knows how
// to identify "the old default" vs "the user customized this" and only rewrites
// the former.
func (s *RoleService) SeedDefaults() error {
	now := time.Now()
	for _, r := range DefaultRoles() {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM roles WHERE "+s.db.Ident("key")+" = ?", r.Key).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		// claude_config_id is left empty so a freshly-seeded role inherits the
		// global active config (matches pre-binding behavior). The user can
		// attach a specific config from Settings → Roles.
		if _, err := s.db.Exec(
			`INSERT INTO roles
			(`+s.roleColumns()+`)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Key, r.Name, r.Description, r.SystemPrompt, r.Model, r.ClaudeConfigID, r.SortOrder, r.Enabled, now, now,
		); err != nil {
			return fmt.Errorf("seed role %s: %w", r.Key, err)
		}
	}
	return nil
}

// developerOldPromptSignature is the substring we look for in the existing
// developer role's system_prompt to decide whether the row still carries the
// pre-sub-task-collaboration persona ("执行者") vs the current
// ("统筹协调者") persona. SeedDefaults already leaves the row alone — but the
// flip from "执行者" to "统筹协调者" is a behavioural change (Claude stops
// writing code and starts emitting a task breakdown), so on upgrade we must
// rewrite the existing row when it still carries the old persona.
//
// The substring is intentionally a stable, distinctive phrase from the
// original developer prompt ("实现需求中描述的功能") — not just "developer" or
// a generic marker — so a user who has genuinely customized the prompt is
// NOT clobbered. If the substring is absent (because the user already
// customized, OR because a previous release already migrated them), we skip.
const developerOldPromptSignature = "实现需求中描述的功能"

// developerNewPromptSignature is the substring that uniquely identifies the
// current "统筹协调者" persona. Used by MigrateDeveloperRole to short-circuit
// when the row already carries the new persona (so the migration is safe to
// call on every boot without re-applying).
const developerNewPromptSignature = "统筹**协调者**"

// MigrateDeveloperRole brings the developer role's built-in system_prompt
// forward to the "统筹协调者" persona for databases whose developer role
// still carries the legacy "执行者" prompt. Idempotent:
//   - row missing (fresh DB): no-op, SeedDefaults already inserted the new prompt
//   - row present + old signature: UPDATE system_prompt to the new default,
//     leave name / model / model / enabled untouched
//   - row present + new signature: no-op (already migrated)
//   - row present + neither signature: user customized; we can't tell old vs
//     custom, so NO-OP. The user can hit the settings page → role → Reset to
//     pick up the new built-in.
//
// Returns (migrated bool, err) so callers can log "developer role upgraded"
// at startup without doing the substring scan themselves.
func (s *RoleService) MigrateDeveloperRole() (bool, error) {
	r, err := s.GetByKey("developer")
	if err != nil {
		// No row yet — SeedDefaults handles the fresh-DB path; nothing to migrate.
		return false, nil
	}
	prompt := r.SystemPrompt
	if strings.Contains(prompt, developerNewPromptSignature) {
		return false, nil
	}
	if !strings.Contains(prompt, developerOldPromptSignature) {
		// User-customized prompt that we don't recognize; leave it alone so the
		// user can keep their wording. The settings UI's reset button is the
		// supported way to opt into the new built-in.
		return false, nil
	}
	newPrompt := developerMigratedPrompt()
	if _, err := s.db.Exec("UPDATE roles SET system_prompt=?, updated_at=? WHERE id=?",
		newPrompt, time.Now(), r.ID); err != nil {
		return false, fmt.Errorf("migrate developer role: %w", err)
	}
	return true, nil
}

// developerV2PromptSignature is the distinctive phrase of the coordinator
// prompt generation that pre-dates the Write-tool channel: it tells the
// agent to paste a machine-readable JSON block into its reply text. That
// text convention broke in production (embedded code fences truncate the
// JSON — req_9d24ef181a5ad5c4), so v3 makes the Write tool the primary
// channel instead.
const developerV2PromptSignature = "在 Markdown 之后立即输出一个机器可读的 JSON 块"

// developerWriteChannelSignature identifies the current prompt generation:
// the agent is told to Write its decomposition to this well-known path and
// the backend captures the structured tool_use input from the event stream.
const developerWriteChannelSignature = ".novaworkbench/subtasks.json"

// MigrateDeveloperRoleWriteChannel upgrades the developer role from the v2
// "统筹协调者 + 文本 JSON 块" prompt to the v3 "统筹协调者 + Write 工具写
// subtasks.json" prompt. Idempotent; no-op for fresh DBs (SeedDefaults
// already inserted v3), pre-coordinator rows (MigrateDeveloperRole's job),
// and prompts we don't recognize (user-customized — the settings page reset
// button is their opt-in).
func (s *RoleService) MigrateDeveloperRoleWriteChannel() (bool, error) {
	r, err := s.GetByKey("developer")
	if err != nil {
		return false, nil
	}
	prompt := r.SystemPrompt
	if strings.Contains(prompt, developerWriteChannelSignature) {
		return false, nil // already on v3
	}
	if !strings.Contains(prompt, developerNewPromptSignature) ||
		!strings.Contains(prompt, developerV2PromptSignature) {
		return false, nil // not the v2 default — leave it alone
	}
	if _, err := s.db.Exec("UPDATE roles SET system_prompt=?, updated_at=? WHERE id=?",
		developerMigratedPrompt(), time.Now(), r.ID); err != nil {
		return false, fmt.Errorf("migrate developer role (write channel): %w", err)
	}
	return true, nil
}

// developerMigratedPrompt returns the "统筹协调者" system_prompt for the
// developer role. Pulled out so MigrateDeveloperRole + Reset can call the same
// source — adding more lines in the future keeps them in sync without a
// second copy-paste.
func developerMigratedPrompt() string {
	for _, d := range DefaultRoles() {
		if d.Key == "developer" {
			return d.SystemPrompt
		}
	}
	return ""
}

// executorOldPromptSignature is the substring we look for in the existing
// executor role's system_prompt to decide whether the row still carries the
// pre-"直接落地实现" persona (the short "严禁拆任务" version) vs the current
// "直接落地实现" persona. SeedDefaults already leaves the row alone — but
// the rewrite is a behavioural change (the new prompt drops the explicit
// "严禁再拆任务 / 不要输出 [SUBTASKS_READY]" prohibition from the system
// level and substitutes a fuller engineering checklist), so on upgrade we
// must rewrite the existing row when it still carries the old persona.
//
// The substring is intentionally a stable, distinctive phrase from the
// original executor prompt — not just "executor" or a generic marker —
// so a user who has genuinely customized the prompt is NOT clobbered.
// If the substring is absent (because the user already customized, OR
// because a previous release already migrated them), we skip.
const executorOldPromptSignature = "不要再做任务拆分"

// executorNewPromptSignature is the substring that uniquely identifies the
// current "直接落地实现" persona. Used by MigrateExecutorRole to short-circuit
// when the row already carries the new persona (so the migration is safe to
// call on every boot without re-applying).
const executorNewPromptSignature = "前置调研"

// MigrateExecutorRole brings the executor role's built-in system_prompt
// forward to the "直接落地实现" persona for databases whose executor role
// still carries the legacy "严禁拆任务" prompt. Idempotent:
//   - row missing (fresh DB): no-op, SeedDefaults already inserted the new prompt
//   - row present + old signature: UPDATE system_prompt to the new default,
//     leave name / model / claude_config_id / enabled untouched
//   - row present + new signature: no-op (already migrated)
//   - row present + neither signature: user customized; we can't tell old vs
//     custom, so NO-OP. The user can hit the settings page → role → Reset to
//     pick up the new built-in.
//
// Returns (migrated bool, err) so callers can log "executor role upgraded"
// at startup without doing the substring scan themselves.
func (s *RoleService) MigrateExecutorRole() (bool, error) {
	r, err := s.GetByKey("executor")
	if err != nil {
		// No row yet — SeedDefaults handles the fresh-DB path; nothing to migrate.
		return false, nil
	}
	prompt := r.SystemPrompt
	if strings.Contains(prompt, executorNewPromptSignature) {
		return false, nil
	}
	if !strings.Contains(prompt, executorOldPromptSignature) {
		// User-customized prompt that we don't recognize; leave it alone so the
		// user can keep their wording. The settings UI's reset button is the
		// supported way to opt into the new built-in.
		return false, nil
	}
	newPrompt := executorMigratedPrompt()
	if _, err := s.db.Exec("UPDATE roles SET system_prompt=?, updated_at=? WHERE id=?",
		newPrompt, time.Now(), r.ID); err != nil {
		return false, fmt.Errorf("migrate executor role: %w", err)
	}
	return true, nil
}

// executorMigratedPrompt returns the current "直接落地实现" system_prompt
// for the executor role. Pulled out so MigrateExecutorRole + Reset can call
// the same source — adding more lines in the future keeps them in sync
// without a second copy-paste.
func executorMigratedPrompt() string {
	for _, d := range DefaultRoles() {
		if d.Key == "executor" {
			return d.SystemPrompt
		}
	}
	return ""
}

// architectOldPromptSignature is the substring we look for in the existing
// architect role's system_prompt to decide whether the row still carries the
// pre-模板化 persona (the old short "涵盖：整体实现思路、涉及文件…" version)
// vs the current template-driven "需求/项目上下文/输出要求/工作方式约束"
// persona. SeedDefaults already leaves the row alone — but the rewrite is a
// behavioural change (the new prompt enforces a structured 6-section output
// + 7 working-style rules that the old one didn't), so on upgrade we must
// rewrite the existing row when it still carries the old persona.
//
// The substring is intentionally a stable, distinctive phrase from the
// original architect prompt — not just "architect" or a generic marker —
// so a user who has genuinely customized the prompt is NOT clobbered.
// If the substring is absent (because the user already customized, OR
// because a previous release already migrated them), we skip.
const architectOldPromptSignature = "涵盖：整体实现思路、涉及文件"

// architectNewPromptSignature is the substring that uniquely identifies the
// current template-driven persona. Used by MigrateArchitectRole to
// short-circuit when the row already carries the new persona (so the
// migration is safe to call on every boot without re-applying).
const architectNewPromptSignature = "需求理解复述"

// MigrateArchitectRole brings the architect role's built-in system_prompt
// forward to the template-driven persona for databases whose architect role
// still carries the legacy "涵盖：整体实现思路…" prompt. Idempotent:
//   - row missing (fresh DB): no-op, SeedDefaults already inserted the new prompt
//   - row present + old signature: UPDATE system_prompt to the new default,
//     leave name / model / claude_config_id / enabled untouched
//   - row present + new signature: no-op (already migrated)
//   - row present + neither signature: user customized; we can't tell old vs
//     custom, so NO-OP. The user can hit the settings page → role → Reset to
//     pick up the new built-in.
//
// Returns (migrated bool, err) so callers can log "architect role upgraded"
// at startup without doing the substring scan themselves.
func (s *RoleService) MigrateArchitectRole() (bool, error) {
	r, err := s.GetByKey("architect")
	if err != nil {
		// No row yet — SeedDefaults handles the fresh-DB path; nothing to migrate.
		return false, nil
	}
	prompt := r.SystemPrompt
	if strings.Contains(prompt, architectNewPromptSignature) {
		return false, nil
	}
	if !strings.Contains(prompt, architectOldPromptSignature) {
		// User-customized prompt that we don't recognize; leave it alone so the
		// user can keep their wording. The settings UI's reset button is the
		// supported way to opt into the new built-in.
		return false, nil
	}
	newPrompt := architectMigratedPrompt()
	if _, err := s.db.Exec("UPDATE roles SET system_prompt=?, updated_at=? WHERE id=?",
		newPrompt, time.Now(), r.ID); err != nil {
		return false, fmt.Errorf("migrate architect role: %w", err)
	}
	return true, nil
}

// architectMigratedPrompt returns the current template-driven system_prompt
// for the architect role. Pulled out so MigrateArchitectRole + Reset can
// call the same source — adding more lines in the future keeps them in
// sync without a second copy-paste.
func architectMigratedPrompt() string {
	for _, d := range DefaultRoles() {
		if d.Key == "architect" {
			return d.SystemPrompt
		}
	}
	return ""
}

func (s *RoleService) List() ([]model.Role, error) {
	rows, err := s.db.Query("SELECT " + s.roleColumns() + " FROM roles ORDER BY sort_order ASC, " + s.db.Ident("key") + " ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roles []model.Role
	for rows.Next() {
		var r model.Role
		if err := rows.Scan(&r.ID, &r.Key, &r.Name, &r.Description, &r.SystemPrompt, &r.Model, &r.ClaudeConfigID, &r.SortOrder, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}
	if roles == nil {
		roles = []model.Role{}
	}
	return roles, nil
}

func (s *RoleService) Get(id string) (*model.Role, error) {
	var r model.Role
	err := s.db.QueryRow("SELECT "+s.roleColumns()+" FROM roles WHERE id = ?", id).
		Scan(&r.ID, &r.Key, &r.Name, &r.Description, &r.SystemPrompt, &r.Model, &r.ClaudeConfigID, &r.SortOrder, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("role not found")
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetByKey returns a role by its stable key (analyst/architect/developer/...),
// used by wizard handlers to load the active system prompt + model.
func (s *RoleService) GetByKey(key string) (*model.Role, error) {
	var r model.Role
	err := s.db.QueryRow("SELECT "+s.roleColumns()+" FROM roles WHERE "+s.db.Ident("key")+" = ?", key).
		Scan(&r.ID, &r.Key, &r.Name, &r.Description, &r.SystemPrompt, &r.Model, &r.ClaudeConfigID, &r.SortOrder, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("role not found: %s", key)
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *RoleService) Update(id string, req model.UpdateRoleReq) (*model.Role, error) {
	// claude_config_id is intentionally always written (including as the
	// empty string), so a user can "unbind" by saving with no config
	// selected. The previous single-active-config behavior is preserved
	// because empty falls through to the global active config.
	if _, err := s.db.Exec(
		"UPDATE roles SET system_prompt=?, model=?, claude_config_id=?, updated_at=? WHERE id=?",
		req.SystemPrompt, req.Model, req.ClaudeConfigID, time.Now(), id,
	); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Reset restores the built-in system_prompt for a role (matched by key),
// leaving the user-chosen model untouched.
func (s *RoleService) Reset(id string) (*model.Role, error) {
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	for _, d := range DefaultRoles() {
		if d.Key == r.Key {
			if _, err := s.db.Exec("UPDATE roles SET system_prompt=?, updated_at=? WHERE id=?", d.SystemPrompt, time.Now(), id); err != nil {
				return nil, err
			}
			break
		}
	}
	return s.Get(id)
}
