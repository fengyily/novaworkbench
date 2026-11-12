package service

import (
	"context"
	"errors"
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

// gitRefExists reports whether a fully-qualified ref resolves in the repo.
// The fetch-failure triage uses it to tell "the remote-tracking ref is still
// here" (the remote HAS the branch, the fetch died on network/credentials)
// from "the remote never had this branch".
func gitRefExists(repoPath, ref string) bool {
	_, err := gitRunPlain(repoPath, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// remoteRefState is the outcome of probing the remote for a branch.
type remoteRefState int

const (
	// remoteRefUnknown — the remote could not be queried (unreachable,
	// unauthenticated, timed out). We learned nothing about the upstream,
	// so the caller must NOT assume the local ref stands in for it.
	remoteRefUnknown remoteRefState = iota
	// remoteRefPresent — the remote has refs/heads/<ref>.
	remoteRefPresent
	// remoteRefAbsent — the remote is reachable but has no such branch.
	remoteRefAbsent
)

// probeRemoteRef asks the remote whether it has refs/heads/<ref> without
// touching the working tree.
//
// The answer is read from `git ls-remote --exit-code`'s exit status rather
// than from its human-readable message, because that message is localized:
// git says "couldn't find remote ref <x>" on an en-US host and
// "无法找到远程引用 <x>" on a zh-CN host. The exit status is not:
// 0 = the remote has the branch, 2 = the remote is reachable but has no
// matching ref, 128 = the remote could not be queried at all.
//
// Only remoteRefAbsent justifies degrading a failed fetch to the local ref —
// it is the one answer that proves "the upstream baseline has simply not been
// pushed yet" instead of "we could not reach the upstream".
func (s *ProjectService) probeRemoteRef(ctx context.Context, repoPath, projectID, ref string, timeout time.Duration) remoteRefState {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(pctx, "git", "-C", repoPath, "ls-remote", "--exit-code", "--heads", "origin", ref)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if projectID != "" && s.gitCredSvc != nil {
		credEnv, credCleanup, _ := s.gitCredSvc.BuildEnv(ctx, projectID)
		if credCleanup != nil {
			defer credCleanup()
		}
		cmd.Env = append(cmd.Env, credEnv...)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return remoteRefPresent
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
		return remoteRefAbsent
	}
	return remoteRefUnknown
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
//	B   rev-parse --abbrev-ref HEAD      → record cur branch     (no rejection)
//	C   fetch origin <base>              → non-zero exit         → FETCH_FAILED
//	D   rev-list --count <baseRef>..HEAD → > 0  (only when HEAD on base) → UNPUSHED_COMMITS
//	E   merge --ff-only <baseRef>        → non-zero exit (only when HEAD on base) → FF_FAILED
//	F   rev-parse <baseRef>              → BaseSHA
//
// <baseRef> is "origin/<base>" normally, but it degrades to the plain
// "<base>" local branch when the remote genuinely does not have the branch
// yet — see the Step C triage below.
//
// Steps D and E only run when HEAD is sitting on baseBranch — they're the
// "fast-forward the local base ref" path, which requires HEAD to be on the
// base branch to mean anything. When HEAD is on a different branch (the
// launch-plan case: user is on main, picked fix/<x> as the design baseline)
// we skip D/E entirely and just record origin/<base> SHA from the fetch —
// the design stage stamps that SHA verbatim and resolveStartRef / the worktree
// already prefer origin/<base> over the local ref, so a stale local <base>
// ref doesn't poison anything.
//
// Step C's fetch failure is triaged instead of failing outright. A child
// requirement inherits its parent's branch (feat/req_<parent>) as its design
// baseline, and that branch routinely has NOT been pushed yet — aborting the
// whole design stage for that would make the "child follows parent" rule
// unusable. So a fetch failure degrades to the local ref ONLY when we can
// prove the remote simply lacks the branch; a network / credential failure
// keeps failing hard, because then we know nothing about the upstream.
//
// logf may be nil (silent). The function never mutates the working tree on
// the failure path — every gate runs read-only first, so a DIRTY_WORKTREE
// rejection leaves any uncommitted changes exactly where they were. ctx is
// used both as the parent deadline and as the per-fetch timeout (the fetch
// itself caps at `timeout`).
//
// projectID, when non-empty, is used to look up the project's bound platform
// token and inject a GIT_ASKPASS env on the Step C fetch so HTTPS private
// repos with a project-bound token actually authenticate. Empty projectID
// (or a nil gitCredSvc) keeps the historical GIT_TERMINAL_PROMPT=0-only
// behaviour — preserves the unit tests' shape (projectID="") and the
// historical best-effort fallback for callers that haven't threaded a
// projectID through.
func (s *ProjectService) SyncToOriginBase(ctx context.Context, projectID, repoPath, baseBranch string, timeout time.Duration, logf func(string)) (GitSyncResult, error) {
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

	// Step B: capture HEAD's branch so steps D / E (the "fast-forward the
	// local base ref" path, which only makes sense when HEAD is on the base
	// branch) can be skipped when HEAD is on something else. Detached HEADs
	// report as literal "HEAD" from --abbrev-ref; we treat that the same as
	// any non-base branch — cur stays non-empty so D/E get skipped. A
	// genuine rev-parse failure (corrupted HEAD) is recorded as cur=""
	// (same skip), which is safer than rejecting on a transient read error.
	//
	// We no longer reject when HEAD is on a different branch from baseBranch:
	// launch-plan lets users pick a non-default baseline while sitting on
	// main, and forcing them to `git checkout <base>` first broke the flow.
	// The design SHA we stamp comes from origin/<base> directly, and
	// resolveStartRef / the worktree prefer origin/<base> over the local
	// ref — so a stale local <base> ref doesn't poison the design.
	var curBranch string
	if head, herr := gitRunPlain(repoPath, "rev-parse", "--abbrev-ref", "HEAD"); herr == nil {
		curBranch = strings.TrimSpace(head)
	}

	// Step C: fetch origin <base> under GIT_TERMINAL_PROMPT=0 so a stalled
	// network / missing creds can never hang us past `timeout`. We DON'T
	// update origin/<base> from a stale local snapshot — fetch is the only
	// source of truth for what the upstream currently is. When the project
	// is bound to a platform token, layer the askpass env on top so the
	// fetch actually authenticates against HTTPS remotes — credEnv is
	// non-empty only when a token was found.
	//
	// baseRef is the ref the rest of the gate reads. It starts as the
	// remote-tracking ref and degrades to the plain local branch when the
	// fetch failed because the remote genuinely doesn't have the branch
	// yet (localOnly). Every downstream step must use baseRef, never a
	// hardcoded "origin/"+baseBranch, or it would reference a ref that
	// doesn't exist in the degraded path.
	baseRef := "origin/" + baseBranch
	localOnly := false

	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fetchCmd := exec.CommandContext(fctx, "git", "-C", repoPath, "fetch", "origin", baseBranch)
	fetchEnv := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if projectID != "" && s.gitCredSvc != nil {
		credEnv, credCleanup, _ := s.gitCredSvc.BuildEnv(ctx, projectID)
		if credCleanup != nil {
			defer credCleanup()
		}
		fetchEnv = append(fetchEnv, credEnv...)
	}
	fetchCmd.Env = fetchEnv
	var fetchErrOut strings.Builder
	fetchCmd.Stderr = &fetchErrOut
	if err := fetchCmd.Run(); err != nil {
		fetchStderr := fetchErrOut.String()
		// Triage the failure before giving up. Three shapes, one of which
		// must NOT abort the design stage:
		//
		//  1. The remote-tracking ref refs/remotes/origin/<base> is still
		//     here → a previous successful fetch saw the branch upstream,
		//     so this fetch died on network/credentials → hard fail.
		//  2. Neither the remote nor the local repo has the branch →
		//     there is no baseline to sync against → hard fail.
		//  3. The remote-tracking ref is gone but the local branch
		//     exists AND the remote answers "I don't have that branch"
		//     → the upstream baseline simply has not been pushed yet →
		//     degrade to the local ref and carry on.
		//
		// Case 3 is the one that matters: a child requirement's baseline is
		// its parent's branch (feat/req_<parent>), which is usually local
		// only until the parent gets merged. Failing there would make the
		// "child follows parent" rule unusable.
		originRefExists := gitRefExists(repoPath, "refs/remotes/origin/"+baseBranch)
		localRefExists := gitRefExists(repoPath, "refs/heads/"+baseBranch)
		if !originRefExists && localRefExists {
			if s.probeRemoteRef(ctx, repoPath, projectID, baseBranch, timeout) == remoteRefAbsent {
				localOnly = true
				baseRef = baseBranch
				syncLog("ℹ️ origin/" + baseBranch + " 尚未推送，改用本地 " + baseBranch + " 作为基线")
			}
		}
		if !localOnly {
			return res, &SyncGateError{
				Code: SyncGateFetchFailed,
				Msg: fmt.Sprintf(
					"无法拉取 origin/%s，请检查网络与凭据后重试",
					baseBranch),
				Stderr: capStderr(fetchStderr),
			}
		}
	}

	// Steps D + E: only meaningful when HEAD is on baseBranch. When the user
	// is sitting on a different branch (e.g. main with design baseline =
	// fix/<x>) HEAD's "ahead-of-origin/<base>" count is irrelevant to the
	// design stage — the design prompt will branch off origin/<base>, not
	// off HEAD. Skipping these steps lets launch-plan users run design
	// against a non-default baseline without manually switching branches.
	if curBranch == baseBranch {
		// Step D: must run AFTER step C's fetch — using a stale origin/<base>
		// would compute an ahead count against the wrong tip. rev-list --count
		// prints the integer directly so we Atoi it; anything non-zero means
		// HEAD has unpushed commits, which would block --ff-only.
		countOut, cerr := gitRunPlain(repoPath, "rev-list", "--count", baseRef+"..HEAD")
		if cerr == nil {
			if n, nerr := atoiTrimmed(countOut); nerr == nil && n > 0 {
				// Capture the actual ahead commits so the user sees exactly
				// what they'd need to push (or reset) — much more useful than
				// the bare count.
				detail, _ := gitRunPlain(repoPath, "log", "--oneline", baseRef+"..HEAD")
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
		mffOut, mffErr := gitRunPlain(repoPath, "merge", "--ff-only", baseRef)
		if mffErr != nil {
			return res, &SyncGateError{
				Code: SyncGateFFFailed,
				Msg: fmt.Sprintf(
					"无法 fast-forward 到 %s，请检查仓库状态后重试",
					baseRef),
				Stderr: capStderr(mffOut + "\n" + mffErr.Error()),
			}
		}
	} else {
		// HEAD on a different branch — the launch-plan / non-default-baseline
		// case. Emit a short note so the SSE panel doesn't go silent during
		// the fetch (and so a confused user can see HEAD is intentionally on
		// a non-base branch). The SHA we stamp comes from baseRef so
		// resolveStartRef / the worktree pick the fresh upstream tip.
		branchLabel := curBranch
		if branchLabel == "" {
			branchLabel = "<unreadable>"
		}
		syncLog("ℹ️ 当前 HEAD 在 `" + branchLabel + "` 分支，跳过本地 `merge --ff-only`（design 基线 = `" + baseRef + "`）")
	}

	// Step F: capture the final SHA + emit the human-readable success lines
	// in the same style as syncBaseBranchService so existing readers aren't
	// surprised by an unfamiliar log format.
	sha, shaErr := gitRunPlain(repoPath, "rev-parse", baseRef)
	if shaErr != nil {
		return res, &SyncGateError{
			Code: SyncGateFFFailed,
			Msg: "无法读取同步后的 " + baseRef + " SHA",
			Stderr: capStderr(shaErr.Error()),
		}
	}
	res.BaseSHA = strings.TrimSpace(sha)
	if res.BaseSHA != res.OldSHA && res.OldSHA != "" {
		res.FastForwarded = true
	}

	syncLog("🔄 已同步 " + baseRef)
	if res.BaseSHA != "" {
		if tip, terr := gitRunPlain(repoPath, "log", "-1", baseRef,
			"--format=%h %s (%an, %ad)", "--date=format:%Y-%m-%d %H:%M"); terr == nil {
			if tip = strings.TrimSpace(tip); tip != "" {
				syncLog("📌 " + baseRef + " 最新提交: " + tip)
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
//
// baseBranch is an explicit override: when non-empty it takes precedence over
// project.DefaultBranch so the design stage can sync against a non-default
// base (release-/* / incoming-*). When empty, the function falls back to
// project.DefaultBranch, preserving the historical behaviour for any caller
// that has not yet been migrated to pass the override.
func (s *ProjectService) SyncDesignBase(ctx context.Context, projectID, baseBranch string, timeout time.Duration, logf func(string)) (GitSyncResult, error) {
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

	// baseBranch override: explicit > project.DefaultBranch. Empty here
	// means "no override requested" so we fall back to project.DefaultBranch,
	// preserving existing callers.
	effectiveBase := baseBranch
	if effectiveBase == "" {
		effectiveBase = p.DefaultBranch
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

	res, err = s.SyncToOriginBase(ctx, p.ID, p.LocalPath, effectiveBase, timeout, logf)
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