package handler

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/novaworkbench/backend/internal/model"
)

// defaultAgentWorktreeRoot is the historical remote work root.
const defaultAgentWorktreeRoot = "/tmp/nova-agent"

// agentWorktreeRootSetting holds the value saved in 设置 → Agent 服务器
// (settings key agent.worktree_root). Precedence: env > this > default.
var agentWorktreeRootSetting atomic.Value // string

// SetAgentWorktreeRootSetting installs the UI-configured work root (already
// normalised; "" clears it). Called at startup and on every settings save.
func SetAgentWorktreeRootSetting(v string) { agentWorktreeRootSetting.Store(v) }

// NormalizeAgentWorktreeRoot validates a user-supplied work root: empty means
// "use the default"; otherwise it must be an absolute path without ".." or
// control characters, and not "/".
func NormalizeAgentWorktreeRoot(v string) (string, error) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return "", nil
	}
	if !strings.HasPrefix(v, "/") {
		return "", errors.New("工作根目录必须是绝对路径（以 / 开头）")
	}
	for _, seg := range strings.Split(v, "/") {
		if seg == ".." {
			return "", errors.New("工作根目录不能包含 ..")
		}
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("工作根目录包含非法字符")
		}
	}
	return v, nil
}

// agentWorktreeRoot returns the GLOBAL fallback for the directory that hosts
// Agent server scratch clones and per-requirement worktrees. Most call sites
// should use agentWorktreeRootFor(srv) instead: each host can (and often must)
// use its own root — small /tmp tmpfs, quota'd home dirs, a separate data disk.
//
// History: the path was hardcoded to "/tmp/nova-agent" everywhere — wizard,
// review, merge, followup. Some deployments needed the actual files on a
// larger / no-quota filesystem, so a global env + saved setting were added.
// The per-server column now sits between them (see agentWorktreeRootFor);
// this function remains the "no server in hand" fallback and the base of that
// chain.
func agentWorktreeRoot() string {
	return agentWorktreeRootFor(nil)
}

// agentWorktreeRootFor returns the effective work root for one Agent server.
// Precedence: NOVA_AGENT_WORKTREE_ROOT (operator-wide hard override) >
// agent_servers.worktree_root (per-server) > settings key agent.worktree_root
// (global default) > /tmp/nova-agent.
//
// The per-server value is what makes multi-host deployments work: each host
// gets a path that matches its own disk layout, while a nil server (or an
// empty column) keeps the historical global behaviour. agent-worker receives
// the resolved value as NOVA_AGENT_WORKTREE_ROOT at launch so its
// assertWorkDirInScope check stays in sync.
func agentWorktreeRootFor(srv *model.AgentServer) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("NOVA_AGENT_WORKTREE_ROOT")), "/"); v != "" {
		return v
	}
	if srv != nil {
		if v := strings.TrimRight(strings.TrimSpace(srv.WorktreeRoot), "/"); v != "" {
			return v
		}
	}
	if v, _ := agentWorktreeRootSetting.Load().(string); v != "" {
		return v
	}
	return defaultAgentWorktreeRoot
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
