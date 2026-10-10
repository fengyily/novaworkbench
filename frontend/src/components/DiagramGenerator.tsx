// DiagramGenerator — 触发一次 AI 出图、并保存为知识条目的 Dialog。
//
// 流：用户在左侧 Textarea 输入（自由文本描述 + 选 type hint）→ 点「生成」
// → 后端 /api/knowledge/generate-diagram 返回 {kind, title, mermaid, …} →
// 右侧实时渲染 → 用户可在 mermaid 源码框微调 → 点「保存为知识条目」
// 即可。Console-style 二次用法（"再生成一版"）保留旧版本对比，临时
// 文件留着让用户检查 / debug；产物最终落 knowledge 表（不写项目目录）。
//
// i18n 暂时只用 zh-CN（与项目其他"立即"Dialog 一致），后期待补 en。
import { useEffect, useState } from 'react';
import DiagramPreview from './DiagramPreview';
import { knowledgeApi } from '../api/client';
import './DiagramGenerator.css';

const EXAMPLES = [
  '用户提交登录 → 校验手机号 → 发送验证码 → 比对验证码 → 颁发 token → 跳转首页',
  '订单状态机：待支付 → 已支付 → 已发货 → 已签收 → 已退款 / 售后',
];

const KIND_OPTIONS = [
  { value: '', label: '自动判断（按内容）' },
  { value: 'flowchart', label: 'flowchart（流程/分层）' },
  { value: 'sequenceDiagram', label: 'sequenceDiagram（时序）' },
  { value: 'stateDiagram-v2', label: 'stateDiagram-v2（状态机）' },
  { value: 'classDiagram', label: 'classDiagram（类图）' },
  { value: 'erDiagram', label: 'erDiagram（数据模型）' },
  { value: 'gantt', label: 'gantt（甘特）' },
];

interface Props {
  open: boolean;
  onClose: () => void;
  projectId: string;
  initialInput?: string;
  initialKind?: string;
  onSaved?: (row: { id: string; title: string }) => void;
}

type Mode = 'idle' | 'generating' | 'ready' | 'saving' | 'saved' | 'error';

export default function DiagramGenerator({ open, onClose, projectId, initialInput, initialKind, onSaved }: Props) {
  const [input, setInput] = useState(initialInput || '');
  const [kindHint, setKindHint] = useState(initialKind || '');
  const [mermaid, setMermaid] = useState('');
  const [title, setTitle] = useState('');
  const [kind, setKind] = useState('');
  const [description, setDescription] = useState('');
  const [mode, setMode] = useState<Mode>('idle');
  const [errorMsg, setErrorMsg] = useState('');

  // Reset state whenever the dialog opens.
  useEffect(() => {
    if (open) {
      setInput(initialInput || '');
      setKindHint(initialKind || '');
      setMermaid('');
      setTitle('');
      setKind('');
      setDescription('');
      setMode('idle');
      setErrorMsg('');
    }
  }, [open, initialInput, initialKind]);

  if (!open) return null;

  async function handleGenerate() {
    if (!input.trim()) return;
    setMode('generating');
    setErrorMsg('');
    try {
      const res = await knowledgeApi.generateDiagramPreview({
        project_id: projectId,
        input,
        kind: kindHint || undefined,
      });
      setMermaid(res.mermaid);
      setTitle(res.title || '');
      setKind(res.kind || '');
      setDescription(res.description || '');
      setMode('ready');
    } catch (e: unknown) {
      setErrorMsg(String((e as Error)?.message || e));
      setMode('error');
    }
  }

  async function handleSave() {
    if (!mermaid.trim()) return;
    setMode('saving');
    setErrorMsg('');
    try {
      // Send the (possibly hand-edited) mermaid source as override so the
      // backend persists exactly what the user sees in the preview pane
      // — no second LLM call, no surprise re-generation.
      const res = await knowledgeApi.generateDiagram({
        project_id: projectId,
        input,
        kind: kindHint || undefined,
        force_overwrite_mermaid: mermaid.trim(),
      });
      setMode('saved');
      if (onSaved) {
        onSaved({ id: res.id, title: res.title });
      } else {
        // Default: open the new row in a new tab so the user sees what landed.
        window.open(`/knowledge/view/${res.id}`, '_blank');
      }
      setTimeout(() => {
        onClose();
      }, 400);
    } catch (e: unknown) {
      setErrorMsg(String((e as Error)?.message || e));
      setMode('error');
    }
  }

  return (
    <div className="diagram-generator-overlay modal-fullscreen-overlay" onClick={onClose}>
      <div className="diagram-generator-box modal-fullscreen" onClick={(e) => e.stopPropagation()}>
        <div className="diagram-generator-header">
          <h3>🧠 生成 Mermaid 架构图</h3>
          <button className="btn btn-sm" onClick={onClose} aria-label="close">
            ✕
          </button>
        </div>
        <div className="diagram-generator-body">
          <div className="diagram-generator-left">
            <label className="dg-label">
              <span className="dg-label-text">描述要可视化的内容</span>
              <textarea
                className="dg-textarea"
                value={input}
                onChange={(e) => setInput(e.target.value)}
                placeholder="支持中文 / 英文 / 多种长度。后端会判断 flow / sequence / state / class / ER / gantt。"
                rows={8}
              />
            </label>
            <div className="dg-row">
              <label className="dg-label">
                <span className="dg-label-text">类型偏好（可空）</span>
                <select
                  className="dg-select"
                  value={kindHint}
                  onChange={(e) => setKindHint(e.target.value)}
                >
                  {KIND_OPTIONS.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </select>
              </label>
              <button
                className="btn btn-primary"
                disabled={!input.trim() || mode === 'generating' || mode === 'saving'}
                onClick={handleGenerate}
              >
                {mode === 'generating' ? '生成中…' : '🔄 生成'}
              </button>
            </div>
            <div className="dg-examples">
              <span className="dg-label-text">示例：</span>
              {EXAMPLES.map((ex) => (
                <button
                  key={ex}
                  className="dg-chip"
                  type="button"
                  onClick={() => setInput(ex)}
                  title="点击填充"
                >
                  {ex.slice(0, 24)}…
                </button>
              ))}
            </div>
            <label className="dg-label">
              <span className="dg-label-text">Mermaid 源码（可手调）</span>
              <textarea
                className="dg-textarea dg-mermaid-src"
                value={mermaid}
                onChange={(e) => setMermaid(e.target.value)}
                placeholder="生成结果会出现在这里；你可以调整节点名 / 连线，右侧预览会实时刷新。"
                rows={10}
              />
            </label>
            <div className="dg-meta">
              {title && (
                <div>
                  <strong>标题:</strong> {title}
                </div>
              )}
              {kind && (
                <div>
                  <strong>类型:</strong> {kind}
                </div>
              )}
              {description && (
                <div>
                  <strong>说明:</strong> {description}
                </div>
              )}
            </div>
            {errorMsg && <div className="dg-error">{errorMsg}</div>}
          </div>
          <div className="diagram-generator-right">
            <div className="dg-preview-label">实时预览</div>
            {mermaid.trim() ? (
              <DiagramPreview source={mermaid} />
            ) : (
              <div className="dg-preview-empty">先在左侧输入文字并点「生成」</div>
            )}
          </div>
        </div>
        <div className="diagram-generator-footer">
          <button className="btn" onClick={onClose} disabled={mode === 'saving' || mode === 'generating'}>
            取消
          </button>
          <button
            className="btn btn-primary"
            disabled={!mermaid.trim() || mode === 'generating' || mode === 'saving' || mode === 'saved'}
            onClick={handleSave}
          >
            {mode === 'saving' ? '保存中…' : mode === 'saved' ? '✓ 已保存' : '保存为知识条目'}
          </button>
        </div>
      </div>
    </div>
  );
}
