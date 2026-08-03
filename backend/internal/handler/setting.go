package handler

import (
	"encoding/json"
	"net/http"

	"github.com/novaworkbench/backend/internal/service"
)

type SettingHandler struct {
	svc *service.SettingService
}

func NewSettingHandler(svc *service.SettingService) *SettingHandler {
	return &SettingHandler{svc: svc}
}

// ClaudeConfigResponse is the API shape for the Claude CLI configuration.
// The auth token is never returned in full — only a masked preview + whether
// one is set — so the UI can display state without leaking the secret.
type ClaudeConfigResponse struct {
	AnthropicAuthTokenSet     bool   `json:"anthropic_auth_token_set"`
	AnthropicAuthTokenPreview string `json:"anthropic_auth_token_preview"`
	AnthropicBaseURL         string `json:"anthropic_base_url"`
}

// GetClaude returns the current Claude CLI configuration (token masked).
func (h *SettingHandler) GetClaude(w http.ResponseWriter, r *http.Request) {
	tok, baseURL, err := h.svc.ClaudeConfig()
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, ClaudeConfigResponse{
		AnthropicAuthTokenSet:     tok != "",
		AnthropicAuthTokenPreview: service.MaskToken(tok),
		AnthropicBaseURL:         baseURL,
	})
}

// UpdateClaude upserts the Claude CLI configuration. An empty auth token means
// "keep the existing secret" (so base-URL-only edits don't wipe the token);
// an empty base URL clears it (use the API default). Set clear_token=true to
// explicitly remove the stored token.
func (h *SettingHandler) UpdateClaude(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AnthropicAuthToken string `json:"anthropic_auth_token"`
		AnthropicBaseURL  string `json:"anthropic_base_url"`
		ClearToken        bool   `json:"clear_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if req.ClearToken {
		if err := h.svc.ClearClaudeAuthToken(); err != nil {
			writeError(w, 500, "INTERNAL", err.Error())
			return
		}
	} else if err := h.svc.SetClaudeConfig(req.AnthropicAuthToken, req.AnthropicBaseURL); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	// Re-read so the response reflects the persisted state (token still masked).
	tok, baseURL, err := h.svc.ClaudeConfig()
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, ClaudeConfigResponse{
		AnthropicAuthTokenSet:     tok != "",
		AnthropicAuthTokenPreview: service.MaskToken(tok),
		AnthropicBaseURL:         baseURL,
	})
}
