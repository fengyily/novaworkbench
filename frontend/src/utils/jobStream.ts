// Deduplication helper for JobStore-backed SSE streams.
//
// The backend contract (backend/internal/store/jobs.go :: Job.Subscribe) is
// "subscribe = replay the whole history, then push live lines". A panel that
// hydrates itself from the durable snapshot (GET /api/wizard/jobs/{id}) and
// then subscribes to the SSE stream therefore receives every historical line
// TWICE: once from the snapshot, once from the replay.
//
// The fix is on the client: tell the stream how many of the leading frames are
// already on screen and drop them. The count MUST be the backend's raw
// LogLine count (snapshot `log.length`), not the number of rows the panel
// renders — the backend emits one SSE frame per LogLine, including the
// `knowledge` / `usage` frames that a panel filters out before rendering.
// Counting only the rendered rows would under-count and leave duplicates.
//
// The counter is deliberately forgiving in one direction: an over-large count
// drops rows (bad), an under-large count duplicates rows (also bad but
// visible). The snapshot and the replay are the same list in the same order,
// so the counts agree; when they don't, the mismatch is a backend bug that
// should be visible rather than silently swallowed.

/**
 * Build a "is this frame a replay of already-rendered history?" predicate.
 *
 * @param count number of leading SSE frames that are already on screen
 *              (the raw LogLine count of the snapshot that was hydrated).
 * @returns a predicate returning true when the frame must NOT be rendered
 *          again. The terminal `job_done` frame is never skipped — it carries
 *          the status/exit code the panel needs to finalize.
 */
export function createReplaySkipper(count: number): (evtType: string) => boolean {
  let seen = 0;
  const limit = Math.max(0, count);
  return (evtType: string) => {
    // The terminal frame is not part of the replayed history — it is emitted
    // after the replay drains, and the panel acts on it (status / exit code).
    if (evtType === 'job_done') return false;
    if (seen < limit) {
      seen++;
      return true;
    }
    seen++;
    return false;
  };
}
