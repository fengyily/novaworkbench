package service

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Sync gate rejection codes. The handler renders user-facing Chinese
// remediation hints from Code alone (see Msg below) — adding a new code means
// adding a new branch in wizard_architect.go's prepareDesignWorkspace as well.
const (
	SyncGateDirtyWorktree    = "DIRTY_WORKTREE"     // uncommitted / untracked changes
	SyncGateHeadNotOnBase    = "HEAD_NOT_ON_BASE"   // HEAD branch ≠ baseBranch (incl. detached)
	SyncGateUnpushedCommits  = "UNPUSHED_COMMITS"   // ahead of origin/<base> — can't --ff-only
	SyncGateFetchFailed      = "FETCH_FAILED"       // git fetch origin <base> exited non-zero
	SyncGateFFFailed         = "FF_FAILED"          // merge --ff-only refused (defensive — A/B/D caught first)
)

// SyncGateError is the hard-sync rejection signal. Code is one of the
// SyncGate* constants so callers can map to HTTP status / SSE event type
// without parsing Msg. Stderr carries the original git output (porcelain /
// rev-list / fetch stderr) verbatim so the user can see exactly what failed.
// Truncated to 2 KiB to keep the SSE event payload bounded.
type SyncGateError struct {
	Code   string
	Msg    string
	Stderr string
}

func (e *SyncGateError) Error() string {
	if e.Stderr == "" {
		return e.Msg + " (code=" + e.Code + ")"
	}
	return e.Msg + " (code=" + e.Code + "): " + e.Stderr
}

// gitSyncStderrCap is the maximum amount of git stderr / porcelain / log
// output we paste into SyncGateError.Stderr. Keeps SSE event payloads bounded
// and prevents a hostile remote from filling the panel with megabytes.
const gitSyncStderrCap = 2 * 1024

// capStderr trims trailing whitespace and clips to gitSyncStderrCap runes so
// SyncGateError.Stderr stays a predictable size. Returns the original string
// unchanged when it's already small.
func capStderr(s string) string {
	s = strings.TrimRight(s, "\n\t ")
	if len(s) <= gitSyncStderrCap {
		return s
	}
	return s[:gitSyncStderrCap] + "\n…(已截断)"
}

// GitSyncResult is the outcome of a SyncToOriginBase attempt. Skipped is set
// for non-fatal "nothing to sync" cases (non-git repo, no origin configured,
// empty repository) — the wizard proceeds as if the gate didn't exist. BaseSHA
// is the 40-char origin/<base> SHA stamped by a successful fast-forward, or
// "" when Skipped / FastForwarded didn't reach step F.
type GitSyncResult struct {
	BaseSHA       string // 40-char origin/<base> SHA after a successful sync (or "" when Skipped)
	OldSHA        string // local HEAD before the fetch ("" when we couldn't read it)
	FastForwarded bool   // true when the working tree was fast-forwarded (vs. already up to date)
	Skipped       bool   // true when the repo is non-git / no origin / no commits — not a failure
}

// SyncToOriginBase runs the architect-design gate against a project checkout:
//
//	0   rev-parse --is-inside-work-tree  → non-git repo         → Skipped=true
//	0b  remote get-url origin            → no origin configured → Skipped=true
//	0c  rev-parse --verify HEAD          → unborn HEAD (empty)   → Skipped=true
//	A   status --porcelain               → non-empty             → DIRTY_WORKTREE
//	B   rev-parse --abbrev-ref HEAD      → != baseBranch         → HEAD_NOT_ON_BASE
//	C   fetch origin <base>              → non-zero exit         → FETCH_FAILED
//	D   rev-list --count origin/<b>..HEAD → > 0                  → UNPUSHED_COMMITS
//	E   merge --ff-only origin/<b>       → non-zero exit         → FF_FAILED
//	F   rev-parse origin/<b>             → BaseSHA
//
// logf may be nil (silent). The function never mutates the working tree on
// the failure path — every gate runs read-only first, so a DIRTY_WORKTREE
// rejection leaves any uncommitted changes exactly where they were. ctx is
// used both as the parent deadline and as the per-fetch timeout (the fetch
// itself caps at `timeout`).
func SyncToOriginBase(ctx context.Context, repoPath, baseBranch string, timeout time.Duration, logf func(string)) (GitSyncResult, error) {
	var res GitSyncResult
	if repoPath == "" {
		return res, fmt.Errorf("repoPath is empty")
	}
	if baseBranch == "" {
		baseBranch = "main"
	}
	syncLog := func(line string) {
		if logf != nil {
			logf(line)
		}
	}

	// Step 0: not a git working tree at all (e.g. project wizard pre-init
	// scratch dir). Skipped — wizard falls back to the legacy in-place path.
	if _, err := gitRunPlain(repoPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		res.Skipped = true
		return res, nil
	}

	// Step 0b: no origin remote → nothing to fetch. Match syncBaseBranchService's
	// log line verbatim so users see a consistent message.
	if _, err := gitRunPlain(repoPath, "remote", "get-url", "origin"); err != nil {
		syncLog("ℹ️ 项目未配置 origin 远程，跳过主分支同步")
		res.Skipped = true
		return res, nil
	}

	// Step 0c: empty repo (clone landed but no first commit yet). Unborn HEAD
	// can't satisfy any of the ahead/behind gates — treat as Skipped.
	if _, err := gitRunPlain(repoPath, "rev-parse", "--verify", "HEAD"); err != nil {
		syncLog("ℹ️ 仓库尚未创建提交，跳过主分支同步")
		res.Skipped = true
		return res, nil
	}

	// Capture pre-sync HEAD so we can tell FastForwarded=true from "already
	// up to date" later. Failure here is non-fatal (defensive — step 0c just
	// confirmed HEAD exists).
	if head, herr := gitRunPlain(repoPath, "rev-parse", "HEAD"); herr == nil {
		res.OldSHA = strings.TrimSpace(head)
	}

	// Step A: working tree must be clean. status --porcelain output (when
	// non-empty) is the canonical "what's dirty" listing — paste it verbatim
	// so the user can fix exactly what git sees, not just our paraphrase.
	if porcelain, perr := gitRunPlain(repoPath, "status", "--porcelain"); perr == nil {
		if strings.TrimSpace(porcelain) != "" {
			return res, &SyncGateError{
				Code: SyncGateDirtyWorktree,
				Msg: fmt.Sprintf(
					"项目工作区有未提交的改动，design 阶段拒绝同步。请先执行 `git stash` 或提交改动，" +
						"然后 `git checkout %s` 切回主分支后重试",
					baseBranch),
				Stderr: capStderr(porcelain),
			}
		}
	}

	// Step B: HEAD must be sitting on baseBranch. Detached HEADs report as
	// literal "HEAD" from --abbrev-ref, which we count as "not on base" — a
	// detached HEAD can't be safely fast-forwarded without a working branch.
	if head, herr := gitRunPlain(repoPath, "rev-parse", "--abbrev-ref", "HEAD"); herr == nil {
		cur := strings.TrimSpace(head)
		if cur != baseBranch {
			return res, &SyncGateError{
				Code: SyncGateHeadNotOnBase,
				Msg: fmt.Sprintf(
					"当前 HEAD 在 `%s` 分支（design 基线要求 `%s`），无法安全 fast-forward。"+
						" 请执行 `git checkout %s` 后重试",
					cur, baseBranch, baseBranch),
				Stderr: "",
			}
		}
	} else {
		// Couldn't read HEAD's branch — treat as HEAD_NOT_ON_BASE (safer than
		// continuing with stale ref info).
		return res, &SyncGateError{
			Code: SyncGateHeadNotOnBase,
			Msg: fmt.Sprintf(
				"无法确定当前 HEAD 分支（design 基线要求 `%s`），请确认仓库状态后重试",
				baseBranch),
			Stderr: capStderr(herr.Error()),
		}
	}

	// Step C: fetch origin <base> under GIT_TERMINAL_PROMPT=0 so a stalled
	// network / missing creds can never hang us past `timeout`. We DON'T
	// update origin/<base> from a stale local snapshot — fetch is the only
	// source of truth for what the upstream currently is.
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fetchCmd := exec.CommandContext(fctx, "git", "-C", repoPath, "fetch", "origin", baseBranch)
	fetchCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var fetchErrOut strings.Builder
	fetchCmd.Stderr = &fetchErrOut
	if err := fetchCmd.Run(); err != nil {
		return res, &SyncGateError{
			Code: SyncGateFetchFailed,
			Msg: fmt.Sprintf(
				"无法拉取 origin/%s，请检查网络与凭据后重试",
				baseBranch),
			Stderr: capStderr(fetchErrOut.String()),
		}
	}

	// Step D: must run AFTER step C's fetch — using a stale origin/<base>
	// would compute an ahead count against the wrong tip. rev-list --count
	// prints the integer directly so we Atoi it; anything non-zero means
	// HEAD has unpushed commits, which would block --ff-only.
	countOut, cerr := gitRunPlain(repoPath, "rev-list", "--count", "origin/"+baseBranch+"..HEAD")
	if cerr == nil {
		if n, nerr := atoiTrimmed(countOut); nerr == nil && n > 0 {
			// Capture the actual ahead commits so the user sees exactly
			// what they'd need to push (or reset) — much more useful than
			// the bare count.
			detail, _ := gitRunPlain(repoPath, "log", "--oneline", "origin/"+baseBranch+"..HEAD")
			return res, &SyncGateError{
				Code: SyncGateUnpushedCommits,
				Msg: fmt.Sprintf(
					"本地有 %d 个未推送的提交，design 阶段拒绝同步。请先 `git push origin %s`" +
						" 或 `git reset --hard origin/%s` 后重试",
					n, baseBranch, baseBranch),
				Stderr: capStderr(detail),
			}
		}
	}
	// rev-list failure (other than counting) is non-fatal: it just means we
	// can't confidently assert "0 ahead" → skip ahead to ff which will fail
	// loudly if our assumption was wrong.

	// Step E: merge --ff-only. By construction (A clean + B on-base + D
	// zero-ahead) this should always succeed; if it doesn't, surface it
	// as FF_FAILED so the user knows something changed underfoot (e.g.
	// concurrent push to origin/<base> between C and E).
	mffOut, mffErr := gitRunPlain(repoPath, "merge", "--ff-only", "origin/"+baseBranch)
	if mffErr != nil {
		return res, &SyncGateError{
			Code: SyncGateFFFailed,
			Msg: fmt.Sprintf(
				"无法 fast-forward 到 origin/%s，请检查仓库状态后重试",
				baseBranch),
			Stderr: capStderr(mffOut + "\n" + mffErr.Error()),
		}
	}

	// Step F: capture the final SHA + emit the human-readable success lines
	// in the same style as syncBaseBranchService so existing readers aren't
	// surprised by an unfamiliar log format.
	sha, shaErr := gitRunPlain(repoPath, "rev-parse", "origin/"+baseBranch)
	if shaErr != nil {
		return res, &SyncGateError{
			Code: SyncGateFFFailed,
			Msg: "无法读取同步后的 origin/<base> SHA",
			Stderr: capStderr(shaErr.Error()),
		}
	}
	res.BaseSHA = strings.TrimSpace(sha)
	if res.BaseSHA != res.OldSHA && res.OldSHA != "" {
		res.FastForwarded = true
	}

	syncLog("🔄 已同步 origin/" + baseBranch)
	if res.BaseSHA != "" {
		if tip, terr := gitRunPlain(repoPath, "log", "-1", "origin/"+baseBranch,
			"--format=%h %s (%an, %ad)", "--date=format:%Y-%m-%d %H:%M"); terr == nil {
			if tip = strings.TrimSpace(tip); tip != "" {
				syncLog("📌 origin/" + baseBranch + " 最新提交: " + tip)
			}
		}
	}
	return res, nil
}

// atoiTrimmed parses a small non-negative integer from a git output line,
// tolerating leading/trailing whitespace. Negative / overflow / non-numeric
// input returns an error so the caller can fall back to its own handling.
func atoiTrimmed(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	// strconv.Atoi handles signs; we keep this thin so the file doesn't pull
	// in strconv solely for one call.
	var n int
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric: %q", s)
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0, fmt.Errorf("overflow: %q", s)
		}
	}
	return n, nil
}

// SyncDesignBase is the ProjectService-bound variant of SyncToOriginBase:
// it resolves the project row, ensures the directory exists (auto-clone via
// EnsureCloned when missing), then runs the gate. Successful runs stamp
// sync_status=ok / last_synced_commit / last_synced_at — same columns as
// EnsureClonedAndSynced, but with one crucial difference: failures return
// a non-nil error so the design stage can HARD-BLOCK instead of falling
// through to a stale local snapshot.
//
// Skipped=true results (non-git / no origin / empty repo) carry a nil
// error and BaseSHA=""; the design stage treats these as "no baseline to
// record" and skips UpdateDesignBaseSHA entirely.
func (s *ProjectService) SyncDesignBase(ctx context.Context, projectID string, timeout time.Duration, logf func(string)) (GitSyncResult, error) {
	var res GitSyncResult
	if projectID == "" {
		return res, fmt.Errorf("projectID is empty")
	}
	p, err := s.Get(projectID)
	if err != nil {
		return res, err
	}
	if p.LocalPath == "" {
		return res, fmt.Errorf("project has no local_path")
	}

	// Auto-clone when the directory is missing — matches EnsureClonedAndSynced's
	// case-2 path so a fresh Docker workspace bind-mount doesn't break the design
	// stage. EnsureCloned's own error message already names the project, so we
	// surface it verbatim and stamp sync_status=error for the audit row.
	if _, statErr := os.Stat(p.LocalPath); statErr != nil {
		if !os.IsNotExist(statErr) {
			return res, fmt.Errorf("stat project dir: %w", statErr)
		}
		if cerr := s.EnsureCloned(projectID); cerr != nil {
			_ = s.updateSyncStatus(projectID, SyncStatusError, "", time.Now())
			log.Printf("[git-sync] auto-clone failed for %s: %v", projectID, cerr)
			return res, fmt.Errorf("auto-clone failed: %w", cerr)
		}
	}

	res, err = SyncToOriginBase(ctx, p.LocalPath, p.DefaultBranch, timeout, logf)
	if err != nil {
		// *SyncGateError is the design-stage rejection signal we EXPECT to
		// see; everything else is an unexpected DB / git plumbing error.
		// Either way we stamp sync_status=error so the audit row reflects
		// reality, and re-return the original error untouched.
		_ = s.updateSyncStatus(projectID, SyncStatusError, "", time.Now())
		return res, err
	}
	if res.Skipped {
		// Skipped: nothing was synced, nothing to stamp. Leaving sync_status
		// untouched matches syncBaseBranchService's behaviour for non-git /
		// no-origin cases.
		return res, nil
	}
	if res.BaseSHA != "" {
		_ = s.updateSyncStatus(projectID, SyncStatusOK, res.BaseSHA, time.Now())
	}
	return res, nil
}