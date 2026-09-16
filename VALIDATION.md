# VALIDATION — 方案设计前同步仓库到工作目录（Step 4 端到端验证）

需求 ID: `req_89a1fce301492b46`
验证日期: 2026-09-16
执行范围: Step 4（端到端内部验证）— Step 1 / Step 2 基础设施列 / Step 3 handler 接入的最终落地状态快照

---

## 0. 前置交付物盘点（重要 ⚠️）

| Step | 范围 | 实际落地状态 |
|---|---|---|
| Step 1 — Schema / Model / 常量 | `db/schema.go`（3 个 ALTER）、`model/project.go`（3 个 JSON 字段）、`service/project.go`（SyncStatus* 常量） | ✅ **完整落盘**（git diff 已确认） |
| Step 2 — service 层 `EnsureClonedAndSynced` + syncBaseBranch 服务化 + SELECT 列更新 | `service/project.go` 新增方法、`service/project_test.go` 新增、`handler/worktree.go` 改造 | ⚠️ **部分缺失**：只有 Step 4 范围内的"SELECT 列更新"（4 个查询）已落盘；`EnsureClonedAndSynced` 方法本体、`syncBaseBranchService` 服务层封装、`service/project_test.go` 单测 — **均未在工作中提交**，未出现在 git diff 中 |
| Step 3 — handler 接入（wizard_common / architect / analyst / coding / remote） | SSE phase 事件、resolveWorkDir 签名扩展 | ❌ **完全缺失**：`resolveWorkDirLogged` 签名未扩展，仍调用老的 `EnsureCloned`；`prepareArchitectDesign` 未增加 phase 事件；`wizard_remote.go` 远端 fetch 回写未实现 |

> **本报告把所有验证项按"可执行 vs 受限"分开记录**。受 Step 2/3 未落地影响的验证项以"❌ 受前置缺失阻塞"形式呈现，不冒充通过。

---

## 1. schema 迁移幂等性 ✅

### SQLite（默认方言）

- 起点：`cp ~/.novaworkbench/data/nova.db /tmp/nova_step4.db`（注意：用户默认配置为 postgres，所以走 `NOVA_DB_DRIVER=sqlite NOVA_DB_PATH=/tmp/nova_step4.db` 强制覆盖）
- 第一次启动日志（关键节选）：

```
2026/09/16 09:33:32 db.go:359: Database initialized: driver=sqlite path=/tmp/nova_step4.db
2026/09/16 09:33:32 main.go:84: [secret] master key loaded from /Users/f1/.novaworkbench/secret.key
... (无 migrate 错误，无 duplicate column 报错)
2026/09/16 09:33:32 main.go:671: Server listening on http://localhost:9528
```

- 第二次启动日志（同一份 /tmp/nova_step4.db）：

```
2026/09/16 09:34:26 main.go:671: Server listening on http://localhost:9528
```

  - 关键确认：`grep -iE "error|migrate|duplicate column|fatal" /tmp/nova_step4_run2.log` 输出为空 → **无 duplicate column 错误，幂等通过**

- `.schema projects` 输出末尾：

```sql
... commit_lang TEXT NOT NULL DEFAULT '',
    commit_lang_override TEXT NOT NULL DEFAULT '',
    commit_lang_source TEXT NOT NULL DEFAULT '',
    commit_lang_updated_at DATETIME,
    last_synced_at DATETIME,
    last_synced_commit TEXT NOT NULL DEFAULT '',
    sync_status TEXT NOT NULL DEFAULT 'idle'
```

- `PRAGMA table_info(projects)` 中匹配 `last_synced_at|last_synced_commit|sync_status` 的列数：**3**

### PostgreSQL（方言冒烟，docker nova-postgres 可用）

- 起点：新建 `nova_step4_test` 数据库
- 第一次启动日志：无 migrate / duplicate column 报错
- 第二次启动日志：`grep -iE "error|migrate|duplicate column|fatal" /tmp/nova_step4_pg_run2.log` 输出为空
- `\\d projects` 输出节选：

```
 last_synced_at         | timestamp without time zone |           |          |
 last_synced_commit     | text                        |           | not null | ''::text
 sync_status            | text                        |           | not null | 'idle'::text
```

  - 关键确认：DATETIME → timestamp without time zone 翻译正确；TEXT DEFAULT '' 类型正确；DEFAULT 'idle' 落库
- 测试结束清理：`DROP DATABASE nova_step4_test`

### MySQL（方言冒烟）

- 本机未运行 mysql 容器，按设计文档约定由 CI / Release pipeline 覆盖。本次仅在 commit message 中标注：**"三方言验证由 CI / 后续 release 流程覆盖"**
- 风险评估：当前 DDL 用的列名 `last_synced_at` / `last_synced_commit` / `sync_status` 均非 MySQL 保留字，不需要 backtick 包裹；`fixupSchema` 已对 `DATETIME` → `DATETIME`（MySQL 原生支持）和 `TEXT DEFAULT ''` 全方言覆盖。
- 后续步骤：CI 阶段在 docker mysql:8 上重复一次迁移幂等性测试。

✅ **通过**：sqlite + postgres 两方言迁移幂等；MySQL 由 CI 覆盖。

---

## 2. API 字段透出 ✅

### 请求

```bash
NOVA_DB_DRIVER=sqlite NOVA_DB_PATH=/tmp/nova_step4.db NOVA_PORT=9528 NOVA_AUTH_DISABLED=1 \
  go run ./cmd/server &
curl -s http://localhost:9528/api/projects/proj_179e5d6f69bdbb17
```

### 响应（jq 等价，python json.tool 格式化）

```json
{
    "success": true,
    "data": {
        "id": "proj_179e5d6f69bdbb17",
        "name": "diagrams",
        "local_path": "/Users/f1/workspace/diagrams",
        "remote_url": "https://github.com/fengyily/diagrams",
        "status": "active",
        "default_branch": "main",
        "project_type": "Unknown",
        "claude_files": "{}",
        "platform_type": "",
        "platform_token_id": "",
        "added_at": "2026-08-20T18:06:34.536052+08:00",
        "updated_at": "2026-08-20T18:06:34.536052+08:00",
        "deleted_dir": 0,
        "description": "",
        "description_manual": false,
        "last_synced_commit": "",
        "sync_status": "idle"
    }
}
```

### 字段确认

| 字段 | 期望 | 实际 | 状态 |
|---|---|---|---|
| `last_synced_commit` | `""`（TEXT DEFAULT ''） | `""` | ✅ |
| `sync_status` | `"idle"`（TEXT DEFAULT 'idle'） | `"idle"` | ✅ |
| `last_synced_at` | null（*time.Time + omitempty） | 字段未出现 | ✅（omitempty 跳过 nil 是预期行为，TS 端已用 `last_synced_at?: string` 可选） |

✅ **通过**：3 个字段均按 JSON tag 正确序列化。

> **注**：本验证之所以能跑通，是因为 Step 4 在 SELECT / Scan 上补齐了 3 列（详见第 7 节"Step 4 自有改动"）。如果仅 Step 1 落地而未做此补齐，运行时会因 Scan arity 不匹配（22 列 vs 25 个 dest）触发 `sql: expected 25 destination arguments, got 22` 错误。

---

## 3. 三方言冒烟 ⚠️（部分覆盖）

| 方言 | 状态 | 备注 |
|---|---|---|
| SQLite | ✅ 已验证（见第 1 节） | /tmp/nova_step4.db，跑通 2 次幂等 |
| PostgreSQL | ✅ 已验证（见第 1 节） | docker nova-postgres（postgres:16），跑通 2 次幂等；DATETIME 翻译正确 |
| MySQL | ⚠️ 未在本机验证 | 本机无 mysql 容器实例；CI / release pipeline 覆盖；DDL 无 reserved keyword，无 backtick 需求 |

✅ **通过**：核心两方言（sqlite + postgres）实测；MySQL 由 CI 兜底。

---

## 4. 失败降级验证 ❌（受前置缺失阻塞）

### 4.1 service 层单测 ❌

- 现状：`backend/internal/service/project_test.go` 不存在
- 设计要求（Step 2 prompt）：覆盖 3 个分支：
  - 目录不存在 + 有 remote_url → Cloned=true，sync_status=ok
  - 目录存在 + 有 remote_url + 远端领先 → Fetched=true，last_synced_commit 更新
  - 目录存在 + remote_url="" → Skipped=true，sync_status 维持 idle
  - 远端不可达 → Err 非 nil 但函数不返回 fatal error，sync_status=error 落库
- **阻塞原因**：`EnsureClonedAndSynced` 方法本体未实现（Step 2 缺失），无法写测试
- **风险**：未实现的降级语义可能在 wizard 流程中产生阻塞（fetch hang 时 wizard 等 30s 超时，或在网络上失败时 wizard 直接报错而不是继续）
- **跟进**：必须在 Step 2 实施回填后补齐此测试，否则降级路径无任何回归保护

### 4.2 handler 层 wizard 测试 ❌

- 现状：`wizard_architect_test.go` 等测试存在，但**不覆盖** `EnsureClonedAndSynced` 调用路径
- 设计要求：handler 调用 `EnsureClonedAndSynced` 返回的 syncErr 不应中断 wizard 流程（wizard_architect 仍能返回 jobID）
- **阻塞原因**：Step 3 handler 接入完全未实现，resolveWorkDirLogged 仍调用老的 `EnsureCloned`（同步但无超时降级）

### 4.3 30s context timeout ❌

- 现状：服务代码中**不存在** `context.WithTimeout(ctx, 30*time.Second)` 在 `EnsureClonedAndSynced` 路径上的应用
- 已 grep 验证：`grep -n "EnsureClonedAndSynced\|syncBaseBranchService\|context.WithTimeout.*30" backend/.../service/project.go backend/.../handler/wizard*.go backend/.../handler/worktree.go` 仅命中 service/project.go:30 的**注释引用**——无实际方法体
- **阻塞原因**：`EnsureClonedAndSynced` 未实现
- 现有机制（仅作背景，非本变更）：
  - `handler/worktree.go:68 gitRunWithTimeout` — 30s timeout 已存在，是 Step 2 应迁移的模板
  - `handler/worktree.go:93 syncBaseBranch` — 当前 inline 用 gitRunWithTimeout + GIT_TERMINAL_PROMPT=0

❌ **未通过（受前置缺失阻塞）**：必须在 Step 2/3 实施后重新跑这条验证。

---

## 5. 回归 `go vet ./...` 与 `go test ./...` ✅

### go vet

```bash
$ cd backend && go vet ./...
$ echo "EXIT=$?"
EXIT=0
```

### go test

```bash
$ cd backend && go test ./...
?   	github.com/novaworkbench/backend/cmd/server	[no test files]
ok  	github.com/novaworkbench/backend/internal/db	0.696s
ok  	github.com/novaworkbench/backend/internal/handler	3.730s
ok  	github.com/novaworkbench/backend/internal/llm	0.620s
?   	github.com/novaworkbench/backend/internal/middleware	[no test files]
?   	github.com/novaworkbench/backend/internal/model	[no test files]
?   	github.com/novaworkbench/backend/internal/platform	[no test files]
?   	github.com/novaworkbench/backend/internal/preflight	[no test files]
?   	github.com/novaworkbench/backend/internal/prompt	[no test files]
ok  	github.com/novaworkbench/backend/internal/scheduler	0.991s
?   	github.com/novaworkbench/backend/internal/secret	[no test files]
ok  	github.com/novaworkbench/backend/internal/service	2.440s
?   	github.com/novaworkbench/backend/internal/servicemgr	[no test files]
ok  	github.com/novaworkbench/backend/internal/ssh	1.663s
ok  	github.com/novaworkbench/backend/internal/store	1.962s
?   	github.com/novaworkbench/backend/internal/util	[no test files]
?   	github.com/novaworkbench/backend/internal/version	[no test files]
?   	github.com/novaworkbench/backend/web	[no test files]
```

✅ **全部通过**：7 个含测试的包（db / handler / llm / scheduler / service / ssh / store）全 OK，无 FAIL；6 个包无测试文件（这与现有项目状态一致，不是新增回归）。

---

## 6. Step 4 自有改动（必要的"地基补齐"）

为了让"API 字段透出"验证能跑通，Step 4 必须把 SELECT 列对齐——否则即使 schema + model 加了 3 列，运行时 Scan 会因 arity 不匹配崩。这部分算"Step 2 范围内的基础设施补齐，不改核心方法语义"。

### 改动文件

- `backend/internal/service/project.go`
  - `Get()` (line 117)：SELECT 列清单末尾追加 `last_synced_at, last_synced_commit, sync_status`，Scan 参数同步追加 `&p.LastSyncedAt, &p.LastSyncedCommit, &p.SyncStatus`
  - `getAny()` (line 139)：同上
  - `ListForUser()` (line 48)：SELECT 与 Scan 同步追加
  - `ListTrash()` (line 161)：SELECT 与 Scan 同步追加

### 改动原则

- 仅修改"列清单 + Scan 参数对"两项机械改动
- 不修改 `EnsureClonedAndSynced` 方法本体（核心方法语义不在 Step 4 范围）
- 不修改 handler 任何 wizard 调用路径（Step 3 范围）

### 改动理由

- Step 1 加了 3 列与 3 字段；Step 2 应负责把 SELECT 列同步；Step 2 缺失后 Step 4 范围内的 API 验证无法跑通
- 这是"不改语义"的最小补齐，等价于 Step 2 必须做的 SELECT 更新

---

## 7. 总览

| 验证项 | 结果 | 阻塞原因 / 跟进 |
|---|---|---|
| 1. schema 迁移幂等 | ✅ | sqlite + postgres 实测通过；MySQL 由 CI 覆盖 |
| 2. API 字段透出 | ✅ | Step 4 补齐 SELECT 列后通过 |
| 3. 三方言冒烟 | ⚠️ | sqlite + postgres 实测，MySQL 未实测（无容器实例） |
| 4. 失败降级验证 | ❌ | EnsureClonedAndSynced 未实现（Step 2 缺失）；service_test.go 不存在；handler wizard 测试未覆盖新路径 |
| 5. 30s context timeout | ❌ | 同上，方法体不存在 |
| 6. go vet / go test 回归 | ✅ | 全过 |

---

## 8. 给后续 Step 的硬性建议

1. **Step 2 必须回填** `EnsureClonedAndSynced` 方法本体与 `service/project_test.go` 单测，否则降级路径无回归保护（30s 超时失败可能让 wizard hang 或误报错）
2. **Step 3 必须实施** wizard 三阶段 + 远端路径的 handler 接入，否则 schema/model/SELECT 列虽落地但 wizard 行为不变
3. **MySQL 验证** 留给 CI/release pipeline；本机无 mysql 容器
4. **回归门槛** 任何一次 Step 2/3 的实施落地后，必须重跑本报告第 1、2、4、5 节

---

## 9. 提交范围与严禁项

本报告随 Step 4 自有改动（`service/project.go` 的 SELECT 列对齐）一并提交。**严禁**：
- 把 `/tmp/nova_step4.db` 或任何测试数据库 commit 进仓库（`.gitignore` 已忽略 `/tmp/`）
- 把任何凭据 / token / 平台账号打印到输出
- 添加 AI 署名 commit（`🤖 Generated with Claude Code`、`Co-Authored-By: Claude`、`Co-authored-by: ...` 等 trailer）
- 把本 VALIDATION.md 改成 AI 署名 PR body