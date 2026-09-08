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
  NETWORK: '网络错误，请稍后重试',
} as const;

export default errors;
