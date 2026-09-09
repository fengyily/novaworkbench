/**
 * ContextUsageBar — live display of the current claude session's context
 * usage, and entry point for the "compress context" action.
 *
 * Data source
 *   - The parent subscribes to the `usage` event in the SSE stream
 *     (LogLine.usage); every result event pushes a fresh snapshot
 *     (input / output / cache_creation / cache_read + context_window + pct).
 *   - When `usage === undefined` this turn is not finished yet, so we render
 *     a stable "waiting for first response" placeholder.
 *
 * Interaction
 *   - Click the bar → calls onCompress() (wired by the parent to
 *     wizardApi.compressContext).
 *   - The bar always shows percent + "used / total"; it turns orange at
 *     ≥80% and red at ≥95% as a haptic hint, not a block.
 *   - When `disabled` (no compressible session) the whole row is greyed out
 *     and ignores clicks.
 *
 * Layout
 *   - Label row (model + step) and value row (percent) are each one line,
 *     the bar hugs the bottom; the whole block is compact enough to fit
 *     under the DeepRefineChat / DocRefineChat / CodingChat headers.
 *   - Colors reuse the Design System's primary / warning / error tokens.
 */

import { useTranslation } from 'react-i18next';
import type { UsageInfo } from '../utils/logLines';
import {
  clampPct,
  bandClass,
  formatTokens,
  isPctOverLimit,
  usageBreakdown,
} from '../utils/logLines';

export interface ContextUsageBarProps {
  /** Latest result-event snapshot; undefined until the first result lands
   *  (renders 0%). */
  usage?: UsageInfo;
  /** Trigger compression; the parent usually shows a confirm dialog and
   *  calls wizardApi.compressContext. */
  onCompress: () => void;
  /** When true the button is disabled (no session_id / compressing /
   *  compression disallowed). */
  disabled?: boolean;
  /** Spinner state while compressing; surfaces inside the button label. */
  compressing?: boolean;
  /** Step label (e.g. "requirement analyst"); used as tooltip and a11y label. */
  stepLabel: string;
  /**
   * Optional preview of the summary written after compression.
   * The parent passes this in after a successful compress; we render a small
   * "compressed · view summary" link so the whole bar stays self-contained
   * (one <button> triggers the compress).
   */
  compressedAt?: string | null;
  onShowSummary?: () => void;
  /**
   * Whether to show the "compress context" button. The design stage does
   * not need it (the design doc is a one-shot plan-mode product; refining
   * the conversation has no compression value). Pass false to keep only
   * the usage display — no button, no click handler. Default true
   * (analyst / dev stages compress normally).
   */
  compressible?: boolean;
}

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
  // When no usage event has fired yet (this turn is unfinished) render a
  // stable 0% placeholder so the bar does not flicker.
  const { t } = useTranslation();
  const used = usage?.used ?? 0;
  const window = usage?.context_window ?? 0;
  const pct = usage?.pct ?? 0;
  const widthPct = clampPct(pct);
  const overLimit = isPctOverLimit(pct);
  // Display cap: never show the user a percentage above 100. With the new
  // computeUsage formula (cache_read excluded from used) raw pct should
  // never reach 100 in normal operation; this is a defensive guard for
  // legacy persisted blobs / upstream callers that may still surface pct>100.
  // Tooltip still exposes the raw pct via the breakdown.
  const displayPctLabel = overLimit
    ? t('components.contextUsage.overLimit')
    : `${pct.toFixed(0)}%`;
  const modelLabel = usage?.model || '—';
  const usedLabel = `${formatTokens(used)} / ${window ? formatTokens(window) : '?'}`;
  const showCompressed = !!compressedAt && !compressing;

  // Tooltip breakdown: line 1 is the four token buckets + window, line 2
  // explains why "percentage" can stay under 100 even when the prompt is
  // clearly hitting the cache hard. Skip when we have no usage yet (avoids
  // showing a noisy "0 · 0 · 0 · ?" tooltip while the bar is still hidden).
  // i18next's TOptions requires an index signature; cast through Record for
  // structural compatibility.
  const breakdown = usageBreakdown(usage);
  const breakdownOpts = breakdown as unknown as Record<string, unknown>;
  const breakdownTitle = breakdown
    ? overLimit
      ? `${t('components.contextUsage.overLimitTitle', { pct: pct.toFixed(1) })}\n${t('components.contextUsage.breakdown', breakdownOpts)}\n${t('components.contextUsage.breakdownNote')}`
      : `${t('components.contextUsage.rawPct', { pct: pct.toFixed(1) })}\n${t('components.contextUsage.breakdown', breakdownOpts)}\n${t('components.contextUsage.breakdownNote')}`
    : undefined;

  // Button label + behaviour: show "view summary" once we have one, otherwise
  // "compress context"; show a spinner while compressing.
  const buttonLabel = compressing
    ? t('components.contextUsage.compressing')
    : showCompressed
      ? t('components.contextUsage.compressedView')
      : t('components.contextUsage.compressLabel');
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
      aria-label={`${stepLabel} ${t('components.contextUsage.ariaUsage')}`}
    >
      <div className="usage-bar-header">
        <span className="usage-bar-step">{stepLabel}</span>
        <span className="usage-bar-model" title={modelLabel}>{modelLabel}</span>
        <span className="usage-bar-used" title={`${used} / ${window} tokens`}>
          {usedLabel}
        </span>
        <span
          className={`usage-bar-pct ${pct >= 95 ? 'usage-bar-pct-critical' : pct >= 80 ? 'usage-bar-pct-warn' : ''}`}
          title={breakdownTitle}
        >
          {displayPctLabel}
        </span>
        {compressible && (
          <button
            type="button"
            className="usage-bar-btn"
            onClick={handleClick}
            disabled={!!disabled && !showCompressed}
            title={showCompressed
              ? t('components.contextUsage.viewSummary')
              : t('components.contextUsage.compressTitle')}
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
