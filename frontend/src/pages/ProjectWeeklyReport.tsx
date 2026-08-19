import { useState, useEffect, useRef, useCallback } from 'react';
import { reportsApi, type WeeklyReport, type ReportGitInfo, type GenerateReportBody } from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import MarkdownViewer from '../components/MarkdownViewer';
import './ProjectWeeklyReport.css';

// This week's Monday through today, formatted as YYYY-MM-DD (display only —
// when no custom dates are picked the backend computes the same default).
function thisWeekRange(): { start: string; end: string } {
  const fmt = (d: Date) => {
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, '0');
    const day = String(d.getDate()).padStart(2, '0');
    return `${y}-${m}-${day}`;
  };
  const now = new Date();
  const monday = new Date(now);
  monday.setDate(now.getDate() - ((now.getDay() + 6) % 7));
  return { start: fmt(monday), end: fmt(now) };
}

export default function ProjectWeeklyReport({ projectId }: { projectId: string }) {
  // Rule template
  const [rule, setRule] = useState('');
  const [savedRule, setSavedRule] = useState('');
  const [ruleSaving, setRuleSaving] = useState(false);
  const [ruleSaved, setRuleSaved] = useState(false);
  const [rulePresets, setRulePresets] = useState<Record<string, string>>({});
  const [presetChoice, setPresetChoice] = useState('standard');

  // Period (custom dates override the default "this week")
  const week = thisWeekRange();
  const [periodStart, setPeriodStart] = useState('');
  const [periodEnd, setPeriodEnd] = useState('');

  // Git scope: branch + author filters for the commit summary.
  // branchChoice: 'current' | 'all' | a branch name. Sent as '.', '', name.
  const [gitInfo, setGitInfo] = useState<ReportGitInfo | null>(null);
  const [branchChoice, setBranchChoice] = useState('all');
  const [author, setAuthor] = useState('');
  const [diffAnalysis, setDiffAnalysis] = useState(false);

  // Generation stream
  const [generating, setGenerating] = useState(false);
  const [genLines, setGenLines] = useState<{ type: string; content: string }[]>([]);
  const [genError, setGenError] = useState('');
  const esRef = useRef<EventStream | null>(null);
  const panelRef = useRef<HTMLDivElement>(null);

  // History
  const [reports, setReports] = useState<WeeklyReport[]>([]);
  const [listLoading, setListLoading] = useState(true);
  const [viewing, setViewing] = useState<WeeklyReport | null>(null);
  const [deletingId, setDeletingId] = useState('');

  const loadReports = useCallback(() => {
    setListLoading(true);
    reportsApi.list(projectId)
      .then(data => setReports(data ?? []))
      .catch(() => {})
      .finally(() => setListLoading(false));
  }, [projectId]);

  useEffect(() => {
    reportsApi.getRule(projectId)
      .then(data => { setRule(data.rule); setSavedRule(data.rule); })
      .catch(() => {});
    reportsApi.rulePresets(projectId)
      .then(setRulePresets)
      .catch(() => {});
    reportsApi.gitInfo(projectId)
      .then(info => {
        setGitInfo(info);
        // Default the author to the most recent committer — for a personal
        // weekly report that is almost always the user themselves.
        if (info.authors.length > 0) setAuthor(info.authors[0]);
      })
      .catch(() => {});
    loadReports();
  }, [projectId, loadReports]);

  // Auto-scroll the stream panel
  useEffect(() => {
    if (panelRef.current) panelRef.current.scrollTop = panelRef.current.scrollHeight;
  }, [genLines]);

  // Close the stream on unmount / project switch
  useEffect(() => {
    return () => { esRef.current?.close(); };
  }, []);

  const ruleDirty = rule !== savedRule;

  const applyPreset = () => {
    const preset = rulePresets[presetChoice];
    if (preset) setRule(preset);
  };

  const handleSaveRule = async () => {
    setRuleSaving(true);
    setRuleSaved(false);
    try {
      await reportsApi.saveRule(projectId, rule);
      setSavedRule(rule);
      setRuleSaved(true);
      setTimeout(() => setRuleSaved(false), 2000);
    } catch { /* ignore */ } finally {
      setRuleSaving(false);
    }
  };

  const handleGenerate = async () => {
    esRef.current?.close();
    setGenerating(true);
    setGenLines([]);
    setGenError('');

    try {
      const body: GenerateReportBody = {};
      if (periodStart) body.period_start = periodStart;
      if (periodEnd) body.period_end = periodEnd;
      if (branchChoice === 'current') body.branch = '.';
      else if (branchChoice !== 'all') body.branch = branchChoice;
      if (author.trim()) body.author = author.trim();
      if (diffAnalysis) body.diff_analysis = true;
      const { job_id } = await reportsApi.generate(projectId, body);

      esRef.current = createEventStream(
        reportsApi.streamJobUrl(projectId, job_id),
        (data) => {
          if (data.type === 'job_done') {
            setGenerating(false);
            esRef.current?.close();
            esRef.current = null;
            if (data.status === 'done') loadReports();
          } else if (data.content) {
            setGenLines(prev => [...prev, { type: data.type, content: data.content }]);
          }
        },
        () => {
          setGenLines(prev => [...prev, { type: 'error', content: 'SSE 连接中断' }]);
          setGenerating(false);
          esRef.current = null;
        },
      );
    } catch (err: unknown) {
      setGenError(err instanceof Error ? err.message : String(err));
      setGenerating(false);
    }
  };

  const handleDelete = async (report: WeeklyReport) => {
    if (!window.confirm(`删除 ${report.period_start} ~ ${report.period_end} 的周报？`)) return;
    setDeletingId(report.id);
    try {
      await reportsApi.remove(projectId, report.id);
      setReports(prev => prev.filter(r => r.id !== report.id));
      if (viewing?.id === report.id) setViewing(null);
    } catch { /* ignore */ } finally {
      setDeletingId('');
    }
  };

  return (
    <div className="weekly-report">
      {/* ── Generation controls ── */}
      <div className="detail-section">
        <div className="section-header" style={{ marginBottom: 12 }}>
          <span style={{ fontWeight: 600, fontSize: 14 }}>生成周报</span>
        </div>

        <div className="weekly-period-row">
          <span className="weekly-period-label">统计周期</span>
          <input
            type="date"
            className="form-input weekly-date-input"
            value={periodStart}
            placeholder={week.start}
            onChange={e => setPeriodStart(e.target.value)}
            disabled={generating}
          />
          <span style={{ color: 'var(--color-text-muted)' }}>~</span>
          <input
            type="date"
            className="form-input weekly-date-input"
            value={periodEnd}
            placeholder={week.end}
            onChange={e => setPeriodEnd(e.target.value)}
            disabled={generating}
          />
          {!periodStart && !periodEnd && (
            <span className="weekly-period-hint">留空默认为本周（{week.start} ~ {week.end}）</span>
          )}
          {(periodStart || periodEnd) && (
            <button className="btn btn-sm" onClick={() => { setPeriodStart(''); setPeriodEnd(''); }} disabled={generating}>
              重置
            </button>
          )}
        </div>

        {/* Git scope: which branch(es) and whose commits to summarize */}
        <div className="weekly-period-row" style={{ marginTop: 8 }}>
          <span className="weekly-period-label">分支</span>
          <select
            className="form-input weekly-branch-select"
            value={branchChoice}
            onChange={e => setBranchChoice(e.target.value)}
            disabled={generating || gitInfo?.is_git === false}
          >
            <option value="current">
              当前分支{gitInfo?.current_branch ? `（${gitInfo.current_branch}）` : ''}
            </option>
            <option value="all">全部分支</option>
            {(gitInfo?.branches ?? [])
              .filter(b => b !== gitInfo?.current_branch)
              .map(b => <option key={b} value={b}>{b}</option>)}
          </select>

          <span className="weekly-period-label" style={{ marginLeft: 8 }}>作者</span>
          <input
            className="form-input weekly-author-input"
            list="weekly-report-authors"
            placeholder="留空 = 全部作者"
            value={author}
            onChange={e => setAuthor(e.target.value)}
            disabled={generating || gitInfo?.is_git === false}
          />
          <datalist id="weekly-report-authors">
            {(gitInfo?.authors ?? []).map(a => <option key={a} value={a} />)}
          </datalist>

          {gitInfo?.is_git === false && (
            <span className="weekly-period-hint">该项目不是 git 仓库，将仅基于需求数据生成</span>
          )}
        </div>

        {/* Diff analysis: summarize from actual code changes, not just messages */}
        <div className="weekly-period-row" style={{ marginTop: 8 }}>
          <label className="weekly-diff-toggle" title="逐条读取提交的代码 diff 交给 AI 总结">
            <input
              type="checkbox"
              checked={diffAnalysis}
              onChange={e => setDiffAnalysis(e.target.checked)}
              disabled={generating || gitInfo?.is_git === false}
            />
            <span>深度分析代码改动</span>
          </label>
          <span className="weekly-period-hint">
            squash merge 的提交描述经常不全；勾选后基于逐条提交的代码 diff 总结，可补全未提及的功能（生成更慢）
          </span>
        </div>

        <div className="form-group" style={{ marginTop: 12 }}>
          <label style={{ fontSize: 12, fontWeight: 600, color: 'var(--color-text-secondary)', display: 'block', marginBottom: 4 }}>
            生成规则（自定义周报的结构、语气与侧重点，生成时注入 AI 提示词）
          </label>
          <div className="weekly-preset-row">
            <span className="weekly-period-label">内置模板</span>
            <select
              className="form-input weekly-preset-select"
              value={presetChoice}
              onChange={e => setPresetChoice(e.target.value)}
              disabled={generating}
            >
              <option value="standard">标准（进展 + 需求 + 统计 + 计划 + 风险）</option>
              <option value="compact">简洁（仅本周进展）</option>
            </select>
            <button className="btn btn-sm" onClick={applyPreset} disabled={generating || !rulePresets[presetChoice]}>
              填入
            </button>
            {ruleDirty && <span className="weekly-period-hint">已修改，点「保存规则」生效</span>}
          </div>
          <textarea
            className="form-input weekly-rule-input"
            rows={9}
            value={rule}
            onChange={e => setRule(e.target.value)}
            disabled={generating}
          />
        </div>

        <div className="weekly-actions">
          <button
            className="btn btn-secondary"
            onClick={handleSaveRule}
            disabled={!ruleDirty || ruleSaving || generating}
          >
            {ruleSaving ? '保存中...' : ruleSaved ? '已保存 ✓' : '保存规则'}
          </button>
          <button
            className="btn btn-primary"
            onClick={handleGenerate}
            disabled={generating}
          >
            {generating ? '生成中...' : '🚀 生成周报'}
          </button>
          {genError && <span className="weekly-gen-error">{genError}</span>}
        </div>
      </div>

      {/* ── Live generation stream ── */}
      {(genLines.length > 0 || generating) && (
        <div className="detail-section" style={{ marginTop: 16 }}>
          <div className="section-header" style={{ marginBottom: 8 }}>
            <span style={{ fontWeight: 600, fontSize: 14 }}>生成过程</span>
            {!generating && (
              <button className="btn btn-secondary btn-sm" onClick={() => setGenLines([])}>清除</button>
            )}
          </div>
          <div className="coding-panel" ref={panelRef} style={{ maxHeight: 320 }}>
            {genLines.map((line, i) => (
              <div key={i} className={`coding-line coding-line-${line.type}`}>{line.content}</div>
            ))}
            {generating && <div className="coding-line coding-line-tool_call">● Claude 正在撰写周报...</div>}
          </div>
        </div>
      )}

      {/* ── History ── */}
      <div className="detail-section" style={{ marginTop: 16 }}>
        <div className="section-header" style={{ marginBottom: 12 }}>
          <span style={{ fontWeight: 600, fontSize: 14 }}>历史周报（{reports.length}）</span>
        </div>

        {listLoading && <div className="tab-empty">⏳ 加载中...</div>}

        {!listLoading && reports.length === 0 && (
          <div className="tab-empty"><p>还没有生成过周报，点击上方「生成周报」试试。</p></div>
        )}

        {!listLoading && reports.length > 0 && (
          <table className="pr-table">
            <thead>
              <tr>
                <th style={{ width: 210 }}>统计周期</th>
                <th style={{ width: 190 }}>范围</th>
                <th style={{ width: 160 }}>生成时间</th>
                <th style={{ width: 90 }}>状态</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {reports.map(r => (
                <tr key={r.id}>
                  <td><code className="pr-branch">{r.period_start} ~ {r.period_end}</code></td>
                  <td style={{ fontSize: 12, color: 'var(--color-text-secondary)' }}>
                    <code className="pr-branch">{r.git_branch || '全部分支'}</code>
                    {' / '}
                    {r.git_author || '全部作者'}
                    {rulePresets.compact && r.rule === rulePresets.compact && (
                      <span className="weekly-compact-tag">简洁</span>
                    )}
                  </td>
                  <td style={{ color: 'var(--color-text-muted)', fontSize: 12 }}>
                    {new Date(r.created_at).toLocaleString('zh-CN', { hour12: false })}
                  </td>
                  <td>
                    <span className={`status-badge ${r.status === 'done' ? 'status-done' : 'status-error'}`}>
                      {r.status === 'done' ? '✅ 完成' : '❌ 失败'}
                    </span>
                  </td>
                  <td style={{ textAlign: 'right' }}>
                    <button className="btn btn-primary btn-sm" onClick={() => setViewing(r)}>查看</button>
                    <button
                      className="btn btn-danger btn-sm"
                      style={{ marginLeft: 8 }}
                      onClick={() => handleDelete(r)}
                      disabled={deletingId === r.id}
                    >
                      {deletingId === r.id ? '删除中...' : '删除'}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {viewing && (
        <MarkdownViewer
          title={`周报 ${viewing.period_start} ~ ${viewing.period_end}`}
          content={viewing.content}
          onClose={() => setViewing(null)}
        />
      )}
    </div>
  );
}
