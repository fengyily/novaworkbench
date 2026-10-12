package util

// pdf_extract.go — PDF → plain-text extraction used by the knowledge-base
// "upload PDF" pipeline (handler/knowledge.go → service/knowledge.go → here).
//
// Default implementation uses the pure-Go library github.com/ledongthuc/pdf
// (no external binary required; no shell-out). The seam is
// `extractFunc` below, so future replacements (e.g. shelling out to
// poppler-utils' pdftotext) only have to swap the function value assigned at
// init time and re-run go mod tidy — no caller changes anywhere else in
// the codebase.
//
// IMPORTANT — shell-out implementation rules (apply to any future variant):
//   * The exec.Command context MUST be bounded (30s is the convention in
//     this repo — see backend/internal/handler/runner.go and preflight/
//     install.go for the same pattern).
//   * stderr MUST be redirected to /dev/null (or io.Discard), NEVER to the
//     parent process stderr: a malformed PDF that floods stderr would
//     surface in the nova server log and confuse operators.
//   * Any temp file written for stdin MUST go through os.CreateTemp("", ...)
//     and be os.Remove'd in defer. NEVER write a temp file into the project
//     root or into ~/.novaworkbench/ — those locations have other
//     ownership (see util/tmp.go DiagramsDir for the canonical pattern).
//
// This file does NOT render Markdown. It only returns the raw text
// extracted from the PDF. Polishing raw text into structured Markdown
// happens in a later stage via the LLM gateway.
//
// Multi-page robustness:
//   * Reader.GetPlainText() in ledongthuc/pdf does call Page.GetPlainText()
//     per page in a loop, but if the very first page's decoder panics
//     (recoverTo resets the result and propagates the error), the whole
//     extraction aborts and the row is persisted with zero text — or if a
//     later page's decoder panics, every page after that point is silently
//     dropped. The user-visible symptom (kb_68170a49c45714b1) was "row
//     contains only page 1 of a multi-page PDF". The version below iterates
//     pages explicitly with per-page panic/error isolation, so one bad page
//     can no longer truncate a multi-page upload.
//   * A null page entry (Page{} returned by Reader.Page when the page tree
//     can't resolve `num`, e.g. /Count is wrong or /Kids is malformed) is
//     skipped instead of silently written as "" and causing the page count
//     to drift.
//   * Pages are joined with "\n\f\n" — a form-feed flanked by newlines.
//     The downstream LLM polish step sees this as a page boundary; "\f"
//     is the canonical PDF-page-break convention so the markdown emerges
//     with one block per page even when the LLM refuses to add headings.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/ledongthuc/pdf"
)

// ErrEmptyPdfInput is returned when a caller hands us a zero-byte input.
// The ledongthuc/pdf library would otherwise report this as a low-level
// "missing startxref" or similar; we surface a stable error so the handler
// can map it to a 400 INVALID_FILE.
var ErrEmptyPdfInput = errors.New("pdf_extract: empty input")

// pdfPageSeparator is the boundary written between two successfully
// extracted pages. "\f" (form-feed) is the canonical PDF page-break
// character; flanking newlines make sure the LLM prompt path sees a
// visible blank line on either side and parses the boundary as its own
// paragraph instead of mashing two pages together.
const pdfPageSeparator = "\n\f\n"

// extractFunc is the swappable implementation behind GetText / GetTextFromReader.
// Reassigning it (e.g. from a future pdftotext-backed variant) is a one-line
// change. sync.Once protects against accidental re-init from concurrent
// callers.
var (
	extractOnce sync.Once
	extractFunc func(r io.ReaderAt, size int64) (string, error)
)

// initExtractFunc wires the default ledongthuc/pdf-backed extractor.
// We only ever run it once per process; replacing extractFunc at runtime
// is not supported (callers should re-compile).
func initExtractFunc() {
	extractFunc = func(r io.ReaderAt, size int64) (string, error) {
		reader, err := pdf.NewReader(r, size)
		if err != nil {
			return "", fmt.Errorf("open pdf: %w", err)
		}

		// Iterate pages explicitly (rather than calling
		// reader.GetPlainText() once) so a single bad page can no
		// longer abort the whole extraction. The library's GetPlainText
		// does iterate pages but propagates per-page errors and lets
		// panics from Page.GetPlainText's recoverTo bubble up; here we
		// log-and-continue on each page failure instead.
		totalPages := reader.NumPage()
		var out bytes.Buffer
		extractedPages := 0
		for i := 1; i <= totalPages; i++ {
			p := reader.Page(i)
			if p.V.IsNull() {
				// Page tree couldn't resolve this index (rare; happens
				// when /Count is wrong or /Kids is malformed). Skip
				// rather than abort.
				continue
			}
			// Page.GetPlainText accepts nil for fonts and falls back to
			// the library's per-page font cache internally — we can't
			// pass the shared cache across pages because fontCache() is
			// unexported, and re-parsing the per-page fonts is cheap
			// enough not to bother shimming.
			text, err := p.GetPlainText(nil)
			if err != nil {
				// Per-page failure (panic recovered to error, malformed
				// CMap, broken XObject ref). Continue with the
				// remaining pages so the row still gets the working
				// ones — the previous behaviour aborted here and lost
				// every page after the first failure.
				fmt.Printf("[pdf_extract] page %d/%d: %v (continuing with remaining pages)\n", i, totalPages, err)
				continue
			}
			if extractedPages > 0 {
				out.WriteString(pdfPageSeparator)
			}
			out.WriteString(text)
			extractedPages++
		}
		if extractedPages == 0 && totalPages > 0 {
			// Every page failed. Surface a single error rather than a
			// blank string so the handler can map it to 400 INVALID_FILE
			// (the "all-pages-failed" case is genuinely a bad PDF).
			return "", fmt.Errorf("pdf_extract: all %d pages failed extraction", totalPages)
		}
		return out.String(), nil
	}
}

// GetText opens the PDF at the given filesystem path and returns the
// concatenated plain-text content of every page. Pages are joined with
// "\n\f\n" so downstream consumers can split pages losslessly and the
// LLM polish step can treat each page as its own block.
//
// Returns ErrEmptyPdfInput for a zero-byte file, and a wrapped error
// (with %w) for any other failure so the handler can log it.
func GetText(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat pdf: %w", err)
	}
	if info.Size() == 0 {
		return "", ErrEmptyPdfInput
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}
	defer f.Close()
	return GetTextFromReader(f, info.Size())
}

// GetTextFromReader parses a PDF from an arbitrary io.ReaderAt. sizeBytes
// is the total byte length the reader will produce — ledongthuc/pdf (and
// the underlying rsc.io/pdf) require a known size to set up random access
// over the file.
//
// When called from a multipart upload handler, sizeBytes is the
// Content.Length reported in the multipart.FileHeader.
//
// An empty reader (sizeBytes == 0) returns ErrEmptyPdfInput.
func GetTextFromReader(r io.ReaderAt, sizeBytes int64) (string, error) {
	if sizeBytes == 0 {
		return "", ErrEmptyPdfInput
	}
	extractOnce.Do(initExtractFunc)
	return extractFunc(r, sizeBytes)
}
