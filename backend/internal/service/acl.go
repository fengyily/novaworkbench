package service

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// sessionTokenLen is the byte length of a random session token (hex-encoded
// → 2x this many chars). 32 bytes = 256 bits of entropy.
const sessionTokenLen = 32

// sessionTTL is how long a login token stays valid.
const sessionTTL = 7 * 24 * time.Hour

// ErrInvalidCredentials is returned by Login on a bad username/password.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrUserDisabled is returned by Login when the account is disabled.
var ErrUserDisabled = errors.New("user is disabled")

// ACLService implements the user-role-permission (RBAC) layer. It owns the
// `users`, `acl_roles`, `permissions`, `acl_role_permissions`,
// `acl_user_roles`, `user_projects` and `sessions` tables.
type ACLService struct{ db *db.DB }

func NewACLService(database *db.DB) *ACLService { return &ACLService{db: database} }

// ---- helpers ---------------------------------------------------------------

func hashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func checkPassword(hash, plain string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// identKey quotes the reserved word `key` for the active dialect (reserved in
// MySQL). Mirrors the pattern in RoleService.
func (s *ACLService) identKey() string { return s.db.Ident("key") }

// ---- seeding ---------------------------------------------------------------

// SeedDefaults idempotently seeds built-in permissions, RBAC roles,
// role↔permission bindings, and a default admin account. Per-key / per-id
// inserts so re-runs and later-release additions backfill existing databases
// (mirrors RoleService.SeedDefaults).
//
// The default admin password is generated randomly on first seed and logged
// by the caller (main.go) — the service returns it only when it creates the
// row, so the caller can surface it once.
func (s *ACLService) SeedDefaults() (defaultAdminPassword string, err error) {
	if err := s.seedPermissions(); err != nil {
		return "", fmt.Errorf("seed permissions: %w", err)
	}
	if err := s.seedRoles(); err != nil {
		return "", fmt.Errorf("seed acl roles: %w", err)
	}
	if err := s.seedRolePermissions(); err != nil {
		return "", fmt.Errorf("seed role permissions: %w", err)
	}
	pw, err := s.seedDefaultAdmin()
	if err != nil {
		return "", fmt.Errorf("seed default admin: %w", err)
	}
	return pw, nil
}

func (s *ACLService) seedPermissions() error {
	for _, p := range DefaultPermissions() {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM permissions WHERE "+s.identKey()+" = ?", p.Key).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if _, err := s.db.Exec(
			`INSERT INTO permissions (id, `+s.identKey()+`, name, module, description, sort_order, created_at)
			 VALUES (?,?,?,?,?,?,?)`,
			p.ID, p.Key, p.Name, p.Module, p.Description, p.SortOrder, time.Now(),
		); err != nil {
			return fmt.Errorf("permission %s: %w", p.Key, err)
		}
	}
	return nil
}

func (s *ACLService) seedRoles() error {
	for _, r := range DefaultACLRoles() {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM acl_roles WHERE "+s.identKey()+" = ?", r.Key).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if _, err := s.db.Exec(
			`INSERT INTO acl_roles (id, `+s.identKey()+`, name, description, is_builtin, sort_order, enabled, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Key, r.Name, r.Description, r.IsBuiltin, r.SortOrder, r.Enabled, time.Now(), time.Now(),
		); err != nil {
			return fmt.Errorf("acl role %s: %w", r.Key, err)
		}
	}
	return nil
}

// seedRolePermissions binds every built-in role to its default permission set.
// Idempotent via the composite PK (ON CONFLICT no-op). For built-in roles we
// also backfill any permission added in a later release — re-sync the full
// default set each boot.
func (s *ACLService) seedRolePermissions() error {
	for roleKey, permKeys := range DefaultRolePermissions() {
		role, err := s.GetACLRoleByKey(roleKey)
		if err != nil {
			return err
		}
		for _, pk := range permKeys {
			perm, err := s.GetPermissionByKey(pk)
			if err != nil {
				return fmt.Errorf("permission %s for role %s: %w", pk, roleKey, err)
			}
			if _, err := s.db.Exec(
				`INSERT INTO acl_role_permissions (role_id, permission_id) VALUES (?,?) `+
					s.db.OnConflict("role_id,permission_id", "role_id=excluded.role_id"),
				role.ID, perm.ID,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedDefaultAdmin creates the admin account on first run only. Returns the
// plaintext password when it creates the row (empty string on subsequent
// boots). The account is marked is_admin=1 so it bypasses permission checks.
func (s *ACLService) seedDefaultAdmin() (string, error) {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", "admin").Scan(&count); err != nil {
		return "", err
	}
	if count > 0 {
		return "", nil
	}
	pw := util.NewID("", 16) // random hex
	hash, err := hashPassword(pw)
	if err != nil {
		return "", err
	}
	id := "usr_admin"
	if _, err := s.db.Exec(
		`INSERT INTO users (id, username, display_name, password_hash, status, is_admin, created_at, updated_at)
		 VALUES (?,?,?,?,?,1,?,?)`,
		id, "admin", "管理员", hash, "active", time.Now(), time.Now(),
	); err != nil {
		return "", err
	}
	return pw, nil
}

// ---- auth -----------------------------------------------------------------

// Login validates credentials, creates a session, and returns the profile.
func (s *ACLService) Login(username, plain string) (*model.SessionProfile, error) {
	var (
		id        string
		display   string
		hash      string
		status    string
		isAdmin   int
	)
	err := s.db.QueryRow(
		`SELECT id, display_name, password_hash, status, is_admin FROM users WHERE username = ?`,
		username,
	).Scan(&id, &display, &hash, &status, &isAdmin)
	if err == sql.ErrNoRows {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, ErrUserDisabled
	}
	if !checkPassword(hash, plain) {
		return nil, ErrInvalidCredentials
	}

	token := util.NewID("", sessionTokenLen) // hex token
	expires := time.Now().Add(sessionTTL)
	if _, err := s.db.Exec(
		`INSERT INTO sessions (token, user_id, expires_at, created_at) VALUES (?,?,?,?)`,
		token, id, expires, time.Now(),
	); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`, time.Now(), id); err != nil {
		return nil, err
	}

	u, err := s.GetUser(id)
	if err != nil {
		return nil, err
	}
	perms, err := s.UserPermissions(id, isAdmin == 1)
	if err != nil {
		return nil, err
	}
	return &model.SessionProfile{Token: token, User: *u, Permissions: perms}, nil
}

// Logout deletes the session row (no-op if already gone).
func (s *ACLService) Logout(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// SessionUser resolves a bearer token to the user + effective permissions.
// Expired or unknown tokens return sql.ErrNoRows.
func (s *ACLService) SessionUser(token string) (*model.User, []string, error) {
	var (
		userID   string
		expires  time.Time
	)
	err := s.db.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token = ?`, token).
		Scan(&userID, &expires)
	if err == sql.ErrNoRows {
		return nil, nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, nil, err
	}
	if time.Now().After(expires) {
		// expired — clean up
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
		return nil, nil, sql.ErrNoRows
	}
	u, err := s.GetUser(userID)
	if err != nil {
		return nil, nil, err
	}
	if u.Status != "active" {
		return nil, nil, sql.ErrNoRows
	}
	perms, err := s.UserPermissions(userID, u.IsAdmin)
	if err != nil {
		return nil, nil, err
	}
	return u, perms, nil
}

// UserPermissions returns the union of permission keys across all of a user's
// roles. Admins short-circuit to "*" (all permissions) so a misconfigured role
// binding can never lock out an admin.
func (s *ACLService) UserPermissions(userID string, isAdmin bool) ([]string, error) {
	if isAdmin {
		return []string{"*"}, nil
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT p.`+s.identKey()+` FROM permissions p
		 JOIN acl_role_permissions rp ON rp.permission_id = p.id
		 JOIN acl_user_roles ur ON ur.role_id = rp.role_id
		 WHERE ur.user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var perms []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		perms = append(perms, k)
	}
	if perms == nil {
		perms = []string{}
	}
	return perms, nil
}

// HasPermission reports whether the permission set (from UserPermissions)
// grants the given key. The "*" wildcard (admin) grants everything.
func HasPermission(perms []string, key string) bool {
	for _, p := range perms {
		if p == "*" || p == key {
			return true
		}
	}
	return false
}

// ---- users ----------------------------------------------------------------

func (s *ACLService) ListUsers() ([]model.User, error) {
	rows, err := s.db.Query(
		`SELECT id, username, display_name, status, is_admin, last_login_at, created_at, updated_at
		 FROM users ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []model.User
	for rows.Next() {
		var u model.User
		var isAdmin int
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Status, &isAdmin, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.IsAdmin = isAdmin == 1
		users = append(users, u)
	}
	return users, nil
}

func (s *ACLService) GetUser(id string) (*model.User, error) {
	var u model.User
	var isAdmin int
	err := s.db.QueryRow(
		`SELECT id, username, display_name, status, is_admin, last_login_at, created_at, updated_at
		 FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.DisplayName, &u.Status, &isAdmin, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("user not found")
	}
	if err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin == 1

	roleIDs, err := s.userRoleIDs(id)
	if err != nil {
		return nil, err
	}
	u.RoleIDs = roleIDs
	projectIDs, err := s.UserProjectIDs(id)
	if err != nil {
		return nil, err
	}
	u.ProjectIDs = projectIDs
	return &u, nil
}

func (s *ACLService) CreateUser(req model.CreateUserRequest) (*model.User, error) {
	if req.Username == "" || req.Password == "" {
		return nil, fmt.Errorf("username and password are required")
	}
	var exists int
	s.db.QueryRow("SELECT COUNT(*) FROM users WHERE username = ?", req.Username).Scan(&exists)
	if exists > 0 {
		return nil, fmt.Errorf("username already exists: %s", req.Username)
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		return nil, err
	}
	id := util.NewID("usr")
	now := time.Now()
	status := req.Status
	if status == "" {
		status = "active"
	}
	if _, err := s.db.Exec(
		`INSERT INTO users (id, username, display_name, password_hash, status, is_admin, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, req.Username, req.DisplayName, hash, status, req.IsAdmin, now, now,
	); err != nil {
		return nil, err
	}
	if err := s.setUserRoles(id, req.RoleIDs); err != nil {
		return nil, err
	}
	if err := s.AssignProjects(id, req.ProjectIDs); err != nil {
		return nil, err
	}
	return s.GetUser(id)
}

func (s *ACLService) UpdateUser(id string, req model.UpdateUserRequest) (*model.User, error) {
	// Build the UPDATE column list dynamically so absent fields are preserved.
	setParts := []string{}
	args := []any{}
	if req.DisplayName != nil {
		setParts = append(setParts, "display_name = ?")
		args = append(args, *req.DisplayName)
	}
	if req.Status != nil {
		setParts = append(setParts, "status = ?")
		args = append(args, *req.Status)
	}
	if req.IsAdmin != nil {
		setParts = append(setParts, "is_admin = ?")
		args = append(args, btoi(*req.IsAdmin))
	}
	if req.Password != "" {
		hash, err := hashPassword(req.Password)
		if err != nil {
			return nil, err
		}
		setParts = append(setParts, "password_hash = ?")
		args = append(args, hash)
		// Changing the password invalidates existing sessions.
		if _, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
			return nil, err
		}
	}
	if len(setParts) > 0 {
		setParts = append(setParts, "updated_at = ?")
		args = append(args, time.Now())
		args = append(args, id)
		if _, err := s.db.Exec(
			`UPDATE users SET `+join(setParts, ", ")+` WHERE id = ?`, args...,
		); err != nil {
			return nil, err
		}
	}

	// RoleIDs / ProjectIDs: only reassign when the slice is non-nil (present
	// in the JSON body). nil = "leave unchanged", []  = "clear all".
	if req.RoleIDs != nil {
		if err := s.setUserRoles(id, req.RoleIDs); err != nil {
			return nil, err
		}
	}
	if req.ProjectIDs != nil {
		if err := s.AssignProjects(id, req.ProjectIDs); err != nil {
			return nil, err
		}
	}
	return s.GetUser(id)
}

func (s *ACLService) DeleteUser(id string) error {
	// Prevent deleting the last admin.
	var isAdmin int
	err := s.db.QueryRow(`SELECT is_admin FROM users WHERE id = ?`, id).Scan(&isAdmin)
	if err == sql.ErrNoRows {
		return fmt.Errorf("user not found")
	}
	if err != nil {
		return err
	}
	if isAdmin == 1 {
		var adminCount int
		s.db.QueryRow("SELECT COUNT(*) FROM users WHERE is_admin = 1").Scan(&adminCount)
		if adminCount <= 1 {
			return fmt.Errorf("cannot delete the last admin account")
		}
	}
	_, err = s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// ---- user ↔ role ----------------------------------------------------------

func (s *ACLService) userRoleIDs(userID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT role_id FROM acl_user_roles WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// setUserRoles replaces the user's role set. Uses a transaction so a partial
// failure never leaves the user with half their roles.
func (s *ACLService) setUserRoles(userID string, roleIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM acl_user_roles WHERE user_id = ?`, userID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, rid := range roleIDs {
		if rid == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO acl_user_roles (user_id, role_id) VALUES (?,?) `+
				s.db.OnConflict("user_id,role_id", "user_id=excluded.user_id"),
			userID, rid,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ---- user ↔ project -------------------------------------------------------

func (s *ACLService) UserProjectIDs(userID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT project_id FROM user_projects WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// AssignProjects replaces the user's project set. Pass an empty slice to
// revoke all assignments.
func (s *ACLService) AssignProjects(userID string, projectIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_projects WHERE user_id = ?`, userID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, pid := range projectIDs {
		if pid == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO user_projects (user_id, project_id, assigned_at) VALUES (?,?,?) `+
				s.db.OnConflict("user_id,project_id", "user_id=excluded.user_id"),
			userID, pid, time.Now(),
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// CanAccessProject reports whether userID may see projectID. Admins see all.
func (s *ACLService) CanAccessProject(userID string, isAdmin bool, projectID string) (bool, error) {
	if isAdmin {
		return true, nil
	}
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM user_projects WHERE user_id = ? AND project_id = ?`, userID, projectID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ---- ACL roles -------------------------------------------------------------

func (s *ACLService) ListACLRoles() ([]model.ACLRole, error) {
	rows, err := s.db.Query(
		`SELECT id, `+s.identKey()+`, name, description, is_builtin, sort_order, enabled, created_at, updated_at
		 FROM acl_roles ORDER BY sort_order ASC, `+s.identKey()+` ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []model.ACLRole
	for rows.Next() {
		var r model.ACLRole
		var isBuiltin, enabled int
		if err := rows.Scan(&r.ID, &r.Key, &r.Name, &r.Description, &isBuiltin, &r.SortOrder, &enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.IsBuiltin = isBuiltin == 1
		r.Enabled = enabled == 1
		roles = append(roles, r)
	}
	if roles == nil {
		roles = []model.ACLRole{}
	}
	for i := range roles {
		permIDs, permKeys, err := s.rolePermissions(roles[i].ID)
		if err != nil {
			return nil, err
		}
		roles[i].PermissionIDs = permIDs
		roles[i].PermissionKeys = permKeys
		var uc int
		s.db.QueryRow("SELECT COUNT(*) FROM acl_user_roles WHERE role_id = ?", roles[i].ID).Scan(&uc)
		roles[i].UserCount = uc
	}
	return roles, nil
}

func (s *ACLService) GetACLRole(id string) (*model.ACLRole, error) {
	var r model.ACLRole
	var isBuiltin, enabled int
	err := s.db.QueryRow(
		`SELECT id, `+s.identKey()+`, name, description, is_builtin, sort_order, enabled, created_at, updated_at
		 FROM acl_roles WHERE id = ?`, id).
		Scan(&r.ID, &r.Key, &r.Name, &r.Description, &isBuiltin, &r.SortOrder, &enabled, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("acl role not found")
	}
	if err != nil {
		return nil, err
	}
	r.IsBuiltin = isBuiltin == 1
	r.Enabled = enabled == 1
	permIDs, permKeys, err := s.rolePermissions(r.ID)
	if err != nil {
		return nil, err
	}
	r.PermissionIDs = permIDs
	r.PermissionKeys = permKeys
	return &r, nil
}

func (s *ACLService) GetACLRoleByKey(key string) (*model.ACLRole, error) {
	var r model.ACLRole
	var isBuiltin, enabled int
	err := s.db.QueryRow(
		`SELECT id, `+s.identKey()+`, name, description, is_builtin, sort_order, enabled, created_at, updated_at
		 FROM acl_roles WHERE `+s.identKey()+` = ?`, key).
		Scan(&r.ID, &r.Key, &r.Name, &r.Description, &isBuiltin, &r.SortOrder, &enabled, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("acl role not found: %s", key)
	}
	if err != nil {
		return nil, err
	}
	r.IsBuiltin = isBuiltin == 1
	r.Enabled = enabled == 1
	return &r, nil
}

func (s *ACLService) CreateACLRole(req model.CreateACLRoleRequest) (*model.ACLRole, error) {
	if req.Key == "" || req.Name == "" {
		return nil, fmt.Errorf("key and name are required")
	}
	var exists int
	s.db.QueryRow("SELECT COUNT(*) FROM acl_roles WHERE "+s.identKey()+" = ?", req.Key).Scan(&exists)
	if exists > 0 {
		return nil, fmt.Errorf("role key already exists: %s", req.Key)
	}
	id := util.NewID("arole")
	now := time.Now()
	if _, err := s.db.Exec(
		`INSERT INTO acl_roles (id, `+s.identKey()+`, name, description, is_builtin, sort_order, enabled, created_at, updated_at)
		 VALUES (?,?,?,?,0,?,?,?,?)`,
		id, req.Key, req.Name, req.Description, req.SortOrder, req.Enabled, now, now,
	); err != nil {
		return nil, err
	}
	if err := s.setRolePermissions(id, req.PermissionIDs); err != nil {
		return nil, err
	}
	return s.GetACLRole(id)
}

func (s *ACLService) UpdateACLRole(id string, req model.UpdateACLRoleRequest) (*model.ACLRole, error) {
	existing, err := s.GetACLRole(id)
	if err != nil {
		return nil, err
	}
	setParts := []string{}
	args := []any{}
	if req.Name != nil {
		setParts = append(setParts, "name = ?")
		args = append(args, *req.Name)
	}
	if req.Description != nil {
		setParts = append(setParts, "description = ?")
		args = append(args, *req.Description)
	}
	if req.SortOrder != nil {
		setParts = append(setParts, "sort_order = ?")
		args = append(args, *req.SortOrder)
	}
	if req.Enabled != nil {
		setParts = append(setParts, "enabled = ?")
		args = append(args, *req.Enabled)
	}
	if len(setParts) > 0 {
		setParts = append(setParts, "updated_at = ?")
		args = append(args, time.Now())
		args = append(args, id)
		if _, err := s.db.Exec(`UPDATE acl_roles SET `+join(setParts, ", ")+` WHERE id = ?`, args...); err != nil {
			return nil, err
		}
	}
	// Only reassign permissions when the slice is non-nil.
	if req.PermissionIDs != nil && !existing.IsBuiltin {
		if err := s.setRolePermissions(id, req.PermissionIDs); err != nil {
			return nil, err
		}
	}
	return s.GetACLRole(id)
}

func (s *ACLService) DeleteACLRole(id string) error {
	r, err := s.GetACLRole(id)
	if err != nil {
		return err
	}
	if r.IsBuiltin {
		return fmt.Errorf("built-in roles cannot be deleted")
	}
	_, err = s.db.Exec(`DELETE FROM acl_roles WHERE id = ?`, id)
	return err
}

// ---- role ↔ permissions ---------------------------------------------------

func (s *ACLService) rolePermissions(roleID string) (ids []string, keys []string, err error) {
	rows, err := s.db.Query(
		`SELECT p.id, p.`+s.identKey()+` FROM permissions p
		 JOIN acl_role_permissions rp ON rp.permission_id = p.id
		 WHERE rp.role_id = ? ORDER BY p.sort_order ASC`, roleID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		keys = append(keys, key)
	}
	if ids == nil {
		ids = []string{}
	}
	if keys == nil {
		keys = []string{}
	}
	return ids, keys, nil
}

func (s *ACLService) setRolePermissions(roleID string, permIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM acl_role_permissions WHERE role_id = ?`, roleID); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, pid := range permIDs {
		if pid == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO acl_role_permissions (role_id, permission_id) VALUES (?,?) `+
				s.db.OnConflict("role_id,permission_id", "role_id=excluded.role_id"),
			roleID, pid,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ---- permissions ----------------------------------------------------------

func (s *ACLService) ListPermissions() ([]model.Permission, error) {
	rows, err := s.db.Query(
		`SELECT id, `+s.identKey()+`, name, module, description, sort_order, created_at
		 FROM permissions ORDER BY sort_order ASC, `+s.identKey()+` ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var perms []model.Permission
	for rows.Next() {
		var p model.Permission
		if err := rows.Scan(&p.ID, &p.Key, &p.Name, &p.Module, &p.Description, &p.SortOrder, &p.CreatedAt); err != nil {
			return nil, err
		}
		perms = append(perms, p)
	}
	if perms == nil {
		perms = []model.Permission{}
	}
	return perms, nil
}

func (s *ACLService) GetPermissionByKey(key string) (*model.Permission, error) {
	var p model.Permission
	err := s.db.QueryRow(
		`SELECT id, `+s.identKey()+`, name, module, description, sort_order, created_at
		 FROM permissions WHERE `+s.identKey()+` = ?`, key).
		Scan(&p.ID, &p.Key, &p.Name, &p.Module, &p.Description, &p.SortOrder, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("permission not found: %s", key)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
