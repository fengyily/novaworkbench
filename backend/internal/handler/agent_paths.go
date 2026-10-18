package handler

import (
	"os"
	"strconv"
	"strings"
)

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
	v := strings.TrimRight(strings.TrimSpace(os.Getenv("NOVA_AGENT_WORKTREE_ROOT")), "/")
	if v == "" {
		return "/tmp/nova-agent"
	}
	return v
}

// agentMinFreeMB is the free-space floor (MB) required on the remote work root
// before a worktree is created. Env NOVA_AGENT_MIN_FREE_MB, default 1024;
// invalid or non-positive values fall back to the default.
func agentMinFreeMB() int {
	if v := strings.TrimSpace(os.Getenv("NOVA_AGENT_MIN_FREE_MB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1024
}
