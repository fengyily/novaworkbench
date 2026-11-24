package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/middleware"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// claudeDocTestRig is a disposable project + handler for the project-level
// CLAUDE.md endpoints.
type claudeDocTestRig struct {
	svc  *service.ProjectService
	h    *ProjectClaudeDocHandler
	mux  *http.ServeMux
	proj string
}

func newClaudeDocTestRig(t *testing.T, localPath string) *claudeDocTestRig {
	t.Helper()
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	svc := service.NewProjectService(d, nil, nil)
	const proj = "proj_claude_doc_test"
	if _, err := d.Exec(
		`INSERT INTO projects (id, name, local_path) VALUES (?, ?, ?)`,
		proj, "doc project", localPath); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	h := NewProjectClaudeDocHandler(svc, nil, store.NewJobStore(8))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/projects/{id}/claude-doc", h.Get)
	mux.HandleFunc("PUT /api/projects/{id}/claude-doc", h.Put)
	mux.HandleFunc("DELETE /api/projects/{id}/claude-doc", h.Delete)
	mux.HandleFunc("POST /api/projects/{id}/claude-doc/generate", h.Generate)
	mux.HandleFunc("POST /api/projects/{id}/claude-doc/import", h.Import)
	mux.HandleFunc("GET /api/projects/{id}/claude-doc/revisions", h.ListRevisions)
	mux.HandleFunc("POST /api/projects/{id}/claude-doc/revisions/{rev_id}/restore", h.RestoreRevision)
	return &claudeDocTestRig{svc: svc, h: h, mux: mux, proj: proj}
}

func (r *claudeDocTestRig) do(t *testing.T, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.SetPathValue("id", r.proj)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

// decodeClaudeDocResp unwraps the standard {success,data} envelope.
func decodeClaudeDocResp(t *testing.T, rec *httptest.ResponseRecorder) claudeDocResponse {
	t.Helper()
	var env struct {
		Success bool                   `json:"success"`
		Data    claudeDocResponse      `json:"data"`
		Error   *struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if !env.Success {
		code := ""
		if env.Error != nil {
			code = env.Error.Code
		}
		t.Fatalf("request failed with code %q: %s", code, rec.Body.String())
	}
	return env.Data
}

// TestClaudeDocGetWithoutDocument pins the "no document yet" contract: a
// 200 with exists=false — NOT a 404. The UI renders an empty state with a
// "生成" button; a 404 would look like a broken endpoint.
func TestClaudeDocGetWithoutDocument(t *testing.T) {
	rig := newClaudeDocTestRig(t, t.TempDir())
	rec := rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	data := decodeClaudeDocResp(t, rec)
	if data.Exists {
		t.Errorf("exists = true, want false")
	}
	if data.Content != "" {
		t.Errorf("content = %q, want empty", data.Content)
	}
}

// TestClaudeDocPutGetDelete walks the manual-edit lifecycle.
func TestClaudeDocPutGetDelete(t *testing.T) {
	rig := newClaudeDocTestRig(t, t.TempDir())

	rec := rig.do(t, "PUT", "/api/projects/"+rig.proj+"/claude-doc", `{"content":"# 约定\n\n用 Go 1.25。"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body: %s", rec.Code, rec.Body.String())
	}

	rec = rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
	data := decodeClaudeDocResp(t, rec)
	if !data.Exists {
		t.Fatal("exists = false after a PUT")
	}
	if data.Content != "# 约定\n\n用 Go 1.25。" {
		t.Errorf("content = %q", data.Content)
	}
	if data.Source != model.ProjectClaudeDocSourceManual {
		t.Errorf("source = %q, want %q", data.Source, model.ProjectClaudeDocSourceManual)
	}
	if data.Bytes == 0 {
		t.Error("bytes = 0")
	}

	rec = rig.do(t, "DELETE", "/api/projects/"+rig.proj+"/claude-doc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, body: %s", rec.Code, rec.Body.String())
	}
	rec = rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
	data = decodeClaudeDocResp(t, rec)
	if data.Exists {
		t.Error("exists = true after a DELETE")
	}
}

// TestClaudeDocPutRejectsEmpty guards against wiping the asset with an
// accidental empty save.
func TestClaudeDocPutRejectsEmpty(t *testing.T) {
	rig := newClaudeDocTestRig(t, t.TempDir())
	rec := rig.do(t, "PUT", "/api/projects/"+rig.proj+"/claude-doc", `{"content":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestClaudeDocGenerateRequiresProjectPath pins the pre-flight check: a
// project whose local directory is gone cannot generate anything, and the
// failure has to be explicit instead of a mysterious CLI error.
func TestClaudeDocGenerateRequiresProjectPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	rig := newClaudeDocTestRig(t, missing)
	rec := rig.do(t, "POST", "/api/projects/"+rig.proj+"/claude-doc/generate", `{"focus":""}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PROJECT_PATH_MISSING") {
		t.Errorf("body = %s, want PROJECT_PATH_MISSING", rec.Body.String())
	}
}

// TestClaudeDocImportFromDisk covers the "项目自带 CLAUDE.md" flow.
func TestClaudeDocImportFromDisk(t *testing.T) {
	dir := t.TempDir()
	rig := newClaudeDocTestRig(t, dir)

	// No file on disk yet → 404 DISK_DOC_NOT_FOUND.
	rec := rig.do(t, "POST", "/api/projects/"+rig.proj+"/claude-doc/import", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}

	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# 磁盘约定"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	rec = rig.do(t, "POST", "/api/projects/"+rig.proj+"/claude-doc/import", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	rec = rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
	data := decodeClaudeDocResp(t, rec)
	if data.Content != "# 磁盘约定" {
		t.Errorf("content = %q", data.Content)
	}
	if data.Source != model.ProjectClaudeDocSourceImported {
		t.Errorf("source = %q, want %q", data.Source, model.ProjectClaudeDocSourceImported)
	}
	if !data.Disk.Exists {
		t.Error("disk.exists = false although the file exists")
	}
	// Importing is the remedy for a diverged pair — after it the two copies
	// must compare equal, otherwise the panel would keep warning.
	if data.Disk.Differs {
		t.Error("disk.differs = true right after importing the disk version")
	}
}

// TestClaudeDocHiddenFromOtherUsers pins the per-project isolation: a user
// without a user_projects grant must not see (or write) the document.
func TestClaudeDocHiddenFromOtherUsers(t *testing.T) {
	rig := newClaudeDocTestRig(t, t.TempDir())
	if _, err := rig.svc.UpsertClaudeDoc(rig.proj, "# 约定", model.ProjectClaudeDocSourceGenerated, true); err != nil {
		t.Fatalf("seed doc: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/projects/"+rig.proj+"/claude-doc", nil)
	req = req.WithContext(middleware.WithUserContext(req.Context(), "usr_other", "other", false, nil))
	req.SetPathValue("id", rig.proj)
	rec := httptest.NewRecorder()
	rig.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestClaudeDocBlockInjection is the injection contract: a project WITH a
// document gets the block, a project WITHOUT one gets an empty string —
// and the empty string is what keeps the prompts byte-for-byte identical
// to the pre-feature behavior.
func TestClaudeDocBlockInjection(t *testing.T) {
	rig := newClaudeDocTestRig(t, t.TempDir())

	if got := claudeDocPromptBlock(rig.svc, rig.proj, t.TempDir()); got != "" {
		t.Errorf("claudeDocBlock without a document = %q, want empty", got)
	}
	if got := claudeDocPromptBlock(nil, rig.proj, ""); got != "" {
		t.Errorf("claudeDocBlock with a nil service = %q, want empty", got)
	}

	if _, err := rig.svc.UpsertClaudeDoc(rig.proj, "# 约定\n\n分层：handler / service / model。", model.ProjectClaudeDocSourceGenerated, true); err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	block := claudeDocPromptBlock(rig.svc, rig.proj, t.TempDir())
	if !strings.Contains(block, prompt.ProjectClaudeDocHeading) {
		t.Errorf("block missing the heading:\n%s", block)
	}
	if !strings.Contains(block, "分层：handler / service / model。") {
		t.Errorf("block missing the content:\n%s", block)
	}
	if strings.Contains(block, "以本块为准") {
		t.Errorf("block claims a disk conflict although the directory has none:\n%s", block)
	}

	// With a CLAUDE.md in the working directory the block must disambiguate.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# 仓库自带的"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	block = claudeDocPromptBlock(rig.svc, rig.proj, dir)
	if !strings.Contains(block, "以本块为准") {
		t.Errorf("block missing the conflict note:\n%s", block)
	}
}

// TestClaudeDocBlockNilServiceIsSafe pins that the helper never panics on a
// partially wired handler (the wizard's projectService is an interface and
// may be nil in some code paths).
func TestClaudeDocBlockNilServiceIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("claudeDocBlock panicked: %v", r)
		}
	}()
	if got := claudeDocPromptBlock(nil, "", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestNormalizeClaudeDocContent pins the cleanup applied to the model's
// raw answer before it is stored.
//
// Two deviations show up in practice:
//   - the model wraps the answer in a ```markdown fence despite the
//     contract saying not to;
//   - the model adds a chat-style lead-in ("This repository is a stub …
//     Here is a CLAUDE.md:") before the document. That lead-in must not be
//     stored — the document is injected verbatim into every AI prompt, so a
//     stray "here you go" paragraph would ride along forever.
//
// The cleanup may only remove formatting noise. Every "keep everything"
// case below is a document the model wrote that the cleanup must NOT
// truncate: a wrong cut DELETES document content, and the survivor is
// stored in the DB and injected into every later stage.
func TestNormalizeClaudeDocContent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain body",
			in:   "# smoke\n\n## Project Overview\n\nText.",
			want: "# smoke\n\n## Project Overview\n\nText.",
		},
		{
			name: "fenced",
			in:   "```markdown\n# smoke\n\n## Project Overview\n```",
			want: "# smoke\n\n## Project Overview",
		},
		{
			name: "preamble before the title",
			in:   "This repository is a stub. Here is a CLAUDE.md:\n\n# smoke\n\n## Tech Stack\n\nGo.",
			want: "# smoke\n\n## Tech Stack\n\nGo.",
		},
		{
			name: "preamble, fenced",
			in:   "Here you go:\n\n```markdown\n# smoke\n\n## Tech Stack\n```",
			want: "# smoke\n\n## Tech Stack",
		},
		{
			name: "no title — keep everything",
			in:   "## Project Overview\n\nText.",
			want: "## Project Overview\n\nText.",
		},
		{
			name: "a document ending with a code block keeps its fences",
			in:   "# smoke\n\n## Commands\n\n```sh\nmake build\n```",
			want: "# smoke\n\n## Commands\n\n```sh\nmake build\n```",
		},
		// ── The regression this rewrite exists for ──
		//
		// A "# " line inside a code fence is a shell / YAML comment, NOT
		// the document title. The previous implementation cut at it and
		// threw away everything before it — the whole document was
		// reduced to the one comment line inside the fence.
		{
			name: "a hash comment inside a code fence is not a title",
			in:   "## Project Overview\n\n正文\n\n```bash\n# install deps\nmake build\n```\n\n## Conventions\n\n分层约定。\n",
			want: "## Project Overview\n\n正文\n\n```bash\n# install deps\nmake build\n```\n\n## Conventions\n\n分层约定。",
		},
		{
			// The cut is made at an H1 title only. This document has no H1
			// (its first heading is an H2), so there is no title to cut at
			// and the lead-in stays — losing a two-sentence lead-in is
			// acceptable, losing the first paragraph of a real document is
			// not. The hash comment inside the fence is untouched either
			// way, which is what this case is really about.
			name: "a hash comment in a fence, preceded by a lead-in",
			in:   "Here is the guide.\n\n## Project Overview\n\n```bash\n# install deps\nmake build\n```\n\n## Conventions\n\n分层约定。",
			want: "Here is the guide.\n\n## Project Overview\n\n```bash\n# install deps\nmake build\n```\n\n## Conventions\n\n分层约定。",
		},
		{
			name: "a document that starts with a paragraph keeps it",
			in:   "This project is a build tool.\n\n```sh\nmake build\n```",
			want: "This project is a build tool.\n\n```sh\nmake build\n```",
		},
		{
			name: "a lead-in followed by a list is document content",
			in:   "- one\n- two\n\n# smoke\n\n## Tech Stack\n\nGo.",
			want: "- one\n- two\n\n# smoke\n\n## Tech Stack\n\nGo.",
		},
		{
			name: "an unterminated fence keeps its content",
			in:   "# smoke\n\n## Commands\n\n```sh\nmake build",
			want: "# smoke\n\n## Commands\n\n```sh\nmake build",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeClaudeDocContent(tc.in); got != tc.want {
				t.Errorf("normalizeClaudeDocContent() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClaudeDocDiskDivergence covers the "the two copies drifted apart" case.
//
// The managed (DB) copy is what every AI stage injects, while the on-disk
// CLAUDE.md is what the user edits and what the Claude CLI auto-loads. When
// they diverge, the user keeps injecting stale conventions without any
// visible sign — the panel used to render the same neutral "a CLAUDE.md
// exists on disk" line in both the identical and the diverged case.
func TestClaudeDocDiskDivergence(t *testing.T) {
	const diskContent = "# 磁盘版本\n\n分层：handler / service / model。\n"

	t.Run("disk file differs from the managed copy", func(t *testing.T) {
		dir := t.TempDir()
		rig := newClaudeDocTestRig(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(diskContent), 0o644); err != nil {
			t.Fatalf("write CLAUDE.md: %v", err)
		}
		// A managed copy that says something else (e.g. an older generation).
		if _, err := rig.svc.UpsertClaudeDoc(rig.proj, "# 旧版本\n", model.ProjectClaudeDocSourceGenerated, true); err != nil {
			t.Fatalf("seed doc: %v", err)
		}

		rec := rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
		data := decodeClaudeDocResp(t, rec)
		if !data.Disk.Exists {
			t.Fatal("disk.exists = false although the file exists")
		}
		if !data.Disk.Differs {
			t.Error("disk.differs = false although the two copies differ")
		}
		if data.Disk.ModifiedAt == nil {
			t.Error("disk.modified_at is nil although the file exists")
		}
	})

	t.Run("disk file matches the managed copy", func(t *testing.T) {
		dir := t.TempDir()
		rig := newClaudeDocTestRig(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(diskContent), 0o644); err != nil {
			t.Fatalf("write CLAUDE.md: %v", err)
		}
		// Exactly what an import stores.
		if _, err := rig.svc.UpsertClaudeDoc(rig.proj, diskContent, model.ProjectClaudeDocSourceImported, false); err != nil {
			t.Fatalf("seed doc: %v", err)
		}

		rec := rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
		data := decodeClaudeDocResp(t, rec)
		if !data.Disk.Exists {
			t.Fatal("disk.exists = false although the file exists")
		}
		if data.Disk.Differs {
			t.Error("disk.differs = true although the two copies are identical")
		}
	})

	t.Run("no managed copy yet", func(t *testing.T) {
		dir := t.TempDir()
		rig := newClaudeDocTestRig(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(diskContent), 0o644); err != nil {
			t.Fatalf("write CLAUDE.md: %v", err)
		}

		rec := rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
		data := decodeClaudeDocResp(t, rec)
		if !data.Disk.Exists {
			t.Fatal("disk.exists = false although the file exists")
		}
		// Nothing to diverge from — the file is simply not imported yet.
		if data.Disk.Differs {
			t.Error("disk.differs = true although there is no managed copy")
		}
	})

	t.Run("managed copy without a disk file", func(t *testing.T) {
		rig := newClaudeDocTestRig(t, t.TempDir())
		if _, err := rig.svc.UpsertClaudeDoc(rig.proj, "# 托管副本\n", model.ProjectClaudeDocSourceGenerated, true); err != nil {
			t.Fatalf("seed doc: %v", err)
		}

		rec := rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
		data := decodeClaudeDocResp(t, rec)
		if data.Disk.Exists {
			t.Error("disk.exists = true although there is no file")
		}
		if data.Disk.Differs {
			t.Error("disk.differs = true although there is no file")
		}
	})
}

// TestClaudeDocGenerateRefusesToOverwriteManualEdits is the P0 guard.
//
// A hand-edited document is the one thing here a human wrote on purpose.
// An AI regeneration replaces it wholesale, so the request is refused
// (409 MANUAL_EDIT_PROTECTED) until the user explicitly confirms — and the
// version that was there is snapshotted, so even a confirmed overwrite is
// recoverable.
func TestClaudeDocGenerateRefusesToOverwriteManualEdits(t *testing.T) {
	rig := newClaudeDocTestRig(t, t.TempDir())
	// A hand-edited document.
	rec := rig.do(t, "PUT", "/api/projects/"+rig.proj+"/claude-doc", `{"content":"# 人工约定"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body: %s", rec.Code, rec.Body.String())
	}

	// Regenerate without the confirmation → refused.
	rec = rig.do(t, "POST", "/api/projects/"+rig.proj+"/claude-doc/generate", `{"focus":""}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "MANUAL_EDIT_PROTECTED") {
		t.Errorf("body = %s, want MANUAL_EDIT_PROTECTED", rec.Body.String())
	}

	// The document is untouched.
	rec = rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
	data := decodeClaudeDocResp(t, rec)
	if data.Content != "# 人工约定" {
		t.Errorf("content = %q, want the hand-edited body", data.Content)
	}
	if data.ManualEditedAt == nil {
		t.Error("manual_edited_at is nil although the document was hand-edited")
	}
}

// TestClaudeDocImportRefusesToOverwriteManualEdits is the same guard for
// the other destructive replace: importing the disk copy over a hand-edited
// managed copy loses the hand edits just the same.
func TestClaudeDocImportRefusesToOverwriteManualEdits(t *testing.T) {
	dir := t.TempDir()
	rig := newClaudeDocTestRig(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# 磁盘版本"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	rec := rig.do(t, "PUT", "/api/projects/"+rig.proj+"/claude-doc", `{"content":"# 人工约定"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body: %s", rec.Code, rec.Body.String())
	}

	rec = rig.do(t, "POST", "/api/projects/"+rig.proj+"/claude-doc/import", `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	rec = rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
	data := decodeClaudeDocResp(t, rec)
	if data.Content != "# 人工约定" {
		t.Errorf("content = %q, want the hand-edited body", data.Content)
	}

	// With the explicit confirmation the import goes through — and the
	// replaced version is kept.
	rec = rig.do(t, "POST", "/api/projects/"+rig.proj+"/claude-doc/import", `{"overwrite_manual":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	rec = rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
	data = decodeClaudeDocResp(t, rec)
	if data.Content != "# 磁盘版本" {
		t.Errorf("content = %q, want the imported body", data.Content)
	}

	rec = rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc/revisions", "")
	var revs []model.ProjectClaudeDocRevision
	if err := json.Unmarshal(rec.Body.Bytes(), &revs); err != nil {
		// The endpoint answers with the envelope.
		var env struct {
			Success bool                             `json:"success"`
			Data    []model.ProjectClaudeDocRevision `json:"data"`
		}
		if err2 := json.Unmarshal(rec.Body.Bytes(), &env); err2 != nil {
			t.Fatalf("decode revisions %q: %v", rec.Body.String(), err2)
		}
		revs = env.Data
	}
	if len(revs) != 1 {
		t.Fatalf("revisions = %+v, want exactly the replaced version", revs)
	}
	if revs[0].Content != "# 人工约定" {
		t.Errorf("revision content = %q, want the hand-edited body", revs[0].Content)
	}
	if revs[0].Reason != model.ProjectClaudeDocRevisionReasonImport {
		t.Errorf("revision reason = %q, want %q", revs[0].Reason, model.ProjectClaudeDocRevisionReasonImport)
	}
}

// TestClaudeDocRestoreRevision pins the recovery path end to end: the
// version that was replaced can be put back.
func TestClaudeDocRestoreRevision(t *testing.T) {
	rig := newClaudeDocTestRig(t, t.TempDir())
	rec := rig.do(t, "PUT", "/api/projects/"+rig.proj+"/claude-doc", `{"content":"# 人工约定"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body: %s", rec.Code, rec.Body.String())
	}
	rev, err := rig.svc.SnapshotClaudeDoc(rig.proj, model.ProjectClaudeDocRevisionReasonRegenerate)
	if err != nil || rev == nil {
		t.Fatalf("snapshot: %v (rev=%+v)", err, rev)
	}
	if _, err := rig.svc.UpsertClaudeDoc(rig.proj, "# AI 约定", model.ProjectClaudeDocSourceGenerated, true); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	rec = rig.do(t, "POST", "/api/projects/"+rig.proj+"/claude-doc/revisions/"+rev.ID+"/restore", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body: %s", rec.Code, rec.Body.String())
	}
	rec = rig.do(t, "GET", "/api/projects/"+rig.proj+"/claude-doc", "")
	data := decodeClaudeDocResp(t, rec)
	if data.Content != "# 人工约定" {
		t.Errorf("content = %q, want the restored body", data.Content)
	}
	if data.ManualEditedAt == nil {
		t.Error("the restored document is not marked as hand-edited")
	}

	// An unknown revision → 404.
	rec = rig.do(t, "POST", "/api/projects/"+rig.proj+"/claude-doc/revisions/pcdr_nope/restore", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestConfirmOverwriteManual pins the guard itself: it lets a replace
// through when there is nothing human-written to lose, refuses it while the
// document carries hand edits, and lets it through again once the user
// confirmed.
func TestConfirmOverwriteManual(t *testing.T) {
	rig := newClaudeDocTestRig(t, t.TempDir())
	proj := &model.Project{ID: rig.proj, Name: "doc project", LocalPath: t.TempDir()}

	// No document at all → allowed.
	rec := httptest.NewRecorder()
	if !rig.h.confirmOverwriteManual(rec, proj, false) {
		t.Errorf("refused without a document: %s", rec.Body.String())
	}

	// A machine-written document → allowed.
	if _, err := rig.svc.UpsertClaudeDoc(rig.proj, "# AI", model.ProjectClaudeDocSourceGenerated, true); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec = httptest.NewRecorder()
	if !rig.h.confirmOverwriteManual(rec, proj, false) {
		t.Errorf("refused for a machine-written document: %s", rec.Body.String())
	}

	// A hand-edited document → refused with 409.
	if _, err := rig.svc.UpsertClaudeDoc(rig.proj, "# 人工", model.ProjectClaudeDocSourceManual, false); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec = httptest.NewRecorder()
	if rig.h.confirmOverwriteManual(rec, proj, false) {
		t.Error("allowed a replace of a hand-edited document")
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MANUAL_EDIT_PROTECTED") {
		t.Errorf("body = %s, want MANUAL_EDIT_PROTECTED", rec.Body.String())
	}

	// The explicit confirmation lets it through.
	rec = httptest.NewRecorder()
	if !rig.h.confirmOverwriteManual(rec, proj, true) {
		t.Errorf("refused despite the confirmation: %s", rec.Body.String())
	}
}

// TestBuildGenerationPromptExistingDocRule pins the wiring between the
// generation root and the prompt's "do not copy the existing document"
// rule.
//
// The rule may only appear when the directory the CLI runs in actually
// holds a CLAUDE.md: the CLI auto-loads that file as project memory, so
// the model needs to be told about it — but telling a model to beware of a
// document that is not there only sends it looking for one.
func TestBuildGenerationPromptExistingDocRule(t *testing.T) {
	h := &ProjectClaudeDocHandler{}

	// No CLAUDE.md in the generation root → no rule.
	empty := t.TempDir()
	prompt, diskExists := h.buildGenerationPrompt("proj", empty, "", false)
	if diskExists {
		t.Error("diskExists = true although the generation root has no CLAUDE.md")
	}
	if strings.Contains(prompt, "既有 CLAUDE.md") {
		t.Errorf("the existing-document rule appears although the generation root has no CLAUDE.md:\n%s", prompt)
	}

	// A CLAUDE.md in the generation root → the rule is there.
	withDoc := t.TempDir()
	if err := os.WriteFile(filepath.Join(withDoc, "CLAUDE.md"), []byte("# 旧文档\n"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	prompt, diskExists = h.buildGenerationPrompt("proj", withDoc, "", false)
	if !diskExists {
		t.Error("diskExists = false although the generation root has a CLAUDE.md")
	}
	if !strings.Contains(prompt, "既有 CLAUDE.md") {
		t.Errorf("the existing-document rule is missing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "不要照抄它") {
		t.Errorf("the existing-document rule does not forbid copying:\n%s", prompt)
	}
}
