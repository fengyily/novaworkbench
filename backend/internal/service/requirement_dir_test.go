package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
)

// reqDirTestDB opens a fresh SQLite db (migrations applied by db.Init) with
// one project + one requirement to hang the directory grants off.
func reqDirTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.Exec("INSERT INTO projects (id, name, local_path, description) VALUES ('proj_1', 'Test', '/tmp/proj1', '')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec("INSERT INTO requirements (id, project_id, title, description) VALUES ('req_1', 'proj_1', '目录授权', '')"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	return d
}

// TestNormalizeDirPath pins the path normalization rules. The stored path is
// what the claude CLI receives as --add-dir AND what the UI shows, so a
// surprise here (a relative path, an unexpanded tilde) would mean the model
// reads a different directory than the user authorized.
func TestNormalizeDirPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	abs, err := filepath.Abs("/tmp")
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}

	cases := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{name: "absolute", in: abs, want: abs},
		{name: "trailing slash", in: abs + "/", want: abs},
		{name: "dot segments", in: abs + "/foo/../bar", want: abs + "/bar"},
		{name: "tilde", in: "~/", want: home},
		{name: "tilde subdir", in: "~/sub", want: filepath.Join(home, "sub")},
		{name: "tilde alone", in: "~", want: home},
		{name: "relative becomes absolute", in: "relative/dir", want: filepath.Join(mustCwd(t), "relative/dir")},
		{name: "empty", in: "   ", wantErr: nil}, // error, but not a sentinel
		{name: "root rejected", in: "/", wantErr: ErrRootPath},
		{name: "control char rejected", in: "/tmp/a\x00b", wantErr: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDirPath(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("NormalizeDirPath(%q) error = %v, want %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if tc.name == "empty" || tc.name == "control char rejected" {
				if err == nil {
					t.Fatalf("NormalizeDirPath(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeDirPath(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeDirPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func mustCwd(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return cwd
}

// TestRequirementDirAddListDelete walks the whole lifecycle against a real
// SQLite db: grant → list → paths → re-grant (dedupe) → revoke.
func TestRequirementDirAddListDelete(t *testing.T) {
	d := reqDirTestDB(t)
	svc := NewRequirementDirService(d)

	dir := t.TempDir()
	other := t.TempDir()

	got, err := svc.Add("req_1", dir, "参考日志", "usr_1")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got.Path != dir {
		t.Fatalf("Add path = %q, want %q", got.Path, dir)
	}
	if got.Label != "参考日志" || got.AddedBy != "usr_1" {
		t.Fatalf("Add label/by = %q/%q, want 参考日志/usr_1", got.Label, got.AddedBy)
	}
	if got.ID == "" {
		t.Fatal("Add returned an empty id")
	}

	list, err := svc.List("req_1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Path != dir {
		t.Fatalf("List = %+v, want one row for %q", list, dir)
	}

	paths, err := svc.Paths("req_1")
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	if len(paths) != 1 || paths[0] != dir {
		t.Fatalf("Paths = %v, want [%q]", paths, dir)
	}

	// Second grant of the same directory is a no-op that refreshes the label.
	again, err := svc.Add("req_1", dir, "改过的备注", "usr_1")
	if err != nil {
		t.Fatalf("Add (dup): %v", err)
	}
	if again.ID != got.ID {
		t.Fatalf("Add (dup) id = %q, want the existing %q", again.ID, got.ID)
	}
	if again.Label != "改过的备注" {
		t.Fatalf("Add (dup) label = %q, want the refreshed 改过的备注", again.Label)
	}
	list, _ = svc.List("req_1")
	if len(list) != 1 {
		t.Fatalf("List after dup = %d rows, want 1", len(list))
	}

	// A second, different directory stacks on top of the first.
	if _, err := svc.Add("req_1", other, "", "usr_1"); err != nil {
		t.Fatalf("Add (second): %v", err)
	}
	paths, _ = svc.Paths("req_1")
	if len(paths) != 2 {
		t.Fatalf("Paths = %v, want 2 entries", paths)
	}

	// Deleting is scoped to the requirement: a grant owned by another
	// requirement must not be revocable through this one.
	if err := svc.Delete("req_1", got.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := svc.Delete("req_1", got.ID); err == nil {
		t.Fatal("Delete on an already-deleted row should report not-found")
	}
	list, _ = svc.List("req_1")
	if len(list) != 1 || list[0].Path != other {
		t.Fatalf("List after delete = %+v, want only %q", list, other)
	}
}

// TestRequirementDirAddSkipsFilesystemChecks pins the add-time contract:
// the path is normalized but NOT checked against the local filesystem.
//
// The grant is consumed by a claude session that may run on an Agent
// server, so a path that resolves nowhere on this host may be exactly the
// directory the agent host needs. Rejecting it here would make the remote
// execution case unusable, so the only path-shape rules that survive are
// the filesystem-root rejection and the requirement-id requirement.
func TestRequirementDirAddSkipsFilesystemChecks(t *testing.T) {
	d := reqDirTestDB(t)
	svc := NewRequirementDirService(d)

	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// A path that does not exist here is accepted — it may exist on the
	// agent host that actually runs claude.
	missing := filepath.Join(t.TempDir(), "nope")
	got, err := svc.Add("req_1", missing, "", "")
	if err != nil {
		t.Fatalf("Add(missing dir) = %v, want it accepted", err)
	}
	if got.Path != missing {
		t.Fatalf("Add(missing dir) path = %q, want %q", got.Path, missing)
	}

	// A path that points at a FILE is accepted too: this host cannot say
	// what the agent host has, and the CLI ignores a --add-dir path it
	// cannot use.
	gotFile, err := svc.Add("req_1", file, "", "")
	if err != nil {
		t.Fatalf("Add(file) = %v, want it accepted", err)
	}
	if gotFile.Path != file {
		t.Fatalf("Add(file) path = %q, want %q", gotFile.Path, file)
	}

	// The root is still rejected: that rule is about the shape of the
	// path, not about whether it resolves on this host.
	if _, err := svc.Add("req_1", "/", "", ""); !errors.Is(err, ErrRootPath) {
		t.Fatalf("Add(root) error = %v, want ErrRootPath", err)
	}
	if _, err := svc.Add("", t.TempDir(), "", ""); err == nil {
		t.Fatal("Add with an empty requirement id should fail")
	}

	// The rejected grants left no rows behind; the accepted ones did.
	list, _ := svc.List("req_1")
	if len(list) != 2 {
		t.Fatalf("List = %+v, want the 2 accepted grants and no more", list)
	}
}

// TestExistsLocally pins the scope of the existence helper: it reports on
// THIS host only. The UI's badge is built on it, and it must not be read
// as "the claude host cannot read this".
func TestExistsLocally(t *testing.T) {
	dir := t.TempDir()
	if !ExistsLocally(dir) {
		t.Fatalf("ExistsLocally(%q) = false, want true", dir)
	}
	if ExistsLocally(filepath.Join(dir, "nope")) {
		t.Fatal("ExistsLocally on a missing path = true, want false")
	}
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if ExistsLocally(file) {
		t.Fatal("ExistsLocally on a file = true, want false")
	}
}

// TestRequirementDirPathsEmpty pins the "nothing granted" contract: the CLI
// args helper must return an empty result (not an error) so callers can pass
// it through unconditionally.
func TestRequirementDirPathsEmpty(t *testing.T) {
	d := reqDirTestDB(t)
	svc := NewRequirementDirService(d)

	paths, err := svc.Paths("req_1")
	if err != nil {
		t.Fatalf("Paths: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("Paths = %v, want empty", paths)
	}
	paths, err = svc.Paths("")
	if err != nil {
		t.Fatalf("Paths(\"\"): %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("Paths(\"\") = %v, want empty", paths)
	}
}

// TestRequirementDirDeleteIsScoped makes sure a grant owned by requirement A
// cannot be revoked through requirement B's endpoint.
func TestRequirementDirDeleteIsScoped(t *testing.T) {
	d := reqDirTestDB(t)
	if _, err := d.Exec("INSERT INTO requirements (id, project_id, title, description) VALUES ('req_2', 'proj_1', '另一个', '')"); err != nil {
		t.Fatalf("seed req_2: %v", err)
	}
	svc := NewRequirementDirService(d)

	got, err := svc.Add("req_1", t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := svc.Delete("req_2", got.ID); err == nil {
		t.Fatal("Delete across requirements should report not-found")
	}
	// The row must survive the cross-requirement delete attempt.
	list, _ := svc.List("req_1")
	if len(list) != 1 {
		t.Fatalf("List = %d rows, want 1 — the cross-requirement delete removed it", len(list))
	}
}

// TestRequirementDirLabelTruncation pins the label cap: an over-long note is
// trimmed rather than rejected (the path is the part that matters).
func TestRequirementDirLabelTruncation(t *testing.T) {
	d := reqDirTestDB(t)
	svc := NewRequirementDirService(d)

	long := strings.Repeat("x", MaxRequirementDirLabel+50)
	got, err := svc.Add("req_1", t.TempDir(), long, "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(got.Label) != MaxRequirementDirLabel {
		t.Fatalf("label len = %d, want %d", len(got.Label), MaxRequirementDirLabel)
	}
}
