// API_BASE defaults to empty (same-origin) so the embedded SPA works on
// any host without a build-time VITE_API_BASE override. When empty, every
// `api.*` call uses a relative path "/api/..." which the browser resolves
// against the page's own origin. In dev (vite serves the SPA on :5173 with
// /api proxied to :9527) this also works transparently. To point the UI
// at a different backend, set VITE_API_BASE=http://other-host:9527 at build
// time (vite inlines it).
import i18next from 'i18next';

export const API_BASE = import.meta.env.VITE_API_BASE || '';

// Display + persistence literal for "no specific model was selected for a
// stage" — mirrors backend handler.DefaultModelLabel. The backend treats it as
// "no model pinned in the --settings env block" (CLI default), and the UI
// normalizes it to the empty option. This is a PROTOCOL literal, not UI copy:
// it is round-tripped through the backend and compared with `===`, so it must
// stay in Chinese regardless of the UI language. Do NOT run it through i18n.
// i18n: protocol literal
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

// ApiError carries the structured envelope error so UI code can translate
// the stable `code` (see utils/errMsg.ts) instead of showing the raw server
// message. It still IS an Error whose `message` reads like the old thrown
// string ("CODE: message (suggestion)"), so any call site that never learned
// about ApiError keeps rendering exactly what it rendered before.
export class ApiError extends Error {
  readonly code: string;
  // detail is the server's raw message without the code prefix — used as the
  // fallback when a code has no translation.
  readonly detail: string;
  readonly suggestion?: string;

  constructor(code: string, message: string, suggestion?: string) {
    super(`${code}: ${message}${suggestion ? ` (${suggestion})` : ''}`);
    this.name = 'ApiError';
    this.code = code;
    this.detail = message;
    this.suggestion = suggestion;
  }
}

// authHeaders returns the bearer Authorization header when a session token is
// present. Use it for any raw fetch() that bypasses request<T> (streaming/SSE,
// job snapshots) so those calls authenticate too.
export function authHeaders(): Record<string, string> {
  const token = getToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

// handleUnauthorized drops the stale token, bounces to /login, then throws so
// the caller stops. Shared by request<T> and authedFetch(). The thrown
// ApiError's code (UNAUTHENTICATED) is what the UI translates; the detail
// here is only the untranslated fallback.
function handleUnauthorized(): never {
  clearToken();
  if (location.pathname !== '/login') {
    location.replace('/login');
  }
  throw new ApiError('UNAUTHENTICATED', 'Not signed in or the session has expired');
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
    throw new ApiError(err.code, err.message, err.suggestion);
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
  add: (req: {
    local_path?: string;
    remote_url?: string;
    init_git?: boolean;
    branch?: string;
    platform_type?: string;
    platform_token_id?: string;
  }) => api.post<Project>('/api/projects', {
    local_path: req.local_path || '',
    remote_url: req.remote_url || '',
    init_git: req.init_git || false,
    branch: req.branch || '',
    platform_type: req.platform_type || '',
    platform_token_id: req.platform_token_id || '',
  }),
  // Soft-delete the project record. When delete_dir is true the local
  // directory is physically removed too (reclaimable later via restore).
  remove: (id: string, opts?: { delete_dir?: boolean }) =>
    api.delete<{ id: string; dir_deleted: boolean }>(
      `/api/projects/${id}?delete_dir=${opts?.delete_dir ? 'true' : 'false'}`,
    ),
  trash: () => api.get<Project[]>('/api/projects/trash'),
  restore: (id: string) => api.post<Project>(`/api/projects/${id}/restore`, {}),
  // Permanently delete a soft-deleted project. Refuses on active rows
  // (the backend returns NOT_IN_TRASH); pair with projectsApi.remove to
  // first soft-delete then purge.
  purge: (id: string) => api.delete<{ id: string; status: string }>(`/api/projects/${id}/purge`),
  updatePlatform: (id: string, platform_type: string, platform_token_id: string) =>
    api.patch<Project>(`/api/projects/${id}/platform`, { platform_type, platform_token_id }),
  // Edit the project name / remote URL / type / local path / default branch.
  // All five fields are optional — omit any field to leave it unchanged.
  // The backend rejects blank name/path with 400 INVALID_NAME /
  // INVALID_LOCAL_PATH and a duplicate path with 409 DUPLICATE_LOCAL_PATH.
  // default_branch is the project's configured main branch (e.g. "main" /
  // "master" / "develop"); the wizard pipeline falls back to "main" when
  // blank, so passing "" is a valid way to reset to the legacy default.
  updateBasicInfo: (id: string, data: { name?: string; remote_url?: string; project_type?: string; local_path?: string; default_branch?: string }) =>
    api.patch<Project>(`/api/projects/${id}`, data),
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

// Sub-tasks: manually-triggered child agents that fork a requirement's main
// session (coding_session_id). Shown in the developer stage's SubTaskPanel.
// The status field mirrors JobStore job status (pending/running/done/error);
// `artifact` holds the final Markdown report and survives JobStore eviction.
export type SubTaskStatus = 'pending' | 'running' | 'done' | 'error' | 'stopped';

export interface SubTask {
  id: string;
  requirement_id: string;
  title: string;
  prompt: string;
  status: SubTaskStatus;
  session_id: string;
  source_session_id: string;
  job_id: string;
  artifact: string;
  model: string;
  // Terminal token usage as recorded on Finish (zero until then). The
  // header badge combines these into a single "12.4k↓ / 3.1k↑" readout
  // without a second token_usage SELECT, so the badge stays cheap to
  // render for every visible card.
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  // Resolved cost in cents (USD-equivalent of the platform's currency).
  // Zero when the active claude config has no unit price for the model
  // yet — the badge then hides the cost cell entirely.
  cost_cents: number;
  // Wall-clock duration from MarkRunning → Finish. Zero while running;
  // the SubTaskCard renders a live ticker (every second) until then.
  duration_seconds: number;
  created_at: string;
  updated_at: string;
  completed_at?: string;
  // Orchestration batch membership (restartable orchestration redesign).
  // batch_id is the parent orchestration_batches.id for auto-dispatched children;
  // empty for both manually-created sub-tasks AND legacy rows predating the
  // source column. batch_seq is the 1-based dispatch order within the batch.
  batch_id?: string;
  batch_seq?: number;
  // Provenance marker: 'auto' = tryAutoOrchestrate 派发；'manual' = 手动创建。
  // 只读字段，后端写入后前端仅展示，不修改。向后兼容：旧后端不返回此字段时为 undefined。
  source?: 'manual' | 'auto';
  // Execution environment for this sub-task: '' (or undefined on legacy
  // backends) = 本地; a non-empty agent_servers.id = runs on that Agent
  // Server. agent_server_name is the resolved display name (join, read-only).
  // This is the RAW column value — it records whether the user overrode the
  // default (see the composer's "已覆盖默认" hint), NOT where the sub-task
  // will actually run.
  agent_server_id?: string;
  agent_server_name?: string;
  // Resolved at read time by the server (attachEffectiveEnv): the environment
  // this sub-task will ACTUALLY run in. It already carries the legacy-NULL
  // fallback (a row created before the column existed inherits the parent
  // requirement's agent_server_id), while an explicit 本地 choice stays empty.
  // 服务端解析后的生效环境，已是最终值，前端不要再自行回退 — render the badge and
  // derive Stop availability from THESE fields, not from agent_server_id.
  effective_agent_server_id?: string;
  effective_agent_server_name?: string;
}

export const subTasksApi = {
  // Start a child agent. Returns the JobStore job_id (for SSE stream) and
  // the sub_task_id (for refetch / list updates).
  //
  // freshSession opts into the 「新会话（含需求上下文）」 recovery path:
  // the backend skips --resume, mints a new claude session id, and
  // prepends a ## 父任务上下文 block (requirement title / design /
  // recent turns / sibling digests) so the new session can answer the
  // instruction without the parent's JSONL. Used when the user saw
  // "源会话已失效" and chose the radio opt-in. The default (false)
  // preserves the legacy --fork-session path.
  // agent_server_id selects the execution environment. Omit the field to
  // inherit the parent requirement's environment (default); pass '' to force
  // 本地, or an agent_servers.id to run on that server.
  create: (
    requirementId: string,
    data: { prompt: string; title?: string; model?: string; freshSession?: boolean; agent_server_id?: string },
  ) => api.post<{ job_id: string; sub_task_id: string }>(`/api/requirements/${requirementId}/sub-tasks`, data),
  // List all sub-tasks for a requirement (oldest first).
  list: (requirementId: string) =>
    api.get<SubTask[]>(`/api/requirements/${requirementId}/sub-tasks`),
  // Manual re-split: resumes the coding session with the
  // decomposition trigger and runs the same parse+dispatch pipeline as
  // StartCoding's auto-orchestrate. Returns a job_id — subscribe to
  // /api/wizard/jobs/{job_id}/stream for the main agent's progress; the
  // dispatched children appear in the sub-tasks list afterwards.
  // 409 when a child is still running.
  reOrchestrate: (requirementId: string, data?: { model?: string }) =>
    api.post<{ job_id: string }>(`/api/requirements/${requirementId}/re-orchestrate`, data ?? {}),
  // Fetch one sub-task (incl. artifact Markdown). 404 when the id doesn't
  // belong to the requirement.
  get: (requirementId: string, subTaskId: string) =>
    api.get<SubTask>(`/api/requirements/${requirementId}/sub-tasks/${subTaskId}`),
  // Append a follow-up instruction to an existing sub-task. Resumes the
  // parent's claude session via --fork-session so the child inherits the
  // parent's prior edits + transcript. Returns a new job_id/sub_task_id
  // (the adjustment itself is a new sub-task row).
  adjust: (requirementId: string, subTaskId: string, data: { prompt: string; model?: string }) =>
    api.post<{ job_id: string; sub_task_id: string }>(
      `/api/requirements/${requirementId}/sub-tasks/${subTaskId}/adjust`,
      data,
    ),
  // Redo a failed sub-task IN PLACE: re-runs the SAME prompt on the same
  // sub_tasks row (no new row created), forking the requirement's main
  // coding_session_id with a brand-new claude session id, optionally with
  // a different model. reused_session is always true on the redo path
  // (the row survives; the session does not).
  redo: (requirementId: string, subTaskId: string, data: { model?: string }) =>
    api.post<{ job_id: string; sub_task_id: string; reused_session: boolean }>(
      `/api/requirements/${requirementId}/sub-tasks/${subTaskId}/redo`,
      data,
    ),
  // Continue a sub-task IN PLACE on its existing claude session via
  // --resume (no fork). Reused for both 'error' and 'stopped' rows; the
  // original artifact is preserved until the new result lands. Returns
  // reused_session=false (the existing session is reused end-to-end).
  continue: (requirementId: string, subTaskId: string, data: { model?: string } = {}) =>
    api.post<{ job_id: string; sub_task_id: string; reused_session: boolean }>(
      `/api/requirements/${requirementId}/sub-tasks/${subTaskId}/continue`,
      data,
    ),
  // Stop a running sub-task: triggers a SIGTERM (5s → SIGKILL fallback)
  // on the underlying claude process and flips the row to status='stopped'
  // with a "⏹ 用户中止" artifact prefix. Returns immediately with
  // {status: "stopping", ...}; the actual state flip surfaces via the
  // SSE job_done frame.
  stop: (requirementId: string, subTaskId: string) =>
    api.post<{ status: 'stopping'; sub_task_id: string; job_id: string }>(
      `/api/requirements/${requirementId}/sub-tasks/${subTaskId}/stop`,
      {},
    ),
  // Auto-orchestrate: ask the developer main agent to decompose + dispatch.
  // Returns the main-agent's reply (sentinel-stripped) + ids of the
  // children it just spawned. Each child's progress streams via the
  // existing /api/wizard/jobs/{jobId}/stream endpoint (use those job_ids
  // on the response to subscribe). The summary Markdown appears on the
  // next GET of the requirement (see requirements.coding_plan).
  orchestrate: (requirementId: string, data: { user_message: string; model?: string }) =>
    api.post<SubTaskOrchestrateResponse>(
      `/api/requirements/${requirementId}/orchestrate`, data,
    ),
  // Manual summary round: kick the orchestration queue to summarize
  // already-finished sub-tasks (used when the user wants to skip the
  // pending ones, or when the auto-orchestration path wasn't taken —
  // e.g. manually-added sub-tasks). The backend creates a
  // summarizing-only orchestration_batches row and dispatches it through
  // the same OrchestrationQueue tick path as the auto flow. Returns the
  // new batch_id so the panel can poll its status. 400 when sub-tasks
  // are still pending/running; 409 when an orchestration batch is already
  // active on this requirement.
  generateSummary: (requirementId: string, data?: { model?: string }) =>
    api.post<{ job_id: string; batch_id: string }>(
      `/api/requirements/${requirementId}/sub-tasks/summary`, data ?? {},
    ),
  // Poll the current orchestration batch for a requirement. Returns null
  // when the backend has no batch row for it (no orchestration has run,
  // or the backend predates the feature). The catch returns plain `null`
  // so callers can treat any failure as "no batch" without wrapping
  // every call site in try/catch or unwrapping the envelope by hand.
  getOrchestrationBatch: (requirementId: string) =>
    api.get<OrchestrationBatch | null>(
      `/api/requirements/${requirementId}/orchestration/batch`,
    ).catch((): OrchestrationBatch | null => null),
};

// SubTaskOrchestrateResponse is what the backend returns from POST
// /orchestrate. sub_task_ids are the children the orchestrator created in
// this call; the caller subscribes to each child's
// /api/wizard/jobs/{id}/stream to see live progress. job_id is the
// orchestrator's own JobStore job (no public stream for it — the main
// agent's text is folded into the children + the next coding_plan
// refresh). plan_id (optional) is the orchestrator's forked session id.
export interface SubTaskOrchestrateResponse {
  job_id: string;
  sub_task_ids: string[];
  plan_id?: string;
  // Orchestration batch that the orchestrator just created (new flow).
  // The children in sub_task_ids all share this batch_id, and the queue
  // ticks them in ascending batch_seq order. Older backends that predate
  // the batch refactor leave these undefined — callers should fall back
  // to sub_task_ids alone.
  batch_id?: string;
  total_children?: number;
}

// OrchestrationBatch is the persistence-side record of one orchestrator
// run (auto-decompose or manual summary). The frontend reads it to
// decide which CTAs to show in SubTaskPanel: while status='dispatching'
// the "📝 提前生成汇总" CTA is offered; while status='summarizing' all
// controls are disabled. summary_status independently tracks the
// post-dispatch summary round (fork-session resume writing the
// requirements.coding_plan), so a batch in 'completed' status always
// has summary_status='done' as well.
export interface OrchestrationBatch {
  id: string;
  requirement_id: string;
  status: 'dispatching' | 'summarizing' | 'completed' | 'errored';
  summary_status: 'pending' | 'running' | 'done' | 'error';
  total_children: number;
  model: string;
  created_at: string;
  updated_at?: string;
  completed_at?: string | null;
}

// CLI command constructor — renders the `claude ...` invocation the user
// can paste into an external terminal to continue a sub-task's session
// outside Nova. The exact command depends on whether the session has been
// forked yet: a fresh sub-task that already ran needs --resume <sid> (not
// --fork-session) so the user continues *that* session verbatim. settings
// (from claudeSettingsPrefix) is prepended so the pasted command hits the
// same model + base URL Nova launches with. The command is a UI hint, not
// executed by Nova — the user copies and runs it in their own shell.
export function subTaskCliCommand(st: SubTask, settings = ''): string {
  const prefix = settings ? `${settings} ` : '';
  const sid = (st.session_id || '').trim();
  if (!sid) {
    // Sub-task never spawned yet — show the placeholder command the user
    // would issue from the parent's session to fork a new one.
    const parent = (st.source_session_id || '').trim();
    if (parent) {
      return `claude ${prefix}--resume ${parent} --fork-session --session-id <new-uuid> -p "${escapeForShell(st.prompt)}"`;
    }
    // Resolved at call time (render) so a language switch updates the hint.
    return i18next.t('components.subTask.waitForSession');
  }
  // Standard continue command — matches the canonical "claude -r"
  // short-flag the CLI accepts.
  return `claude ${prefix}--resume ${sid}`;
}

// subTaskAdjustCommand: paste-able --fork-session resume that continues
// this sub-task's session with a new instruction. Useful after the user
// applies an AdjustSubTask round inside Nova and wants to keep iterating
// from their own terminal.
export function subTaskAdjustCommand(st: SubTask, nextPrompt: string, settings = ''): string {
  const prefix = settings ? `${settings} ` : '';
  const sid = (st.session_id || '').trim();
  if (!sid) return subTaskCliCommand(st, settings);
  return `claude ${prefix}--resume ${sid} --fork-session --session-id <new-uuid> -p "${escapeForShell(nextPrompt)}"`;
}

// escapeForShell quotes the prompt for inclusion in a bash/zsh single
// command. Keeps things conservative (single quotes, escape any inner '
// as '\''), so a multi-line prompt pasted into a terminal stays parseable.
function escapeForShell(s: string): string {
  return s.replace(/'/g, `'\\''`).replace(/\n/g, ' ');
}

// Requirements
export type Kind = 'issue' | 'requirement' | 'idea';

export interface Requirement {
  id: string; project_id: string; title: string; description: string;
  status: string; priority: string; kind?: Kind;
  acceptance_criteria: string;
  design_docs: string; conversation_ids: string; assigned_to: string;
  created_by: string; analysis_session_id: string;
  // SourceRequirementID links this row to the requirement it was promoted
  // from (typically an idea whose discussion was summarized into a brand-new
  // requirement). Empty for directly-created rows or rows that predate this
  // column. Rendered as a "← source: <title>" link in the detail header so the
  // user can jump back to the originating idea.
  source_requirement_id?: string;
  design_session_id: string; design_job_id: string; analysis_job_id: string; apply_job_id: string; coding_session_id: string;
  skip_analysis: boolean;
  skip_design: boolean;
  branch_name?: string;
  worktree_path?: string;
  // Effective model actually dispatched to the claude CLI for each stage
  // (the model pinned in the --settings env block, or the DefaultModelLabel
  // literal when none was specified). Empty = the stage hasn't run yet (or
  // predates this feature).
  analyst_model?: string;
  architect_model?: string;
  developer_model?: string;
  reviewer_model?: string;
  // Per-stage context-compression state. Populated by POST
  // /api/wizard/compress-context (which writes the summary, stamps the time,
  // and clears the matching session_id). Used by the requirement detail
  // header to render a "compressed" badge and by the chat components to know
  // whether to inject the summary into the next prompt's compression-summary
  // section. Empty summary + null timestamp = never compressed.
  analyst_context_summary?: string;
  analyst_compressed_at?: string | null;
  design_context_summary?: string;
  design_compressed_at?: string | null;
  coding_context_summary?: string;
  coding_compressed_at?: string | null;
  // Session-level context-usage snapshots, one JSON blob keyed by wizard
  // session (analyst_chat / architect_design / coding). Written by the
  // backend at the end of every claude turn (same point it emits the `usage`
  // SSE event) so the usage bars can seed themselves from the Requirement GET
  // and survive a page refresh / panel collapse instead of dropping to 0%.
  // Empty/missing = no snapshot recorded yet. Parsed by parseUsageSnapshots
  // in utils/logLines.ts.
  usage_snapshots?: string;
  // coding_plan: the developer main agent's task breakdown Markdown. Set
  // either (a) when StartCoding's mainAgent output contains a
  // task-breakdown heading (or `<!-- CODING_PLAN_START -->...<!-- CODING_PLAN_END -->`)
  // section, or (b) when /api/requirements/{id}/orchestrate runs and the
  // main agent emits its structured plan. Rendered by SubTaskPanel as the
  // suggested-sub-tasks preview.
  coding_plan?: string;
  // sub_task_count: number of rows in sub_tasks linked to this requirement.
  // Populated by the backend on GET /api/requirements/{id} via a
  // SELECT COUNT(*); used by RequirementDetail to hide the requirement-
  // level follow-up composer once the requirement has been decomposed
  // into sub-tasks (further adjustments must then flow through the
  // SubTaskPanel composer instead). Defaults to 0 on legacy responses that
  // predate this field.
  sub_task_count?: number;
  // Development-environment provenance, stamped when the coding stage starts.
  // 'agent' = the requirement was developed on a remote Agent server (and all
  // follow-up actions — push + PR / clean dev env / sub-tasks — are routed back
  // to that same server); 'local' = developed on the NovaWorkbench host.
  // Empty/undefined = the coding stage never ran, or the row predates this
  // field — the UI shows no badge in that case rather than guessing "local".
  dev_source?: '' | 'local' | 'agent';
  // agent_servers row id + its display name (joined server-side). Both empty
  // for local rows; agent_server_name can also be empty when the server was
  // deleted after the requirement was developed on it.
  agent_server_id?: string;
  agent_server_name?: string;
  // Design-stage agent server binding (mirrors the dev-stage fields above).
  // The architect stage is allowed to run on a remote Agent server just like
  // coding; this column records which one the requirement last designed on
  // so a page refresh can preselect the dropdown without forcing the user
  // to re-pick. Empty / undefined = architect stage ran locally (or never
  // ran); the design section's selector falls back to local.
  design_agent_server_id?: string;
  design_agent_server_name?: string;
  // Development-mode provenance for the coding stage, stamped when StartCoding
  // runs. 'session' = fork the design/analysis session (legacy default —
  // Claude inherits the full conversation). 'design' = fresh session, hand
  // the stored design doc to the agent via the -p prompt. Empty/undefined
  // = never coded or predates this field; the UI shows no badge in that
  // case rather than guessing "session".
  dev_mode?: '' | 'session' | 'design';
  // Agent-server code-transport (sync) mode, stamped when the coding stage
  // starts on an Agent server. '' = 远程 Git 仓库同步 (origin clone/push,
  // legacy default). 'local' = 本地仓库同步 (git-bundle over SFTP for a
  // self-hosted repo with no reachable remote; code is synced back to the
  // local isolated worktree and integrated via 本地合并). Empty/undefined for
  // local (non-agent) rows or rows that predate this field.
  sync_mode?: '' | 'local';
  created_at: string; updated_at: string;
  completed_at?: string;
  // 方案分析计时（requirements 列）。analysis_started_at 在进入「分析中」时打点
  // （每次重新进入分析都会重置起点、清空终点，实现「重新计算」）；
  // analysis_ended_at 在方案完成（进入「已设计」）时打点。均为 RFC3339 字符串，
  // 历史需求 / 未到达阶段为 undefined。
  analysis_started_at?: string;
  analysis_ended_at?: string;
  // 开发阶段整体耗时（派生字段，非数据库列）。后端 Get 从 sub_tasks 聚合：
  // dev_started_at = MIN(created_at)、dev_ended_at = MAX(completed_at)
  //（「以最后一个任务结束时间为准」）。无子任务（旧的单次编码流程）时为 undefined。
  dev_started_at?: string;
  dev_ended_at?: string;
  // 日历视图排期字段。NULL = 未排期，前端回退到 created_at；拖拽或编辑
  // 时通过 PATCH /api/requirements/{id}/schedule 写入。后端序列化为
  // RFC3339 字符串。
  planned_start_at?: string;
  planned_end_at?: string;
  // 派生字段：最早一条 pending scheduled_tasks.run_at。Calendar 端点附
  // 带，前端用 🕐 icon 标识。List/Get 不携带。
  scheduled_run_at?: string;
}

// Default to "requirement" on the client too, so legacy rows missing the
// kind field (or pre-upgrade API responses) still render with a badge.
export const kindOf = (r: { kind?: Kind } | null | undefined): Kind =>
  (r && r.kind) || 'requirement';

// ---- i18n label keys ------------------------------------------------------
// The dictionaries below map backend enum codes onto *translation keys*
// (resolved at render time via i18n/label.ts tLabel) instead of onto display
// strings. A module-level string would freeze the language at import time;
// a key keeps every switcher change live. Unmapped codes fall back to the
// raw value, exactly like the old `map[code] || code` lookups.
export const kindLabelKeys: Record<Kind, string> = {
  issue: 'status.kind.issue',
  requirement: 'status.kind.requirement',
  idea: 'status.kind.idea',
};

// Short plain-text label (no emoji) for chip-style filter buttons on the
// cross-project RequirementsList page.
export const kindShortLabelKeys: Record<Kind, string> = {
  issue: 'status.kindShort.issue',
  requirement: 'status.kindShort.requirement',
  idea: 'status.kindShort.idea',
};

// Hint text shown beneath each kind card in the create form.
export const kindHintKeys: Record<Kind, string> = {
  issue: 'status.kindHint.issue',
  requirement: 'status.kindHint.requirement',
  idea: 'status.kindHint.idea',
};

// Placeholder text for the create-form description textarea.
export const kindPlaceholderKeys: Record<Kind, string> = {
  issue: 'status.kindPlaceholder.issue',
  requirement: 'status.kindPlaceholder.requirement',
  idea: 'status.kindPlaceholder.idea',
};

// CTA button label for the create form, per kind.
export const kindCreateLabelKeys: Record<Kind, string> = {
  issue: 'status.kindCreate.issue',
  requirement: 'status.kindCreate.requirement',
  idea: 'status.kindCreate.idea',
};

// Placeholder text for the analyst-chat composer textarea, per kind.
export const kindChatPlaceholderKeys: Record<Kind, string> = {
  issue: 'status.kindChatPlaceholder.issue',
  requirement: 'status.kindChatPlaceholder.requirement',
  idea: 'status.kindChatPlaceholder.idea',
};

// Stages visible in the detail-page stepper, per kind. An Idea only walks the
// analyst stage — the architect and developer stages are hidden in the UI
// (the frontend doesn't render their CTAs), but the requirement row keeps
// going through the legacy status lifecycle for storage simplicity.
export const STAGE_KEYS = ['analyst', 'architect', 'developer'] as const;
export type StageKey = typeof STAGE_KEYS[number];

export const STAGE_VISIBILITY: Record<Kind, ReadonlyArray<StageKey>> = {
  issue: ['analyst', 'architect', 'developer'],
  requirement: ['analyst', 'architect', 'developer'],
  idea: ['analyst'],
};

export const requirementStatuses = ['draft', 'analyzing', 'designing', 'designed', 'developing', 'done'] as const;
export const statusLabelKeys: Record<string, string> = {
  draft: 'status.req.draft',
  analyzing: 'status.req.analyzing',
  designing: 'status.req.designing',
  designed: 'status.req.designed',
  developing: 'status.req.developing',
  done: 'status.req.done',
  archived: 'status.req.archived',
};

// Priority display labels. The DB stores free-form "high"/"medium"/"low"
// (the create-form only writes those three), but legacy rows can have
// anything — the lookup falls back to the raw value so we never render
// undefined. Key map — translated at render time via tLabel.
export const priorityLabelKeys: Record<string, string> = {
  high: 'requirements.list.priority.high',
  medium: 'requirements.list.priority.medium',
  low: 'requirements.list.priority.low',
};

export const requirementsApi = {
  list: (params?: { project_id?: string; status?: string; priority?: string; kind?: string }) => {
    const q = new URLSearchParams();
    Object.entries(params || {}).forEach(([k, v]) => { if (v) q.set(k, String(v)); });
    return api.get<Requirement[]>(`/api/requirements?${q.toString()}`);
  },
  create: (data: {
    project_id: string;
    description: string;
    priority?: string;
    kind?: Kind;
    skip_analysis?: boolean;
    skip_design?: boolean;
    // skip_organize: when true, the backend skips the LLM-organized markdown
    // pass that normally distills a title + structured body from the raw
    // description. The raw text is stored verbatim and a fallback title
    // (first line, capped) is used. UI default = true (skip).
    skip_organize?: boolean;
  }) => api.post<Requirement>('/api/requirements', data),
  get: (id: string) => api.get<Requirement>(`/api/requirements/${id}`),
  update: (id: string, data: { title: string; description: string; priority: string; skip_analysis?: boolean }) =>
    api.put<Requirement>(`/api/requirements/${id}`, data),
  updateStatus: (id: string, status: string) =>
    api.patch<Requirement>(`/api/requirements/${id}/status`, { status }),
  // Promote a finished Issue or Idea into a Requirement. Only one-way (issue/idea → requirement);
  // the backend validates the rule and rejects everything else with a 400.
  updateKind: (id: string, kind: Kind) =>
    api.patch<Requirement>(`/api/requirements/${id}/kind`, { kind }),
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
  // PromoteFromIdea: summarize an idea's accumulated discussion (description +
  // chat history + acceptance_criteria) into a brand-new requirement row. The
  // original idea keeps its own kind + status; only the new row carries
  // source_requirement_id back to the idea. The backend returns 422 with
  // code "NOT_CONVERGED" when the LLM decides the discussion didn't converge
  // into a concrete feature yet — the modal turns that into a friendlier
  // "not converged yet" message and lets the user keep chatting before
  // retrying.
  promoteFromIdea: (id: string) =>
    api.post<Requirement>(`/api/requirements/${id}/promote`, {}),
  // Calendar view: 瘦 SELECT + 日期范围过滤 + scheduled_run_at 派生字段。
  // from/to 接受 "YYYY-MM-DD" 或 RFC3339；不传则后端默认当月窗口。返回
  // 数组只包含「与 [from, to) 窗口」有交集的需求（包含跨天条）。
  calendar: (params: { from: string; to: string; project_id?: string; kind?: string }) => {
    const q = new URLSearchParams();
    q.set('from', params.from);
    q.set('to', params.to);
    if (params.project_id) q.set('project_id', params.project_id);
    if (params.kind) q.set('kind', params.kind);
    return api.get<Requirement[]>(`/api/requirements/calendar?${q.toString()}`);
  },
  // 排期写入：拖拽/编辑后调用。传入 null 清除对应字段（全 null = 回到
  // created_at 锚点）。
  updateSchedule: (id: string, body: { planned_start_at?: string | null; planned_end_at?: string | null }) =>
    api.patch<Requirement>(`/api/requirements/${id}/schedule`, body),
};

/**
 * Per-stage context-compression record returned by
 * GET /api/wizard/requirement/{id}/context-summary?step=… and embedded in the
 * `done` event of POST /api/wizard/compress-context. The frontend uses
 * `summary` (Chinese summary produced by claude) to render a preview modal;
 * `compressed_at` is the ISO timestamp the row was written, used as a stable
 * key in the requirement header badge.
 */
export interface ContextSummary {
  requirement_id: string;
  step: 'analyst_chat' | 'architect_design' | 'coding' | 'adjust_coding';
  summary: string;
  compressed_at: string | null;
}

/**
 * wizardApi — long-running / SSE wizard operations that aren't covered by
 * the static CRUD endpoints on `requirementsApi`. The two endpoints here
 * back the compress-context button in each chat component: `compressContext`
 * runs a one-shot claude turn that summarizes the session and persists it
 * to the requirements row, while `getContextSummary` reads the persisted
 * summary back for the preview modal and the requirement-detail badge.
 */

/**
 * Request shape for POST /api/wizard/start-coding. The frontend's call
 * sites currently inline this body (RequirementDetail, WizardPage), but
 * keeping a typed shape here documents the contract and lets the type
 * checker flag drift. `claude_config_id` is the user-picked claude_configs
 * row id from the ModelSelect config dropdown; sending it explicitly fixes
 * the "BASE URL doesn't match selected model" bug.
 */
export interface StartCodingReq {
  project_path: string;
  requirement_title: string;
  requirement_desc: string;
  requirement_id?: string;
  branch_name?: string;
  base_branch?: string;
  /** Per-request model override; empty = role's configured model. */
  model?: string;
  /**
   * Per-request claude_configs row id; empty = backend resolves via
   * resolveConfigIDForRun (model owner > role binding > global active).
   */
  claude_config_id?: string;
  read_knowledge?: boolean;
  /** Empty = local execution; non-empty = route through that Agent server. */
  agent_server_id?: string;
  /** false = developer persona direct implementation; true = sub-task split. */
  split_tasks?: boolean;
  /**
   * Whether to auto-dispatch the "提交 → 推送 → 创建 PR" sub-task once
   * development finishes. Undefined = backend keeps the requirement row's
   * persisted value (DB default = true); a boolean is stamped onto the row so
   * the split / non-split / Agent-server completion points all agree.
   */
  auto_push_pr?: boolean;
  /**
   * Coding session threading strategy. 'session' = fork the design/analysis
   * session (legacy default, Claude inherits the conversation). 'design' =
   * fresh session, hand the stored design doc to the agent via the -p
   * prompt. Empty/undefined = backend falls back to the requirement row's
   * persisted value (or 'session' on rows that predate dev_mode).
   */
  dev_mode?: '' | 'session' | 'design';
  /**
   * Agent-server code-transport strategy (only meaningful when
   * agent_server_id is set). 'remote'/'' = origin clone/push (legacy). 'local'
   * = git-bundle over SFTP for a self-hosted repo with no reachable remote.
   * Empty/undefined = backend infers it from the project's remote_url (empty
   * remote_url → local). Persisted so follow-up actions reuse it.
   */
  sync_mode?: '' | 'local' | 'remote';
}

export const wizardApi = {
  /**
   * Trigger claude to compress the current stage's conversation into a short
   * Chinese summary, persist it on `requirements.{step}_context_summary`,
   * stamp `*_compressed_at`, and clear the matching session_id so the next
   * turn sees a fresh context window.
   *
   * `step` is one of "analyst_chat" | "architect_design" | "coding" — the
   * chat components send the value they used as their `usageCtx.Step`.
   * Returns the compressed ContextSummary so the UI can refresh its badge
   * without a second round-trip.
   */
  compressContext: (requirementId: string, step: string) =>
    api.post<ContextSummary>('/api/wizard/compress-context', {
      requirement_id: requirementId,
      step,
    }),
  /**
   * Fetch the persisted compression summary for one stage. Returns an
   * empty summary + null timestamp when the stage has never been
   * compressed — the chat components check this before showing the
   * "compressed" badge.
   */
  getContextSummary: (requirementId: string, step: string) =>
    api.get<ContextSummary>(
      `/api/wizard/requirement/${requirementId}/context-summary?step=${encodeURIComponent(step)}`,
    ),
  /**
   * Snapshot of a finished background wizard job. Backs the schedule log
   * popup — when a scheduled design/coding run finishes, the SchedulesPage
   * view-log button fetches this and renders the log lines inline (the
   * durable job_logs row persists across backend restarts). The wizard's
   * StreamJob SSE uses the same handler internally for live progress.
   */
  getJob: (jobId: string) => api.get<RunJob>(`/api/wizard/jobs/${jobId}`),
  /**
   * Snapshot of all currently-running wizard jobs (across every project in
   * this backend process). Used by the requirement list / detail pages to
   * a working badge on rows whose requirement_id appears in the
   * returned set. Backed by GET /api/wizard/active-jobs which walks the
   * in-memory JobStore ring buffer (cap 50). 5s polling cadence on the
   * frontend — see RequirementDetail / ProjectDetail useEffect.
   */
  listActiveJobs: () => api.get<{ jobs: ActiveJob[] }>('/api/wizard/active-jobs'),
};

/**
 * Per-job projection returned by GET /api/wizard/active-jobs. Intentionally
 * minimal — the frontend only needs to know "is requirement X being worked
 * on right now?" so we surface only id + requirement_id + status + the
 * free-form type label. `status` is always "running" while the job is in
 * the ring buffer (finished jobs are filtered out server-side).
 */
export interface ActiveJob {
  job_id: string;
  requirement_id: string;
  status: 'running';
  type: string;
}

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
  // DefaultModelLabel literal). Present for review jobs; empty for wizard jobs that
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

// Merge / PR step (post-coding merge). Local merge into a target branch with
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
  // Push + create PR is now delegated to a sub-task. The backend returns the
  // usual job_id (for the live SSE stream of the child agent) PLUS the
  // sub_task_id so the SubTaskPanel can pick the row up via its periodic
  // poll and render the artifact Markdown / model badge after completion.
  // Existing callers can still use job_id directly; sub_task_id is read by
  // RequirementDetail so the panel refreshes the moment the row appears.
  push: (reqId: string, body: { commit_message?: string; model?: string }) =>
    api.post<{ job_id: string; sub_task_id: string }>(`/api/requirements/${reqId}/merge/push`, body),
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
  // Git commit identity bound to this token — injected as `-c user.name=...`
  // / `-c user.email=...` so commits work in Docker hosts without a mounted
  // ~/.gitconfig. Empty values fall back to git's normal config lookup.
  git_user_name: string;
  git_user_email: string;
  // GPG signing config. `gpg_key_id` is the 16-hex key id that was last
  // imported on the Agent host (filled in after a successful provision, so
  // the UI can echo "Verified with 0xDEAD…BEEF" without ever echoing the
  // private key itself). The encrypted gpg_private_key / gpg_passphrase
  // columns live server-side and are NEVER returned here.
  gpg_enabled: boolean;
  gpg_key_id: string;
  created_at: string;
  updated_at: string;
}

export const platformApi = {
  list: () => api.get<PlatformToken[]>('/api/settings/tokens'),
  // Create a new token. GPG private key + passphrase are encrypted on the
  // server with AES-256-GCM before they land in the DB, and never leave the
  // server again; passing them here only matters on the create / rotate
  // request, and only the backend ever sees the plaintext.
  create: (data: {
    name: string;
    platform: string;
    base_url: string;
    token: string;
    git_user_name?: string;
    git_user_email?: string;
    gpg_enabled?: boolean;
    gpg_private_key?: string;
    gpg_passphrase?: string;
  }) => api.post<PlatformToken>('/api/settings/tokens', data),
  // Edit the editable fields of an existing token. Token secret is left alone
  // by default (use new_token to rotate it). Updates immediately become the
  // commit identity used by the wizard's merge push.
  //
  // GPG mirrors the secret semantics: gpg_private_key / gpg_passphrase left
  // empty = keep the existing ciphertext. Set `clear_gpg: true` to wipe the
  // stored key + key id and turn gpg_enabled off (used by the UI when the
  // user un-checks the toggle on an already-enabled row).
  update: (id: string, data: {
    name: string;
    base_url: string;
    git_user_name?: string;
    git_user_email?: string;
    new_token?: string;
    gpg_enabled?: boolean;
    gpg_private_key?: string;
    gpg_passphrase?: string;
    clear_gpg?: boolean;
  }) => api.put<PlatformToken>(`/api/settings/tokens/${id}`, data),
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
  // Active config's base URL ("" = Anthropic default). Used to render the
  // `--settings '{"env":{...}}'` prefix on copy-paste launch commands so they
  // hit the same gateway + model Nova itself uses. The auth token is never
  // sent to the frontend — the pasted command relies on the user's own
  // `claude` login/auth, while --settings pins model + base URL.
  base_url?: string;
}

// claudeSettingsPrefix renders the `--settings '{"env":{...}}'` argument that
// every Nova-launched claude process carries (mirrors the backend gateway's
// settingsArg and the agent-worker's buildSettingsArg — one launch shape
// everywhere). baseURL / defaultModel come from the active claude config;
// empty values are omitted so the CLI's own defaults apply. The auth token
// is deliberately not part of it: tokens never leave the backend, and the
// user's own claude auth takes over in their terminal.
export function claudeSettingsPrefix(baseURL?: string, defaultModel?: string): string {
  const env: Record<string, string> = {};
  if (baseURL) env.ANTHROPIC_BASE_URL = baseURL;
  if (defaultModel) {
    env.ANTHROPIC_MODEL = defaultModel;
    env.ANTHROPIC_DEFAULT_HAIKU_MODEL = defaultModel;
    env.ANTHROPIC_DEFAULT_SONNET_MODEL = defaultModel;
    env.ANTHROPIC_DEFAULT_OPUS_MODEL = defaultModel;
  }
  if (Object.keys(env).length === 0) return '';
  return `--settings '${JSON.stringify({ env })}'`;
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

// Roles (per-role system prompt + model + Claude config binding)
//
// claude_config_id pins the role to a specific Claude configuration: the
// role's chosen model then runs against that config's base URL + auth
// token, not just the global active config. Empty = "use the global
// active config" so old rows keep their pre-binding behavior.
export interface Role {
  id: string;
  key: string;
  name: string;
  description: string;
  system_prompt: string;
  model: string;
  claude_config_id: string;
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
  update: (
    id: string,
    data: { system_prompt: string; model: string; claude_config_id?: string },
  ) =>
    api.put<RoleUpdateResult>(`/api/settings/roles/${id}`, data),
  reset: (id: string) => api.post<Role>(`/api/settings/roles/${id}/reset`, {}),
};

export interface Skill {
  id: string;
  name: string;
  slug: string;
  content: string;
  description: string;
  enabled: boolean;
  source_url: string;
  created_at: string;
  updated_at: string;
}

export interface MarketSkill {
  name: string;
  slug: string;
  description: string;
  content: string;
  source_url: string;
}

export interface SkillMarket {
  id: string;
  name: string;
  description: string;
  repo_url: string;
}

export const skillsApi = {
  list: () => api.get<Skill[]>('/api/settings/skills'),
  create: (data: { name: string; slug: string; content: string; description?: string; source_url?: string }) =>
    api.post<Skill>('/api/settings/skills', data),
  update: (id: string, data: { name: string; slug: string; content: string; description: string; enabled: boolean }) =>
    api.put<Skill>(`/api/settings/skills/${id}`, data),
  delete: (id: string) => api.delete<{ status: string }>(`/api/settings/skills/${id}`),
  markets: () => api.get<SkillMarket[]>('/api/settings/skills/markets'),
  market: (params: { market?: string; registry?: string }) => {
    const qs = params.market
      ? `?market=${params.market}`
      : params.registry
        ? `?registry=${encodeURIComponent(params.registry)}`
        : '';
    return api.get<MarketSkill[]>('/api/settings/skills/market' + qs);
  },
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
  // Per-invocation summaries lifted from token_usage.meta (e.g. each follow-up
  // request's first 200 chars). May be absent for steps that don't record a
  // summary — render conditionally.
  summaries?: string[];
}
export interface RequirementUsage {
  by_step: StepUsage[];
  total: UsageTotals;
}

// UsageRow is one token_usage row in its native form. Returned by the
// per-requirement per-row endpoint so the UI can show every individual
// invocation (model, tokens, cost, time, summary) instead of an aggregated
// per-step rollup.
export interface UsageRow {
  id: string;
  requirement_id: string;
  job_id: string;
  step: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  costs: CostItem[];
  summary: string;
  created_at: string;
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

// stepLabelKeys mirrors backend service.StepLabels: raw step code →
// translation key, resolved at render time (i18n/label.ts tLabel) so the
// usage tables follow the UI language.
export const stepLabelKeys: Record<string, string> = {
  requirement_create: 'status.step.requirement_create',
  analyst_chat: 'status.step.analyst_chat',
  architect_design: 'status.step.architect_design',
  refine_doc: 'status.step.refine_doc',
  apply_doc: 'status.step.apply_doc',
  coding: 'status.step.coding',
  adjust_coding: 'status.step.adjust_coding',
  continue_coding: 'status.step.continue_coding',
  developer_chat: 'status.step.developer_chat',
  merge: 'status.step.merge',
  review: 'status.step.review',
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

// fmtNum renders a token count with k/M suffix for >=1k numbers so the
// sub-task TokenStrip stays compact on narrow screens. < 10k keeps one
// decimal ("1.2k"); >= 10k rounds to whole ("12k"). Zero → "0".
export const fmtNum = (n: number): string => {
  if (n === 0) return '0';
  if (n >= 1000000) return `${(n / 1000000).toFixed(n >= 10000000 ? 0 : 1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}k`;
  return n.toLocaleString();
};

// JobUsageSummary mirrors the backend's service.JobUsageSummary. The
// sub-task panel calls usageApi.byJob() to render the 🪙 token strip on
// each sub-task card. total_tokens is the pre-summed convenience field
// (input + output + cache_creation + cache_read) so the UI doesn't have
// to add them up. cost_usd is recomputed from the active config's unit
// prices so a price-list edit applies retroactively.
export interface JobUsageSummary {
  job_id: string;
  input_tokens: number;
  output_tokens: number;
  cache_creation_tokens: number;
  cache_read_tokens: number;
  total_tokens: number;
  step: string;
  model: string;
  invocation_count: number;
  cost_usd: number;
}

export const usageApi = {
  requirement: (id: string) => api.get<RequirementUsage>(`/api/usage/requirement/${id}`),
  rows: (id: string, step?: string) => api.get<UsageRow[]>(`/api/usage/requirement/${id}/rows${step ? `?step=${encodeURIComponent(step)}` : ''}`),
  byRequirement: (projectId: string) => api.get<ReqUsage[]>(`/api/usage/by-requirement?project_id=${projectId}`),
  project: (id: string) => api.get<ProjectUsage>(`/api/usage/project/${id}`),
  // Per-JobStore-job token rollup — sub-task cards fetch this so each child
  // agent's 🪙 token strip can show its own usage. Empty body when the job
  // has no token_usage row yet (still-running / pre-result).
  byJob: (jobId: string) => api.get<JobUsageSummary>(`/api/usage/job/${jobId}`),
};

// ---- Auth & RBAC ---------------------------------------------------------
// User account (RBAC). Distinct from AI-persona Role below.
export interface User {
  id: string;
  username: string;
  display_name: string;
  status: string; // active | disabled
  is_admin: boolean;
  // Preferred UI language (BCP-47: "zh-CN" / "en-US"). Empty = no explicit
  // preference — the client keeps following the browser-level choice.
  locale?: string;
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
  // Persist the signed-in user's UI language (BCP-47, "" = clear the
  // preference and follow the browser setting). Returns the refreshed user.
  setLocale: (locale: string) => api.put<User>('/api/auth/locale', { locale }),
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

// Agent servers — remote Linux/macOS execution targets with AES-256-GCM
// encrypted credentials (the plaintext auth_value never leaves the backend).
// auth_value_set on the read shape tells the UI whether a credential is
// configured without exposing it; create/update accepts the plaintext once
// and the service encrypts before INSERT/UPDATE.
export type AgentServerStatus = 'unknown' | 'checking' | 'installing' | 'ready' | 'error';
export interface AgentServer {
  id: string;
  name: string;
  host: string;
  port: number;
  username: string;
  auth_type: 'key' | 'password';
  auth_value_set: boolean;
  auth_value_algo: string;
  status: AgentServerStatus;
  last_check_at: string | null;
  check_result: string;
  // Active install JobStore job id; empty when no install is running.
  // Persisted server-side so a page refresh can reconnect to the SSE
  // stream and replay history — see SettingsAgentServers mount effect.
  install_job_id: string;
  // nova-agent-worker version the last successful install deployed; empty
  // until the first install that stamps it (see backend agentWorkerVersion).
  worker_version: string;
  created_at: string;
  updated_at: string;
}
export interface CreateAgentServerReq {
  name: string;
  host: string;
  port?: number;
  username?: string;
  auth_type: 'key' | 'password';
  auth_value: string;
}
export interface UpdateAgentServerReq {
  name?: string;
  host?: string;
  port?: number;
  username?: string;
  auth_type?: 'key' | 'password';
  auth_value?: string;
}

export const agentServersApi = {
  list: () => api.get<AgentServer[]>('/api/settings/agent-servers'),
  get: (id: string) => api.get<AgentServer>(`/api/settings/agent-servers/${id}`),
  create: (data: CreateAgentServerReq) =>
    api.post<AgentServer>('/api/settings/agent-servers', data),
  update: (id: string, data: UpdateAgentServerReq) =>
    api.put<AgentServer>(`/api/settings/agent-servers/${id}`, data),
  remove: (id: string) =>
    api.delete<void>(`/api/settings/agent-servers/${id}`),
  check: (id: string) =>
    api.post<{ job_id: string }>(`/api/settings/agent-servers/${id}/check`, {}),
  install: (id: string) =>
    api.post<{ job_id: string }>(`/api/settings/agent-servers/${id}/install`, {}),
  jobUrl: (jobId: string) => `${API_BASE}/api/settings/agent-servers/jobs/${jobId}`,
  jobStreamUrl: (jobId: string) => `${API_BASE}/api/settings/agent-servers/jobs/${jobId}/stream`,
};

// ────────────────────────────────────────────────────────────────────────
// Scheduled tasks — one-shot future-dated wizard actions.
// See backend internal/service/scheduled_task.go for the persistence layer
// and internal/scheduler for the polling loop.
// ────────────────────────────────────────────────────────────────────────

export type ScheduledTaskType = 'design' | 'coding';
export type ScheduledTaskStatus =
  | 'pending'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'canceled';

export interface ScheduledTask {
  id: string;
  task_type: ScheduledTaskType;
  requirement_id: string;
  project_id: string;
  requirement_title: string;
  run_at: string; // RFC3339 from server, preserves the offset the client sent so the moment round-trips correctly even when server TZ ≠ client TZ
  model: string; // '' = role default
  read_knowledge: boolean;
  branch_name: string; // coding only
  base_branch: string; // coding only
  agent_server_id: string; // coding only
  split_tasks: boolean; // coding only
  status: ScheduledTaskStatus;
  job_id: string;
  error_message: string;
  created_by: string;
  created_at: string;
  updated_at: string;
  executed_at: string | null;
}

export interface CreateScheduleReq {
  requirement_id: string;
  task_type: ScheduledTaskType;
  run_at: string; // RFC3339 with timezone offset (e.g. "2026-09-07T23:30:00+08:00"); the frontend converts the datetime-local picker value to this so the absolute moment is unambiguous regardless of the server's local TZ.
  model?: string;
  read_knowledge?: boolean;
  branch_name?: string;
  base_branch?: string;
  agent_server_id?: string;
  split_tasks?: boolean;
}

export const schedulesApi = {
  list: (params?: { status?: string; task_type?: string; requirement_id?: string }) => {
    const q = new URLSearchParams();
    if (params?.status) q.set('status', params.status);
    if (params?.task_type) q.set('task_type', params.task_type);
    if (params?.requirement_id) q.set('requirement_id', params.requirement_id);
    const qs = q.toString();
    return api.get<ScheduledTask[]>(`/api/schedules${qs ? `?${qs}` : ''}`);
  },
  get: (id: string) => api.get<ScheduledTask>(`/api/schedules/${id}`),
  create: (data: CreateScheduleReq) =>
    api.post<ScheduledTask>('/api/schedules', data),
  cancel: (id: string) =>
    api.post<ScheduledTask>(`/api/schedules/${id}/cancel`, {}),
  remove: (id: string) => api.delete<{ id: string; status: string }>(`/api/schedules/${id}`),
};
