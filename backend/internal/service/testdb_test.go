package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
)

// newServiceTestDB returns a disposable database for service tests.
//
// These tests used to call db.LoadConfig(), which resolves to whatever the
// developer configured in 设置→数据库 (~/.novaworkbench/dbconfig.json). A plain
// `go test ./...` therefore wrote into a live database — attachment_sync_test
// seeded six `agent_servers` rows named "sync-test" per run, and they showed
// up in the Agent Servers UI forever.
//
// The default is now a fresh SQLite file under t.TempDir(). Set
// NOVA_TEST_DB_DRIVER (plus NOVA_TEST_DB_DSN) to opt into a real backend when
// you explicitly want Postgres/MySQL dialect coverage.
func newServiceTestDB(t *testing.T) *db.DB {
	t.Helper()

	var cfg db.Config
	if drv := os.Getenv("NOVA_TEST_DB_DRIVER"); drv != "" {
		cfg = db.Config{Driver: drv, DSN: os.Getenv("NOVA_TEST_DB_DSN")}
		if cfg.DSN == "" {
			t.Fatalf("NOVA_TEST_DB_DRIVER=%s requires NOVA_TEST_DB_DSN", drv)
		}
	} else {
		cfg = db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")}
	}

	d, err := db.Init(cfg)
	if err != nil {
		t.Skipf("db init (%s): %v", cfg.Driver, err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}
