package handler

// review_auto_iterate.go — the requirement-level Review's AUTO-ITERATION
// chain.
//
// The manual loop this file automates:
//
//	Review 第 N 轮 → 报告有 P0/P1 → 用户点「生成修复子任务」
//	              → 用户点「回归 Review」→ 第 N+1 轮 → …… 直到 P0/P1 清零
//
// Every one of those hops was a button press. The chain wires them together
// so the user presses ONE button (「启动 Review」 with 自动迭代 on) and then
// watches the rounds come in.
//
// NOTHING here re-implements the Review pipeline: the report面 is still the
// code_reviews table + prompt.BuildReviewPrompt + the contract parser, and
// the execution面 is still the SubTaskRunner. This file only owns the
// ORCHESTRATION between rounds — which round is the tip, when the fix
// sub-tasks of a round are done, when the next round may start, and when the
// chain must stop.
//
// WHY the chain state lives in the DATABASE (code_reviews.auto_iterate_*) and
// not in an in-memory registry: the chain's driver is a goroutine, and a
// backend restart kills it. A registry would leave the chain stranded
// forever — the same "永远排队中 / 永远 running" family of failures this
// repository has hit repeatedly. With the state on the row, the reconcile
// tick (StartAutoIterateReconciler) picks the chain back up.
//
// WHY there is BOTH an event hook and a reconcile tick: the hook is the fast
// path (the chain advances the moment a fix task finishes), the tick is the
// safety net (the chain advances even if the hook's goroutine died). They
// share the SAME advancement functions and the SAME per-requirement lock, so
// the two can never advance a round twice.

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// ── chain states ───────────────────────────────────────────────────────────

const (
	// autoIterateStateCurrent marks the chain's tip: the round is in
	// flight, or it has finished and is waiting for its fix sub-tasks.
	autoIterateStateCurrent = "current"
	// autoIterateStateAdvanced marks a round that handed over to the next.
	autoIterateStateAdvanced = "advanced"
	// autoIterateStateConverged marks the round where P0/P1 were cleared.
	autoIterateStateConverged = "converged"
	// autoIterateStateStopped marks a round whose chain stopped.
	autoIterateStateStopped = "stopped"
)

// Chain stop reasons. Machine-readable: the frontend maps them to a
// localized sentence, the backend never stores a localized string.
const (
	autoIterateNoteConverged    = "converged"
	autoIterateNoteMaxRounds    = "max_rounds"
	autoIterateNoteFixFailed    = "fix_failed"
	autoIterateNoteNoFindings   = "no_findings"
	autoIterateNoteReviewFailed = "review_failed"
)

// Chain actions returned by decideAutoIterate.
const (
	// autoIterateActionContinue means "create this round's fix sub-tasks
	// and keep the chain going".
	autoIterateActionContinue = "continue"
	// autoIterateActionStop means "end the chain here".
	autoIterateActionStop = "stop"
)

// autoIterateMaxRounds caps the EXTRA rounds a user may ask for.
//
// 10 extra rounds is 11 model runs of a full Review — already a lot of
// money and a lot of wall clock. Beyond that the loop stops being "run
// until the code is clean" and starts being a runaway, so the cap is
// enforced on the way in (the API rejects anything above it).
const autoIterateMaxRounds = 10

// autoIterateReconcileInterval is how often the reconcile tick looks for a
// chain that lost its driver (a backend restart).
const autoIterateReconcileInterval = 20 * time.Second

// autoIterateDecision is what the chain does after one Review round
// finished.
type autoIterateDecision struct {
	// Action is autoIterateActionContinue ("build the fix sub-tasks and
	// keep going") or autoIterateActionStop ("end the chain here").
	Action string
	// State is the chain state to write when Action == autoIterateActionStop.
	State string
	// Note is the machine-readable stop reason. Empty when the chain
	// continues.
	Note string
}

// decideAutoIterate decides what the chain does after one Review round
// finished.
//
// It is a PURE function on purpose: the chain's rules (when to keep going,
// when to stop, and WHY) are the part most likely to be wrong, and a pure
// function is the only version of them a table-driven unit test can pin.
//
// The parameters:
//
//	round        — the 1-based round number that just finished
//	totalRounds  — the chain's round budget (1 + the user's extra rounds)
//	reviewFailed — the round produced no report (crash / timeout / stopped)
//	p0, p1       — the round's severity counts
//	hasFindings  — the round produced a USABLE structured findings list.
//	               TRUE even when that list is EMPTY: an empty list means
//	               "nothing was found", while FALSE means "the model broke
//	               the contract and we cannot tell what was found".
//
// The order of the rules matters:
//
//  1. a failed round stops the chain — it has no verdict to act on, and
//     without this check its zero counts would read as "converged";
//  2. an unusable findings list stops the chain — there is nothing to
//     turn into fix sub-tasks, so the chain would keep re-reviewing the
//     same code and burning tokens until the budget ran out;
//  3. no P0/P1 left means the chain CONVERGED — that is the whole point of
//     the loop, and it beats every other reason to stop;
//  4. the round budget is exhausted → stop;
//  5. otherwise → keep going.
func decideAutoIterate(round, totalRounds int, reviewFailed bool, p0, p1 int, hasFindings bool) autoIterateDecision {
	if reviewFailed {
		return autoIterateDecision{Action: autoIterateActionStop, State: autoIterateStateStopped, Note: autoIterateNoteReviewFailed}
	}
	if !hasFindings {
		return autoIterateDecision{Action: autoIterateActionStop, State: autoIterateStateStopped, Note: autoIterateNoteNoFindings}
	}
	if p0 == 0 && p1 == 0 {
		return autoIterateDecision{Action: autoIterateActionStop, State: autoIterateStateConverged, Note: autoIterateNoteConverged}
	}
	if round >= totalRounds {
		return autoIterateDecision{Action: autoIterateActionStop, State: autoIterateStateStopped, Note: autoIterateNoteMaxRounds}
	}
	return autoIterateDecision{Action: autoIterateActionContinue}
}

// autoIterateTotalRounds is the chain's round budget: the first round plus
// the extra rounds the user asked for.
func autoIterateTotalRounds(rv *model.CodeReview) int {
	if rv == nil {
		return 1
	}
	extra := rv.AutoIterateRounds
	if extra < 0 {
		extra = 0
	}
	if extra > autoIterateMaxRounds {
		extra = autoIterateMaxRounds
	}
	return 1 + extra
}

// normalizeAutoIterateRounds clamps the user's round pick into the legal
// range. It returns the clamped value and whether the input was already
// legal — the API answers 400 for an out-of-range pick rather than silently
// correcting it, because a silently corrected pick would make the user
// believe they got more rounds than they did.
func normalizeAutoIterateRounds(rounds int) (int, bool) {
	if rounds < 1 || rounds > autoIterateMaxRounds {
		return 0, false
	}
	return rounds, true
}

// ── the per-requirement chain lock ─────────────────────────────────────────

// reviewChainLocks serialises the chain's advancement per requirement.
//
// Two things can try to advance the same chain at the same moment: the fix
// sub-task's terminal hook (one per fix task — they finish a few hundred
// milliseconds apart) and the reconcile tick. A per-requirement mutex keeps
// them from interleaving; the "is this round still the chain's tip?" check
// inside the critical section then guarantees that exactly ONE next round is
// ever minted.
//
// sync.Map keyed by requirement id: the map grows with the requirements that
// ever ran a chain, which is bounded by the project size.
var reviewChainLocks sync.Map

// withChainLock runs fn while holding the requirement's chain lock.
//
// The lock is deliberately coarse (it covers the whole advancement step,
// including the git scope resolution) — the chain advances at most once per
// round, so contention is a non-issue, and a coarse lock is much easier to
// reason about than a fine-grained one whose critical sections could
// interleave.
func withChainLock(reqID string, fn func()) {
	if reqID == "" {
		fn()
		return
	}
	mu, _ := reviewChainLocks.LoadOrStore(reqID, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	defer m.Unlock()
	fn()
}

// ── the chain steps ────────────────────────────────────────────────────────

// markChainState writes the chain state of one round. Every state change of
// the chain goes through here, so the state machine has a single writer.
func (h *RequirementReviewHandler) markChainState(reviewID, state, note string) {
	if h.reviewSvc == nil || reviewID == "" {
		return
	}
	if err := h.reviewSvc.MarkAutoIterateState(reviewID, state, note); err != nil {
		log.Printf("[req-review] mark auto-iterate state %s=%s: %v", reviewID, state, err)
	}
}

// onReviewRoundFinished runs after a Review round reached its terminal
// state: it decides whether the chain continues (and creates the round's fix
// sub-tasks) or stops (and records why).
//
// It is a no-op for a review that is not part of a chain — the ordinary,
// purely manual Review must behave EXACTLY as it did before auto-iteration
// existed, and the cheapest way to guarantee that is to leave the row's
// state empty and step out here.
func (h *RequirementReviewHandler) onReviewRoundFinished(rv *model.CodeReview, st *model.SubTask) {
	if rv == nil || st == nil {
		return
	}
	// Re-read the row: the report write-back has just landed, so the
	// findings / counts / status on disk are the freshest truth.
	fresh, err := h.reviewSvc.Get(rv.ID)
	if err != nil {
		fresh = rv
	}
	if fresh.AutoIterateState != autoIterateStateCurrent {
		return
	}
	withChainLock(fresh.RequirementID, func() {
		h.onReviewRoundFinishedLocked(fresh, st)
	})
}

// onReviewRoundFinishedLocked is onReviewRoundFinished's body. The caller
// holds the requirement's chain lock.
func (h *RequirementReviewHandler) onReviewRoundFinishedLocked(rv *model.CodeReview, st *model.SubTask) {
	// A row that is not part of a chain must not be touched AT ALL — not
	// even to write a state onto it. The ordinary, purely manual Review has
	// to look exactly as it did before auto-iteration existed, and an empty
	// state is what says "this row is not in a chain".
	if rv.AutoIterateState != autoIterateStateCurrent {
		return
	}
	// The sub-task row carries the round number; re-read it so the decision
	// uses the persisted value rather than whatever the runner's in-memory
	// copy happened to hold.
	round := st.ReviewRound
	if cur, err := h.subTaskSvc.Get(st.ID); err == nil && cur != nil && cur.ReviewRound > 0 {
		round = cur.ReviewRound
	}
	_, hasFindings := parseFindingsArray(rv.FindingsJSON)
	decision := decideAutoIterate(
		round,
		autoIterateTotalRounds(rv),
		rv.Status == model.ReviewStatusError,
		rv.P0Count,
		rv.P1Count,
		hasFindings,
	)
	switch decision.Action {
	case autoIterateActionContinue:
		if _, err := h.ensureAutoFixTasks(rv, st); err != nil {
			log.Printf("[req-review] ensure fix tasks for round %d (%s): %v", round, st.ID, err)
			h.markChainState(rv.ID, autoIterateStateStopped, autoIterateNoteFixFailed)
		}
	default:
		h.markChainState(rv.ID, decision.State, decision.Note)
		log.Printf("[req-review] auto-iterate stopped at %s: %s", rv.ID, decision.Note)
	}
}

// ensureAutoFixTasks creates the P0/P1 fix sub-tasks of one round, skipping
// the ones that already exist.
//
// IDEMPOTENT BY CONSTRUCTION: a fix task's title carries its finding ref
// ("[P0-1] …"), so a second call recognises the already-created ref and
// creates nothing. Both the round's terminal hook and the reconcile tick
// call this, and neither may create a duplicate batch.
//
// The scope is P0 + P1 — the same batch the manual 「生成修复子任务」 button
// converts by default. P3 findings are suggestions nobody files, and
// auto-fixing them would burn tokens on cosmetics.
//
// The caller holds the requirement's chain lock.
func (h *RequirementReviewHandler) ensureAutoFixTasks(rv *model.CodeReview, st *model.SubTask) ([]string, error) {
	if rv == nil || st == nil {
		return nil, nil
	}
	findings, ok := parseFindingsArray(rv.FindingsJSON)
	if !ok {
		return nil, nil
	}
	items := selectFixFindings(findings, CreateFixesReq{})
	if len(items) == 0 {
		return nil, nil
	}
	existing, err := h.subTaskSvc.ListChildren(st.ID)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for i := range existing {
		if ref := fixTaskRef(existing[i].Title); ref != "" {
			have[ref] = true
		}
	}
	missing := make([]findingToIssue, 0, len(items))
	for _, it := range items {
		if !have[it.ref] {
			missing = append(missing, it)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	req, err := h.reqSvc.Get(rv.RequirementID)
	if err != nil {
		return nil, err
	}
	return h.createFixTasks(req, rv, st.ID, missing, "", h.chainFixFinished)
}

// fixTaskRef extracts the finding ref ("P0-1") out of a fix sub-task's title.
//
// fixTaskTitle renders it as "[P0-1] <title>"; the ref is what makes the
// auto-iteration chain idempotent, so it has to be recoverable from the row
// alone. A title without a parsable ref simply has none — such a row is
// never treated as "already created", because mistaking it for one would
// silently drop a finding.
func fixTaskRef(title string) string {
	t := strings.TrimSpace(title)
	if !strings.HasPrefix(t, "[") {
		return ""
	}
	end := strings.Index(t, "]")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(t[1:end])
}

// chainFixFinished is the terminal hook of an auto-created fix sub-task.
//
// When the LAST fix of a round is done the chain may advance. The hook fires
// once per fix task, and advanceChainIfReady is the one that decides whether
// this was the last one.
func (h *RequirementReviewHandler) chainFixFinished(st *model.SubTask, out claudeStreamOutcome, artifactBody string) {
	if st == nil || st.ParentSubtaskID == "" {
		return
	}
	// The parent of a fix task IS the Review round it belongs to.
	parent, err := h.subTaskSvc.Get(st.ParentSubtaskID)
	if err != nil || parent == nil || parent.ReviewID == "" {
		return
	}
	rv, err := h.reviewSvc.Get(parent.ReviewID)
	if err != nil || rv == nil {
		return
	}
	if rv.AutoIterateState != autoIterateStateCurrent {
		return
	}
	// In its own goroutine: the advancement may resolve a git scope (a
	// `git fetch` against a slow remote), and that must not hold the fix
	// task's SSE stream open.
	go h.advanceChainIfReady(rv, parent)
}

// advanceChainIfReady advances the chain past one round when that round's
// fix sub-tasks are all finished.
//
// It is the chain's single advancement step, shared by the fix task's
// terminal hook and the reconcile tick, so the two can never disagree about
// what "ready" means.
//
// The outcomes:
//
//   - no fix child at all → the round's fix tasks were never created (the
//     backend died between the two steps) → create them now;
//   - a fix child still pending/running → wait;
//   - a fix child errored or was stopped → the chain STOPS: an aborted fix
//     is not a success, and continuing would re-review code nobody fixed
//     until the budget ran out;
//   - every fix child done → mint the next round.
func (h *RequirementReviewHandler) advanceChainIfReady(rv *model.CodeReview, st *model.SubTask) {
	if rv == nil || st == nil {
		return
	}
	withChainLock(rv.RequirementID, func() {
		h.advanceChainIfReadyLocked(rv, st)
	})
}

// advanceChainIfReadyLocked is advanceChainIfReady's body. The caller holds
// the requirement's chain lock.
func (h *RequirementReviewHandler) advanceChainIfReadyLocked(rv *model.CodeReview, st *model.SubTask) {
	children, err := h.subTaskSvc.ListChildren(st.ID)
	if err != nil {
		log.Printf("[req-review] list children of %s: %v", st.ID, err)
		return
	}
	if len(children) == 0 {
		// The round finished but its fix tasks were never created — the
		// backend died between the two steps. Create them now, through the
		// same idempotent path the round's own hook uses.
		h.onReviewRoundFinishedLocked(rv, st)
		return
	}
	pending := false
	failed := false
	for i := range children {
		switch children[i].Status {
		case model.SubTaskStatusDone:
		case model.SubTaskStatusError, model.SubTaskStatusStopped:
			failed = true
		default:
			pending = true
		}
	}
	if pending {
		return
	}
	if failed {
		h.markChainState(rv.ID, autoIterateStateStopped, autoIterateNoteFixFailed)
		log.Printf("[req-review] auto-iterate stopped at %s: a fix sub-task did not finish", rv.ID)
		return
	}
	h.startNextRoundForChainLocked(rv, st)
}

// startNextRoundForChainLocked mints the next round of the chain.
//
// It re-checks that the round it advances FROM is still the chain's tip —
// otherwise two drivers (the fix hook and the reconcile tick) could both
// mint a "round N+1" and fork the chain in two.
//
// The caller holds the requirement's chain lock.
func (h *RequirementReviewHandler) startNextRoundForChainLocked(prev *model.CodeReview, prevSub *model.SubTask) {
	if prev == nil || prevSub == nil {
		return
	}
	reqID := prev.RequirementID
	latest, latestSub, err := h.latestReviewRound(reqID)
	if err != nil {
		log.Printf("[req-review] latest round of %s: %v", reqID, err)
		return
	}
	if latest == nil || latestSub == nil || latestSub.ID != prevSub.ID {
		// Somebody already advanced past this round.
		return
	}
	req, err := h.reqSvc.Get(reqID)
	if err != nil {
		log.Printf("[req-review] load requirement %s: %v", reqID, err)
		return
	}
	// The identity / model / effort carry over from the round that just
	// finished — the chain is one conversation about one piece of work.
	roleKey := strings.TrimSpace(prev.RoleKey)
	if roleKey == "" {
		roleKey = reviewerRoleKey
	}
	pickedModel := strings.TrimSpace(prev.Model)
	pickedConfig := strings.TrimSpace(prev.ClaudeConfigID)
	effortPick := strings.TrimSpace(prev.Effort)

	rv, st, _, err := h.startNextRoundLocked(req, prev, roleKey, pickedModel, pickedConfig, effortPick)
	if err != nil {
		log.Printf("[req-review] auto-iterate could not start the next round of %s: %v", reqID, err)
		h.markChainState(prev.ID, autoIterateStateStopped, autoIterateNoteReviewFailed)
		return
	}
	// The round we came from has handed over.
	h.markChainState(prev.ID, autoIterateStateAdvanced, "")
	log.Printf("[req-review] auto-iterate advanced %s → %s (round %d)", prev.ID, rv.ID, st.ReviewRound)
}

// ── the reconcile tick ─────────────────────────────────────────────────────

// StartAutoIterateReconciler starts the loop that re-drives a chain whose
// driver died.
//
// WHY a loop at all: the chain's normal driver is the event hook — the round
// finishes → the fix tasks are created → they finish → the next round
// starts. But a backend restart in the middle of the chain kills the
// goroutine that was going to drive it, and the chain would then sit still
// FOREVER — the same "永远排队中" family of bugs this repository has hit
// repeatedly. The loop re-reads the persisted chain state and advances
// whatever is stuck.
//
// It shares the advancement function with the hook, and both run under the
// same per-requirement lock, so the two paths are idempotent and neither can
// double-advance a round.
func (h *RequirementReviewHandler) StartAutoIterateReconciler(ctx context.Context) {
	if h == nil || h.reviewSvc == nil {
		return
	}
	ticker := time.NewTicker(autoIterateReconcileInterval)
	defer ticker.Stop()
	log.Printf("[req-review] auto-iterate reconciler started (interval=%s)", autoIterateReconcileInterval)
	for {
		select {
		case <-ctx.Done():
			log.Printf("[req-review] auto-iterate reconciler stopped")
			return
		case <-ticker.C:
			h.reconcileAutoIterateChains()
		}
	}
}

// reconcileAutoIterateChains advances every chain that is waiting for it.
//
// It is deliberately forgiving: a chain that cannot be advanced right now
// (a round still running, a sub-task row missing) is simply skipped — the
// next tick looks again. A chain that CAN be advanced is advanced, and the
// advancement itself is idempotent.
func (h *RequirementReviewHandler) reconcileAutoIterateChains() {
	rows, err := h.reviewSvc.ListAutoIterateCurrent()
	if err != nil {
		log.Printf("[req-review] list auto-iterate chains: %v", err)
		return
	}
	for i := range rows {
		rv := rows[i]
		st, err := h.subTaskSvc.ByReviewID(rv.ID)
		if err != nil || st == nil {
			// A round without a sub-task row can never advance (the chain
			// hangs off the round's sub-task). Stop it explicitly rather
			// than leaving a chain that looks alive forever.
			h.markChainState(rv.ID, autoIterateStateStopped, autoIterateNoteReviewFailed)
			continue
		}
		if st.Status == model.SubTaskStatusRunning || st.Status == model.SubTaskStatusPending {
			// The round itself is still in flight.
			continue
		}
		if rv.Status == model.ReviewStatusError {
			h.markChainState(rv.ID, autoIterateStateStopped, autoIterateNoteReviewFailed)
			continue
		}
		h.advanceChainIfReady(&rv, st)
	}
}
