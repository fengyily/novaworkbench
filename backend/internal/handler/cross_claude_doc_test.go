package handler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/util"
)

// 跨项目作业的「项目约定」必须真正进入 CLI 收到的提示词。
//
// 这是 P1-1 的回归测试：约定块曾经是在 llm.StreamOpts 构造**之后**才拼到
// 提示词字符串上的 —— StreamOpts.Prompt 是值拷贝，那次拼接对已经构造好的
// opts 没有任何影响，约定就那样留在了本地变量里，模型永远看不到。
//
// 所以断言的对象是 **opts.Prompt**（CLI 真正收到的那份），而不是中间变量。

// seedRepoWithClaudeDoc 建一个带 CLAUDE.md 的仓库目录，并插入 project 行。
//
// 返回 project id 与仓库路径。
func seedRepoWithClaudeDoc(t *testing.T, d *db.DB, name, doc string) (string, string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if doc != "" {
		if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte(doc), 0o644); err != nil {
			t.Fatalf("write CLAUDE.md: %v", err)
		}
	}
	id := util.NewID("proj")
	now := time.Now()
	if _, err := d.Exec(
		`INSERT INTO projects (id,name,local_path,status,added_at,updated_at) VALUES (?,?,?, 'active', ?, ?)`,
		id, name, repo, now, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return id, repo
}

// TestCrossJobPromptCarriesProjectConventions — 仓库自带的 CLAUDE.md 必须出现在
// CLI 收到的提示词里。
func TestCrossJobPromptCarriesProjectConventions(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	repoID, repoPath := seedRepoWithClaudeDoc(t, d, "web-app", "仓库约定：所有组件必须写单测。")

	x, err := h.svc.Create(model.CreateCrossRequirementReq{
		Title:      "跨仓库需求",
		ProjectIDs: []string{repoID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 约定被收集到，并且带着仓库归属。
	docs := h.crossConventions(x.ID, nil)
	if len(docs) != 1 {
		t.Fatalf("conventions = %d, want 1", len(docs))
	}
	if !strings.Contains(docs[0].Content, "所有组件必须写单测") {
		t.Fatalf("convention content = %q", docs[0].Content)
	}
	if docs[0].ProjectName != "web-app" {
		t.Fatalf("convention project name = %q, want web-app", docs[0].ProjectName)
	}
	if docs[0].Managed {
		t.Fatal("convention reported as managed although the project has no managed copy")
	}

	// 提示词里有约定 —— 用**作业自己的**组装入口（crossDesignPrompt /
	// crossSplitPrompt / crossPhasePrompt），这样「作业忘了把约定带进提示词」
	// 会被这条断言抓住。
	projects, err := h.crossProjectInputs(x.ID, nil)
	if err != nil {
		t.Fatalf("project inputs: %v", err)
	}
	xrow, err := h.svc.Get(x.ID)
	if err != nil {
		t.Fatalf("get cross requirement: %v", err)
	}
	designPrompt := h.crossDesignPrompt(xrow, projects, "", nil, nil)
	if !strings.Contains(designPrompt, "所有组件必须写单测") {
		t.Fatalf("design prompt lost the convention:\n%s", designPrompt)
	}
	splitPrompt := h.crossSplitPrompt(xrow, "## 整体方案\n\n方案正文\n", projects, nil)
	if !strings.Contains(splitPrompt, "所有组件必须写单测") {
		t.Fatalf("split prompt lost the convention:\n%s", splitPrompt)
	}
	members := []prompt.CrossPhaseMember{{
		RequirementID: "req_1",
		Title:         "订单服务",
		ProjectID:     repoID,
		ProjectName:   "web-app",
		ProjectPath:   repoPath,
		Status:        "done",
	}}
	phase := &model.CrossRequirementPhase{Name: "阶段一", Description: "接口打通"}
	phasePrompt := h.crossPhasePrompt(xrow, phase, members)
	if !strings.Contains(phasePrompt, "所有组件必须写单测") {
		t.Fatalf("phase prompt lost the convention:\n%s", phasePrompt)
	}
	promptText := designPrompt

	// **CLI 真正收到的那份**（opts.Prompt）里也有约定 —— 这一条才是 P1-1 的
	// 回归断言：约定曾经在 opts 构造之后才被拼上去，于是从未到达 CLI。
	// 作业读到的是**工作区**里的隔离 worktree（降级时是共享检出）。
	// 本测试的仓库不是 git 仓库，所以基线锁不上，读取路径退回共享检出 ——
	// 这正是降级路径要保证的事。
	ws, err := h.buildCrossWorkspace(context.Background(), x.ID, nil)
	if err != nil {
		t.Fatalf("build workspace: %v", err)
	}
	opts := h.crossStreamOpts(crossJobSpec{
		Prompt:    promptText,
		WorkDir:   t.TempDir(),
		Workspace: ws,
	})
	if !strings.Contains(opts.Prompt, "所有组件必须写单测") {
		t.Fatalf("opts.Prompt lost the convention:\n%s", opts.Prompt)
	}
	// 作业是只读的，并且拿到了参与仓库的读权限。
	for _, tool := range []string{"Write", "Edit", "Bash"} {
		if !crossHasString(opts.DisallowedTools, tool) {
			t.Fatalf("opts.DisallowedTools = %v, want %s", opts.DisallowedTools, tool)
		}
	}
	if !crossHasString(opts.AddDirs, repoPath) {
		t.Fatalf("opts.AddDirs = %v, want the repository %s", opts.AddDirs, repoPath)
	}
}

// TestCrossJobPromptPrefersManagedConvention — 受管副本优先于仓库自带的副本。
func TestCrossJobPromptPrefersManagedConvention(t *testing.T) {
	h, d, projSvc := newCrossHandlerTest(t)
	repoID, _ := seedRepoWithClaudeDoc(t, d, "api-svc", "仓库自带约定：旧规矩。")

	if _, err := projSvc.UpsertClaudeDoc(repoID, "受管约定：新规矩。", "manual", false); err != nil {
		t.Fatalf("upsert managed claude doc: %v", err)
	}

	x, err := h.svc.Create(model.CreateCrossRequirementReq{
		Title:      "受管副本优先",
		ProjectIDs: []string{repoID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	docs := h.crossConventions(x.ID, nil)
	if len(docs) != 1 {
		t.Fatalf("conventions = %d, want 1", len(docs))
	}
	if !docs[0].Managed {
		t.Fatal("convention not reported as managed")
	}
	if !strings.Contains(docs[0].Content, "受管约定") {
		t.Fatalf("convention content = %q, want the managed copy", docs[0].Content)
	}
	if strings.Contains(docs[0].Content, "旧规矩") {
		t.Fatalf("the repository's own copy leaked into the convention: %q", docs[0].Content)
	}
	// 仓库目录里另有一份 —— 提示词要说明「以受管副本为准」。
	if !docs[0].DiskExists {
		t.Fatal("DiskExists = false, want true (the checkout ships its own CLAUDE.md)")
	}
}

// TestCrossJobPromptWithoutConventions — 没有约定的仓库不出一节，空提示词
// 不该被撑大。
func TestCrossJobPromptWithoutConventions(t *testing.T) {
	h, d, _ := newCrossHandlerTest(t)
	repoID, _ := seedRepoWithClaudeDoc(t, d, "mobile", "")

	x, err := h.svc.Create(model.CreateCrossRequirementReq{
		Title:      "没有约定的仓库",
		ProjectIDs: []string{repoID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	docs := h.crossConventions(x.ID, nil)
	if len(docs) != 0 {
		t.Fatalf("conventions = %d, want 0", len(docs))
	}
	if section := prompt.BuildCrossProjectConventionSection(docs); section != "" {
		t.Fatalf("section = %q, want empty", section)
	}
}

// crossHasString is a tiny membership helper (the slice is a []string).
func crossHasString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
