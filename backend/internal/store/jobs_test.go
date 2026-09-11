package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Regression guards for the "frontend stuck forever after the task finished"
// incident on Agent-server runs. Two independent defects in this file fed it:
//
//  1. Subscribe pre-seeded a fixed 256-line channel WHILE holding j.mu. A job
//     with more than 256 logged lines therefore blocked forever on the first
//     send, holding the write lock — so Append/Finish/Snapshot for that job
//     (and, via JobStore.mu, every other job) queued behind it. The live SSE
//     stream AND the refresh-time snapshot both hung with no error anywhere.
//  2. Finish was not idempotent and left j.subs populated, so a late Append
//     hit a closed channel and panicked. The coding goroutines have no
//     recover, so that panic exited the whole nova process.
//
// These tests are pure Go: no DB, no claude CLI, no filesystem.

// mustNotPanic runs f and fails the test if it panics.
func mustNotPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	f()
}

// nextLine reads one line from ch, failing the test if nothing arrives within d.
// open=false means the channel was closed.
func nextLine(t *testing.T, ch <-chan LogLine, d time.Duration, what string) (LogLine, bool) {
	t.Helper()
	select {
	case l, open := <-ch:
		return l, open
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for %s", d, what)
		return LogLine{}, false
	}
}

// subscribeAsync calls Subscribe in a goroutine so a regression turns into a
// test failure instead of a silent hang (the bug held j.mu forever, which would
// otherwise only surface as the go test -timeout panic).
func subscribeAsync(job *Job) (<-chan LogLine, int, bool) {
	type subResult struct {
		ch <-chan LogLine
		n  int
	}
	resc := make(chan subResult, 1)
	go func() {
		ch, n := job.Subscribe()
		resc <- subResult{ch: ch, n: n}
	}()
	select {
	case res := <-resc:
		return res.ch, res.n, true
	case <-time.After(2 * time.Second):
		return nil, 0, false
	}
}

// drainReplayAndExpectClosed consumes exactly n replayed lines and then asserts
// the channel is closed. The subscriber's job must already have finished.
func drainReplayAndExpectClosed(t *testing.T, ch <-chan LogLine, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, open := nextLine(t, ch, 2*time.Second, "replayed line"); !open {
			t.Fatalf("channel closed after %d replayed lines, want %d", i, n)
		}
	}
	if _, open := nextLine(t, ch, 2*time.Second, "channel close"); open {
		t.Fatal("channel stayed open after the replay was drained; the job is finished, so it must be closed")
	}
}

func TestSubscribeWithLargeLogDoesNotBlock(t *testing.T) {
	jobs := NewJobStore(50)
	job := jobs.Create("req_test")
	const lines = 500 // > the old fixed 256-line replay buffer
	for i := 0; i < lines; i++ {
		job.Append(LogLine{Type: "message", Content: fmt.Sprintf("line %d", i)})
	}

	ch, n, ok := subscribeAsync(job)
	if !ok {
		t.Fatal("Subscribe blocked for >2s on a 500-line job: the replay pre-seed ran while j.mu was held, so every Append/Finish/Snapshot for this job (and Snapshot for every other job, via JobStore.mu) queued behind the blocked send")
	}
	if n != lines {
		t.Fatalf("Subscribe reported %d existing lines, want %d", n, lines)
	}
	if got := len(ch); got != lines {
		t.Fatalf("replay buffered %d lines, want %d", got, lines)
	}

	job.Finish(0, JobDone)

	seen := 0
	for {
		l, open := nextLine(t, ch, 2*time.Second, "replayed line or channel close")
		if !open {
			if seen != lines {
				t.Fatalf("channel closed after %d replayed lines, want %d", seen, lines)
			}
			return
		}
		if want := fmt.Sprintf("line %d", seen); l.Content != want {
			t.Fatalf("replayed line %d = %q, want %q", seen, l.Content, want)
		}
		seen++
	}
}

func TestSubscribeConcurrentWithAppend(t *testing.T) {
	jobs := NewJobStore(50)
	job := jobs.Create("req_test")
	const replayLines = 400
	for i := 0; i < replayLines; i++ {
		job.Append(LogLine{Type: "message", Content: fmt.Sprintf("replay %d", i)})
	}

	ch, n, ok := subscribeAsync(job)
	if !ok {
		t.Fatal("Subscribe blocked on a 400-line job (replay deadlock)")
	}
	if n != replayLines {
		t.Fatalf("Subscribe reported %d existing lines, want %d", n, replayLines)
	}

	// Writers keep running while a subscriber is attached. replayLines + live
	// fits inside the replay+256 buffer, so nothing may be dropped.
	const writers, perWriter = 10, 10
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				job.Append(LogLine{Type: "message", Content: fmt.Sprintf("live %d-%d", w, i)})
			}
		}(w)
	}
	writersDone := make(chan struct{})
	go func() { wg.Wait(); close(writersDone) }()
	select {
	case <-writersDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Append deadlocked while a subscriber was attached")
	}

	// The job is still running: draining must not close the channel.
	if _, status, _ := job.Snapshot(); status != JobRunning {
		t.Fatalf("job status = %q before Finish, want %q", status, JobRunning)
	}

	total := replayLines + writers*perWriter
	for seen := 0; seen < total; seen++ {
		if _, open := nextLine(t, ch, 2*time.Second, "replayed + live lines"); !open {
			t.Fatalf("channel closed after %d lines, want %d (job is still running)", seen, total)
		}
	}

	job.Finish(0, JobDone)
	if _, open := nextLine(t, ch, 2*time.Second, "channel close after Finish"); open {
		t.Fatal("subscriber channel still open after Finish")
	}
}

func TestSubscribeAfterFinishReturnsClosedChannel(t *testing.T) {
	jobs := NewJobStore(50)
	job := jobs.Create("req_test")
	const lines = 300 // > 256: the old fixed buffer deadlocked on this path too
	for i := 0; i < lines; i++ {
		job.Append(LogLine{Type: "message", Content: fmt.Sprintf("line %d", i)})
	}
	job.Finish(0, JobDone)

	ch, n, ok := subscribeAsync(job)
	if !ok {
		t.Fatal("Subscribe blocked for >2s on a finished 300-line job")
	}
	if n != lines {
		t.Fatalf("Subscribe reported %d existing lines, want %d", n, lines)
	}
	if got := len(ch); got != lines {
		t.Fatalf("replay buffered %d lines, want %d", got, lines)
	}
	drainReplayAndExpectClosed(t, ch, lines)
}

func TestFinishIsIdempotent(t *testing.T) {
	jobs := NewJobStore(50)
	job := jobs.Create("req_test")
	job.Append(LogLine{Type: "message", Content: "hello"})

	ch, n, ok := subscribeAsync(job)
	if !ok {
		t.Fatal("Subscribe blocked on a running 1-line job")
	}
	if n != 1 {
		t.Fatalf("Subscribe reported %d existing lines, want 1", n)
	}

	job.Finish(0, JobDone)
	drainReplayAndExpectClosed(t, ch, n)

	// Before the fix this closed the same channels a second time → panic. The
	// stop watchdog (wizard_subtask.go) and the coding goroutines' terminal
	// fallback can both race the normal path, so a second Finish must be a
	// no-op that leaves the first terminal state intact.
	mustNotPanic(t, "second Finish", func() { job.Finish(1, JobError) })

	_, status, exitCode := job.Snapshot()
	if status != JobDone || exitCode != 0 {
		t.Fatalf("after second Finish: status=%q exit=%d, want done/0 (first writer wins)", status, exitCode)
	}
}

func TestAppendAfterFinishDoesNotPanic(t *testing.T) {
	jobs := NewJobStore(50)
	job := jobs.Create("req_test")
	job.Append(LogLine{Type: "message", Content: "hello"})

	// Attach a subscriber while the job is still running: that is what
	// registers its channel in j.subs, i.e. the exact channel a late Append
	// used to send into after Finish had closed it.
	ch, n, ok := subscribeAsync(job)
	if !ok {
		t.Fatal("Subscribe blocked on a running 1-line job")
	}

	job.Finish(0, JobDone)
	drainReplayAndExpectClosed(t, ch, n)

	// A lingering SSH/SFTP writer goroutine appending after the job finished
	// used to hit that closed channel → panic. The coding goroutines have no
	// recover, so the panic exited the whole nova process. Post-terminal lines
	// are dropped on purpose: every terminal done/result line is appended
	// BEFORE Finish (wizard_coding.go, sub_task_runner.go).
	mustNotPanic(t, "Append after Finish", func() {
		job.Append(LogLine{Type: "message", Content: "late line"})
		job.Append(LogLine{Type: "error", Content: "late error"})
	})

	// Subscribing to an already-finished job still replays the log and returns
	// a closed channel.
	ch2, n2, ok := subscribeAsync(job)
	if !ok {
		t.Fatal("Subscribe blocked on a finished job")
	}
	drainReplayAndExpectClosed(t, ch2, n2)

	if _, status, _ := job.Snapshot(); status != JobDone {
		t.Fatalf("job status = %q, want %q", status, JobDone)
	}
}
