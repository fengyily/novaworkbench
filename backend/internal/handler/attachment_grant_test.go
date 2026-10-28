package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// newAttachGrantFixture wires a real AttachmentService over a temp SQLite DB
// and points the (package-global) attachment root at a temp dir, restoring the
// previous value on cleanup so sibling tests are unaffected.
func newAttachGrantFixture(t *testing.T) (*service.AttachmentService, string) {
	t.Helper()
	oldRoot := service.AttachmentRoot()
	root := t.TempDir()
	service.SetAttachmentRoot(root)
	t.Cleanup(func() { service.SetAttachmentRoot(oldRoot) })

	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: path})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { d.Close(); os.Remove(path) })
	if _, err := d.Exec("INSERT INTO projects (id, name, local_path) VALUES ('proj_1', 'Test', '/tmp/proj1')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := d.Exec("INSERT INTO requirements (id, project_id, title) VALUES ('req_1', 'proj_1', '附件授权')"); err != nil {
		t.Fatalf("seed requirement: %v", err)
	}
	return service.NewAttachmentService(d), root
}

// uploadAttach seeds one attachment for ownerID and returns its owner dir.
func uploadAttach(t *testing.T, svc *service.AttachmentService, ownerID, filename string) {
	t.Helper()
	if _, err := svc.Upload(ownerID, model.AttachmentUpload{
		Filename:   filename,
		Reader:     strings.NewReader("boom"),
		UploadedBy: "u1",
	}); err != nil {
		t.Fatalf("upload attachment for %s: %v", ownerID, err)
	}
}

func TestAttachGrantDirsKeepsUserDirsInOrder(t *testing.T) {
	svc, _ := newAttachGrantFixture(t)
	uploadAttach(t, svc, "req_1", "error.log")

	got := attachGrantDirs([]string{"/a/specs", "/b/logs"}, svc, localAttachmentGrant("req_1"))

	if len(got) < 2 {
		t.Fatalf("expected the user dirs to survive, got %v", got)
	}
	if got[0] != "/a/specs" || got[1] != "/b/logs" {
		t.Fatalf("user dirs must keep their order, got %v", got)
	}
}

func TestAttachGrantDirsAppendsAttachmentDirWhenOwnerHasAttachments(t *testing.T) {
	svc, root := newAttachGrantFixture(t)
	uploadAttach(t, svc, "req_1", "error.log")

	want := filepath.Join(root, "req_1")
	got := attachGrantDirs(nil, svc, localAttachmentGrant("req_1"))

	if len(got) != 1 || got[0] != want {
		t.Fatalf("expected exactly [%s], got %v", want, got)
	}
}

func TestAttachGrantDirsSkipsOwnerWithoutAttachments(t *testing.T) {
	svc, _ := newAttachGrantFixture(t)

	got := attachGrantDirs(nil, svc, localAttachmentGrant("req_1"))

	if got != nil {
		t.Fatalf("an owner without attachments must not be granted a directory, got %v", got)
	}
}

func TestAttachGrantDirsFallsBackToLocalRootWhenDirEmpty(t *testing.T) {
	svc, root := newAttachGrantFixture(t)
	uploadAttach(t, svc, "req_1", "error.log")

	// The fallback must be byte-identical to attachManifestFor's own fallback
	// (filepath.Join(service.AttachmentRoot(), ownerID)) — otherwise the grant
	// and the manifest point at different directories and the attachment stays
	// invisible.
	got := attachGrantDirs(nil, svc, attachmentGrant{ownerID: "req_1", dir: ""})

	want := filepath.Join(service.AttachmentRoot(), "req_1")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("expected [%s], got %v", want, got)
	}
	if want != filepath.Join(root, "req_1") {
		t.Fatalf("fixture root mismatch: %s vs %s", want, filepath.Join(root, "req_1"))
	}
}

func TestAttachGrantDirsDeduplicates(t *testing.T) {
	svc, root := newAttachGrantFixture(t)
	uploadAttach(t, svc, "req_1", "error.log")

	attachDir := filepath.Join(root, "req_1")
	got := attachGrantDirs([]string{"/a/specs", attachDir}, svc, localAttachmentGrant("req_1"))

	seen := map[string]int{}
	for _, d := range got {
		seen[d]++
	}
	if seen[attachDir] != 1 {
		t.Fatalf("the attachment dir must appear exactly once, got %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries after dedup, got %v", got)
	}
}

func TestAttachGrantDirsWithNilAttachmentService(t *testing.T) {
	got := attachGrantDirs([]string{"/a/specs"}, nil, localAttachmentGrant("req_1"))

	if len(got) != 1 || got[0] != "/a/specs" {
		t.Fatalf("only the user dirs may survive, got %v", got)
	}
}

func TestAttachGrantDirsWithMultipleOwners(t *testing.T) {
	svc, root := newAttachGrantFixture(t)
	uploadAttach(t, svc, "req_1", "error.log")
	if _, err := svc.Upload("req_2", model.AttachmentUpload{
		Filename:   "spec.md",
		Reader:     strings.NewReader("spec"),
		UploadedBy: "u1",
	}); err != nil {
		t.Fatalf("upload attachment for req_2: %v", err)
	}

	got := attachGrantDirs(nil, svc,
		localAttachmentGrant("req_1"),
		attachmentGrant{ownerID: "req_2", dir: filepath.Join(root, "req_2")},
	)

	want1 := filepath.Join(root, "req_1")
	want2 := filepath.Join(root, "req_2")
	if len(got) != 2 || got[0] != want1 || got[1] != want2 {
		t.Fatalf("expected [%s %s], got %v", want1, want2, got)
	}
}

// TestGrantDirsForCoversTheManifestPath pins the invariant that makes this
// feature work at all: every path the attachment manifest names must live
// inside a directory that is granted via --add-dir. If the two drift apart,
// the manifest points claude at a file it may not read — and in plan mode
// (a non-interactive -p run, nobody to approve) the attachment is silently
// invisible. This is the failure the whole change exists to remove.
func TestGrantDirsForCoversTheManifestPath(t *testing.T) {
	svc, root := newAttachGrantFixture(t)
	uploadAttach(t, svc, "req_1", "error.log")

	h := &WizardHandler{attachSvc: svc}
	attachDir := filepath.Join(root, "req_1")

	manifest := h.attachManifestFor("req_1", attachDir)
	if manifest == "" {
		t.Fatal("expected an attachment manifest for a requirement with an attachment")
	}

	// The manifest names "<attachDir>/<att_id>__<sha8>__error.log".
	var named string
	for _, line := range strings.Split(manifest, "\n") {
		if !strings.HasPrefix(line, "**位置**：") {
			continue
		}
		named = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "**位置**：")), "`")
	}
	if named == "" {
		t.Fatalf("manifest has no 位置 line:\n%s", manifest)
	}

	grants := h.grantDirsFor("req_1", attachDir)
	for _, g := range grants {
		if strings.HasPrefix(named, g+string(filepath.Separator)) {
			return
		}
	}
	t.Fatalf("manifest path %q is not covered by the grants %v", named, grants)
}

// TestGrantDirsReachTheCliArgv closes the loop end to end: the attachment
// directory has to end up in the claude CLI's argv as an --add-dir value, and
// --add-dir has to stay the last flag (it is variadic — anything after it
// would be swallowed as another directory). If either half breaks, the
// attachment goes back to being invisible in plan mode.
func TestGrantDirsReachTheCliArgv(t *testing.T) {
	svc, root := newAttachGrantFixture(t)
	uploadAttach(t, svc, "req_1", "error.log")

	h := &WizardHandler{attachSvc: svc}
	attachDir := filepath.Join(root, "req_1")
	grants := h.grantDirsFor("req_1", attachDir)

	args := (&llm.Gateway{}).BuildStreamArgs(llm.StreamOpts{
		Prompt:         "p",
		PermissionMode: "plan",
		AddDirs:        grants,
	})

	i := -1
	for idx, a := range args {
		if a == "--add-dir" {
			i = idx
			break
		}
	}
	if i == -1 {
		t.Fatalf("args = %v, want --add-dir", args)
	}
	found := false
	for _, a := range args[i+1:] {
		if a == attachDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("args = %v, want the attachment dir %q among the --add-dir values", args, attachDir)
	}
	if i != len(args)-len(grants)-1 {
		t.Fatalf("--add-dir at index %d of %d — it must be the last flag (args = %v)", i, len(args), args)
	}
}
