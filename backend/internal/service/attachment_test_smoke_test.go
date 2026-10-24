package service

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
)

// Smoke test for BuildManifest: opens a real DB (the one the user pointed at
// via NOVA_DB_PATH), reads the attachments for a requirement, prints the
// prompt-injection block. NOT a pure unit test — needs a live DB.
//
// Run with: go test ./internal/service -run TestBuildManifestSmoke
func TestBuildManifestSmoke(t *testing.T) {
	cfg := db.LoadConfig()
	if cfg.Driver == "" {
		t.Skip("no driver configured; set NOVA_DB_DRIVER=sqlite + NOVA_DB_PATH or use the existing pg fixture")
	}
	d, err := db.Init(cfg)
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	defer d.Close()

	svc := NewAttachmentService(d)
	reqID := getFirstReqID(t, d)
	if reqID == "" {
		// The test suite sandboxes HOME (see TestMain), so the "live DB" this
		// smoke test is written against is usually absent: LoadConfig falls
		// back to a fresh sqlite file with no requirements. Skipping keeps the
		// suite green while preserving the manual
		// `NOVA_DB_DRIVER=postgres NOVA_DB_DSN=... go test -run TestBuildManifestSmoke`
		// workflow its doc comment describes.
		t.Skip("no requirements in the configured DB; point NOVA_DB_* at a populated DB to run this smoke test")
	}
	t.Logf("using req %s", reqID)

	got := svc.BuildManifest(reqID, filepath.Join(AttachmentRoot(), reqID))
	if got == "" {
		t.Logf("no attachments for %s — manifest is empty (as designed)", reqID)
		return
	}
	fmt.Println("=== BuildManifest output ===")
	fmt.Println(got)
	fmt.Println("=== end ===")
}

func getFirstReqID(t *testing.T, d *db.DB) string {
	t.Helper()
	var id string
	err := d.QueryRow(`SELECT id FROM requirements ORDER BY created_at DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "" // caller decides: this smoke test skips on an empty DB
	}
	if err != nil {
		t.Fatalf("read req: %v", err)
	}
	return id
}

// TestBuildManifestUsesStoredPathBasename pins down the on-disk filename
// contract: BuildManifest's `**位置**:` line must print the `stored_path`
// basename (which carries the `<att_id>__<sha8>__<safe_name>` prefix that
// Upload enforces), NOT the bare `filename` column. A previous version of
// the function used `a.Filename` and produced paths that did not exist on
// disk, so Claude's Read tool failed to open any pasted image.
//
// Hermetic: spins up a fresh sqlite DB under t.TempDir, inserts one project
// + requirement + attachment row, then asserts the rendered manifest.
func TestBuildManifestUsesStoredPathBasename(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "t.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	defer d.Close()

	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?)`,
		"proj_manifest", "manifest test", filepath.Join(dir, "proj"),
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO requirements (id, project_id, kind, title, description, status) VALUES (?, ?, ?, ?, ?, ?)`,
		"req_x", "proj_manifest", "feature", "manifest test req", "desc", "draft",
	); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	// The interesting row: stored_path carries the hash prefix; filename does not.
	if _, err := d.Exec(`
		INSERT INTO requirement_attachments
		  (id, requirement_id, filename, stored_path, mime_type,
		   size_bytes, sha256, description, manifest_text, uploaded_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"att_abc", "req_x",
		"shot.png", "req_x/att_abc__deadbeef__shot.png",
		"image/png", 1234,
		"deadbeef0000000000000000000000000000000000000000000000000000000000",
		"test desc", "", "usr_t",
	); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	svc := NewAttachmentService(d)
	attachDir := filepath.Join(dir, "attachments")
	got := svc.BuildManifest("req_x", attachDir)
	if got == "" {
		t.Fatal("BuildManifest returned empty for a row with one attachment")
	}

	// The fix: the `**位置**:` line must contain the on-disk basename, not
	// the bare original filename.
	wantPath := filepath.Join(attachDir, "att_abc__deadbeef__shot.png")
	wantLine := "**位置**：`" + wantPath + "`"
	if !strings.Contains(got, wantLine) {
		t.Fatalf("manifest missing stored-basename path line\nwant substring: %q\nactual:\n%s", wantLine, got)
	}

	// Guard against the old bug: a backticked path composed of the bare
	// filename must NOT appear in the output.
	bareLine := "**位置**：`" + filepath.Join(attachDir, "shot.png") + "`"
	if strings.Contains(got, bareLine) {
		t.Fatalf("manifest still emits the bare filename (regression):\n%s", got)
	}
}
