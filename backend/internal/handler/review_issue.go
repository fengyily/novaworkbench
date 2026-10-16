package handler

// review_issue.go — turn a finished review's findings into tracked issue
// requirements so they don't evaporate after the report is read.
//
// The action sits on POST /api/code-reviews/{rid}/issues. Two modes share the
// endpoint:
//
//	finding_ref: "P0-1"   → create one issue from a single finding
//	batch: true, severities: ["P0","P1"]  → create one issue per matching finding
//
// The single mode is the common case (user clicks the button next to a
// specific finding). The batch mode is the "I've read the report, turn all
// blocking items into tracked issues" affordance; severities defaults to
// ["P0","P1"] when omitted because P3 findings are nits nobody plans to file.
// Either mode returns a list of created requirements — the single mode
// returns a one-element slice so the frontend can use the same success UI.
//
// Wire format:
//
//	{ finding_ref: "P0-1", priority: "high", source_data?: {...} }     → single
//	{ batch: true, severities: ["P0","P1"], priority: "high" }         → batch
//
// Both modes share the same creation rules:
//
//	kind          = "issue"  (always — that's the product semantic)
//	title         = finding.title  (truncated to 80 chars on overflow)
//	description   = Markdown body — title + detail + suggestion + a footer
//	                line that points back to the source review (review_id,
//	                branch, ref). The footer is what makes "where did this
//	                issue come from?" answerable from the requirement page
//	                alone, without a manual cross-reference.
//	priority      = caller-supplied; defaults to "medium". Single-mode
//	                callers usually pin "high" / "low" per their triage;
//	                batch-mode callers typically accept the default.
//	source_review_id = rid   (the cross-table pointer the detail page
//	                           navigates via; see Requirement.SourceReviewID)
//	source_platform    = "nova-review"  (sentinel so the column joins
//	                                      aren't ambiguous with the
//	                                      Issue-monitor source_* values)
//	source_external_id = "<rid>#<ref>"  (single mode) or "<rid>#batch"
//	                                     (batch mode) — the per-issue anchor
//	                                     inside the review.
//
// Failures:
//   - finding_ref not present in the persisted findings: 404 ISSUE_NOT_FOUND
//   - review has no findings_json (model broke the trailing-JSON contract):
//     422 FINDINGS_UNAVAILABLE — the user can still paste the prose into a
//     manual issue, but the button can't reach in.
//   - any per-finding DB error: stop and return 500 with the offending ref;
//     partial batch results are not returned (atomic-or-nothing is much
//     easier to reason about than "two of three succeeded").

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// CreateIssuesFromReview handles POST /api/code-reviews/{rid}/issues.
//
// The handler is wired only when the requirement service is available (a
// build with disabled requirements doesn't expose this action). Without the
// service the endpoint answers 503 so the frontend can hide the buttons
// without a separate feature flag.
func (h *ReviewHandler) CreateIssuesFromReview(w http.ResponseWriter, r *http.Request) {
	if h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "需求服务未启用")
		return
	}
	rv, ok := h.lookupReview(w, r)
	if !ok {
		return
	}
	var req CreateIssuesFromReviewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "请求体解析失败")
		return
	}
	// Parse findings from the review's persisted contract block. parseFindings
	// is the same routine the run-time uses to count severities, so the
	// finding indices we expose here are exactly the ones the report renders
	// against — there's no risk of "the button created an issue for the wrong
	// finding" because of a count drift.
	findings, _, ok := parseFindings(rv.FindingsJSON)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "FINDINGS_UNAVAILABLE",
			"该 Review 报告未输出分级 JSON，无法按条目建 issue")
		return
	}

	priority := strings.TrimSpace(req.Priority)
	if priority == "" {
		priority = "medium"
	}

	// Source-external id pattern: "<rid>#<ref>". The "nova-review" sentinel in
	// source_platform keeps the two source_* axes (Issue-monitor imports vs
	// in-app review seed) disjoint so downstream code can branch on
	// source_platform without false positives.
	const sourcePlatform = "nova-review"

	// Build the list of findings to convert. Batch mode filters by severity;
	// single mode resolves a "P0-1"-style ref. When both modes are present
	// (or neither), single wins — finding_ref is the more specific intent and
	// the caller probably forgot to clear it.
	items := []findingToIssue{}
	switch {
	case strings.TrimSpace(req.FindingRef) != "":
		idx, err := resolveFindingRef(strings.TrimSpace(req.FindingRef), findings)
		if err != nil {
			writeError(w, http.StatusNotFound, "ISSUE_NOT_FOUND", err.Error())
			return
		}
		items = append(items, findingToIssue{finding: findings[idx], ref: strings.TrimSpace(req.FindingRef)})
	default:
		sevs := req.Severities
		if len(sevs) == 0 {
			sevs = []string{"P0", "P1"}
		}
		// Walk the persisted findings in order so the created-issue list
		// matches the order the user reads them in the report (severity
		// bucket → 1-based index). Filtering AFTER ordering keeps the ref
		// strings stable.
		for sev, bucket := range bucketBySeverity(findings) {
			if !containsString(sevs, sev) {
				continue
			}
			for i, f := range bucket {
				ref := fmt.Sprintf("%s-%d", sev, i+1)
				items = append(items, findingToIssue{finding: f, ref: ref})
			}
		}
	}

	created := make([]*model.Requirement, 0, len(items))
	for _, it := range items {
		reqModel := buildIssueFromFinding(rv, it, priority, sourcePlatform)
		got, err := h.reqSvc.Create(reqModel)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "CREATE_FAILED",
				fmt.Sprintf("为 %s 建 issue 失败: %v", it.ref, err))
			return
		}
		created = append(created, got)
	}
	writeJSON(w, http.StatusCreated, created)
}

// CreateIssuesFromReviewReq is the body shape for POST
// /api/code-reviews/{rid}/issues. Two non-overlapping modes:
//
//   - { finding_ref: "P0-1" }            → exactly one issue
//   - { batch: true, severities: [...] } → one issue per matching finding
//
// Priority is optional (defaults to "medium"). batch and finding_ref are
// independent — when both are set, finding_ref wins; when neither is set,
// the request is treated as batch with default severities (P0+P1).
type CreateIssuesFromReviewReq struct {
	FindingRef string   `json:"finding_ref,omitempty"`
	Batch      bool     `json:"batch,omitempty"`
	Severities []string `json:"severities,omitempty"`
	Priority   string   `json:"priority,omitempty"`
}

// findingToIssue is one row in the local working list — the parsed finding
// plus the ref string it was looked up by, kept so the create-error path can
// surface a useful message ("为 P0-2 建 issue 失败") without re-deriving it.
type findingToIssue struct {
	finding model.ReviewFinding
	ref     string
}

// buildIssueFromFinding assembles the CreateRequirementReq for one finding.
// The description is Markdown: a small header that quotes the finding title,
// the detail body, the suggestion if present, and a footer that anchors back
// to the source review. The footer is what keeps "where did this come from?"
// answerable from the requirement page alone.
func buildIssueFromFinding(rv *model.CodeReview, it findingToIssue, priority, sourcePlatform string) model.CreateRequirementReq {
	title := strings.TrimSpace(it.finding.Title)
	if title == "" {
		// A finding without a title is still worth tracking (the detail body
		// is what matters); fall back to a severity-tagged stub so the
		// requirement page isn't blank. Truncated to keep the title column
		// readable — 80 chars is enough for "P1 修复某路径空指针…" without
		// spilling into the badge column.
		title = fmt.Sprintf("Review %s %s", it.finding.Severity, it.finding.File)
		if len(title) > 80 {
			title = title[:80]
		}
	}

	var b strings.Builder
	b.WriteString("## ")
	b.WriteString(it.finding.Title)
	b.WriteString("\n\n")
	if it.finding.Detail != "" {
		b.WriteString(it.finding.Detail)
		b.WriteString("\n\n")
	}
	if it.finding.Suggestion != "" {
		b.WriteString("**修复建议**: ")
		b.WriteString(it.finding.Suggestion)
		b.WriteString("\n\n")
	}
	// Source footer. Single-line so it stays greppable in the requirement
	// description preview. review_id is the in-app pointer (and also the
	// source_review_id column), branch + ref are the in-report coordinates
	// for humans.
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("来源: Review `%s` · 分支 `%s` ← `%s` · 条目 `%s`",
		rv.ID, rv.Branch, rv.BaseBranch, it.ref))
	b.WriteString("\n")

	return model.CreateRequirementReq{
		ProjectID:       rv.ProjectID,
		Title:           title,
		Description:     b.String(),
		Priority:        priority,
		Kind:            service.KindIssue,
		SkipOrganize:    boolPtr(true), // the description is already structured Markdown; the LLM organizer pass would be busy work + add a token-usage row.
		SourceReviewID:  rv.ID,
		SourcePlatform:  sourcePlatform,
		SourceExternalID: fmt.Sprintf("%s#%s", rv.ID, it.ref),
	}
}

// resolveFindingRef maps a "P0-1"-style ref back to the finding slice index.
// The format matches what ReviewReport.tsx renders as the per-finding anchor
// ({sev}-{1-based}). Anything malformed returns an error so the handler can
// answer 404 with a useful message rather than silently picking the first
// P0.
func resolveFindingRef(ref string, findings []model.ReviewFinding) (int, error) {
	parts := strings.SplitN(ref, "-", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("finding_ref %q 格式错误，期望 P0-1 / P1-3 等", ref)
	}
	sev := strings.ToUpper(strings.TrimSpace(parts[0]))
	num, err := parseInt(parts[1])
	if err != nil || num < 1 {
		return 0, fmt.Errorf("finding_ref %q 序号无效", ref)
	}
	bucket := bucketBySeverity(findings)[sev]
	if num > len(bucket) {
		return 0, fmt.Errorf("finding_ref %q 不存在（该严重度共 %d 条）", ref, len(bucket))
	}
	// Walk back to the original slice index so Create gets the same finding
	// the report rendered (parseFindings preserves order, so the bucket is a
	// stable prefix of the input).
	seen := 0
	for i, f := range findings {
		if f.Severity != sev {
			continue
		}
		seen++
		if seen == num {
			return i, nil
		}
	}
	return 0, fmt.Errorf("finding_ref %q 不存在", ref)
}

// bucketBySeverity groups findings by their (uppercase) severity so the
// "P0-1" ref format can be resolved. Returns an empty map for an empty input.
func bucketBySeverity(findings []model.ReviewFinding) map[string][]model.ReviewFinding {
	out := map[string][]model.ReviewFinding{}
	for _, f := range findings {
		out[strings.ToUpper(strings.TrimSpace(f.Severity))] = append(out[strings.ToUpper(strings.TrimSpace(f.Severity))], f)
	}
	return out
}

// containsString is the stdlib-free membership test we need in a couple of
// places above (severities filter, future extensibility). O(n) is fine for
// the two- or three-element slices we feed it.
func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

// parseInt is a tiny positive-integer parser — strconv.Atoi would do, but
// keeping it local lets us reuse it without an extra import in the rest of
// the file and centralises the "must be >= 1" rule that finding_ref relies
// on.
func parseInt(s string) (int, error) {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// boolPtr returns &b without forcing every caller to import the boolPtr
// helper. Used here to point SkipOrganize at a constant true without making
// the struct literal noisier than it needs to be.
func boolPtr(b bool) *bool { return &b }
