package store

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type LogLine struct {
	Type    string `json:"type"`    // "tool_call" | "tool_result" | "message" | "error" | "done"
	Content string `json:"content"`
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
	mu            sync.RWMutex
	subs          []chan LogLine
}

func (j *Job) Append(line LogLine) {
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
