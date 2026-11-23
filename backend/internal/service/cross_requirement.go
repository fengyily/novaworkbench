package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// 「跨项目需求」的服务层。
//
// 设计要点（改这块代码前先读一遍）：
//
//  1. **独立实体**。cross_requirements 与 requirements 并存，不是 requirements
//     的一种 kind —— requirements.project_id 是 NOT NULL + FK + CASCADE，且
//     既有数十条路径假设「一需求一项目」。
//  2. **子需求 = 普通 requirements 行**。各归各的项目，靠
//     requirements.cross_requirement_id 挂回父实体，于是子需求自动继承现有的
//     analyst / architect / coding 三阶段与全部下游能力。
//  3. **进展不落库**。完成百分比、各项目完成数都是读路径实时聚合的 —— 冗余
//     存储一份就会与子需求的真实状态不一致。状态（生命周期）可以落库，因为
//     它是粗粒度的、用户可见的。
//  4. **删除不删子需求**。删跨项目需求 = 让子需求「脱离」（cross_requirement_id
//     置空，成为普通需求），而不是连带删掉已产出的代码 / 分支。

// ErrCrossRequirementNotFound — 跨项目需求不存在。
var ErrCrossRequirementNotFound = errors.New("cross requirement not found")

// ErrProjectNotInScope — 候选子需求的项目不在参与集合里。
//
// AI 可能幻觉出不存在的项目名，服务端必须在落库前拒绝 —— 否则子需求会落到
// 错误的仓库里，整批拒绝（事务回滚）比部分写入安全。
var ErrProjectNotInScope = errors.New("project is not part of the cross requirement")

// ErrDuplicateProject — 参与项目集合里有重复的项目。
var ErrDuplicateProject = errors.New("duplicate project in cross requirement")

// CrossRequirementService — 跨项目需求的实体 / 映射 / 阶段 / 进展聚合。
type CrossRequirementService struct {
	db *db.DB
}

// NewCrossRequirementService builds the service over the shared DB handle.
func NewCrossRequirementService(database *db.DB) *CrossRequirementService {
	return &CrossRequirementService{db: database}
}

// ── 实体 CRUD ───────────────────────────────────────────────────────────────

// Create inserts a cross requirement and (optionally) its project mapping.
//
// The entity row and the mapping rows land in ONE transaction: a cross
// requirement without any project is useless (the AI has nothing to read),
// so a partial write would only produce a broken row the user has to fix by
// hand.
func (s *CrossRequirementService) Create(req model.CreateCrossRequirementReq) (*model.CrossRequirement, error) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, fmt.Errorf("cross requirement title is required")
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = KindRequirement
	}
	priority := strings.TrimSpace(req.Priority)
	if priority == "" {
		priority = "medium"
	}
	criteria := "[]"
	if len(req.AcceptanceCriteria) > 0 {
		if raw, err := json.Marshal(req.AcceptanceCriteria); err == nil {
			criteria = string(raw)
		}
	}

	projects, err := s.normalizeProjectInputs(req.ProjectIDs)
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("cross requirement needs at least one project")
	}

	id := util.NewID("xreq")
	now := time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO cross_requirements (id,title,description,status,priority,kind,acceptance_criteria,design_docs,created_by,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,'[]','user',?,?)`,
		id, title, req.Description, model.XReqStatusDraft, priority, kind, criteria, now, now); err != nil {
		return nil, fmt.Errorf("create cross requirement: %w", err)
	}
	for i, p := range projects {
		if _, err := tx.Exec(
			`INSERT INTO cross_requirement_projects (id,cross_requirement_id,project_id,role,sort_order,created_at)
			 VALUES (?,?,?,?,?,?)`,
			util.NewID("xrp"), id, p.ProjectID, p.Role, i+1, now); err != nil {
			return nil, fmt.Errorf("attach project %s: %w", p.ProjectID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Get reads one cross requirement by id.
func (s *CrossRequirementService) Get(id string) (*model.CrossRequirement, error) {
	row := s.db.QueryRow(
		`SELECT id,title,description,status,priority,kind,acceptance_criteria,design_docs,design_session_id,design_job_id,split_batch_id,status_locked,created_by,created_at,updated_at,completed_at
		 FROM cross_requirements WHERE id = ?`, id)
	var x model.CrossRequirement
	var createdAt, updatedAt, completedAt sql.NullString
	if err := row.Scan(&x.ID, &x.Title, &x.Description, &x.Status, &x.Priority, &x.Kind,
		&x.AcceptanceCriteria, &x.DesignDocs, &x.DesignSessionID, &x.DesignJobID, &x.SplitBatchID, &x.StatusLocked, &x.CreatedBy,
		&createdAt, &updatedAt, &completedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrCrossRequirementNotFound
		}
		return nil, err
	}
	x.CreatedAt = createdAt.String
	x.UpdatedAt = updatedAt.String
	x.CompletedAt = completedAt.String
	return &x, nil
}

// List returns the cross requirements, newest first.
//
// Archived rows are hidden unless status == "archived" is asked for
// explicitly — same convention as the requirement list.
func (s *CrossRequirementService) List(status string, keyword string) ([]model.CrossRequirement, error) {
	where := "WHERE 1=1"
	args := []interface{}{}
	if status != "" && status != "all" {
		if status == "archived" {
			where += " AND status = 'archived'"
		} else {
			where += " AND status = ?"
			args = append(args, status)
		}
	} else {
		where += " AND status != 'archived'"
	}
	if kw := strings.TrimSpace(keyword); kw != "" {
		where += " AND (title LIKE ? OR description LIKE ?)"
		like := "%" + kw + "%"
		args = append(args, like, like)
	}

	rows, err := s.db.Query(
		`SELECT id,title,description,status,priority,kind,acceptance_criteria,design_docs,design_session_id,design_job_id,split_batch_id,status_locked,created_by,created_at,updated_at,completed_at
		 FROM cross_requirements `+where+` ORDER BY updated_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.CrossRequirement{}
	for rows.Next() {
		var x model.CrossRequirement
		var createdAt, updatedAt, completedAt sql.NullString
		if err := rows.Scan(&x.ID, &x.Title, &x.Description, &x.Status, &x.Priority, &x.Kind,
			&x.AcceptanceCriteria, &x.DesignDocs, &x.DesignSessionID, &x.DesignJobID, &x.SplitBatchID, &x.StatusLocked, &x.CreatedBy,
			&createdAt, &updatedAt, &completedAt); err != nil {
			return nil, err
		}
		x.CreatedAt = createdAt.String
		x.UpdatedAt = updatedAt.String
		x.CompletedAt = completedAt.String
		out = append(out, x)
	}
	return out, rows.Err()
}

// Update writes the editable fields of a cross requirement.
//
// **Partial-update semantics**: an empty field means「本次不修改」, the stored
// value is kept. Only the fields the UI owns are written — status is handled
// separately (SetStatus) so a stale form cannot resurrect a stale lifecycle
// state.
//
// 为什么是部分更新：状态与基本信息共用一个 PUT 端点，而「只改状态」是这一路
// 最常见的调用（归档 / 取消归档）。如果空字段被当成「清空」，那么
// `PUT {"status":"archived"}` 就会顺手把标题、优先级、类型、验收标准全清成
// 默认值；如果空标题被当成非法，请求会直接 400，归档在 API 层根本不可用。
//
// 验收标准是唯一的例外：它用「字段缺省 = nil，字段给了 = 非 nil（哪怕是空
// 数组）」区分，所以 `[]` 仍然可以清空它。
func (s *CrossRequirementService) Update(id string, req model.UpdateCrossRequirementReq) error {
	current, err := s.Get(id)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = current.Title
	}
	description := req.Description
	if description == "" {
		description = current.Description
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = current.Kind
	}
	if kind == "" {
		kind = KindRequirement
	}
	priority := strings.TrimSpace(req.Priority)
	if priority == "" {
		priority = current.Priority
	}
	if priority == "" {
		priority = "medium"
	}
	criteria := current.AcceptanceCriteria
	if req.AcceptanceCriteria != nil {
		criteria = "[]"
		if raw, mErr := json.Marshal(req.AcceptanceCriteria); mErr == nil {
			criteria = string(raw)
		}
	}
	res, err := s.db.Exec(
		`UPDATE cross_requirements SET title=?,description=?,priority=?,kind=?,acceptance_criteria=?,updated_at=? WHERE id=?`,
		title, description, priority, kind, criteria, time.Now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCrossRequirementNotFound
	}
	return nil
}

// SetStatus writes the lifecycle status **by hand** (archive / unarchive /
// manual "mark as done") and LOCKS it against the derived refresh.
//
// 为什么要有锁：读路径会用「子需求 + 阶段」重算生命周期（ApplyDerivedStatus）
// 并把结果写回。没有锁的话，人工置的 done / integrating 会在下一次 GET 时被
// 重算覆盖 —— 人工的判断被读路径悄悄推翻，而且用户完全看不出发生过什么。
//
// 锁在「事实变了」时释放：
//   - 离开「已归档」= 把生命周期交回机器（解档 = 继续做事，派生状态重新接管，
//     否则解档后一直停在人工写的值上）；
//   - 整体方案被保存 / 拆分产生新子需求（见 SaveDesign / ConfirmSplit）——
//     这两件事改变了状态所依据的事实。
func (s *CrossRequirementService) SetStatus(id string, status string) error {
	if err := validateCrossStatus(status); err != nil {
		return err
	}
	current, err := s.Get(id)
	if err != nil {
		return err
	}
	lock := 1
	if current.Status == model.XReqStatusArchived && status != model.XReqStatusArchived {
		lock = 0
	}
	now := time.Now()
	var completedAt interface{}
	if status == model.XReqStatusDone {
		completedAt = now
	}
	res, err := s.db.Exec(
		`UPDATE cross_requirements SET status=?,status_locked=?,updated_at=?,completed_at=COALESCE(?,completed_at) WHERE id=?`,
		status, lock, now, completedAt, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCrossRequirementNotFound
	}
	return nil
}

// writeDerivedStatus writes a status **computed** from the children + phases.
//
// 它**不碰** status_locked：派生路径既不能加锁（否则下一轮刷新会被自己上
// 一轮的结果豁免），也不该解锁（解锁是「事实变了」时由 SaveDesign /
// ConfirmSplit 显式做的）。
func (s *CrossRequirementService) writeDerivedStatus(id string, status string) error {
	if err := validateCrossStatus(status); err != nil {
		return err
	}
	now := time.Now()
	var completedAt interface{}
	if status == model.XReqStatusDone {
		completedAt = now
	}
	res, err := s.db.Exec(
		`UPDATE cross_requirements SET status=?,updated_at=?,completed_at=COALESCE(?,completed_at) WHERE id=?`,
		status, now, completedAt, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCrossRequirementNotFound
	}
	return nil
}

// ClearStatusLock releases the manual lock so the derived lifecycle takes the
// row back.
//
// Called when the facts behind the status change: a new overall design / a new
// batch of sub-requirements. Those events make a previously manual verdict
// stale, so the machine must be allowed to recompute it.
func (s *CrossRequirementService) ClearStatusLock(id string) error {
	_, err := s.db.Exec(`UPDATE cross_requirements SET status_locked=0 WHERE id=?`, id)
	return err
}

// ValidateCrossRequirementStatus rejects an unknown lifecycle status.
//
// Exported so the HTTP layer can validate the status **before** writing the
// other fields: an invalid status must not leave a half-applied update behind.
func ValidateCrossRequirementStatus(status string) error {
	return validateCrossStatus(status)
}

// validateCrossStatus rejects an unknown lifecycle status.
func validateCrossStatus(status string) error {
	switch status {
	case model.XReqStatusDraft, model.XReqStatusDesigning, model.XReqStatusDesigned,
		model.XReqStatusDeveloping, model.XReqStatusIntegrating, model.XReqStatusDone,
		model.XReqStatusArchived:
		return nil
	}
	return fmt.Errorf("invalid cross requirement status: %q", status)
}

// Delete removes a cross requirement and detaches (never deletes) its
// children.
//
// 删跨项目需求 ≠ 删子需求：子需求背后可能已经产出了代码、分支和 PR，级联删
// 掉会造成不可恢复的损失。所以这里只把 cross_requirement_id 置空（子需求
// 变成普通需求），再删映射 / 阶段 / 阶段成员 / 实体行。
func (s *CrossRequirementService) Delete(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE requirements SET cross_requirement_id='' WHERE cross_requirement_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM cross_requirement_phase_items WHERE phase_id IN (SELECT id FROM cross_requirement_phases WHERE cross_requirement_id=?)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cross_requirement_phases WHERE cross_requirement_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cross_requirement_projects WHERE cross_requirement_id=?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM cross_requirements WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCrossRequirementNotFound
	}
	return tx.Commit()
}

// ── 参与项目映射 ───────────────────────────────────────────────────────────

// normalizeProjectInputs dedupes and validates the project list.
func (s *CrossRequirementService) normalizeProjectInputs(ids []string) ([]model.CrossRequirementProjectInput, error) {
	out := []model.CrossRequirementProjectInput{}
	seen := map[string]struct{}{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, model.CrossRequirementProjectInput{ProjectID: id})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cross requirement needs at least one project")
	}
	return out, nil
}

// SetProjects replaces the participating project set.
//
// REPLACE (not append) semantics: the user edits a full list in the UI, so a
// merge would leave removed projects behind. Project ids that do not exist
// are rejected — a mapping to a non-existent project would produce a detail
// page that can never resolve its project name.
func (s *CrossRequirementService) SetProjects(id string, projects []model.CrossRequirementProjectInput) error {
	if len(projects) == 0 {
		return fmt.Errorf("cross requirement needs at least one project")
	}
	seen := map[string]struct{}{}
	for _, p := range projects {
		pid := strings.TrimSpace(p.ProjectID)
		if pid == "" {
			return fmt.Errorf("project id is required")
		}
		if _, dup := seen[pid]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateProject, pid)
		}
		seen[pid] = struct{}{}
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(1) FROM projects WHERE id = ?`, pid).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("project %s not found", pid)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM cross_requirement_projects WHERE cross_requirement_id=?`, id); err != nil {
		return err
	}
	now := time.Now()
	for i, p := range projects {
		if _, err := tx.Exec(
			`INSERT INTO cross_requirement_projects (id,cross_requirement_id,project_id,role,sort_order,created_at)
			 VALUES (?,?,?,?,?,?)`,
			util.NewID("xrp"), id, strings.TrimSpace(p.ProjectID), strings.TrimSpace(p.Role), i+1, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE cross_requirements SET updated_at=? WHERE id=?`, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListProjects returns the participating projects (with their display names).
func (s *CrossRequirementService) ListProjects(id string) ([]model.CrossRequirementProject, error) {
	rows, err := s.db.Query(
		`SELECT m.id,m.cross_requirement_id,m.project_id,COALESCE(p.name,''),m.role,m.sort_order
		 FROM cross_requirement_projects m
		 LEFT JOIN projects p ON p.id = m.project_id
		 WHERE m.cross_requirement_id = ? ORDER BY m.sort_order ASC, m.id ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CrossRequirementProject{}
	for rows.Next() {
		var p model.CrossRequirementProject
		if err := rows.Scan(&p.ID, &p.CrossRequirementID, &p.ProjectID, &p.ProjectName, &p.Role, &p.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectIDs returns the participating project ids (used to validate the
// split candidates).
func (s *CrossRequirementService) ProjectIDs(id string) ([]string, error) {
	projects, err := s.ListProjects(id)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		out = append(out, p.ProjectID)
	}
	return out, nil
}

// AlreadySplit reports whether this cross requirement has already been split
// into sub-requirements.
//
// 两个来源：
//   - 父行上的 split_batch_id —— 正常路径（确认拆分时原子写入）；
//   - 子需求的数量 —— 兜住「子需求存在但父行没有批次 id」的行（早期构建
//     写出的数据）。
//
// 调用方用它来**提前**拒绝第二次拆分（让用户不必等到确认那一步才被拒）。
// 注意它只是给 UI / 作业入口用的预检：真正的仲裁在 ConfirmSplit 的原子
// 抢占上（那里才防得住并发）。
func (s *CrossRequirementService) AlreadySplit(id string) (bool, error) {
	var batchID string
	err := s.db.QueryRow(`SELECT split_batch_id FROM cross_requirements WHERE id = ?`, id).Scan(&batchID)
	if err == sql.ErrNoRows {
		return false, ErrCrossRequirementNotFound
	}
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(batchID) != "" {
		return true, nil
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM requirements WHERE cross_requirement_id = ?`, id).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// ── 子需求 ─────────────────────────────────────────────────────────────────

// ListChildren returns the sub-requirements of a cross requirement.
//
// Ordered by split_order (the order the user arranged in the confirm dialog)
// then created_at — the same convention as the ordinary child list, because
// one split batch shares a single created_at.
func (s *CrossRequirementService) ListChildren(id string) ([]model.CrossRequirementChild, error) {
	rows, err := s.db.Query(
		`SELECT r.id,r.project_id,COALESCE(p.name,''),r.title,r.status,r.priority,r.split_order
		 FROM requirements r
		 LEFT JOIN projects p ON p.id = r.project_id
		 WHERE r.cross_requirement_id = ?
		 ORDER BY CASE WHEN r.split_order > 0 THEN 0 ELSE 1 END ASC, r.split_order ASC, r.created_at ASC, r.id ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.CrossRequirementChild{}
	for rows.Next() {
		var c model.CrossRequirementChild
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ProjectName, &c.Title, &c.Status, &c.Priority, &c.SplitOrder); err != nil {
			return nil, err
		}
		c.DependsOn = []string{}
		c.BlockedBy = []string{}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attach the dependency edges + which prerequisites are still unmet.
	// The dependency table has NO project constraint — it is already a
	// cross-project DAG — so the edges between children are read directly.
	for i := range out {
		deps, err := s.requirementDependencies(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].DependsOn = deps.dependsOn
		out[i].BlockedBy = deps.unmet
	}
	return out, nil
}

// requirementDependencies reads a requirement's prerequisites plus which of
// them are not done yet.
type reqDeps struct {
	dependsOn []string
	unmet     []string
}

func (s *CrossRequirementService) requirementDependencies(reqID string) (reqDeps, error) {
	out := reqDeps{dependsOn: []string{}, unmet: []string{}}
	rows, err := s.db.Query(
		`SELECT d.depends_on_id, COALESCE(r.status,'') FROM requirement_dependencies d
		 LEFT JOIN requirements r ON r.id = d.depends_on_id
		 WHERE d.requirement_id = ? ORDER BY d.created_at ASC, d.id ASC`, reqID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var depID, status string
		if err := rows.Scan(&depID, &status); err != nil {
			return out, err
		}
		out.dependsOn = append(out.dependsOn, depID)
		if status != "done" {
			out.unmet = append(out.unmet, depID)
		}
	}
	return out, rows.Err()
}

// ListDependencies returns the dependency edges among the children.
func (s *CrossRequirementService) ListDependencies(id string) ([]model.CrossRequirementDependency, error) {
	rows, err := s.db.Query(
		`SELECT d.requirement_id,d.depends_on_id,
		        COALESCE(a.title,''),COALESCE(b.title,''),
		        COALESCE(a.project_id,''),COALESCE(b.project_id,''),
		        COALESCE(b.status,'')
		 FROM requirement_dependencies d
		 JOIN requirements a ON a.id = d.requirement_id
		 JOIN requirements b ON b.id = d.depends_on_id
		 WHERE a.cross_requirement_id = ? OR b.cross_requirement_id = ?
		 ORDER BY d.created_at ASC, d.id ASC`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CrossRequirementDependency{}
	for rows.Next() {
		var dep model.CrossRequirementDependency
		var aProject, bProject, bStatus string
		if err := rows.Scan(&dep.RequirementID, &dep.DependsOnID, &dep.RequirementTitle, &dep.DependsOnTitle,
			&aProject, &bProject, &bStatus); err != nil {
			return nil, err
		}
		dep.CrossProject = aProject != bProject
		dep.Met = bStatus == "done"
		out = append(out, dep)
	}
	return out, rows.Err()
}

// ── 进展聚合 ───────────────────────────────────────────────────────────────

// Progress aggregates the live progress of a cross requirement.
//
// Nothing is read from a stored counter — the numbers are computed from the
// children rows on every call, so they can never drift from reality.
func (s *CrossRequirementService) Progress(id string) (model.CrossRequirementProgress, error) {
	progress := model.CrossRequirementProgress{
		Projects:     []model.CrossRequirementProjectProgress{},
		StatusCounts: map[string]int{},
	}

	projects, err := s.ListProjects(id)
	if err != nil {
		return progress, err
	}
	byProject := map[string]*model.CrossRequirementProjectProgress{}
	for _, p := range projects {
		progress.Projects = append(progress.Projects, model.CrossRequirementProjectProgress{
			ProjectID:   p.ProjectID,
			ProjectName: p.ProjectName,
			Role:        p.Role,
		})
	}
	for i := range progress.Projects {
		byProject[progress.Projects[i].ProjectID] = &progress.Projects[i]
	}

	rows, err := s.db.Query(
		`SELECT project_id,status,COUNT(1) FROM requirements WHERE cross_requirement_id = ? GROUP BY project_id,status`, id)
	if err != nil {
		return progress, err
	}
	defer rows.Close()
	for rows.Next() {
		var projectID, status string
		var count int
		if err := rows.Scan(&projectID, &status, &count); err != nil {
			return progress, err
		}
		progress.StatusCounts[status] = progress.StatusCounts[status] + count
		progress.Total += count
		if status == "done" {
			progress.Done += count
		}
		if p, ok := byProject[projectID]; ok {
			p.Total += count
			if status == "done" {
				p.Done += count
			}
		}
	}
	if err := rows.Err(); err != nil {
		return progress, err
	}

	// Blocked = children whose prerequisites are not done yet. Reuses the same
	// rule as the coding gate (UnmetDependencies) — a child is blocked iff some
	// prerequisite row is not 'done'.
	children, err := s.ListChildren(id)
	if err != nil {
		return progress, err
	}
	for _, c := range children {
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
	return progress, nil
}

// RefreshDerivedStatus recomputes the coarse lifecycle status from the
// children + phases and writes it back — but only when it actually changed.
//
// 状态是「存储的粗粒度生命周期」，进展是「读路径实时聚合」：把状态落库是为了
// 列表页能按状态筛选，而进展实时算是因为它变化频繁。refresh 必须**幂等** ——
// 状态没变就不写，否则每次读都产生一次 UPDATE。
func (s *CrossRequirementService) RefreshDerivedStatus(id string) error {
	x, err := s.Get(id)
	if err != nil {
		return err
	}
	// 设计中 / 已归档是「用户显式状态」，不被派生逻辑覆盖 —— 早退，省掉
	// 一次没必要的进展聚合。
	if x.Status == model.XReqStatusDesigning || x.Status == model.XReqStatusArchived {
		return nil
	}
	progress, err := s.Progress(id)
	if err != nil {
		return err
	}
	_, err = s.ApplyDerivedStatus(x, progress, nil)
	return err
}

// ApplyDerivedStatus recomputes the lifecycle status of a cross requirement
// from an already-loaded row + progress, writes it back when it changed, and
// returns the effective status.
//
// 读路径（列表页 / 详情页）已经算好了 progress，直接复用它即可 —— 不用
// 为了刷新状态再跑一遍聚合。
//
// phases 可以传 nil（此时按需加载阶段），也可以传调用方已经查好的阶段列表
// —— 详情页已经把阶段装进载荷里了，不必再查一次。
//
// 返回值是**刷新后生效的状态**：刷新可能因为「状态没变」而跳过写入，但读
// 路径仍要把当前生效的状态回填到响应里，否则详情页 / 列表页会显示旧值。
// 状态真的变了时，x 会被就地更新（含 updated_at / completed_at），调用方
// 不必再查一次库。
func (s *CrossRequirementService) ApplyDerivedStatus(
	x *model.CrossRequirement,
	progress model.CrossRequirementProgress,
	phases []model.CrossRequirementPhase,
) (string, error) {
	// 人工锁定的状态不被派生逻辑覆盖 —— 人工的判断不能被读路径悄悄推翻
	// （见 SetStatus 的说明）。
	if x.StatusLocked {
		return x.Status, nil
	}
	// 设计中 / 已归档是「用户显式状态」，不被派生逻辑覆盖。
	if x.Status == model.XReqStatusDesigning || x.Status == model.XReqStatusArchived {
		return x.Status, nil
	}

	// 阶段只在「子需求全部完成」时才会影响结论（否则不是 developing，就是
	// 还没走到阶段那一步），所以不用每次都去查阶段。
	if phases == nil && progress.Total > 0 && progress.Done == progress.Total {
		var err error
		phases, err = s.ListPhases(x.ID)
		if err != nil {
			return "", err
		}
	}

	next := x.Status
	switch {
	case progress.Total == 0:
		// 还没有子需求：回到「已设计」或「草稿」。
		if strings.TrimSpace(x.DesignDocs) != "" && x.DesignDocs != "[]" {
			next = model.XReqStatusDesigned
		} else {
			next = model.XReqStatusDraft
		}
	case progress.Done < progress.Total:
		next = model.XReqStatusDeveloping
	default:
		// 全部子需求完成：有阶段就等阶段结论。
		if phasesPendingConclusion(phases) {
			next = model.XReqStatusIntegrating
		} else {
			next = model.XReqStatusDone
		}
	}

	if next == x.Status {
		return next, nil
	}
	if err := s.writeDerivedStatus(x.ID, next); err != nil {
		return "", err
	}
	// 状态变了 → 行上的时间戳也变了（updated_at / completed_at）。重新读一次，
	// 保证调用方拿到的结构体与库里的行完全一致。
	fresh, err := s.Get(x.ID)
	if err != nil {
		return "", err
	}
	*x = *fresh
	return next, nil
}

// phasesPendingConclusion reports whether any phase still lacks a final
// verdict, i.e. the cross requirement is not finished yet.
//
// 只有 passed / skipped 是终态。pending / ready / verifying / pending_review
// 表示阶段还在跑；**failed 也算未完成** —— 联调不通过意味着要返工，整体需求
// 不能被标成 done，否则列表页会把「联调失败」显示成 100% 完成。
func phasesPendingConclusion(phases []model.CrossRequirementPhase) bool {
	for _, p := range phases {
		switch p.Status {
		case model.PhaseStatusPending, model.PhaseStatusReady,
			model.PhaseStatusVerifying, model.PhaseStatusPendingReview,
			model.PhaseStatusFailed:
			return true
		}
	}
	return false
}

// ── 整体方案 ───────────────────────────────────────────────────────────────

// SaveDesign stores the overall design document.
//
// design_docs is stored as a JSON array (same shape as requirements.design_docs)
// so the frontend can reuse the same Markdown renderer. The incoming Markdown
// is wrapped into a one-element array.
//
// 保存方案 = 状态所依据的事实变了，所以这里顺带**释放人工锁**：机器重新接管
// 生命周期（调用方随后会刷新一次派生状态）。
func (s *CrossRequirementService) SaveDesign(id string, markdown string, sessionID string, jobID string) error {
	docs := "[]"
	if trimmed := strings.TrimSpace(markdown); trimmed != "" {
		if raw, err := json.Marshal([]string{trimmed}); err == nil {
			docs = string(raw)
		}
	}
	res, err := s.db.Exec(
		`UPDATE cross_requirements SET design_docs=?,design_session_id=?,design_job_id=?,status_locked=0,updated_at=? WHERE id=?`,
		docs, strings.TrimSpace(sessionID), strings.TrimSpace(jobID), time.Now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCrossRequirementNotFound
	}
	return nil
}

// DesignMarkdown returns the overall design document as plain Markdown.
func (s *CrossRequirementService) DesignMarkdown(id string) (string, error) {
	x, err := s.Get(id)
	if err != nil {
		return "", err
	}
	return DesignDocsToMarkdown(x.DesignDocs), nil
}

// DesignDocsToMarkdown extracts the Markdown body out of a design_docs JSON
// array. Tolerant: a non-array value is returned as-is (trimmed).
func DesignDocsToMarkdown(docs string) string {
	trimmed := strings.TrimSpace(docs)
	if trimmed == "" {
		return ""
	}
	var arr []string
	if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
		parts := make([]string, 0, len(arr))
		for _, s := range arr {
			if t := strings.TrimSpace(s); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return trimmed
}

// ── 阶段 ───────────────────────────────────────────────────────────────────

// ListPhases returns the phases of a cross requirement with their members.
//
// The pending / ready statuses are DERIVED here (they are not stored):
// a phase is ready when every member requirement is done and its
// prerequisites are met. passed / failed / skipped are human verdicts and are
// stored as-is — a refresh never overwrites them.
func (s *CrossRequirementService) ListPhases(id string) ([]model.CrossRequirementPhase, error) {
	rows, err := s.db.Query(
		`SELECT id,cross_requirement_id,name,description,sort_order,status,verify_job_id,verify_report,verified_at,created_at,updated_at
		 FROM cross_requirement_phases WHERE cross_requirement_id = ? ORDER BY sort_order ASC, created_at ASC, id ASC`, id)
	if err != nil {
		return nil, err
	}
	items := []model.CrossRequirementPhase{}
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
		items = append(items, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range items {
		members, err := s.phaseMembers(items[i].ID)
		if err != nil {
			return nil, err
		}
		items[i].Items = members
		items[i].Status = derivePhaseStatus(items[i].Status, members)
	}
	return items, nil
}

// phaseMembers returns the phase's member requirements with their statuses.
type phaseMember struct {
	requirementID string
	status        string
	blocked       bool
}

func (s *CrossRequirementService) phaseMembers(phaseID string) ([]model.CrossRequirementPhaseItem, error) {
	rows, err := s.db.Query(
		`SELECT i.requirement_id, COALESCE(r.status,'')
		 FROM cross_requirement_phase_items i
		 LEFT JOIN requirements r ON r.id = i.requirement_id
		 WHERE i.phase_id = ? ORDER BY r.split_order ASC, r.created_at ASC, i.id ASC`, phaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CrossRequirementPhaseItem{}
	for rows.Next() {
		var item model.CrossRequirementPhaseItem
		if err := rows.Scan(&item.RequirementID, &item.Status); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// derivePhaseStatus computes the effective status of a phase.
//
// Human verdicts (passed / failed / skipped) and the in-flight status
// (verifying / pending_review) are stored and returned verbatim. Only
// pending ↔ ready is derived from the members' state.
func derivePhaseStatus(stored string, members []model.CrossRequirementPhaseItem) string {
	switch stored {
	case model.PhaseStatusPassed, model.PhaseStatusFailed, model.PhaseStatusSkipped,
		model.PhaseStatusVerifying, model.PhaseStatusPendingReview:
		return stored
	}
	if len(members) == 0 {
		return model.PhaseStatusPending
	}
	for _, m := range members {
		if m.Status != "done" {
			return model.PhaseStatusPending
		}
	}
	return model.PhaseStatusReady
}

// CreatePhase adds a phase to a cross requirement.
func (s *CrossRequirementService) CreatePhase(id string, req model.CrossRequirementPhaseReq) (*model.CrossRequirementPhase, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("phase name is required")
	}
	if err := s.requireChildren(id, req.RequirementIDs); err != nil {
		return nil, err
	}
	var maxOrder int
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(sort_order),0) FROM cross_requirement_phases WHERE cross_requirement_id = ?`, id).Scan(&maxOrder); err != nil {
		return nil, err
	}
	phaseID := util.NewID("xph")
	now := time.Now()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO cross_requirement_phases (id,cross_requirement_id,name,description,sort_order,status,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		phaseID, id, name, strings.TrimSpace(req.Description), maxOrder+1, model.PhaseStatusPending, now, now); err != nil {
		return nil, err
	}
	for _, rid := range req.RequirementIDs {
		if _, err := tx.Exec(
			`INSERT INTO cross_requirement_phase_items (id,phase_id,requirement_id) VALUES (?,?,?)`,
			util.NewID("xpi"), phaseID, strings.TrimSpace(rid)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetPhase(id, phaseID)
}

// UpdatePhase updates a phase's editable fields / members / verdict.
func (s *CrossRequirementService) UpdatePhase(id string, phaseID string, req model.UpdateCrossRequirementPhaseReq) (*model.CrossRequirementPhase, error) {
	current, err := s.GetPhase(id, phaseID)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = current.Name
	}
	description := req.Description
	if description == "" {
		description = current.Description
	}
	status := strings.TrimSpace(req.Status)
	if status != "" {
		switch status {
		case model.PhaseStatusReady,
			model.PhaseStatusPassed, model.PhaseStatusFailed, model.PhaseStatusSkipped:
			// ready 是**可写**的：联调不通过（failed）之后要能返工重来，
			// 必须有一个把阶段放回「就绪」的入口 —— 否则阶段永远停在 failed，
			// 既跑不了下一轮验证，也没法改判。写回 ready 后，状态重新由读路径
			// 按成员完成情况派生（成员没做完就是 pending）。
		default:
			return nil, fmt.Errorf("invalid phase status: %q", status)
		}
	} else {
		status = current.Status
	}
	report := req.VerifyReport
	if report == "" {
		report = current.VerifyReport
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE cross_requirement_phases SET name=?,description=?,status=?,verify_report=?,verified_at=?,updated_at=? WHERE id=? AND cross_requirement_id=?`,
		name, description, status, report, time.Now(), time.Now(), phaseID, id); err != nil {
		return nil, err
	}
	if req.RequirementIDs != nil {
		if err := s.requireChildren(id, req.RequirementIDs); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM cross_requirement_phase_items WHERE phase_id=?`, phaseID); err != nil {
			return nil, err
		}
		for _, rid := range req.RequirementIDs {
			if _, err := tx.Exec(
				`INSERT INTO cross_requirement_phase_items (id,phase_id,requirement_id) VALUES (?,?,?)`,
				util.NewID("xpi"), phaseID, strings.TrimSpace(rid)); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetPhase(id, phaseID)
}

// GetPhase reads one phase.
func (s *CrossRequirementService) GetPhase(id string, phaseID string) (*model.CrossRequirementPhase, error) {
	phases, err := s.ListPhases(id)
	if err != nil {
		return nil, err
	}
	for i := range phases {
		if phases[i].ID == phaseID {
			return &phases[i], nil
		}
	}
	return nil, fmt.Errorf("phase %s not found", phaseID)
}

// DeletePhase removes a phase and its member rows.
func (s *CrossRequirementService) DeletePhase(id string, phaseID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM cross_requirement_phase_items WHERE phase_id=?`, phaseID); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM cross_requirement_phases WHERE id=? AND cross_requirement_id=?`, phaseID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("phase %s not found", phaseID)
	}
	return tx.Commit()
}

// SetPhaseVerifyJob stamps (or clears) the phase's running verification job.
func (s *CrossRequirementService) SetPhaseVerifyJob(phaseID string, jobID string) error {
	_, err := s.db.Exec(`UPDATE cross_requirement_phases SET verify_job_id=?,updated_at=? WHERE id=?`,
		strings.TrimSpace(jobID), time.Now(), phaseID)
	return err
}

// SetPhaseVerifyReport stores the verification report produced by the AI job.
//
// The report is PERSISTED (not left in the JobStore ring buffer) so it
// survives a backend restart — the phase's verification result is a fact about
// the project, not a transient stream.
func (s *CrossRequirementService) SetPhaseVerifyReport(phaseID string, report string) error {
	_, err := s.db.Exec(
		`UPDATE cross_requirement_phases SET verify_report=?,status=?,verified_at=?,updated_at=? WHERE id=?`,
		report, model.PhaseStatusPendingReview, time.Now(), time.Now(), phaseID)
	return err
}

// requireChildren validates that every referenced requirement really belongs
// to this cross requirement.
//
// A phase member pointing at a foreign requirement would let a phase claim
// "the API contract is verified" while actually covering an unrelated task.
func (s *CrossRequirementService) requireChildren(id string, requirementIDs []string) error {
	if len(requirementIDs) == 0 {
		return nil
	}
	for _, rid := range requirementIDs {
		trimmed := strings.TrimSpace(rid)
		if trimmed == "" {
			continue
		}
		var owner string
		err := s.db.QueryRow(`SELECT cross_requirement_id FROM requirements WHERE id = ?`, trimmed).Scan(&owner)
		if err == sql.ErrNoRows {
			return fmt.Errorf("requirement %s not found", trimmed)
		}
		if err != nil {
			return err
		}
		if owner != id {
			return fmt.Errorf("requirement %s does not belong to this cross requirement", trimmed)
		}
	}
	return nil
}

// ── 详情聚合 ───────────────────────────────────────────────────────────────

// DetailFresh assembles the full detail payload **and** refreshes the derived
// lifecycle status, so the payload never carries a stale status.
//
// 刷新在装配之后进行，但刷新结果会回填到载荷的 status 字段 —— 子需求在别处
// 完成后，详情页立刻显示新状态，而不是刷新前的旧值。
//
// 刷新失败不会让读取失败（退回库里存的状态），这跟读路径的其它聚合一致。
func (s *CrossRequirementService) DetailFresh(id string) (*model.CrossRequirementDetail, error) {
	detail, err := s.Detail(id)
	if err != nil {
		return nil, err
	}
	status, err := s.ApplyDerivedStatus(&detail.CrossRequirement, detail.Progress, detail.Phases)
	if err != nil {
		log.Printf("[xreq] refresh derived status %s: %v", id, err)
		return detail, nil
	}
	detail.Status = status
	return detail, nil
}

// Detail assembles the full detail payload of a cross requirement.
func (s *CrossRequirementService) Detail(id string) (*model.CrossRequirementDetail, error) {
	x, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	projects, err := s.ListProjects(id)
	if err != nil {
		return nil, err
	}
	progress, err := s.Progress(id)
	if err != nil {
		return nil, err
	}
	children, err := s.ListChildren(id)
	if err != nil {
		return nil, err
	}
	deps, err := s.ListDependencies(id)
	if err != nil {
		return nil, err
	}
	phases, err := s.ListPhases(id)
	if err != nil {
		return nil, err
	}

	detail := &model.CrossRequirementDetail{
		CrossRequirement: *x,
		Projects:         progress.Projects,
		Children:         children,
		Dependencies:     deps,
		Phases:           phases,
		Progress:         progress,
	}
	// The per-project role comes from the mapping (the progress aggregate
	// only knows counts).
	roleByProject := map[string]string{}
	for _, p := range projects {
		roleByProject[p.ProjectID] = p.Role
	}
	for i := range detail.Projects {
		if role, ok := roleByProject[detail.Projects[i].ProjectID]; ok {
			detail.Projects[i].Role = role
		}
	}
	return detail, nil
}
