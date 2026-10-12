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
		stream, err := reader.GetPlainText()
		if err != nil {
			return "", fmt.Errorf("read pdf text: %w", err)
		}
		if stream == nil {
			return "", nil
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, stream); err != nil {
			return "", fmt.Errorf("buffer pdf text: %w", err)
		}
		return buf.String(), nil
	}
}

// GetText opens the PDF at the given filesystem path and returns the
// concatenated plain-text content of every page. Pages are joined with no
// separator — the caller is expected to feed the result to an LLM that
// will re-segment into Markdown.
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
// Content-Length reported in the multipart.FileHeader.
//
// An empty reader (sizeBytes == 0) returns ErrEmptyPdfInput.
func GetTextFromReader(r io.ReaderAt, sizeBytes int64) (string, error) {
	if sizeBytes == 0 {
		return "", ErrEmptyPdfInput
	}
	extractOnce.Do(initExtractFunc)
	return extractFunc(r, sizeBytes)
}
