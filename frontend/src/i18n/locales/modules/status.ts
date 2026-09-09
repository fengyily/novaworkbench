// status — canonical display labels for every enumeration the backend stores
// as a free-form string code. The UI resolves them at render time via
// i18n/label.ts `tLabel(t, <keyMap>, code)`; the key maps themselves live in
// src/api/client.ts (statusLabelKeys / stepLabelKeys / ...) so the API layer
// stays the single place that knows the code vocabulary.
export const status = {
  // requirement lifecycle
  req: {
    draft: '📝 草稿',
    analyzing: '🔍 需求分析中',
    designing: '📐 方案设计中',
    designed: '📐 方案完成',
    developing: '🚀 开发中',
    done: '✅ 开发完成',
    archived: '📦 已归档',
  },
  // requirement kinds
  kind: {
    issue: '🐛 Issue',
    requirement: '📋 需求',
    idea: '💡 想法',
  },
  // chip labels for the cross-project list filter (问题 differs from the
  // badge short label 'Issue')
  kindFilter: {
    issue: '问题',
    requirement: '需求',
    idea: '想法',
  },
  kindShort: {
    issue: 'Issue',
    requirement: '需求',
    idea: '想法',
  },
  kindHint: {
    issue: '需要：复现路径 / 报错信息 / 期望行为',
    requirement: '需要：背景 / 目标 / 功能要点 / 验收标准',
    idea: '一句话或一段话都行，AI 会帮你评估可行性',
  },
  kindPlaceholder: {
    issue: '请描述问题现象 / 复现步骤 / 报错信息……',
    requirement: '用自然语言描述你想要实现的功能……',
    idea: '写下你的想法或灵感，AI 会帮你评估可行性……',
  },
  kindCreate: {
    issue: '🐛 创建 Issue',
    requirement: '📋 创建需求',
    idea: '💡 创建想法',
  },
  kindChatPlaceholder: {
    issue: '贴 URL、描述页面元素、报错截图，或补充复现步骤... 输入 @ 引用 Skill',
    requirement: '贴URL、描述页面元素、或回复AI的问题... 输入 @ 引用 Skill',
    idea: '说说你的疑问、顾虑或备选思路... 输入 @ 引用 Skill',
  },
  // wizard stages (stepper)
  stage: {
    analyst: '需求分析',
    architect: '技术方案',
    developer: '编码开发',
  },
  // token-usage step codes — mirrors backend service.StepLabels
  step: {
    requirement_create: '需求整理',
    analyst_chat: '需求分析',
    architect_design: '技术方案',
    refine_doc: '方案精炼',
    apply_doc: '方案应用',
    coding: '编码开发',
    adjust_coding: '追加调整',
    continue_coding: '继续开发',
    developer_chat: '开发讨论',
    merge: '合入解决',
    review: '代码审查',
  },
  // scheduled tasks
  schedule: {
    pending: '待执行',
    running: '执行中',
    succeeded: '已成功',
    failed: '已失败',
    canceled: '已取消',
    design: '生成技术方案',
    coding: '启动开发',
  },
  // agent servers
  agentServer: {
    unknown: '未知',
    checking: '检测中',
    installing: '安装中',
    ready: '可用',
    error: '异常',
  },
  // project status (active/trash) — rendered as text where needed
  project: {
    active: '使用中',
    deleted: '已删除',
  },
} as const;

export default status;
