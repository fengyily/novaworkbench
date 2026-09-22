// Package handler — shell-driven push + PR for the LOCAL auto-push path.
//
// push_pr_shell.go is the non-LLM counterpart of dispatchPushPRSubTask: it
// runs the "提交 → 合并主分支 → 推送 → 创建 PR" flow as a deterministic shell
// sequence (git + gh) inside a JobStore job, so the SubTaskPanel card + SSE
// stream + job_logs durability all keep working — but without burning a full
// Claude turn (req_7c04316f83837af6's push sub-task spent ~59K input + 348K
// cache tokens + ~40s just to run `git status && git merge && git push &&
// gh pr create`, and even ran git in the wrong dir before correcting).
//
// Only the happy path is shell-driven. The one step that genuinely needs LLM
// judgment — merge conflict resolution — falls back to dispatchPushPRSubTask
// (the existing LLM sub-task), which resolves the conflict and writes the PR
// body. So: clean merge → fast shell job; conflicting merge → LLM takes over.
//
// Routing: autoPushPR (wizard_common.go) calls runPushPRShellJob for LOCAL
// requirements (codeLivesOnAgent == false). Remote (origin-transport) requires
// the push to run on the agent host, which a local shell can't reach — those
// still go through dispatchPushPRSubTask (the LLM child runs on the agent host).
//
// The manual 「推送并发起 PR」 button (MergeHandler.Push) ALSO keeps using
// dispatchPushPRSubTask, so a user who wants the LLM's polished PR body or its
// conflict resolution can always click that — the shell path only serves the
// automated happy path.
package handler

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/store"
)

// pushShellStepTimeout caps each git/gh invocation so a hung network call
// (push to a slow remote, gh pr create waiting on OAuth) can't pin the job
// forever. 10s is enough for fetch/push on a warm remote; gh pr create can
// take longer but the cap keeps the failure bounded and visible.
const pushShellStepTimeout = 10 * time.Minute

// runPushPRShellJob dispatches a shell-driven push+PR job for a LOCAL
// requirement. It mirrors dispatchPushPRSubTask's shape (idempotency guard →
// NewPendingSubTask → run in a goroutine → job_logs persistence) so the
// SubTaskPanel card is indistinguishable from an LLM push child, but the
// goroutine body runs `git`/`gh` directly instead of a Claude subprocess.
//
// dev/base/remote/platformType/commitMessage are the same params
// dispatchPushPRSubTask resolves (passed in by autoPushPR). pushModel/
// pushCfgID are only used by the conflict-fallback LLM dispatch, not the
// shell steps themselves (git doesn't talk to an LLM). commitLang feeds the
// (LLM-only) fallback path.
//
// Returns (jobID, subTaskID, error) like dispatchPushPRSubTask so autoPushPR
// can log symmetrically.
func (h *WizardHandler) runPushPRShellJob(reqRow *model.Requirement, dev, base, remote, platformType, commitMessage, pushModel, pushCfgID, commitLang string) (jobID, subTaskID string, err error) {
	if h.subTaskRunner == nil || h.subTaskSvc == nil {
		log.Printf("[push-pr-shell] %s: sub-task runner not wired, skip", reqRow.ID)
		return "", "", nil
	}

	// Idempotency guard — same window as dispatchPushPRSubTask so a manual
	// button click racing an auto dispatch collapses onto one job.
	const lookbackSec = 90
	if existing, ferr := h.subTaskSvc.FindRecentPushForReq(reqRow.ID, lookbackSec); ferr != nil {
		log.Printf("[push-pr-shell] %s: idempotency lookup failed: %v (continuing)", reqRow.ID, ferr)
	} else if existing != nil {
		log.Printf("[push-pr-shell] %s: idempotent hit existing sub_task=%s job_id=%s status=%s", reqRow.ID, existing.ID, existing.JobID, existing.Status)
		return existing.JobID, existing.ID, nil
	}

	// Resolve the local workdir: the worktree the coding agent actually
	// committed into. devBranchAndDir returns (dev, dir) using the
	// requirement's persisted worktree_path when it exists, else the project
	// checkout.
	proj, perr := h.projectSvc.Get(reqRow.ProjectID)
	if perr != nil || proj == nil {
		log.Printf("[push-pr-shell] %s: project load failed (%v), skip", reqRow.ID, perr)
		return "", "", nil
	}
	dir := proj.LocalPath
	_, dir = devBranchAndDir(reqRow, dir) // dev already resolved by autoPushPR; only need dir
	if dir == "" {
		log.Printf("[push-pr-shell] %s: no local workdir, skip", reqRow.ID)
		return "", "", nil
	}

	shortDesc := "本地 shell 推送并创建 PR: " + dev
	st, job, _, nerr := h.subTaskRunner.NewPendingSubTask(reqRow.ID, "推送并创建 PR", shortDesc, "", "", "", "", "")
	if nerr != nil {
		return "", "", nerr
	}
	if uerr := h.subTaskSvc.UpdateSource(st.ID, model.SubTaskSourcePushPR); uerr != nil {
		log.Printf("[push-pr-shell] %s: failed to stamp source=push_pr on %s: %v", reqRow.ID, st.ID, uerr)
	}
	log.Printf("[push-pr-shell] %s: dispatched shell sub_task=%s job_id=%s dir=%s dev=%s base=%s", reqRow.ID, st.ID, job.ID, dir, dev, base)

	go h.execPushPRShell(reqRow, st, job, dir, proj.LocalPath, dev, base, remote, platformType, commitMessage, pushModel, pushCfgID, commitLang)
	return job.ID, st.ID, nil
}

// execPushPRShell is the goroutine body. It owns the job's lifecycle: every
// exit path calls job.Finish and the deferred Save/panic-recovery mirrors
// SubTaskRunner.Run's scaffolding (copied, not shared, to avoid refactoring
// the working Run path).
//
// sub-task row status: NewPendingSubTask inserts the row in 'pending'; this
// function flips it to 'running' once it has won its admission gate, then to
// 'done' | 'error' on exit. Without those flips the SubTaskPanel chip would
// read "排队中 · 等待项目空闲" forever (the LLM path's equivalent is
// Run → MarkRunning → finishSubTask → subTaskSvc.Finish). The artifact body
// is the full JobStore log rendered as Markdown so the user can reopen it
// from the SubTaskPanel card even after the in-memory ring buffer evicts.
func (h *WizardHandler) execPushPRShell(reqRow *model.Requirement, st *model.SubTask, job *store.Job, dir, projectPath, dev, base, remote, platformType, commitMessage, pushModel, pushCfgID, commitLang string) {
	var runStartTime time.Time
	defer func() {
		lines, status, exitCode := job.Snapshot()
		log.Printf("[push-pr-shell] defer-Save job_id=%s req_id=%s sub_task_id=%s status=%s exit=%d lines=%d", job.ID, st.RequirementID, st.ID, status, exitCode, len(lines))
		if perr := h.jobLogSvc.Save(job.ID, st.RequirementID, string(status), exitCode, job.StartedAt, job.FinishedAt, lines, ""); perr != nil {
			log.Printf("[push-pr-shell] failed to persist job log %s: %v", job.ID, perr)
		}
		// Mirror the LLM path: persist a final sub_tasks row state so the
		// SubTaskPanel chip leaves "排队中" and the artifact lands in the DB.
		// runStartTime stays zero when the job never made it past the
		// gate/stopped-check (those exits call job.Finish(0, JobDone)
		// directly); zero is the documented "skip duration" sentinel per
		// SubTaskService.Finish, so duration_seconds stays at the column
		// default — matching the LLM path's behavior for stopped-while-queued
		// children.
		if h.subTaskSvc != nil {
			finalStatus := model.SubTaskStatusDone
			if status == store.JobError {
				finalStatus = model.SubTaskStatusError
			}
			artifactBody := renderShellJobLog(lines)
			artifact := buildSubTaskArtifact(st, "", artifactBody, time.Now())
			if ferr := h.subTaskSvc.Finish(st.ID, finalStatus, artifact, "", model.SubTaskTokens{}, 0, runStartTime); ferr != nil {
				log.Printf("[push-pr-shell] failed to finish sub_task %s: %v", st.ID, ferr)
			}
		}
		// Bump the parent requirement's updated_at so the auto-push shows up
		// in RequirementsList (mirrors autoPushPR's Touch).
		if terr := h.reqSvc.Touch(st.RequirementID); terr != nil {
			log.Printf("[push-pr-shell] touch requirement %s: %v", st.RequirementID, terr)
		}
	}()
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[push-pr-shell] panic recovered in job %s (sub_task %s): %v", job.ID, st.ID, rec)
			job.Append(store.LogLine{Type: "error", Content: "❌ 内部异常，任务已中止: " + fmt.Sprint(rec)})
		}
		if _, status, _ := job.Snapshot(); status == store.JobRunning {
			job.Finish(1, store.JobError)
		}
	}()

	// Honor the same admission gates as an LLM child so parallel requirements
	// don't race git index locks on the same checkout.
	release := h.subTaskRunner.AcquireGates(reqRow.ProjectID, job)
	defer release()

	// A queued shell job can be stopped while waiting (StopSubTask flips the
	// row). Don't spawn git then.
	if cur, gerr := h.subTaskSvc.Get(st.ID); gerr == nil && cur != nil && cur.Status == model.SubTaskStatusStopped {
		job.Append(store.LogLine{Type: "message", Content: "⏹ 排队期间已被停止，未启动执行"})
		job.Finish(0, store.JobDone)
		return
	}
	// Flip the row to "running" — same gate the LLM path uses
	// (sub_task_runner.go: MarkRunning). Without this the SubTaskPanel chip
	// renders "排队中 · 等待项目空闲" for the entire shell lifetime, even
	// after the job has finished. runStartTime feeds sub_tasks.duration_seconds.
	runStartTime, mErr := h.subTaskSvc.MarkRunning(st.ID)
	if mErr != nil {
		log.Printf("[push-pr-shell] failed to mark running for %s: %v", st.ID, mErr)
	}
	job.Append(store.LogLine{Type: "phase", Content: "🚀 本地 shell 推送流程启动（dev=" + dev + ", base=" + base + "）"})

	// dev == base 安全闸门：开发分支与目标分支相同时,推送会把未隔离的
	// 改动直推 main,且 gh pr create 必然失败 (head == base)。拒绝执行
	// 而不是产生一个 "✅ 完成" 的假成功 (req_82e061807ef0372f)。
	if dev == base {
		job.Append(store.LogLine{Type: "error", Content: "❌ 开发分支与目标分支相同（dev=" + dev + ", base=" + base + "），拒绝直推 base 分支。请检查 worktree 是否正常创建。"})
		job.Finish(1, store.JobError)
		return
	}

	// Credential + GPG env (shared with the coding path): GIT_AUTHOR_*,
	// HTTPS askpass, and GPG worktree-config writes so `git commit`/`git
	// merge` in dir sign under the project's identity. projectPath is the
	// MAIN checkout — worktrees share its .git, and buildGPGProvisionScript
	// needs the main repo path for extensions.worktreeConfig.
	credEnv, credCleanup := h.assembleGitCredEnv(reqRow, dir, projectPath, job)
	if credCleanup != nil {
		defer credCleanup()
	}

	// git runner that injects the credential env.
	git := func(args ...string) (string, error) {
		return gitRunWithTimeout(dir, pushShellStepTimeout, credEnv, args...)
	}

	// ── Step 1: commit uncommitted changes (idempotent — no-op if the
	// coding agent already committed+pushed, which is the common case).
	if dirty, _ := git("status", "--porcelain"); strings.TrimSpace(dirty) != "" {
		job.Append(store.LogLine{Type: "message", Content: "📝 工作区有未提交改动，执行 git add -A && git commit"})
		if _, gerr := git("add", "-A"); gerr != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ git add 失败: " + gerr.Error()})
			job.Finish(1, store.JobError)
			return
		}
		msg := strings.TrimSpace(commitMessage)
		if msg == "" {
			// Fall back to the requirement title, not the branch name.
			// Using dev (the branch name) produced meaningless commit
			// messages like "main" when no worktree was created
			// (req_82e061807ef0372f: commit -m "main").
			if title := strings.TrimSpace(reqRow.Title); title != "" {
				msg = title
			} else {
				msg = reqRow.ID
			}
		}
		if _, cerr := git("commit", "-m", msg, "--no-verify"); cerr != nil {
			// commit can fail benignly when add raced with another committer;
			// surface but continue to merge/push (the existing commits still
			// need pushing).
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

	// ── Step 3: merge base into dev. Conflict → abort + hand off to the LLM
	// sub-task (which resolves conflicts and writes the PR body).
	if _, behind := aheadBehindInfo(dir, "origin/"+base, dev); behind == 0 {
		job.Append(store.LogLine{Type: "message", Content: "✅ dev 已是 base 的最新，无需合并"})
	} else if out, merr := git("merge", "origin/"+base, "--no-edit"); merr != nil {
		// Conflict (or merge failure). Abort to leave the worktree clean,
		// then dispatch the LLM sub-task to handle it.
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 合并 " + base + " 失败/冲突: " + out + " — 回退到 LLM 子任务解决"})
		if _, aerr := git("merge", "--abort"); aerr != nil {
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ git merge --abort: " + aerr.Error()})
		}
		job.Append(store.LogLine{Type: "message", Content: "🔄 已转交 LLM 子任务解决冲突并创建 PR"})
		job.Finish(0, store.JobDone) // shell job's job is "delegate", not error
		// The shell job is now terminal, so the idempotency guard in
		// dispatchPushPRSubTask won't block this new dispatch.
		_, _, derr := dispatchPushPRSubTask(h.subTaskRunner, reqRow, dev, base, remote, platformType, commitMessage, pushModel, pushCfgID, commitLang, "auto")
		if derr != nil {
			log.Printf("[push-pr-shell] %s: conflict-fallback dispatch failed: %v", reqRow.ID, derr)
		}
		return
	} else {
		job.Append(store.LogLine{Type: "message", Content: "🔀 已合并 origin/" + base + " 到 " + dev})
	}

	// ── Step 4: push dev to origin.
	if _, perr := git("push", "-u", "origin", dev); perr != nil {
		// Retry: pull --rebase then push (handles a remote ahead of local).
		job.Append(store.LogLine{Type: "message", Content: "ℹ️ 首次 push 失败，尝试 pull --rebase 后重推: " + perr.Error()})
		if _, rerr := git("pull", "--rebase", "origin", dev); rerr != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ git pull --rebase 失败: " + rerr.Error()})
			job.Finish(1, store.JobError)
			return
		}
		if out2, perr2 := git("push", "-u", "origin", dev); perr2 != nil {
			job.Append(store.LogLine{Type: "error", Content: "❌ git push 失败: " + out2 + " " + perr2.Error()})
			job.Finish(1, store.JobError)
			return
		}
		job.Append(store.LogLine{Type: "message", Content: "⬆️ 已推送 " + dev + " (rebase 后)"})
	} else {
		job.Append(store.LogLine{Type: "message", Content: "⬆️ 已推送 " + dev + " → origin"})
	}

	// ── Step 5: create PR (gh / glab / tea) or surface a compare link.
	prURL, prCreated := h.createPRShell(job, git, dir, dev, base, platformType, remote, reqRow)
	if prCreated {
		job.Append(store.LogLine{Type: "result", Content: "✅ 推送并创建 PR 完成\n\nPR: " + prURL})
	} else if prURL != "" {
		job.Append(store.LogLine{Type: "result", Content: "⚠️ 推送完成，但 PR 创建失败。请通过 compare 链接手动创建: " + prURL})
	} else {
		job.Append(store.LogLine{Type: "result", Content: "⚠️ 推送完成，但未能自动创建 PR（无平台 CLI / 无 remote）。请手动创建 PR。"})
	}
	job.Append(store.LogLine{Type: "done", Content: "✅ 推送流程完成"})
	job.Finish(0, store.JobDone)
	log.Printf("[push-pr-shell] job %s finished for %s", job.ID, reqRow.ID)
}

// createPRShell runs the platform-appropriate PR-creation CLI, falling back to
// a compare URL when the CLI is unavailable. Returns the PR URL (or compare
// URL when no PR was created) AND a bool reporting whether a real PR was
// created. When prCreated=false, the URL is a best-effort compare link the
// user can open manually — the caller must NOT report it as "✅ PR 创建完成"
// (req_82e061807ef0372f: gh pr create failed on head==base but the shell still
// reported "✅ 推送并创建 PR 完成 / PR: ...compare/main...main").
func (h *WizardHandler) createPRShell(job *store.Job, git func(...string) (string, error), dir, dev, base, platformType, remote string, reqRow *model.Requirement) (string, bool) {
	title := strings.TrimSpace(reqRow.Title)
	if title == "" {
		title = dev
	}
	// PR body: the commits on dev not on base, plus a diff stat. Deterministic
	// and good enough for the auto-push happy path; the manual push button
	// still routes through the LLM for a polished body.
	logOut, _ := git("log", "origin/"+base+".."+dev, "--oneline", "--no-decorate")
	diffOut, _ := git("diff", "--stat", "origin/"+base+"..."+dev)
	var body strings.Builder
	body.WriteString("## 改动概述\n\n")
	body.WriteString("由 NovaWorkbench auto-push 自动创建（基于需求 ")
	body.WriteString(reqRow.ID)
	body.WriteString("）。\n\n## 提交\n```\n")
	body.WriteString(logOut)
	body.WriteString("\n```\n\n## 变更统计\n```\n")
	body.WriteString(diffOut)
	body.WriteString("\n```")
	bodyStr := body.String()

	run := func(name string, args ...string) (string, error) {
		c := exec.Command(name, args...)
		c.Dir = dir
		var stdout, stderr strings.Builder
		c.Stdout = &stdout
		c.Stderr = &stderr
		if err := c.Run(); err != nil {
			return stdout.String(), fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
		}
		return strings.TrimSpace(stdout.String()), nil
	}

	switch platformType {
	case "github":
		if out, err := run("gh", "pr", "create", "--base", base, "--head", dev, "--title", title, "--body", bodyStr); err != nil {
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ gh pr create 不可用/失败: " + err.Error()})
			return compareURL(remote, base, dev), false
		} else {
			// gh prints the PR URL to stdout.
			return extractURL(out), true
		}
	case "gitlab":
		if out, err := run("glab", "mr", "create", "--target-branch", base, "--source-branch", dev, "--title", title, "--description", bodyStr); err != nil {
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ glab mr create 不可用/失败: " + err.Error()})
			return compareURL(remote, base, dev), false
		} else {
			return extractURL(out), true
		}
	case "gitea":
		if out, err := run("tea", "pr", "create", "--base", base, "--head", dev, "--title", title, "--description", bodyStr); err != nil {
			job.Append(store.LogLine{Type: "message", Content: "ℹ️ tea pr create 不可用/失败: " + err.Error()})
			return compareURL(remote, base, dev), false
		} else {
			return extractURL(out), true
		}
	default:
		return compareURL(remote, base, dev), false
	}
}

// renderShellJobLog renders the JobStore's full log as the sub_task.artifact
// Markdown body. Mirrors the "full transcript" affordance the LLM child path
// already gives users — open the SubTaskPanel card after the run and you
// see every line the shell produced, phase / message / error / result all
// preserved. Empty input falls through to a single "（无日志）" so the
// Markdown body is never blank.
func renderShellJobLog(lines []store.LogLine) string {
	if len(lines) == 0 {
		return "（无日志）"
	}
	var b strings.Builder
	for _, ln := range lines {
		typ := strings.TrimSpace(ln.Type)
		content := strings.TrimRight(ln.Content, "\n")
		switch typ {
		case "phase":
			b.WriteString("\n## ")
			b.WriteString(content)
			b.WriteString("\n")
		case "error":
			b.WriteString("\n- ")
			b.WriteString(content)
		case "tool_call", "message", "tool_result":
			b.WriteString("\n- ")
			b.WriteString(content)
		case "result":
			b.WriteString("\n### 结果\n\n")
			b.WriteString(content)
			b.WriteString("\n")
		case "done":
			b.WriteString("\n**")
			b.WriteString(content)
			b.WriteString("**\n")
		case "conflict":
			b.WriteString("\n> ⚠️ ")
			b.WriteString(content)
			b.WriteString("\n")
		default:
			b.WriteString("\n")
			b.WriteString(content)
		}
	}
	return strings.TrimSpace(b.String())
}

// aheadBehindInfo is a thin wrapper over `git rev-list --left-right --count`
// returning (ahead, behind) — behind==0 means dev has everything base has,
// so the merge is a no-op. Returns 0,0 on error so the caller falls through
// to attempting the merge (which then reports the real error).
func aheadBehindInfo(dir, target, dev string) (ahead, behind int) {
	out, err := gitRun(dir, "rev-list", "--left-right", "--count", target+"..."+dev)
	if err != nil {
		return 0, 0
	}
	parts := strings.Fields(out)
	if len(parts) >= 1 {
		fmt.Sscanf(parts[0], "%d", &behind)
	}
	if len(parts) >= 2 {
		fmt.Sscanf(parts[1], "%d", &ahead)
	}
	return ahead, behind
}

// compareURL builds a web compare URL for manual PR creation. Best-effort:
// rewrites github/gitlab/gitea git URLs into their compare paths. Returns the
// remote verbatim when the host isn't recognized.
func compareURL(remote, base, dev string) string {
	// Strip a trailing .git and the proto, then derive host/owner/repo.
	u := remote
	u = strings.TrimSuffix(u, ".git")
	for _, proto := range []string{"https://", "http://", "git@", "ssh://"} {
		u = strings.TrimPrefix(u, proto)
	}
	// git@github.com:owner/repo → github.com/owner/repo
	u = strings.Replace(u, ":", "/", 1)
	// Heuristic: github / gitlab / gitea all use /owner/repo/compare/base...dev
	if strings.Contains(u, "github.com") || strings.Contains(u, "gitlab") || strings.Contains(u, "gitea") {
		return u + "/compare/" + base + "..." + dev
	}
	return remote + " (compare " + base + "..." + dev + ")"
}

// extractURL pulls the first http(s) URL out of a CLI's stdout (gh/glab/tea
// print the created PR/MR URL). Falls back to the full trimmed output.
func extractURL(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return strings.TrimSpace(s)
}
