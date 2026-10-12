// Page-number window for a `.pagination` control — always include the first
// and last page, keep a small neighborhood around the current one, and
// collapse the gap in between into an ellipsis. Yields numbers interleaved
// with '…'.
//
// Extracted verbatim from ProjectDetail's requirements tab, which is now one
// of two callers (the other is the paginated project list).
export function pageWindow(total: number, current: number): (number | '…')[] {
  if (total <= 7) return Array.from({ length: total }, (_, i) => i + 1);
  const candidates = new Set([1, total, current - 1, current, current + 1]);
  const pages = [...candidates].filter(p => p >= 1 && p <= total).sort((a, b) => a - b);
  const out: (number | '…')[] = [];
  let prev = 0;
  for (const p of pages) {
    if (prev && p - prev > 1) out.push('…');
    out.push(p);
    prev = p;
  }
  return out;
}
