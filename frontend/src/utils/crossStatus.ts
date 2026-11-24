// 跨项目需求的状态 → 全站共享的状态 chip 词汇。
//
// 全站只有一套 `.status-badge.status-*` 词汇（index.css），本页**不另造**一套
// —— 两套并行词汇表是 UI 审计里最刺眼的一条（同一个基类、两套语义）。
// 「联调中」借用运行中的颜色：它的含义就是「正在推进」。
export function crossStatusBadgeClass(status: string): string {
  switch (status) {
    case 'draft': return 'status-draft';
    case 'designing': return 'status-designing';
    case 'designed': return 'status-designed';
    case 'developing': return 'status-developing';
    case 'integrating': return 'status-running';
    case 'done': return 'status-done';
    case 'archived': return 'status-archived';
    default: return 'status-draft';
  }
}

/** 阶段状态 → 全站共享的状态 chip 词汇。 */
export function crossPhaseBadgeClass(status: string): string {
  switch (status) {
    case 'ready': return 'status-active';
    case 'verifying': return 'status-running';
    case 'pending_review': return 'status-pending';
    case 'passed': return 'status-done';
    case 'failed': return 'status-failed';
    case 'skipped': return 'status-archived';
    default: return 'status-pending';
  }
}

/**
 * Whether a phase may run (another) verification round.
 *
 *   ready          — first verification;
 *   pending_review — the report landed and awaits a human verdict, another
 *                    round is still allowed;
 *   failed         — the integration failed; after the rework the phase has
 *                    to be verifiable again, otherwise it stays stuck on the
 *                    verdict: no fresh report and no way to re-judge.
 *
 * pending (members not finished yet), verifying (a round is running) and
 * skipped (the phase was waived) are all excluded — verifying a phase whose
 * members are not done is meaningless.
 */
export function crossPhaseCanVerify(status: string): boolean {
  return status === 'ready' || status === 'pending_review' || status === 'failed';
}
