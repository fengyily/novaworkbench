package service

import "github.com/novaworkbench/backend/internal/model"

// DefaultPermissions returns the seed permission catalog. Keys are stable
// strings consumed by the frontend (menu.* / setting.* / project.* / action.*).
// Adding a new permission here backfills it into existing databases on next
// boot (SeedDefaults is per-key idempotent).
func DefaultPermissions() []model.Permission {
	return []model.Permission{
		{ID: "perm_menu_dashboard", Key: "menu.dashboard", Name: "仪表盘", Module: "menu", SortOrder: 1},
		{ID: "perm_menu_projects", Key: "menu.projects", Name: "项目", Module: "menu", SortOrder: 2},
		{ID: "perm_menu_knowledge", Key: "menu.knowledge", Name: "知识库", Module: "menu", SortOrder: 3},
		{ID: "perm_menu_chat", Key: "menu.chat", Name: "AI对话", Module: "menu", SortOrder: 4},
		{ID: "perm_menu_reports", Key: "menu.reports", Name: "周报", Module: "menu", SortOrder: 5},
		{ID: "perm_menu_settings", Key: "menu.settings", Name: "设置入口", Module: "menu", SortOrder: 6},

		{ID: "perm_proj_manage", Key: "project.manage", Name: "项目管理（增删改）", Module: "project", SortOrder: 10},

		{ID: "perm_set_users", Key: "setting.users", Name: "用户管理", Module: "setting", SortOrder: 20},
		{ID: "perm_set_acl", Key: "setting.acl", Name: "角色权限管理", Module: "setting", SortOrder: 21},
		{ID: "perm_set_tokens", Key: "setting.tokens", Name: "平台 Token", Module: "setting", SortOrder: 22},
		{ID: "perm_set_roles_ai", Key: "setting.roles_ai", Name: "AI 角色", Module: "setting", SortOrder: 23},
		{ID: "perm_set_claude", Key: "setting.claude", Name: "Claude 配置", Module: "setting", SortOrder: 24},
		{ID: "perm_set_llm", Key: "setting.llm", Name: "直连 LLM", Module: "setting", SortOrder: 25},
		{ID: "perm_set_database", Key: "setting.database", Name: "数据库", Module: "setting", SortOrder: 26},
		{ID: "perm_set_preflight", Key: "setting.preflight", Name: "环境依赖", Module: "setting", SortOrder: 27},
	}
}

// DefaultACLRoles returns the built-in RBAC role definitions. is_builtin roles
// cannot be deleted (their permission set is re-synced each boot from
// DefaultRolePermissions).
func DefaultACLRoles() []model.ACLRole {
	return []model.ACLRole{
		{
			ID: "arole_developer", Key: "developer", Name: "开发者",
			Description: "可访问项目、知识库、周报、AI对话，可执行需求开发流程。",
			IsBuiltin: true, SortOrder: 10, Enabled: true,
		},
		{
			ID: "arole_viewer", Key: "viewer", Name: "观察者",
			Description: "只读查看被分配的项目与知识库。",
			IsBuiltin: true, SortOrder: 20, Enabled: true,
		},
	}
}

// DefaultRolePermissions maps a built-in role key to its default permission
// keys. Re-applied on every boot so a later release that grants a new
// permission to a built-in role propagates automatically. Admins are NOT
// listed here — they short-circuit to "*" in UserPermissions.
func DefaultRolePermissions() map[string][]string {
	return map[string][]string{
		"developer": {
			"menu.dashboard", "menu.projects", "menu.knowledge", "menu.chat", "menu.reports",
			"menu.settings", "setting.roles_ai", "setting.preflight",
		},
		"viewer": {
			"menu.dashboard", "menu.projects", "menu.knowledge",
		},
	}
}
