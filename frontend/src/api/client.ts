export const API_BASE = import.meta.env.VITE_API_BASE || 'http://localhost:9527';

// Display + persistence literal for "no specific model was selected for a
// stage" — mirrors backend handler.DefaultModelLabel. The backend treats it as
// "no --model flag" (CLI default), and the UI normalizes it to the empty
// option label "默认模型".
export const DefaultModelLabel = '默认模型';

// Token storage for the bearer auth layer. login() stores the token here; the
// request wrapper reads it on every call. On 401 the wrapper clears it and
// redirects to /login.
const TOKEN_KEY = 'nova_token';
export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) || '';
}
export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token);
}
export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY);
}

interface APIError {
  code: string;
  message: string;
  suggestion?: string;
}

interface APIResponse<T> {
  success: boolean;
  data?: T;
  error?: APIError;
}

// authHeaders returns the bearer Authorization header when a session token is
// present. Use it for any raw fetch() that bypasses request<T> (streaming/SSE,
// job snapshots) so those calls authenticate too.
export function authHeaders(): Record<string, string> {
  const token = getToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

// handleUnauthorized drops the stale token, bounces to /login, then throws so
// the caller stops. Shared by request<T> and authedFetch().
function handleUnauthorized(): never {
  clearToken();
  if (location.pathname !== '/login') {
    location.replace('/login');
  }
  throw new Error('UNAUTHENTICATED: 未登录或会话已过期，请重新登录');
}

// authedFetch wraps fetch() for call sites that need a raw Response (streaming
// SSE bodies, job snapshots) but must still send the bearer token and handle a
// 401 exactly like request<T> does.
export async function authedFetch(url: string, init?: RequestInit): Promise<Response> {
  const headers: Record<string, string> = {
    ...(init?.headers as Record<string, string> | undefined),
    ...authHeaders(),
  };
  const res = await fetch(url, { ...init, headers });
  if (res.status === 401) handleUnauthorized();
  return res;
}

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const url = `${API_BASE}${path}`;
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(options?.headers as Record<string, string> | undefined),
  };

  // authedFetch injects the bearer token and throws (after clearing the token
  // + redirecting) on 401.
  const res = await authedFetch(url, { ...options, headers });

  const json: APIResponse<T> = await res.json();

  if (!json.success || json.error) {
    const err = json.error || { code: 'UNKNOWN', message: 'Unknown error' };
    throw new Error(`${err.code}: ${err.message}${err.suggestion ? ` (${err.suggestion})` : ''}`);
  }

  return json.data as T;
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body: unknown) =>
    request<T>(path, { method: 'POST', body: JSON.stringify(body) }),
  put: <T>(path: string, body: unknown) =>
    request<T>(path, { method: 'PUT', body: JSON.stringify(body) }),
  patch: <T>(path: string, body: unknown) =>
    request<T>(path, { method: 'PATCH', body: JSON.stringify(body) }),
  delete: <T>(path: string) => request<T>(path, { method: 'DELETE' }),
};

// API endpoints
export interface Project {
  id: string;
  name: string;
  local_path: string;
  remote_url: string;
  status: string;
  default_branch: string;
  project_type: string;
  claude_files: string;
  platform_type: string;
  platform_token_id: string;
  added_at: string;
  updated_at: string;
  last_scanned_at?: string;
  deleted_at?: string;
  deleted_dir?: number;
  description: string;
  description_manual: boolean;
}

export interface DashboardData {
  total_projects: number;
  active_requirements: number;
  pending_reviews: number;
  weekly_commits: number;
  projects: Project[];
  recent_activity: { project_name: string; project_id: string; action: string; detail: string; timestamp: string }[];
}

export const projectsApi = {
  list: () => api.get<Project[]>('/api/projects'),
  get: (id: string) => api.get<Project>(`/api/projects/${id}`),
  add: (local_path: string, remote_url?: string, init_git?: boolean) =>
    api.post<Project>('/api/projects', { local_path, remote_url: remote_url || '', init_git: init_git || false }),
  // Soft-delete the project record. When delete_dir is true the local
  // directory is physically removed too (reclaimable later via restore).
  remove: (id: string, opts?: { delete_dir?: boolean }) =>
    api.delete<{ id: string; dir_deleted: boolean }>(
      `/api/projects/${id}?delete_dir=${opts?.delete_dir ? 'true' : 'false'}`,
    ),
  trash: () => api.get<Project[]>('/api/projects/trash'),
  restore: (id: string) => api.post<Project>(`/api/projects/${id}/restore`, {}),
  updatePlatform: (id: string, platform_type: string, platform_token_id: string) =>
    api.patch<Project>(`/api/projects/${id}/platform`, { platform_type, platform_token_id }),
  // Save a manually-edited description; locks it from auto-regeneration.
  updateDescription: (id: string, description: string) =>
    api.put<Project>(`/api/projects/${id}/description`, { description }),
  // Clear the manual lock and regenerate the AI summary from CLAUDE.md on demand.
  regenerateDescription: (id: string) =>
    api.post<Project>(`/api/projects/${id}/description/regenerate`, {}),
  // One-shot: generate a description for every project that lacks one.
  backfillDescriptions: () =>
    api.post<{ updated: number; skipped: number; failed: number }>(
      '/api/projects/descriptions/backfill', {},
    ),
};

export const dashboardApi = {
  get: () => api.get<DashboardData>('/api/dashboard'),
};

export interface FsItems {
  current: string;
  items: { name: string; path: string; is_dir: boolean; is_git: boolean; size: number }[];
  breadcrumb: { name: string; path: string; is_dir: boolean }[];
}

export const fsApi = {
  listDir: (path: string) => api.get<FsItems>(`/api/fs/ls?path=${encodeURIComponent(path)}`),
  validate: (path: string) => api.get<any>(`/api/fs/validate?path=${encodeURIComponent(path)}`),
};

// Memories
export interface Memory {
  id: string; project_id: string; type: string; title: string; content: string;
  source: string; file_path: string; tags: string; created_at: string; updated_at: string;
  valid_until?: string;
}
export interface MemoriesList { items: Memory[]; total: number; }

export const memoriesApi = {
  list: (params?: { project_id?: string; type?: string; search?: string; limit?: number; offset?: number }) => {
    const q = new URLSearchParams();
    Object.entries(params || {}).forEach(([k, v]) => { if (v) q.set(k, String(v)); });
    return api.get<MemoriesList>(`/api/memories?${q.toString()}`);
  },
  create: (data: { project_id: string; type: string; title: string; content: string; tags: string }) =>
    api.post<Memory>('/api/memories', data),
  update: (id: string, data: Partial<Memory>) => api.put<Memory>(`/api/memories/${id}`, data),
  delete: (id: string) => api.delete<{ status: string }>(`/api/memories/${id}`),
};

// Knowledge
export interface KnowledgeItem {
  id: string; project_id: string; title: string; content: string; category: string;
  source_type: string; source_ref: string; is_reviewed: boolean; is_approved: boolean;
  created_at: string; updated_at: string;
}
export interface KnowledgeList { items: KnowledgeItem[]; total: number; }

export const knowledgeApi = {
  list: (params?: { project_id?: string; category?: string; source_type?: string; search?: string; limit?: number; offset?: number }) => {
    const q = new URLSearchParams();
    Object.entries(params || {}).forEach(([k, v]) => { if (v) q.set(k, String(v)); });
    return api.get<KnowledgeList>(`/api/knowledge?${q.toString()}`);
  },
  search: (q: string, project_id?: string) =>
    api.get<KnowledgeItem[]>(`/api/knowledge/search?q=${encodeURIComponent(q)}${project_id ? '&project_id=' + project_id : ''}`),
  create: (data: { project_id: string; title: string; content: string; category: string; source_type?: string; source_ref?: string }) =>
    api.post<KnowledgeItem>('/api/knowledge', data),
  update: (id: string, data: Partial<KnowledgeItem>) => api.put<KnowledgeItem>(`/api/knowledge/${id}`, data),
  delete: (id: string) => api.delete<{ status: string }>(`/api/knowledge/${id}`),
  listForReview: (project_id?: string) =>
    api.get<KnowledgeItem[]>(`/api/knowledge/review/list${project_id ? '?project_id=' + project_id : ''}`),
  batchReview: (ids: string[], action: string) =>
    api.post<{ status: string }>('/api/knowledge/review/batch', { ids, action }),
};

// Scanner
export interface ScanResult {
  project_id: string; project_type: string; claude_files: string[];
  knowledge_new: number; knowledge_updated: number; files_scanned: number; duration: string;
}
export const scannerApi = {
  scan: (projectId: string) => api.post<ScanResult>(`/api/projects/${projectId}/scan`, {}),
};

// Requirements
export interface Requirement {
  id: string; project_id: string; title: string; description: string;
  status: string; priority: string; acceptance_criteria: string;
  design_docs: string; conversation_ids: string; assigned_to: string;
  created_by: string; analysis_session_id: string;
  design_session_id: string; design_job_id: string; analysis_job_id: string; apply_job_id: string; coding_session_id: string;
  skip_analysis: boolean;
  skip_design: boolean;
  branch_name?: string;
  worktree_path?: string;
  // Effective model actually dispatched to the claude CLI for each stage
  // (the --model value, or the "默认模型" literal when none was specified).
  // Empty = the stage hasn't run yet (or predates this feature).
  analyst_model?: string;
  architect_model?: string;
  developer_model?: string;
  reviewer_model?: string;
  created_at: string; updated_at: string;
  completed_at?: string;
}

export const requirementStatuses = ['draft', 'analyzing', 'designing', 'designed', 'developing', 'done'] as const;
export const statusLabels: Record<string, string> = {
  draft: '📝 草稿',
  analyzing: '🔍 需求分析中',
  designing: '📐 方案设计中',
  designed: '📐 方案完成',
  developing: '🚀 开发中',
  done: '✅ 开发完成',
  archived: '📦 已归档',
};

export const requirementsApi = {
  list: (params?: { project_id?: string; status?: string; priority?: string }) => {
    const q = new URLSearchParams();
    Object.entries(params || {}).forEach(([k, v]) => { if (v) q.set(k, String(v)); });
    return api.get<Requirement[]>(`/api/requirements?${q.toString()}`);
  },
  create: (data: { project_id: string; description: string; priority?: string; skip_analysis?: boolean; skip_design?: boolean }) =>
    api.post<Requirement>('/api/requirements', data),
  get: (id: string) => api.get<Requirement>(`/api/requirements/${id}`),
  update: (id: string, data: { title: string; description: string; priority: string; skip_analysis?: boolean }) =>
    api.put<Requirement>(`/api/requirements/${id}`, data),
  updateStatus: (id: string, status: string) =>
    api.patch<Requirement>(`/api/requirements/${id}/status`, { status }),
  delete: (id: string) => api.delete<{ status: string }>(`/api/requirements/${id}`),
  // Drop the stored claude analyst session so the next chat turn starts fresh
  // instead of resuming a broken / over-long conversation.
  clearAnalysisSession: (id: string) =>
    api.delete<{ status: string }>(`/api/requirements/${id}/analysis-session`),
  // Archive a finished ("done") requirement into the project knowledge base
  // (final requirement + design docs). Returns the created/updated knowledge row.
  archive: (id: string) =>
    api.post<KnowledgeItem>(`/api/requirements/${id}/archive`, {}),
  // Reverse archive: status returns to "done" and the knowledge entry is removed.
  unarchive: (id: string) =>
    api.post<Requirement>(`/api/requirements/${id}/unarchive`, {}),
};

export interface RunStatus {
  status: 'running' | 'done' | 'error' | 'stopped';
  job_id: string | null;
  exit_code?: number;
  log: { type: string; content: string }[];
  started_at?: string;
  finished_at?: string;
  compose_file?: string;
}

export interface RunJob {
  job_id: string;
  status: 'running' | 'done' | 'error';
  exit_code: number;
  log: { type: string; content: string }[];
  // Effective model the claude CLI ran with (display value, may be the
  // "默认模型" literal). Present for review jobs; empty for wizard jobs that
  // predate the column or were never given one.
  model?: string;
  started_at: string;
  finished_at: string;
}

export const runnerApi = {
  start: (projectId: string) =>
    api.post<{ job_id: string }>(`/api/projects/${projectId}/run/start`, {}),
  stop: (projectId: string) =>
    api.post<{ status: string }>(`/api/projects/${projectId}/run/stop`, {}),
  status: (projectId: string) =>
    api.get<RunStatus>(`/api/projects/${projectId}/run/status`),
  getJob: (jobId: string) =>
    api.get<RunJob>(`/api/wizard/jobs/${jobId}`),
};

// Merge / PR step (post-coding 合入). Local merge into a target branch with
// AI-assisted conflict resolution, or push + create-PR link. Long work runs as
// a JobStore job streamed via the shared /api/wizard/jobs/{id} endpoints.
export interface MergeState {
  is_git: boolean;
  requirement_id?: string;
  dev_branch: string;
  target_branch: string;
  uncommitted_count: number;
  uncommitted_files: string[];
  ahead: number;
  behind: number;
  has_remote: boolean;
  remote_url: string;
  platform: string;
  pr_url: string;
  mid_merge: boolean;
  conflict_files: string[];
  worktree_path?: string;
}
export const mergeApi = {
  state: (reqId: string) => api.get<MergeState>(`/api/requirements/${reqId}/merge/state`),
  local: (reqId: string, body: { target_branch?: string; commit_message?: string; delete_branch?: boolean }) =>
    api.post<{ job_id: string }>(`/api/requirements/${reqId}/merge/local`, body),
  abort: (reqId: string) => api.post<{ ok: boolean }>(`/api/requirements/${reqId}/merge/abort`, {}),
  cont: (reqId: string) => api.post<{ job_id: string }>(`/api/requirements/${reqId}/merge/continue`, {}),
  resolve: (reqId: string) => api.post<{ job_id: string }>(`/api/requirements/${reqId}/merge/resolve`, {}),
  push: (reqId: string, body: { commit_message?: string }) =>
    api.post<{ job_id: string }>(`/api/requirements/${reqId}/merge/push`, body),
  cleanup: (reqId: string, body: { force?: boolean }) =>
    api.post<{ ok: boolean }>(`/api/requirements/${reqId}/worktree/cleanup`, body),
  jobStreamUrl: (jobId: string) => `${API_BASE}/api/wizard/jobs/${jobId}/stream`,
  jobUrl: (jobId: string) => `${API_BASE}/api/wizard/jobs/${jobId}`,
};

// Pull Requests (from platform API)
export interface PR {
  number: number;
  title: string;
  body: string;
  author: string;
  head_branch: string;
  base_branch: string;
  state: string;
  html_url: string;
  updated_at: string;
}

export interface PRListResponse {
  configured: boolean;
  prs: PR[];
}

export const reviewApi = {
  listPRs: (projectId: string) =>
    api.get<PRListResponse>(`/api/projects/${projectId}/prs`),
  startReview: (projectId: string, branch: string, baseBranch: string, prNumber: number, prTitle: string, extraRequirements?: string) =>
    api.post<{ job_id: string }>(`/api/projects/${projectId}/prs/review`, {
      branch,
      base_branch: baseBranch,
      pr_number: prNumber,
      pr_title: prTitle,
      extra_requirements: extraRequirements ?? '',
    }),
  streamJobUrl: (projectId: string, jobId: string) =>
    `${API_BASE}/api/projects/${projectId}/prs/jobs/${jobId}/stream`,
  submitComment: (projectId: string, prNumber: number, body: string) =>
    api.post<{ status: string }>(`/api/projects/${projectId}/prs/${prNumber}/comment`, { body }),
};

// Platform tokens
export interface PlatformToken {
  id: string;
  name: string;
  platform: string;
  base_url: string;
  created_at: string;
  updated_at: string;
}

export const platformApi = {
  list: () => api.get<PlatformToken[]>('/api/settings/tokens'),
  create: (data: { name: string; platform: string; base_url: string; token: string }) =>
    api.post<PlatformToken>('/api/settings/tokens', data),
  delete: (id: string) => api.delete<{ status: string }>(`/api/settings/tokens/${id}`),
};

// One model of a Claude config (platform) with per-million-token unit prices.
// Binding prices to the config means each platform can carry different rates.
export interface ModelEntry {
  model: string;
  input_price: number;
  output_price: number;
}

// Claude CLI configurations (multiple named configs; the active one is
// injected as env vars into every claude subprocess, and switching it also
// re-points all roles to its default model). The auth token is never returned
// in full — only a set flag + masked preview. currency is the platform's
// accounting unit (USD/CNY); models are priced entries.
export interface ClaudeConfigItem {
  id: string;
  name: string;
  base_url: string;
  auth_token_set: boolean;
  auth_token_preview: string;
  models: ModelEntry[];
  default_model: string;
  currency: string;
  is_active: boolean;
  created_at: string;
  updated_at: string;
}
export interface ClaudeActiveModels {
  models: string[];
  default_model: string;
}
export interface ClaudeActivateResult {
  configs: ClaudeConfigItem[];
  roles_updated: boolean;
  applied_model: string;
}
export const claudeApi = {
  list: () => api.get<ClaudeConfigItem[]>('/api/settings/claude/configs'),
  create: (data: { name: string; base_url: string; auth_token?: string; models?: ModelEntry[]; default_model?: string; currency?: string }) =>
    api.post<ClaudeConfigItem>('/api/settings/claude/configs', data),
  update: (id: string, data: { name: string; base_url: string; auth_token?: string; clear_token?: boolean; models?: ModelEntry[]; default_model?: string; currency?: string }) =>
    api.put<ClaudeConfigItem>(`/api/settings/claude/configs/${id}`, data),
  remove: (id: string) => api.delete<{ status: string }>(`/api/settings/claude/configs/${id}`),
  activate: (id: string) => api.post<ClaudeActivateResult>(`/api/settings/claude/configs/${id}/activate`, {}),
  active: () => api.get<ClaudeActiveModels | null>('/api/settings/claude/configs/active'),
};

// Direct HTTP LLM channel config (OpenAI-compatible, e.g. DeepSeek). Used for
// lightweight tasks like requirement title distillation — bypasses claude CLI.
export interface LLMConfig {
  base_url: string;
  api_key_set: boolean;
  api_key_preview: string;
  model: string;
}
export const llmApi = {
  get: () => api.get<LLMConfig>('/api/settings/llm'),
  update: (data: { base_url: string; api_key?: string; model: string; clear_api_key?: boolean }) =>
    api.put<LLMConfig>('/api/settings/llm', data),
};

// Database driver config (sqlite default; mysql/postgres via settings UI or
// NOVA_DB_DRIVER/NOVA_DB_DSN env). Saving takes effect on server restart.
export interface DatabaseInfo {
  driver: string; // "sqlite" | "mysql" | "postgres"
  dsn_masked: string;
  source: string; // "env" | "file" | "default"
  sqlite_path: string;
}
export interface DatabaseConnReq {
  driver: string;
  host: string;
  port: string;
  user: string;
  password: string;
  dbname: string;
}
export interface MigrateTableStat {
  table: string;
  inserted: number;
  skipped: number;
}
export interface MigrateResult {
  tables: MigrateTableStat[];
  target_driver: string;
  restart_required: boolean;
}
export const databaseApi = {
  get: () => api.get<DatabaseInfo>('/api/settings/database'),
  test: (data: DatabaseConnReq) =>
    api.post<{ ok: boolean; version: string }>('/api/settings/database/test', data),
  save: (data: DatabaseConnReq) =>
    api.put<{ restart_required: boolean }>('/api/settings/database', data),
  migrate: () => api.post<MigrateResult>('/api/settings/database/migrate', {}),
};

// Roles (per-role system prompt + model)
export interface Role {
  id: string;
  key: string;
  name: string;
  description: string;
  system_prompt: string;
  model: string;
  sort_order: number;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface RoleUpdateResult {
  role: Role;
  warning?: string;
}
export const rolesApi = {
  list: () => api.get<Role[]>('/api/settings/roles'),
  get: (id: string) => api.get<Role>(`/api/settings/roles/${id}`),
  update: (id: string, data: { system_prompt: string; model: string }) =>
    api.put<RoleUpdateResult>(`/api/settings/roles/${id}`, data),
  reset: (id: string) => api.post<Role>(`/api/settings/roles/${id}/reset`, {}),
};

// Weekly reports (AI-generated from git log + requirement data)
export interface WeeklyReport {
  id: string;
  project_id: string;
  period_start: string;
  period_end: string;
  git_branch: string;
  git_author: string;
  rule: string;
  content: string;
  status: string;
  created_at: string;
}

export interface ReportGitInfo {
  is_git: boolean;
  current_branch: string;
  branches: string[];
  authors: string[];
}

export interface GenerateReportBody {
  period_start?: string;
  period_end?: string;
  rule?: string;
  // branch: '' = all branches; 'current' = the checked-out branch; otherwise a
  // branch name. author: '' = everyone, otherwise an --author pattern.
  branch?: string;
  author?: string;
  // Also feed each commit's full message + file stat + code diff to the model,
  // so squash-merged PRs whose subject hides several features get summarized
  // from what actually changed.
  diff_analysis?: boolean;
}

export const reportsApi = {
  list: (projectId: string) => api.get<WeeklyReport[]>(`/api/projects/${projectId}/reports`),
  getRule: (projectId: string) => api.get<{ rule: string }>(`/api/projects/${projectId}/reports/rule`),
  saveRule: (projectId: string, rule: string) =>
    api.put<{ rule: string }>(`/api/projects/${projectId}/reports/rule`, { rule }),
  // Named built-in rule templates: { standard, compact }
  rulePresets: (projectId: string) =>
    api.get<Record<string, string>>(`/api/projects/${projectId}/reports/rule-presets`),
  gitInfo: (projectId: string) => api.get<ReportGitInfo>(`/api/projects/${projectId}/reports/git-info`),
  // period_start / period_end are YYYY-MM-DD; omit both for "this week (Mon–today)".
  // rule is a per-run override (not persisted); omit to use the saved template.
  generate: (projectId: string, body: GenerateReportBody) =>
    api.post<{ job_id: string; period_start: string; period_end: string }>(
      `/api/projects/${projectId}/reports/generate`, body),
  streamJobUrl: (projectId: string, jobId: string) =>
    `${API_BASE}/api/projects/${projectId}/reports/jobs/${jobId}/stream`,
  remove: (projectId: string, reportId: string) =>
    api.delete<{ status: string }>(`/api/projects/${projectId}/reports/${reportId}`),
};

// Token usage — per-step / per-requirement / per-project aggregation. Review
// rows are recorded but never counted in requirement or project totals; they
// are surfaced separately in the project breakdown.
export interface CostItem {
  currency: string;
  amount: number;
}
export interface UsageTotals {
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  costs: CostItem[];
}
export interface StepUsage {
  step: string;
  label: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  count: number;
  costs: CostItem[];
}
export interface RequirementUsage {
  by_step: StepUsage[];
  total: UsageTotals;
}
export interface ReqUsage {
  requirement_id: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  costs: CostItem[];
}
export interface ModelUsage {
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  costs: CostItem[];
}
export interface DailyUsage {
  date: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  costs: CostItem[];
}
export interface ReviewUsage {
  id: string;
  job_id: string;
  pr_number: number;
  pr_title: string;
  branch: string;
  input_tokens: number;
  output_tokens: number;
  created_at: string;
}
export interface ProjectUsage {
  total: UsageTotals;
  by_requirement: ReqUsage[];
  by_model: ModelUsage[];
  by_day: DailyUsage[];
  review: ReviewUsage[];
}

// stepLabels mirrors backend service.StepLabels so the UI shows Chinese step
// names for any raw step code (e.g. rows recorded before a label was joined).
export const stepLabels: Record<string, string> = {
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
};

// totalInput counts cache reads/creations as billed input tokens.
export const usageTotalInput = (t: { input_tokens: number; cache_creation_tokens: number; cache_read_tokens: number }): number =>
  t.input_tokens + t.cache_creation_tokens + t.cache_read_tokens;

// currencySymbol maps a currency code to its display symbol; unknown codes show
// the code itself so a new currency never renders as a bare number.
const currencySymbol: Record<string, string> = {
  CNY: '¥',
  USD: '$',
};

// fmtCost renders a per-currency cost list as a display string. Single
// currency → "¥123.45"; multiple currencies → "¥123.45 +1" (amounts from
// different platforms are never summed); no pricing → "—".
export const fmtCost = (costs?: CostItem[]): string => {
  if (!costs || costs.length === 0) return '—';
  const symbol = currencySymbol[costs[0].currency] ?? `${costs[0].currency} `;
  const primary = `${symbol}${costs[0].amount.toFixed(2)}`;
  return costs.length > 1 ? `${primary} +${costs.length - 1}` : primary;
};

export const usageApi = {
  requirement: (id: string) => api.get<RequirementUsage>(`/api/usage/requirement/${id}`),
  byRequirement: (projectId: string) => api.get<ReqUsage[]>(`/api/usage/by-requirement?project_id=${projectId}`),
  project: (id: string) => api.get<ProjectUsage>(`/api/usage/project/${id}`),
};

// ---- Auth & RBAC ---------------------------------------------------------
// User account (RBAC). Distinct from AI-persona Role below.
export interface User {
  id: string;
  username: string;
  display_name: string;
  status: string; // active | disabled
  is_admin: boolean;
  last_login_at?: string;
  created_at: string;
  updated_at: string;
  role_ids?: string[];
  project_ids?: string[];
}

export interface Permission {
  id: string;
  key: string;
  name: string;
  module: string;
  description: string;
  sort_order: number;
  created_at: string;
}

// ACLRole = RBAC role (NOT the AI-persona Role interface above). Holds the
// permission set granted to the role.
export interface ACLRole {
  id: string;
  key: string;
  name: string;
  description: string;
  is_builtin: boolean;
  sort_order: number;
  enabled: boolean;
  created_at: string;
  updated_at: string;
  permission_ids?: string[];
  permission_keys?: string[];
  user_count?: number;
}

export interface SessionProfile {
  token: string;
  user: User;
  permissions: string[]; // union across roles; ["*"] for admin
}

export const authApi = {
  login: (username: string, password: string) =>
    api.post<SessionProfile>('/api/auth/login', { username, password }),
  logout: () => api.post<{ status: string }>('/api/auth/logout', {}),
  me: () => api.get<SessionProfile>('/api/auth/me'),
};

export const aclApi = {
  // users
  listUsers: () => api.get<User[]>('/api/acl/users'),
  createUser: (data: { username: string; password: string; display_name?: string; status?: string; is_admin?: boolean; role_ids?: string[]; project_ids?: string[] }) =>
    api.post<User>('/api/acl/users', data),
  getUser: (id: string) => api.get<User>(`/api/acl/users/${id}`),
  updateUser: (id: string, data: Partial<{ display_name: string; password: string; status: string; is_admin: boolean; role_ids: string[]; project_ids: string[] }>) =>
    api.put<User>(`/api/acl/users/${id}`, data),
  deleteUser: (id: string) => api.delete<{ status: string }>(`/api/acl/users/${id}`),
  assignProjects: (id: string, project_ids: string[]) =>
    api.put<User>(`/api/acl/users/${id}/projects`, { project_ids }),
  // roles
  listRoles: () => api.get<ACLRole[]>('/api/acl/roles'),
  createRole: (data: { key: string; name: string; description?: string; sort_order?: number; enabled?: boolean; permission_ids?: string[] }) =>
    api.post<ACLRole>('/api/acl/roles', data),
  updateRole: (id: string, data: Partial<{ name: string; description: string; sort_order: number; enabled: boolean; permission_ids: string[] }>) =>
    api.put<ACLRole>(`/api/acl/roles/${id}`, data),
  deleteRole: (id: string) => api.delete<{ status: string }>(`/api/acl/roles/${id}`),
  // permissions
  listPermissions: () => api.get<Permission[]>('/api/acl/permissions'),
};

// hasPermission checks a permission set (from SessionProfile) for a key. The
// "*" wildcard (admin) grants everything.
export function hasPermission(perms: string[] | undefined, key: string): boolean {
  if (!perms) return false;
  return perms.includes('*') || perms.includes(key);
}

// Preflight — runtime dependency check + auto-install. The JSON wrapper
// handles snapshot + install-start; install progress streams over SSE so the
// UI uses raw fetch + EventSource (see SettingsPreflight).
export interface PreflightDep {
  key: string;
  label: string;
  installed: boolean;
  version: string;
  path: string;
  required: boolean;
  depends_on: string[];
  err: string;
  manual: string;
}
export interface PreflightSnapshot {
  deps: PreflightDep[];
  claude_bin: string;
  autoinstall: boolean;
}
export const preflightApi = {
  snapshot: () => api.get<PreflightSnapshot>('/api/preflight'),
  install: (key: string) => api.post<{ job_id: string }>('/api/preflight/install', { key }),
  installStreamUrl: (jobId: string) => `${API_BASE}/api/preflight/jobs/${jobId}/stream`,
};
