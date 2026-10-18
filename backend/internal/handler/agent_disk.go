package handler

import (
	"bytes"
	"context"
	"errors"
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

// remoteWorkRootProbeScript builds the POSIX shell that makes sure the Agent
// work root exists and that the current SSH user (the same account the worker
// and every claude run use) can write into it. Remote runs create per-project
// scratch clones and per-requirement worktrees under this directory; when it
// is missing and cannot be created, or exists read-only, the failure otherwise
// only surfaces much later as an opaque `git clone` / `git worktree` EACCES
// in the middle of a development run.
//
// The script prints NOVA_USER=<name> first so the Go side can name the account
// in its guidance, `ls -ld` on success (owner/mode for the install log), and
// exits 11 (mkdir failed) / 12 (not a directory) / 13 (not writable).
func remoteWorkRootProbeScript(root string) string {
	return `root=` + shellQuote(root) + `
echo "NOVA_USER=$(id -un)"
if [ ! -e "$root" ]; then
  if ! mkdir -p "$root"; then
    echo "无法创建目录（父目录不存在，或当前用户对父目录无写权限）"
    exit 11
  fi
fi
if [ ! -d "$root" ]; then
  echo "路径已存在但不是目录"
  exit 12
fi
probe="$root/.nova-write-probe.$$"
if ! ( : > "$probe" ); then
  echo "目录不可写"
  exit 13
fi
rm -f "$probe" 2>/dev/null || true
ls -ld "$root"
`
}

// preCreateWorkRootScript builds the root-phase companion of
// remoteWorkRootProbeScript. The recommended work root (/var/lib/nova-agent)
// lives outside the home directory of the non-root user the installer
// provisions, so creating it AFTER the user switch would fail with EACCES.
// This runs while the install still has root and only touches a path that does
// not exist yet — re-owning an existing directory could clobber shared
// storage, so that case is left to the post-switch permission check, which
// reports it with explicit chown guidance. Best-effort by design.
func preCreateWorkRootScript(root, user string) string {
	u := shellQuote(user)
	return `root=` + shellQuote(root) + `
if [ -e "$root" ]; then
  echo "[nova-agent] 工作根目录已存在: $root"
  exit 0
fi
if ! mkdir -p "$root"; then
  echo "[nova-agent] ⚠️ 无法创建工作根目录 $root，将在切换用户后重试"
  exit 1
fi
chown ` + u + `:` + u + ` "$root" 2>/dev/null || chown ` + u + ` "$root" 2>/dev/null || true
echo "[nova-agent] 已创建工作根目录并授权给 ` + user + `: $root"
`
}

// ensureRemoteWorkRoot creates the configured work root when it is missing and
// verifies the SSH user can write into it. Called from the install flow (a
// failure aborts the install), from Check (reported as an error status), and
// from every remote run right before it clones / creates a worktree — the last
// one catches hosts installed before this provisioning shipped, or whose root
// was just changed in 设置 → Agent 服务器. Idempotent on an existing writable
// directory. logSuccess is false on the per-run path so healthy runs don't
// grow a log line; failures are always reported.
func ensureRemoteWorkRoot(ctx context.Context, client *gossh.Client, root string, job *store.Job, logSuccess bool) error {
	var out bytes.Buffer
	exit, err := client.Exec(ctx, remoteWorkRootProbeScript(root), "", nil, &out, nil)
	if err != nil {
		return fmt.Errorf("检查 Agent 工作根目录 %s 失败: %w", root, err)
	}

	user := ""
	var detail []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if name, ok := strings.CutPrefix(line, "NOVA_USER="); ok {
			user = strings.TrimSpace(name)
			continue
		}
		detail = append(detail, line)
	}
	info := strings.Join(detail, "；")

	if exit == 0 {
		if job != nil && logSuccess {
			if info == "" {
				info = root
			}
			job.Append(store.LogLine{Type: "message", Content: "✓ 工作根目录可写: " + info})
		}
		return nil
	}

	if user == "" {
		user = "当前 SSH 用户"
	}
	if info == "" {
		info = fmt.Sprintf("exit=%d", exit)
	}
	msg := fmt.Sprintf("Agent 工作根目录 %s 不可用：%s。"+
		"远端开发会在该目录下创建 scratch clone / worktree，目录不可写会在开发阶段报权限错误。"+
		"请先在服务器上执行 sudo mkdir -p %s && sudo chown %s %s，"+
		"或在「设置 → Agent 服务器」改到当前用户可写的目录（如 ~/nova-agent）后重新点「安装依赖」",
		root, info, root, user, root)
	if job != nil {
		job.Append(store.LogLine{Type: "error", Content: "❌ " + msg})
	}
	return errors.New(msg)
}

// ensureRemoteDiskSpace fails fast, with a readable message, when the work
// root's filesystem has less than agentMinFreeMB() free. An unusable probe is
// not an error — the check is advisory and must not block on odd hosts.
func ensureRemoteDiskSpace(ctx context.Context, client *gossh.Client, root string, job *store.Job) error {
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
func addRemoteWorktree(ctx context.Context, client *gossh.Client, job *store.Job, root, baseRepo, wtPath, branch, baseRef string, cmds []string) error {
	if err := ensureRemoteDiskSpace(ctx, client, root, job); err != nil {
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
				"请清理空间或为该 Agent 服务器配置更大的工作根目录。git 输出：\n%s",
				root, exit, firstLines(out.String(), 5))
		}
	}
	return fmt.Errorf("git worktree 创建失败（exit=%d），请检查仓库状态：\n%s", exit, firstLines(out.String(), 5))
}
