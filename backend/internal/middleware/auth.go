package middleware

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/novaworkbench/backend/internal/service"
)

type contextKey string

const (
	// CtxUserID is the authenticated user's id.
	CtxUserID contextKey = "acl.user_id"
	// CtxUsername is the authenticated user's username.
	CtxUsername contextKey = "acl.username"
	// CtxIsAdmin marks admin accounts (bypass permission checks).
	CtxIsAdmin contextKey = "acl.is_admin"
	// CtxPermissions is the user's effective permission-key set ("*" for admin).
	CtxPermissions contextKey = "acl.permissions"
)

// UserContext is the authenticated principal extracted from the request.
type UserContext struct {
	UserID      string
	Username    string
	IsAdmin     bool
	Permissions []string
}

// CurrentUser returns the authenticated principal from the request context, or
// nil when unauthenticated.
func CurrentUser(r *http.Request) *UserContext {
	v := r.Context().Value(CtxUserID)
	if v == nil {
		return nil
	}
	uid, _ := v.(string)
	if uid == "" {
		return nil
	}
	uname, _ := r.Context().Value(CtxUsername).(string)
	isAdmin, _ := r.Context().Value(CtxIsAdmin).(bool)
	perms, _ := r.Context().Value(CtxPermissions).([]string)
	return &UserContext{
		UserID:      uid,
		Username:    uname,
		IsAdmin:     isAdmin,
		Permissions: perms,
	}
}

// authDisabled reports whether the NOVA_AUTH_DISABLED env bypass is on (local
// debug only). When set, every request is treated as an admin so the existing
// "open" local experience keeps working without a login.
func authDisabled() bool {
	return os.Getenv("NOVA_AUTH_DISABLED") == "1"
}

// publicPrefixes are reachable without authentication (login + health + the
// me-while-unauthenticated probe). CORS preflight (OPTIONS) is allowed in Auth.
var publicPrefixes = []string{
	"/api/auth/login",
	"/api/health",
}

func isPublic(path string) bool {
	for _, p := range publicPrefixes {
		if path == p {
			return true
		}
	}
	return false
}

// Auth wraps the router with bearer-token session authentication. On success
// the authenticated principal is injected into the request context; on failure
// it writes 401. Public paths (login, health) and the NOVA_AUTH_DISABLED
// bypass skip the check.
func Auth(aclSvc *service.ACLService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// CORS preflight — let the CORS middleware own it, but be safe.
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			// Local debug bypass: act as admin.
			if authDisabled() {
				ctx := withUser(r.Context(), "admin", "admin", true, []string{"*"})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if isPublic(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			token := bearerToken(r)
			if token == "" {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "未登录或会话已过期，请重新登录")
				return
			}
			user, perms, err := aclSvc.SessionUser(token)
			if err != nil {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "未登录或会话已过期，请重新登录")
				return
			}
			ctx := withUser(r.Context(), user.ID, user.Username, user.IsAdmin, perms)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func withUser(ctx context.Context, userID, username string, isAdmin bool, perms []string) context.Context {
	ctx = context.WithValue(ctx, CtxUserID, userID)
	ctx = context.WithValue(ctx, CtxUsername, username)
	ctx = context.WithValue(ctx, CtxIsAdmin, isAdmin)
	ctx = context.WithValue(ctx, CtxPermissions, perms)
	return ctx
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// RequirePermission returns a guard that rejects the request (403) unless the
// authenticated principal holds the given permission key. Admins ("*") always
// pass. Wire as: middleware.RequirePermission(aclSvc, "setting.users")(h.ListUsers).
// aclSvc is reserved for future re-resolution; permissions are read from context.
func RequirePermission(_ *service.ACLService, key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authDisabled() {
				next.ServeHTTP(w, r)
				return
			}
			u := CurrentUser(r)
			if u == nil {
				writeAuthError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "未登录或会话已过期，请重新登录")
				return
			}
			if u.IsAdmin || service.HasPermission(u.Permissions, key) {
				next.ServeHTTP(w, r)
				return
			}
			writeAuthError(w, http.StatusForbidden, "FORBIDDEN", "无权限执行此操作")
		})
	}
}

// writeAuthError mirrors the {success,data,error} envelope from
// handler/response.go but lives here to avoid an import cycle (handler imports
// middleware already for Logger/CORS, so middleware cannot import handler).
func writeAuthError(w http.ResponseWriter, code int, errCode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write([]byte(`{"success":false,"error":{"code":"` + errCode + `","message":"` + jsonEscape(msg) + `"}}`))
}

// jsonEscape minimally escapes a string for embedding in a JSON string value.
func jsonEscape(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 {
				continue
			}
			b.WriteRune(c)
		}
	}
	return b.String()
}
