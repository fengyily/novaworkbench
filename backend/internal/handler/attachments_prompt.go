package handler

import (
	"context"
	"log"
	"path/filepath"
	"regexp"
	"strings"

	gossh "github.com/novaworkbench/backend/internal/ssh"

	"github.com/novaworkbench/backend/internal/model"
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

// resolveAttachDirFor returns the directory the caller should pass into
// attachManifestFor for the given execution context.
//
//   - Local execution (serverID == ""): returns the local attachment root
//     for the owner (~/.novaworkbench/attachments/<reqID>).
//   - Remote Agent-server execution: opens a short-lived SSH/SFTP session
//     and reconciles the owner's attachments into the agent host's mirror
//     (~/.nova/attachments/<reqID>) via AttachmentService.SyncToAgentServer.
//     On success the canonical remote directory is returned; on any failure
//     (missing dep, agent_servers lookup error, SSH dial error, sync error)
//     we log and silently fall back to the local directory so an
//     intermittent remote hiccup never breaks a wizard / sub-task run.
//
// Package-level so SubTaskRunner (no WizardHandler dep — would cycle) can
// share one implementation. WizardHandler.resolveAttachDir delegates here.
func resolveAttachDirFor(ctx context.Context, attachSvc *service.AttachmentService, agentSvrSvc *service.AgentServerService, reqID, serverID string) string {
	local := filepath.Join(service.AttachmentRoot(), reqID)
	if strings.TrimSpace(reqID) == "" || strings.TrimSpace(serverID) == "" {
		return local
	}
	if attachSvc == nil || agentSvrSvc == nil {
		return local
	}
	srv, plain, err := agentSvrSvc.GetWithCredential(serverID)
	if err != nil {
		log.Printf("[attachments] remote attach-dir lookup %s failed: %v (using local paths)", serverID, err)
		return local
	}
	sshClient, err := gossh.Dial(ctx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		log.Printf("[attachments] remote attach-dir SSH dial %s failed: %v (using local paths)", srv.Name, err)
		return local
	}
	defer sshClient.Close()
	rd, err := attachSvc.SyncToAgentServer(ctx, serverID, sshClient, reqID)
	if err != nil {
		log.Printf("[attachments] remote attach-dir sync to %s failed: %v (using local paths)", srv.Name, err)
		return local
	}
	return rd
}

// novaAttachRe matches the placeholder the frontend inserts when the user
// pastes a screenshot into a sub-task composer — `![image](nova://attachment/<id>)`.
// The id captures the attachment row's primary key (e.g. `att_a1b2c3d4e5`).
var novaAttachRe = regexp.MustCompile(`nova://attachment/([A-Za-z0-9_-]+)`)

// attachLookupForMarker is the indirection rewriteAttachmentMarkers uses to
// fetch attachment rows. Tests in this package override it to inject
// fixtures without spinning up a real *service.AttachmentService (which
// holds a live *db.DB — see service.NewAttachmentService).
var attachLookupForMarker = func(svc *service.AttachmentService, id string) (*model.Attachment, error) {
	return svc.Get(id)
}

// rewriteAttachmentMarkers rewrites every `nova://attachment/<id>` placeholder
// in `text` to the attachment's real absolute path on this execution's
// attachment root (local = ~/.novaworkbench/attachments/<owner>; remote =
// the synced mirror on the agent host — see resolveAttachDirFor).
//
// The on-disk filename is NOT `a.Filename` (the user's original name) but
// `<att_id>__<sha8>__<safe_name>` from Upload — see service.Upload at
// attachment.go's stored-path construction. The manifest printed for Claude
// uses filepath.Base(a.StoredPath) for the same reason; the two surfaces
// must agree or Claude gets one path and tries to Read another.
//
// Cross-requirement references (`a.OwnerID != ownerID`) are NOT rewritten:
// the placeholder came from the user-typed body of THIS sub-task, so an id
// pointing at a different requirement is either a typo or a deliberate
// paste leak — keep it visible so the AI can flag it. Same for any id that
// fails to resolve (deleted attachment, draft row that never finished
// uploading).
//
// Repeated ids within the same body hit a small per-call cache so a
// paste-leaked id or an inlined thumbnail reference doesn't trigger N
// SQLite lookups.
func rewriteAttachmentMarkers(svc *service.AttachmentService, ownerID, attachDir, text string) string {
	if svc == nil || text == "" {
		return text
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return text
	}
	if attachDir == "" {
		attachDir = filepath.Join(service.AttachmentRoot(), ownerID)
	}
	type resolved struct {
		path string
		ok   bool
	}
	cache := map[string]resolved{}
	resolve := func(id string) (string, bool) {
		if r, hit := cache[id]; hit {
			return r.path, r.ok
		}
		a, err := attachLookupForMarker(svc, id)
		if err != nil || a == nil || a.OwnerID != ownerID || a.StoredPath == "" {
			cache[id] = resolved{}
			return "", false
		}
		p := filepath.Join(attachDir, filepath.Base(filepath.FromSlash(a.StoredPath)))
		cache[id] = resolved{path: p, ok: true}
		return p, true
	}
	return novaAttachRe.ReplaceAllStringFunc(text, func(match string) string {
		sm := novaAttachRe.FindStringSubmatch(match)
		if len(sm) < 2 {
			return match
		}
		if p, ok := resolve(sm[1]); ok {
			return p
		}
		return match
	})
}
