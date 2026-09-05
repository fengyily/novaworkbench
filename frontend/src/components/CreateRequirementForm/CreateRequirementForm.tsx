// 新建需求表单（独立组件）
//
// 面板按「先分类 → 再描述 → 后设置」三段组织：
//   1. 顶部 segmented 类型切换（问题 / 需求 / 想法），只占一行，选中后
//      在下方给出该类型需要补充的信息提示。
//   2. 描述输入是面板主体：自动聚焦、字数计数、⌘/Ctrl+Enter 直接提交。
//   3. 归属项目 / 优先级 / 开发流程属于次要设置，放在描述之下。
//
// 开发流程用「阶段管线」呈现：分析 → 设计 → 开发，被跳过的阶段置灰划线，
// 让用户一眼看出这次创建会走到哪一步，而不是读三行文字去比较差异。
// kind=idea 不显示优先级与流程（想法尚未确定是否实施，固定走分析讨论）。
//
// props:
//   - projectId / projectOptions：必填一个。projectId 表示"已知项目内创建"，
//     projectOptions 表示"从跨项目列表/Dashboard 创建"，会渲染一个项目下拉。
//   - onCreated / onClose：成功回调与关闭回调。

import { useEffect, useRef, useState } from 'react';
import AtMentionTextarea from '../AtMentionTextarea';
import { kindHints, kindLabels, kindPlaceholders, kindCreateLabels, type Kind, requirementsApi, type Requirement } from '../../api/client';
import './CreateRequirementForm.css';

type Flow = 'full' | 'skip-analysis' | 'direct';

// Each flow is described by which of the three lifecycle stages it runs.
// The pipeline chips render straight from this table, so the UI can never
// drift from what the flow actually does.
const FLOW_STAGES: Record<Flow, { analysis: boolean; design: boolean }> = {
  full: { analysis: true, design: true },
  'skip-analysis': { analysis: false, design: true },
  direct: { analysis: false, design: false },
};

const FLOW_OPTIONS: { value: Flow; label: string; note: string }[] = [
  { value: 'skip-analysis', label: '跳过分析', note: '需求已经清楚，直接出方案' },
  { value: 'direct', label: '直接开发', note: '小改动，创建后立即进入开发' },
  { value: 'full', label: '标准流程', note: '需求还需要和 AI 讨论清楚' },
];

const KIND_SUBMIT_HINTS: Record<Kind, string> = {
  issue: '提交后 AI 整理为 Bug 报告：现象 / 复现步骤 / 期望行为 / 实际行为。',
  idea: '提交后 AI 整理为灵感记录：灵感来源 / 初步设想 / 待回答的关键问题。',
  requirement: '提交后 AI 整理为结构化文档：背景 / 目标 / 功能要点 / 验收标准。',
};

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
  const rootRef = useRef<HTMLDivElement>(null);

  // Cross-project mode: pick the first project as a sensible default so the
  // submit button isn't disabled on first paint.
  useEffect(() => {
    if (!fixedProjectId && projectOptions && projectOptions.length > 0 && !projectId) {
      setProjectId(projectOptions[0].id);
    }
  }, [fixedProjectId, projectOptions, projectId]);

  // The description is the one thing every user has to fill in — put the
  // caret there on mount so opening the panel is one click, not two.
  useEffect(() => {
    rootRef.current?.querySelector('textarea')?.focus();
  }, []);

  const handleSubmit = async () => {
    if (saving) return;
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
      // Idea kind is discussion-first by design: drop the user straight into
      // the analyst stage so they can talk to Claude about feasibility. The
      // architect/dev stages remain hidden in the UI, so the `flow` controls
      // (and any skip flags) only apply to issue/requirement.
      const skipAnalysis = kind === 'idea' ? false : flow !== 'full';
      const skipDesign = kind === 'idea' ? false : flow === 'direct';
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

  const handleTextareaKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      handleSubmit();
    }
  };

  const showOptions = kind !== 'idea';
  const showProjectPicker = !fixedProjectId && !!projectOptions && projectOptions.length > 0;
  const canSubmit = !saving && !!description.trim() && !!projectId;
  const charCount = description.trim().length;

  return (
    <div className="create-req-form" data-testid="create-requirement-form" ref={rootRef}>
      <div className="create-req-form-header">
        <h3>新需求</h3>
        <button className="btn btn-secondary btn-sm" onClick={onClose} disabled={saving}>收起</button>
      </div>

      {/* Step 1 — Kind switcher. One row, so the panel opens on the writing
          area rather than on a wall of category cards. */}
      <div className="create-req-kind" role="radiogroup" aria-label="需求类型">
        {(Object.keys(kindLabels) as Kind[]).map((k) => (
          <button
            key={k}
            type="button"
            role="radio"
            aria-checked={kind === k}
            className={`create-req-kind-tab${kind === k ? ' selected' : ''}`}
            onClick={() => setKind(k)}
            disabled={saving}
            data-kind={k}
          >
            {kindLabels[k]}
          </button>
        ))}
      </div>
      <p className="create-req-kind-hint">{kindHints[kind]}</p>

      {/* Step 2 — Description (the panel's main job) */}
      <div className="form-group create-req-desc">
        <AtMentionTextarea
          value={description}
          onChange={setDescription}
          onKeyDown={handleTextareaKeyDown}
          className="form-input"
          rows={7}
          placeholder={kindPlaceholders[kind]}
          disabled={saving}
        />
        <div className="create-req-desc-meta">
          <small className="form-hint">{KIND_SUBMIT_HINTS[kind]}</small>
          <span className="create-req-count">{charCount} 字</span>
        </div>
      </div>

      {/* Step 3 — Secondary settings */}
      {(showProjectPicker || showOptions) && (
        <div className="create-req-meta-row">
          {showProjectPicker && (
            <div className="form-group">
              <label htmlFor="create-req-project">归属项目</label>
              <select
                id="create-req-project"
                className="form-input"
                value={projectId}
                onChange={(e) => setProjectId(e.target.value)}
                disabled={saving}
              >
                <option value="" disabled>请选择项目</option>
                {projectOptions!.map((p) => (
                  <option key={p.id} value={p.id}>{p.name}</option>
                ))}
              </select>
            </div>
          )}

          {showOptions && (
            <div className="form-group">
              <label htmlFor="create-req-priority">优先级</label>
              <select
                id="create-req-priority"
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
          )}
        </div>
      )}

      {showOptions && (
        <div className="form-group">
          <label>开发流程</label>
          <div className="create-req-flow" role="radiogroup" aria-label="开发流程">
            {FLOW_OPTIONS.map((opt) => {
              const stages = FLOW_STAGES[opt.value];
              return (
                <button
                  key={opt.value}
                  type="button"
                  role="radio"
                  aria-checked={flow === opt.value}
                  className={`create-req-flow-option${flow === opt.value ? ' selected' : ''}`}
                  onClick={() => setFlow(opt.value)}
                  disabled={saving}
                >
                  <span className="create-req-flow-pipeline" aria-hidden>
                    <span className={`stage${stages.analysis ? '' : ' skipped'}`}>分析</span>
                    <span className={`stage${stages.design ? '' : ' skipped'}`}>设计</span>
                    <span className="stage">开发</span>
                  </span>
                  <span className="create-req-flow-text">
                    <span className="create-req-flow-label">{opt.label}</span>
                    <span className="create-req-flow-note">{opt.note}</span>
                  </span>
                </button>
              );
            })}
          </div>
        </div>
      )}

      {error && (
        <div className="create-req-error" role="alert">{error}</div>
      )}

      <div className="form-actions">
        <span className="create-req-shortcut" aria-hidden>⌘/Ctrl + Enter 提交</span>
        <button className="btn" onClick={onClose} disabled={saving}>取消</button>
        <button className="btn btn-primary" onClick={handleSubmit} disabled={!canSubmit}>
          {saving ? '创建中…AI 正在整理' : kindCreateLabels[kind]}
        </button>
      </div>
    </div>
  );
}
