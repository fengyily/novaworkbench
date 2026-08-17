export const API_BASE = import.meta.env.VITE_API_BASE || 'http://localhost:9527';

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

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const url = `${API_BASE}${path}`;
  const res = await fetch(url, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      ...options?.headers,
    },
  });

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
  sprint: string; created_by: string; analysis_session_id: string;
  design_session_id: string; design_job_id: string; analysis_job_id: string; apply_job_id: string; coding_session_id: string;
  skip_analysis: boolean;
  created_at: string; updated_at: string;
  completed_at?: string;
}

export const requirementStatuses = ['draft', 'analyzing', 'designing', 'designed', 'developing', 'done'] as const;
export const statusLabels: Record<string, string> = {
  draft: '📝 草稿',
  analyzing: '🔍 需求分析中',
  designing: '📐 方案设计中',
  designed: '✅ 方案完成',
  developing: '🚀 开发中',
  done: '✅ 开发完成',
  archived: '📦 已归档',
};

export const requirementsApi = {
  list: (params?: { project_id?: string; status?: string; priority?: string; sprint?: string }) => {
    const q = new URLSearchParams();
    Object.entries(params || {}).forEach(([k, v]) => { if (v) q.set(k, String(v)); });
    return api.get<Requirement[]>(`/api/requirements?${q.toString()}`);
  },
  create: (data: { project_id: string; description: string; priority?: string; sprint?: string; skip_analysis?: boolean }) =>
    api.post<Requirement>('/api/requirements', data),
  get: (id: string) => api.get<Requirement>(`/api/requirements/${id}`),
  update: (id: string, data: { title: string; description: string; priority: string; sprint: string; skip_analysis?: boolean }) =>
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

// Claude CLI configuration (auth token + base URL, injected as env vars)
export interface ClaudeConfig {
  anthropic_auth_token_set: boolean;
  anthropic_auth_token_preview: string;
  anthropic_base_url: string;
}
export const claudeApi = {
  get: () => api.get<ClaudeConfig>('/api/settings/claude'),
  update: (data: { anthropic_auth_token?: string; anthropic_base_url: string; clear_token?: boolean }) =>
    api.put<ClaudeConfig>('/api/settings/claude', data),
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

export const rolesApi = {
  list: () => api.get<Role[]>('/api/settings/roles'),
  get: (id: string) => api.get<Role>(`/api/settings/roles/${id}`),
  update: (id: string, data: { system_prompt: string; model: string }) =>
    api.put<Role>(`/api/settings/roles/${id}`, data),
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
