// login — the auth page.
export const login = {
  title: '登录',
  subtitle: '用户角色权限体系已启用，请使用账号登录。',
  username: '用户名',
  password: '密码',
  submit: '登录',
  submitting: '登录中…',
  failed: '登录失败',
  hintPrefix: '首次启动时管理员账号由系统自动创建，密码打印在后端启动日志中（',
  hintSuffix: '）。',
} as const;

export default login;
