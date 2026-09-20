// components — copy for the reusable widgets (model picker, pickers, badges,
// viewers, fullscreen toggle, sub-task panel...). Grows as components are
// internationalized; every key must exist in modules/components.en.ts too.
export const components = {
  modelSelect: {
    stageAnalyst: '分析',
    stageArchitect: '方案',
    stageDeveloper: '开发',
    stageAnalystShort: '析',
    stageArchitectShort: '方',
    stageDeveloperShort: '开',
    default: '默认模型',
    defaultWithModel: '默认模型（{{model}}）',
    configTitle: '先选择 Claude 配置，再选该配置下的模型',
    configAria: '配置',
    configDefault: '默认（不指定）',
    activeSuffix: '（默认）',
    modelAria: '模型',
    outOfList: '当前：{{model}}（不在当前配置列表中）',
    noModels: '该配置未配置模型列表',
  },
  devSource: {
    localTitle: '该需求在本机 NovaWorkbench 上开发',
    local: '本地开发',
    agentLabel: 'Agent Server 开发',
    unknownServer: '未知服务器',
    agentTip: '由 Agent Server「{{server}}」开发',
    modelLine: '模型: {{model}}',
  },
  // execEnv — shared "execution environment" selector + badge (design stage,
  // sub-tasks). Kept separate from devSource so the dev-stage badge can keep
  // its own richer wording (model chip) while these stay generic.
  execEnv: {
    local: '本地',
    localTitle: '在本机 NovaWorkbench 上执行',
    localOption: '本地执行',
    agentLabel: 'Agent Server',
    agentTip: '由 Agent Server「{{server}}」执行',
  },
  markdownViewer: {
    preview: '文档预览',
    close: '✕ 关闭',
    closeTitle: '关闭 (Esc)',
  },
  folderPicker: {
    placeholder: '路径: ~/workspace/my-project',
    loading: '⏳ 加载中...',
    emptyDir: '目录为空',
    choose: '选择',
    selected: '已选择',
  },
  fullscreen: {
    enter: '⤢ 全屏',
    exit: '⤢ 退出全屏',
    enterTitle: '全屏显示输出',
    exitTitle: '退出全屏 (Esc)',
  },
  subTask: {
    // Rendered by api/client.ts subTaskCliCommand when the sub-task has no
    // session to resume yet.
    waitForSession: '# 等待主 Agent 会话就绪 (需求未启动 coding)',
  },
  subTaskPanel: {
    title: '子任务协作',
    preFlightHint: '请先在主 Agent 中执行「开始开发」，主 Agent 完成会自动派发第一批子任务。',
    preFlightCurrent: '（当前需求：{{title}}）',
    shareSession: '共享主 Agent 会话',
    countSuffix: '个子任务',
    autoOrchestrateRunning: '🪄 主 Agent 自动派发了 {{n}} 个子任务，等待执行完成并生成汇总报告…',
    reSplitRunning: '🔄 主 Agent 重新拆分任务中…',
    reSplitDone: '重新拆分结束',
    summaryTitle: '主 Agent 汇总报告',
    summaryCopyBtn: '复制汇总报告',
    summaryCopiedHint: '已复制',
    summaryExpandShow: '展开全部',
    summaryExpandCollapse: '收起',
    composerPlaceholder: '描述这个子任务要做什么…输入 @ 引用 Skill',
    modelLabel: '子任务模型',
    // Execution-environment selector in the sub-task composer. Defaults to the
    // main task's environment but can be overridden per sub-task.
    execEnvLabel: '执行环境',
    execEnvHint: '默认与主任务一致；切换到不同环境时，将从 origin 检出该需求分支执行（未推送的本地改动不会带过来）。',
    modelEmptyWarning: '⚠️ 当前 Claude 配置中没有可用模型，请前往「设置 → Claude 配置」配置后再开启子任务。',
    composerHint: '启动后子 Agent 将 fork 主会话上下文，所有子任务共享同一项目认知',
    reSplitTitleBusy: '有子任务正在执行，完成后才能重新拆分',
    reSplitTitle: '让主 Agent 重新进行任务拆分并自动派发子任务',
    reSplitBusy: '拆分中…',
    reSplitBtn: '🔄 重新拆分',
    submitBusy: '启动中…',
    submitBtn: '🚀 启动子任务',
    loading: '加载中…',
    empty: '暂无子任务。可点击「🔄 重新拆分」让主 Agent 拆分并自动派发，或在上方手动创建。',
    errLoad: '加载子任务失败',
    errCreate: '启动子任务失败',
    errReSplit: '重新拆分失败',
    stalledHint: 'Claude 流静默超时，子任务已被看门狗终止。可点击「继续」或「重新执行」重试，或检查任务是否触发了超长 IO（npm install / 大文件编辑）。',
    // Banner status texts (batch state machine — see summaryCta derivation).
    bannerSummarizing: '📝 主 Agent 正在生成汇总报告…',
    bannerSummaryFailed: '❌ 汇总报告生成失败，请点击重试',
    bannerDispatchingStatus: '🪄 主 Agent 正在派发子任务…',
    bannerAllDoneManual: '✅ 所有子任务已结束，可手动生成汇总报告。',
    // Summary CTA cluster (early / manual / progress).
    summaryCtaEarlyTitle: '跳过未完成的子任务直接生成汇总',
    summaryCtaEarlyBtn: '📝 提前生成汇总',
    summaryCtaManualTitle: '对已完成的子任务生成汇总报告',
    summaryCtaManualBtn: '📝 生成汇总',
    summaryRetryBtn: '🔄 重试汇总',
    summaryCtaProgressTitle: '汇总进行中',
    summaryCtaProgressBtn: '汇总中…',
    summarySending: '发起中…',
    summaryConfirmEarly: '将跳过未完成的子任务直接生成汇总，是否继续？',
    summaryToastOk: '✅ 汇总已发起',
    summaryToastErrPrefix: '❌',
    summaryToastErrFallback: '汇总发起失败',
    // Session-mode radio. Always visible now (was previously gated by
    // Agent-server / stale-artifact conditions). Three options let the
    // user pick the conversation-threading policy at manual sub-task
    // create time:
    //   - resume      继承主任务会话 — fork the parent coding session.
    //   - withContext 带上下文 — new session, parent context injected.
    //   - bare        新会话 — no context, no role system prompt.
    // The `freshHint` survives as the "why is the mode non-default" advisory
    // shown only when the latest sub-task artifact looks like the legacy
    // missing-jsonl bug (so a follow-up click pre-selects with_context).
    // Segmented control: labels must stay short enough to sit on ONE line in
    // a third of the composer width (long parentheticals used to force the
    // row to wrap). The per-mode explanation lives in the single
    // `.sub-session-mode-desc` line rendered under the control instead.
    sessionMode: {
      label: '会话模式',
      resume: '继承主任务会话',
      resumeHint: 'fork 主开发 session id，子任务延续主任务的对话上下文',
      withContext: '带上下文',
      withContextHint: '新建会话，把需求 / 技术方案 / 父会话近期内容拼进 prompt',
      bare: '新会话',
      bareHint: '什么都不带，也不带角色系统提示词，直接用 claude 默认',
      freshHint: '上次会话在 Agent 服务器上找不到，建议用「带上下文」模式（会自动带上需求 + 设计文档 + 上一会话最近 10 轮作为上下文）',
    },
  },
  subTaskCard: {
    statusPending: '排队中',
    statusRunning: '运行中',
    statusDone: '已完成',
    statusError: '出错',
    statusStopped: '已停止',
    // Shown instead of statusPending when the row is waiting on the project's
    // concurrency gate (no job yet) — it makes "为什么不动" answerable at a glance.
    statusQueued: '排队中 · 等待项目空闲',
    // Automatic failure redo counter (设置 → 子任务 → 失败自动重做).
    retryBadge: '🔁 自动重做 {{count}}/{{max}}',
    // Same badge when the configured cap couldn't be read (the count alone is
    // still true; inventing a denominator would not be).
    retryBadgeNoMax: '🔁 自动重做 {{count}} 次',
    retryBadgeTitle: '该子任务失败后已被系统自动重做 {{count}} 次（上限 {{max}} 次，可在 设置 → 子任务 调整）',
    secondsAgo: '{{n}}秒前',
    minutesAgo: '{{n}}分钟前',
    hoursAgo: '{{n}}小时前',
    secondsOnly: '{{n}}秒',
    minutesSeconds: '{{m}}分{{s}}秒',
    minutesOnly: '{{m}}分',
    hoursMinutes: '{{h}}小时{{m}}分',
    hoursOnly: '{{h}}小时',
    liveTicker: '⏱ {{duration}}',
    logEmpty: '⏳ 等待 Claude 输出…',
    tokenTitle: '输入 / 输出 tokens（含缓存）',
    tokenBadge: '🪙 {{cell}}',
    costTitle: '本次子任务费用',
    usageLabel: '上下文',
    usageNoData: '等待首次响应…',
    usageStepLabel: '子任务',
    usagePercentTitle: '上下文已用 {{pct}}%',
    noTitle: '（无标题）',
    noArtifact: '无产物。',
    copyCliContinue: '复制到终端继续：',
    copyCliAdjust: '从源会话 fork（仅在子任务未启动时）：',
    copyBtn: '复制',
    copied: '✓ 已复制',
    adjustToggle: '+ 追加调整',
    redoToggle: '🔄 重做',
    adjustPlaceholder: '追加的指令…输入 @ 引用 Skill',
    adjustModelLabel: '调整模型',
    adjustSubmitHint: 'Enter 发送 · Shift+Enter 换行',
    cancel: '取消',
    adjustBusy: '启动中…',
    adjustSubmit: '🚀 追加调整',
    // Redo: 重新执行该任务，新行作为子节点挂在原任务下（不再原地更新）。
    // 走一个全新的 claude session id，parent_subtask_id 指回原行。
    redoHint: '重新执行该任务，新任务作为子节点显示',
    redoModelLabel: '重做模型',
    redoSubmit: '🚀 开始重做',
    errAdjust: '追加调整失败',
    errRedo: '仅失败的任务可重做',
    // Continue: 在原会话上 --resume 续接，新行作为子节点挂在原任务下。
    // 可在 error / stopped 状态下触发。Resume 比 Redo 便宜，能继承上次运行的中间产物。
    continueToggle: '继续',
    continueHint: '在原会话上 --resume 续接，新任务作为子节点显示',
    continueSubmit: '▶ 继续执行',
    continueBusy: '续接中…',
    errContinue: '该子任务无法继续',
    // Stop: interrupt a running sub-task. Sends SIGTERM (5s → SIGKILL)
    // through JobStore and flips the row to status='stopped' with a
    // "⏹ 用户中止" artifact prefix; the previous artifact (if any)
    // is preserved under that banner so the user can still see what was
    // done before stopping.
    stopToggle: '⏹ 停止',
    stopConfirm: '确认停止当前正在执行的子任务？',
    stopping: '⏹ 停止中…',
    stopped: '已停止',
    stopRemoteDisabled: '远程 Agent 服务器执行暂不支持停止',
    errStop: '停止失败',
    // Delete: 仅对失败（error）状态的子任务开放。删除该子任务及其所有子级，
    // 并一并删除各行关联的 claude 会话文件（本地 + 远端 Agent 服务器，尽力而为）。
    deleteToggle: '🗑 删除',
    deleting: '删除中…',
    deleteConfirm: '确认删除该失败子任务及其所有子级？关联的 Claude 会话文件也会被删除，操作不可恢复。',
    errDelete: '删除失败',
    errContinueNoSession: '该子任务无法续接：原会话 id 为空',
    sourceAuto: '自动',
    sourceAutoTitle: '由主 Agent 自动派发',
    sourceManual: '手动',
    sourceManualTitle: '由用户手动创建',
    // Per-card session-mode badge (only renders for non-default modes;
    // 'fork' is intentionally hidden). Labels are short so the meta line
    // stays one row, titles hold the full explanation on hover.
    sessionModeWithContext: '带上下文',
    sessionModeWithContextTitle: '新建会话，但带上了需求 / 设计方案 / 父会话近期内容',
    // Wording matches the composer's session-mode segment label so the card
    // badge and the picker read as the same option.
    sessionModeBare: '新会话',
    sessionModeBareTitle: '新建会话，不带上下文也不带角色系统提示词，使用 claude 默认',
    // Tree 折叠/徽标文案：父卡片展示子任务数量并允许展开/收起整棵子树。
    treeExpand: '展开后续操作',
    treeCollapse: '收起后续操作',
    childCount: '{{count}} 个后续操作',
    // Planning-time execution order for orchestrated sub-tasks. The
    // ordinal is sub_tasks.batch_seq (1..N), and the count is the batch's
    // total_children so the user can see "this is step 2 of 5".
    // Renders only when batch_id is non-empty (i.e. the row belongs to
    // an orchestrated batch, not a manual sub-task).
    plannedSeq: '规划序号 {{n}} / {{total}}',
    // Surfaced when the project-level concurrency cap > 1 lets a child
    // run alongside tasks from OTHER projects. Within the same orchestrated
    // batch the queue enforces strict in-order, so the warning only
    // mentions cross-project parallelism.
    parallelWarn: '⚠️ 当前批次与其他项目子任务并发执行（项目闸门 {{n}}）',
  },
  createRequirement: {
    title: '新需求',
    collapse: '收起',
    kindAria: '需求类型',
    charCount: '{{n}} 字',
    skipOrganize: '跳过 AI 整理',
    skipOrganizeOn: '提交后直接保存原始描述，不调用 AI',
    skipOrganizeOff: '提交后让 AI 把描述整理为结构化 Markdown',
    project: '归属项目',
    projectPlaceholder: '请选择项目',
    priority: '优先级',
    flow: '开发流程',
    flowDirect: '直接开发',
    flowSkipAnalysis: '跳过分析',
    flowFull: '完整流程',
    flowNoteDirect: '小改动，创建后立即进入开发',
    flowNoteSkipAnalysis: '需求已经清楚，直接出方案再开发',
    flowNoteFull: '需求还不清楚，先和 AI 讨论清楚，再出方案再开发',
    submitHintIssue: '提交后 AI 整理为 Bug 报告：现象 / 复现步骤 / 期望行为 / 实际行为。',
    submitHintIdea: '提交后 AI 整理为灵感记录：灵感来源 / 初步设想 / 待回答的关键问题。',
    submitHintRequirement: '提交后 AI 整理为结构化文档：背景 / 目标 / 功能要点 / 验收标准。',
    shortcut: '⌘/Ctrl + Enter 提交',
    saving: '创建中…',
    savingOrganize: '创建中…AI 正在整理',
    errNeedDesc: '请填写描述',
    errNeedProject: '请选择项目',
    errCreate: '创建失败',
    launchPlan: {
      title: '⚙ 启动计划（可选）',
      hint: '默认折叠；不展开时与今天的创建行为完全一致',
      immediate: '立即执行',
      scheduled: '定时执行',
      fullFlowImmediateDisabled: '完整流程需求需先完成需求分析才能立即执行',
      fullFlowScheduledHint: '定时到点时若分析未完成将自动失败，请先完成需求分析',
      recurrence: '执行频率',
      scheduleOnce: '一次性',
      scheduleDaily: '每天',
      scheduleWeekly: '每周',
      runAt: '执行时间',
      recurTime: '执行时刻',
      recurDays: '执行星期',
      designSection: '方案段',
      codingSection: '开发段',
      submitImmediate: '创建并开始',
      submitScheduled: '创建并定时',
      launchFailed: '需求已创建，但自动启动失败：{{reason}}，请在详情页手动启动',
    },
  },

  codingChat: {
    compressTitle: '压缩开发者会话上下文',
    compressTitleHint: '📦 已压缩',
    compressConfirm: '让 Claude 总结当前对话并压缩上下文？\n\n该操作会清空当前会话 ID，下次对话将看到压缩摘要而不是完整历史。',
    compressFail: '压缩失败',
    compressSummaryFallback: '（暂无压缩摘要）',
    compressSummaryLoadFail: '（加载摘要失败）',
    composerTitle: '💬 追加调整',
    composerTooltip: '本次已发送到后端的调整请求',
  },
  devChat: {
    sentHint: '📤 已发送至后端的调整请求',
    empty: '（空）',
    userPrefix: '你追问：',
    aiPrefix: '🤖 AI：',
    thinking: '⏳ 思考中…',
    placeholder: '描述需要调整的内容，AI 会先确认理解再开始修改...\nEnter 发送  ·  Shift+Enter 换行',
    send: '发送',
    confirm: '✅ 确认，开始修改',
    stepLabel: '开发调整',
    summaryTitle: '📦 已压缩上下文摘要',
  },
  contextUsage: {
    compressLabel: '📦 压缩上下文',
    compressing: '⏳ 压缩中…',
    compressedView: '📦 已压缩 · 查看摘要',
    ariaUsage: '上下文使用量',
    rawPct: '原始使用率 {{pct}}%',
    viewSummary: '查看已压缩摘要',
    compressTitle: '让 claude 总结当前会话并清空上下文',
    overLimit: '99%+',
    overLimitTitle: '上下文已满或溢出（原始 {{pct}}%；缓存命中不计为新增）',
    breakdown: '输入 {{input}} · 缓存创建 {{cc}} · 缓存命中 {{cr}} · 窗口 {{window}}',
    breakdownNote: '百分比仅计净新增（输入 + 缓存创建）；缓存命中视为复用，不计入已用。',
  },
  sessionStrip: {
    ariaLabel: '会话上下文使用量',
    analyst: '分析师',
    design: '方案',
    coding: '开发',
    compressedBadge: '📦 已压缩',
    compressedTitle: '{{stage}}阶段已压缩',
    usageTitle: '{{stage}}: {{used}} / {{window}} tokens（原始 {{pct}}%）',
  },
} as const;

export default components;
