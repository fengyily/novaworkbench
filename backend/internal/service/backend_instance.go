package service

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/db"
)

// backendInstanceFreshWindow is how long an instance's heartbeat stays
// authoritative.
//
// The heartbeat is refreshed every 15s, so 60s tolerates four missed
// beats (a GC pause, a slow DB round-trip) before the instance is declared
// dead. It must be comfortably LONGER than the heartbeat interval, or a
// healthy instance would be treated as dead and the other instance would
// take over the dispatch lease — two dispatchers, which is the very thing
// this table exists to prevent.
const backendInstanceFreshWindow = 60 * time.Second

// BackendInstanceService tracks the nova server processes that are pointed
// at the same database.
//
// Why it exists: two backends on one database each run their OWN scheduler
// tick and their OWN in-memory JobStore. The consequences were concrete:
//
//   - the same orchestration batch got dispatched twice (two claude
//     processes for one sub-task → two agents editing one worktree);
//   - a Stop request could not reach the process owned by the OTHER
//     instance, because the JobStore is per-process;
//   - the in-memory JobStore of one instance could not see the other's jobs.
//
// The service implements a simple LEASE: exactly one instance is the
// dispatcher at a time. The lease holder is the OLDEST live instance (by
// started_at) — deterministic, so two instances starting at the same moment
// still agree on one holder without coordination.
//
// An instance registers at boot, refreshes its heartbeat every 15s and
// deletes its row on graceful exit. An instance that was SIGKILLed leaves a
// stale row behind, which is ignored once its heartbeat ages out.
type BackendInstanceService struct {
	db *db.DB

	mu       sync.Mutex
	instance string // this process's instance id
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewBackendInstanceService builds the service. The instance id is
// generated lazily by Register.
func NewBackendInstanceService(database *db.DB) *BackendInstanceService {
	return &BackendInstanceService{db: database}
}

// BackendInstance is one row of the backend_instances table.
type BackendInstance struct {
	ID          string
	PID         int
	Host        string
	StartedAt   time.Time
	HeartbeatAt time.Time
}

// Register records this process as a live instance and starts its heartbeat.
//
// The returned id identifies this process for the lifetime of the service.
// It is also written into the log so an operator can correlate a running
// backend with its row.
//
// Register is idempotent per service instance (a second call re-registers
// under the same id).
func (s *BackendInstanceService) Register(host string) (string, error) {
	if host == "" {
		h, err := os.Hostname()
		if err != nil {
			host = "unknown"
		} else {
			host = h
		}
	}
	s.mu.Lock()
	if s.instance == "" {
		s.instance = fmt.Sprintf("inst_%d_%d", os.Getpid(), time.Now().UnixNano())
	}
	id := s.instance
	s.mu.Unlock()

	now := time.Now()
	upsert := s.db.OnConflict("id", "pid=excluded.pid, host=excluded.host, heartbeat_at=excluded.heartbeat_at")
	_, err := s.db.Exec(`INSERT INTO backend_instances (id, pid, host, started_at, heartbeat_at)
		VALUES (?, ?, ?, ?, ?)`+upsert,
		id, os.Getpid(), host, now, now)
	if err != nil {
		// The upsert suffix is dialect-specific; a driver that rejects it
		// must not leave the instance unregistered. Fall back to a plain
		// delete+insert so the lease table works everywhere.
		if _, derr := s.db.Exec(`DELETE FROM backend_instances WHERE id=?`, id); derr != nil {
			return id, fmt.Errorf("register backend instance: %w (cleanup failed: %v)", err, derr)
		}
		if _, ierr := s.db.Exec(`INSERT INTO backend_instances (id, pid, host, started_at, heartbeat_at) VALUES (?, ?, ?, ?, ?)`,
			id, os.Getpid(), host, now, now); ierr != nil {
			return id, fmt.Errorf("register backend instance: %w", ierr)
		}
	}
	return id, nil
}

// Heartbeat refreshes this instance's liveness stamp.
//
// It is called on a timer; a failure is logged but never fatal — the
// instance simply loses the lease after the fresh window expires, which is
// the safe direction (the other instance takes over instead of both
// dispatching).
func (s *BackendInstanceService) Heartbeat() error {
	s.mu.Lock()
	id := s.instance
	s.mu.Unlock()
	if id == "" {
		return errors.New("backend instance is not registered")
	}
	_, err := s.db.Exec(`UPDATE backend_instances SET heartbeat_at=? WHERE id=?`, time.Now(), id)
	return err
}

// Clear removes this instance's row (graceful shutdown).
//
// It is best-effort: a failure only leaves a stale row behind, which ages
// out of the fresh window and is then ignored.
func (s *BackendInstanceService) Clear() {
	s.mu.Lock()
	id := s.instance
	s.mu.Unlock()
	if id == "" {
		return
	}
	if _, err := s.db.Exec(`DELETE FROM backend_instances WHERE id=?`, id); err != nil {
		log.Printf("[instance] failed to clear registration %s: %v", id, err)
	}
}

// ListLive returns the instances whose heartbeat is still fresh.
//
// A row whose heartbeat is older than the fresh window belongs to a process
// that was killed without cleanup — it is ignored.
func (s *BackendInstanceService) ListLive() ([]BackendInstance, error) {
	cutoff := time.Now().Add(-backendInstanceFreshWindow)
	rows, err := s.db.Query(`SELECT id, pid, host, started_at, heartbeat_at
		FROM backend_instances WHERE heartbeat_at IS NOT NULL AND heartbeat_at >= ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("list live backend instances: %w", err)
	}
	defer rows.Close()
	var out []BackendInstance
	for rows.Next() {
		var b BackendInstance
		var startedAt, heartbeatAt sql.NullTime
		if err := rows.Scan(&b.ID, &b.PID, &b.Host, &startedAt, &heartbeatAt); err != nil {
			return nil, fmt.Errorf("scan backend instance: %w", err)
		}
		if startedAt.Valid {
			b.StartedAt = startedAt.Time
		}
		if heartbeatAt.Valid {
			b.HeartbeatAt = heartbeatAt.Time
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// HoldsLease reports whether this instance is the one allowed to dispatch.
//
// The lease holder is the OLDEST live instance. Ties (two instances started
// in the same second) are broken by the id, so both sides agree on one
// holder without coordination.
//
// The result is only meaningful together with a HEARTBEAT: an instance that
// just started has not written its row yet, and would wrongly conclude it
// does not hold the lease. Callers therefore register (which writes the
// row) before asking.
func (s *BackendInstanceService) HoldsLease() (bool, error) {
	s.mu.Lock()
	id := s.instance
	s.mu.Unlock()
	if id == "" {
		return false, errors.New("backend instance is not registered")
	}
	live, err := s.ListLive()
	if err != nil {
		return false, err
	}
	if len(live) == 0 {
		// Our own row is missing (or everyone else's is stale). Treat
		// ourselves as the holder: the alternative — nobody dispatching —
		// is strictly worse.
		return true, nil
	}
	holder := live[0]
	for _, b := range live[1:] {
		if b.StartedAt.Before(holder.StartedAt) ||
			(b.StartedAt.Equal(holder.StartedAt) && b.ID < holder.ID) {
			holder = b
		}
	}
	return holder.ID == id, nil
}

// StartHeartbeat registers this instance and refreshes its heartbeat every
// 15s until Stop is called.
//
// It returns the instance id so the caller can log it.
func (s *BackendInstanceService) StartHeartbeat(host string) (string, error) {
	id, err := s.Register(host)
	if err != nil {
		return id, err
	}
	s.mu.Lock()
	s.stopCh = make(chan struct{})
	stopCh := s.stopCh
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				if err := s.Heartbeat(); err != nil {
					log.Printf("[instance] heartbeat failed for %s: %v", id, err)
				}
			}
		}
	}()
	return id, nil
}

// Stop ends the heartbeat and removes the registration.
func (s *BackendInstanceService) Stop() {
	s.mu.Lock()
	if s.stopCh != nil {
		select {
		case <-s.stopCh:
		default:
			close(s.stopCh)
		}
		s.stopCh = nil
	}
	s.mu.Unlock()
	s.Clear()
}
