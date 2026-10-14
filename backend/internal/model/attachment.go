package model

import (
	"io"
	"time"
)

// Attachment is one file uploaded against a requirement. The wizard's three
// role-gated stages (analyst / architect / developer) all read from the same
// set; manifest_text pre-lists zip entries so Claude can grep / unzip -p
// without a separate `unzip -l` call.
type Attachment struct {
	ID            string    `json:"id"`
	RequirementID string    `json:"requirement_id"`
	Filename      string    `json:"filename"`
	StoredPath    string    `json:"stored_path"`            // relative to AttachmentRoot
	MimeType      string    `json:"mime_type"`
	SizeBytes     int64     `json:"size_bytes"`
	SHA256        string    `json:"sha256"`
	Description   string    `json:"description"`            // user-written annotation
	ManifestText  string    `json:"manifest_text,omitempty"` // zip entry listing
	UploadedBy    string    `json:"uploaded_by"`
	UploadedAt    time.Time `json:"uploaded_at"`
}

// AttachmentUpload is the input shape for AttachmentService.Upload. The file
// content is streamed from Reader and consumed once; the caller is
// responsible for closing the source (multipart.File has its own Close).
type AttachmentUpload struct {
	Filename    string
	Reader      io.Reader
	Description string
	UploadedBy  string // user id (or "" when auth disabled)
}
