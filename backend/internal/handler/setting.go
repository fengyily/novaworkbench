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

// SubTaskConfigResponse is the API shape for the sub-task execution policy:
// per-project concurrency + automatic failure retry. Mirrors the three
// settings keys one-for-one (see service.SettingService.SubTaskConfig).
type SubTaskConfigResponse struct {
	// Concurrency is how many sub-tasks ONE project may run at the same
	// time. Different projects still run in parallel — this is the "从项目的
	// 角度排队" knob, not a process-wide cap (that stays on env
	// NOVA_SUBTASK_CONCURRENCY).
	Concurrency int `json:"concurrency"`
	// AutoRetry turns automatic re-dispatch of failed auto-orchestrated
	// children on. Default false: recovery is a manual action.
	AutoRetry bool `json:"auto_retry"`
	// RetryMax caps automatic re-arms per child.
	RetryMax int `json:"retry_max"`
}

// GetSubTaskConfig returns the current sub-task execution policy.
func (h *SettingHandler) GetSubTaskConfig(w http.ResponseWriter, r *http.Request) {
	concurrency, autoRetry, retryMax, err := h.svc.SubTaskConfig()
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, SubTaskConfigResponse{Concurrency: concurrency, AutoRetry: autoRetry, RetryMax: retryMax})
}

// UpdateSubTaskConfig persists the sub-task execution policy. The write only
// touches the settings table — the orchestration tick and SubTaskRunner.Run
// re-read it on their next pass (≤10s for the tick), so the new values take
// effect without a restart and without this handler holding a reference to the
// ProjectLimiter.
func (h *SettingHandler) UpdateSubTaskConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Concurrency int  `json:"concurrency"`
		AutoRetry   bool `json:"auto_retry"`
		RetryMax    int  `json:"retry_max"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if req.Concurrency < 1 {
		writeError(w, 400, "INVALID", "concurrency 必须 ≥ 1")
		return
	}
	if req.RetryMax < 0 {
		writeError(w, 400, "INVALID", "retry_max 不能为负数")
		return
	}
	if err := h.svc.SetSubTaskConfig(req.Concurrency, req.AutoRetry, req.RetryMax); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	// Re-read so the response reflects the persisted (clamped) state.
	concurrency, autoRetry, retryMax, err := h.svc.SubTaskConfig()
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, SubTaskConfigResponse{Concurrency: concurrency, AutoRetry: autoRetry, RetryMax: retryMax})
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
