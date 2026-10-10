# 知识库（wiki）需求类型 — 技术方案

> 状态：随 `feat(wiki): add 'wiki' requirement kind for read-only knowledge docs`
> 落盘（`a2bebe6`），本文档覆盖其设计动机、关键决策、调用链与风险点。

---

## 1. 背景与动机

NovaWorkbench 的需求流水线历史上只有三种 `kind`：`issue` / `requirement` /
`idea`。它们都遵循 **draft → analyzing → designing → designed → developing
→ done** 的状态机，且最终都会进入「commit-push-PR → merge」环节。这套路径
不适合「沉淀可复用知识条目」类需求——一个团队成员的运维笔记、一次调研总结
、一份对外接口说明，都不应该被当作一个 issue 提走。

`wiki` kind 引入了一种新的**只读型知识库文档**：AI 在 plan 模式下阅读相关
源文件，输出一份可归档的 Markdown 文档，可以单独归档到项目知识表，或者由
「知识库」列表页筛选检索。文档本身**永远不会触发 commit-push-PR**。

目标：

1. 让「沉淀一份可复用知识」这件事有 first-class 的入口与产出物。
2. 完全复用现有 wizard 基础设施（plan-mode、JobStore+SSE、`runClaudeStream`
   捕获 plan、DocRefineChat 微调链路），**不引入新的状态机、新的表、新的
   流式协议**。
3. 严格保证「只读」语义：plan-mode + `DisallowedTools` 双层防御，wiki 文档
   不留任何修改项目文件的可能。

## 2. 设计概览

### 2.1 数据流

```
  UI: 项目详情 → 知识库 tab → 生成/微调/应用/归档
            │
            ▼
  POST /api/wizard/wiki/generate               (handler/wizard_wiki.go)
            │  {requirement_id, model?, claude_config_id?, agent_server_id?, read_knowledge?}
            ▼
  WizardHandler.prepareWikiDoc                  ← 同步：kind 守卫 + 状态提升 + JobStore 创建
            │
            ▼
  WizardHandler.prepareWikiWorkspace            ← 异步：git sync 锁基线 + worktree + 拼 prompt
            │
            ▼
  h.llm.StreamCmd(...)  plan 模式 + DisallowedTools=[Write,Edit,NotebookEdit]
            │
            ▼
  runClaudeStream 捕获 planContent
            │
            ▼
  WizardHandler.finalizeWikiRun  → UpdateWikiDoc(id, markdown)
                                   → 自动提升 status: designing → designed
                                    │
            ▼
  DocRefineChat (doc_type='wiki')            ← 微调 / 应用，落到 wiki_docs
            │
            ▼
  POST /api/requirements/{id}/wiki-archive    → knowledge 表 upsert
                                                  source_ref='wiki:<id>'
                                                  source_type='wiki_doc'
```

### 2.2 状态机差异（与常规需求的对比）

| 阶段         | issue/requirement/idea        | wiki                                 |
|--------------|-------------------------------|--------------------------------------|
| 分析师会话   | ✅ 必须                       | ❌ 跳过（强制 `skip_analysis=true`）  |
| 架构师方案   | ✅ 可选                       | ❌ 跳过（强制 `skip_design=true`）    |
| 开发者会话   | ✅ 可选                       | ❌ 4 处入口全部 guard 拒绝            |
| 启动 commit-push-PR | ✅ 触发                | ❌ 永不触发                           |
| 归档到知识表 | 走普通 `Archive`，`source_type='requirement'` | 走 `WikiArchive`，`source_ref='wiki:<id>'`，`source_type='wiki_doc'` |

4 处 wiki guard：

- `handler/wizard_orchestration.go` — 编排子任务入口拒绝 wiki；
- `handler/wizard_coding.go` — `StartCoding` 入口拒绝；
- `handler/wizard_immediate.go` — `DesignCodingImmediateModal` 路径拒绝；
- `handler/schedule.go` — `scheduled_tasks` 的 `coding` / `design_and_coding`
  类型拒绝 wiki 需求作为输入。

## 3. 关键决策

### 3.1 为什么不新建一张表 / 一个状态机

考虑过「`requirements` 表加 `is_wiki` 标志 + 新增 `wiki_status`」方案。
否决理由：

- 数据库迁移会引入两条并行状态机，回归测试需要在两个项目里跑；
- 与 `design_docs` 字段镜像后，前端几乎不用改——一行 `reqKind === 'wiki'`
  即可条件渲染。

权衡后选择「镜像 `design_docs` 字段 + 加 wiki_docs / wiki_session_id + 同
状态机」。这样「设计阶段产物」的概念在表里只有一份实现。

### 3.2 为什么不复用 analyst → design 的 fork-session 链路

`architect-design` 走 `--resume <analysis_sid> --fork-session`，继承分析师
会话上下文。wiki 不需要分析师阶段，所以**没有可 fork 的源会话**——第一轮
一定是新会话。`prepareWikiDoc` 里直接调 `util.NewUUID()` mint 一个
`newWikiSID`，并 `UpdateWikiSession(id, newWikiSID)` 在 spawn 前持久化，
以保证中途崩溃也能记录 ID。

重跑时走 `ResumeSID == req.WikiSessionID` 的分支，与 design-stage 重跑
行为完全一致。

### 3.3 只读约束的三重防线

| 层次            | 机制                                                                     |
|-----------------|--------------------------------------------------------------------------|
| 文本约束        | `prompt.WikiBlock("wiki")`：明确写明「禁止改源码 / 禁止 git 写入 / 仅输出 Markdown」 |
| 工具约束        | `wikiDisallowedTools = ["Write","Edit","NotebookEdit"]`                  |
| 模式约束        | `PermissionMode: "plan"`（已隐含禁止非只读工具；显式声明作为冗余防御）   |

### 3.4 自动 `designing → designed` 提升

与「技术方案」不同，wiki 文档**没有「标记完成」的人工 gate**：

- 技术方案需要用户主动审核设计是否合理；
- wiki 文档是 plan-mode 一次性产物：plan 内容稳定后直接归档意义不大，
  让用户多一步点击反而增加误触。

所以 `finalizeWikiRun` 在 `UpdateWikiDoc` 成功后**主动调
`UpdateStatus(id, "designed")`**，免去手动 gate。归档用
`WikiArchive`/`WikiUnarchive` 把 `designed` ↔ `archived` 状态互转，原始
`wiki_docs` 永远保留，所以取消归档是幂等的。

### 3.5 归档幂等性

`WikiArchive` 用事务 + 复合条件查询 `source_ref='wiki:<id>' AND
source_type='wiki_doc'`：

- 存在 → `UPDATE knowledge SET content=?, title=?`；
- 不存在 → `INSERT` 一条新的 `kb_` ID 知识条目。

这样**多次归档**与**归档后修改 wiki_docs 再归档**都收敛到同一行。普通
`Archive`（`source_type='requirement'`）的同行不会被覆盖——靠
`source_ref` 不同（ID vs `wiki:<id>`）天然分离。

### 3.6 plan-mode plan 捕获复用

`runClaudeStream` 已经会在 plan 模式下从 `~/.claude/plans/<slug>.md` 抓
plan markdown（`wizard_stream.go` 里 `out.planContent` 字段）。wiki 完全复
用：不引入新的 plan-mode 处理路径，也不解析模型最终消息——以 Write
tool_use 的内容为准，回退到 `finalResult`。

### 3.7 沙箱只跑 local（不接入 agent server）

当前 `execWikiDoc` 只走本地分支。不引入 `BuildRemoteEnvPairs*` 或
`runRemoteArchitectDesign` 这条远程路径。理由：

- wiki 多半用于沉淀自己项目的内部笔记，远程跑反而要额外部署 agent-worker；
- 如果后续确实有「在 Agent Server 上跑 wiki」需求，落点是 `execWikiDoc`
  内的 `if agentServerID != ""` 分支（与 `runRemoteArchitectDesign` 同构），
  当前设计的 `wikiRunParams.AgentServerID` 字段已经预留。

## 4. 接口与表结构

### 4.1 新增列（幂等 ALTER，已落 schema.go）

```sql
ALTER TABLE requirements ADD COLUMN wiki_docs        TEXT NOT NULL DEFAULT '';
ALTER TABLE requirements ADD COLUMN wiki_session_id  TEXT NOT NULL DEFAULT '';
```

两列与 `design_docs` / `design_session_id` 对称，迁移脚本与 schema 修复函
数里都已 idempotent 处理。

### 4.2 新增服务方法

| 方法 | 文件 | 行为 |
|------|------|------|
| `KindWiki` 常量 + `ValidKind` 分支 | `service/requirement.go` | 把 `wiki` 加入白名单 |
| `UpdateWikiDoc(id, md)` | 同上 | `UPDATE requirements SET wiki_docs=?, status='designing'` |
| `UpdateWikiSession(id, sid)` | 同上 | 仅写 `wiki_session_id` |
| `WikiArchive(id)` | 同上 | 事务 + knowledge upsert（`source_ref='wiki:<id>'`） |
| `WikiUnarchive(id)` | 同上 | 删除 knowledge 行，status 回到 `designed` |

### 4.3 新增 HTTP 路由

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/wizard/wiki/generate` | 启动 wiki 文档生成（plan-mode JobStore） |
| POST | `/api/requirements/{id}/wiki-archive` | 归档到 `knowledge` 表 |
| POST | `/api/requirements/{id}/wiki-unarchive` | 取消归档 |

`RefineDoc` / `ApplyDoc` 在 `doc_type='wiki'` 分支被路由到
`UpdateWikiDoc`，**绝不写 `design_docs`**——两个 doc_type 在数据库里隔离。

### 4.4 前端改动

- `frontend/src/api/client.ts` — `Kind` / `kindLabelKeys` / `kindHintKeys`
  / `kindPlaceholderKeys` / `kindCreateLabelKeys` / `kindShortLabelKeys` /
  `kindChatPlaceholderKeys` / `STAGE_VISIBILITY` / `branchPrefixForKind`
  都新增 `wiki` 条目；新增 `requirementsApi.generateWikiDoc` /
  `wikiArchive` / `wikiUnarchive`；
- `CreateRequirementForm.tsx` — wiki radiogroup + 强制 `skip_analysis` /
  `skip_design`；
- `pages/RequirementDetail.tsx` — 条件渲染「📚 知识库文档」section、
  4 个状态门控按钮（生成/微调/应用/归档 + 取消归档），**隐藏 coding /
  merge / sub-task CTA**；
- `pages/RequirementsList.tsx` + `RequirementsCalendar.tsx` — wiki 种类的
  筛选 chip；
- `components/DocRefineChat.tsx` — `docType` 类型扩大到 `'design' |
  'coding' | 'wiki'`；
- i18n zh-CN / en-US 模块同步新增 wiki 相关键；
- `index.css` — `.kind-badge.kind-wiki`。

## 5. 风险与回滚

| 风险                                           | 缓解                                                                 |
|-----------------------------------------------|----------------------------------------------------------------------|
| 模型在 plan 模式下意外改源码                   | 三重防线（文本 + 工具 + 模式），并落到 server 日志可追溯              |
| `wiki_docs` 内容把 Mermaid 解析开销带进 React 渲染 | wiki block 已禁用任何重型转换；MarkdownViewer 仍按设计渲染          |
| 用户误把普通需求标成 wiki 后想立刻归档         | `WikiArchive` 仍要求 `kind=wiki && status ∈ {designed,done}`，前端隐藏 |
| 老的「设计」需求想切到 wiki                    | **不支持**：kind 是写时确定的，改 kind 需要用户复制正文 + 新建条目    |
| wiki session id 失效后用户无法继续微调         | `finalizeWikiRun` 检测 staleSession 会清空 id 并要求重跑             |
| 多语言 key 缺失                                | CI 上 `scripts/check-i18n-keys.mjs` 会校验 zh-CN / en-US 同步         |

回滚路径：删除 `kind=wiki` 的需求行（保留 `wiki_docs` 备份到外部归档），
不再创建新的 wiki 需求即可；schema 列可保留以做历史可读性，不影响其他需求。

## 6. 验收

- 单元 / 手工覆盖：见 `VALIDATION.md`（本仓库已有验证报告）。
- 端到端 smoke（人工）：
  1. 项目详情 → 创建需求（类型选「📚 知识库」）→
     状态为 `draft`、跳过分析 / 设计 CTA；
  2. 点击「生成知识库文档」→ plan-mode 流式日志 → 完成后跳转到
     `designed`、`wiki_docs` 有正文；
  3. 「微调文档」/`应用文档` 走 `DocRefineChat` 同样的流式面板；
  4. 「归档到知识库」→ 知识页可见新条目，`source_type='wiki_doc'`；
  5. 「取消归档」→ 条目消失，原需求回到 `designed`。
- 任何 `kind ≠ wiki` 的请求打到 `/api/wizard/wiki/generate` 都应返回
  `400 WIKI`。

## 7. 未来扩展

- 远端 agent-server 执行（`BuildRemoteEnvPairs*` 路径预留 `agentServerID`
  字段，未启用）；
- 知识库搜索 / 全文检索（与现有 `knowledge` 表的搜索能力同步）；
- wiki 文档版本化（在 `requirements` 表加 `wiki_docs_history` JSON 数组）。
