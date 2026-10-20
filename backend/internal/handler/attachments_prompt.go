package handler

import (
	"context"
	"log"
	"path/filepath"
	"strings"

	gossh "github.com/novaworkbench/backend/internal/ssh"

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
