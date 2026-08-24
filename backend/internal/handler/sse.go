package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/novaworkbench/backend/internal/store"
)

// sseHeartbeatInterval is how often the JobStore SSE pump emits a keepalive
// comment frame. Intermediate reverse proxies default to a ~60s read timeout
// (nginx proxy_read_timeout, 60s out of the box) and claude design/coding jobs
// have silent thinking gaps that easily exceed that. Without a heartbeat a
// proxy tears the stream down mid-run — manifesting on an HTTP/2 client leg as
// net::ERR_HTTP2_PROTOCOL_ERROR 200 (OK), or as a silent drop on HTTP/1.1 —
// discarding the entire SSE response and leaving the UI with no live output.
//
// The comment frame (": ping\n\n") is invisible to the browser's SSE parser: a
// line starting with ':' is an SSE comment per the spec, and the fetch-based
// parser in frontend/src/api/stream.ts only decodes `data:` lines (the stray
// ": ping" fails JSON.parse and is silently skipped), so this produces no
// client-visible event — it only keeps every proxy's idle timer alive.
const sseHeartbeatInterval = 15 * time.Second

// streamJobSSE is the shared JobStore→SSE pump used by every /jobs/{id}/stream
// handler (wizard, preflight, review, report). It writes the SSE headers,
// subscribes to the job (which pre-seeds the channel with existing log lines so
// a reconnecting client replays history), then pumps live lines to the client
// until the job finishes or the client disconnects — flushing a heartbeat
// comment frame every sseHeartbeatInterval so no proxy idle timer fires during
// claude's silent thinking gaps.
//
// doneFrame builds the terminal job_done payload so each handler can carry its
// own fields (wizard adds started/finished/duration; review adds the model).
func streamJobSSE(w http.ResponseWriter, r *http.Request, job *store.Job, doneFrame func(status store.JobStatus, exitCode int) []byte) {
	rc := http.NewResponseController(w)
	writeSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	ch, _ := job.Subscribe()
	defer job.Unsubscribe(ch)

	ticker := time.NewTicker(sseHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// Keepalive comment frame — see sseHeartbeatInterval doc.
			fmt.Fprint(w, ": ping\n\n")
			rc.Flush()
		case line, open := <-ch:
			if !open {
				_, status, exitCode := job.Snapshot()
				payload := doneFrame(status, exitCode)
				fmt.Fprintf(w, "data: %s\n\n", payload)
				rc.Flush()
				return
			}
			data, _ := json.Marshal(line)
			fmt.Fprintf(w, "data: %s\n\n", data)
			rc.Flush()
		}
	}
}
