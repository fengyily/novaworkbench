package handler

import (
	"net/http"
)

// apiFailure is a validation failure surfaced as an HTTP error by the
// wizard handler and as a task-failure message by the scheduler — same
// field set, two paths share one validation source. The Status/Code/Msg
// triples are exactly the (status, code, message) that writeError writes
// to the JSON response; refactor the prepareXxx methods to return *apiFailure
// so both HTTP and scheduler paths can emit the same error verbatim.
type apiFailure struct {
	Status int    // 400 / 404 / 409 / 500
	Code   string // NO_SESSION / UNANCHORED_SESSION / WORKTREE_FAILED / NOT_FOUND / INVALID
	Msg    string
}

func (e *apiFailure) Error() string { return e.Msg }

// fail is a small constructor to keep call sites readable.
func fail(status int, code, msg string) *apiFailure {
	return &apiFailure{Status: status, Code: code, Msg: msg}
}

// writeIfAPIError sends an apiFailure as an HTTP error response. Returns
// true when err was non-nil and the response has been written (the caller
// MUST then return without further writes).
func writeIfAPIError(w http.ResponseWriter, err *apiFailure) bool {
	if err == nil {
		return false
	}
	writeError(w, err.Status, err.Code, err.Msg)
	return true
}