package service

import (
	"database/sql"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// 跨项目需求的**批量聚合**。
//
// 为什么需要这一层：列表页原来对每一条跨项目需求各跑一次聚合，而聚合内部又
// 对**每个子需求**单跑一次前置依赖查询 —— 20 条需求 × 10 个子需求 ≈ 200+ 条
// 语句，每刷新一次页面就重来一遍。详情页更糟：Progress 与 ListChildren 各跑
// 一遍，同样的查询做了两次（而且 ListChildren 的结果根本没人用上）。
//
// 这一层把「子需求状态 + 前置依赖」一次性 JOIN 出来，**语句数与行数无关**：
//
//	1. 参与项目（一个 IN 子句）
//	2. 子需求（一个 IN 子句）
//	3. 子需求的前置依赖边（一条 JOIN —— 依赖边的「依赖方」行带着
//	   cross_requirement_id，所以不需要 IN 子句）
//	4. 阶段（一个 IN 子句）
//	5. 阶段成员（一条 JOIN）
//
// 列表页与详情页复用同一份聚合结果：详情页不再把 Progress 和 ListChildren
// 各跑一遍。
//
// 语义与旧的逐行实现**逐字一致** —— 聚合只是把同样的查询合并了，没有改任何
// 判定规则（哪些算「未满足前置」、进度怎么算、阶段状态怎么派生）。

// CrossRequirementAggregate — 一批跨项目需求的快照。
type CrossRequirementAggregate struct {
	// Projects — 参与项目（cross requirement id → 项目列表，按 sort_order）。
	Projects map[string][]model.CrossRequirementProject
	// Children — 子需求（含各自的前置依赖 / 未满足前置）。
	Children map[string][]model.CrossRequirementChild
	// Progress — 聚合出的进展。
	Progress map[string]model.CrossRequirementProgress
	// Phases — 阶段（含成员，状态已按成员派生）。
	Phases map[string][]model.CrossRequirementPhase
}

// LoadAggregate loads the batched snapshot of the given cross requirements.
//
// 每个 id 都会在返回的 map 里有一项（哪怕是空切片 / 零值），调用方不必判空。
func (s *CrossRequirementService) LoadAggregate(ids []string) (*CrossRequirementAggregate, error) {
	agg, err := s.loadChildAggregate(ids)
	if err != nil {
		return nil, err
	}
	phases, err := s.loadPhaseAggregate(ids)
	if err != nil {
		return nil, err
	}
	agg.Phases = phases
	return agg, nil
}

// loadChildAggregate loads the participating projects, the sub-requirements
// and their prerequisite edges for the given cross requirements.
//
// THREE statements in total, no matter how many cross requirements / children
// are involved. The per-child prerequisite lookup it replaces was the N+1:
// every child used to run its own query.
func (s *CrossRequirementService) loadChildAggregate(ids []string) (*CrossRequirementAggregate, error) {
	agg := &CrossRequirementAggregate{
		Projects: map[string][]model.CrossRequirementProject{},
		Children: map[string][]model.CrossRequirementChild{},
		Progress: map[string]model.CrossRequirementProgress{},
	}
	for _, id := range ids {
		agg.Projects[id] = []model.CrossRequirementProject{}
		agg.Children[id] = []model.CrossRequirementChild{}
		agg.Progress[id] = model.CrossRequirementProgress{
			Projects:     []model.CrossRequirementProjectProgress{},
			StatusCounts: map[string]int{},
		}
	}
	if len(ids) == 0 {
		return agg, nil
	}

	// ── 1. 参与项目 ───────────────────────────────────────────────────────
	placeholders, args := inClause(ids)
	rows, err := s.db.Query(
		`SELECT m.cross_requirement_id,m.id,m.project_id,COALESCE(p.name,''),m.role,m.sort_order
		 FROM cross_requirement_projects m
		 LEFT JOIN projects p ON p.id = m.project_id
		 WHERE m.cross_requirement_id IN (`+placeholders+`)
		 ORDER BY m.cross_requirement_id ASC, m.sort_order ASC, m.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var owner string
		var p model.CrossRequirementProject
		if err := rows.Scan(&owner, &p.ID, &p.ProjectID, &p.ProjectName, &p.Role, &p.SortOrder); err != nil {
			rows.Close()
			return nil, err
		}
		agg.Projects[owner] = append(agg.Projects[owner], p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// ── 2. 子需求 ─────────────────────────────────────────────────────────
	placeholders, args = inClause(ids)
	rows, err = s.db.Query(
		`SELECT r.cross_requirement_id,r.id,r.project_id,COALESCE(p.name,''),r.title,r.status,r.priority,r.split_order
		 FROM requirements r
		 LEFT JOIN projects p ON p.id = r.project_id
		 WHERE r.cross_requirement_id IN (`+placeholders+`)
		 ORDER BY r.cross_requirement_id ASC, CASE WHEN r.split_order > 0 THEN 0 ELSE 1 END ASC, r.split_order ASC, r.created_at ASC, r.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var owner string
		var c model.CrossRequirementChild
		if err := rows.Scan(&owner, &c.ID, &c.ProjectID, &c.ProjectName, &c.Title, &c.Status, &c.Priority, &c.SplitOrder); err != nil {
			rows.Close()
			return nil, err
		}
		c.DependsOn = []string{}
		c.BlockedBy = []string{}
		agg.Children[owner] = append(agg.Children[owner], c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// ── 3. 前置依赖边 ─────────────────────────────────────────────────────
	//
	// 依赖表没有项目约束，它本来就是一张跨项目的 DAG。这里按「依赖方」那一端
	// JOIN —— 依赖方就是子需求，它带着 cross_requirement_id，所以一条 JOIN
	// 就能取回这一批跨项目需求的全部依赖边（旧的实现是每个子需求一条查询）。
	placeholders, args = inClause(ids)
	rows, err = s.db.Query(
		`SELECT c.cross_requirement_id,d.requirement_id,d.depends_on_id,COALESCE(pr.status,'')
		 FROM requirement_dependencies d
		 JOIN requirements c ON c.id = d.requirement_id
		 LEFT JOIN requirements pr ON pr.id = d.depends_on_id
		 WHERE c.cross_requirement_id IN (`+placeholders+`)
		 ORDER BY c.cross_requirement_id ASC, d.created_at ASC, d.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	// 子需求 id → 它在 agg.Children 里的位置。一个需求只属于一个跨项目需求，
	// 所以一张全局索引表就够。
	indexByReq := map[string]int{}
	for _, list := range agg.Children {
		for i := range list {
			indexByReq[list[i].ID] = i
		}
	}
	for rows.Next() {
		var owner, reqID, depID, depStatus string
		if err := rows.Scan(&owner, &reqID, &depID, &depStatus); err != nil {
			rows.Close()
			return nil, err
		}
		idx, ok := indexByReq[reqID]
		if !ok {
			continue
		}
		list := agg.Children[owner]
		list[idx].DependsOn = append(list[idx].DependsOn, depID)
		if depStatus != "done" {
			list[idx].BlockedBy = append(list[idx].BlockedBy, depID)
		}
		agg.Children[owner] = list
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// ── 4. 进展 ───────────────────────────────────────────────────────────
	//
	// 进展完全由子需求行算出（状态分布 / 完成数 / 各项目计数 / 被阻塞数），
	// 所以旧的「GROUP BY project_id,status」那条查询也省掉了 —— 它数的就是
	// 同一批子需求行。
	for owner, list := range agg.Children {
		progress := agg.Progress[owner]
		for _, mp := range agg.Projects[owner] {
			progress.Projects = append(progress.Projects, model.CrossRequirementProjectProgress{
				ProjectID:   mp.ProjectID,
				ProjectName: mp.ProjectName,
				Role:        mp.Role,
			})
		}
		byProject := map[string]*model.CrossRequirementProjectProgress{}
		for i := range progress.Projects {
			byProject[progress.Projects[i].ProjectID] = &progress.Projects[i]
		}
		for _, c := range list {
			progress.Total++
			progress.StatusCounts[c.Status]++
			if c.Status == "done" {
				progress.Done++
			}
			if p, ok := byProject[c.ProjectID]; ok {
				p.Total++
				if c.Status == "done" {
					p.Done++
				}
			}
			// Blocked = 前置未完成 —— 与编码闸门(UnmetDependencies)同一口径。
			if len(c.BlockedBy) > 0 {
				progress.Blocked++
				if p, ok := byProject[c.ProjectID]; ok {
					p.Blocked++
				}
			}
		}
		if progress.Total > 0 {
			progress.Percent = progress.Done * 100 / progress.Total
		}
		agg.Progress[owner] = progress
	}

	return agg, nil
}

// loadPhaseAggregate loads the phases (with their members) of the given cross
// requirements.
//
// TWO statements in total — the members of every phase come back in one JOIN,
// instead of one query per phase.
func (s *CrossRequirementService) loadPhaseAggregate(ids []string) (map[string][]model.CrossRequirementPhase, error) {
	out := map[string][]model.CrossRequirementPhase{}
	for _, id := range ids {
		out[id] = []model.CrossRequirementPhase{}
	}
	if len(ids) == 0 {
		return out, nil
	}

	placeholders, args := inClause(ids)
	rows, err := s.db.Query(
		`SELECT id,cross_requirement_id,name,description,sort_order,status,verify_job_id,verify_report,verified_at,created_at,updated_at
		 FROM cross_requirement_phases WHERE cross_requirement_id IN (`+placeholders+`)
		 ORDER BY cross_requirement_id ASC, sort_order ASC, created_at ASC, id ASC`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p model.CrossRequirementPhase
		var verifiedAt, createdAt, updatedAt sql.NullString
		if err := rows.Scan(&p.ID, &p.CrossRequirementID, &p.Name, &p.Description, &p.SortOrder, &p.Status,
			&p.VerifyJobID, &p.VerifyReport, &verifiedAt, &createdAt, &updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		p.VerifiedAt = verifiedAt.String
		p.CreatedAt = createdAt.String
		p.UpdatedAt = updatedAt.String
		p.Items = []model.CrossRequirementPhaseItem{}
		out[p.CrossRequirementID] = append(out[p.CrossRequirementID], p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 阶段成员：一条 JOIN 取回这一批跨项目需求的全部成员。
	placeholders, args = inClause(ids)
	rows, err = s.db.Query(
		`SELECT ph.cross_requirement_id,i.phase_id,i.id,i.requirement_id,COALESCE(r.status,'')
		 FROM cross_requirement_phase_items i
		 JOIN cross_requirement_phases ph ON ph.id = i.phase_id
		 LEFT JOIN requirements r ON r.id = i.requirement_id
		 WHERE ph.cross_requirement_id IN (`+placeholders+`)
		 ORDER BY ph.cross_requirement_id ASC, ph.sort_order ASC, r.split_order ASC, r.created_at ASC, i.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	// 阶段 id → 它在所属跨项目需求的阶段列表里的位置。
	indexByPhase := map[string]int{}
	for _, list := range out {
		for i := range list {
			indexByPhase[list[i].ID] = i
		}
	}
	for rows.Next() {
		var owner, phaseID, itemID, reqID, status string
		if err := rows.Scan(&owner, &phaseID, &itemID, &reqID, &status); err != nil {
			rows.Close()
			return nil, err
		}
		idx, ok := indexByPhase[phaseID]
		if !ok {
			continue
		}
		list := out[owner]
		list[idx].Items = append(list[idx].Items, model.CrossRequirementPhaseItem{
			ID:            itemID,
			PhaseID:       phaseID,
			RequirementID: reqID,
			Status:        status,
		})
		out[owner] = list
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// pending / ready 是**派生**状态（不落库）：成员全部完成 → ready。
	// passed / failed / skipped 是人工结论，原样保留。
	for owner, list := range out {
		for i := range list {
			list[i].Status = derivePhaseStatus(list[i].Status, list[i].Items)
		}
		out[owner] = list
	}
	return out, nil
}

// inClause builds the placeholder list of an `IN (...)` clause together with
// its bind arguments.
//
// 占位符永远是 `?`（db 包装层在 Postgres 上会改写成 $N），值一律走绑定参数 ——
// 没有任何 id 被拼进 SQL 文本。
func inClause(ids []string) (string, []any) {
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	return strings.Join(placeholders, ","), args
}
