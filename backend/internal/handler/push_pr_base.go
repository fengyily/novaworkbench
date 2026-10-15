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
//   - dev == base (no dev branch; we operate on the project's base branch)
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

	// Resolve the local workdir: the project's main checkout. WorktreePath
	// is empty for no-branch rows so devBranchAndDir collapses to the project
	// checkout (currentBranch == base by construction — StartCoding /
	// AdjustCoding / ContinueCoding all checkout base before claude spawns).
	proj, perr := r.projectSvc.Get(reqRow.ProjectID)
	if perr != nil || proj == nil {
		log.Printf("[push-base-shell] %s: project load failed (%v), skip", reqRow.ID, perr)
		return "", "", nil
	}
	dir := proj.LocalPath
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

	// ── Sanity: refuse to run if the project's main checkout isn't actually
	// on the base branch anymore (an AdjustCoding round-trip could leave the
	// row with NoBranch=true but the working dir detached). The branch
	// checkout at StartCoding / AdjustCoding / ContinueCoding normally handles
	// this; the check here is a belt-and-suspenders guard for re-dispatched
	// jobs after a restart.
	if cur := currentBranch(dir); cur != "" && cur != "HEAD" && cur != base {
		job.Append(store.LogLine{Type: "error", Content: "❌ 当前 HEAD 在分支 " + cur + ",cur != base=" + base + ",拒绝推送。请先切回 base 分支后重试。"})
		job.Finish(1, store.JobError)
		return
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

	// ── Step 3 (conditional): push base to origin. No PR step (no-branch
	// path always skips PR creation; gh/glab/tea reject head==base). When
	// autoPushRemote=false the user opted out of pushing — bail here so the
	// commit sits locally until they push manually.
	if !autoPushRemote {
		job.Append(store.LogLine{Type: "result", Content: "✅ 已本地提交至 base 分支 " + base + "（未推送 — 在 start-coding 弹窗里关掉了 auto_push_remote）"})
		job.Append(store.LogLine{Type: "done", Content: "✅ commit-only 流程完成"})
		job.Finish(0, store.JobDone)
		log.Printf("[push-base-shell] job %s finished for %s (autoPushRemote=false, commit-only)", job.ID, reqRow.ID)
		return
	}
	if _, perr := git("push", "-u", "origin", base); perr != nil {
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ 首次 push base 失败，尝试 pull --rebase 后重推: " + perr.Error()})
		if _, rerr := git("pull", "--rebase", "origin", base); rerr != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ git pull --rebase 失败: " + rerr.Error()})
			job.Finish(1, store.JobError)
			return
		}
		if out2, perr2 := git("push", "-u", "origin", base); perr2 != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ git push base 失败: " + out2 + " " + perr2.Error()})
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "message", Content: "⬆️ 已推送 base (rebase 后)"})
	} else {
		job.Append(store.LogLine{Type: "message", Content: "⬆️ 已推送 base → origin"})
	}

	job.Append(store.LogLine{Type: "result", Content: "✅ 已提交并推送至 base 分支 " + base + "\n\n(no-branch 模式:跳过 PR 创建,head==base 在 gh/glab/tea 会拒绝)"})
	job.Append(store.LogLine{Type: "done", Content: "✅ 推送 base 流程完成"})
	job.Finish(0, store.JobDone)
	log.Printf("[push-base-shell] job %s finished for %s", job.ID, reqRow.ID)
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