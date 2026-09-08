// requirements — the cross-project requirements list and the requirement
// detail page (header, stage panels, usage tables, merge panel...).
export const requirements = {
  list: {
    title: '📋 需求列表',
    collapse: '收起',
    create: '➕ 新建需求',
    createShort: '新建需求',
    allProjects: '全部项目',
    allStatuses: '全部状态',
    searchPlaceholder: '搜索标题或描述...',
    kindFilters: {
      issue: '问题',
      requirement: '需求',
      idea: '想法',
    },
    chipsNone: '（未选，显示全部）',
    chipsAll: '（显示全部）',
    chipsSelected: '已选 {{n}} 类',
    loading: '⏳ 加载中...',
    mobileLoading: '加载中...',
    empty: '暂无需求。点击右上角「➕ 新建需求」开始，或切换筛选条件。',
    mobileEmpty: '还没有需求',
    mobileEmptyDesc: '点击右下角「新建需求」开始，或切换筛选条件查看其它项目。',
    colType: '类型',
    colTitle: '标题',
    colStatus: '状态',
    colModel: '模型名称',
    colProject: '项目',
    colPriority: '优先级',
    colAgentServer: 'Agent 服务器',
    colUpdated: '更新时间',
    priority: {
      high: '🔴 High',
      medium: '🟡 Medium',
      low: '🟢 Low',
    },
    noTitle: '（无标题）',
    priorityTitle: '优先级: {{label}}',
  },
exportDesign: {
  pdfSuffix: '技术方案',
  designFallback: '技术方案',
  generatedAt: '生成于',
},
} as const;

export default requirements;
