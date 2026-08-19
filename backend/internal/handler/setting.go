package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/service"
)

type SettingHandler struct {
	svc *service.SettingService
}

func NewSettingHandler(svc *service.SettingService) *SettingHandler {
	return &SettingHandler{svc: svc}
}

// LLMConfigResponse is the API shape for the direct HTTP LLM channel
// configuration (OpenAI-compatible, e.g. DeepSeek). The API key is never
// returned in full — only a masked preview + whether one is set.
type LLMConfigResponse struct {
	BaseURL       string `json:"base_url"`
	APIKeySet     bool   `json:"api_key_set"`
	APIKeyPreview string `json:"api_key_preview"`
	Model         string `json:"model"`
}

// GetLLM returns the current direct LLM channel configuration (key masked).
func (h *SettingHandler) GetLLM(w http.ResponseWriter, r *http.Request) {
	baseURL, apiKey, model, err := h.svc.LLMConfig()
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, LLMConfigResponse{
		BaseURL:       baseURL,
		APIKeySet:     apiKey != "",
		APIKeyPreview: service.MaskToken(apiKey),
		Model:         model,
	})
}

// UpdateLLM upserts the direct LLM channel configuration. An empty api_key
// means "keep the existing secret" (so base-URL/model-only edits don't wipe
// the key); set clear_api_key=true to explicitly remove the stored key.
func (h *SettingHandler) UpdateLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL     string `json:"base_url"`
		APIKey      string `json:"api_key"`
		Model       string `json:"model"`
		ClearAPIKey bool   `json:"clear_api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if req.ClearAPIKey {
		if err := h.svc.ClearLLMAPIKey(); err != nil {
			writeError(w, 500, "INTERNAL", err.Error())
			return
		}
	} else if err := h.svc.SetLLMConfig(req.BaseURL, req.APIKey, req.Model); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	// Re-read so the response reflects the persisted state (key still masked).
	baseURL, apiKey, model, err := h.svc.LLMConfig()
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, LLMConfigResponse{
		BaseURL:       baseURL,
		APIKeySet:     apiKey != "",
		APIKeyPreview: service.MaskToken(apiKey),
		Model:         model,
	})
}

// GetCodingTimeout returns the effective coding-task timeout as a duration
// string (resolved from the settings table / env, default 2h).
// GET /api/settings/coding-timeout
func (h *SettingHandler) GetCodingTimeout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"coding_timeout": h.svc.CodingTimeout().String()})
}

// UpdateCodingTimeout persists the coding-task timeout. The value must be a
// parseable Go duration string ("2h", "90m", "45m30s").
// PUT /api/settings/coding-timeout
func (h *SettingHandler) UpdateCodingTimeout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CodingTimeout string `json:"coding_timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	d, err := time.ParseDuration(strings.TrimSpace(req.CodingTimeout))
	if err != nil || d <= 0 {
		writeError(w, 400, "INVALID", "无效的时长，示例: 2h / 90m / 45m30s")
		return
	}
	if err := h.svc.SetCodingTimeout(d); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"coding_timeout": d.String()})
}
