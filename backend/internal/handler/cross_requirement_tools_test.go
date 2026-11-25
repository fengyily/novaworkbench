package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 跨项目作业的工具权限。
//
// 背景：跨项目作业过去在协议层把 Write / Edit / Bash / NotebookEdit 全禁了，
// 模型只能 Read / Glob / Grep —— 一份不看 git 历史、不看 blame、不看跨仓库
// diff 的方案。用户要求放开 Bash（对齐「项目」里的方案设计 / 开发权限），
// 但**不允许**跨项目阶段改代码（真正的开发在拆出来的子需求里做）。
//
// 所以这里钉住两件事：
//
//  1. 协议层：Bash 放开，Write / Edit / NotebookEdit 仍然禁用。
//  2. 兜底层：Bash 放开后，「不改仓库」这条保证就只剩 shell 这一个缺口，
//     破坏性命令守卫必须把「删除 / 重置参与仓库」这条路堵上。
//
// 守卫生效的目录 = 作业被授权读取的那些目录（opts.AddDirs）。授权与保护是
// 同一个决定，所以两者取自同一份清单。

// TestCrossJobKeepsWriteBlockedButOpensBash — 协议层的工具口径。
func TestCrossJobKeepsWriteBlockedButOpensBash(t *testing.T) {
	h, _, _ := newCrossHandlerTest(t)

	opts := h.crossStreamOpts(crossJobSpec{
		Prompt:  "写一份整体方案",
		WorkDir: t.TempDir(),
	})

	for _, tool := range []string{"Write", "Edit", "NotebookEdit"} {
		if !crossHasString(opts.DisallowedTools, tool) {
			t.Fatalf("opts.DisallowedTools = %v, want %s denied (the cross stage must not change code)",
				opts.DisallowedTools, tool)
		}
	}
	if crossHasString(opts.DisallowedTools, "Bash") {
		t.Fatalf("opts.DisallowedTools = %v, must not deny Bash", opts.DisallowedTools)
	}
}

// TestCrossDisallowedToolsCoversMutatingGit — 会改动仓库的 git 子命令必须在
// CLI 层被拒掉，而只读命令不能被误伤。
//
// 两条形态都要在：直接调用（`git checkout …`）与 `-C <路径>` 调用
// （`git -C /repo checkout …`）—— 作业的工作目录不是仓库，模型要指定仓库
// 就得用 `-C`，或者先 `cd` 过去（CLI 的 shell 保留 cwd）。
func TestCrossDisallowedToolsCoversMutatingGit(t *testing.T) {
	list := crossDisallowedTools()

	mutating := []string{
		"Bash(git checkout *)", "Bash(git -C * checkout *)",
		"Bash(git switch *)", "Bash(git -C * switch *)",
		"Bash(git restore *)", "Bash(git -C * restore *)",
		"Bash(git stash *)", "Bash(git -C * stash *)",
		"Bash(git commit *)", "Bash(git -C * commit *)",
		"Bash(git push *)", "Bash(git -C * push *)",
		"Bash(git reset *)", "Bash(git -C * reset *)",
		"Bash(git clean *)", "Bash(git -C * clean *)",
		"Bash(git merge *)", "Bash(git -C * merge *)",
		"Bash(git rebase *)", "Bash(git -C * rebase *)",
		"Bash(git worktree remove *)", "Bash(git -C * worktree remove *)",
	}
	for _, want := range mutating {
		if !crossHasString(list, want) {
			t.Fatalf("denylist is missing %q", want)
		}
	}

	// 只读命令不能被误伤 —— 挡掉它们等于把「开放 Bash」这件事抵消掉。
	readOnly := []string{
		"Bash(git log *)", "Bash(git show *)", "Bash(git diff *)",
		"Bash(git blame *)", "Bash(git status *)", "Bash(git ls-files *)",
		"Bash(git rev-parse *)", "Bash(git describe *)", "Bash(git worktree list *)",
	}
	for _, unwanted := range readOnly {
		if crossHasString(list, unwanted) {
			t.Fatalf("denylist must not contain the read-only pattern %q", unwanted)
		}
	}

	// 形态说明：CLI 的 --disallowedTools 把整个列表拼成一个字符串，模式里的
	// 空格属于模式本身 —— 工具名与模式混排时两者都要生效。
	t.Log("joined denylist:", strings.Join(list, " "))
}

// TestCrossJobGuardBlocksDestroyingAGrantedDirectory — 守卫必须挡住
// 「删掉参与仓库」这类命令。
//
// 用例覆盖三种形态：
//
//   - 直接点名：`rm -rf <仓库>`
//   - cwd 隐式：`git -C <仓库> reset --hard` / `git clean -fdx`
//   - 变量展开：`rm -rf "$TREE"`（解析不出来 ⇒ fail closed）
func TestCrossJobGuardBlocksDestroyingAGrantedDirectory(t *testing.T) {
	repo := t.TempDir()
	sink := crossJobSink{protected: []string{repo}}

	cases := []struct {
		name string
		cmd  string
	}{
		{"rm -rf the repo", "rm -rf " + repo},
		{"rm -rf the repo's parent", "rm -rf " + filepath.Dir(repo)},
		{"git reset --hard in the repo", "git -C " + repo + " reset --hard"},
		{"git clean -fdx in the repo", "git -C " + repo + " clean -fdx"},
		{"unresolvable variable", `rm -rf "$TREE"`},
		{"command substitution", "echo $(rm -rf " + repo + ")"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := sink.GuardToolUse("Bash", map[string]interface{}{"command": tc.cmd})
			if reason == "" {
				t.Fatalf("guard allowed a destructive command: %s", tc.cmd)
			}
		})
	}
}

// TestCrossJobGuardAllowsInspectionAndCleanup — 守卫不能变成「什么都不许做」。
//
// 只读探查（这正是放开 Bash 的目的）与常规清理必须照常通过，否则模型连
// git log 都跑不了，功能反而更受限。
func TestCrossJobGuardAllowsInspectionAndCleanup(t *testing.T) {
	repo := t.TempDir()
	sink := crossJobSink{protected: []string{repo}}

	allowed := []string{
		"git log --oneline -20",
		"git show HEAD:README.md",
		"git diff origin/main...HEAD",
		"git blame -L 10,20 src/app.go",
		"git -C " + repo + " log --oneline",
		"grep -rn 'func Handler' " + repo,
		"ls -la " + repo,
		"rm -rf build",
		"rm -rf " + filepath.Join(repo, "build"),
		"cat " + filepath.Join(repo, "go.mod"),
	}
	for _, cmd := range allowed {
		if reason := sink.GuardToolUse("Bash", map[string]interface{}{"command": cmd}); reason != "" {
			t.Fatalf("guard blocked a harmless command %q: %s", cmd, reason)
		}
	}
}

// TestCrossJobGuardIgnoresNonBashTools — 守卫只管 Bash。
func TestCrossJobGuardIgnoresNonBashTools(t *testing.T) {
	repo := t.TempDir()
	sink := crossJobSink{protected: []string{repo}}

	if reason := sink.GuardToolUse("Read", map[string]interface{}{"file_path": repo}); reason != "" {
		t.Fatalf("guard blocked a Read: %s", reason)
	}
	if reason := sink.GuardToolUse("Bash", nil); reason != "" {
		t.Fatalf("guard blocked a Bash call without input: %s", reason)
	}
}

// TestCrossJobSinkHealthCheck — 作业目录被删掉时要中止，而不是继续跑。
func TestCrossJobSinkHealthCheck(t *testing.T) {
	workDir := t.TempDir()
	sink := crossJobSink{workDir: workDir}
	if reason := sink.CheckStreamHealth(); reason != "" {
		t.Fatalf("healthy work dir reported a problem: %s", reason)
	}
	if err := os.RemoveAll(workDir); err != nil {
		t.Fatalf("remove work dir: %v", err)
	}
	reason := sink.CheckStreamHealth()
	if reason == "" {
		t.Fatal("a deleted work dir must abort the run")
	}
	if !strings.Contains(reason, workDir) {
		t.Fatalf("reason %q should name the missing work dir", reason)
	}
}

// TestCrossJobGuardProtectsEveryGrantedDirectory — 被授权的每个目录都要保护到。
//
// 参与仓库是多家的：保护住第一家却漏掉第二家，等于没保护。
func TestCrossJobGuardProtectsEveryGrantedDirectory(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	sink := crossJobSink{protected: []string{first, second}}

	for _, repo := range []string{first, second} {
		if reason := sink.GuardToolUse("Bash", map[string]interface{}{"command": "rm -rf " + repo}); reason == "" {
			t.Fatalf("guard allowed deleting granted directory %s", repo)
		}
	}
}

// TestCrossEmptyOutputMessageSurfacesTheRealReason — 作业失败时要说真话。
//
// 守卫拦下破坏性命令后，作业没有产出；这时报「未产出整体方案，请重试」等于
// 把真因藏起来 —— 用户重跑一遍会撞上同一堵墙，日志里却什么都没说。
func TestCrossEmptyOutputMessageSurfacesTheRealReason(t *testing.T) {
	guardMsg := "❌ 已阻止破坏性命令（会删除/重置只读目录 /repo）：rm -rf /repo。禁止删除工作目录或其父目录。"

	if got := crossEmptyOutputMessage(claudeStreamOutcome{errMsg: guardMsg}, "未产出整体方案，请重试"); got != guardMsg {
		t.Fatalf("got %q, want the guard's reason %q", got, guardMsg)
	}
	if got := crossEmptyOutputMessage(claudeStreamOutcome{}, "未产出整体方案，请重试"); got != "未产出整体方案，请重试" {
		t.Fatalf("got %q, want the fallback message", got)
	}
}
