package handler

import (
	"errors"
	"net/http"

	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type AuthHandler struct {
	svc *service.ACLService
}

func NewAuthHandler(svc *service.ACLService) *AuthHandler {
	return &AuthHandler{svc: svc}
}

// Login validates credentials and returns a session profile (token + user +
// effective permissions). POST /api/auth/login  body: {username,password}
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELDS", "用户名和密码不能为空")
		return
	}
	prof, err := h.svc.Login(req.Username, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "用户名或密码错误")
		case errors.Is(err, service.ErrUserDisabled):
			writeError(w, http.StatusForbidden, "USER_DISABLED", "账号已被禁用，请联系管理员")
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, prof)
}

// Logout invalidates the current session. POST /api/auth/logout
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	u := middleware.CurrentUser(r)
	// The bearer token is the one in the Authorization header.
	token := bearerTokenFromRequest(r)
	if u != nil && token != "" {
		_ = h.svc.Logout(token)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

// Me returns the current user profile + permissions. GET /api/auth/me
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	u := middleware.CurrentUser(r)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "未登录")
		return
	}
	user, err := h.svc.GetUser(u.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, model.SessionProfile{
		Token:       bearerTokenFromRequest(r),
		User:        *user,
		Permissions: u.Permissions,
	})
}

// bearerTokenFromRequest extracts the Bearer token. Kept here (not exported
// from middleware) to avoid exposing it as a public helper.
func bearerTokenFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return ""
	}
	return h[len(prefix):]
}
