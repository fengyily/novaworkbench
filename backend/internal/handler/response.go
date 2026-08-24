package handler

import (
	"encoding/json"
	"net/http"
)

type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   *APIError   `json:"error,omitempty"`
}

type APIError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(APIResponse{Success: true, Data: data})
}

// decodeJSON reads and decodes a JSON request body into dst. It mirrors the
// inline pattern used across handlers so error messages stay consistent.
func decodeJSON(r *http.Request, dst any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}

func writeError(w http.ResponseWriter, code int, errCode string, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(APIResponse{
		Success: false,
		Error: &APIError{
			Code:    errCode,
			Message: msg,
		},
	})
}

// writeErrorSuggestion is writeError with a user-facing suggestion (shown by
// the frontend alongside the error message). Use for recoverable conflicts
// like "cannot delete the active config".
func writeErrorSuggestion(w http.ResponseWriter, code int, errCode, msg, suggestion string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(APIResponse{
		Success: false,
		Error: &APIError{
			Code:       errCode,
			Message:    msg,
			Suggestion: suggestion,
		},
	})
}

// writeSSEHeaders sets the response headers required for an SSE stream.
// X-Accel-Buffering: no disables nginx proxy buffering so each flushed frame
// is forwarded to the client immediately instead of being held in nginx's
// buffer (which manifested as delayed/batched output on desktop and missing
// output on mobile when the upstream buffer was never flushed before the
// connection was torn down by an intermediate proxy).
func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}
