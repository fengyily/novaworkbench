# 「📚 知识库」需求使用指南

> 这是一篇**给最终用户看的操作手册**：怎么在 NovaWorkbench 里创建一份
> 知识库条目，怎么生成 / 微调 / 应用 / 归档到知识库。

如果你想了解为什么这样设计、代码层是怎么实现的，请看
[`technical-plan.md`](./technical-plan.md)。

---

## 1. 什么是「知识库」类需求

NovaWorkbench 把每条需求分为四类：**问题修复（issue）**、**功能需求
（requirement）**、**想法探讨（idea）**、**知识库（wiki）**。前三类最终
都会走「开发 → commit-push → PR → 合并」链路；**知识库类只产出一份
Markdown 文档**，不会改项目里任何代码，也不会触发 PR。

适合作为知识库的需求：

- 一份给团队新人的 onboarding 指南；
- 一次架构调研 / 外部库的对比总结；
- 一份对外的接口说明、运维手册；
- 一段奇怪的 bug 复现路径，希望被未来遇到的人快速检索到。

不适合：

- 真的要修的 bug（选 **问题修复**）；
- 真的要做的功能（选 **功能需求**）；
- 还在发散的方向讨论（选 **想法探讨**）。

## 2. 创建一条知识库需求

1. 进入项目详情页，点 **+ 新建需求**；
2. 在「需求类型」一栏选择 **📚 知识库**；
3. 写下需求标题（建议是动词 + 对象，例如「如何排错 v0.5.x 的 Agent
   Server 远端命令卡死」「NovaWorkbench Wiki kind 全景说明」）；
4. 写下需求描述（背景 + 期望覆盖哪些点 + 想要的格式 / 是否需要图表）；
5. 提交。

提交后状态为 **`draft`**，并且**自动跳过**分析师会话与技术方案阶段——
直接进入「生成知识库文档」入口。

## 3. 生成知识库文档

在项目详情页的「📚 知识库文档」section，点 **生成知识库文档**。后端会启
一个 plan-mode 的 Claude 子任务：

- 工作流与「生成技术方案」一致：plan-mode（只读）+ `DisallowedTools` =
  `[Write, Edit, NotebookEdit]`；
- 子任务**不会改源代码**，只会阅读项目上下文与已有源文件，最终输出一份
  Markdown 文档；
- 流程图、时序图、状态图请用 Mermaid 代码块（` ```mermaid ... ``` `），
  渲染时会自动出图；
- 通过 SSE 实时回传 plan-mode 的中间过程（phase / tool_call /
  message），失败 / 部分结果会在卡片中暴露。

完成后状态自动从 `draft` 提升到 **`designed`**，文档正文持久化到
`requirements.wiki_docs`。

> 重新生成（如不满意换风格 / 想扩充）会沿用同一份 Claude session，
> 上下文会保留；如要彻底重开，刷新页面 + 重跑即可（session id 失效会
> 自动清空并提示）。

## 4. 微调 / 应用文档

与「技术方案」共用同一个 `DocRefineChat` 组件，区别只是 `doc_type =
'wiki'`：

- **微调文档**：与 Claude 多轮对话修改同一份 Markdown。`apply` 后增量写
  入 `wiki_docs`；整个流程跑完不会改任何源文件；
- **应用文档**：直接生成一份完整 Markdown 替换 `wiki_docs`（适合大改）。

操作按钮在「📚 知识库文档」section 底部，仅在 `status === 'designed'`
时可见。

## 5. 归档到知识库

文档满意后，点 **归档到知识库**（`designed` → `archived`）。它会在
项目知识库表里插入（或更新）一条条目：

| 字段         | 写入值                                           |
|--------------|--------------------------------------------------|
| `source_type`| `wiki_doc`                                       |
| `source_ref` | `wiki:<需求 id>`（与普通需求归档 `id` 区分）      |
| `category`   | `wiki_doc`                                       |
| `title`      | 需求标题                                         |
| `content`    | `# <标题>\n\n<wiki_docs 全文>`                    |
| `is_reviewed` / `is_approved` | 均为 `1`（归档即已审核通过） |

这样 `/knowledge` 页面里 wiki 条目与「需求归档」条目**互不冲突**——
`source_ref` 不同，永远是两行。多次归档幂等，永远写到同一行。

## 6. 取消归档

如果想撤回归档（同时保留 `wiki_docs`），点 **取消归档**（`archived` →
`designed`）即可。`wiki_docs` 不会丢，再次归档会得到字节级一致的知识条
目。

## 7. 常见问题

**Q：为什么 wiki 不能像普通需求那样开发？**
A：wiki 是「只读沉淀」，刻意阻断 commit-push-PR。如果有可执行的工作，建
议新建一条 issue 或 requirement 引用本 wiki 文档。

**Q：归档条目能否再次修改？**
A：`/knowledge` 页面提供条目级别的编辑入口，但**推荐**改回去改
`wiki_docs` 然后重新归档——这样版本来源唯一，幂等性自动保证。

**Q：能不能把已有的设计文档改成 wiki？**
A：不行——`kind` 是创建时定死的。如果确实要切换，把 `design_docs` 内容
复制出来，新建一条 wiki 需求即可。

**Q：Claude 误改了源码怎么办？**
A：plan-mode 自身会拒绝 `Write/Edit/NotebookEdit` 工具调用；本功能还显
式把上述三个工具加进 `DisallowedTools`。即便模型「误改」，子进程会直接
报错退出，**不会真正落盘**。如果你真的看到了被改动的文件，请提一个
issue 并附上 session 日志。

**Q：wiki 文档能跨项目吗？**
A：当前只挂在所属项目下。`source_ref='wiki:<id>'` 是项目内唯一的；如
需跨项目共享，可把归档的条目手工迁移（`knowledge` 表行级导出）。

## 8. 相关文档

- [技术方案（给开发者看）](./technical-plan.md)
- [NovaWorkbench 仓库 README](../../README.md)
- `handler/wizard_wiki.go` — 后端生成入口
- `pages/RequirementDetail.tsx` — 「📚 知识库文档」section 的实现
