import { useState, useEffect, useCallback, useRef } from 'react';
import {
  agentServersApi,
  type AgentServer,
  type CreateAgentServerReq,
  type UpdateAgentServerReq,
} from '../api/client';
import './SettingsAgentServers.css';

// Status badge colors mirror the wizard/preflight pattern (CSS in index.css).
const statusBadge: Record<AgentServer['status'], string> = {
  unknown: 'badge-unknown',
  checking: 'badge-checking',
  installing: 'badge-installing',
  ready: 'badge-ready',
  error: 'badge-error',
};
const statusLabel: Record<AgentServer['status'], string> = {
  unknown: '未检查',
  checking: '检查中…',
  installing: '安装中…',
  ready: '就绪',
  error: '异常',
};

interface Form {
  name: string;
  host: string;
  port: number;
  username: string;
  auth_type: 'key' | 'password';
  auth_value: string;
}

const emptyForm: Form = {
  name: '',
  host: '',
  port: 22,
  username: 'root',
  auth_type: 'key',
  auth_value: '',
};

export default function SettingsAgentServers() {
  const [servers, setServers] = useState<AgentServer[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [toast, setToast] = useState('');

  const [showModal, setShowModal] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [form, setForm] = useState<Form>(emptyForm);
  const [saving, setSaving] = useState(false);
  // Banner-level vs modal-level error are separate: a validation failure
  // shouldn't leak to the page-level red strip after the modal closes.
  const [modalError, setModalError] = useState('');

  // Per-server live-log state: serverId → SSE lines + running flag.
  const [logs, setLogs] = useState<Record<string, string[]>>({});
  const [busy, setBusy] = useState<Record<string, string>>({}); // serverId → 'check'|'install'|''
  const abortRef = useRef<Record<string, AbortController>>({});

  const load = useCallback(() => {
    setLoading(true);
    agentServersApi.list()
      .then((rows) => setServers(rows ?? []))
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => { load(); }, [load]);

  // Page-refresh reconnect: the backend persists the running install's
  // job id in agent_servers.install_job_id (set by Install, cleared by
  // runInstall's defer on Finish). On mount, for any server that still
  // has a non-empty install_job_id, subscribe to that job's SSE stream so
  // the user picks up the live log + history replay instead of staring at
  // a frozen "安装中…" badge until the install finishes server-side.
  //
  // Guarded with a ref so the run-once intent is preserved across React
  // StrictMode double-mounts in dev — without the ref the first mount
  // would subscribe, then the immediate remount would open a second
  // duplicate SSE connection to the same job.
  const reconnectRanRef = useRef(false);
  useEffect(() => {
    if (reconnectRanRef.current) return;
    reconnectRanRef.current = true;
    let cancelled = false;
    (async () => {
      let rows: AgentServer[] = [];
      try {
        rows = (await agentServersApi.list()) ?? [];
      } catch {
        return;
      }
      if (cancelled) return;
      for (const s of rows) {
        if (!s.install_job_id) continue;
        // Skip if a fresh job is already being driven by startJob (e.g.
        // the user clicked Install then immediately refreshed; both paths
        // set busy, and startJob's AbortController would conflict).
        if (busy[s.id]) continue;
        setBusy((b) => ({ ...b, [s.id]: 'install' }));
        void subscribeToJob(s.id, s.install_job_id);
      }
    })();
    return () => {
      cancelled = true;
    };
    // subscribeToJob is intentionally omitted from deps: it reads load via
    // closure but is itself stable across renders (useCallback with [load]
    // dep — and load is also stable). Including it would re-fire the
    // reconnect every time load's identity changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const showToast = (msg: string) => {
    setToast(msg);
    window.setTimeout(() => setToast(''), 4000);
  };

  const openCreate = () => {
    setEditingId(null);
    setForm(emptyForm);
    setError('');
    setModalError('');
    setShowModal(true);
  };

  const openEdit = (s: AgentServer) => {
    setEditingId(s.id);
    setForm({
      name: s.name,
      host: s.host,
      port: s.port || 22,
      username: s.username || 'root',
      auth_type: (s.auth_type === 'password' ? 'password' : 'key'),
      auth_value: '', // never pre-fill — must be re-typed to rotate
    });
    setError('');
    setModalError('');
    setShowModal(true);
  };

  // Closing the modal must wipe the modal-local error so it doesn't bleed
  // onto the page-level red strip (setError) after dismissal.
  const closeModal = () => {
    setShowModal(false);
    setModalError('');
  };

  const handleSave = async () => {
    if (!form.name.trim() || !form.host.trim()) {
      setModalError('名称和 IP/主机名必填');
      return;
    }
    if (!editingId && !form.auth_value.trim()) {
      setModalError('请填写凭据（SSH 私钥或密码）');
      return;
    }
    setSaving(true);
    setModalError('');
    try {
      if (editingId) {
        const req: UpdateAgentServerReq = {
          name: form.name,
          host: form.host,
          port: form.port,
          username: form.username,
          auth_type: form.auth_type,
        };
        // Only send auth_value when the user actually re-typed one. nil
        // (= undefined here) preserves the existing ciphertext.
        if (form.auth_value) req.auth_value = form.auth_value;
        await agentServersApi.update(editingId, req);
      } else {
        const req: CreateAgentServerReq = {
          name: form.name,
          host: form.host,
          port: form.port,
          username: form.username,
          auth_type: form.auth_type,
          auth_value: form.auth_value,
        };
        await agentServersApi.create(req);
      }
      closeModal();
      showToast(editingId ? '已保存' : '已添加');
      load();
    } catch (err) {
      setModalError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async (id: string) => {
    if (!window.confirm('确认删除此 Agent 服务器？此操作不可撤销。')) return;
    try {
      await agentServersApi.remove(id);
      showToast('已删除');
      load();
    } catch (err) {
      showToast('删除失败: ' + (err instanceof Error ? err.message : String(err)));
    }
  };

  // Subscribe to an existing job's SSE stream and pump lines into the log
  // panel. Split out from startJob so the page-refresh reconnect path can
  // pass a jobId already persisted in agent_servers.install_job_id without
  // re-clearing the log (which would discard lines the user wants to keep
  // visible across reloads). streamJobSSE replays the full history first
  // so the user sees everything that happened while they were away.
  //
  // Handles three terminal cases:
  //   1. `job_done` frame → Finish arrived cleanly; reload to pick up the
  //      cleared install_job_id and the post-install server status.
  //   2. HTTP 404 (e.g. backend restarted and JobStore evicted the job while
  //      DB still held the old id) → assume stale, reload to clear.
  //   3. fetch error / aborted → mark not-busy so the button re-enables.
  //
  // Defined before startJob so startJob can list it as a dep without an
  // ordering dance. `load` is wrapped in useCallback with no deps so its
  // identity is stable; listing it is harmless and keeps oxlint happy.
  const subscribeToJob = useCallback(async (serverId: string, jobId: string) => {
    const ctrl = new AbortController();
    abortRef.current[serverId] = ctrl;
    try {
      const resp = await fetch(agentServersApi.jobStreamUrl(jobId), {
        headers: { Authorization: `Bearer ${localStorage.getItem('nova_token') || ''}` },
        signal: ctrl.signal,
      });
      if (!resp.ok || !resp.body) {
        // 404 = the job is gone from JobStore (most likely because the
        // backend restarted and the in-memory ring buffer was cleared
        // while the DB still held this install_job_id). Reload to pick
        // up the cleared id and the post-install status.
        if (resp.status === 404) {
          load();
          return;
        }
        throw new Error(`SSE 失败: HTTP ${resp.status}`);
      }

      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      let done = false;
      while (!done) {
        const { value, done: rdDone } = await reader.read();
        done = rdDone;
        if (value) buf += decoder.decode(value, { stream: true });
        let nl;
        while ((nl = buf.indexOf('\n\n')) >= 0) {
          const frame = buf.slice(0, nl);
          buf = buf.slice(nl + 2);
          // Skip keepalive comments (": ping")
          if (frame.startsWith(':')) continue;
          const line = frame.replace(/^data:\s*/, '');
          try {
            const parsed = JSON.parse(line);
            if (parsed.type === 'job_done') {
              setBusy((b) => ({ ...b, [serverId]: '' }));
              load(); // refresh status badge + install_job_id (now cleared by defer)
              return;
            }
            if (parsed.content) {
              setLogs((l) => ({ ...l, [serverId]: [...(l[serverId] ?? []), parsed.content] }));
            }
          } catch {
            // ignore non-JSON frames
          }
        }
      }
    } catch (err) {
      // AbortError is the cancelJob path — silent, the button click already
      // cleared busy. Any other error is a real failure: surface it.
      if ((err as Error)?.name !== 'AbortError') {
        setLogs((l) => ({ ...l, [serverId]: [...(l[serverId] ?? []), `❌ ${err instanceof Error ? err.message : String(err)}`] }));
      }
    } finally {
      setBusy((b) => ({ ...b, [serverId]: '' }));
      delete abortRef.current[serverId];
    }
  }, [load]);

  // Stream a check/install job to the per-server log panel. Reuses the SSE
  // pump pattern the wizard's CodingChat uses: open POST → fetch response
  // stream → parse `data:` lines → append to log. The actual SSE pump lives
  // in subscribeToJob so the mount-time reconnect path can reuse it without
  // clearing the log buffer (which would drop the history replay).
  const startJob = useCallback(async (serverId: string, action: 'check' | 'install') => {
    setBusy((b) => ({ ...b, [serverId]: action }));
    setLogs((l) => ({ ...l, [serverId]: [] }));

    let jobId = '';
    try {
      const res = action === 'check'
        ? await agentServersApi.check(serverId)
        : await agentServersApi.install(serverId);
      jobId = res.job_id;
    } catch (err) {
      setLogs((l) => ({ ...l, [serverId]: [...(l[serverId] ?? []), `❌ 提交失败: ${err instanceof Error ? err.message : String(err)}`] }));
      setBusy((b) => ({ ...b, [serverId]: '' }));
      return;
    }

    await subscribeToJob(serverId, jobId);
  }, [subscribeToJob]);

  const cancelJob = (serverId: string) => {
    abortRef.current[serverId]?.abort();
    setBusy((b) => ({ ...b, [serverId]: '' }));
  };

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">🖥️ Agent 服务器</h3>
          <p className="settings-section-desc">
            配置远程 Linux / macOS 服务器作为 Claude CLI 的执行环境。平台自动检查并安装依赖（claude / node / npm / git），需求开发时可选择在该服务器上执行。
          </p>
        </div>
        <button className="btn btn-primary" onClick={openCreate}>+ 添加服务器</button>
      </div>

      {error && <div className="form-error">❌ {error}</div>}
      {toast && <div className="form-toast">✅ {toast}</div>}

      <div className="security-banner">
        ⚠️ 凭据已使用 AES-256-GCM 加密存储，主密钥位于 <code>~/.novaworkbench/secret.key</code>，
        请妥善保管该文件；密钥丢失则旧凭据不可解，需重新配置服务器。
      </div>

      {loading ? (
        <div className="loading">加载中...</div>
      ) : servers.length === 0 ? (
        <div className="empty-state">
          <span className="empty-state-mark" aria-hidden="true">🛰️</span>
          <div className="empty-state-title">还没有 Agent 服务器</div>
          <div className="empty-state-desc">
            添加一台远程 Linux 或 macOS 主机，让 Claude CLI 在那里执行需求开发。
          </div>
          <button className="btn btn-primary" onClick={openCreate} style={{ marginTop: 14 }}>
            + 添加服务器
          </button>
        </div>
      ) : (
        <div className="server-list">
          {servers.map((s) => (
            <ServerCard
              key={s.id}
              server={s}
              logs={logs[s.id] ?? []}
              busy={busy[s.id] || ''}
              onEdit={() => openEdit(s)}
              onDelete={() => handleDelete(s.id)}
              onCheck={() => startJob(s.id, 'check')}
              onInstall={() => startJob(s.id, 'install')}
              onCancel={() => cancelJob(s.id)}
            />
          ))}
        </div>
      )}

      {showModal && (
        <div className="modal-overlay" onClick={closeModal}>
          <div className="modal-content" onClick={(e) => e.stopPropagation()}>
            <h3>{editingId ? '编辑 Agent 服务器' : '添加 Agent 服务器'}</h3>
            {modalError && <div className="form-error">❌ {modalError}</div>}
            <div className="form-group">
              <label>名称 *</label>
              <input
                type="text"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                placeholder="如：办公室 Mac mini"
              />
            </div>
            <div className="form-group">
              <label>IP / 主机名 *</label>
              <input
                type="text"
                value={form.host}
                onChange={(e) => setForm({ ...form, host: e.target.value })}
                placeholder="192.168.1.100 或 host.example.com"
              />
            </div>
            <div className="form-row">
              <div className="form-group">
                <label>端口</label>
                <input
                  type="number"
                  value={form.port}
                  onChange={(e) => setForm({ ...form, port: parseInt(e.target.value || '22', 10) })}
                  min={1}
                  max={65535}
                />
              </div>
              <div className="form-group">
                <label>用户名</label>
                <input
                  type="text"
                  value={form.username}
                  onChange={(e) => setForm({ ...form, username: e.target.value })}
                />
              </div>
            </div>
            <div className="form-group">
              <label>认证方式</label>
              <select
                value={form.auth_type}
                onChange={(e) => setForm({ ...form, auth_type: e.target.value as 'key' | 'password' })}
              >
                <option value="key">SSH 私钥（PEM）</option>
                <option value="password">密码</option>
              </select>
            </div>
            <div className="form-group">
              <label>
                {form.auth_type === 'key' ? '私钥内容（PEM）' : '密码'}
                {editingId && <span className="hint">（留空保持不变）</span>}
              </label>
              <textarea
                value={form.auth_value}
                onChange={(e) => setForm({ ...form, auth_value: e.target.value })}
                placeholder={
                  form.auth_type === 'key'
                    ? '-----BEGIN OPENSSH PRIVATE KEY-----\n...\n-----END OPENSSH PRIVATE KEY-----'
                    : '密码'
                }
                rows={form.auth_type === 'key' ? 8 : 2}
                style={{ fontFamily: form.auth_type === 'key' ? 'monospace' : 'inherit' }}
              />
            </div>
            <div className="form-actions">
              <button className="btn" onClick={closeModal} disabled={saving}>取消</button>
              <button className="btn btn-primary" onClick={handleSave} disabled={saving}>
                {saving ? '保存中...' : '保存'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function ServerCard({
  server, logs, busy,
  onEdit, onDelete, onCheck, onInstall, onCancel,
}: {
  server: AgentServer;
  logs: string[];
  busy: string;
  onEdit: () => void;
  onDelete: () => void;
  onCheck: () => void;
  onInstall: () => void;
  onCancel: () => void;
}) {
  const [expanded, setExpanded] = useState(true); // expanded by default — install logs are the main signal
  const logRef = useRef<HTMLPreElement>(null);
  const showLogs = busy !== '' || logs.length > 0;

  // Auto-scroll the terminal panel to the bottom whenever new lines arrive.
  // Without this a streaming install fills the panel above the fold and the
  // user has to manually drag the scrollbar to keep up.
  useEffect(() => {
    if (logRef.current && (busy !== '' || logs.length > 0)) {
      logRef.current.scrollTop = logRef.current.scrollHeight;
    }
  }, [logs, busy]);

  return (
    <div className="server-card">
      <div className="server-card-header">
        <div className="server-card-info">
          <div className="server-card-title-row">
            {/* Indigo pulse — twin of .ai-pulse-dot.is-work, surfaces "agent
                is doing something" using the platform's AI-busy vocabulary. */}
            {busy !== '' && <span className="agent-pulse" aria-hidden="true" />}
            <h4>{server.name}</h4>
            <span className={`badge ${statusBadge[server.status]}`}>{statusLabel[server.status]}</span>
          </div>
          <div className="server-card-meta">
            {/* Terminal-style SSH connection: "$ ssh user@host:port". The "$"
                prefix and host part are split so the prompt can stay muted
                while the host is highlighted — like a real terminal line. */}
            <span className="server-card-ssh">
              <span className="server-card-ssh-prompt">$ ssh</span>
              <span className="server-card-ssh-host">{server.username}@{server.host}:{server.port}</span>
            </span>
            {server.last_check_at && (
              <span className="server-card-check-time">
                上次检查: {new Date(server.last_check_at).toLocaleString()}
              </span>
            )}
          </div>
          {server.check_result && (
            <div className="server-card-result">{server.check_result}</div>
          )}
        </div>
        <div className="server-card-actions">
          <button className="btn" onClick={onCheck} disabled={busy !== ''}>🔍 检查环境</button>
          <button className="btn" onClick={onInstall} disabled={busy !== ''}>⚙️ 安装依赖</button>
          <button className="btn" onClick={onEdit} disabled={busy !== ''}>编辑</button>
          <button className="btn btn-danger" onClick={onDelete} disabled={busy !== ''}>删除</button>
        </div>
      </div>
      {showLogs && (
        <div className="server-card-logs">
          <div className="server-card-logs-header">
            <span className="server-card-logs-header-left">
              Terminal output
              {busy !== '' && <span className="server-card-caret" aria-hidden="true" />}
            </span>
            {busy !== '' ? (
              <button className="btn-link" onClick={onCancel}>取消</button>
            ) : (
              <button className="btn-link" onClick={() => setExpanded((v) => !v)}>
                {expanded ? '收起' : '展开'}
              </button>
            )}
          </div>
          {expanded && (
            <pre className="server-card-log" ref={logRef}>
              {logs.length === 0 ? '(暂无输出)' : logs.join('\n')}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}
