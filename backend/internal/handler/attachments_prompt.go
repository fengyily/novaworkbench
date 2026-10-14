package handler

import (
	"path/filepath"
	"strings"

	"github.com/novaworkbench/backend/internal/service"
)

// attachManifestFor renders an owner's attachment manifest as a Markdown
// block ready to append to a system prompt. Returns "" when the service
// isn't wired, the owner id is blank, or there are no attachments — callers
// can append it unconditionally.
//
// ownerID is a requirement id (`req_*`) or a code-review session id
// (`crv_*`); which table is read is decided by the svc instance the caller
// holds (service.NewAttachmentService vs NewCodeReviewAttachmentService).
//
// attachDir is the directory the model should treat as the attachment root.
// Local execution passes the local root; remote Agent-server execution
// passes the synced mirror on the agent host. Passing "" falls back to the
// local root for that owner.
//
// Package-level (rather than a WizardHandler method) so ReviewHandler shares
// one implementation — WizardHandler.attachManifestFor now delegates here.
func attachManifestFor(svc *service.AttachmentService, ownerID, attachDir string) string {
	if svc == nil || strings.TrimSpace(ownerID) == "" {
		return ""
	}
	if strings.TrimSpace(attachDir) == "" {
		attachDir = filepath.Join(service.AttachmentRoot(), ownerID)
	}
	return svc.BuildManifest(ownerID, attachDir)
}
