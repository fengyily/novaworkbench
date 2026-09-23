package handler

import (
	"errors"
	"fmt"
	"testing"
)

// TestWriteServiceError_Mapping covers the central prefix → (status, code)
// table. Each new service error only needs to add a row here AND one case in
// mapServiceErr — keeping this list aligned is the single source of truth
// for what the handler layer promises to surface to clients.
func TestWriteServiceError_Mapping(t *testing.T) {
	tests := []struct {
		prefix     string
		wantStatus int
		wantCode   string
	}{
		{"TOKEN_NOT_FOUND", 404, "TOKEN_NOT_FOUND"},
		{"PLATFORM_UNSUPPORTED", 400, "PLATFORM_UNSUPPORTED"},
		{"TOKEN_INVALID", 400, "TOKEN_INVALID"},
		{"PLATFORM_MISMATCH", 400, "PLATFORM_MISMATCH"},
		{"AGENT_NOT_FOUND", 404, "AGENT_NOT_FOUND"},
		{"AUTH_DECRYPT_FAILED", 500, "AUTH_DECRYPT_FAILED"},
		{"SSH_CONNECT_FAILED", 502, "SSH_CONNECT_FAILED"},
		{"SSH_SESSION_FAILED", 502, "SSH_CONNECT_FAILED"},
		{"SSH_COMMAND_FAILED", 502, "SSH_CONNECT_FAILED"},
		{"LLM_CONFIG_NOT_FOUND", 404, "LLM_CONFIG_NOT_FOUND"},
		{"BASE_URL_MISSING", 400, "BASE_URL_MISSING"},
		{"LLM_TOKEN_INVALID", 502, "LLM_PROBE_FAILED"},
		{"LLM_CONNECT_FAILED", 502, "LLM_PROBE_FAILED"},
		{"LLM_PROBE_FAILED", 502, "LLM_PROBE_FAILED"},
		{"REMOTE_UNREACHABLE", 502, "REMOTE_UNREACHABLE"},
		{"CLONE_PRECHECK_FAILED", 502, "REMOTE_UNREACHABLE"},
		{"PATH_OUT_OF_WORKSPACE", 400, "PATH_OUT_OF_WORKSPACE"},
		{"REMOVE_DIR_FAILED", 500, "REMOVE_DIR_FAILED"},
		{"DIR_EXISTS", 409, "DIR_EXISTS"},
		{"NO_REMOTE", 400, "NO_REMOTE"},
		{"RESTORE_FAILED", 500, "RESTORE_FAILED"},
	}
	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			status, code := mapServiceErr(fmt.Errorf("%s: foo", tt.prefix))
			if status != tt.wantStatus {
				t.Errorf("status: got %d, want %d", status, tt.wantStatus)
			}
			if code != tt.wantCode {
				t.Errorf("code: got %q, want %q", code, tt.wantCode)
			}
		})
	}
}

// TestWriteServiceError_UnknownPrefix confirms the safety net catches any
// unprefixed error and surfaces it as a generic 500.
func TestWriteServiceError_UnknownPrefix(t *testing.T) {
	status, code := mapServiceErr(errors.New("totally_unknown: foo"))
	if status != 500 {
		t.Errorf("status: got %d, want 500", status)
	}
	if code != "INTERNAL_ERROR" {
		t.Errorf("code: got %q, want INTERNAL_ERROR", code)
	}
}