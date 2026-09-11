package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type LogLine struct {
	Type    string `json:"type"` // "tool_call" | "tool_result" | "message" | "error" | "done"
	Content string `json:"content"`
	At      int64  `json:"at,omitempty"` // Unix ms; set automatically by Job.Append / sseSink.emit / sendStatus
}

type JobStatus string

const (
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobError   JobStatus = "error"
)

type Job struct {
	ID            string    `json:"job_id"`
	RequirementID string    `json:"requirement_id"`
	Status        JobStatus `json:"status"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	ExitCode      int       `json:"exit_code"`
	Log           []LogLine `json:"log"`
	// Model is the effective model the claude CLI ran with (display value —
	// may be the "默认模型" literal). Set by the handler that owns the job so
	// GetJob + the job_done SSE frame can surface it without a DB round-trip
	// while the job is still in the in-memory ring buffer.
	Model string `json:"model"`
	// Type is a free-form label set by the wizard handler right after Create
	// (e.g. "analyst_chat" | "architect_design" | "apply_doc" |
	// "start_coding" | "adjust_coding" | "continue_coding"). Survives in
	// the ring buffer only — old in-flight jobs from a pre-upgrade process
	// may have an empty Type; that's accepted (ActiveJobs consumers only
	// care about requirement_id). Used by the global active-jobs endpoint
	// to surface the wizard pipeline state to list / detail pages.
	Type string `json:"type,omitempty"`
	mu    sync.RWMutex
	subs  []chan LogLine
	// cmd / cancel wire the running subprocess into the job so a handler-side
	// Stop path can interrupt it (exec.CommandContext串起 SIGTERM → WaitDelay
	// → SIGKILL — see gateway.go around GenerateCode). Both fields stay
	// nil for jobs that never spawned a claude process (e.g. recovered
	// jobs that had their subprocess killed by a backend restart); in
	// that case Cancel() is a no-op and the Stop handler returns 409
	// JOB_GONE so the caller can fall back to Continue/Redo.
	cmd    *exec.Cmd          `json:"-"`
	cancel context.CancelFunc `json:"-"`
	// lineCarry holds an unfinished line between successive Write calls so a
	// streamed stdout doesn't get split mid-line. Access only via Write/finish.
	lineCarry string
}

// SetModel records the effective model on the job so subscribers + snapshots
// can surface it. Safe to call once, before the job finishes.
func (j *Job) SetModel(model string) {
	j.mu.Lock()
	j.Model = model
	j.mu.Unlock()
}

// SetType records the wizard pipeline label on the job
// ("analyst_chat" | "architect_design" | "apply_doc" | "start_coding" |
// "adjust_coding" | "continue_coding") right after Create. Mirrors the
// SetModel pattern so the ActiveJobs reader (which RLock-gates access)
// sees a consistent value without racing the handler's assignment.
func (j *Job) SetType(t string) {
	j.mu.Lock()
	j.Type = t
	j.mu.Unlock()
}

// SetCmd stores the running subprocess's *exec.Cmd and its cancellation
// function on the job so a later Stop path can interrupt it. Safe to call
// once after Create; overwrites silently if called twice (the second spawn
// wins, which matches the SubTaskRunner's "cmd is the live one" invariant).
//
// Both args may be nil — Cancel() then becomes a no-op. The wizard runner
// always passes non-nil once GenerateCode has returned, so the no-op branch
// only matters for recovered jobs whose process was already reaped.
func (j *Job) SetCmd(cmd *exec.Cmd, cancel context.CancelFunc) {
	j.mu.Lock()
	j.cmd = cmd
	j.cancel = cancel
	j.mu.Unlock()
}

// Cancel triggers the stored cancel func (typically backed by an
// exec.CommandContext whose WaitDelay already chains SIGTERM → 5s → SIGKILL)
// so the running subprocess gets interrupted. Idempotent: nil cancel is
// silently skipped so callers don't have to nil-check before invoking.
//
// Reads cancel under RLock and releases before invoking the func to avoid
// re-entering the mutex — CancelFunc implementations themselves may end up
// touching the job (via the deferred Finish call in the runner), and holding
// j.mu across cancel() would deadlock.
func (j *Job) Cancel() {
	j.mu.RLock()
	cancel := j.cancel
	j.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

func (j *Job) Append(line LogLine) {
	if line.At == 0 {
		line.At = time.Now().UnixMilli()
	}
	j.mu.Lock()
	j.Log = append(j.Log, line)
	subs := j.subs
	j.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- line:
		default:
		}
	}
}

// Write splits p into lines (keeping a small carry-over buffer for partial
// lines) and emits each non-empty line as a `message` LogLine. It lets *Job
// satisfy io.Writer so SSH and preflight code can stream stdout/stderr
// straight into a job's log without a per-line adapter.
func (j *Job) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	pending := string(p)
	for {
		i := strings.IndexByte(pending, '\n')
		if i < 0 {
			j.lineCarry = j.lineCarry + pending
			break
		}
		line := pending[:i]
		if j.lineCarry != "" {
			line = j.lineCarry + line
			j.lineCarry = ""
		}
		if line != "" {
			j.Append(LogLine{Type: "message", Content: line})
		}
		pending = pending[i+1:]
	}
	return len(p), nil
}

func (j *Job) Finish(exitCode int, status JobStatus) {
	j.mu.Lock()
	j.ExitCode = exitCode
	j.Status = status
	j.FinishedAt = time.Now()
	subs := j.subs
	j.mu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
}

// Subscribe returns a channel that receives new log lines as they are appended,
// pre-seeded with all lines already written. The channel is closed when the job finishes.
// If the job is already done, the channel is returned closed after draining existing lines.
func (j *Job) Subscribe() (<-chan LogLine, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	existing := make([]LogLine, len(j.Log))
	copy(existing, j.Log)
	ch := make(chan LogLine, 256)
	if j.Status == JobRunning {
		j.subs = append(j.subs, ch)
	}
	// Send existing lines into the buffered channel before returning.
	for _, l := range existing {
		ch <- l
	}
	if j.Status != JobRunning {
		close(ch)
	}
	return ch, len(existing)
}

// Unsubscribe removes a subscriber channel (called on client disconnect).
func (j *Job) Unsubscribe(ch <-chan LogLine) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, s := range j.subs {
		if s == ch {
			j.subs = append(j.subs[:i], j.subs[i+1:]...)
			return
		}
	}
}

// Snapshot returns a consistent copy of the job's log, status, and exit code.
func (j *Job) Snapshot() (log []LogLine, status JobStatus, exitCode int) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	log = make([]LogLine, len(j.Log))
	copy(log, j.Log)
	return log, j.Status, j.ExitCode
}

// JobStore holds the most recent cap jobs in a ring buffer.
type JobStore struct {
	mu   sync.Mutex
	ring []*Job
	cap  int
	next int
	size int
}

func NewJobStore(cap int) *JobStore {
	return &JobStore{cap: cap, ring: make([]*Job, cap)}
}

func (s *JobStore) Create(reqID string) *Job {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	job := &Job{
		ID:            "job_" + hex.EncodeToString(b),
		RequirementID: reqID,
		Status:        JobRunning,
		StartedAt:     time.Now(),
	}
	s.mu.Lock()
	s.ring[s.next] = job
	s.next = (s.next + 1) % s.cap
	if s.size < s.cap {
		s.size++
	}
	s.mu.Unlock()
	return job
}

func (s *JobStore) Get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.ring {
		if j != nil && j.ID == id {
			return j, true
		}
	}
	return nil, false
}

// Live reports whether the job with the given id is both present in the ring
// buffer AND still running. A requirement can carry a job-id pointer
// (design_job_id / analysis_job_id / apply_job_id) that outlives the in-memory
// job: the server restarted, the ring buffer evicted it, or the goroutine
// finished (and should have cleared the pointer but the process died first).
// Callers use this to self-heal those stale pointers — a non-Live id means no
// work is happening, so the UI must not show a spinner nor hide the retry
// button behind it.
func (s *JobStore) Live(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.ring {
		if j != nil && j.ID == id {
			j.mu.RLock()
			running := j.Status == JobRunning
			j.mu.RUnlock()
			return running
		}
	}
	return false
}

// ActiveJob is the minimal per-job projection surfaced by ActiveJobs so
// callers (frontend list / detail pages polling every 5s) get just enough
// to badge "Claude 工作中" without paying for the full log history.
type ActiveJob struct {
	JobID         string    `json:"job_id"`
	RequirementID string    `json:"requirement_id"`
	Status        JobStatus `json:"status"`
	Type          string    `json:"type"`
}

// ActiveJobs returns a snapshot of all currently-running jobs across all
// requirements. Used by list / detail pages to badge "Claude 工作中"
// without N+1 polling per-requirement *_job_id columns. Cost is O(cap) —
// the ring buffer is bounded (default 50) and a single RLock per entry is
// enough to read Status / Type without blocking writers. Once a job has
// Finish()ed it's no longer running and is filtered out, so the returned
// slice only ever contains in-flight work.
func (s *JobStore) ActiveJobs() []ActiveJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ActiveJob, 0, s.size)
	for _, j := range s.ring {
		if j == nil {
			continue
		}
		j.mu.RLock()
		running := j.Status == JobRunning
		t := j.Type
		j.mu.RUnlock()
		if !running {
			continue
		}
		out = append(out, ActiveJob{
			JobID:         j.ID,
			RequirementID: j.RequirementID,
			Status:        JobRunning,
			Type:          t,
		})
	}
	return out
}
