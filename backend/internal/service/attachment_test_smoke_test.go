package service

import (
	"fmt"
	"path/filepath"
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
	if err != nil {
		t.Fatalf("read req: %v", err)
	}
	return id
}
