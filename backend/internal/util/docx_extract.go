package util

// docx_extract.go — DOCX → Markdown text extraction using stdlib only.
//
// A .docx file is a ZIP archive; the primary content lives in
// word/document.xml. We walk the XML tree looking for paragraph (<w:p>),
// paragraph-style (<w:pStyle>), and text-run (<w:t>) elements to produce a
// lightweight Markdown representation.
//
// Heading detection: a paragraph whose <w:pStyle w:val="..."> is "Heading1"
// through "Heading6" is emitted as the corresponding number of "#" characters.
// All other paragraphs are emitted as plain text separated by blank lines.
// Table cells and text boxes are treated as regular paragraphs.
//
// This does NOT handle:
//   - images, charts, OLE objects (silently skipped)
//   - complex field codes (MERGEFORMAT etc) — just the plain text is taken
//   - tracked changes — both old and new text appear (XML walk order)
//
// The result is a best-effort extraction suitable for pre-filling a
// requirement description; users are expected to review and edit it.

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// ErrEmptyDocx is returned when the DOCX archive contains no extractable text.
var ErrEmptyDocx = fmt.Errorf("docx_extract: document contains no extractable text")

// ExtractDocxText reads a DOCX file from data (the raw bytes of the .docx
// file) and returns a Markdown-flavored plain-text representation.
func ExtractDocxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx_extract: open zip: %w", err)
	}

	// Find word/document.xml — the primary document body.
	var docFile *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docFile = f
			break
		}
	}
	if docFile == nil {
		return "", fmt.Errorf("docx_extract: word/document.xml not found in archive")
	}

	rc, err := docFile.Open()
	if err != nil {
		return "", fmt.Errorf("docx_extract: open document.xml: %w", err)
	}
	defer rc.Close()

	content, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("docx_extract: read document.xml: %w", err)
	}

	result, err := parseDocumentXML(content)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(result) == "" {
		return "", ErrEmptyDocx
	}
	return result, nil
}

// parseDocumentXML walks the XML event stream and accumulates paragraphs.
func parseDocumentXML(data []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))

	var (
		paragraphs  []string
		paraBuf     strings.Builder
		headingLevel int    // 0 = not a heading, 1-6 = heading level
		inPStyle    bool
		inWt        bool
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx_extract: xml decode: %w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch localName(t.Name) {
			case "p":
				// New paragraph: reset state.
				paraBuf.Reset()
				headingLevel = 0
			case "pStyle":
				inPStyle = true
				// Read the w:val attribute to detect heading levels.
				for _, attr := range t.Attr {
					if localName(attr.Name) == "val" {
						headingLevel = headingLevelFromStyle(attr.Value)
					}
				}
			case "t":
				inWt = true
				// Preserve leading/trailing spaces that OOXML marks with
				// xml:space="preserve".
			}
		case xml.EndElement:
			switch localName(t.Name) {
			case "p":
				// Flush the paragraph.
				text := strings.TrimRight(paraBuf.String(), " \t")
				if headingLevel > 0 && text != "" {
					text = strings.Repeat("#", headingLevel) + " " + text
				}
				paragraphs = append(paragraphs, text)
				paraBuf.Reset()
				headingLevel = 0
			case "pStyle":
				inPStyle = false
			case "t":
				inWt = false
			}
		case xml.CharData:
			if inWt && !inPStyle {
				paraBuf.WriteString(string(t))
			}
		}
	}

	// Join paragraphs with blank lines; skip runs of consecutive empty lines.
	var out strings.Builder
	prevEmpty := false
	for _, p := range paragraphs {
		if p == "" {
			if !prevEmpty {
				out.WriteString("\n")
			}
			prevEmpty = true
			continue
		}
		prevEmpty = false
		out.WriteString(p)
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String()), nil
}

// localName returns only the local part of an XML name (ignores namespace).
func localName(n xml.Name) string {
	return n.Local
}

// headingLevelFromStyle maps OOXML paragraph style names to Markdown heading
// depths. "Heading1" → 1, "Heading6" → 6; anything else → 0 (plain text).
func headingLevelFromStyle(val string) int {
	// OOXML style names use camel-case and may be locale-prefixed in some
	// Office installations (e.g. "1" or "heading1"), but the built-in English
	// names "Heading1"…"Heading6" are the most common.
	lower := strings.ToLower(val)
	for i := 1; i <= 6; i++ {
		if lower == fmt.Sprintf("heading%d", i) {
			return i
		}
	}
	return 0
}
