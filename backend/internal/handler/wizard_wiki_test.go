package handler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// TestLooksLikeWikiPreamble pins the existing short-progress-message
// detector. The motivating case (commit 444a2bd) was a one-line preamble
// like "我已并行启动 2 个 Explore 子代理深入调研 ... 等待回收中…" — the
// tier-3 streamText fallback used to grab that and persist it as the
// wiki doc. Negative cases pin the markdown-structure guard.
func TestLooksLikeWikiPreamble(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "motivating preamble", body: "我已并行启动 2 个 Explore 子代理深入调研 ... 等待回收中…", want: true},
		{name: "single short line", body: "思考中...", want: true},
		{name: "empty", body: "", want: true},

		// negative — long enough to pass the length check
		{name: "long prose >= 200 chars", body: strings.Repeat("a", 250), want: false},
		// negative — has markdown structure even though short
		{name: "short with heading", body: "# 标题\n", want: false},
		{name: "short with code fence", body: "```bash\necho hi\n```", want: false},
		{name: "short with list", body: "- a\n- b", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeWikiPreamble(c.body); got != c.want {
				t.Errorf("looksLikeWikiPreamble(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// TestLooksLikeWikiOutline covers the "TOC instead of body" failure mode
// discovered on req_b37e6f385d371b7e. The wiki doc that landed in
// `requirements.wiki_docs` for that requirement was 1073 chars of pure
// outline ("知识库正文已完整输出。文档覆盖：1. **背景与目标**：MCP 在
// Controller 内的定位 ... 2. **关键概念**：模块分层图 ...") — no
// Mermaid block, no file paths, no real content. The preamble heuristic
// missed it (1073 > 200 + has list structure); this heuristic catches it.
//
// Three rejection paths are tested:
//   (c) self-praise tell (e.g. "知识库正文已完整输出")
//   (a)+(b) 3+ label-only list items AND < 4 backticks
func TestLooksLikeWikiOutline(t *testing.T) {
	// The actual 1073-char content that landed in req_b37e6f385d371b7e —
	// pinned here as the regression target. If a future prompt change
	// re-introduces this shape, this case should still reject.
	const actualFailureBody = `知识库正文已完整输出。文档覆盖：

1. **背景与目标**：MCP 在 Controller 内的定位、与 ` + "`datapipe/agent`" + ` 的关系、关键术语表。
2. **关键概念**：模块分层图、三道门（租户/会话/角色）、scope 集合、标识符解析、site 作用域、PHI 输出闸口、尺寸上限、限流、SSE 升级、审计、token 与 OAuth。
3. **源文件清单**：按层（核心 / OAuth / Service / HTTP 注册 / 协议 / DB / 前端 / Agent 客户端）逐一列出，标注必读/选读。
4. **典型调用路径**：包含两张 Mermaid 时序图（注册→授权→调用→审计、tools/call 内部路径）、代码骨架、客户端调用模板、管理员 REST 表、部署要点。
5. **风险与注意事项**：协议安全、资源耗尽、数据一致性、协议演进、可观察性、扩展新工具的清单、与 Agent 客户端的对齐点。
6. **一图速记**：单一流程图覆盖发现/注册/授权/MCP 调用全链路。
7. **信息不足**：明确标注本轮未深读的小节，便于下一轮补全。`

	// A realistic wiki-doc fragment: has a mermaid block AND file paths
	// AND function names. Must NOT be rejected.
	realDoc := "## 模块分层图\n\n" +
		"```mermaid\nflowchart TD\n  A[Controller] --> B[OAuth]\n```\n\n" +
		"### 涉及文件\n\n" +
		"- `datapipe/controller/mcp/server.go`：HTTP 注册入口\n" +
		"- `datapipe/controller/mcp/auth.go`：token 校验\n\n" +
		"### 关键函数\n\n" +
		"`func (s *Server) Authorize(ctx context.Context, tok string) error` 是 token 校验主入口。\n"

	// A "label-only" outline that omits the self-praise tell but matches
	// the (a)+(b) rules: 3+ label items, no backticks.
	labelOnlyOutline := "下面是文档结构：\n\n" +
		"1. **背景**：系统定位与术语表\n" +
		"2. **关键概念**：模块分层、限流、审计\n" +
		"3. **源文件清单**：核心、Service、HTTP\n" +
		"4. **调用路径**：调用排示例\n"

	// Negative control: a label-only list with a code block — the (b)
	// branch (< 4 backticks) should fail and the doc is kept.
	labelWithCode := "1. **背景**：系统定位\n" +
		"2. **关键概念**：模块分层\n" +
		"3. **源文件清单**：核心\n\n" +
		"```mermaid\nflowchart TD\n  A-->B\n```\n"

	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "actual req_b37e6f385d371b7e failure", body: actualFailureBody, want: true},
		{name: "real wiki doc fragment (mermaid + paths)", body: realDoc, want: false},
		{name: "label-only outline, no self-praise, no code", body: labelOnlyOutline, want: true},
		{name: "label items WITH code fence → keep", body: labelWithCode, want: false},
		{name: "short preamble (let old heuristic handle)", body: "思考中...", want: false},
		{name: "single label item alone (need 3+) → keep", body: "1. **背景**：定位\n2. **关键概念**：分层\n", want: false},
		{name: "empty", body: "", want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeWikiOutline(c.body); got != c.want {
				t.Errorf("looksLikeWikiOutline(%q) = %v, want %v", truncateForLog(c.body, 60), got, c.want)
			}
		})
	}
}
// --------------------------------------------------------------------
// Execution environment / model persistence (Agent Server alignment)
// --------------------------------------------------------------------

// TestPrepareWikiDocStampsExecEnv pins the prologue that brings the wiki
// stage in line with architect-design: the picked Agent server and the
// code-transport mode are persisted BEFORE any SSH / worktree work, so a
// later failure still records what the user chose and a page refresh
// re-hydrates the toolbar.
//
// kind=wiki rows reuse the design-stage columns (design_agent_server_id /
// sync_mode) because a wiki row never has a design stage — see
// wikiRunParams.AgentServerID.
//
// The sync_mode cases also pin Stamp-If-Non-Empty semantics: an empty
// sync_mode keeps whatever is stored, and local execution never stamps at
// all (nothing is shipped over SFTP).
func TestPrepareWikiDocStampsExecEnv(t *testing.T) {
	cases := []struct {
		name          string
		agentServerID string
		syncMode      string
		seedSyncMode  string
		wantServer    string
		wantSync      string
	}{
		{
			name:          "agent server + bundle transport are both stamped",
			agentServerID: "agent_1",
			syncMode:      "local",
			wantServer:    "agent_1",
			wantSync:      "local",
		},
		{
			name:          "empty sync_mode keeps the stored value",
			agentServerID: "agent_1",
			syncMode:      "",
			seedSyncMode:  "local",
			wantServer:    "agent_1",
			wantSync:      "local",
		},
		{
			name:          "local execution clears the server and never touches sync_mode",
			agentServerID: "",
			syncMode:      "local",
			seedSyncMode:  "",
			wantServer:    "",
			wantSync:      "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, reqID := seedWikiRequirement(t)
			reqSvc := service.NewRequirementService(d, nil)
			if c.seedSyncMode != "" {
				if err := reqSvc.UpdateSyncMode(reqID, c.seedSyncMode); err != nil {
					t.Fatalf("seed sync_mode: %v", err)
				}
			}
			h := &WizardHandler{
				reqSvc:     reqSvc,
				projectSvc: service.NewProjectService(d, nil, nil),
				jobs:       store.NewJobStore(8),
				roleSvc:    service.NewRoleService(d),
				claudeCfg:  service.NewClaudeConfigService(d),
			}

			p, job, af := h.prepareWikiDoc(context.Background(), reqID, "", "", c.agentServerID, false, c.syncMode)
			if af != nil {
				t.Fatalf("prepareWikiDoc failed: %+v", af)
			}
			if job == nil {
				t.Fatal("prepareWikiDoc returned no job")
			}
			if p.AgentServerID != c.agentServerID {
				t.Errorf("params.AgentServerID = %q, want %q", p.AgentServerID, c.agentServerID)
			}
			if p.SyncMode != c.syncMode {
				t.Errorf("params.SyncMode = %q, want %q", p.SyncMode, c.syncMode)
			}

			got, err := reqSvc.Get(reqID)
			if err != nil {
				t.Fatalf("reload req: %v", err)
			}
			if got.DesignAgentServerID != c.wantServer {
				t.Errorf("design_agent_server_id = %q, want %q", got.DesignAgentServerID, c.wantServer)
			}
			if got.SyncMode != c.wantSync {
				t.Errorf("sync_mode = %q, want %q", got.SyncMode, c.wantSync)
			}
			// The job pointer must be live so a refresh mid-run reconnects.
			if got.WikiJobID != job.ID {
				t.Errorf("wiki_job_id = %q, want %q", got.WikiJobID, job.ID)
			}
		})
	}
}

// TestPrepareWikiDocRejectsNonWikiKind pins the hard kind guard — the
// scheduled path (RunScheduledWiki) and the launch-spec path both bottom
// out here, so this is the last line of defense if either gate is ever
// relaxed.
func TestPrepareWikiDocRejectsNonWikiKind(t *testing.T) {
	d, reqID := seedWikiRequirement(t)
	if _, err := d.Exec(`UPDATE requirements SET kind='requirement' WHERE id=?`, reqID); err != nil {
		t.Fatalf("flip kind: %v", err)
	}
	h := &WizardHandler{
		reqSvc:     service.NewRequirementService(d, nil),
		projectSvc: service.NewProjectService(d, nil, nil),
		jobs:       store.NewJobStore(8),
		roleSvc:    service.NewRoleService(d),
		claudeCfg:  service.NewClaudeConfigService(d),
	}
	_, _, af := h.prepareWikiDoc(context.Background(), reqID, "", "", "", false, "")
	if af == nil || af.Code != "WIKI" {
		t.Fatalf("expected 400 WIKI, got %+v", af)
	}
}

// TestFinalizeWikiRunPersistsModelAndConfig pins the success-path writes
// that let the wiki toolbar re-hydrate its model + Claude-config dropdowns
// after a refresh. kind=wiki reuses the architect columns (a wiki row has
// no architect stage), mirroring finalizeArchitectRun.
func TestFinalizeWikiRunPersistsModelAndConfig(t *testing.T) {
	d, reqID := seedWikiRequirement(t)
	reqSvc := service.NewRequirementService(d, nil)
	jobs := store.NewJobStore(8)
	job := jobs.Create(reqID)
	if err := reqSvc.UpdateWikiJob(reqID, job.ID); err != nil {
		t.Fatalf("seed wiki_job_id: %v", err)
	}
	h := &WizardHandler{reqSvc: reqSvc, jobs: jobs}

	req, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	p := &wikiRunParams{
		Req:            req,
		NewWikiSID:     "sid-wiki",
		Model:          "claude-opus-5",
		ClaudeConfigID: "ccfg_remote",
		AgentServerID:  "agent_1",
		SyncMode:       "local",
	}

	// A doc body that survives SanitizeDesignDoc and both rejection
	// heuristics: a heading plus backticked file paths / function names.
	// No fenced block — SanitizeDesignDoc unwraps an outer fence, and a
	// doc whose only fence is the trailing one gets reduced to its body.
	body := "# 模块总览\n\n" +
		"核心入口在 `internal/handler/wizard_wiki.go` 的 `execWikiDoc`，" +
		"远端分支走 `runRemoteWikiDoc`，两条路径汇合到 `finalizeWikiRun`。\n"
	h.finalizeWikiRun(claudeStreamOutcome{sessionID: "sid-wiki", planContent: body}, p, job, reqID, p.Model)

	got, err := reqSvc.Get(reqID)
	if err != nil {
		t.Fatalf("reload req: %v", err)
	}
	if got.ArchitectModel != "claude-opus-5" {
		t.Errorf("architect_model = %q, want %q (wiki reuses the architect columns)", got.ArchitectModel, "claude-opus-5")
	}
	if got.ArchitectConfigID != "ccfg_remote" {
		t.Errorf("architect_config_id = %q, want %q", got.ArchitectConfigID, "ccfg_remote")
	}
	if got.Status != "designed" {
		t.Errorf("status = %q, want designed", got.Status)
	}
	// Terminal path must always clear the job pointer, remote or local.
	if got.WikiJobID != "" {
		t.Errorf("wiki_job_id = %q, want \"\" on the terminal path", got.WikiJobID)
	}
	if _, _, exit := job.Snapshot(); exit != 0 {
		t.Errorf("job exit code = %d, want 0", exit)
	}
}

// seedWikiRequirement spins up a throwaway SQLite DB holding one project +
// one kind=wiki requirement, the minimum the wiki stage resolves through
// RequirementService.Get.
func seedWikiRequirement(t *testing.T) (*db.DB, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wiki-exec-")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	const projID, reqID = "proj_wiki_exec", "req_wiki_exec"
	if _, err := d.Exec(`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "Seed", filepath.Join(dir, "proj"), "ready", "main"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title, kind, status) VALUES (?, ?, ?, ?, ?)`,
		reqID, projID, "Seed wiki", service.KindWiki, "draft"); err != nil {
		t.Fatalf("insert requirement: %v", err)
	}
	return d, reqID
}
