// requirements (en-US) — must mirror modules/requirements.ts key-for-key.
export const requirements = {
  list: {
    title: '📋 Requirements',
    collapse: 'Collapse',
    create: '➕ New requirement',
    createShort: 'New requirement',
    allProjects: 'All projects',
    allStatuses: 'All statuses',
    searchPlaceholder: 'Search title or description…',
    kindFilters: {
      issue: 'Issue',
      requirement: 'Requirement',
      idea: 'Idea',
    },
    chipsNone: '(none selected — showing all)',
    chipsAll: '(showing all)',
    chipsSelected: '{{n}} kinds selected',
    loading: '⏳ Loading…',
    mobileLoading: 'Loading…',
    empty: 'No requirements yet. Use "➕ New requirement" top-right, or change the filters.',
    mobileEmpty: 'No requirements yet',
    mobileEmptyDesc: 'Tap "New requirement" bottom-right, or change the filters to see other projects.',
    colType: 'Type',
    colTitle: 'Title',
    colStatus: 'Status',
    colModel: 'Model',
    colProject: 'Project',
    colPriority: 'Priority',
    colAgentServer: 'Agent server',
    colUpdated: 'Updated',
    priority: {
      high: '🔴 High',
      medium: '🟡 Medium',
      low: '🟢 Low',
    },
    noTitle: '(untitled)',
    priorityTitle: 'Priority: {{label}}',
  },
exportDesign: {
  pdfSuffix: 'Design',
  designFallback: 'Design',
  generatedAt: 'Generated at',
},
} as const;

export default requirements;
