// useReviewSession — owns the state and SSE plumbing for one Code Review
// detail view. Both the tab-internal ProjectCodeReview container and the
// standalone `/reviews/:id` window page call this with the same id; the
// hook's return value is the single source of truth for "what is on
// screen right now" (the report, the 追问 thread, the version list, the
// streaming log).
//
// Three actions share one SSE channel — start / ask / regenerate / rerun
// all post a JobStore id and subscribe to the same reviewApi.streamJobUrl
// endpoint. Starting a new action closes whatever was streaming before
// so the user never sees interleaved log lines from two jobs.
//
// `projectId` is required alongside `reviewId` because the SSE URL is
// `/api/projects/{projectId}/prs/jobs/{jobId}/stream` and we need it
// before the CodeReview row is loaded (e.g. when rerun is clicked on the
// just-loaded detail). The window page and the tab both have it.

import { useCallback, useEffect, useRef, useState } from 'react';
import {
  codeReviewsApi, reviewApi,
  type CodeReview, type CodeReviewFollowup, type CodeReviewRevision,
} from '../api/client';
import { createEventStream, type EventStream } from '../api/stream';
import { errorMessage } from '../utils/errMsg';
import type { StreamLine } from '../pages/codeReview/shared';

/** Placeholder for future run-on-an-existing-row options. */
export type RerunOpts = Record<string, never>;

export interface UseReviewSessionResult {
  review: CodeReview | null;
  detailLoading: boolean;
  followups: CodeReviewFollowup[];
  revisions: CodeReviewRevision[];
  activeRevisionNo: number | null;
  currentJobId: string | null;
  liveLines: StreamLine[];
  running: boolean;

  askFollowup: (question: string) => Promise<void>;
  regenerate: () => Promise<void>;
  rerun: (opts?: RerunOpts) => Promise<void>;
  setActiveRevisionNo: (n: number | null) => void;
  restoreRevision: (n: number) => Promise<void>;
  reload: () => Promise<void>;
}

export function useReviewSession(
  reviewId: string | null,
  projectId: string,
): UseReviewSessionResult {
  const [review, setReview] = useState<CodeReview | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [followups, setFollowups] = useState<CodeReviewFollowup[]>([]);
  const [revisions, setRevisions] = useState<CodeReviewRevision[]>([]);
  const [activeRevisionNo, setActiveRevisionNo] = useState<number | null>(null);
  const [currentJobId, setCurrentJobId] = useState<string | null>(null);
  const [liveLines, setLiveLines] = useState<StreamLine[]>([]);
  const [running, setRunning] = useState(false);

  // Single shared SSE — start/ask/regenerate/rerun all funnel through
  // it so only one JobStore stream is open at a time. Held in a ref so
  // closing the previous one on a new action doesn't depend on the
  // current render's value.
  const esRef = useRef<EventStream | null>(null);
  const reviewIdRef = useRef<string | null>(reviewId);
  reviewIdRef.current = reviewId;

  // ── SSE plumbing ────────────────────────────────────────────────────────
  //
  // The four call sites are the same shape: POST a job, get {job_id},
  // close the previous SSE if any, open a new one. Keeping the
  // subscriber inline keeps the four call sites short; the only
  // difference between them is what to refresh once job_done lands.

  const closeStream = useCallback(() => {
    esRef.current?.close();
    esRef.current = null;
  }, []);

  // Refreshes just the review row and the version list — what
  // start/rerun/regenerate need once the run finishes.
  const refreshAfterRun = useCallback((rid: string) => {
    codeReviewsApi.get(rid)
      .then(full => setReview(full))
      .catch(() => { /* the SSE already ended; nothing to do */ });
    codeReviewsApi.listRevisions(rid)
      .then(rows => setRevisions(Array.isArray(rows) ? rows : []))
      .catch(() => { /* the switcher just stays on the current version */ });
  }, []);

  // Refreshes just the followup thread — what ask needs.
  const refreshFollowups = useCallback((rid: string) => {
    codeReviewsApi.listFollowups(rid)
      .then(rows => setFollowups(Array.isArray(rows) ? rows : []))
      .catch(() => { /* the turn is persisted either way */ });
  }, []);

  // subscribe wires the SSE for a freshly-started job. The caller
  // chooses which refresh to run on job_done via the `onSettled`
  // callback. Errors are appended into liveLines rather than thrown,
  // matching the prior container's behavior — a dropped stream is a UI
  // event, not a programmer error.
  const subscribe = useCallback((
    jobId: string,
    onSettled: (rid: string) => void,
  ) => {
    closeStream();
    setCurrentJobId(jobId);
    setLiveLines([]);
    setRunning(true);
    esRef.current = createEventStream(
      reviewApi.streamJobUrl(projectId, jobId),
      (data) => {
        if (data.type === 'job_done') {
          setRunning(false);
          setCurrentJobId(null);
          esRef.current?.close();
          esRef.current = null;
          const rid = reviewIdRef.current;
          if (rid) onSettled(rid);
        } else if (data.content) {
          setLiveLines(prev => [...prev, { type: data.type, content: data.content }]);
        }
      },
      () => {
        setRunning(false);
        setCurrentJobId(null);
        esRef.current = null;
        setLiveLines(prev => [...prev, { type: 'error', content: 'SSE connection dropped' }]);
        const rid = reviewIdRef.current;
        if (rid) onSettled(rid);
      },
    );
  }, [projectId, closeStream]);

  // ── Effect: re-load detail when reviewId changes ────────────────────────
  useEffect(() => {
    if (!reviewId) {
      setReview(null);
      setDetailLoading(false);
      setFollowups([]);
      setRevisions([]);
      setActiveRevisionNo(null);
      return;
    }
    let cancelled = false;
    setDetailLoading(true);
    Promise.all([
      codeReviewsApi.get(reviewId)
        .then(rv => { if (!cancelled) setReview(rv); }),
      codeReviewsApi.listFollowups(reviewId)
        .then(rows => { if (!cancelled) setFollowups(Array.isArray(rows) ? rows : []); }),
      codeReviewsApi.listRevisions(reviewId)
        .then(rows => { if (!cancelled) setRevisions(Array.isArray(rows) ? rows : []); }),
    ]).catch(() => { /* detailLoading resets in finally regardless */ })
      .finally(() => { if (!cancelled) setDetailLoading(false); });
    return () => { cancelled = true; };
  }, [reviewId]);

  // Close the SSE when the hook unmounts so a navigated-away review
  // doesn't keep a dangling reader around.
  useEffect(() => () => { closeStream(); }, [closeStream]);

  // ── Actions ─────────────────────────────────────────────────────────────
  //
  // Each action returns a Promise so callers can await the POST step
  // (the SSE that follows is fire-and-forget). The catch block appends
  // a single error line into liveLines and resets the in-flight flags
  // — same shape as the old container.

  const askFollowup = useCallback(async (question: string) => {
    const rid = reviewIdRef.current;
    if (!rid || running) return;
    try {
      const { job_id } = await codeReviewsApi.ask(rid, question);
      subscribe(job_id, (r) => refreshFollowups(r));
    } catch (err: unknown) {
      setLiveLines(prev => [...prev, { type: 'error', content: errorMessage(err) }]);
      setRunning(false);
      setCurrentJobId(null);
    }
  }, [running, subscribe, refreshFollowups]);

  const regenerate = useCallback(async () => {
    const rid = reviewIdRef.current;
    if (!rid || running) return;
    try {
      const { job_id } = await codeReviewsApi.regenerate(rid);
      subscribe(job_id, refreshAfterRun);
    } catch (err: unknown) {
      setLiveLines(prev => [...prev, { type: 'error', content: errorMessage(err) }]);
      setRunning(false);
      setCurrentJobId(null);
    }
  }, [running, subscribe, refreshAfterRun]);

  const rerun = useCallback(async (_opts: RerunOpts = {}) => {
    const rid = reviewIdRef.current;
    if (!rid || running) return;
    try {
      const { job_id } = await codeReviewsApi.start(rid);
      subscribe(job_id, refreshAfterRun);
    } catch (err: unknown) {
      setLiveLines(prev => [...prev, { type: 'error', content: errorMessage(err) }]);
      setRunning(false);
      setCurrentJobId(null);
    }
  }, [running, subscribe, refreshAfterRun]);

  const restoreRevision = useCallback(async (n: number) => {
    const rid = reviewIdRef.current;
    if (!rid) return;
    try {
      const updated = await codeReviewsApi.restoreRevision(rid, n);
      setReview(updated);
    } catch (err: unknown) {
      setLiveLines(prev => [...prev, { type: 'error', content: errorMessage(err) }]);
    }
  }, []);

  const reload = useCallback(async () => {
    const rid = reviewIdRef.current;
    if (!rid) return;
    try {
      const [full, fu, rv] = await Promise.all([
        codeReviewsApi.get(rid),
        codeReviewsApi.listFollowups(rid),
        codeReviewsApi.listRevisions(rid),
      ]);
      setReview(full);
      setFollowups(Array.isArray(fu) ? fu : []);
      setRevisions(Array.isArray(rv) ? rv : []);
    } catch (err: unknown) {
      setLiveLines(prev => [...prev, { type: 'error', content: errorMessage(err) }]);
    }
  }, []);

  return {
    review,
    detailLoading,
    followups,
    revisions,
    activeRevisionNo,
    currentJobId,
    liveLines,
    running,
    askFollowup,
    regenerate,
    rerun,
    setActiveRevisionNo,
    restoreRevision,
    reload,
  };
}
