package handler

// requirement_parse.go — POST /api/requirements/parse-file
//
// Stateless RPC: accepts a multipart upload, detects file type by extension,
// extracts the text content, and returns it as Markdown-ready text for the
// frontend to pre-fill the requirement description box.
//
// Supported formats:
//   .md / .markdown / .txt  — raw bytes returned as-is
//   .pdf                    — util.GetTextFromReader (pure-Go ledongthuc/pdf)
//   .docx                   — util.ExtractDocxText (stdlib-only ZIP + XML walk)
//
// No database writes — pure text extraction.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/novaworkbench/backend/internal/util"
)

const (
	parseFileMaxMemory = 32 << 20 // 32 MB in-memory threshold
	parseFileMaxBody   = 32 << 20 // 32 MB hard request cap
	parseFileMaxChars  = 50_000   // truncate extracted text beyond this
)

// ParseFile — POST /api/requirements/parse-file (multipart/form-data, field "file").
func (h *RequirementHandler) ParseFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, parseFileMaxBody)

	if err := r.ParseMultipartForm(parseFileMaxMemory); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "TOO_LARGE",
				fmt.Sprintf("File exceeds %d MB limit", parseFileMaxBody>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_FORM",
			"Failed to parse multipart form: "+err.Error())
		return
	}

	f, fh, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_FILE",
			"Form field \"file\" is required: "+err.Error())
		return
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(fh.Filename))

	var content string
	switch ext {
	case ".md", ".markdown", ".txt":
		raw, err := io.ReadAll(f)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "READ_FAILED",
				"Failed to read file: "+err.Error())
			return
		}
		content = string(raw)

	case ".pdf":
		text, err := util.GetTextFromReader(f, fh.Size)
		if err != nil {
			if errors.Is(err, util.ErrEmptyPdfInput) {
				writeError(w, http.StatusBadRequest, "INVALID_FILE",
					"PDF is empty or contains no extractable text")
				return
			}
			writeError(w, http.StatusBadRequest, "EXTRACT_FAILED",
				"Failed to extract text from PDF: "+err.Error())
			return
		}
		content = text

	case ".docx":
		raw, err := io.ReadAll(f)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "READ_FAILED",
				"Failed to read file: "+err.Error())
			return
		}
		text, err := util.ExtractDocxText(raw)
		if err != nil {
			if errors.Is(err, util.ErrEmptyDocx) {
				writeError(w, http.StatusBadRequest, "INVALID_FILE",
					"DOCX contains no extractable text")
				return
			}
			writeError(w, http.StatusBadRequest, "EXTRACT_FAILED",
				"Failed to extract text from DOCX: "+err.Error())
			return
		}
		content = text

	default:
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_FORMAT",
			fmt.Sprintf("Unsupported file format %q; accepted: .md, .markdown, .txt, .pdf, .docx", ext))
		return
	}

	// Truncate to prevent absurdly large payloads filling the description box.
	runes := []rune(content)
	if len(runes) > parseFileMaxChars {
		content = string(runes[:parseFileMaxChars])
	}

	writeJSON(w, http.StatusOK, map[string]string{"content": content})
}
