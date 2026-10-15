package handler

import "os"

// agentWorktreeRoot returns the directory the backend uses as the parent of
// Agent server scratch clones and per-requirement worktrees on the remote host.
//
// History: the path was hardcoded to "/tmp/nova-agent" everywhere — wizard,
// review, merge, followup. Some deployments (small /tmp tmpfs with strict
// per-user block quotas) need the actual files to live on a larger / no-quota
// filesystem, so we now read NOVA_AGENT_WORKTREE_ROOT and fall back to the
// historical default when unset. agent-worker reads the same env var on its
// end so its workDir safety check stays in sync.
func agentWorktreeRoot() string {
	if v := os.Getenv("NOVA_AGENT_WORKTREE_ROOT"); v != "" {
		return v
	}
	return "/tmp/nova-agent"
}