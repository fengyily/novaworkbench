package handler

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// 跨项目需求的附件：路由 + 落库 + 提示词可达性。
//
// 这三件事必须一起成立：
//
//  1. 附件能挂到一条跨项目需求上（cross_requirement_attachments 表，owner 列
//     cross_requirement_id）—— 走的是与需求 / Review 完全同一套 handler，
//     所以这里只测「换个 owner 也通」。
//  2. 附件清单与描述里的 nova:// 标记能到达跨项目作业的提示词 —— 用户在需求
//     里附了什么，AI 就该看到什么。
//  3. 提示词里给的是**真实磁盘路径**，不是模型打不开的 nova:// 协议串。

// newCrossAttachFixture wires the cross-requirement handler together with a
// cross-requirement-scoped attachment service over a throwaway DB, and points
// the (package-global) attachment root at a temp dir.
func newCrossAttachFixture(t *testing.T) (*CrossRequirementHandler, *service.AttachmentService, *service.CrossRequirementService, *db.DB) {
	t.Helper()
	oldRoot := service.AttachmentRoot()
	root := t.TempDir()
	service.SetAttachmentRoot(root)
	t.Cleanup(func() { service.SetAttachmentRoot(oldRoot) })

	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	projSvc := service.NewProjectService(d, nil, nil)
	reqSvc := service.NewRequirementService(d, nil)
	crossSvc := service.NewCrossRequirementService(d)
	attachSvc := service.NewCrossRequirementAttachmentService(d)
	h := NewCrossRequirementHandler(crossSvc, reqSvc, projSvc, nil, nil, nil, nil, nil, nil, nil, attachSvc)
	return h, attachSvc, crossSvc, d
}

// seedCrossAttachProject inserts a project row directly — ProjectService.Add
// insists on a real git checkout, which this path never touches.
func seedCrossAttachProject(t *testing.T, d *db.DB, name string) string {
	t.Helper()
	id := "proj_" + name
	now := time.Now()
	if _, err := d.Exec(
		`INSERT INTO projects (id,name,local_path,status,added_at,updated_at) VALUES (?,?,?, 'active', ?, ?)`,
		id, name, filepath.Join(t.TempDir(), name), now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id
}

// uploadCrossAttachment posts one file to the cross-requirement attachment
// endpoint and returns the recorder.
func uploadCrossAttachment(t *testing.T, h *service.AttachmentService, ownerID, filename, content string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/cross-requirements/"+ownerID+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", ownerID)
	rec := httptest.NewRecorder()
	NewAttachmentHandler(h).Upload(rec, req)
	return rec
}

// 附件能挂到跨项目需求上：上传 → 列表 → 取内容 → 删除。
func TestCrossRequirementAttachmentLifecycle(t *testing.T) {
	_, attachSvc, crossSvc, d := newCrossAttachFixture(t)
	projID := seedCrossAttachProject(t, d, "web-app")

	x, err := crossSvc.Create(model.CreateCrossRequirementReq{
		Title:      "跨仓库订单重构",
		ProjectIDs: []string{projID},
	})
	if err != nil {
		t.Fatalf("create cross requirement: %v", err)
	}

	rec := uploadCrossAttachment(t, attachSvc, x.ID, "err.log", "boom\n")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	atts, err := attachSvc.List(x.ID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(atts))
	}
	if atts[0].OwnerID != x.ID {
		t.Fatalf("owner = %q, want %q", atts[0].OwnerID, x.ID)
	}
	if atts[0].Filename != "err.log" {
		t.Fatalf("filename = %q, want err.log", atts[0].Filename)
	}

	// 内容端点要能取回字节（前端插图靠它渲染）。
	contentReq := httptest.NewRequest("GET", "/api/cross-requirements/"+x.ID+"/attachments/"+atts[0].ID+"/content", nil)
	contentReq.SetPathValue("id", x.ID)
	contentReq.SetPathValue("att_id", atts[0].ID)
	contentRec := httptest.NewRecorder()
	NewAttachmentHandler(attachSvc).Content(contentRec, contentReq)
	if contentRec.Code != http.StatusOK {
		t.Fatalf("content status = %d, want 200", contentRec.Code)
	}
	if contentRec.Body.String() != "boom\n" {
		t.Fatalf("content = %q, want %q", contentRec.Body.String(), "boom\n")
	}

	// 删除：行没了，文件也没了。
	delReq := httptest.NewRequest("DELETE", "/api/cross-requirements/"+x.ID+"/attachments/"+atts[0].ID, nil)
	delReq.SetPathValue("id", x.ID)
	delReq.SetPathValue("att_id", atts[0].ID)
	delRec := httptest.NewRecorder()
	NewAttachmentHandler(attachSvc).Delete(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204: %s", delRec.Code, delRec.Body.String())
	}
	atts, _ = attachSvc.List(x.ID)
	if len(atts) != 0 {
		t.Fatalf("attachments after delete = %d, want 0", len(atts))
	}
}

// 别的需求的附件不能通过本需求的端点取走（owner 作用域）。
func TestCrossRequirementAttachmentIsOwnerScoped(t *testing.T) {
	_, attachSvc, crossSvc, d := newCrossAttachFixture(t)
	projID := seedCrossAttachProject(t, d, "web-app")

	mine, _ := crossSvc.Create(model.CreateCrossRequirementReq{
		Title: "我的", ProjectIDs: []string{projID},
	})
	theirs, _ := crossSvc.Create(model.CreateCrossRequirementReq{
		Title: "别人的", ProjectIDs: []string{projID},
	})

	rec := uploadCrossAttachment(t, attachSvc, mine.ID, "secret.log", "hidden\n")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", rec.Code)
	}
	atts, _ := attachSvc.List(mine.ID)
	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(atts))
	}

	// 用别人的 id 去取我的附件 → 404。
	req := httptest.NewRequest("GET", "/api/cross-requirements/"+theirs.ID+"/attachments/"+atts[0].ID+"/content", nil)
	req.SetPathValue("id", theirs.ID)
	req.SetPathValue("att_id", atts[0].ID)
	rec2 := httptest.NewRecorder()
	NewAttachmentHandler(attachSvc).Content(rec2, req)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("cross-owner content status = %d, want 404", rec2.Code)
	}
}

// 删除跨项目需求时，附件行必须一起消失。
//
// SQLite 从不开 PRAGMA foreign_keys，schema 里的 ON DELETE CASCADE 是装饰 ——
// 不显式删就留下孤儿行。
func TestCrossRequirementDeleteRemovesAttachmentRows(t *testing.T) {
	_, attachSvc, crossSvc, d := newCrossAttachFixture(t)
	projID := seedCrossAttachProject(t, d, "web-app")

	x, _ := crossSvc.Create(model.CreateCrossRequirementReq{
		Title: "跨仓库订单重构", ProjectIDs: []string{projID},
	})
	rec := uploadCrossAttachment(t, attachSvc, x.ID, "err.log", "boom\n")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", rec.Code)
	}

	// 删除是两步：软删进回收站，再彻底删除。附件行在 purge 时才消失。
	if err := crossSvc.MoveToTrash(x.ID); err != nil {
		t.Fatalf("move to trash: %v", err)
	}
	if err := crossSvc.Purge(x.ID); err != nil {
		t.Fatalf("purge cross requirement: %v", err)
	}
	atts, err := attachSvc.List(x.ID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(atts) != 0 {
		t.Fatalf("attachment rows survived the delete: %+v", atts)
	}
}

// 附件清单与 nova:// 标记要到达跨项目作业的提示词。
//
// 这是「用户附了什么，AI 就该看到什么」的落点：清单里是真实磁盘路径，
// 描述里的 nova://attachment/<id> 也被换成了同一个真实路径。
func TestCrossRequirementPromptCarriesAttachments(t *testing.T) {
	h, attachSvc, crossSvc, d := newCrossAttachFixture(t)
	projID := seedCrossAttachProject(t, d, "web-app")

	x, _ := crossSvc.Create(model.CreateCrossRequirementReq{
		Title:      "跨仓库订单重构",
		ProjectIDs: []string{projID},
	})
	rec := uploadCrossAttachment(t, attachSvc, x.ID, "err.log", "boom\n")
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", rec.Code)
	}
	atts, _ := attachSvc.List(x.ID)
	attID := atts[0].ID

	// 描述里带一个插图标记（前端粘贴截图后写进去的形态）。
	x.Description = "这是报错截图：\n\n![image](nova://attachment/" + attID + ")\n"

	prompt := h.crossDesignPrompt(x, nil, "", nil)

	if !strings.Contains(prompt, "已附加文件") || !strings.Contains(prompt, "err.log") {
		t.Fatalf("prompt lost the attachment manifest: %q", prompt)
	}
	if strings.Contains(prompt, "nova://attachment/") {
		t.Fatalf("prompt still carries an unrewritten nova:// marker: %q", prompt)
	}
	// 标记指向的必须是真的落盘路径（attachment 根目录下的那个文件）。
	abs := attachSvc.AbsolutePath(&atts[0])
	if !strings.Contains(prompt, abs) {
		t.Fatalf("prompt does not carry the on-disk path %q: %q", abs, prompt)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("attachment file is missing on disk: %v", err)
	}
}
