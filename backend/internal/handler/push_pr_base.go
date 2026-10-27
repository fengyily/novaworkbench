// Package handler — shell-driven "commit + push base" for the LOCAL no-branch
// auto-push path (v0.6.x).
//
// push_pr_base.go is the no-branch counterpart of push_pr_shell.go: it runs
// the "提交 → 推送 base" flow as a deterministic shell sequence (git only,
// no gh / glab / tea) inside a JobStore job, so the SubTaskPanel SSE stream +
// job_logs durability all keep working. No PR step — the user explicitly
// chose to develop on base, so the PR head==base rejection is the whole point.
//
// Differences from push_pr_shell.go:
//   - dev == base (no dev branch; we operate on the requirement's designated
//     base branch — requirements.base_branch, with the project's
//     default_branch only as the last-resort fallback)
//   - no "merge origin/base into dev" step (already on base)
//   - no gh/glab/tea PR step
//   - source tag = SubTaskSourcePushBase (so SubTaskPanel / merge stage can
//     render 「已在 base 完成」 instead of the PR-link badge)
//
// Routing: autoPushPR (wizard_common.go) calls RunPushBaseShellJob for
// NoBranch==true LOCAL requirements. MergeHandler.Push (manual button)
// routes here too — see merge.go Push no-branch branch. Remote path is
// unreachable for no-branch (StartCoding prologue refuses agent_server +
// no_branch combination).
//
// Methods live on *SubTaskRunner rather than *WizardHandler / *MergeHandler
// so both call sites can share them — SubTaskRunner already owns the
// projectSvc / subTaskSvc / reqSvc / jobs / jobLogSvc dependencies the body
// needs.
package handler

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
)

// RunPushBaseShellJob dispatches a shell-driven commit+push-base job for a
// LOCAL no-branch requirement. Mirrors runPushPRShellJob's shape (idempotency
// guard → NewPendingSubTask → goroutine body → job_logs persistence) but the
// source tag is SubTaskSourcePushBase and the title says "推送 base 分支".
//
// base/remote/platformType/commitMessage are the same params autoPushPR /
// MergeHandler.Push resolve. pushModel/pushCfgID/commitLang are unused by the
// shell body (git doesn't talk to an LLM) but accepted for signature parity
// with runPushPRShellJob so the two paths can share call sites.
//
// Returns (jobID, subTaskID, error) like runPushPRShellJob so autoPushPR /
// MergeHandler.Push can log symmetrically.
// autoCommit / autoPushRemote gate the post-development stages (the same
// split as the feature-branch push_pr_shell path): when autoCommit=false
// we bail (nothing to push); when autoPushRemote=false we still commit but
// skip the push step. PR creation is always skipped on the no-branch path
// (gh/glab/tea reject head==base by design — auto_create_pr is documented
// as a no-op here).
func (r *SubTaskRunner) RunPushBaseShellJob(reqRow *model.Requirement, base, remote, platformType, commitMessage, pushModel, pushCfgID, commitLang, prTitle string, autoCommit, autoPushRemote bool) (jobID, subTaskID string, err error) {
	if r == nil || r.subTaskSvc == nil {
		log.Printf("[push-base-shell] %s: sub-task runner not wired, skip", reqRow.ID)
		return "", "", nil
	}
	if !reqRow.NoBranch {
		// Defensive: caller misrouted a feature-branch requirement; refuse
		// rather than silently push base on a requirement that was developed
		// in a feature branch. The PR path would have rejected dev==base
		// anyway; here we surface a clear log line so the operator sees why.
		log.Printf("[push-base-shell] %s: requirement is not in no-branch mode (reqRow.NoBranch=false), skip", reqRow.ID)
		return "", "", nil
	}
	if !autoCommit {
		log.Printf("[push-base-shell] %s: auto_commit disabled, skip (push=%v)", reqRow.ID, autoPushRemote)
		return "", "", nil
	}

	// Idempotency guard — same window as runPushPRShellJob so a manual button
	// click racing an auto dispatch collapses onto one job. We use the same
	// SubTaskSourcePushPR-aware lookup because FindRecentPushForReq already
	// keys on the push sub-task family; the additional source filter inside
	// the lookup keeps push_base / push_pr from racing each other on the
	// same requirement (req_xxx cannot be in both modes simultaneously).
	const lookbackSec = 90
	if existing, ferr := r.subTaskSvc.FindRecentPushForReq(reqRow.ID, lookbackSec); ferr != nil {
		log.Printf("[push-base-shell] %s: idempotency lookup failed: %v (continuing)", reqRow.ID, ferr)
	} else if existing != nil {
		log.Printf("[push-base-shell] %s: idempotent hit existing sub_task=%s job_id=%s status=%s", reqRow.ID, existing.ID, existing.JobID, existing.Status)
		return existing.JobID, existing.ID, nil
	}

	// Resolve the local workdir: the requirement's isolated worktree. A
	// no-branch requirement now develops in a DETACHED HEAD worktree parked
	// on the base line, so the commit lives there and the push has to happen
	// from there too. Rows written by an older build (which developed
	// straight in the project checkout) have an empty worktree_path and fall
	// back to the project directory — their behaviour is unchanged.
	proj, perr := r.projectSvc.Get(reqRow.ProjectID)
	if perr != nil || proj == nil {
		log.Printf("[push-base-shell] %s: project load failed (%v), skip", reqRow.ID, perr)
		return "", "", nil
	}
	dir := resolveNoBranchWorkDir(reqRow, proj.LocalPath)
	if dir == "" {
		log.Printf("[push-base-shell] %s: no local workdir, skip", reqRow.ID)
		return "", "", nil
	}

	shortDesc := "本地 shell 推送 base 分支: " + base
	var title string
	switch {
	case autoPushRemote:
		title = "推送 base 分支"
	default:
		title = "本地提交 base 分支"
		shortDesc = "本地 shell 提交 base 分支（不推送）: " + base
	}
	st, job, _, nerr := r.NewPendingSubTask(reqRow.ID, title, shortDesc, "", "", "", "", "", "")
	if nerr != nil {
		return "", "", nerr
	}
	if uerr := r.subTaskSvc.UpdateSource(st.ID, model.SubTaskSourcePushBase); uerr != nil {
		log.Printf("[push-base-shell] %s: failed to stamp source=push_base on %s: %v", reqRow.ID, st.ID, uerr)
	}
	log.Printf("[push-base-shell] %s: dispatched shell sub_task=%s job_id=%s dir=%s base=%s commit=%v push=%v", reqRow.ID, st.ID, job.ID, dir, base, autoCommit, autoPushRemote)

	go r.execPushBaseShell(reqRow, st, job, dir, proj.LocalPath, base, remote, platformType, commitMessage, pushModel, pushCfgID, commitLang, prTitle, autoPushRemote)
	return job.ID, st.ID, nil
}

// execPushBaseShell is the goroutine body for RunPushBaseShellJob. Mirrors
// execPushPRShell's scaffolding (deferred Save / MarkRunning / panic
// recovery / admission gates) so the SubTaskPanel lifecycle is identical
// across both shell jobs. The body itself is the no-branch subset:
// commit (step 1) → fetch (step 2, best-effort) → push base (step 3, with
// pull --rebase retry) → done. No merge / no PR step.
//
// assembleGitCredEnv is on *WizardHandler (lives outside the runner because
// the coding path also uses it and it depends on platformSvc / gitCredSvc /
// worktreeH wired into WizardHandler). MergeHandler also has these wired,
// so we resolve the cred env at call time via the runner's credential
// helper — currently a thin pass-through to assembleGitCredEnv's logic
// shared via the gitRefSignature helper (see sub_task_runner.go).
func (r *SubTaskRunner) execPushBaseShell(reqRow *model.Requirement, st *model.SubTask, job *store.Job, dir, projectPath, base, remote, platformType, commitMessage, pushModel, pushCfgID, commitLang, prTitle string, autoPushRemote bool) {
	var runStartTime time.Time
	defer func() {
		lines, status, exitCode := job.Snapshot()
		log.Printf("[push-base-shell] defer-Save job_id=%s req_id=%s sub_task_id=%s status=%s exit=%d lines=%d", job.ID, st.RequirementID, st.ID, status, exitCode, len(lines))
		if perr := r.jobLogSvc.Save(job.ID, st.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, ""); perr != nil {
			log.Printf("[push-base-shell] failed to persist job log %s: %v", job.ID, perr)
		}
		if r.subTaskSvc != nil {
			finalStatus := model.SubTaskStatusDone
			if status == store.JobError {
				finalStatus = model.SubTaskStatusError
			}
			artifactBody := renderShellJobLog(lines)
			artifact := buildSubTaskArtifact(st, "", artifactBody, time.Now())
			if ferr := r.subTaskSvc.Finish(st.ID, finalStatus, artifact, "", model.SubTaskTokens{}, 0, runStartTime); ferr != nil {
				log.Printf("[push-base-shell] failed to finish sub_task %s: %v", st.ID, ferr)
			}
		}
		if terr := r.reqSvc.Touch(st.RequirementID); terr != nil {
			log.Printf("[push-base-shell] touch requirement %s: %v", st.RequirementID, terr)
		}
	}()
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[push-base-shell] panic recovered in job %s (sub_task %s): %v", job.ID, st.ID, rec)
			job.Append(store.LogLine{Type: "error", Content: "❌ 内部异常，任务已中止: " + fmt.Sprint(rec)})
		}
		if _, status, _ := job.Snapshot(); status == store.JobRunning {
			job.Finish(1, store.JobError)
		}
	}()

	release := r.AcquireGates(reqRow.ProjectID, job)
	defer release()

	if cur, gerr := r.subTaskSvc.Get(st.ID); gerr == nil && cur != nil && cur.Status == model.SubTaskStatusStopped {
		job.Append(store.LogLine{Type: "message", Content: "⏹ 排队期间已被停止，未启动执行"})
		job.Finish(0, store.JobDone)
		return
	}
	runStartTime, mErr := r.subTaskSvc.MarkRunning(st.ID)
	if mErr != nil {
		log.Printf("[push-base-shell] failed to mark running for %s: %v", st.ID, mErr)
	}

	job.Append(store.LogLine{Type: "phase", Content: "🚀 本地 shell 推送 base 流程启动（no-branch 模式, base=" + base + "）"})

	// ── Sanity: the no-branch mode's whole point is "this requirement's work
	// lands on the base branch, and no branch is created anywhere". The
	// working dir is normally a DETACHED HEAD worktree parked on the base
	// line, but the coding agent can leave a branch checked out instead —
	// a detached HEAD looks broken, so the agent runs `git checkout -b` and
	// carries on (req_1a42496198026380, 2026-10-27: the agent created
	// feat/1a42496198026380, pushed it and opened a PR, and this guard then
	// refused the platform's own push with "cur != base").
	//
	// So the guard does NOT refuse. The branch name is irrelevant here — the
	// isolated worktree belongs to THIS requirement, and git itself still
	// enforces the only rule that matters (a non-fast-forward push is
	// rejected; we never force-push). We push the current HEAD straight onto
	// the base branch and clean the stray branch up afterwards.
	cur := currentBranch(dir)
	detached := cur == "" || cur == "HEAD"
	strayBranch := ""
	switch {
	case detached:
		job.Append(store.LogLine{Type: "message",
			Content: "🌿 从隔离 worktree 的 detached HEAD 直推 base: " + base + "（不新建分支）"})
	case cur == base:
		job.Append(store.LogLine{Type: "message",
			Content: "🌿 隔离 worktree 已在 base 分支 " + base + " 上，直接推送（不新建分支）"})
	default:
		strayBranch = cur
		job.Append(store.LogLine{Type: "warning",
			Content: "⚠️ 隔离 worktree 的 HEAD 在分支 " + cur + "（不是 base=" + base + "）—— 这通常是开发代理自行创建了分支。" +
				"不创建分支模式下不新建任何分支，将把当前 HEAD 直接推送到 base 分支 " + base + "。"})
	}

	credEnv, credCleanup := r.assembleShellJobCredEnv(reqRow, dir, projectPath, job)
	if credCleanup != nil {
		defer credCleanup()
	}
	git := func(args ...string) (string, error) {
		return gitRunWithTimeout(dir, pushShellStepTimeout, credEnv, args...)
	}

	// ── Step 1: commit uncommitted changes (idempotent — no-op if the coding
	// agent already committed, which is the common case).
	if dirty, _ := git("status", "--porcelain"); strings.TrimSpace(dirty) != "" {
		job.Append(store.LogLine{Type: "message", Content: "📝 工作区有未提交改动，执行 git add -A && git commit"})
		if _, gerr := git("add", "-A"); gerr != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ git add 失败: " + gerr.Error()})
			job.Finish(1, store.JobError)
			return
		}
		msg := strings.TrimSpace(commitMessage)
		if msg == "" {
			// Fall back to the requirement title, not the branch name —
			// reusing `base` as a commit message would produce "main" every
			// time. The title (or the reqID as last resort) is the
			// human-meaningful default.
			if title := strings.TrimSpace(reqRow.Title); title != "" {
				msg = title
			} else {
				msg = reqRow.ID
			}
		}
		if _, cerr := git("commit", "-m", msg, "--no-verify"); cerr != nil {
			// commit can fail benignly when add raced with another committer;
			// surface but continue to push (the existing commits still need
			// pushing).
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ git commit 未产生新提交（可能已无改动）: " + cerr.Error()})
		}
	} else {
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ 工作区干净，跳过提交"})
	}

	// ── Step 2: fetch base (best-effort — no remote / offline just proceeds
	// with the local base).
	if _, ferr := git("fetch", "origin", base); ferr != nil {
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ git fetch origin " + base + " 跳过: " + ferr.Error()})
	}

	// ── Step 3: commit-only bail. No PR step (no-branch path always skips
	// PR creation; gh/glab/tea reject head==base). When autoPushRemote=false
	// the user opted out of pushing — bail here so the commit sits locally
	// until they push manually.
	if !autoPushRemote {
		job.Append(store.LogLine{Type: "result", Content: "✅ 已本地提交至 base 分支 " + base + "（未推送 — 在 start-coding 弹窗里关掉了 auto_push_remote）"})
		job.Append(store.LogLine{Type: "done", Content: "✅ commit-only 流程完成"})
		job.Finish(0, store.JobDone)
		log.Printf("[push-base-shell] job %s finished for %s (autoPushRemote=false, commit-only)", job.ID, reqRow.ID)
		return
	}
	// ── Step 4 (conditional): push the current HEAD onto the base branch.
	//
	// The refspec form (HEAD:refs/heads/<base>) is used unconditionally and
	// `-u` is never added: it pushes whatever HEAD points at — the detached
	// HEAD of the isolated worktree, or the branch the coding agent left
	// checked out — straight onto the base branch. No branch is created on
	// either side; only refs/heads/<base> moves forward.
	pushArgs := pushBaseRefspecArgs(base)
	if _, perr := git(pushArgs...); perr != nil {
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ 首次 push base 失败，尝试 rebase 后重推: " + perr.Error()})
		// `git pull --rebase` has no clear meaning on a detached HEAD, so
		// fetch + rebase explicitly (git supports rebase on a detached
		// HEAD, and on a branch just as well). Never force-push — a rejected
		// push is the user's cue to resolve the conflict themselves.
		if _, ferr := git("fetch", "origin", base); ferr != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ git fetch origin " + base + " 失败: " + ferr.Error()})
			job.Finish(1, store.JobError)
			return
		}
		if _, rerr := git("rebase", "origin/"+base); rerr != nil {
			// Abort so the worktree stays usable and the user can resolve
			// the conflict by hand.
			_, _ = git("rebase", "--abort")
			job.Append(store.LogLine{Type: "error", Content: "❌ git rebase origin/" + base + " 失败（可能与 base 上的新提交冲突）: " + rerr.Error()})
			job.Finish(1, store.JobError)
			return
		}
		if out2, perr2 := git(pushArgs...); perr2 != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ git push base 失败: " + out2 + " " + perr2.Error()})
			appendProtectedBaseHint(job, out2, perr2)
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "message", Content: "⬆️ 已推送 base (rebase 后)"})
	} else {
		job.Append(store.LogLine{Type: "message", Content: "⬆️ 已推送 base → origin"})
	}

	// ── Step 5: restore the no-branch shape when the coding agent left a
	// branch of its own checked out in the isolated worktree.
	//
	// The work is on the base branch now, so the stray branch is a leftover
	// artifact of the no-branch mode: detach HEAD again (the commits stay
	// reachable — they are on base now) and drop the LOCAL branch. The
	// remote branch is NOT deleted — the agent may have pushed it and opened
	// a PR, and closing that out is the user's call. We only report it.
	if strayBranch != "" {
		cleanupStrayBranchAfterPush(job, dir, strayBranch, git)
	}

	// The push advanced refs/heads/<base> on the remote and origin/<base>
	// locally, but the project checkout's LOCAL base branch still points at
	// the old commit. Fast-forward it when the project directory is sitting
	// on that branch — best effort, a failure here is cosmetic.
	if cur := currentBranch(projectPath); cur == base {
		if _, err := gitRun(projectPath, "merge", "--ff-only", "origin/"+base); err != nil {
			log.Printf("[push-base-shell] %s: 项目 checkout 快进 base 失败（可忽略）: %v", reqRow.ID, err)
		}
	}

	job.Append(store.LogLine{Type: "result", Content: "✅ 已提交并推送至 base 分支 " + base + "\n\n(no-branch 模式:跳过 PR 创建,head==base 在 gh/glab/tea 会拒绝)"})
	job.Append(store.LogLine{Type: "done", Content: "✅ 推送 base 流程完成"})
	job.Finish(0, store.JobDone)
	log.Printf("[push-base-shell] job %s finished for %s", job.ID, reqRow.ID)
}

// cleanupStrayBranchAfterPush restores the no-branch shape after the base
// push succeeded while the working dir sat on a branch the coding agent had
// created for itself.
//
// What it does, in order:
//
//  1. Reports whether the stray branch also exists on the remote (the agent
//     may have pushed it and opened a PR — req_1a42496198026380 did exactly
//     that). The remote branch is NOT deleted: a PR may be waiting for
//     review, and closing it out is the user's call. The log line carries
//     the exact command so the cleanup is one copy-paste away.
//  2. Detaches HEAD again (`git checkout --detach`) — the commits stay
//     reachable, they are on the base branch now.
//  3. Drops the LOCAL branch with `git branch -d` (the safe variant: git
//     refuses when the branch is not merged, and we never force it away).
//
// Every step is best-effort: a failure here is cosmetic compared to the
// push that already succeeded, so it is logged and never fails the job.
//
// git is the caller's credential-aware git runner (see execPushBaseShell).
func cleanupStrayBranchAfterPush(job *store.Job, dir, strayBranch string, git func(args ...string) (string, error)) {
	if job == nil || dir == "" || strayBranch == "" {
		return
	}
	// 1. Did the agent push the stray branch to the remote?
	if upstream, err := git("rev-parse", "--abbrev-ref", strayBranch+"@{upstream}"); err == nil &&
		strings.TrimSpace(upstream) != "" {
		job.Append(store.LogLine{Type: "warning",
			Content: "⚠️ 开发代理曾把分支 " + strayBranch + " 推送到远端（" + strings.TrimSpace(upstream) + "）—— 不创建分支模式下不应保留它。" +
				"如需清理请手动执行: git push origin --delete " + strayBranch})
	}
	// 2. Detach HEAD again so the worktree matches the no-branch shape.
	if out, err := git("checkout", "--detach"); err != nil {
		job.Append(store.LogLine{Type: "warning",
			Content: "⚠️ 未能把隔离 worktree 切回 detached HEAD（分支 " + strayBranch + "）: " + strings.TrimSpace(out+" "+err.Error())})
		return
	}
	job.Append(store.LogLine{Type: "message",
		Content: "🌿 隔离 worktree 已切回 detached HEAD（分支 " + strayBranch + " 不再被检出）"})
	// 3. Drop the local branch. `-d` refuses when the branch is not merged
	// into HEAD — we never force it.
	if out, err := git("branch", "-d", strayBranch); err != nil {
		job.Append(store.LogLine{Type: "warning",
			Content: "ℹ️ 本地分支 " + strayBranch + " 未删除（可能未合并或被其它 worktree 占用）: " +
				strings.TrimSpace(out+" "+err.Error()) + "。如需清理请手动执行: git branch -D " + strayBranch})
		return
	}
	job.Append(store.LogLine{Type: "message",
		Content: "🧹 已删除不创建分支模式下的残留本地分支: " + strayBranch})
}

// appendProtectedBaseHint surfaces an actionable message when the remote
// rejected the push because the base branch is protected (GitHub branch
// protection / required PR). The raw remote text is passed through so the
// operator sees the platform's own wording.
//
// The no-branch path never force-pushes — a protected base is a policy
// decision, and silently overwriting it would be the wrong move. The hint
// points the user at the branch strategy instead.
func appendProtectedBaseHint(job *store.Job, out string, err error) {
	if job == nil {
		return
	}
	raw := strings.ToLower(out + " " + err.Error())
	hit := strings.Contains(raw, "protected") ||
		strings.Contains(raw, "denied") ||
		strings.Contains(raw, "pre-receive hook declined") ||
		strings.Contains(raw, "拒绝")
	if !hit {
		return
	}
	job.Append(store.LogLine{Type: "error",
		Content: "❌ base 分支受保护，推送被拒: " + strings.TrimSpace(out) + "。请改用「新建 feat/req_xxx 分支」策略。"})
}

// assembleShellJobCredEnv is the SubTaskRunner-side twin of WizardHandler.
// assembleGitCredEnv — same GIT_AUTHOR_* / askpass / GPG logic but uses the
// runner's projectSvc + platformSvc instead of the handler's. Lives here
// rather than being shared via a helper function so the GPG provisioning
// path doesn't have to thread platformSvc / projectSvc through every
// caller. The credential_env + GPG helpers (gitCredentialEnv /
// provisionLocalGPG) are already package-level so the body mirrors the
// WizardHandler version byte-for-byte. Kept private — only the shell-job
// goroutines in this file use it.
func (r *SubTaskRunner) assembleShellJobCredEnv(reqRow *model.Requirement, workDir, projectPath string, job *store.Job) (env []string, cleanup func()) {
	var cleanups []func()
	cleanup = func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	if reqRow == nil {
		return env, cleanup
	}
	name, email := lookupGitIdentity(r.projectSvc, r.platformSvc, reqRow)
	if name != "" {
		env = append(env, "GIT_AUTHOR_NAME="+name, "GIT_COMMITTER_NAME="+name)
	}
	if email != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_EMAIL="+email)
	}
	if credEnv, credCleanup := gitCredentialEnv(r.projectSvc, r.platformSvc, reqRow); len(credEnv) > 0 {
		env = append(env, credEnv...)
		cleanups = append(cleanups, credCleanup)
	}
	if project, pErr := r.projectSvc.Get(reqRow.ProjectID); pErr == nil && project != nil && project.PlatformTokenID != "" {
		enabled, _, armored, passphrase, gpgErr := r.platformSvc.GPGSigningMaterial(project.PlatformTokenID)
		switch {
		case gpgErr != nil:
			if job != nil {
				job.Append(store.LogLine{Type: "message", Content: "⚠️ 读取 GPG 配置失败：" + gpgErr.Error() + "，本次提交将不签名"})
			}
		case enabled && armored != "":
			if job != nil {
				job.Append(store.LogLine{Type: "phase", Content: "🔐 配置本地 GPG 签名..."})
			}
			gnupgHome, keyID, gpgCleanup, provErr := provisionLocalGPG(workDir, projectPath, armored, passphrase, name, email)
			if provErr != nil {
				if gpgCleanup != nil {
					gpgCleanup()
				}
				if job != nil {
					job.Append(store.LogLine{Type: "message", Content: "⚠️ " + provErr.Error() + "，本次提交将不签名"})
				}
			} else {
				cleanups = append(cleanups, gpgCleanup)
				if job != nil {
					job.Append(store.LogLine{Type: "message", Content: "✅ 本地 GPG 已就绪（keyid=" + keyID + "）"})
				}
				_ = r.platformSvc.UpdateGPGKeyID(project.PlatformTokenID, keyID)
				_ = gnupgHome
			}
		case enabled:
			if job != nil {
				job.Append(store.LogLine{Type: "message", Content: "⚠️ 已启用 GPG 签名但未保存私钥，本次提交未签名"})
			}
		}
	}
	return env, cleanup
}