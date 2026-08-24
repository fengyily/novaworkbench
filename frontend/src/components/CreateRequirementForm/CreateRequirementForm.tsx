// 新建需求表单（独立组件）
//
// 在创建阶段就要求用户选择「问题 / 需求 / 想法」三类 kind，并按 kind 切换：
//   1. 占位提示（textarea placeholder）
//   2. 卡片下方的 hint 文案
//   3. 优先级下拉（kind=idea 不显示，idea 暂未确定是否实施）
//   4. 开发流程 radio（kind=idea 不显示，idea 不进入开发）
//   5. 提交按钮文案（按 kind 切）
// 这样把分类引导提前到输入前，避免「想法」被套上功能需求模板。
//
// props:
//   - projectId / projectOptions：必填一个。projectId 表示"已知项目内创建"，
//     projectOptions 表示"从跨项目列表/Dashboard 创建"，会渲染一个项目下拉。
//   - onCreated / onClose：成功回调与关闭回调。

import { useEffect, useState } from 'react';
import AtMentionTextarea from '../AtMentionTextarea';
import { kindHints, kindLabels, kindPlaceholders, kindCreateLabels, type Kind, requirementsApi, type Requirement } from '../../api/client';
import './CreateRequirementForm.css';

type Flow = 'full' | 'skip-analysis' | 'direct';

export interface CreateRequirementFormProps {
  projectId?: string;
  projectOptions?: { id: string; name: string }[];
  defaultKind?: Kind;
  onClose: () => void;
  onCreated: (req: Requirement) => void;
}

export function CreateRequirementForm({
  projectId: fixedProjectId,
  projectOptions,
  defaultKind = 'requirement',
  onClose,
  onCreated,
}: CreateRequirementFormProps) {
  const [kind, setKind] = useState<Kind>(defaultKind);
  const [description, setDescription] = useState('');
  const [priority, setPriority] = useState('medium');
  const [flow, setFlow] = useState<Flow>('skip-analysis');
  const [projectId, setProjectId] = useState(fixedProjectId || '');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Cross-project mode: pick the first project as a sensible default so the
  // submit button isn't disabled on first paint.
  useEffect(() => {
    if (!fixedProjectId && projectOptions && projectOptions.length > 0 && !projectId) {
      setProjectId(projectOptions[0].id);
    }
  }, [fixedProjectId, projectOptions, projectId]);

  const handleSubmit = async () => {
    if (!description.trim()) {
      setError('请填写描述');
      return;
    }
    if (!projectId) {
      setError('请选择项目');
      return;
    }
    setSaving(true);
    setError(null);
    try {
      // Idea kind never reaches the developer stage — the backend StartCoding
      // guard rejects it, but we also pre-set skip_analysis/skip_design so the
      // legacy fields stay consistent.
      const skipAnalysis = kind === 'idea' ? true : flow !== 'full';
      const skipDesign = kind === 'idea' ? true : flow === 'direct';
      const created = await requirementsApi.create({
        project_id: projectId,
        description,
        priority: kind === 'idea' ? 'medium' : priority,
        kind,
        skip_analysis: skipAnalysis,
        skip_design: skipDesign,
      });
      onCreated(created);
    } catch (err: any) {
      setError(err?.message || '创建失败');
    } finally {
      setSaving(false);
    }
  };

  const showPriority = kind !== 'idea';
  const showFlow = kind !== 'idea';

  return (
    <div className="create-req-form" data-testid="create-requirement-form">
      <div className="create-req-form-header">
        <h3>新需求</h3>
        <button className="btn btn-secondary btn-sm" onClick={onClose} disabled={saving}>收起</button>
      </div>

      {/* Step 1 — Kind picker (3 cards) */}
      <div className="form-group">
        <label>请选择类型</label>
        <div className="kind-card-grid">
          {(Object.keys(kindLabels) as Kind[]).map((k) => (
            <button
              key={k}
              type="button"
              className={`kind-card kind-card-${k}${kind === k ? ' selected' : ''}`}
              onClick={() => setKind(k)}
              disabled={saving}
              data-kind={k}
            >
              <div className="kind-card-title">{kindLabels[k]}</div>
              <div className="kind-card-hint">{kindHints[k]}</div>
              {kind === k && <span className="kind-card-check" aria-hidden>✓</span>}
            </button>
          ))}
        </div>
      </div>

      {/* Step 2 — Project + description */}
      {!fixedProjectId && projectOptions && projectOptions.length > 0 && (
        <div className="form-group">
          <label>归属项目</label>
          <select
            className="form-input"
            value={projectId}
            onChange={(e) => setProjectId(e.target.value)}
            disabled={saving}
          >
            <option value="" disabled>请选择项目</option>
            {projectOptions.map((p) => (
              <option key={p.id} value={p.id}>{p.name}</option>
            ))}
          </select>
        </div>
      )}

      <div className="form-group">
        <label>需求内容 (必填)</label>
        <AtMentionTextarea
          value={description}
          onChange={setDescription}
          className="form-input"
          rows={6}
          placeholder={kindPlaceholders[kind]}
        />
        <small className="form-hint">
          {kind === 'issue'
            ? '提交后由 AI 整理为 Bug 报告格式（现象 / 复现步骤 / 期望行为 / 实际行为）并提炼标题。'
            : kind === 'idea'
              ? '提交后由 AI 整理为灵感记录（灵感来源 / 初步设想 / 待回答的关键问题），不强求完整需求模板。'
              : '提交后由 AI 整理为结构化 Markdown（背景 / 目标 / 功能要点 / 验收标准）并提炼标题，可在详情页继续编辑。'}
        </small>
      </div>

      {/* Step 3 — Per-kind fields */}
      {showPriority && (
        <div className="form-row">
          <div className="form-group" style={{ flex: 1 }}>
            <label>优先级</label>
            <select
              value={priority}
              onChange={(e) => setPriority(e.target.value)}
              className="form-input"
              disabled={saving}
            >
              <option value="high">🔴 High</option>
              <option value="medium">🟡 Medium</option>
              <option value="low">🟢 Low</option>
            </select>
          </div>
        </div>
      )}

      {showFlow && (
        <div className="form-group">
          <label>开发流程</label>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
            <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', userSelect: 'none' }}>
              <input
                type="radio"
                name="dev-flow"
                checked={flow === 'skip-analysis'}
                onChange={() => setFlow('skip-analysis')}
                style={{ width: 'auto' }}
                disabled={saving}
              />
              <span>跳过分析（直接方案设计 → 开发）</span>
            </label>
            <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', userSelect: 'none' }}>
              <input
                type="radio"
                name="dev-flow"
                checked={flow === 'direct'}
                onChange={() => setFlow('direct')}
                style={{ width: 'auto' }}
                disabled={saving}
              />
              <span>直接开发（跳过分析与设计）</span>
            </label>
            <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', userSelect: 'none' }}>
              <input
                type="radio"
                name="dev-flow"
                checked={flow === 'full'}
                onChange={() => setFlow('full')}
                style={{ width: 'auto' }}
                disabled={saving}
              />
              <span>标准流程（分析 → 设计 → 开发）</span>
            </label>
          </div>
          <small style={{ display: 'block', marginTop: 4, color: 'var(--text-secondary, #64748B)' }}>
            小改动可选「直接开发」，跳过分析与设计阶段，创建后直接在详情页进入开发实现。
          </small>
        </div>
      )}

      {error && (
        <div className="form-error" role="alert" style={{ color: 'var(--color-error, #EF4444)', marginBottom: 8 }}>
          {error}
        </div>
      )}

      <div className="form-actions">
        <button className="btn" onClick={onClose} disabled={saving}>取消</button>
        <button
          className="btn btn-primary"
          onClick={handleSubmit}
          disabled={saving || !description.trim() || !projectId}
        >
          {saving ? '创建中...（AI 整理中）' : kindCreateLabels[kind]}
        </button>
      </div>
    </div>
  );
}
