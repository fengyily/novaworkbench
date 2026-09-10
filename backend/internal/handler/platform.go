package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

type PlatformHandler struct {
	svc *service.PlatformTokenService
}

func NewPlatformHandler(svc *service.PlatformTokenService) *PlatformHandler {
	return &PlatformHandler{svc: svc}
}

// List returns all platform tokens (without the raw secret).
// GET /api/settings/tokens
func (h *PlatformHandler) List(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.svc.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if tokens == nil {
		tokens = []model.PlatformToken{}
	}
	writeJSON(w, http.StatusOK, tokens)
}

// Create adds a new platform token.
// POST /api/settings/tokens
//   body: {
//     name, platform, base_url, token,
//     git_user_name?, git_user_email?,
//     gpg_enabled?, gpg_private_key?, gpg_passphrase?
//   }
//
// The GPG private key / passphrase are accepted here, encrypted on the
// server (AES-256-GCM via internal/secret) and never returned in any
// subsequent API response — List/Get responses go through Redact() so
// even an accidental struct return path drops them.
func (h *PlatformHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		Platform      string `json:"platform"`
		BaseURL       string `json:"base_url"`
		Token         string `json:"token"`
		GitUserName   string `json:"git_user_name"`
		GitUserEmail  string `json:"git_user_email"`
		GPGEnabled    bool   `json:"gpg_enabled"`
		GPGPrivateKey string `json:"gpg_private_key"`
		GPGPassphrase string `json:"gpg_passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if req.Name == "" || req.Platform == "" || req.Token == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELDS", "name、platform、token 不能为空")
		return
	}
	if req.Platform != "github" && req.Platform != "gitlab" && req.Platform != "gitea" {
		writeError(w, http.StatusBadRequest, "INVALID_PLATFORM", "platform 必须是 github、gitlab 或 gitea")
		return
	}
	if req.GPGEnabled {
		if msg, ok := validateArmoredKey(req.GPGPrivateKey); !ok {
			writeError(w, http.StatusBadRequest, "INVALID_GPG_KEY", msg)
			return
		}
	}

	tok, err := h.svc.Create(
		req.Name, req.Platform, req.BaseURL, req.Token,
		req.GitUserName, req.GitUserEmail,
		req.GPGPrivateKey, req.GPGPassphrase, req.GPGEnabled,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, tok.Redact())
}

// Delete removes a platform token.
// DELETE /api/settings/tokens/{id}
func (h *PlatformHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Update edits an existing platform token's metadata + optional Git
// identity + optional GPG material. All secret-bearing fields are
// tri-state from the caller's perspective:
//   - new_token        "" = keep existing PAT
//   - gpg_private_key  "" = keep existing key ciphertext
//   - gpg_passphrase   "" = keep existing passphrase ciphertext
//   - clear_gpg        true = wipe all GPG material + disable (overrides
//                      the "empty = no change" rule above for GPG only)
//   - gpg_enabled      absent = keep existing flag, true/false = set
// Returns the updated row, redacted.
//
// PUT /api/settings/tokens/{id}
//   body: {name, base_url, git_user_name?, git_user_email?, new_token?,
//          gpg_enabled?, gpg_private_key?, gpg_passphrase?, clear_gpg?}
func (h *PlatformHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name          string `json:"name"`
		BaseURL       string `json:"base_url"`
		GitUserName   string `json:"git_user_name"`
		GitUserEmail  string `json:"git_user_email"`
		NewToken      string `json:"new_token"`
		GPGEnabled    *bool  `json:"gpg_enabled"`
		GPGPrivateKey string `json:"gpg_private_key"`
		GPGPassphrase string `json:"gpg_passphrase"`
		ClearGPG      bool   `json:"clear_gpg"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "MISSING_FIELDS", "name 不能为空")
		return
	}
	// Only validate the armored block when the user is actually replacing
	// the key. clear_gpg=true intentionally ignores any new key material.
	if !req.ClearGPG && req.GPGPrivateKey != "" {
		if msg, ok := validateArmoredKey(req.GPGPrivateKey); !ok {
			writeError(w, http.StatusBadRequest, "INVALID_GPG_KEY", msg)
			return
		}
	}

	if err := h.svc.Update(
		id, req.Name, req.BaseURL, req.GitUserName, req.GitUserEmail, req.NewToken,
		req.GPGPrivateKey, req.GPGPassphrase, req.GPGEnabled, req.ClearGPG,
	); err != nil {
		if strings.Contains(err.Error(), "token not found") {
			writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	tok, err := h.svc.Get(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tok.Redact())
}

// validateArmoredKey returns (message, ok=false) when the supplied block
// doesn't look like a gpg --armor --export-secret-keys output. It is a
// cheap first-line check — the actual import happens on the Agent
// server in a later step where a real gpg binary will reject malformed
// blocks authoritatively.
func validateArmoredKey(armored string) (string, bool) {
	if armored == "" {
		return "请粘贴 ASCII-armored 私钥（gpg --armor --export-secret-keys 的输出）", false
	}
	if !strings.Contains(armored, "-----BEGIN PGP PRIVATE KEY BLOCK-----") {
		return "GPG 私钥格式不正确，请粘贴 ASCII-armored 私钥（gpg --armor --export-secret-keys 的输出）", false
	}
	return "", true
}
