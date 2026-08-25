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
}

const COMPRESS_LABEL = '📦 压缩上下文';
const COMPRESSING_LABEL = '⏳ 压缩中…';
const COMPRESSED_LABEL = '📦 已压缩 · 查看摘要';

/**
 * 把 server 给的 pct 夹紧到 [0, 100] 用于条形宽度;原始 pct 仍展示在 tooltip 上,
 * 因为 Claude 把 cache_read 计入后,pct 可能瞬间超过 100%(长会话 cache 命中时正常)。
 */
function clampPct(pct: number): number {
  if (!Number.isFinite(pct) || pct < 0) return 0;
  if (pct > 100) return 100;
  return pct;
}

/**
 * 数字格式化:把 123456 渲染成 "123K" / "1.2M"。条形上的紧凑数字用此函数,
 * 避免显示成 6 位数把标题挤掉。
 */
function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(0)}K`;
  return String(n);
}

/**
 * 选择条形颜色:基于 clampPct(0..100),阈值 80 / 95。
 * 注意:不是基于原始 pct,避免缓存命中瞬间把条形染红又染回来闪烁。
 */
function bandClass(pct: number): string {
  if (pct >= 95) return 'usage-bar-band usage-bar-band-critical';
  if (pct >= 80) return 'usage-bar-band usage-bar-band-warn';
  return 'usage-bar-band usage-bar-band-ok';
}

export function ContextUsageBar({
  usage,
  onCompress,
  disabled,
  compressing,
  stepLabel,
  compressedAt,
  onShowSummary,
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
        <button
          type="button"
          className="usage-bar-btn"
          onClick={handleClick}
          disabled={!!disabled && !showCompressed}
          title={showCompressed ? '查看已压缩摘要' : '让 claude 总结当前会话并清空上下文'}
        >
          {buttonLabel}
        </button>
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