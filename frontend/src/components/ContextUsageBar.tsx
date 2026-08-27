/**
 * ContextUsageBar — 实时显示 claude 当前会话的上下文占用比例,并触发"压缩上下文"操作。
 *
 * 数据来源
 *   - 父组件在 SSE 流里订阅 `usage` 事件(`LogLine.usage`),每次 result 事件到来
 *     都会推送一份新的快照(input/output/cache_creation/cache_read + context_window + pct)。
 *   - `usage === undefined` 时表示本回合还没结束,渲染一个静态的"待首次响应"占位。
 *
 * 交互
 *   - 单击条形 → 调用 `onCompress()`(由父组件传入,实际触发 wizardApi.compressContext)。
 *   - 条形上始终显示百分比 + "已用 / 上限";≥80% 时变橙,≥95% 时变红,这是触觉提示而非阻断。
 *   - `disabled` 时(没有可压缩的会话)整条按钮灰掉,不响应点击。
 *
 * 排版
 *   - 标签行(模型 + 步骤)和数值行(百分比)各一行,条形紧贴底部;整体紧凑,
 *     适配 DeepRefineChat / DocRefineChat / CodingChat 三处头部的高度限制。
 *   - 颜色直接复用 Design System 的 primary / warning / error token,保持主题一致。
 */

import type { UsageInfo } from '../utils/logLines';
import { clampPct, bandClass, formatTokens } from '../utils/logLines';

export interface ContextUsageBarProps {
  /** 最新一次 result 事件的快照;未到结果事件时为 undefined(显示 0%)。 */
  usage?: UsageInfo;
  /** 触发压缩;父组件通常会弹确认框 + 调用 wizardApi.compressContext。 */
  onCompress: () => void;
  /** true 时按钮禁用(无 session_id / 正在压缩 / 不允许压缩)。 */
  disabled?: boolean;
  /** 压缩中的 spinner 状态;显示在按钮文案里。 */
  compressing?: boolean;
  /** 步骤中文标签(如 "需求分析师"),用作 tooltip 与可访问性 label。 */
  stepLabel: string;
  /**
   * 可选:压缩后写入的摘要预览。
   * 父组件在压缩成功后传入,这里渲染一个小的"已压缩 · 查看摘要"链接,
   * 整体行为保持自包含(单条 `<button>` 即可触发压缩)。
   */
  compressedAt?: string | null;
  onShowSummary?: () => void;
  /**
   * 是否展示"压缩上下文"按钮。设计阶段不需要压缩(方案是一次性生成的
   * plan-mode 产物,微调对话没有压缩价值),传 false 即只保留用量展示,
   * 不渲染按钮、不响应点击。默认 true(分析师 / 开发阶段照常压缩)。
   */
  compressible?: boolean;
}

const COMPRESS_LABEL = '📦 压缩上下文';
const COMPRESSING_LABEL = '⏳ 压缩中…';
const COMPRESSED_LABEL = '📦 已压缩 · 查看摘要';

// clampPct / bandClass / formatTokens are imported from utils/logLines so the
// in-panel bars and the top SessionContextStrip share one set of thresholds.

export function ContextUsageBar({
  usage,
  onCompress,
  disabled,
  compressing,
  stepLabel,
  compressedAt,
  onShowSummary,
  compressible = true,
}: ContextUsageBarProps) {
  // 没有 usage 事件时(本轮没结束),给一个 0% 的稳定占位;条形不抖动。
  const used = usage?.used ?? 0;
  const window = usage?.context_window ?? 0;
  const pct = usage?.pct ?? 0;
  const widthPct = clampPct(pct);
  const modelLabel = usage?.model || '—';
  const usedLabel = `${formatTokens(used)} / ${window ? formatTokens(window) : '?'}`;
  const showCompressed = !!compressedAt && !compressing;

  // 按钮文案 + 行为:有摘要则显示"查看摘要",否则显示"压缩上下文";压缩中显示 spinner。
  const buttonLabel = compressing
    ? COMPRESSING_LABEL
    : showCompressed
      ? COMPRESSED_LABEL
      : COMPRESS_LABEL;
  const handleClick = () => {
    if (disabled || compressing) return;
    if (showCompressed && onShowSummary) {
      onShowSummary();
      return;
    }
    onCompress();
  };

  return (
    <div
      className="usage-bar"
      role="group"
      aria-label={`${stepLabel}上下文使用量`}
    >
      <div className="usage-bar-header">
        <span className="usage-bar-step">{stepLabel}</span>
        <span className="usage-bar-model" title={modelLabel}>{modelLabel}</span>
        <span className="usage-bar-used" title={`${used} / ${window} tokens`}>
          {usedLabel}
        </span>
        <span
          className={`usage-bar-pct ${pct >= 95 ? 'usage-bar-pct-critical' : pct >= 80 ? 'usage-bar-pct-warn' : ''}`}
          title={`原始使用率 ${pct.toFixed(1)}%`}
        >
          {pct.toFixed(0)}%
        </span>
        {compressible && (
          <button
            type="button"
            className="usage-bar-btn"
            onClick={handleClick}
            disabled={!!disabled && !showCompressed}
            title={showCompressed ? '查看已压缩摘要' : '让 claude 总结当前会话并清空上下文'}
          >
            {buttonLabel}
          </button>
        )}
      </div>
      <div className="usage-bar-track" aria-hidden="true">
        <div
          className={bandClass(pct)}
          style={{ width: `${widthPct}%` }}
        />
      </div>
    </div>
  );
}

export default ContextUsageBar;