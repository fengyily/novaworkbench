package service

import (
	"path/filepath"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
)

// Covers the per-user star end to end on SQLite: ordering (starred first),
// isolation between users, idempotent re-star, PROJECT_NOT_FOUND on an
// unknown id, and Purge cleaning project_stars by hand — SQLite never
// enables PRAGMA foreign_keys, so the ON DELETE CASCADE is a no-op there.
func TestSetStarAndListOrder(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	s := NewProjectService(d, nil, nil)
	for _, id := range []string{"proj_a", "proj_b", "proj_c"} {
		if _, err := d.Exec(`INSERT INTO projects (id, name, local_path) VALUES (?,?,?)`, id, id, filepath.Join(dir, id)); err != nil {
			t.Fatal(err)
		}
	}
	// star the oldest row; it must sort first for that user only
	if err := s.SetStar("usr_1", "proj_a", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStar("usr_1", "proj_a", true); err != nil {
		t.Fatalf("re-star not idempotent: %v", err)
	}
	ps, err := s.ListForUser("usr_1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 || ps[0].ID != "proj_a" || !ps[0].Starred {
		t.Fatalf("want proj_a starred first, got %+v", ps)
	}
	other, _ := s.ListForUser("usr_2", true)
	for _, p := range other {
		if p.Starred {
			t.Fatalf("star leaked to usr_2: %s", p.ID)
		}
	}
	// unknown project -> PROJECT_NOT_FOUND
	if err := s.SetStar("usr_1", "proj_zzz", true); err == nil {
		t.Fatal("want PROJECT_NOT_FOUND")
	}
	// unstar
	if err := s.SetStar("usr_1", "proj_a", false); err != nil {
		t.Fatal(err)
	}
	ps, _ = s.ListForUser("usr_1", true)
	for _, p := range ps {
		if p.Starred {
			t.Fatalf("still starred: %s", p.ID)
		}
	}
	// purge must clear stars (SQLite CASCADE is off)
	_ = s.SetStar("usr_1", "proj_b", true)
	if err := s.Remove("proj_b", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Purge("proj_b"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM project_stars WHERE project_id='proj_b'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("orphan star rows after purge: %d", n)
	}
}
