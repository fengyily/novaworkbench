package model

import "time"

// User is an application account. Distinct from AI personas stored in the
// `roles` table (analyst/architect/...). PasswordHash holds a bcrypt hash;
// it is never serialized to JSON clients (json:"-").
type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	DisplayName  string     `json:"display_name"`
	Status       string     `json:"status"` // active | disabled
	IsAdmin      bool       `json:"is_admin"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	// RoleIDs / ProjectIDs are populated by the ACL service for the management
	// UI; not scanned in the base row query.
	RoleIDs    []string `json:"role_ids,omitempty"`
	ProjectIDs []string `json:"project_ids,omitempty"`
	// PasswordHash is write-only — accepted on create/update, never returned.
	PasswordHash string `json:"-"`
}

// CreateUserRequest is the body of POST /api/acl/users.
type CreateUserRequest struct {
	Username    string   `json:"username"`
	DisplayName string   `json:"display_name"`
	Password    string   `json:"password"`
	Status      string   `json:"status"`
	IsAdmin     bool     `json:"is_admin"`
	RoleIDs     []string `json:"role_ids"`
	ProjectIDs  []string `json:"project_ids"`
}

// UpdateUserRequest is the body of PUT /api/acl/users/{id}. Password is
// optional — empty means "leave unchanged".
type UpdateUserRequest struct {
	DisplayName *string  `json:"display_name"`
	Password    string   `json:"password"`
	Status      *string  `json:"status"`
	IsAdmin     *bool    `json:"is_admin"`
	RoleIDs     []string `json:"role_ids"`
	ProjectIDs  []string `json:"project_ids"`
}

// ACLRole is an RBAC role definition (lives in `acl_roles`). NOT the AI-persona
// `roles` table.
type ACLRole struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsBuiltin   bool      `json:"is_builtin"`
	SortOrder   int       `json:"sort_order"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// PermissionIDs / PermissionKeys are populated for the management UI.
	PermissionIDs   []string `json:"permission_ids,omitempty"`
	PermissionKeys  []string `json:"permission_keys,omitempty"`
	UserCount       int      `json:"user_count,omitempty"`
}

// CreateACLRoleRequest is the body of POST /api/acl/roles.
type CreateACLRoleRequest struct {
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	SortOrder    int      `json:"sort_order"`
	Enabled      bool     `json:"enabled"`
	PermissionIDs []string `json:"permission_ids"`
}

// UpdateACLRoleRequest is the body of PUT /api/acl/roles/{id}.
type UpdateACLRoleRequest struct {
	Name          *string  `json:"name"`
	Description   *string  `json:"description"`
	SortOrder     *int     `json:"sort_order"`
	Enabled       *bool    `json:"enabled"`
	PermissionIDs []string `json:"permission_ids"`
}

// Permission is a single capability key (e.g. menu.projects, setting.users).
type Permission struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Module      string    `json:"module"`
	Description string    `json:"description"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
}

// SessionProfile is the shape of GET /api/auth/me and the login response: the
// current user plus the effective permission keys (union across all roles).
type SessionProfile struct {
	Token       string   `json:"token"`
	User        User     `json:"user"`
	Permissions []string `json:"permissions"`
}
