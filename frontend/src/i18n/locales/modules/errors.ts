// errors — translations for the stable uppercase error codes the backend
// returns in the `{success,error:{code,message}}` envelope. Codes without an
// entry here fall back to the raw server message (see tErr in i18n/label.ts).
// Only add a code when its meaning is stable and user-facing; one-off
// validation strings are not worth a key.
export const errors = {
  UNAUTHENTICATED: '未登录或会话已过期，请重新登录',
  INVALID_CREDENTIALS: '用户名或密码错误',
  USER_DISABLED: '账号已被禁用，请联系管理员',
  FORBIDDEN: '没有权限执行此操作',
  NOT_FOUND: '资源不存在或已被删除',
  PROJECT_NOT_FOUND: '项目不存在或已被删除',
  TOKEN_NOT_FOUND: '令牌不存在或已被删除',
  PATH_NOT_FOUND: '项目本地路径不存在，请在项目设置中修正或重新导入仓库',
  BAD_REQUEST: '请求格式错误',
  MISSING_FIELDS: '缺少必填字段',
  INVALID: '参数不合法',
  INVALID_REQUEST: '请求参数不合法',
  INVALID_STATUS: '状态不合法',
  INVALID_LOCALE: '不支持的语言',
  NO_SESSION: '会话不存在，请先启动对应阶段',
  INTERNAL: '服务器内部错误',
  INTERNAL_ERROR: '服务器内部错误',
  DB_ERROR: '数据库错误',
  UNKNOWN: '未知错误',
  // kind ↔ scheduled task_type gate (backend handler/schedule.go Create).
  WIKI_ONLY: '「知识库」类需求的定时任务只能是「生成知识库文档」',
  NOT_WIKI: '「生成知识库文档」定时任务仅适用于「知识库」类需求',
  WIKI_NOT_DEVELOPABLE: '「知识库」类需求不支持进入开发阶段',
  // Review 报告的结构化结果恢复路径（handler/review_reparse.go、
  // handler/review_ai_parse.go）。
  CONTRACT_UNAVAILABLE: '报告正文里既没有 JSON 契约块，也没有从分级小节中解析出问题清单',
  FINDINGS_UNAVAILABLE: '报告中的问题清单段落格式异常，无法解析',
  REVIEW_NOT_FINISHED: '本次 Review 尚未产出最终报告，结束后才能解析',
  NOT_AVAILABLE: '该功能在当前环境未启用',
  AI_PARSE_IN_PROGRESS: '本次 Review 正在 AI 解析中，请等待当前解析结束',
  NETWORK: '网络错误，请稍后重试',
} as const;

export default errors;
