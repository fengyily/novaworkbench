package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/util"
)

// 「跨项目拆分」的落库：把一批候选子需求**原子地**写成普通 requirements 行
// （各归各的项目），并把子需求之间的先后顺序落进既有的依赖边表。
//
// 与同项目拆分（requirement_split.go）的三点不同：
//
//  1. **没有 master 行**。跨项目需求的子需求是**平级**的：parent_id 恒为空，
//     归属靠 requirements.cross_requirement_id 表达。
//  2. **base_branch 留空**。同项目拆分的所有子需求堆叠在主需求的开发分支上；
//     跨项目的子需求分属不同仓库，堆叠没有意义 —— 每个子需求在**自己**的项目
//     默认分支上开发（空 base → 解析链落到项目默认）。
//  3. **依赖边可以跨项目**。requirement_dependencies 本来就没有项目约束，
//     于是「B 项目的对接依赖 A 项目的接口定义」天然成立 —— 而且既有的编码闸门
//     （UnmetDependencies → GATE_DEPENDENCY_NOT_MET）零改动即生效。
//
// 三条不变量：
//
//   - **原子**：子需求行 + 依赖边在同一个事务里落库；任一步失败整体回滚。
//   - **项目归属受控**：每个候选的 project_id 必须 ∈ 参与项目集合，否则整批
//     拒绝（AI 幻觉出的项目名不能落到错误的仓库里）。
//   - **成环即回滚**：复用 checkDependencyAcyclic（与同项目拆分同一份实现）。

// crossSplitMaxTasks — 单次拆分的子需求数量上限（与 prompt 契约一致）。
const crossSplitMaxTasks = 12

// ConfirmSplit persists the user-confirmed cross-project candidates as
// ordinary requirements rows.
//
// The candidates arrive with a `project_id` the user may have edited on the
// confirm screen. Every project id is validated against the participating set
// BEFORE the transaction opens — an unknown project aborts the WHOLE batch
// (nothing is written), because a partially written batch would leave some
// sub-requirements in the wrong repository.
func (s *CrossRequirementService) ConfirmSplit(
	xreqID string,
	candidates []model.CrossRequirementSplitCandidate,
) ([]model.CrossRequirementChild, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("cross split: no candidates to create")
	}
	if len(candidates) > crossSplitMaxTasks {
		candidates = candidates[:crossSplitMaxTasks]
	}

	xreq, err := s.Get(xreqID)
	if err != nil {
		return nil, err
	}
	scope, err := s.ProjectIDs(xreqID)
	if err != nil {
		return nil, err
	}
	if len(scope) == 0 {
		return nil, fmt.Errorf("cross split: no participating project")
	}
	scopeSet := make(map[string]struct{}, len(scope))
	for _, id := range scope {
		scopeSet[id] = struct{}{}
	}

	// Order the candidates and normalize their ordinals into a contiguous
	// 1..N space (the same single coordinate space the prompt layer uses).
	ordered, remap := orderCrossCandidates(candidates)

	// Validate the project assignment BEFORE opening the transaction.
	for i, c := range ordered {
		title := strings.TrimSpace(c.Title)
		if title == "" {
			return nil, fmt.Errorf("cross split: candidate %d has no title", i+1)
		}
		pid := strings.TrimSpace(c.ProjectID)
		if pid == "" {
			return nil, fmt.Errorf("%w: candidate %q (%s)", ErrProjectNotInScope, title, c.Project)
		}
		if _, ok := scopeSet[pid]; !ok {
			return nil, fmt.Errorf("%w: %q wants project %q", ErrProjectNotInScope, title, pid)
		}
	}

	// The overall design document travels into every child's description: the
	// child's execution agent never sees the parent's design, so the
	// cross-project contract has to be self-contained (D10).
	design := DesignDocsToMarkdown(xreq.DesignDocs)
	projects, err := s.ListProjects(xreqID)
	if err != nil {
		return nil, err
	}
	refs := make([]prompt.CrossProjectRef, 0, len(projects))
	for _, p := range projects {
		refs = append(refs, prompt.CrossProjectRef{ID: p.ProjectID, Name: p.ProjectName, Role: p.Role})
	}

	ids := make([]string, len(ordered))
	for i := range ordered {
		ids[i] = util.NewID("req")
	}

	// depends_on holds batch-local ordinals (1-based) → translate them into
	// real ids.
	edges := []model.DependencyEdge{}
	for i, cand := range ordered {
		for _, dep := range RemapSplitOrdinals(cand.DependsOn, remap) {
			idx := dep - 1
			if idx < 0 || idx >= len(ordered) || idx == i {
				continue
			}
			edges = append(edges, model.DependencyEdge{
				RequirementID: ids[i],
				DependsOnID:   ids[idx],
			})
		}
	}

	batchID := util.NewID("spb")
	now := time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for i, cand := range ordered {
		criteria := "[]"
		if len(cand.Acceptance) > 0 {
			if raw, mErr := json.Marshal(cand.Acceptance); mErr == nil {
				criteria = string(raw)
			}
		}
		description := buildCrossDescription(cand, design, refs)
		if _, err := tx.Exec(
			`INSERT INTO requirements (id,project_id,title,description,status,priority,kind,acceptance_criteria,design_docs,conversation_ids,created_by,source_requirement_id,skip_analysis,skip_design,created_at,updated_at,parent_id,base_branch,base_branch_source,split_batch_id,dev_delegated,split_order,cross_requirement_id)
			 VALUES (?,?,?,?,'draft','medium','requirement',?,'[]','[]','user','',1,0,?,?,'','','',?,0,?,?)`,
			ids[i], strings.TrimSpace(cand.ProjectID), strings.TrimSpace(cand.Title), description,
			criteria, now, now, batchID, i+1, xreqID); err != nil {
			return nil, fmt.Errorf("cross split: insert sub-requirement %d: %w", i, err)
		}
	}

	for _, edge := range edges {
		if edge.RequirementID == edge.DependsOnID {
			return nil, fmt.Errorf("%w: a requirement cannot depend on itself", ErrDependencyCycle)
		}
		if _, err := tx.Exec(
			`INSERT INTO requirement_dependencies (id,requirement_id,depends_on_id,batch_id,created_at) VALUES (?,?,?,?,?)`,
			util.NewID("dep"), edge.RequirementID, edge.DependsOnID, batchID, now); err != nil {
			return nil, fmt.Errorf("cross split: insert dependency edge: %w", err)
		}
	}

	// Topology check BEFORE the commit: a cycle must abort the whole batch.
	if err := checkDependencyAcyclic(ids, edges); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// The lifecycle status follows from the children that now exist.
	if err := s.RefreshDerivedStatus(xreqID); err != nil {
		return nil, err
	}

	children, err := s.ListChildren(xreqID)
	if err != nil {
		return nil, err
	}
	return children, nil
}

// orderCrossCandidates sorts the candidates by their order and renumbers them
// into a contiguous 1..N space, returning the old→new ordinal mapping.
//
// Same semantics as OrderSplitTasks (the same-project split): a missing order
// is filled from the array position, the sort is stable, and duplicated input
// orders resolve to their first occurrence.
func orderCrossCandidates(cands []model.CrossRequirementSplitCandidate) ([]model.CrossRequirementSplitCandidate, map[int]int) {
	ordered := make([]model.CrossRequirementSplitCandidate, len(cands))
	copy(ordered, cands)

	for i := range ordered {
		if ordered[i].Order <= 0 {
			ordered[i].Order = i + 1
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })

	remap := make(map[int]int, len(ordered))
	for i := range ordered {
		old := ordered[i].Order
		if _, dup := remap[old]; !dup {
			remap[old] = i + 1
		}
		ordered[i].Order = i + 1
	}
	return ordered, remap
}

// buildCrossDescription assembles a child's description **deterministically**.
//
// The body has two parts:
//
//  1. the candidate's own fields (目标 / 涉及范围 / 验收标准 / 边界) — same
//     shape as the same-project split (buildSplitDescription);
//  2. the cross-project contract block — the part that makes the body
//     self-contained for an agent that cannot see the parent's design.
func buildCrossDescription(cand model.CrossRequirementSplitCandidate, design string, refs []prompt.CrossProjectRef) string {
	base := buildSplitDescription(model.SplitTaskInput{
		Title:       cand.Title,
		Description: cand.Description,
		Scope:       cand.Scope,
		Acceptance:  cand.Acceptance,
		Boundary:    cand.Boundary,
	})
	block := prompt.BuildCrossContextBlock(design, refs, cand.Title)
	if block == "" {
		return base
	}
	return base + "\n" + block
}
