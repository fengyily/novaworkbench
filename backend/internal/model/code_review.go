package model

import "time"

// Review session lifecycle. A row is created in StatusDraft so attachments
// have a stable owner BEFORE the run starts; StartReviewSession flips it to
// running, and the stream's tail writes the terminal status.
const (
	ReviewStatusDraft   = "draft"
	ReviewStatusRunning = "running"
	ReviewStatusDone    = "done"
	ReviewStatusError   = "error"
)

// Review type discriminator. All kinds share one table so the history list
// is a single query; PR reviews keep their legacy entry point.
const (
	ReviewTypeBranch = "branch"
	ReviewTypePR     = "pr"
	// ReviewTypeRequirement is a requirement-level Review: launched from the
	// requirement detail page as a SUB-TASK (sub_tasks.task_kind='review'),
	// scoped to the requirement's branch + working tree instead of a
	// project-level branch pair. Its report is the authoritative copy — the
	// requirement page renders it, and 追问 / 重新生成 / 版本 / 导出 all reuse
	// the existing code_reviews read paths unchanged.
	ReviewTypeRequirement = "requirement"
	// ReviewTypeAcceptance is an 「验收核对」 session: a READ-ONLY AI check of
	// whether the sub-requirements a master requirement was split into
	// actually deliver what the master asked for.
	//
	// It is a different thing from ReviewTypeRequirement: a requirement-level
	// Review grades CODE (findings, P0/P1/P3, fix sub-tasks), while an
	// acceptance check grades the master's CONTRACT (did the children reach
	// the goal, what is still missing, should we split further). It produces
	// no findings and no fixes — it produces a verdict plus a list of gaps.
	//
	// The rows live in the same table so one query lists the whole history,
	// but every read path that serves the requirement's Review round chain
	// filters them out (see CodeReviewService.ListByRequirement).
	ReviewTypeAcceptance = "acceptance"
)

// Acceptance verdicts — the overall conclusion of an acceptance check.
//
// They reuse the yes / partial / no vocabulary of the Review contract
// (prompt.GoalMet*) so the UI can colour them with the same tokens.
const (
	AcceptanceVerdictYes     = "yes"
	AcceptanceVerdictPartial = "partial"
	AcceptanceVerdictNo      = "no"
)

// AcceptanceCheckUnknown is the per-criterion verdict for a point that could
// NOT be checked — e.g. the sub-requirement it belongs to never entered
// development, so there is no implementation to read.
//
// It is deliberately distinct from AcceptanceVerdictNo ("not implemented"):
// claiming a gap that was never verifiable would blame a sub-requirement for
// something the acceptance check could not see.
const AcceptanceCheckUnknown = "unknown"

// Acceptance child stages — the development state of a split sub-requirement
// at the moment an acceptance check ran.
//
// The stage describes what the CHECK could see, not what the workflow
// believes: a child that is still being developed holds a PARTIAL
// implementation, and a child whose implementation is not on this machine
// holds none at all. Neither is "not implemented", and the difference
// matters — the acceptance verdict is read as "the children failed to
// deliver", and blaming a child for work that is still in flight is exactly
// the wrong conclusion.
//
// The stage is also what makes an acceptance check runnable at ANY moment:
// the user can ask for a check while the children are still being developed,
// and the result is then a snapshot of the current progress rather than a
// sign-off.
const (
	// AcceptanceStageDone — the child finished its development. Its code is
	// the final delivery, and a point it missed is a real gap.
	AcceptanceStageDone = "done"
	// AcceptanceStageInProgress — the child is still being developed AND its
	// implementation directory holds a partial snapshot to read.
	AcceptanceStageInProgress = "in_progress"
	// AcceptanceStageNotStarted — the child never entered development: there
	// is nothing to read, so its points are unverifiable.
	AcceptanceStageNotStarted = "not_started"
	// AcceptanceStageUnavailable — the child is being developed but its
	// implementation is not on this machine (no worktree): there is nothing
	// to read either.
	AcceptanceStageUnavailable = "unavailable"
)

// CodeReview is one persisted Review session: the owner of its attachments,
// the carrier of the user-written description (the "改动目标" the reviewer
// checks against), and the landing spot for the finished report.
type CodeReview struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	ReviewType  string `json:"review_type"`
	Branch      string `json:"branch"`
	BaseBranch  string `json:"base_branch"`
	PRNumber    int    `json:"pr_number"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	JobID       string `json:"job_id"`
	// ResultMD is the Markdown report with the trailing JSON contract block
	// stripped; FindingsJSON is that block's `findings` array verbatim (empty
	// when the model didn't honour the contract — the UI then renders
	// ResultMD alone).
	ResultMD     string `json:"result_md"`
	FindingsJSON string `json:"findings_json"`
	// FindingsSource records WHERE the structured findings came from:
	// "contract" (the report's trailing ```json block), "markdown" (the
	// report's own P0/P1/P3 sections — the deterministic second source) or
	// "llm" (the LLM extractor, used when neither of the above yielded
	// anything). Empty = the report has no structured findings at all.
	//
	// It is informational: nothing branches on it, but it lets the UI tell
	// the user how the grading was obtained (and lets an operator see how
	// often the contract block goes missing).
	FindingsSource string `json:"findings_source"`
	P0Count        int    `json:"p0_count"`
	P1Count        int    `json:"p1_count"`
	P3Count        int    `json:"p3_count"`
	GoalMet        string `json:"goal_met"`
	// GoalChecksJSON is the contract's optional `goal_checks` array verbatim
	// (a []GoalCheck): the per-criterion evidence behind GoalMet's single
	// yes/partial/no verdict. Empty when the model skipped it — the verdict
	// alone still renders, so this never gates anything.
	GoalChecksJSON string `json:"goal_checks_json"`
	// AcceptanceJSON is the structured result of an 「验收核对」 session
	// (review_type='acceptance'): an AcceptanceSummary marshalled as JSON.
	//
	// It is EMPTY on every other review row, and also empty when the model
	// broke the contract (the parser then keeps the Markdown report alone
	// and the UI degrades to plain-Markdown rendering).
	//
	// It is a separate column rather than a reuse of GoalChecksJSON because
	// the shape is different: the acceptance result answers "did the children
	// reach the master's goal, what is missing, should we split further",
	// which the {criterion,status,evidence} triple cannot express.
	AcceptanceJSON string `json:"acceptance_json"`
	// RevisionNo is which version of the report code_reviews currently holds.
	// It starts at 1 and climbs by one per finished run (a re-review or a
	// 「基于对话重新生成」), matching the rev_no of the newest
	// code_review_revisions row. Older versions stay readable there.
	RevisionNo int `json:"revision_no"`
	// Model is the review's model binding: the user's pick from the Review
	// toolbar when one was made, overwritten with the EFFECTIVE model (role
	// override → config default → "默认模型" literal) once a run starts, so
	// the picker always shows what the last run actually used.
	Model string `json:"model"`
	// Effort is the review's reasoning-effort tier binding (claude CLI
	// --effort): the user's pick from the Review toolbar when one was made,
	// overwritten with the EFFECTIVE tier once a run starts, so the picker
	// always shows what the last run actually used.
	//
	// 追问 (follow-up) and 「基于对话重新生成」 both resume this review's
	// session and INHERIT this tier — they must not silently re-resolve it.
	Effort         string `json:"effort"`
	// ClaudeConfigID is the claude_configs row the gateway sources auth +
	// base URL from. Empty = fall back to the reviewer role's binding, then
	// the global active config.
	ClaudeConfigID string `json:"claude_config_id"`
	// AgentServerID is the execution environment: empty = 本地, otherwise an
	// agent_servers.id the run is dispatched to over SSH.
	AgentServerID string `json:"agent_server_id"`
	// ClaudeSessionID is the stream session_id of the review's LAST run.
	// Follow-up questions resume it so the answer is grounded in the diff and
	// the report the model already produced. Empty = no session to resume
	// (old rows, or a run that never emitted an init event); the follow-up
	// then starts fresh with the report embedded in its prompt.
	ClaudeSessionID string `json:"claude_session_id"`
	// RequirementID links the session to the requirement it reviews. Empty =
	// a project-level review (every pre-existing row). It is what lets the
	// requirement page list the Review rounds of one requirement
	// (CodeReviewService.ListByRequirement).
	RequirementID string `json:"requirement_id"`
	// RoleKey is the AI persona (roles.key) the review ran under — the
	// "Review 身份" the user picks in the requirement page. Empty = fall back
	// to the default "reviewer" persona. It lives on the review (not on the
	// sub-task) because the persona is a property of the review session, and
	// the same session can be re-run / resumed / regenerated with it.
	RoleKey string `json:"role_key"`
	// AutoIterateRounds is how many EXTRA rounds the user asked the
	// auto-iteration chain to run AFTER the first one ("自动迭代 · N 轮").
	// 1 means "run one more round after this one". The chain's total round
	// count is therefore 1 + AutoIterateRounds.
	//
	// It is stored on EVERY row (default 1) so a chain round can be
	// reconstructed from the row alone; whether a row PARTICIPATES in a
	// chain is decided by AutoIterateState, not by this column.
	AutoIterateRounds int `json:"auto_iterate_rounds"`
	// AutoIterateState is this round's state inside the auto-iteration
	// chain:
	//
	//   ""         — the row is not part of an auto-iteration chain (the
	//                ordinary, purely manual Review);
	//   "current"  — the chain's tip: this round is in flight, or it has
	//                finished and is waiting for its fix sub-tasks;
	//   "advanced" — this round handed over to the next round;
	//   "converged"— the chain ended because P0/P1 were cleared;
	//   "stopped"  — the chain ended; AutoIterateNote says why.
	//
	// The state lives on the round rows rather than in an in-memory
	// registry so the chain survives a backend restart.
	AutoIterateState string `json:"auto_iterate_state"`
	// AutoIterateNote is the machine-readable reason a chain stopped:
	// "converged" | "max_rounds" | "fix_failed" | "no_findings" |
	// "review_failed". The frontend maps it to a localized sentence; the
	// backend never writes a localized string here.
	AutoIterateNote string `json:"auto_iterate_note"`
	// Verdict is a NON-PERSISTED verdict for the round chain: "pass" |
	// "fail" | "pending" | "error". It is computed from p0/p1 at read time
	// rather than stored, so it can never drift from the counts the report
	// actually carries. Empty for rows that have not finished yet.
	Verdict     string     `json:"verdict,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// ReviewFinding is one graded issue from the report's trailing JSON block.
// Severity is one of prompt.SevP0 / SevP1 / SevP3; anything else is dropped
// by the parser so the frontend's three-bucket grouping stays total.
type ReviewFinding struct {
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Line       string `json:"line"`
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion"`
}

// GoalCheck is one row of the "修改目标是否达成" evidence table: a criterion
// taken from the user's 改动目标 description, the verdict for it, and the
// concrete evidence the reviewer based that verdict on (a diff hunk, a log
// line from an uploaded run log, a section of an attached design doc).
//
// It is the structured half of what GoalMet summarises in one word. The
// contract marks it optional, so a model that answers only `goal_met` still
// produces a usable report — the UI simply has no evidence table to show.
type GoalCheck struct {
	Criterion string `json:"criterion"`
	// Status is yes | partial | no | unknown. "unknown" is its own verdict
	// here (unlike GoalMet's empty string): a criterion the reviewer could
	// not check — e.g. it needs a runtime log nobody attached — is a real
	// outcome the user should see, not a missing answer.
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

// Goal-check statuses. The first three mirror prompt.GoalMet*; "unknown" is
// the extra verdict only a per-criterion row can carry.
const (
	GoalCheckUnknown = "unknown"
)

// ── 验收核对 (acceptance check) ───────────────────────────────────────────
//
// The structured result of one acceptance check on a master requirement whose
// development was delegated to its split children.
//
// It answers four questions: did the children reach the master's goal, how
// far did each acceptance point get, what is still missing, and should the
// master be split further.
//
// The four sections are SEPARATE on purpose: the verdict alone is not
// actionable, the per-point verdicts are the evidence behind it, the gaps
// are the "not implemented" list, and the proposals are the "split again"
// advice. Collapsing them would lose exactly the detail the user needs to
// decide what to do next.

// AcceptanceSummary is the whole structured result of one acceptance check.
//
// It is persisted as JSON in code_reviews.acceptance_json. Every field is
// optional in practice: a model that only answers the verdict still produces
// a usable row (the UI shows the verdict and the Markdown report).
type AcceptanceSummary struct {
	// Verdict is the overall conclusion: AcceptanceVerdictYes / Partial / No.
	// Empty when the model said something outside that vocabulary.
	Verdict string `json:"verdict"`
	// Summary is the one-sentence headline of the verdict.
	Summary string `json:"summary"`
	// Checks is the per-criterion evidence table.
	Checks []AcceptanceCheck `json:"checks"`
	// Gaps is the "not implemented" list.
	Gaps []AcceptanceGap `json:"gaps"`
	// Proposals is the "split further" advice.
	Proposals []AcceptanceProposal `json:"proposals"`
	// Pending lists the children that had NOT delivered when the check ran.
	//
	// It is computed from the database, NOT from the model's report: the
	// model cannot know which children are still in flight, and the user
	// needs this list to read the verdict correctly — a verdict produced
	// while two children were still developing is a snapshot, not a
	// sign-off.
	Pending []AcceptanceChild `json:"pending"`
	// Provisional marks the verdict as a snapshot: true when Pending is not
	// empty, i.e. at least one child had not delivered yet.
	Provisional bool `json:"provisional"`
}

// AcceptanceChild is one sub-requirement as the acceptance check saw it.
//
// It is the deterministic half of the "when" of a check: the model's report
// says what it read, this says what state each child was in.
type AcceptanceChild struct {
	// ChildID is the sub-requirement id.
	ChildID string `json:"child_id"`
	// ChildTitle is the sub-requirement title.
	ChildTitle string `json:"child_title"`
	// Status is the sub-requirement's raw status ("developing", "draft", …).
	Status string `json:"status"`
	// Stage is the development stage (AcceptanceStage*).
	Stage string `json:"stage"`
}

// AcceptanceCheck is one acceptance point of the master requirement and the
// verdict the acceptance check reached for it.
type AcceptanceCheck struct {
	// Criterion is the acceptance point being judged.
	Criterion string `json:"criterion"`
	// Status is yes | partial | no | unknown.
	Status string `json:"status"`
	// Evidence is the traceable basis for the verdict (a file:line, a
	// function name, a test name). Free text — the parser only checks that
	// it is not one of the hollow phrases ("已确认" and friends).
	Evidence string `json:"evidence"`
	// ChildID / ChildTitle say WHICH sub-requirement the point belongs to.
	// ChildID is empty when the point is not attributable to a single child
	// (or when the model named a child that does not exist).
	ChildID    string `json:"child_id"`
	ChildTitle string `json:"child_title"`
	// ChildState is the owner's development stage at check time
	// (AcceptanceStage*): what the check could see of the child that owns
	// this point. Empty when the point is not attributable to a child.
	//
	// It is what lets the UI say "this point is still in development"
	// instead of "this point failed" — the two look identical in the
	// model's report.
	ChildState string `json:"child_state"`
}

// AcceptanceGap is one thing the children did NOT implement.
type AcceptanceGap struct {
	// Title is a one-line headline of the gap.
	Title string `json:"title"`
	// Detail explains what is missing and what it should look like.
	Detail string `json:"detail"`
	// ChildID / ChildTitle name the sub-requirement that should have
	// delivered it (empty when the gap belongs to no single child).
	ChildID    string `json:"child_id"`
	ChildTitle string `json:"child_title"`
	// ChildState is the owner's development stage at check time
	// (AcceptanceStage*). A gap owned by a child that is still being
	// developed is "not delivered YET", not "not implemented" — the UI
	// labels it apart from a real gap.
	ChildState string `json:"child_state"`
}

// AcceptanceProposal is a suggested additional sub-requirement.
//
// It is ADVICE ONLY: the acceptance check never creates rows. Changing the
// requirement tree is the user's call (and the split flow is the tool for
// it), so the check just names what is missing.
type AcceptanceProposal struct {
	// Title is the proposed sub-requirement's title.
	Title string `json:"title"`
	// Goal is what it is supposed to achieve.
	Goal string `json:"goal"`
	// Acceptance is the list of acceptance points it would carry.
	Acceptance []string `json:"acceptance"`
	// DependsOn names the sub-requirements it would depend on (titles or
	// ids, whatever the model wrote — the UI shows them verbatim).
	DependsOn []string `json:"depends_on"`
}

// Revision triggers: what produced this version of the report.
const (
	RevisionTriggerInitial    = "initial"    // a Review run (first or re-run)
	RevisionTriggerRegenerate = "regenerate" // 基于追问对话重新生成
)

// CodeReviewRevision is one frozen version of a review's report.
//
// code_reviews always holds the CURRENT version — every existing read path
// (history list, PR comment, 追问 prompt) keeps working untouched. This table
// is the history behind it, so 「基于对话重新生成」 can produce a v2 without
// destroying the v1 the user already read, and so a disappointing v2 can be
// switched back.
type CodeReviewRevision struct {
	ID       string `json:"id"`
	ReviewID string `json:"review_id"`
	// RevNo is 1-based and unique per review; the newest row's RevNo equals
	// CodeReview.RevisionNo.
	RevNo        int    `json:"rev_no"`
	Trigger      string `json:"trigger"`
	ResultMD     string `json:"result_md"`
	FindingsJSON string `json:"findings_json"`
	P0Count      int    `json:"p0_count"`
	P1Count      int    `json:"p1_count"`
	P3Count      int    `json:"p3_count"`
	GoalMet      string `json:"goal_met"`
	// GoalChecksJSON mirrors CodeReview.GoalChecksJSON for this version.
	GoalChecksJSON string `json:"goal_checks_json"`
	// FollowupCount is how many 追问 turns existed when this version was
	// produced. The UI reads it as "v2 已纳入 N 条追问", and the difference
	// against the live thread length is what makes 「重新生成」 worth
	// offering again.
	FollowupCount int       `json:"followup_count"`
	Model         string    `json:"model"`
	CreatedAt     time.Time `json:"created_at"`
}

// Follow-up (追问) lifecycle. A follow-up is created already `running` — it
// has no draft window, since the question is submitted and dispatched in one
// request.
const (
	FollowupStatusRunning = "running"
	FollowupStatusDone    = "done"
	FollowupStatusError   = "error"
)

// CodeReviewFollowup is one question-and-answer turn asked against a finished
// Review. The thread is persisted (rather than living in the memory-only
// JobStore) so reopening the review from history shows the whole discussion.
//
// A follow-up is strictly read-only with respect to the review: it never
// rewrites result_md, findings_json or the severity counts.
type CodeReviewFollowup struct {
	ID       string `json:"id"`
	ReviewID string `json:"review_id"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Status   string `json:"status"`
	JobID    string `json:"job_id"`
	Model    string `json:"model"`
	// Resumed is true when the turn continued the review's claude session.
	// False means the session was gone and the answer came from a fresh run
	// seeded with the stored report — still useful, but without the tool
	// history of the original review.
	Resumed    bool       `json:"resumed"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
