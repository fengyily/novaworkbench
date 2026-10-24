// Package prompt holds shared task-prompt fragments that are appended to the
// dynamic `-p` message sent to the claude CLI subprocess. Keeping the strings
// here (instead of inlined in each handler) gives us one place to evolve the
// wording and ensures every entry point that drives a claude child to run
// git gets the same instructions.
//
// The contents below are raw text (backtick-quoted Go raw strings) — they are
// concatenated into the user's -p prompt verbatim and never parsed as code.
package prompt

// GitCommitConvention is the mandatory tail block appended to every task
// prompt that may drive the claude child to run `git commit` / `git push` /
// `gh/glab/tea pr create`. It enforces two rules:
//   1. No AI attribution trailers (🤖 Generated with Claude Code /
//      Co-Authored-By / Co-authored-by) in commit messages or PR bodies.
//   2. No manual token / password typing in shell commands; the platform
//      credentials have already been arranged by the backend (GIT_ASKPASS
//      script in the child env, or token-injected origin URL on the
//      agent-server remote path). The delivery mechanism is intentionally
//      opaque to the model — it just runs plain `git push origin <branch>`
//      and lets git pick up creds from env / URL. If auth still fails,
//      stop and surface the error instead of improvising.
//
// Task prompts are rebuilt every run, so appending this constant immediately
// takes effect on existing DB rows — no role migration is needed.

// WorkingDirSafety is the "do not destroy the directory you were given" block
// appended to every claude child that runs with tool access inside a project
// checkout or worktree.
//
// # WHY THIS EXISTS
//
// On 2026-10-24 a push/PR sub-task lost its working directory mid-run (the
// directory was deleted out from under it by an unrelated process). Instead of
// stopping, the child tried to "repair" the situation: it ran
// `rm -rf /Users/f1/workspace/vivalink-sdk`, then four `git clone` attempts
// with 75s connection timeouts, then a 40-iteration `gh pr checks` polling
// loop — 11 minutes of tool calls that could only make things worse, because
// the PR had in fact already been created and merged.
//
// Three separate instructions follow from that:
//  1. The working directory is not the child's to delete, move or reset.
//  2. If it disappears, stop and report — do not improvise a repair.
//  3. Do not poll CI. Report the PR link and finish.
//
// This block is advisory (a prompt), not enforcement. The enforcement lives
// in sub_task_runner.go (destructive-command interception + missing-cwd
// detection); the prompt exists so a well-behaved model never needs it.
const WorkingDirSafety = `## 工作目录安全约束（强制）
- 当前工作目录是本次改动唯一的落盘位置。**严禁删除、移动、重命名、清空它，也严禁对其任何父目录做同样操作**。
- 禁止对工作目录执行：` + "`rm -rf`" + `、` + "`rm -fr`" + `、` + "`git clean -fdx`" + `、` + "`git worktree remove`" + `、` + "`git reset --hard`" + `、` + "`mv`" + ` 目录。
- 如果工作目录消失或不可读（例如报 ` + "`No such file or directory`" + ` / ` + "`无法读取当前工作目录`" + ` / ` + "`Working directory ... was deleted`" + `）：**立即停止并如实报告**。不要删除后重新 clone，不要用任何方式自行「修复」目录 —— 那只会破坏证据并可能误删其它仓库。
- 不要轮询等待 CI（例如 ` + "`gh pr checks`" + ` 循环、` + "`sleep`" + ` 后重试）。PR 创建成功即输出链接并结束；CI 状态由用户自己查看。`

const GitCommitConvention = `## Git 提交与推送规范（强制遵守）
- 所有 git 操作（提交 / 推送 / 创建 PR）都在当前工作目录内进行。系统已根据项目配置的「平台 Token」（设置 → 平台 Token）准备好本次运行的 git 认证凭据，直接 ` + "`git push origin <分支>`" + ` 即可完成认证。**切勿在命令中手写 token / 用户名 / 密码，也不要把任何凭据打印到输出**。
- 提交信息只描述本次改动本身，遵循项目既有 commit 风格；严禁添加任何 AI 署名或协作者尾注，包括但不限于 ` + "`🤖 Generated with Claude Code`" + `、` + "`Co-Authored-By: Claude <...>`" + `、` + "`Co-authored-by: ...`" + ` 等 trailer。创建 PR 时同理，正文不得包含上述署名。
- 若遇认证 / 权限失败，明确报告「凭据或权限问题」并停止，不要绕过、不要伪造提交者身份。`
