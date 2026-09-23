package handler

import (
	"net/http"
	"strings"
)

// writeServiceError centralises the mapping of service-layer error prefixes to
// (HTTP status, error code) pairs. New error codes are added by appending one
// more case in mapServiceErr — no need to revisit every handler.
func writeServiceError(w http.ResponseWriter, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	status, code := mapServiceErr(err)
	writeError(w, status, code, err.Error())
}

// mapServiceErr matches the leading "PREFIX:" segment of an error message to
// a (status, code) pair. Unknown prefixes fall back to (500, "INTERNAL_ERROR").
// Prefix strings must be ASCII and must end with a literal ':' so HasPrefix
// is unambiguous when one prefix is a substring of another (e.g. SSH_*
// aliases).
func mapServiceErr(err error) (int, string) {
	msg := err.Error()
	switch {
	// Token / 平台
	case strings.HasPrefix(msg, "TOKEN_NOT_FOUND"):
		return http.StatusNotFound, "TOKEN_NOT_FOUND"
	case strings.HasPrefix(msg, "PLATFORM_UNSUPPORTED"):
		return http.StatusBadRequest, "PLATFORM_UNSUPPORTED"
	case strings.HasPrefix(msg, "TOKEN_INVALID"):
		return http.StatusBadRequest, "TOKEN_INVALID"
	case strings.HasPrefix(msg, "PLATFORM_MISMATCH"):
		return http.StatusBadRequest, "PLATFORM_MISMATCH"

	// Agent Server
	case strings.HasPrefix(msg, "AGENT_NOT_FOUND"):
		return http.StatusNotFound, "AGENT_NOT_FOUND"
	case strings.HasPrefix(msg, "AUTH_DECRYPT_FAILED"):
		return http.StatusInternalServerError, "AUTH_DECRYPT_FAILED"
	case strings.HasPrefix(msg, "SSH_CONNECT_FAILED"),
		strings.HasPrefix(msg, "SSH_SESSION_FAILED"),
		strings.HasPrefix(msg, "SSH_COMMAND_FAILED"):
		return http.StatusBadGateway, "SSH_CONNECT_FAILED"

	// LLM / Claude
	case strings.HasPrefix(msg, "LLM_CONFIG_NOT_FOUND"):
		return http.StatusNotFound, "LLM_CONFIG_NOT_FOUND"
	case strings.HasPrefix(msg, "BASE_URL_MISSING"):
		return http.StatusBadRequest, "BASE_URL_MISSING"
	case strings.HasPrefix(msg, "LLM_TOKEN_INVALID"),
		strings.HasPrefix(msg, "LLM_CONNECT_FAILED"),
		strings.HasPrefix(msg, "LLM_PROBE_FAILED"):
		return http.StatusBadGateway, "LLM_PROBE_FAILED"

	// 远端可达性
	case strings.HasPrefix(msg, "REMOTE_UNREACHABLE"),
		strings.HasPrefix(msg, "CLONE_PRECHECK_FAILED"):
		return http.StatusBadGateway, "REMOTE_UNREACHABLE"

	// 项目
	case strings.HasPrefix(msg, "PATH_OUT_OF_WORKSPACE"):
		return http.StatusBadRequest, "PATH_OUT_OF_WORKSPACE"
	case strings.HasPrefix(msg, "REMOVE_DIR_FAILED"):
		return http.StatusInternalServerError, "REMOVE_DIR_FAILED"
	case strings.HasPrefix(msg, "DIR_EXISTS"):
		return http.StatusConflict, "DIR_EXISTS"
	case strings.HasPrefix(msg, "NO_REMOTE"):
		return http.StatusBadRequest, "NO_REMOTE"
	case strings.HasPrefix(msg, "RESTORE_FAILED"):
		return http.StatusInternalServerError, "RESTORE_FAILED"
	}
	return http.StatusInternalServerError, "INTERNAL_ERROR"
}