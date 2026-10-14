package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// fakeRemote is a stand-in for *ssh.Client. Records every PutFile /
// Remove / Mkdirp call so tests can assert the diff behaviour without
// spinning up a real SFTP server.
type fakeRemote struct {
	mu          sync.Mutex
	put         map[string][]byte // remotePath -> bytes
	mkdirCalls  []string
	putCalls    []string
	removeCalls []string
}

func newFakeRemote() *fakeRemote { return &fakeRemote{put: map[string][]byte{}} }

func (r *fakeRemote) PutFile(local, remote string, _ os.FileMode) error {
	data, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.put[remote] = data
	r.putCalls = append(r.putCalls, remote)
	return nil
}
func (r *fakeRemote) Mkdirp(p string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mkdirCalls = append(r.mkdirCalls, p)
	return nil
}
func (r *fakeRemote) Remove(p string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.put, p)
	r.removeCalls = append(r.removeCalls, p)
	return nil
}

// TestAttachmentSync_DiffRoundTrip exercises the upload-if-sha-changed
// contract. Seeds two local attachments, runs sync #1 (uploads both),
// then mutates one and runs sync #2 (only re-uploads the changed one).
func TestAttachmentSync_DiffRoundTrip(t *testing.T) {
	cfg := db.LoadConfig()
	d, err := db.Init(cfg)
	if err != nil {
		t.Skipf("db init (%s) — start postgres or set NOVA_DB_DRIVER=sqlite + NOVA_DB_PATH", err)
	}
	defer d.Close()

	// Use a fresh requirement id so re-runs of this test don't pile up
	// rows. The fakes track only putCalls + put map, so cross-test
	// contamination would only affect the persisted attachment rows.
	reqID := "req_" + util.NewID("synctest")[:12]
	createReqForTest(t, d, reqID)

	// Override attachment root under t.TempDir so the test doesn't
	// touch the user's real ~/.novaworkbench/attachments/ tree.
	root := t.TempDir()
	oldRoot := attachmentRoot
	attachmentRoot = root
	defer func() { attachmentRoot = oldRoot }()

	svc := NewAttachmentService(d)

	// Two uploads on the first round.
	a1 := mustUpload(t, svc, reqID, "alpha.log", []byte("alpha v1\n"), "")
	a2 := mustUpload(t, svc, reqID, "beta.zip", []byte("PK fake"), "")

	serverID := "srv_" + util.NewID("sync")[:12]
	createAgentServerForTest(t, d, serverID)

	remote := newFakeRemote()

	// Sync #1 — both files upload, state populated.
	rd, err := svc.SyncToAgentServer(context.Background(), serverID, remote, reqID)
	if err != nil {
		t.Fatalf("sync #1: %v", err)
	}
	if rd == "" {
		t.Fatal("expected non-empty remote attach dir")
	}
	if got := len(remote.putCalls); got != 2 {
		t.Fatalf("sync #1 put count = %d, want 2", got)
	}
	var stateJSON string
	if err := d.QueryRow(
		`SELECT attachment_sync_state FROM agent_servers WHERE id = ?`,
		serverID).Scan(&stateJSON); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !containsAll(stateJSON, []string{a1.SHA256[:8], a2.SHA256[:8]}) {
		t.Fatalf("sync state missing shas: %s", stateJSON)
	}

	// Mutate one file (re-upload). Same SHA iff content identical, so we
	// append a byte to force a SHA change.
	newBytes := []byte("alpha v2\n")
	if err := os.WriteFile(svc.AbsolutePath(&a1), newBytes, 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, err := svc.db.Exec(
		`UPDATE requirement_attachments SET size_bytes=?, sha256=? WHERE id=?`,
		int64(len(newBytes)), shaOfBytes(newBytes), a1.ID); err != nil {
		t.Fatalf("update sha: %v", err)
	}

	// Sync #2 — only the changed file gets re-uploaded.
	beforeCalls := len(remote.putCalls)
	remote.mu.Lock()
	prePutKeys := map[string]bool{}
	for k := range remote.put {
		prePutKeys[k] = true
	}
	remote.mu.Unlock()

	if _, err := svc.SyncToAgentServer(context.Background(), serverID, remote, reqID); err != nil {
		t.Fatalf("sync #2: %v", err)
	}
	added := len(remote.putCalls) - beforeCalls
	if added != 1 {
		t.Fatalf("sync #2 expected 1 fresh upload, got %d", added)
	}
}

// TestAttachmentSync_NoWorkIsCheap asserts an idempotent re-sync
// (no file changes) uploads nothing.
func TestAttachmentSync_NoWorkIsCheap(t *testing.T) {
	cfg := db.LoadConfig()
	d, err := db.Init(cfg)
	if err != nil {
		t.Skip("db init — set NOVA_DB_DRIVER=sqlite + NOVA_DB_PATH for SQLite test")
	}
	defer d.Close()

	reqID := "req_" + util.NewID("nosync")[:12]
	createReqForTest(t, d, reqID)
	serverID := "srv_" + util.NewID("nosync")[:12]
	createAgentServerForTest(t, d, serverID)

	root := t.TempDir()
	oldRoot := attachmentRoot
	attachmentRoot = root
	defer func() { attachmentRoot = oldRoot }()

	svc := NewAttachmentService(d)
	a := mustUpload(t, svc, reqID, "delta.log", []byte("delta\n"), "")

	remote := newFakeRemote()
	if _, err := svc.SyncToAgentServer(context.Background(), serverID, remote, reqID); err != nil {
		t.Fatalf("sync #1: %v", err)
	}
	before := len(remote.putCalls)
	if _, err := svc.SyncToAgentServer(context.Background(), serverID, remote, reqID); err != nil {
		t.Fatalf("sync #2: %v", err)
	}
	if got := len(remote.putCalls) - before; got != 0 {
		t.Fatalf("idempotent sync should not upload; got %d new puts", got)
	}
	_ = a
}

// TestAttachmentSync_PersistsStateAcrossRuns asserts that two
// SyncToAgentServer calls running from the same starting state don't
// re-upload anything on the second call (state survives).
func TestAttachmentSync_PersistsStateAcrossRuns(t *testing.T) {
	cfg := db.LoadConfig()
	d, err := db.Init(cfg)
	if err != nil {
		t.Skip("db init — set NOVA_DB_DRIVER=sqlite + NOVA_DB_PATH for SQLite test")
	}
	defer d.Close()

	reqID := "req_" + util.NewID("persist")[:12]
	createReqForTest(t, d, reqID)
	serverID := "srv_" + util.NewID("persist")[:12]
	createAgentServerForTest(t, d, serverID)

	root := t.TempDir()
	oldRoot := attachmentRoot
	attachmentRoot = root
	defer func() { attachmentRoot = oldRoot }()

	svc := NewAttachmentService(d)
	mustUpload(t, svc, reqID, "gamma.log", []byte("gamma\n"), "")

	r1 := newFakeRemote()
	if _, err := svc.SyncToAgentServer(context.Background(), serverID, r1, reqID); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Fresh fake, same server: state loaded from DB should short-circuit
	// all uploads.
	r2 := newFakeRemote()
	if _, err := svc.SyncToAgentServer(context.Background(), serverID, r2, reqID); err != nil {
		t.Fatalf("sync #2: %v", err)
	}
	if len(r2.putCalls) != 0 {
		t.Fatalf("fresh fake should not upload: %v", r2.putCalls)
	}
}

// TestAttachmentSync_OrphanPrunedOnLocalDelete seeds two attachments,
// syncs, deletes one locally, then re-syncs. The next sync must SFTP
// remove the deleted file's remote twin and prune the state entry.
func TestAttachmentSync_OrphanPrunedOnLocalDelete(t *testing.T) {
	cfg := db.LoadConfig()
	d, err := db.Init(cfg)
	if err != nil {
		t.Skip("db init — set NOVA_DB_DRIVER=sqlite + NOVA_DB_PATH for SQLite test")
	}
	defer d.Close()

	reqID := "req_" + util.NewID("orphan")[:12]
	createReqForTest(t, d, reqID)
	serverID := "srv_" + util.NewID("orphan")[:12]
	createAgentServerForTest(t, d, serverID)

	root := t.TempDir()
	oldRoot := attachmentRoot
	attachmentRoot = root
	defer func() { attachmentRoot = oldRoot }()

	svc := NewAttachmentService(d)
	aKeep := mustUpload(t, svc, reqID, "keep.log", []byte("keep\n"), "")
	aDrop := mustUpload(t, svc, reqID, "drop.log", []byte("drop\n"), "")

	remote := newFakeRemote()
	if _, err := svc.SyncToAgentServer(context.Background(), serverID, remote, reqID); err != nil {
		t.Fatalf("sync #1: %v", err)
	}
	if len(remote.putCalls) != 2 {
		t.Fatalf("expected 2 puts, got %d", len(remote.putCalls))
	}

	// Locally delete aDrop — svc.Delete removes DB row + on-disk file.
	if err := svc.Delete(aDrop.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Reset fake remote counters; reuse the same backing map to track
	// what's on remote after the second sync.
	beforePuts := len(remote.putCalls)
	beforeRemoves := len(remote.removeCalls)

	if _, err := svc.SyncToAgentServer(context.Background(), serverID, remote, reqID); err != nil {
		t.Fatalf("sync #2: %v", err)
	}

	// One SFTP remove (aDrop), zero SFTP puts (aKeep untouched).
	if got := len(remote.removeCalls) - beforeRemoves; got != 1 {
		t.Fatalf("expected 1 remote Remove, got %d", got)
	}
	if got := len(remote.putCalls) - beforePuts; got != 0 {
		t.Fatalf("aKeep should not have been re-uploaded; got %d puts", got)
	}

	// Read the LIVE post-sync state of the fake remote to verify which
	// att_ids are still on the wire. fakeRemote.Remove deleted from the
	// map, so we walk the live `remote.put` keys.
	attIDsOnRemote := map[string]bool{}
	for k := range remote.put {
		base := filepath.Base(k)
		idx := strings.Index(base, "__")
		if idx < 0 {
			continue
		}
		attIDsOnRemote[base[:idx]] = true
	}
	if !attIDsOnRemote[aKeep.ID] {
		t.Fatalf("aKeep missing from remote after sync; att_ids on remote: %v", attIDsOnRemote)
	}
	if attIDsOnRemote[aDrop.ID] {
		t.Fatalf("aDrop still on remote after orphan prune; att_ids: %v", attIDsOnRemote)
	}
}

// TestAttachmentSync_LegacyEntryPreserved checks a hand-crafted legacy
// "att_id: \"sha256\"" entry (the v0.5.x shape without a name field).
// The orphan-prune loop MUST NOT remove the matching remote file because
// we don't know the filename — it stays untouched until a re-upload
// repopulates the entry with a name.
func TestAttachmentSync_LegacyEntryPreserved(t *testing.T) {
	cfg := db.LoadConfig()
	d, err := db.Init(cfg)
	if err != nil {
		t.Skip("db init — set NOVA_DB_DRIVER=sqlite + NOVA_DB_PATH for SQLite test")
	}
	defer d.Close()

	reqID := "req_" + util.NewID("legacy")[:12]
	createReqForTest(t, d, reqID)
	serverID := "srv_" + util.NewID("legacy")[:12]
	createAgentServerForTest(t, d, serverID)

	root := t.TempDir()
	oldRoot := attachmentRoot
	attachmentRoot = root
	defer func() { attachmentRoot = oldRoot }()

	svc := NewAttachmentService(d)
	a := mustUpload(t, svc, reqID, "legacy.log", []byte("legacy\n"), "")

	// Hand-seed a legacy entry — note: matching att_id but NO name
	// field. The orphan-prune pass should skip it.
	legacyJSON := fmt.Sprintf(
		`{"%s": {"%s": %q}}`,
		reqID, a.ID, a.SHA256)
	if _, err := d.Exec(
		`UPDATE agent_servers SET attachment_sync_state = ? WHERE id = ?`,
		legacyJSON, serverID); err != nil {
		t.Fatalf("seed legacy state: %v", err)
	}

	remote := newFakeRemote()
	// Pretend the remote file exists by recording its basename in put.
	// We don't actually need it on the wire for this test — the
	// assertion is "no Remove was called" because of the legacy skip.
	legacyName := basenameForStoredPath(t, svc, a.StoredPath)
	remote.put["~/.nova/attachments/"+reqID+"/"+legacyName] = []byte("legacy\n")

	if _, err := svc.SyncToAgentServer(context.Background(), serverID, remote, reqID); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if len(remote.removeCalls) != 0 {
		t.Fatalf("legacy entry without name should NOT trigger Remove; got %v", remote.removeCalls)
	}
	// sha matches → skip the PutFile too (no fresh upload).
	if len(remote.putCalls) != 0 {
		t.Fatalf("matching sha should short-circuit upload; got %d puts", len(remote.putCalls))
	}
}

// TestAttachmentSync_DeleteThenNewName asserts that locally deleting
// one attachment and uploading a different one with the same filename
// — but a different att_id — leaves exactly one entry on remote.
func TestAttachmentSync_DeleteThenNewName(t *testing.T) {
	cfg := db.LoadConfig()
	d, err := db.Init(cfg)
	if err != nil {
		t.Skip("db init — set NOVA_DB_DRIVER=sqlite + NOVA_DB_PATH for SQLite test")
	}
	defer d.Close()

	reqID := "req_" + util.NewID("rename")[:12]
	createReqForTest(t, d, reqID)
	serverID := "srv_" + util.NewID("rename")[:12]
	createAgentServerForTest(t, d, serverID)

	root := t.TempDir()
	oldRoot := attachmentRoot
	attachmentRoot = root
	defer func() { attachmentRoot = oldRoot }()

	svc := NewAttachmentService(d)
	v1 := mustUpload(t, svc, reqID, "build.log", []byte("v1\n"), "")
	remote := newFakeRemote()
	if _, err := svc.SyncToAgentServer(context.Background(), serverID, remote, reqID); err != nil {
		t.Fatalf("sync #1: %v", err)
	}
	// Delete v1 entirely.
	if err := svc.Delete(v1.ID); err != nil {
		t.Fatalf("delete v1: %v", err)
	}
	// Re-upload using the SAME filename. Service auto-generates a
	// fresh att_id (not the same as v1) and a different stored_path,
	// because Upload writes a new row via util.NewID.
	v2 := mustUpload(t, svc, reqID, "build.log", []byte("v2\n"), "")
	if v2.ID == v1.ID {
		t.Fatalf("expected new att_id after re-upload; got %s again", v1.ID)
	}

	beforeRemoves := len(remote.removeCalls)
	if _, err := svc.SyncToAgentServer(context.Background(), serverID, remote, reqID); err != nil {
		t.Fatalf("sync #2: %v", err)
	}
	// v1 should have been pruned remotely (orphan), v2 uploaded.
	if len(remote.removeCalls)-beforeRemoves != 1 {
		t.Fatalf("expected 1 Remove for v1's basename, got %d", len(remote.removeCalls)-beforeRemoves)
	}
	if len(remote.putCalls) != 2 {
		t.Fatalf("expected 2 puts total (v1 first sync + v2 second sync), got %d", len(remote.putCalls))
	}
}

// ─── helpers ───

func basenameForStoredPath(t *testing.T, _ *AttachmentService, storedPath string) string {
	t.Helper()
	return filepath.Base(storedPath)
}

func mustUpload(t *testing.T, svc *AttachmentService, reqID, filename string, content []byte, description string) model.Attachment {
	t.Helper()
	dir, err := svc.ownerDir(reqID)
	if err != nil {
		t.Fatalf("req dir: %v", err)
	}
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	a, err := svc.Upload(reqID, model.AttachmentUpload{
		Filename:    filename,
		Reader:      openReader(t, p),
		Description: description,
		UploadedBy:  "test",
	})
	if err != nil {
		t.Fatalf("upload %s: %v", filename, err)
	}
	return *a
}

func shaOfBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func containsAll(s string, needles []string) bool {
	for _, n := range needles {
		if !json.Valid([]byte(s)) {
			return false
		}
		if indexOf(s, n) < 0 {
			return false
		}
	}
	return true
}

func indexOf(s, sub string) int {
	// avoid pulling strings just for this; keep deps minimal in the test
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func createReqForTest(t *testing.T, d *db.DB, id string) {
	t.Helper()
	// requirements has a FK on project_id (ON DELETE CASCADE). Seed a
	// stub project row first; ON CONFLICT makes re-runs idempotent.
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		"proj_attachtest", "attachment sync test", "/tmp/attachment-sync-test"); err != nil {
		t.Skipf("seed project: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO requirements (id, project_id, title) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		id, "proj_attachtest", "attachment sync test"); err != nil {
		t.Skipf("seed requirement: %v", err)
	}
}

func createAgentServerForTest(t *testing.T, d *db.DB, id string) {
	t.Helper()
	// agent_servers UNIQUE index is on id only; auth_value can be empty for
	// the test (we never dial — only call SyncToAgentServer which uses the
	// passed-in RemoteAttacher directly).
	_, err := d.Exec(
		`INSERT INTO agent_servers (id, name, host, port, username, auth_value)
		 VALUES (?, ?, ?, ?, ?, '')
		 ON CONFLICT(id) DO NOTHING`,
		id, "sync-test", "127.0.0.1", 22, "root")
	if err != nil {
		t.Skipf("seed agent_server: %v", err)
	}
}
