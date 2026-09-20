package service

import (
	"sync"
)

// SubTaskEventHub is a process-local pub/sub keyed by requirement_id. The
// SubTaskService notifies after every status-affecting write
// (MarkRunning / Finish / FinishForSession / MarkStopped) so a per-
// requirement SSE endpoint can fan out a "refresh" signal to subscribers.
// The payload intentionally stays minimal — the frontend re-fetches the
// full sub_tasks list to stay consistent with any column it depends on,
// rather than us inventing a delta protocol.
//
// Per-subscriber channel buffer is 4; the producer drops notifications on a
// full channel (the 5s poll on the frontend is the safety net). We never
// block the writer path on the subscriber's network speed.
type SubTaskEventHub struct {
	mu   sync.RWMutex
	subs map[string]map[chan struct{}]struct{} // reqID -> set of subscribers
}

func NewSubTaskEventHub() *SubTaskEventHub {
	return &SubTaskEventHub{subs: map[string]map[chan struct{}]struct{}{}}
}

// Subscribe registers a buffered channel for reqID. Returns the channel —
// callers select on it and call Unsubscribe when their context cancels.
func (h *SubTaskEventHub) Subscribe(reqID string) chan struct{} {
	ch := make(chan struct{}, 4)
	h.mu.Lock()
	if h.subs[reqID] == nil {
		h.subs[reqID] = map[chan struct{}]struct{}{}
	}
	h.subs[reqID][ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe removes ch from reqID's subscriber set. Safe to call
// multiple times — non-registered channels are a no-op.
func (h *SubTaskEventHub) Unsubscribe(reqID string, ch chan struct{}) {
	h.mu.Lock()
	if s := h.subs[reqID]; s != nil {
		delete(s, ch)
		if len(s) == 0 {
			delete(h.subs, reqID)
		}
	}
	h.mu.Unlock()
}

// Notify fans a single change signal to all subscribers of reqID. Each
// subscriber's channel buffer is 4 deep — a slower reader drops events
// rather than block the writer. This matches the frontend's
// loadList()-on-event strategy where multiple sub-task transitions can
// collapse into a single re-fetch.
func (h *SubTaskEventHub) Notify(reqID string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[reqID] {
		select {
		case ch <- struct{}{}:
		default:
			// Drop: subscriber is behind. Their 5s polling timer will catch up.
		}
	}
}

// NotifyAll fans a signal to every requirement's subscribers — used by the
// boot recovery path when many rows get re-claimed at once. Not currently
// called from the sub-task hot path; kept for future batch scenarios.
func (h *SubTaskEventHub) NotifyAll() {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, subs := range h.subs {
		for ch := range subs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// Stats returns (reqID, subscriber-count) pairs for diagnostics / logging.
// The values are a snapshot under the read lock; the slice is safe to
// iterate even after the lock is released.
func (h *SubTaskEventHub) Stats() []SubTaskHubStat {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]SubTaskHubStat, 0, len(h.subs))
	for reqID, subs := range h.subs {
		out = append(out, SubTaskHubStat{ReqID: reqID, Subscribers: len(subs)})
	}
	return out
}

type SubTaskHubStat struct {
	ReqID       string
	Subscribers int
}