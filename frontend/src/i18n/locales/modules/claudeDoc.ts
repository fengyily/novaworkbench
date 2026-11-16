// claudeDoc — the project-level CLAUDE.md panel on the project detail page.
//
// The document is a managed project asset (stored in the DB, injected into
// the AI stages), so the wording here is about "this project's conventions"
// rather than about a file in the repository.
export const claudeDoc = {
  title: '项目 CLAUDE.md',
  loading: '加载中...',

  // Empty state
  emptyTitle: '还没有为该项目生成 CLAUDE.md。',
  emptyHint: '生成后，需求分析 / 方案设计 / 开发实现 / 子任务都会自动带上它，模型不必每次重新摸索项目结构。',

  // Header badges
  sourceLabel: '来源',
  sourceGenerated: 'AI 生成',
  sourceManual: '手工编辑',
  sourceImported: '磁盘导入',
  lastGenerated: '最后生成：{{time}}',
  neverGenerated: '尚未生成过',
  sizeLabel: '{{n}} 字节',

  // Disk hint
  diskExists: '仓库目录中已存在 CLAUDE.md，Claude CLI 会自动加载它；本面板的版本为准。',
  diskMissing: '仓库目录中没有 CLAUDE.md。',
  importFromDisk: '从磁盘导入',

  // Actions
  generate: '🚀 生成 CLAUDE.md',
  regenerate: '重新生成',
  edit: '编辑',
  save: '保存',
  saving: '保存中...',
  cancel: '取消',
  viewSource: '查看源码',
  viewRendered: '渲染视图',
  remove: '删除',
  removeConfirm: '确定删除该项目级 CLAUDE.md 吗？删除后需求分析 / 设计 / 开发将不再自动携带项目约定。',
  savedOk: '已保存 ✓',

  // Generation
  focusLabel: '生成重点（可选）',
  focusPlaceholder: '例如：重点关注后端分层与数据库迁移',
  generating: '生成中...',
  progressTitle: '生成过程',
  clearLog: '清除',
  generateFailed: '生成失败',

  // Editing
  editHint: '手工编辑不会改变「最后生成时间」。',
};
