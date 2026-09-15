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
//      credentials are delivered via env (GIT_ASKPASS), not via the prompt
//      or the shell. If auth still fails, stop and surface the error.
//
// Task prompts are rebuilt every run, so appending this constant immediately
// takes effect on existing DB rows — no role migration is needed.
const GitCommitConvention = `## Git 提交与推送规范（强制遵守）
- 所有 git 操作（提交 / 推送 / 创建 PR）都在当前工作目录内进行；origin 已由平台预先注入访问凭据，直接 ` + "`git push origin <分支>`" + ` 即可完成认证。切勿在命令中手写 token / 用户名 / 密码，也不要把任何凭据打印到输出。
- 提交信息只描述本次改动本身，遵循项目既有 commit 风格；严禁添加任何 AI 署名或协作者尾注，包括但不限于 ` + "`🤖 Generated with Claude Code`" + `、` + "`Co-Authored-By: Claude <...>`" + `、` + "`Co-authored-by: ...`" + ` 等 trailer。创建 PR 时同理，正文不得包含上述署名。
- 若遇认证 / 权限失败，明确报告「凭据或权限问题」并停止，不要绕过、不要伪造提交者身份。`