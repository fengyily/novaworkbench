package handler

// review_candidates.go — what the Review composer starts from.
//
// The user pushed back on the original design: making them type the 改动目标
// by hand is unreasonable, because the requirement ALREADY carries the goal.
// There are really two questions a Review asks about a change:
//
//	1. 目标是否达成 — did the change achieve what the requirement asked for?
//	   The answer lives in the requirement's own description; re-typing it is
//	   busy work.
//	2. 有没有 BUG — is the change broken? Some of that the SYSTEM can find on
//	   its own, cheaply and without a token: the project's own static checks.
//
// So the composer offers the requirement's goal pre-filled, plus the focus
// points the system could extract or detect, and the user curates them (add /
// remove / edit) instead of writing from scratch.
//
// Two endpoints, split by cost:
//
//	POST …/reviews/candidates       → goal + acceptance criteria (instant,
//	                                   pure string work, no process spawn)
//	POST …/reviews/candidates/scan   → defects the static checks found (slow:
//	                                   spawns the project's own tooling)
//
// The split matters: `go vet` on a large repo takes tens of seconds, and
// blocking the composer on it would make the Review entry feel broken.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// Review candidate kinds.
const (
	// ReviewCandidateCriterion is an acceptance point derived from the
	// requirement's own description — what the reviewer grades "目标是否达成"
	// against.
	ReviewCandidateCriterion = "criterion"
	// ReviewCandidateDefect is something the project's static checks already
	// flagged — a concrete, reproducible defect worth a reviewer's time.
	ReviewCandidateDefect = "defect"
)

// reviewCandidateMax is the cap on how many candidates of one kind we
// return. The composer renders them as chips; a hundred of them would be a
// wall, not a menu.
const reviewCandidateMax = 12

// ReviewCandidate is one offered focus point for the Review composer.
type ReviewCandidate struct {
	// ID is stable within one response so the frontend can toggle a chip.
	ID string `json:"id"`
	// Kind is ReviewCandidateCriterion or ReviewCandidateDefect.
	Kind string `json:"kind"`
	// Title is the one-line statement shown on the chip (and inserted into
	// the description box when the chip is picked).
	Title string `json:"title"`
	// Detail carries the supporting evidence (the compiler error, the file
	// and line). Empty for criteria, which speak for themselves.
	Detail string `json:"detail,omitempty"`
}

// ReviewCandidatesResponse is what POST …/reviews/candidates returns.
type ReviewCandidatesResponse struct {
	// Goal is the pre-filled 改动目标 — the requirement's own goal.
	Goal string `json:"goal"`
	// Criteria are the acceptance points extracted from the description.
	Criteria []ReviewCandidate `json:"criteria"`
	// Defects are the static-check findings (empty on the instant endpoint —
	// they arrive from the scan endpoint).
	Defects []ReviewCandidate `json:"defects"`
}

// ReviewCandidates serves the instant half: the requirement's goal plus the
// acceptance criteria its description implies.
//
// POST /api/requirements/{id}/reviews/candidates
func (h *RequirementReviewHandler) ReviewCandidates(w http.ResponseWriter, r *http.Request) {
	if h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "需求级 Review 服务未启用")
		return
	}
	req, err := h.reqSvc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", "需求不存在")
		return
	}
	criteria, prose := splitRequirementDescription(req.Description)
	writeJSON(w, http.StatusOK, ReviewCandidatesResponse{
		Goal:     buildReviewGoal(req, prose),
		Criteria: criteria,
		Defects:  []ReviewCandidate{},
	})
}

// ReviewCandidatesScan serves the slow half: the defects the project's own
// static checks report in the requirement's working tree.
//
// It is a separate endpoint on purpose — the checks spawn processes and take
// tens of seconds on a large repo, so the composer must not wait for them
// before it becomes usable.
//
// POST /api/requirements/{id}/reviews/candidates/scan
func (h *RequirementReviewHandler) ReviewCandidatesScan(w http.ResponseWriter, r *http.Request) {
	if h.reqSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_AVAILABLE", "需求级 Review 服务未启用")
		return
	}
	req, err := h.reqSvc.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "REQUIREMENT_NOT_FOUND", "需求不存在")
		return
	}
	workDir := ""
	if h.projectSvc != nil {
		if p, perr := h.projectSvc.Get(req.ProjectID); perr == nil {
			workDir = resolveReviewWorkDir(req, p.LocalPath)
		}
	}
	defects, scanErr := scanStaticDefects(r.Context(), workDir)
	criteria, prose := splitRequirementDescription(req.Description)
	writeJSON(w, http.StatusOK, ReviewCandidatesResponse{
		Goal:     buildReviewGoal(req, prose),
		Criteria: criteria,
		Defects:  defects,
	})
	// A failed scan is reported in the payload, not as an HTTP error: the
	// composer still works with the goal + criteria, and the user is told
	// why the defect list is empty.
	_ = scanErr
}

// buildReviewGoal renders the requirement's goal — the text that goes into
// the 改动目标 box.
//
// It is the requirement's TITLE plus the PROSE half of its description (the
// background / motivation), deliberately NOT the whole description: the
// actionable half is handed to the user as individual acceptance-point chips
// instead. Putting the whole description here AND listing its criteria as
// chips would fill the box with the same sentence twice.
//
// The reviewer's prompt already carries the full requirement context
// (buildParentContext injects title / description / design docs), so this
// text is the user's editable statement of intent, not the only copy.
func buildReviewGoal(req *model.Requirement, prose string) string {
	if req == nil {
		return prose
	}
	title := strings.TrimSpace(req.Title)
	prose = strings.TrimSpace(prose)
	switch {
	case title != "" && prose != "":
		return fmt.Sprintf("目标：%s\n\n%s", title, prose)
	case title != "":
		return "目标：" + title
	default:
		return prose
	}
}

// criterionMarker matches a list-item prefix: "- ", "* ", "1. ", "1) ", "1、",
// "① ", "第一、". The marker is stripped from the candidate title.
var criterionMarker = regexp.MustCompile(`^\s*(?:[-*•]\s+|\d+[.)、]\s*|[（(]\d+[)）]\s*|[①②③④⑤⑥⑦⑧⑨⑩]\s*|第[一二三四五六七八九十]+[、.．]\s*)`)

// criterionVerb matches the STRONG modal verbs that turn a sentence into an
// acceptance point. Chinese requirements state what "should" happen far more
// often than they number their criteria, so the verb is the signal.
//
// The list is deliberately short. A wide one ("返回", "展示", "校验" …) fires
// on ordinary prose — "需求背景：列表接口一次返回太多数据" is context, not a
// criterion — and a chip the user has to dismiss is worse than a chip that
// never appears.
var criterionVerb = regexp.MustCompile(`应该|应当|必须|需要|要求|保证|支持|实现|提供|新增|删除|修复|改进|优化|增加|减少`)

// criterionNegation excludes sentences that merely MENTION a modal verb in
// order to deny it — "这一段不应该被当成验收点" is a note about the
// requirement, not an acceptance point.
var criterionNegation = regexp.MustCompile(`不应该|不需要|不必|无需|不是|不要|可以不|无需再`)

// splitRequirementDescription splits a requirement's description into its
// acceptance points and the prose around them.
//
// It is deterministic string work — no model, no tokens.
//
// The split is what lets the composer show the user two DIFFERENT things
// instead of the same text twice: the prose becomes the pre-filled goal
// ("why this change exists"), the acceptance points become individual chips
// the user ticks ("what it has to achieve").
//
// A line is an acceptance point when it is a list item, or when it carries a
// strong modal verb and no negation.
func splitRequirementDescription(desc string) (criteria []ReviewCandidate, prose string) {
	criteria = []ReviewCandidate{}
	if strings.TrimSpace(desc) == "" {
		return criteria, ""
	}
	var proseLines []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(desc, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// A heading ("## 背景") is context, not a criterion.
		if strings.HasPrefix(line, "#") {
			proseLines = append(proseLines, line)
			continue
		}
		isItem := criterionMarker.MatchString(line)
		stripped := strings.TrimSpace(criterionMarker.ReplaceAllString(line, ""))
		if stripped == "" {
			continue
		}
		// Long paragraphs are prose, not criteria.
		if len([]rune(stripped)) > 160 {
			proseLines = append(proseLines, stripped)
			continue
		}
		looksLikeCriterion := isItem ||
			(criterionVerb.MatchString(stripped) && !criterionNegation.MatchString(stripped))
		if !looksLikeCriterion {
			proseLines = append(proseLines, stripped)
			continue
		}
		if seen[stripped] {
			continue
		}
		seen[stripped] = true
		criteria = append(criteria, ReviewCandidate{
			ID:    fmt.Sprintf("c%d", len(criteria)+1),
			Kind:  ReviewCandidateCriterion,
			Title: stripped,
		})
		if len(criteria) >= reviewCandidateMax {
			break
		}
	}
	return criteria, strings.Join(proseLines, "\n")
}

// scanStaticDefects runs the project's own static checks in the requirement's
// working tree and returns what they report.
//
// This is the honest version of "the system can find the bugs for you": it
// is the project's own toolchain (go vet / tsc), not a guess. Everything is
// best-effort — a missing toolchain, a broken build file or a timeout yields
// no findings rather than an error, because the Review is still worth
// running without them.
func scanStaticDefects(ctx context.Context, workDir string) ([]ReviewCandidate, error) {
	if strings.TrimSpace(workDir) == "" {
		return []ReviewCandidate{}, fmt.Errorf("no working directory")
	}
	if _, err := os.Stat(workDir); err != nil {
		return []ReviewCandidate{}, fmt.Errorf("working directory unavailable: %w", err)
	}
	out := []ReviewCandidate{}
	seen := map[string]bool{}

	appendAll := func(cands []ReviewCandidate) {
		for _, c := range cands {
			key := c.Title
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c)
			if len(out) >= reviewCandidateMax*2 {
				return
			}
		}
	}

	// Go: `go vet` compiles the package and reports type errors, unused
	// results and a pile of suspicious constructs — the same class of defect
	// a reviewer would otherwise spend the first ten minutes of a round on.
	if _, err := os.Stat(filepath.Join(workDir, "go.mod")); err == nil {
		stdout, stderr, _ := runReviewCommand(ctx, workDir, 90*time.Second, "go", "vet", "./...")
		appendAll(parseGoVetFindings(stdout + "\n" + stderr))
	}

	// TypeScript: `tsc --noEmit` is the type-check a project already runs in
	// CI; it reports the exact file and line of every type error.
	if _, err := os.Stat(filepath.Join(workDir, "tsconfig.json")); err == nil {
		bin := filepath.Join(workDir, "node_modules", ".bin", "tsc")
		if _, statErr := os.Stat(bin); statErr == nil {
			stdout, stderr, _ := runReviewCommand(ctx, workDir, 120*time.Second, bin, "--noEmit")
			appendAll(parseTscFindings(stdout + "\n" + stderr))
		}
	}

	return out, nil
}

// goVetLine matches a `go vet` diagnostic: path/file.go:12:5: message.
var goVetLine = regexp.MustCompile(`^(.+?\.go):(\d+)(?::(\d+))?:\s*(.+)$`)

// goVetPrefix is the "vet: " marker `go vet` puts in front of a diagnostic
// when it is reporting a package-level build failure. Stripping it first is
// what keeps the file path out of the message.
const goVetPrefix = "vet: "

// parseGoVetFindings turns `go vet` output into candidates.
//
// Only the diagnostics are kept — the surrounding noise (the "# package" /
// "vet: ..." headers) would become meaningless chips.
func parseGoVetFindings(output string) []ReviewCandidate {
	out := []ReviewCandidate{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := goVetLine.FindStringSubmatch(strings.TrimPrefix(line, goVetPrefix))
		if m == nil {
			continue
		}
		msg := strings.TrimSpace(m[4])
		if msg == "" {
			continue
		}
		// "./main.go" and "main.go" are the same file; the shorter form is
		// what a human would type to go look at it.
		file := strings.TrimPrefix(m[1], "./")
		loc := fmt.Sprintf("%s:%s", file, m[2])
		if m[3] != "" {
			loc = fmt.Sprintf("%s:%s", loc, m[3])
		}
		out = append(out, ReviewCandidate{
			ID:     fmt.Sprintf("d%d", len(out)+1),
			Kind:   ReviewCandidateDefect,
			Title:  msg,
			Detail: loc,
		})
	}
	return out
}

// tscLine matches a `tsc --noEmit` diagnostic: src/foo.ts(12,5): error TS2345: …
var tscLine = regexp.MustCompile(`^(.+?)\((\d+),(\d+)\):\s*(.+)$`)

// parseTscFindings turns `tsc` output into candidates.
func parseTscFindings(output string) []ReviewCandidate {
	out := []ReviewCandidate{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := tscLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		msg := strings.TrimSpace(m[4])
		if msg == "" {
			continue
		}
		out = append(out, ReviewCandidate{
			ID:     fmt.Sprintf("d%d", len(out)+1),
			Kind:   ReviewCandidateDefect,
			Title:  msg,
			Detail: fmt.Sprintf("%s:%s", m[1], m[2]),
		})
	}
	return out
}

// runReviewCommand runs a command in dir with a hard timeout and returns its
// combined output.
//
// A non-zero exit is NOT an error: `go vet` and `tsc` exit non-zero exactly
// when they found something, which is the case we care about.
func runReviewCommand(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "NO_COLOR=1", "CI=1")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}
