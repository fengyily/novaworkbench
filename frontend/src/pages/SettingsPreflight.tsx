import { useEffect, useState } from 'react';
import {
  preflightApi,
  authedFetch,
  type PreflightDep,
  type PreflightSnapshot,
} from '../api/client';

interface LogLine {
  type: string;
  content: string;
}

interface InstallState {
  job_id: string;
  log: LogLine[];
  done: boolean;
  exit_code: number;
  status: string;
  abort?: () => void;
}

const STATUS_LABEL: Record<string, string> = {
  installed: '已安装',
  missing: '未安装',
  running: '安装中',
};

export default function SettingsPreflight() {
  const [snap, setSnap] = useState<PreflightSnapshot | null>(null);
  const [error, setError] = useState('');
  const [installing, setInstalling] = useState<Record<string, InstallState>>({});

  const refresh = async () => {
    try {
      const data = await preflightApi.snapshot();
      setSnap(data);
      setError('');
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  useEffect(() => { refresh(); }, []);

  // Abort any in-flight install streams on unmount.
  useEffect(() => {
    return () => {
      Object.values(installing).forEach(s => s.abort?.());
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const handleInstall = async (key: string) => {
    try {
      const { job_id } = await preflightApi.install(key);
      setInstalling(prev => ({
        ...prev,
        [key]: { job_id, log: [], done: false, exit_code: 0, status: 'running' },
      }));

      // Consume the SSE stream via fetch + ReadableStream so we can send the
      // bearer token header (EventSource can't). Mirrors the pattern used by
      // the wizard chat components (CLAUDE.md: "consume SSE streams over
      // fetch with manual ReadableStream parsing").
      const ctrl = new AbortController();
      const res = await authedFetch(preflightApi.installStreamUrl(job_id), {
        signal: ctrl.signal,
      });
      if (!res.ok || !res.body) {
        throw new Error(`SSE 打开失败: HTTP ${res.status}`);
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = '';
      const pushLine = (line: string) => {
        if (!line.startsWith('data:')) return;
        const payload = line.slice(5).trim();
        if (!payload) return;
        try {
          const data = JSON.parse(payload);
          setInstalling(prev => {
            const cur = prev[key];
            if (!cur) return prev;
            if (data.type === 'job_done') {
              return {
                ...prev,
                [key]: {
                  ...cur,
                  done: true,
                  status: data.status || 'done',
                  exit_code: data.exit_code ?? 0,
                },
              };
            }
            return {
              ...prev,
              [key]: { ...cur, log: [...cur.log, { type: data.type, content: data.content }] },
            };
          });
          if (data.type === 'job_done') {
            reader.cancel();
            refresh();
          }
        } catch { /* ignore non-JSON */ }
      };
      // Wire the abort handler into state so unmount can cancel.
      setInstalling(prev => {
        const cur = prev[key];
        if (!cur) return prev;
        return { ...prev, [key]: { ...cur, abort: () => ctrl.abort() } };
      });
      // Read loop
      (async () => {
        try {
          while (true) {
            const { value, done } = await reader.read();
            if (done) break;
            buffer += decoder.decode(value, { stream: true });
            const lines = buffer.split('\n');
            buffer = lines.pop() || '';
            for (const line of lines) pushLine(line);
          }
          if (buffer) pushLine(buffer);
        } catch {
          // Aborted or network error — leave state as-is
        }
      })();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  if (!snap) {
    return <div className="settings-empty">{error || '加载中...'}</div>;
  }

  const overallOk = snap.deps.filter(d => d.required).every(d => d.installed);

  return (
    <div className="settings-section">
      <div className="section-header">
        <div>
          <h3 className="settings-section-title">环境依赖</h3>
          <p className="settings-section-desc">
            NovaWorkbench 在启动时会自动检查 Claude CLI、Node.js、git、docker 等运行依赖。
            缺失依赖时后端会尽力自动安装；此处可手动重装或查看手动安装指引。
            当前 <code>CLAUDE_BIN</code>：
            <code>{snap.claude_bin || '(PATH 中寻找 claude)'}</code>。
          </p>
        </div>
        <button className="btn btn-secondary" onClick={refresh}>重新检测</button>
      </div>

      {error && <div className="form-error">{error}</div>}

      {!overallOk && (
        <div className="form-hint" style={{ marginBottom: 12, color: '#b45309' }}>
          必需依赖未全部就绪，AI 相关功能可能不可用。
        </div>
      )}

      <div style={{ display: 'flex', flexDirection: 'column', gap: 12, marginTop: 12 }}>
        {snap.deps.map((dep) => (
          <DepCard
            key={dep.key}
            dep={dep}
            state={installing[dep.key]}
            onInstall={() => handleInstall(dep.key)}
          />
        ))}
      </div>
    </div>
  );
}

function DepCard({ dep, state, onInstall }: { dep: PreflightDep; state?: InstallState; onInstall: () => void }) {
  const status: keyof typeof STATUS_LABEL = state?.done ? (state.status === 'done' ? 'installed' : 'missing')
    : state ? 'running'
    : dep.installed ? 'installed' : 'missing';
  const statusColor = status === 'installed' ? '#10B981' : status === 'running' ? '#4F46E5' : '#b45309';

  return (
    <div className="form-group" style={{ border: '1px solid #e5e7eb', borderRadius: 8, padding: 12 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12 }}>
        <div>
          <div style={{ fontSize: 15, fontWeight: 600 }}>
            {dep.label}
            {dep.required && <span style={{ color: '#b91c1c', marginLeft: 6, fontSize: 12 }}>必需</span>}
            {dep.depends_on.length > 0 && (
              <span style={{ color: '#64748B', marginLeft: 6, fontSize: 12 }}>
                依赖：{dep.depends_on.join(', ')}
              </span>
            )}
          </div>
          <div style={{ fontSize: 13, color: statusColor, marginTop: 4 }}>{STATUS_LABEL[status]}</div>
          {dep.installed && dep.path && (
            <div style={{ fontSize: 12, color: '#64748B', marginTop: 4, fontFamily: 'monospace' }}>
              {dep.path}{dep.version ? `  (${dep.version})` : ''}
            </div>
          )}
          {!dep.installed && dep.err && (
            <div style={{ fontSize: 12, color: '#64748B', marginTop: 4 }}>{dep.err}</div>
          )}
        </div>
        <button
          className="btn btn-primary"
          onClick={onInstall}
          disabled={status === 'running'}
          style={{ whiteSpace: 'nowrap' }}
        >
          {status === 'running' ? '安装中...' : dep.installed ? '重新安装' : '一键安装'}
        </button>
      </div>

      {!dep.installed && dep.manual && status !== 'running' && (
        <div style={{ marginTop: 10, padding: 10, background: '#f8fafc', borderRadius: 6, fontSize: 12, color: '#475569' }}>
          <div style={{ fontWeight: 600, marginBottom: 4 }}>手动安装：</div>
          <code style={{ wordBreak: 'break-all' }}>{dep.manual}</code>
        </div>
      )}

      {state && state.log.length > 0 && (
        <div style={{ marginTop: 10, background: '#0f172a', color: '#f1f5f9', padding: 10, borderRadius: 6, fontFamily: 'monospace', fontSize: 12, maxHeight: 240, overflowY: 'auto' }}>
          {state.log.map((line, idx) => (
            <div key={idx} style={{ color: line.type === 'error' ? '#fca5a5' : '#f1f5f9' }}>
              {line.content}
            </div>
          ))}
          {state.done && (
            <div style={{ marginTop: 8, color: state.status === 'done' ? '#86efac' : '#fca5a5' }}>
              {state.status === 'done' ? `安装完成 (exit ${state.exit_code})` : `安装失败 (exit ${state.exit_code})`}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
