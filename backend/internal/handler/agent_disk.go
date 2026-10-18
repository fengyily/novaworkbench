package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
)

// isDiskFullError reports whether git output from a failed `worktree add`
// looks like ENOSPC. A full tmpfs surfaces as hundreds of "unable to write
// file" lines followed by "Could not reset index file to revision 'HEAD'",
// never as a literal "No space left on device", so all three are matched.
func isDiskFullError(out string) bool {
	for _, sig := range []string{
		"unable to write file",
		"No space left on device",
		"Could not reset index file",
	} {
		if strings.Contains(out, sig) {
			return true
		}
	}
	return false
}

// remoteDiskInfo is the parsed `df -Pk` line for the filesystem holding the
// agent work root.
type remoteDiskInfo struct {
	Mount   string
	UsedKB  int64
	AvailKB int64
	FSType  string // empty when `stat -f` is unsupported (e.g. macOS)
}

// parseDfPk parses `df -Pk` output (header + one data line).
func parseDfPk(out string) (remoteDiskInfo, bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return remoteDiskInfo{}, false
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 6 {
		return remoteDiskInfo{}, false
	}
	used, err1 := strconv.ParseInt(f[2], 10, 64)
	avail, err2 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil {
		return remoteDiskInfo{}, false
	}
	return remoteDiskInfo{Mount: f[5], UsedKB: used, AvailKB: avail}, true
}

// probeRemoteDisk inspects the filesystem that holds (or would hold) root. The
// root may not exist yet, so we walk up to the nearest existing ancestor. The
// fs-type probe is best-effort: a failing `stat -f -c` only blanks FSType.
func probeRemoteDisk(ctx context.Context, client *gossh.Client, root string) (remoteDiskInfo, bool) {
	q := shellQuote(root)
	walk := `d=` + q + `; while [ ! -e "$d" ] && [ "$d" != "/" ]; do d=$(dirname "$d"); done; `
	var dfOut bytes.Buffer
	if exit, _ := client.Exec(ctx, walk+`df -Pk "$d"`, "", nil, &dfOut, nil); exit != 0 {
		return remoteDiskInfo{}, false
	}
	info, ok := parseDfPk(dfOut.String())
	if !ok {
		return remoteDiskInfo{}, false
	}
	var fsOut bytes.Buffer
	if exit, _ := client.Exec(ctx, walk+`stat -f -c %T "$d" 2>/dev/null`, "", nil, &fsOut, nil); exit == 0 {
		info.FSType = strings.TrimSpace(fsOut.String())
	}
	return info, true
}

// ensureRemoteDiskSpace fails fast, with a readable message, when the work
// root's filesystem has less than agentMinFreeMB() free. An unusable probe is
// not an error — the check is advisory and must not block on odd hosts.
func ensureRemoteDiskSpace(ctx context.Context, client *gossh.Client, job *store.Job) error {
	root := agentWorktreeRoot()
	info, ok := probeRemoteDisk(ctx, client, root)
	if !ok {
		return nil
	}
	minMB := agentMinFreeMB()
	availMB := info.AvailKB / 1024
	if availMB >= int64(minMB) {
		return nil
	}
	fs := ""
	if info.FSType != "" {
		fs = "，类型 " + info.FSType
	}
	msg := fmt.Sprintf("Agent 服务器工作目录 %s 所在文件系统（挂载点 %s%s）剩余空间不足：可用 %d MB、已用 %d MB，至少需要 %d MB。"+
		"请清理空间，或在后端设置 NOVA_AGENT_WORKTREE_ROOT 指向磁盘路径（如 /var/lib/nova-agent）并重装 worker；阈值可由 NOVA_AGENT_MIN_FREE_MB 调整",
		root, info.Mount, fs, availMB, info.UsedKB/1024, minMB)
	if job != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ " + msg})
	}
	return fmt.Errorf("%s", msg)
}

// cleanupFailedWorktree removes the half-built worktree directory left behind
// by a failed `git worktree add`, and drops the branch ref only when it is
// provably empty: not checked out in any worktree AND still pointing at the
// base ref. Anything else may hold unpushed commits ("continue" flow) and is
// kept for the attach strategy.
func cleanupFailedWorktree(ctx context.Context, client *gossh.Client, job *store.Job, baseRepo, wtPath, branch, baseRef string) {
	repo := shellQuote(baseRepo)
	wt := shellQuote(wtPath)
	br := shellQuote(branch)
	ref := shellQuote("refs/heads/" + branch)
	script := "cd " + repo + " && " +
		"(git worktree remove --force " + wt + " 2>/dev/null || true); " +
		"rm -rf " + wt + "; " +
		"cd " + repo + " && git worktree prune; " +
		`if ! git worktree list --porcelain | grep -qx "branch ` + "refs/heads/" + strings.ReplaceAll(branch, `"`, `\"`) + `"; then ` +
		`tip=$(git rev-parse --verify -q ` + ref + `); base=$(git rev-parse --verify -q ` + shellQuote(baseRef) + `); ` +
		`if [ -n "$tip" ] && [ "$tip" = "$base" ]; then git branch -D ` + br + ` >/dev/null 2>&1; fi; fi`
	client.Exec(ctx, script, "", nil, &jobWriter{job: job}, nil)
}

// firstLines returns at most n non-empty lines of s, for embedding git output
// in an error message without dumping hundreds of "unable to write" lines.
func firstLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
			if len(keep) == n {
				break
			}
		}
	}
	return strings.Join(keep, "\n")
}

// addRemoteWorktree runs the worktree-creation strategies in order after a
// disk-space pre-check. cmds are full shell commands (run from baseRepo's
// parent shell, each already cd-ing as needed). A disk-full failure aborts
// immediately — later strategies would only hit the same ENOSPC and trip over
// the leftovers of this one — cleans up, and reports git's own output.
func addRemoteWorktree(ctx context.Context, client *gossh.Client, job *store.Job, baseRepo, wtPath, branch, baseRef string, cmds []string) error {
	if err := ensureRemoteDiskSpace(ctx, client, job); err != nil {
		return err
	}
	exit := 0
	var out bytes.Buffer
	for _, c := range cmds {
		out.Reset()
		exit, _ = client.Exec(ctx, c, "", nil, io.MultiWriter(&jobWriter{job: job}, &out), nil)
		if exit == 0 {
			return nil
		}
		if isDiskFullError(out.String()) {
			cleanupFailedWorktree(ctx, client, job, baseRepo, wtPath, branch, baseRef)
			return fmt.Errorf("git worktree 创建失败：Agent 服务器磁盘空间不足（工作目录 %s，exit=%d）。"+
				"请清理空间或设置 NOVA_AGENT_WORKTREE_ROOT 指向磁盘路径。git 输出：\n%s",
				agentWorktreeRoot(), exit, firstLines(out.String(), 5))
		}
	}
	return fmt.Errorf("git worktree 创建失败（exit=%d），请检查仓库状态：\n%s", exit, firstLines(out.String(), 5))
}
