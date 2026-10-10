// knowledge — the knowledge base page (memories / entries / review queue +
// the memory dialog).
export const knowledge = {
  title: '🧠 知识库',
  scanDone: '扫描完成: 新增 {{added}} 条, 更新 {{updated}} 条',
  scanFailed: '扫描失败: {{msg}}',
  allProjects: '全部项目',
  searchPlaceholder: '搜索...',
  scan: '🔄 扫描项目',
  // AI 出图入口 (从知识库工具栏打开) —— 翻译键统一加 generateDiagram* 前缀。
  generateDiagram: '生成架构图',
  generateDiagramTitle: '通过 AI 生成一张 Mermaid 架构图并落库为知识条目',
  tabMemories: '记忆',
  tabKnowledge: '知识条目',
  tabReview: '待Review',
  loading: '⏳ 加载中...',
  memoryCount: '{{n}} 条记忆',
  addNew: '+ 新增',
  memoriesEmpty: '暂无记忆，点击「+ 新增」或「🔄 扫描项目」来自动生成',
  knowledgeCount: '{{n}} 条知识',
  knowledgeEmpty: '暂无知识条目，点击「🔄 扫描项目」来自动索引',
  unreviewed: '未Review',
  rejected: '已驳回',
  sourceLabel: '来源: {{type}}',
  edit: '编辑',
  delete: '删除',
  reviewProgress: '第 {{index}} 条 / 共 {{total}} 条',
  reviewEditApprove: '✏️ 编辑后确认',
  reviewApprove: '✅ 确认',
  reviewSkip: '⏭️ 跳过',
  reviewReject: '❌ 拒绝',
  reviewEmpty: '🎉 没有待审核的知识条目',
  memType: {
    business_context: '业务背景',
    technical_debt: '技术债务',
    design_rationale: '设计决策',
    code_explanation: '代码说明',
    // Report-archive categories (主 Agent 汇总报告 + 子任务报告).
    dev_report: '开发报告',
    subtask_report: '子任务报告',
  },
  dialog: {
    editTitle: '编辑记忆',
    addTitle: '新增记忆',
    project: '项目',
    projectPlaceholder: '选择项目',
    type: '类型',
    title: '标题 (可选)',
    content: '内容',
    contentPlaceholder: '例如: Redis 连接池使用 deadpool，最大连接数 20',
    tags: '标签 (逗号分隔)',
    cancel: '取消',
    save: '保存',
    saving: '保存中...',
  },
  // view.* — 阅读视图 (`/knowledge/view/:id`，新窗口打开) 专用文案。
  // 独立 key 避免与列表页文案混用 (列表页用「待审」「未Review」，
  // 这里给阅读视图更明确的「待审核 / 已通过审核 / 已驳回」措辞)。
  view: {
    openInNewTab: '在新标签页打开',
    openInNewTabHint: '在新标签页中以阅读视图查看完整内容',
    backToList: '返回知识库',
    closeTab: '关闭标签页',
    loading: '正在加载知识条目…',
    loadFailed: '加载失败：{{msg}}',
    notFound: '知识条目不存在或已被删除',
    noTitle: '（无标题）',
    retry: '重试',
    metaSource: '来源：{{type}}',
    metaProject: '项目 ID：{{id}}',
    metaCreated: '创建于 {{date}}',
    metaUpdated: '最后更新 {{date}}',
    reviewStatus: '状态：{{status}}',
    statusApproved: '已通过审核',
    statusPending: '待审核',
    statusRejected: '已驳回',
    // PDF 导出（与 utils/exportDesignPdf.tsx 配合） —— pdfLabel 用作
    // 下载文件名的中段（如 "<title>-知识条目.pdf"），fallback 同样如此；
    // pdfExportFailPrefix 与 requirements.detail2.pdfExportFailPrefix 保持
    // 同款前缀风格「导出失败：...」，便于统一排障。
    pdfLabel: '知识条目',
    exportPdfTitle: '将本知识条目导出为 PDF（包含 Mermaid 图）',
    exportPdfBtn: '导出 PDF',
    exportingPdf: '导出中…',
    pdfExportFailPrefix: '导出失败：',
    // 标题 / 内容 / 分类可编辑（覆盖扫描器 + AI 归档偶发的脏数据）。
    editBtn: '编辑',
    editBtnTitle: '修正标题 / 分类 / 内容（覆盖扫描器或 AI 归档的不规范结果）',
    editTitleLabel: '标题',
    editTitlePlaceholder: '可空，留空则用“无标题”',
    editCategoryLabel: '分类',
    editContentLabel: '内容（Markdown）',
    editContentPlaceholder: '支持 GFM 语法 + ```mermaid``` 代码块',
    editContentRequired: '内容不能为空',
    editCancelBtn: '取消',
    editSaveBtn: '保存',
    editSavingBtn: '保存中…',
    editSaveFailPrefix: '保存失败：',
    // v0.5 之前曾有 "窄/中/宽" 三档切换，后取消（最大档在大屏两侧留白
    // 仍过多）。图片工具栏（放大 / 下载 PNG-JPG-SVG）的文案在共享组件
    // 命名空间 components.markdownImage 下，因为 MarkdownRender 被全应用复用。
  },
} as const;

export default knowledge;
