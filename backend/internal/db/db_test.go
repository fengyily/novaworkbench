package db

import (
	"strings"
	"testing"
)

func TestRebindPostgres(t *testing.T) {
	d := &DB{dialect: Postgres}
	got := d.Rebind("SELECT * FROM t WHERE a = ? AND b = ? OR c = ?")
	want := "SELECT * FROM t WHERE a = $1 AND b = $2 OR c = $3"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRebindPassthrough(t *testing.T) {
	for _, dialect := range []Dialect{SQLite, MySQL} {
		d := &DB{dialect: dialect}
		const q = "SELECT * FROM t WHERE a = ?"
		if got := d.Rebind(q); got != q {
			t.Fatalf("%s: got %q want passthrough", dialect, got)
		}
	}
}

func TestIdent(t *testing.T) {
	if got := (&DB{dialect: MySQL}).Ident("key"); got != "`key`" {
		t.Fatalf("mysql: got %q", got)
	}
	for _, dialect := range []Dialect{SQLite, Postgres} {
		if got := (&DB{dialect: dialect}).Ident("key"); got != `"key"` {
			t.Fatalf("%s: got %q", dialect, got)
		}
	}
}

func TestOnConflict(t *testing.T) {
	pg := (&DB{dialect: Postgres}).OnConflict("job_id", "status = excluded.status")
	if want := " ON CONFLICT(job_id) DO UPDATE SET status = excluded.status"; pg != want {
		t.Fatalf("pg: got %q want %q", pg, want)
	}
	my := (&DB{dialect: MySQL}).OnConflict("job_id", "status = excluded.status, log = excluded.log")
	if want := " ON DUPLICATE KEY UPDATE status = VALUES(status), log = VALUES(log)"; my != want {
		t.Fatalf("mysql: got %q want %q", my, want)
	}
}

func TestFixupSchemaMySQL(t *testing.T) {
	s := fixupSchema(MySQL, canonicalSchema)
	checks := []string{
		"id VARCHAR(64) PRIMARY KEY",             // indexed TEXT → VARCHAR
		"`key`        VARCHAR(191) PRIMARY KEY",   // reserved word quoted
		"local_path VARCHAR(512) NOT NULL UNIQUE", // unique TEXT → VARCHAR
		"DEFAULT ('[]')",                          // TEXT literal default → expression form
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("mysql schema missing %q", c)
		}
	}
	if strings.Contains(s, "id TEXT PRIMARY KEY") {
		t.Error("mysql schema still has unindexed-able TEXT primary key")
	}
}

func TestFixupSchemaPostgres(t *testing.T) {
	s := fixupSchema(Postgres, canonicalSchema)
	if strings.Contains(s, "DATETIME") {
		t.Error("pg schema still contains DATETIME")
	}
	if !strings.Contains(s, "created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP") {
		t.Error("pg schema missing TIMESTAMP conversion")
	}
}

func TestMaskDSN(t *testing.T) {
	if got := MaskDSN("user:secret@tcp(localhost:3306)/db?parseTime=true"); got != "user:****@tcp(localhost:3306)/db?parseTime=true" {
		t.Errorf("mysql mask: got %q", got)
	}
	if got := MaskDSN("postgres://u:secret@h:5432/db?sslmode=disable"); got != "postgres://u:%2A%2A%2A%2A@h:5432/db?sslmode=disable" && got != "postgres://u:****@h:5432/db?sslmode=disable" {
		t.Errorf("pg mask: got %q", got)
	}
}

func TestBuildDSN(t *testing.T) {
	my, err := BuildDSN("mysql", "db", "", "u", "p", "nova")
	if err != nil || !strings.Contains(my, "@tcp(db:3306)/nova") || !strings.Contains(my, "parseTime=true") {
		t.Errorf("mysql dsn: %q err=%v", my, err)
	}
	pg, err := BuildDSN("postgres", "db", "", "u", "p@ss", "nova")
	if err != nil || !strings.HasPrefix(pg, "postgres://u:p%40ss@db:5432/nova") {
		t.Errorf("pg dsn: %q err=%v", pg, err)
	}
}
