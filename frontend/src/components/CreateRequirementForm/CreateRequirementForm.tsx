// 新建需求表单（独立组件）
//
// 面板按「先分类 → 再描述 → 后设置」三段组织：
//   1. 顶部 segmented 类型切换（问题 / 需求 / 想法），只占一行，选中后
//      在下方给出该类型需要补充的信息提示。
//   2. 描述输入是面板主体：自动聚焦、字数计数、⌘/Ctrl+Enter 直接提交；
//      描述框下方一行收「跳过 AI 整理」的开关与说明。
//   3. 归属项目 / 优先级 / 开发流程属于次要设置，放在描述之下；开发流程
//      用与类型切换同款的 segmented 单选（完整流程 / 跳过分析 / 直接开发），
//      默认「直接开发」，让最常见的「小改动立即动工」成为 0-点击路径。
//
// kind=idea 不显示优先级与流程（想法尚未确定是否实施，固定走分析讨论）；
// 「跳过 AI 整理」开关对所有类型都生效——它控制的是描述是否进入
// LLM-organized Markdown 步骤，与后续走哪条流程无关。
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

// 流程选项的展示文案保持极简：与上方需求类型切换同款 segmented 控件
// 共用一套视觉语言，单行排开，长度由字数最多的标签决定。
const FLOW_OPTIONS: { value: Flow; label: string }[] = [
  { value: 'direct', label: '直接开发' },
  { value: 'skip-analysis', label: '跳过分析' },
  { value: 'full', label: '完整流程' },
];

const FLOW_NOTES: Record<Flow, string> = {
  full: '需求还不清楚，先和 AI 讨论清楚，再出方案再开发',
  'skip-analysis': '需求已经清楚，直接出方案再开发',
  direct: '小改动，创建后立即进入开发',
};

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
  // Default to 「直接开发」 so the common "small change" path is zero-clicks.
  const [flow, setFlow] = useState<Flow>('direct');
  // Skip the LLM-organized description pass by default. Most creators already
  // write their own structured prose; the LLM round-trip is mostly cost with
  // marginal gain. Users who want the structured output opt back in.
  const [skipOrganize, setSkipOrganize] = useState(true);
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
        skip_organize: skipOrganize,
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
        {/* Skip-LLM-organize toggle. Lives directly under the description
            because the toggle's only effect is on how that text gets stored.
            Default ON (skip): the panel is a fast notepad, not an editor that
            runs an LLM round-trip on every submit. */}
        <label className={`create-req-toggle${skipOrganize ? ' on' : ''}`}>
          <input
            type="checkbox"
            checked={skipOrganize}
            onChange={(e) => setSkipOrganize(e.target.checked)}
            disabled={saving}
          />
          <span className="create-req-toggle-track" aria-hidden>
            <span className="create-req-toggle-thumb" />
          </span>
          <span className="create-req-toggle-text">
            <span className="create-req-toggle-label">跳过 AI 整理</span>
            <span className="create-req-toggle-note">
              {skipOrganize
                ? '提交后直接保存原始描述，不调用 AI'
                : '提交后让 AI 把描述整理为结构化 Markdown'}
            </span>
          </span>
        </label>
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
          {/* Shared segmented-control style — same look as the kind switcher
              so the panel reads as one visual family rather than two
              competing selection patterns. The note below explains what the
              current selection actually does (the segmented control only
              shows the labels). */}
          <div className="create-req-flow" role="radiogroup" aria-label="开发流程">
            {FLOW_OPTIONS.map((opt) => (
              <button
                key={opt.value}
                type="button"
                role="radio"
                aria-checked={flow === opt.value}
                className={`create-req-kind-tab${flow === opt.value ? ' selected' : ''}`}
                onClick={() => setFlow(opt.value)}
                disabled={saving}
                data-flow={opt.value}
              >
                {opt.label}
              </button>
            ))}
          </div>
          <p className="create-req-flow-note">{FLOW_NOTES[flow]}</p>
        </div>
      )}

      {error && (
        <div className="create-req-error" role="alert">{error}</div>
      )}

      <div className="form-actions">
        <span className="create-req-shortcut" aria-hidden>⌘/Ctrl + Enter 提交</span>
        <button className="btn" onClick={onClose} disabled={saving}>取消</button>
        <button className="btn btn-primary" onClick={handleSubmit} disabled={!canSubmit}>
          {saving
            ? skipOrganize ? '创建中…' : '创建中…AI 正在整理'
            : kindCreateLabels[kind]}
        </button>
      </div>
    </div>
  );
}
