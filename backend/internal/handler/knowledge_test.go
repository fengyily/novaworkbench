package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/service"
)

// newUploadPdfTestHandler wires a KnowledgeHandler against a real SQLite DB
// (one project row) and stubs the PDF extract / LLM polish steps so the
// tests never touch a real PDF parser or HTTP LLM. The stub closures match
// the production signatures used by KnowledgeService.CreateFromPdf.
func newUploadPdfTestHandler(t *testing.T) *KnowledgeHandler {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Init(db.Config{Driver: string(db.SQLite), SQLitePath: path})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { d.Close(); os.Remove(path) })
	if _, err := d.Exec("INSERT INTO projects (id, name, local_path) VALUES ('proj_1', 'Test', '/tmp/proj1')"); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	svc := service.NewKnowledgeService(d)
	// llm.New(nil, nil) → llmCfg is nil; the real ConvertPdfTextToMarkdown
	// would error on missing config, but the handler uses the injected
	// h.pdfPolish default of h.llm.ConvertPdfTextToMarkdown only when
	// h.pdfPolish is nil. We override both closures below, so this Gateway
	// is never actually invoked.
	gw := llm.New(nil, nil)
	h := NewKnowledgeHandler(svc, gw, nil)
	h.pdfExtract = func(path string) (string, error) {
		return "raw extracted text from " + filepath.Base(path), nil
	}
	h.pdfPolish = func(rawText, file string) (string, *llm.Usage, error) {
		return "# " + file + "\n\n" + rawText, &llm.Usage{
			PromptTokens: 12, CompletionTokens: 34, Model: "test-model",
		}, nil
	}
	return h
}

// buildUploadRequest assembles a multipart/form-data POST with a single
// "file" field and an optional "project_id" field, sized exactly to the
// desired payload. The per-part Content-Type is set explicitly so a
// "text/plain" filename can be paired with a spoofed PDF content-type
// (the second leg of the handler's type validation) and the success path
// can be exercised with application/pdf. The returned request is ready
// to feed into UploadPdf.
func buildUploadRequest(t *testing.T, filename, contentType, projectID string, body []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if projectID != "" {
		if err := w.WriteField("project_id", projectID); err != nil {
			t.Fatalf("write project_id field: %v", err)
		}
	}
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	hdr.Set("Content-Type", contentType)
	fw, err := w.CreatePart(hdr)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/upload-pdf", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestUploadPdf_Success_Returns201(t *testing.T) {
	h := newUploadPdfTestHandler(t)

	pdfBody := []byte("%PDF-1.4\nfake pdf body for unit test\n%%EOF")
	req := buildUploadRequest(t, "manual.pdf", "application/pdf", "proj_1", pdfBody)
	w := httptest.NewRecorder()

	h.UploadPdf(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d, body=%s", w.Code, w.Body.String())
	}
	var resp APIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success=true, got body=%s", w.Body.String())
	}
	// The data payload must carry the same KnowledgeItem fields the
	// production POST /api/knowledge returns — id, project_id, title,
	// content, source_type, source_ref, etc.
	raw, err := json.Marshal(resp.Data)
	if err != nil {
		t.Fatalf("re-marshal data: %v", err)
	}
	for _, want := range []string{
		`"project_id":"proj_1"`,
		`"title":"manual.pdf"`,         // polished H1 "# manual.pdf" → "manual.pdf" (md# stripped)
		`"source_type":"pdf_upload"`,   // produced by the service
		`"category":"document"`,        // produced by the service
		`"source_ref":"pdf:`,           // pdf: namespace prefix
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("expected response to contain %s, got %s", want, raw)
		}
	}
	// Title is supposed to come from the polished markdown's first header
	// ("manual.pdf" after the H1 `#` is stripped by service.pdfKnowledgeTitle),
	// not the raw-filename fallback ("PDF: manual.pdf"). Guard against that
	// regression.
	if bytes.Contains(raw, []byte(`"title":"PDF:`)) {
		t.Errorf("expected polished H1 title, got raw-filename fallback: %s", raw)
	}
}

func TestUploadPdf_NonPdfContentType_Returns400(t *testing.T) {
	h := newUploadPdfTestHandler(t)

	// Per-part Content-Type is text/plain — the handler's first leg of
	// type validation should reject it before any extraction.
	body := []byte("module example.com/foo\n\ngo 1.25\n")
	req := buildUploadRequest(t, "go.mod", "text/plain", "proj_1", body)

	w := httptest.NewRecorder()
	h.UploadPdf(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("INVALID_FILE_TYPE")) {
		t.Errorf("expected INVALID_FILE_TYPE code, got %s", w.Body.String())
	}
}

func TestUploadPdf_WrongExtension_Returns400(t *testing.T) {
	h := newUploadPdfTestHandler(t)

	// Content-Type claims PDF but the file extension is .txt — second leg
	// of the type-validation should catch it. We need to manually craft
	// the multipart body so the per-part Content-Type is application/pdf
	// (CreateFormFile only accepts a default content type).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("project_id", "proj_1"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="notes.txt"`)
	hdr.Set("Content-Type", "application/pdf") // spoofed
	fw, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := io.WriteString(fw, "%PDF-1.4\nfake\n%%EOF"); err != nil {
		t.Fatalf("write part: %v", err)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/upload-pdf", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	w := httptest.NewRecorder()
	h.UploadPdf(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("INVALID_FILE_TYPE")) {
		t.Errorf("expected INVALID_FILE_TYPE code, got %s", w.Body.String())
	}
}

func TestUploadPdf_OversizeBody_Returns413(t *testing.T) {
	h := newUploadPdfTestHandler(t)

	// pdfUploadMaxBody is 50 MB. Build a 60 MB body to overflow the
	// MaxBytesReader cap and surface a *http.MaxBytesError from
	// ParseMultipartForm. Allocating 60 MB is fine for a one-shot test.
	oversize := bytes.Repeat([]byte("A"), 60<<20)
	req := buildUploadRequest(t, "huge.pdf", "application/pdf", "proj_1", oversize)

	w := httptest.NewRecorder()
	h.UploadPdf(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("TOO_LARGE")) {
		t.Errorf("expected TOO_LARGE code, got %s", w.Body.String())
	}
}

func TestUploadPdf_MissingProjectID_Returns400(t *testing.T) {
	h := newUploadPdfTestHandler(t)

	pdfBody := []byte("%PDF-1.4\nfine\n%%EOF")
	req := buildUploadRequest(t, "x.pdf", "application/pdf", "", pdfBody)
	w := httptest.NewRecorder()
	h.UploadPdf(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("INVALID_PROJECT_ID")) {
		t.Errorf("expected INVALID_PROJECT_ID code, got %s", w.Body.String())
	}
}

func TestUploadPdf_MissingFile_Returns400(t *testing.T) {
	h := newUploadPdfTestHandler(t)

	// Multipart body with project_id but no "file" field.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("project_id", "proj_1"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/upload-pdf", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	w := httptest.NewRecorder()
	h.UploadPdf(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("INVALID_FILE")) {
		t.Errorf("expected INVALID_FILE code, got %s", w.Body.String())
	}
}

// TestUploadPdf_TempFileRemoved — the handler must os.Remove its spill
// temp file even on the success path. Asserting on a missing side-effect
// (the file should be gone) is what makes the cleanup contract testable.
func TestUploadPdf_TempFileRemoved(t *testing.T) {
	h := newUploadPdfTestHandler(t)

	// Wrap the stub extract to capture the temp file path and assert on
	// its existence while the pipeline is still in flight, then again
	// after the handler returns.
	var capturedPath string
	h.pdfExtract = func(path string) (string, error) {
		capturedPath = path
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected temp file at %s while pipeline in flight, got %v", path, err)
		}
		return "ok", nil
	}

	pdfBody := []byte("%PDF-1.4\nok\n%%EOF")
	req := buildUploadRequest(t, "x.pdf", "application/pdf", "proj_1", pdfBody)
	w := httptest.NewRecorder()
	h.UploadPdf(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d, body=%s", w.Code, w.Body.String())
	}
	if capturedPath == "" {
		t.Fatal("extract stub was not invoked")
	}
	// Give the OS a moment to actually unlink in case the runtime defers
	// the close. A 50 ms sleep is overkill; a sync check would also work
	// but Go's runtime is unpredictable about when deferred removals flush
	// across test boundaries.
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if _, err := os.Stat(capturedPath); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("temp file %s still present after handler returned", capturedPath)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
