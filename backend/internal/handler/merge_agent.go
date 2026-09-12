package handler

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	gossh "github.com/novaworkbench/backend/internal/ssh"
)

// codeLivesOnAgent reports whether a requirement's working code physically
// lives on an Agent server (rather than in a local worktree). This is true
// only for the origin-transport Agent path: a local-sync requirement
// (sync_mode == "local") runs on an Agent server for compute but its code is
// synced back to the LOCAL isolated worktree after every round, so all
// integration / cleanup happens locally. Every follow-up router that used to
// branch on AgentServerID alone must use this instead, or a local-sync
// requirement would be wrongly redirected to the agent host for merge/cleanup.
func codeLivesOnAgent(r *model.Requirement) bool {
	return r != nil && r.AgentServerID != "" && r.SyncMode != service.SyncModeLocal
}

// ── Agent-server execution consistency for worktree cleanup ────────────────
//
// A requirement developed on an Agent server has its working tree on THAT
// host, under /tmp/nova-agent/<projectID>/<reqID> (see runRemoteCoding). The
// NovaWorkbench host may have no checkout for it at all — start-coding never
// creates a local worktree on the remote path. The "清理开发环境" action has
// to remove THAT worktree, not a stale local one.
//
// "推送并发起 PR" is now handled by SubTaskRunner.Run: it inspects
// req.AgentServerID and dispatches the child to the agent server via
// WizardHandler.runRemoteCoding (wired through SetRemoteCoding), so all
// post-coding dispatch lives in one place.
//
// Requirements with dev_source != "agent" (or with a blank agent_server_id)
// are untouched and keep taking the original local path.

// remoteWorktreePaths mirrors the layout runRemoteCoding creates on the agent
// host. Keeping the two in one place means a future layout change only has to
// be made twice in the same package rather than hunted across files.
func remoteWorktreePaths(projectID, reqID string) (baseRepo, wtPath string) {
	return "/tmp/nova-agent/" + projectID + "/base",
		"/tmp/nova-agent/" + projectID + "/" + reqID
}

// remoteBranchFor resolves the dev branch name the same way runRemoteCoding
// does, so cleanup/push target the branch the coding run actually created.
func remoteBranchFor(reqRow *model.Requirement) string {
	if reqRow.BranchName != "" {
		return reqRow.BranchName
	}
	return "requirement-" + reqRow.ID
}

// dialAgentForReq opens an SSH connection to the Agent server this requirement
// was developed on. Returns (nil, name, err) when the requirement isn't an
// agent-developed one, the service isn't wired, or the credential/connection
// fails. The returned name is used for user-facing log lines.
func (h *MergeHandler) dialAgentForReq(ctx context.Context, reqRow *model.Requirement) (*gossh.Client, string, error) {
	if reqRow == nil || reqRow.AgentServerID == "" {
		return nil, "", fmt.Errorf("需求未标记 Agent 服务器")
	}
	if h.agentSvrSvc == nil {
		return nil, "", fmt.Errorf("Agent 服务器服务未初始化")
	}
	srv, plain, err := h.agentSvrSvc.GetWithCredential(reqRow.AgentServerID)
	if err != nil {
		return nil, "", fmt.Errorf("无法读取 Agent 服务器凭据: %w", err)
	}
	client, err := gossh.Dial(ctx, srv.Host, srv.Port, srv.Username, srv.AuthType, plain)
	if err != nil {
		return nil, srv.Name, fmt.Errorf("SSH 连接 %s (%s) 失败: %w", srv.Name, srv.Host, err)
	}
	return client, srv.Name, nil
}

// usesAgentServer reports whether follow-up actions for this requirement must
// be routed to an Agent server.
func (h *MergeHandler) usesAgentServer(reqRow *model.Requirement) bool {
	return codeLivesOnAgent(reqRow) && h.agentSvrSvc != nil
}

// remoteCapture runs a command on the agent host and returns its combined
// output as a string (instead of streaming it into the job log). Used for the
// small read-only probes — dirty-file listing, commit log for the PR body.
func remoteCapture(ctx context.Context, client *gossh.Client, cmd string) (string, int) {
	var buf bytes.Buffer
	exit, err := client.Exec(ctx, cmd, "", nil, &buf, nil)
	if err != nil && exit == 0 {
		exit = -1
	}
	return strings.TrimSpace(buf.String()), exit
}

// remoteCleanupResult carries the outcome of a remote worktree cleanup back to
// the (synchronous) Cleanup handler so it can pick the right HTTP status.
type remoteCleanupResult struct {
	dirty   []string // non-empty → refused because the remote tree has uncommitted files
	errMsg  string   // non-empty → hard failure
	skipped bool     // the remote worktree no longer exists (already cleaned)
}

// remoteCleanup removes the per-requirement worktree + dev branch on the Agent
// server the requirement was developed on. Mirrors MergeHandler.Cleanup's
// safety rules: a dirty tree is refused unless force is set.
func (h *MergeHandler) remoteCleanup(reqRow *model.Requirement, force bool) remoteCleanupResult {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client, srvName, err := h.dialAgentForReq(ctx, reqRow)
	if err != nil {
		return remoteCleanupResult{errMsg: err.Error()}
	}
	defer client.Close()

	baseRepo, wtPath := remoteWorktreePaths(reqRow.ProjectID, reqRow.ID)
	q := shellQuoteSingle
	if !client.Exists(wtPath) {
		// Still prune metadata + drop the branch so a re-run starts clean.
		client.Exec(ctx, "cd "+q(baseRepo)+" && git worktree prune", "", nil, nil, nil)
		return remoteCleanupResult{skipped: true}
	}

	if !force {
		out, exit := remoteCapture(ctx, client, "cd "+q(wtPath)+" && git status --porcelain")
		if exit == 0 && out != "" {
			var dirty []string
			for _, line := range strings.Split(out, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					dirty = append(dirty, line)
				}
			}
			if len(dirty) > 0 {
				return remoteCleanupResult{dirty: dirty}
			}
		}
	}

	rmFlag := ""
	if force {
		rmFlag = " --force"
	}
	script := "cd " + q(baseRepo) + " && git worktree remove" + rmFlag + " " + q(wtPath) + " && git worktree prune"
	if exit, _ := client.Exec(ctx, script, "", nil, nil, nil); exit != 0 {
		// Fall back to a raw rm + prune: a worktree whose metadata is already
		// broken can't be removed by git but must still be reclaimable.
		if !force {
			return remoteCleanupResult{errMsg: "Agent 服务器上移除 worktree 失败（exit=" + fmtInt(exit) + "），可勾选强制清理重试"}
		}
		client.Exec(ctx, "rm -rf "+q(wtPath)+" && cd "+q(baseRepo)+" && git worktree prune", "", nil, nil, nil)
	}
	if br := remoteBranchFor(reqRow); br != "" {
		// Best-effort: an unmerged branch refuses -d but -D always works; a
		// failure here is not worth failing the cleanup over.
		client.Exec(ctx, "cd "+q(baseRepo)+" && git branch -D "+q(br)+" || true", "", nil, nil, nil)
	}
	log.Printf("[worktree-cleanup] removed remote worktree %s on %s for %s", wtPath, srvName, reqRow.ID)
	return remoteCleanupResult{}
}
