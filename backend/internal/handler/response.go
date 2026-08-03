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
