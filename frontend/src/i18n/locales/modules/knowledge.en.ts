// knowledge (en-US) — must mirror modules/knowledge.ts key-for-key.
export const knowledge = {
  title: '🧠 Knowledge base',
  scanDone: 'Scan finished: {{added}} added, {{updated}} updated',
  scanFailed: 'Scan failed: {{msg}}',
  allProjects: 'All projects',
  searchPlaceholder: 'Search…',
  scan: '🔄 Scan project',
  // AI diagram button (KnowledgePage toolbar entry point).
  generateDiagram: 'Generate diagram',
  generateDiagramTitle: 'Have AI generate a Mermaid diagram and persist as a knowledge entry',
  tabMemories: 'Memories',
  tabKnowledge: 'Entries',
  tabReview: 'To review',
  loading: '⏳ Loading…',
  memoryCount: '{{n}} memories',
  addNew: '+ Add',
  memoriesEmpty: 'No memories yet — use "+ Add" or "🔄 Scan project" to generate them',
  knowledgeCount: '{{n}} entries',
  knowledgeEmpty: 'No entries yet — use "🔄 Scan project" to index automatically',
  unreviewed: 'Unreviewed',
  rejected: 'Rejected',
  sourceLabel: 'Source: {{type}}',
  edit: 'Edit',
  delete: 'Delete',
  reviewProgress: '{{index}} of {{total}}',
  reviewEditApprove: '✏️ Edit then approve',
  reviewApprove: '✅ Approve',
  reviewSkip: '⏭️ Skip',
  reviewReject: '❌ Reject',
  reviewEmpty: '🎉 Nothing left to review',
  memType: {
    business_context: 'Business context',
    technical_debt: 'Technical debt',
    design_rationale: 'Design decision',
    code_explanation: 'Code explanation',
    // Report-archive categories (主 Agent 汇总报告 + 子任务报告).
    dev_report: 'Dev report',
    subtask_report: 'Subtask report',
  },
  dialog: {
    editTitle: 'Edit memory',
    addTitle: 'Add memory',
    project: 'Project',
    projectPlaceholder: 'Select a project',
    type: 'Type',
    title: 'Title (optional)',
    content: 'Content',
    contentPlaceholder: 'e.g. Redis connections use deadpool with a pool size of 20',
    tags: 'Tags (comma separated)',
    cancel: 'Cancel',
    save: 'Save',
    saving: 'Saving…',
  },
  // view.* — reading-view page (`/knowledge/view/:id`, opened in a new tab).
  // Kept distinct from list-page strings: the list uses "To review" / "Rejected"
  // short labels; the reading view spells out the full review verdict
  // ("Pending review / Approved / Rejected") so the new tab reads as an
  // article, not a control panel.
  view: {
    openInNewTab: 'Open in new tab',
    openInNewTabHint: 'Open the full content in a reading view in a new tab',
    backToList: 'Back to knowledge base',
    closeTab: 'Close tab',
    loading: 'Loading knowledge entry…',
    loadFailed: 'Failed to load: {{msg}}',
    notFound: 'This knowledge entry no longer exists',
    noTitle: '(Untitled)',
    retry: 'Retry',
    metaSource: 'Source: {{type}}',
    metaProject: 'Project: {{id}}',
    metaCreated: 'Created {{date}}',
    metaUpdated: 'Updated {{date}}',
    reviewStatus: 'Status: {{status}}',
    statusApproved: 'Approved',
    statusPending: 'Pending review',
    statusRejected: 'Rejected',
    // PDF export (mirrors utils/exportDesignPdf.tsx). pdfLabel becomes the
    // middle segment of the downloaded filename (`<title>-knowledge.pdf`),
    // and the same string is used as the empty-filename fallback.
    pdfLabel: 'knowledge',
    exportPdfTitle: 'Export this entry to PDF (with Mermaid diagrams baked in)',
    exportPdfBtn: 'Export PDF',
    exportingPdf: 'Exporting…',
    pdfExportFailPrefix: 'PDF export failed: ',
    // Edit mode — fixes messy titles / wrong categories / stale content that
    // can come out of the scanner or AI archive pipeline.
    editBtn: 'Edit',
    editBtnTitle: 'Fix the title / category / content (override what the scanner or AI archive produced)',
    editTitleLabel: 'Title',
    editTitlePlaceholder: 'Optional — leave blank for “Untitled”',
    editCategoryLabel: 'Category',
    editContentLabel: 'Content (Markdown)',
    editContentPlaceholder: 'GFM syntax supported, including ```mermaid``` blocks',
    editContentRequired: 'Content cannot be empty',
    editCancelBtn: 'Cancel',
    editSaveBtn: 'Save',
    editSavingBtn: 'Saving…',
    editSaveFailPrefix: 'Save failed: ',
    // v0.5 dropped the previous narrow/medium/wide segmented control; the
    // 1100px wide preset still left too much gutter on big displays, so the
    // page now stretches edge-to-edge with side padding only.
    // The image toolbar (zoom / download PNG-JPG-SVG) copy lives in the shared
    // component namespace at components.markdownImage, since MarkdownRender is
    // reused app-wide.
  },
} as const;

export default knowledge;
