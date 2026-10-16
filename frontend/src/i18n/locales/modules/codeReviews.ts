// codeReviews — strings for the project Code Review tab, its composer
// toggle, and the standalone /reviews/:id reading window. The view-page
// strings (topbar close button, loading / not-found copy) are referenced
// from CodeReviewViewPage; history.openDraftHint feeds the row tooltip
// that hints at the click-to-edit behaviour on draft rows.
export const codeReviews = {
  page: {
    title: '代码 Review',
  },
  create: {
    toggleOpen: '新建 Code Review',
    toggleClose: '收起表单',
  },
  view: {
    loading: '加载中…',
    backToProject: '返回项目',
    notFound: '未找到该 Code Review',
    projectFallback: '无法确定所属项目',
  },
  history: {
    openDraftHint: '此为草稿，点击回到编辑器继续',
    openViewerHint: '打开详情',
    purgeBlankDrafts: '清理 {{n}} 个空白草稿',
  },
};
