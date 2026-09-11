package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/store"
)

// HTTP-layer regression guards for the incident this fix addresses:
//
//	"Agent Server 任务完成后前端永久卡住，刷新页面也无法恢复"
//
// The store-level guards live in store/jobs_test.go and pin the two defects
// themselves (the blocking Subscribe replay under j.mu, and the non-idempotent
// Finish). These tests pin the *user-visible* symptom one layer up, because
// that is what the report described:
//
//   - GET /api/wizard/jobs/{id}  is the refresh-time snapshot, and the ONLY
//     reconnect entry point the frontend has. It used to hang outright (not
//     "return running") once a job had logged more than 256 lines, which is
//     exactly why refreshing did not help.
//   - GET /api/wizard/active-jobs is polled every 5s by the list / detail
//     pages. It reads every job under JobStore.mu, so it hung behind the same
//     blocked writer and could wedge unrelated jobs' Get / Create too.
//   - GET /api/wizard/jobs/{id}/stream must replay the full history and then
//     emit the terminal job_done frame; that frame is what clears the
//     frontend's `coding` flag and reveals the "开发完成" button.
//
// The snapshot / poll tests attach a stream subscriber FIRST (attachStream):
// that is the trigger that armed the deadlock, and reproducing the user's
// sequence — a new subscription, then a refresh — is what makes these fail on
// the broken code instead of passing vacuously.
//
// No DB, no claude CLI, no SSH: a real JobStore + the real routes over httptest.

// largeLogLines exceeds the old fixed 256-line Subscribe replay buffer, which
// is the threshold that triggered the deadlock.
const largeLogLines = 500

// frameDeadline bounds every read in these tests so a reintroduced hang fails
// loudly in seconds instead of showing up as a go test -timeout panic.
const frameDeadline = 5 * time.Second

// newJobsTestServer wires the real wizard job routes to a real JobStore. Every
// other WizardHandler dependency is left nil on purpose: these three routes only
// touch h.jobs (GetJob reaches for h.jobLogSvc solely when the job is missing
// from the ring buffer, which never happens here).
func newJobsTestServer() (*httptest.Server, *store.JobStore) {
	jobs := store.NewJobStore(50)
	h := NewWizardHandler(nil, nil, nil, nil, nil, jobs, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/wizard/jobs/{id}", h.GetJob)
	mux.HandleFunc("GET /api/wizard/jobs/{id}/stream", h.StreamJob)
	mux.HandleFunc("GET /api/wizard/active-jobs", h.GetActiveJobs)
	return httptest.NewServer(mux), jobs
}

// seedLargeLog creates a running job with largeLogLines lines logged.
func seedLargeLog(jobs *store.JobStore) *store.Job {
	job := jobs.Create("req_test")
	for i := 0; i < largeLogLines; i++ {
		job.Append(store.LogLine{Type: "message", Content: "line " + itoa(i)})
	}
	return job
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// attachStream opens a job SSE stream the way a browser tab / sub-task card does
// and leaves it connected — this is the *trigger*, not the thing under test.
//
// Why the other tests need it: the pre-fix deadlock was armed by a NEW
// subscription (a page refresh, a sub-task card mounting, a tab switch), not by
// the snapshot alone. Subscribe blocked inside its own request goroutine while
// holding j.mu, so it was that goroutine — not the log volume by itself — that
// froze every later Append / Finish / Snapshot. A snapshot issued with no
// subscriber attached therefore still succeeded on the broken code, which is
// why the poison (an attached subscriber) has to come first to reproduce the
// user's "刷新页面也无法恢复".
//
// The returned cancel/close pair disconnects the client. On the broken code that
// cannot unblock a server goroutine wedged inside Subscribe — see closeServer.
func attachStream(t *testing.T, srv *httptest.Server, jobID string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/wizard/jobs/"+jobID+"/stream", nil)
	if err != nil {
		cancel()
		t.Fatalf("build stream request: %v", err)
	}
	// The SSE handler flushes headers before it subscribes, so Do returning
	// means the handler is about to enter Subscribe. Drain in the background:
	// the replay is ~500 lines and must never block on a full socket buffer.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("attach stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		resp.Body.Close()
		t.Fatalf("attach stream status = %d, want 200", resp.StatusCode)
	}
	go func() { _, _ = io.Copy(io.Discard, resp.Body) }()
	// Give the server goroutine time to reach Subscribe and block there. There
	// is no exported hook to synchronise on, and the window is a few
	// instructions (headers were already flushed); the callers additionally
	// retry their request, so a slow scheduler cannot hide the regression.
	time.Sleep(200 * time.Millisecond)
	return resp, cancel
}

// closeServer tears the test server down without hanging the suite when a
// handler goroutine is wedged inside Subscribe (the pre-fix failure mode: the
// server can never drain that request, so Close blocks forever).
func closeServer(srv *httptest.Server) {
	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Deliberately leak the wedged handler goroutine: the test binary is
		// about to report the failure and exit.
		srv.CloseClientConnections()
	}
}

// readSSEFrameTypes consumes `data:` frames until the stream ends, returning the
// frame `type` values in order. A frame that never arrives fails the test after
// frameDeadline rather than hanging the package.
func readSSEFrameTypes(t *testing.T, resp *http.Response) []string {
	t.Helper()
	frames := make(chan string, 1024)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			// Skip the ": ping" keepalive comments and frame separators.
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &head); err != nil {
				continue
			}
			frames <- head.Type
		}
	}()

	var out []string
	deadline := time.After(frameDeadline * 4)
	for {
		select {
		case f, open := <-frames:
			if !open {
				return out
			}
			out = append(out, f)
			if f == "job_done" {
				// The handler returns right after the terminal frame.
				return out
			}
		case <-deadline:
			t.Fatalf("SSE stream stalled after %d frames", len(out))
			return out
		}
	}
}

// TestJobSnapshotReturnsImmediatelyWithLargeLog is the direct check for the
// "刷新页面也无法恢复" half of the report: with a subscriber attached to a
// >256-line running job — the state a refresh walks into — the snapshot
// endpoint must still answer at once.
func TestJobSnapshotReturnsImmediatelyWithLargeLog(t *testing.T) {
	srv, jobs := newJobsTestServer()
	defer closeServer(srv)
	job := seedLargeLog(jobs)

	resp, cancel := attachStream(t, srv, job.ID)
	defer func() {
		cancel()
		resp.Body.Close()
	}()

	// Retry across the attach window; every attempt must come back promptly.
	// Before the fix the first attempt after the subscriber blocked would hang
	// until the client timeout, which is precisely the refresh that never
	// completed for the user.
	client := &http.Client{Timeout: frameDeadline}
	for attempt := 0; attempt < 6; attempt++ {
		start := time.Now()
		r, err := client.Get(srv.URL + "/api/wizard/jobs/" + job.ID)
		if err != nil {
			t.Fatalf("snapshot attempt %d failed after %s (refresh-time read path blocked behind a subscriber?): %v",
				attempt, time.Since(start).Round(time.Millisecond), err)
		}
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			t.Fatalf("snapshot status = %d, want 200", r.StatusCode)
		}

		var env struct {
			Success bool `json:"success"`
			Data    struct {
				JobID  string          `json:"job_id"`
				Status string          `json:"status"`
				Log    []store.LogLine `json:"log"`
			} `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			r.Body.Close()
			t.Fatalf("decode snapshot: %v", err)
		}
		r.Body.Close()
		if !env.Success {
			r.Body.Close()
			t.Fatal("snapshot envelope reported success=false")
		}
		if env.Data.JobID != job.ID {
			t.Fatalf("snapshot job_id = %q, want %q", env.Data.JobID, job.ID)
		}
		if env.Data.Status != string(store.JobRunning) {
			t.Fatalf("snapshot status = %q, want %q", env.Data.Status, store.JobRunning)
		}
		if len(env.Data.Log) != largeLogLines {
			t.Fatalf("snapshot log has %d lines, want %d", len(env.Data.Log), largeLogLines)
		}
		time.Sleep(150 * time.Millisecond)
	}
	job.Finish(0, store.JobDone)
}

// TestActiveJobsReturnsImmediatelyWithLargeLog guards the 5s list/detail poll,
// which shared the same blocked critical section (and could wedge other jobs,
// since it reads every job while holding JobStore.mu).
func TestActiveJobsReturnsImmediatelyWithLargeLog(t *testing.T) {
	srv, jobs := newJobsTestServer()
	defer closeServer(srv)
	job := seedLargeLog(jobs)

	resp, cancel := attachStream(t, srv, job.ID)
	defer func() {
		cancel()
		resp.Body.Close()
	}()

	client := &http.Client{Timeout: frameDeadline}
	for attempt := 0; attempt < 6; attempt++ {
		start := time.Now()
		r, err := client.Get(srv.URL + "/api/wizard/active-jobs")
		if err != nil {
			t.Fatalf("active-jobs poll attempt %d failed after %s (5s poll blocked behind a subscriber?): %v",
				attempt, time.Since(start).Round(time.Millisecond), err)
		}
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			t.Fatalf("active-jobs status = %d, want 200", r.StatusCode)
		}
		var env struct {
			Data struct {
				Jobs []store.ActiveJob `json:"jobs"`
			} `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			r.Body.Close()
			t.Fatalf("decode active-jobs: %v", err)
		}
		r.Body.Close()
		found := false
		for _, aj := range env.Data.Jobs {
			if aj.JobID == job.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("running job %s missing from active-jobs %+v", job.ID, env.Data.Jobs)
		}
		time.Sleep(150 * time.Millisecond)
	}
	job.Finish(0, store.JobDone)
}

// TestJobStreamReplaysLargeLogThenJobDone pins the terminal frame: the frontend
// only clears `coding` (and thus reveals the "开发完成" button) on job_done.
func TestJobStreamReplaysLargeLogThenJobDone(t *testing.T) {
	srv, jobs := newJobsTestServer()
	defer closeServer(srv)
	job := seedLargeLog(jobs)
	job.Finish(0, store.JobDone)

	client := &http.Client{Timeout: frameDeadline * 2}
	resp, err := client.Get(srv.URL + "/api/wizard/jobs/" + job.ID + "/stream")
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	frames := readSSEFrameTypes(t, resp)
	if len(frames) != largeLogLines+1 {
		t.Fatalf("stream delivered %d frames, want %d replayed lines + 1 job_done", len(frames), largeLogLines)
	}
	if last := frames[len(frames)-1]; last != "job_done" {
		t.Fatalf("last frame = %q, want job_done", last)
	}
	for i, f := range frames[:largeLogLines] {
		if f != "message" {
			t.Fatalf("replayed frame %d = %q, want message", i, f)
		}
	}
}

// TestJobStreamResumesMidRunWithoutDuplicatingHistory covers the reconnect
// contract the frontend relies on (skipFirst): reattaching to a running job
// replays exactly the lines logged so far, then continues live, then closes with
// job_done.
func TestJobStreamResumesMidRunWithoutDuplicatingHistory(t *testing.T) {
	srv, jobs := newJobsTestServer()
	defer closeServer(srv)
	job := seedLargeLog(jobs)

	client := &http.Client{Timeout: frameDeadline * 2}
	resp, err := client.Get(srv.URL + "/api/wizard/jobs/" + job.ID + "/stream")
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()

	// Live continuation: the pump is already subscribed after the replay, so a
	// line appended now must arrive after the replayed history.
	types := make(chan string, 1024)
	go func() {
		defer close(types)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var head struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &head); err != nil {
				continue
			}
			types <- head.Type
		}
	}()

	seen := 0
	deadline := time.After(frameDeadline * 4)
	for seen < largeLogLines {
		select {
		case f, open := <-types:
			if !open {
				t.Fatalf("stream ended after %d replayed frames, want %d", seen, largeLogLines)
			}
			if f != "message" {
				t.Fatalf("replayed frame %d = %q, want message", seen, f)
			}
			seen++
		case <-deadline:
			t.Fatalf("replay stalled after %d frames, want %d", seen, largeLogLines)
		}
	}

	job.Append(store.LogLine{Type: "phase", Content: "live line"})
	select {
	case f, open := <-types:
		if !open {
			t.Fatal("stream closed instead of delivering the live line")
		}
		if f != "phase" {
			t.Fatalf("live frame = %q, want phase", f)
		}
	case <-time.After(frameDeadline):
		t.Fatal("live line never arrived: the subscriber was not attached during replay")
	}

	job.Finish(0, store.JobDone)
	select {
	case f, open := <-types:
		if !open {
			t.Fatal("stream closed without a job_done frame")
		}
		if f != "job_done" {
			t.Fatalf("terminal frame = %q, want job_done", f)
		}
	case <-time.After(frameDeadline):
		t.Fatal("job_done never arrived: the SSE pump did not see the channel close")
	}
}
