package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// newSplitContextFixture wires a WizardHandler that carries the requirement's
// material: a granted directory, an uploaded attachment and (optionally) a
// referenced requirement.
func newSplitContextFixture(t *testing.T, reqID string) (*WizardHandler, string) {
	t.Helper()
	h, _, d := newSplitTestHandler(t, reqID, "requirement", "designed", "# 方案", "", "")

	oldRoot := service.AttachmentRoot()
	root := t.TempDir()
	service.SetAttachmentRoot(root)
	t.Cleanup(func() { service.SetAttachmentRoot(oldRoot) })

	h.dirSvc = service.NewRequirementDirService(d)
	h.attachSvc = service.NewAttachmentService(d)
	return h, root
}

// The split prompt must carry the requirement's own material — the
// attachments, the granted directories and the referenced requirements —
// otherwise the model splits a plan it cannot ground in anything.
func TestSplit_PromptCarriesTheRequirementContext(t *testing.T) {
	h, root := newSplitContextFixture(t, "req_m")

	if _, err := h.dirSvc.Add("req_m", "/tmp/specs", "接口规格", "u1"); err != nil {
		t.Fatalf("grant dir: %v", err)
	}
	if _, err := h.attachSvc.Upload("req_m", model.AttachmentUpload{
		Filename:   "接口文档.md",
		Reader:     strings.NewReader("spec"),
		UploadedBy: "u1",
	}); err != nil {
		t.Fatalf("upload attachment: %v", err)
	}

	type captured struct {
		promptText string
		run        splitRuntime
	}
	got := make(chan captured, 1)
	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		got <- captured{promptText: promptText, run: run}
		return []prompt.SplitTask{{Title: "子需求 A", Order: 1}}, nil, ""
	}

	status, jobID := splitStartJob(t, h, "req_m", "{}")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if jobID == "" {
		t.Fatal("the response carries no job_id")
	}

	var c captured
	select {
	case c = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the split executor was never called")
	}

	for _, want := range []string{
		"## 需求上下文",
		"📎 已附加文件",
		"接口文档.md",
		"📂 已授权读取目录",
		"/tmp/specs",
	} {
		if !strings.Contains(c.promptText, want) {
			t.Errorf("the split prompt is missing %q:\n%s", want, c.promptText)
		}
	}
	// The split disallows Bash — the directory manifest must not suggest it.
	if strings.Contains(c.promptText, "Bash `ls`") {
		t.Errorf("the split prompt suggests a disabled tool:\n%s", c.promptText)
	}

	// The grants must reach the execution layer: the directories the manifest
	// names are useless without the matching --add-dir.
	wantAttachDir := filepath.Join(root, "req_m")
	foundDir, foundAttach := false, false
	for _, d := range c.run.AddDirs {
		if d == "/tmp/specs" {
			foundDir = true
		}
		if d == wantAttachDir {
			foundAttach = true
		}
	}
	if !foundDir {
		t.Errorf("the granted directory is missing from the --add-dir list %v", c.run.AddDirs)
	}
	if !foundAttach {
		t.Errorf("the attachment directory is missing from the --add-dir list %v", c.run.AddDirs)
	}
	if c.run.AttachDir != wantAttachDir {
		t.Errorf("AttachDir = %q, want %q", c.run.AttachDir, wantAttachDir)
	}
}

// A requirement without any material produces no context block at all — the
// prompt must stay exactly what it was.
func TestSplit_WithoutMaterialProducesNoContextBlock(t *testing.T) {
	h, _ := newSplitContextFixture(t, "req_m")

	type captured struct{ promptText string }
	got := make(chan captured, 1)
	h.splitExec = func(job *store.Job, reqRow *model.Requirement, target splitExecTarget,
		promptText, systemPrompt, modelName, configID string, run splitRuntime, usage *usageCtx) ([]prompt.SplitTask, []prompt.SplitReference, string) {
		got <- captured{promptText: promptText}
		return []prompt.SplitTask{{Title: "子需求 A", Order: 1}}, nil, ""
	}

	if status, _ := splitStartJob(t, h, "req_m", "{}"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var c captured
	select {
	case c = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the split executor was never called")
	}
	if strings.Contains(c.promptText, "## 需求上下文") {
		t.Errorf("a requirement without material produced a context block:\n%s", c.promptText)
	}
}

// The children inherit the master's granted directories: the access the user
// authorized for the master must survive the delegation of development.
func TestConfirmSplit_ChildrenInheritTheMasterDirectories(t *testing.T) {
	h, _ := newSplitContextFixture(t, "req_m")

	if _, err := h.dirSvc.Add("req_m", "/tmp/specs", "接口规格", "u1"); err != nil {
		t.Fatalf("grant dir: %v", err)
	}

	body := `{"tasks":[{"title":"子需求 A","order":1,"depends_on":[]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data struct {
			Children []struct {
				ID string `json:"id"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Data.Children) != 1 {
		t.Fatalf("got %d children, want 1", len(resp.Data.Children))
	}
	childID := resp.Data.Children[0].ID

	dirs, err := h.dirSvc.List(childID)
	if err != nil {
		t.Fatalf("list child dirs: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("the child holds %d directories, want 1: %+v", len(dirs), dirs)
	}
	if dirs[0].Path != "/tmp/specs" || dirs[0].Label != "接口规格" {
		t.Errorf("inherited directory = %+v, want /tmp/specs with the master's label", dirs[0])
	}
	if dirs[0].AddedBy != splitInheritedDirAddedBy {
		t.Errorf("added_by = %q, want %q", dirs[0].AddedBy, splitInheritedDirAddedBy)
	}
}

// The master's own grants are untouched by the inheritance.
func TestConfirmSplit_InheritanceLeavesTheMasterUntouched(t *testing.T) {
	h, _ := newSplitContextFixture(t, "req_m")

	if _, err := h.dirSvc.Add("req_m", "/tmp/specs", "接口规格", "u1"); err != nil {
		t.Fatalf("grant dir: %v", err)
	}
	body := `{"tasks":[{"title":"子需求 A","order":1,"depends_on":[]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/requirements/req_m/split/confirm", strings.NewReader(body))
	req.SetPathValue("id", "req_m")
	rec := httptest.NewRecorder()
	h.ConfirmSplit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	dirs, err := h.dirSvc.List("req_m")
	if err != nil {
		t.Fatalf("list master dirs: %v", err)
	}
	if len(dirs) != 1 || dirs[0].AddedBy == splitInheritedDirAddedBy {
		t.Errorf("the master's own directory row was modified: %+v", dirs)
	}
}
