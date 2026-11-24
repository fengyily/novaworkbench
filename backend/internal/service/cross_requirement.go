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

// ErrCrossRequirementNoProject — 跨项目需求没有参与项目。
//
// 做成哨兵错误而不是一句 fmt.Errorf：草稿行的创建路径要能**只**吞掉这一条
// （零项目对草稿合法），而把别的失败（项目不存在、基础分支名非法）照旧拒绝。
var ErrCrossRequirementNoProject = errors.New("cross requirement needs at least one project")

// ErrCrossRequirementInTrash — 该跨项目需求在回收站里，写操作一律拒绝。
//
// 回收站里的行是**只读**的：整体方案 / 拆分 / 阶段 / 验证 / 改参与项目一律
// 拒绝。否则用户会在垃圾堆里干活，而作业跑完会把结果写回一条已经被「删掉」
// 的行 —— 状态从此错乱，且用户以为自己已经删干净了。
var ErrCrossRequirementInTrash = errors.New("cross requirement is in the trash")

// ErrCrossRequirementNotInTrash — 该跨项目需求不在回收站里，不能彻底删除。
//
// 防御性闸门，抄 projects 的做法（service/project.go Purge 的 NOT_IN_TRASH）：
// 误点在「进行中」的行上点「彻底删除」时，应该被挡下来并引导用户先走软删。
var ErrCrossRequirementNotInTrash = errors.New("cross requirement is not in the trash")

// DraftCrossRequirementTitle — 草稿行的占位标题。
//
// 面板式创建流程在用户填完标题之前就先落一行（插图 / 附件需要一个已存在的
// 宿主行），标题此时还是空的。跨项目需求的 title 是 NOT NULL，所以草稿先用
// 一个占位标题顶上，提交时由前端改回真标题。
const DraftCrossRequirementTitle = "（草稿）"

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
//
// req.Draft relaxes exactly two of those constraints (empty title, zero
// projects) so the panel-style create flow can pre-create a writable row the
// moment it opens — inline images and file attachments need a row that
// already exists to hang off (the same trick CreateRequirementForm plays for
// ordinary requirements). Everything else (branch-name validation, project
// existence, status / priority defaults) is unchanged, and a draft that never
// gets submitted is deleted by the client.
func (s *CrossRequirementService) Create(req model.CreateCrossRequirementReq) (*model.CrossRequirement, error) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		if !req.Draft {
			return nil, fmt.Errorf("cross requirement title is required")
		}
		title = DraftCrossRequirementTitle
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

	projects, err := s.normalizeCreateProjects(req)
	if err != nil {
		// A draft may legitimately carry zero projects, so the "at least one"
		// complaint is the one error we swallow here. Any other failure (a
		// duplicated project, an illegal base-branch name) still rejects the
		// row — the branch name goes straight into a git command, so it has
		// to be legal before it is written.
		if !req.Draft || !errors.Is(err, ErrCrossRequirementNoProject) {
			return nil, err
		}
		projects = nil
	}
	if len(projects) == 0 && !req.Draft {
		return nil, ErrCrossRequirementNoProject
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
			`INSERT INTO cross_requirement_projects (id,cross_requirement_id,project_id,role,base_branch,sort_order,created_at)
			 VALUES (?,?,?,?,?,?,?)`,
			util.NewID("xrp"), id, p.ProjectID, p.Role, p.BaseBranch, i+1, now); err != nil {
			return nil, fmt.Errorf("attach project %s: %w", p.ProjectID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// xreqColumns — the column list shared by Get / List / ListTrash.
//
// 一处定义，三处使用：新增列时漏改一处，就会出现「详情页显示 A、列表页显示
// B」这种只有用户才看得出的分叉。
const xreqColumns = `id,title,description,status,priority,kind,acceptance_criteria,design_docs,` +
	`design_session_id,design_job_id,split_batch_id,status_locked,created_by,created_at,updated_at,completed_at,deleted_at`

// scanXReq scans one row into a CrossRequirement.
//
// created_at / updated_at / completed_at / deleted_at 都是可空的时间戳：
// 统一扫进 sql.NullString（驱动给 time.Time 时 convertAssign 会转成
// RFC3339 字符串），空值在模型层表现为空串。
func scanXReq(scan func(dest ...any) error) (model.CrossRequirement, error) {
	var x model.CrossRequirement
	var createdAt, updatedAt, completedAt, deletedAt sql.NullString
	if err := scan(
		&x.ID, &x.Title, &x.Description, &x.Status, &x.Priority, &x.Kind,
		&x.AcceptanceCriteria, &x.DesignDocs, &x.DesignSessionID, &x.DesignJobID, &x.SplitBatchID, &x.StatusLocked, &x.CreatedBy,
		&createdAt, &updatedAt, &completedAt, &deletedAt,
	); err != nil {
		return x, err
	}
	x.CreatedAt = createdAt.String
	x.UpdatedAt = updatedAt.String
	x.CompletedAt = completedAt.String
	x.DeletedAt = deletedAt.String
	return x, nil
}

// Get reads one cross requirement by id.
//
// 回收站里的行**照样读得到** —— 详情页要显示「已移入回收站」的横幅，用户
// 也要能从回收站里点进去看它长什么样再决定恢复还是彻底删除。
func (s *CrossRequirementService) Get(id string) (*model.CrossRequirement, error) {
	x, err := scanXReq(s.db.QueryRow(
		`SELECT `+xreqColumns+` FROM cross_requirements WHERE id = ?`, id).Scan)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrCrossRequirementNotFound
		}
		return nil, err
	}
	return &x, nil
}

// requireActive loads a cross requirement and refuses to act on it when it
// sits in the trash.
//
// 回收站里的行是只读的：这个闸门由**所有**写路径共用（更新 / 归档 / 改参与
// 项目 / 存方案 / 拆分 / 阶段 / 验证），否则用户会在垃圾堆里干活。
func (s *CrossRequirementService) requireActive(id string) (*model.CrossRequirement, error) {
	x, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if x.DeletedAt != "" {
		return nil, ErrCrossRequirementInTrash
	}
	return x, nil
}

// List returns the cross requirements, newest first.
//
// 两层过滤：
//   - 回收站里的行（deleted_at 非空）**不在这里** —— 它们由 ListTrash 单独
//     提供（抄 projects 的 active / trash 双视图）。
//   - 已归档的行除非显式按 status == "archived" 查，否则隐藏 —— 与需求列表
//     同一口径。
func (s *CrossRequirementService) List(status string, keyword string) ([]model.CrossRequirement, error) {
	where := "WHERE deleted_at IS NULL"
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
		`SELECT `+xreqColumns+` FROM cross_requirements `+where+` ORDER BY updated_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.CrossRequirement{}
	for rows.Next() {
		x, err := scanXReq(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListTrash returns the cross requirements sitting in the trash, most
// recently deleted first.
//
// 回收站列表**不按 status 过滤**：回收站里的行带着它被删时的生命周期状态
// （这是用户判断「这条是不是还有用」的关键信息），按删除时间倒序才是这个
// 视图的主序。
func (s *CrossRequirementService) ListTrash() ([]model.CrossRequirement, error) {
	rows, err := s.db.Query(
		`SELECT ` + xreqColumns + ` FROM cross_requirements WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.CrossRequirement{}
	for rows.Next() {
		x, err := scanXReq(rows.Scan)
		if err != nil {
			return nil, err
		}
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
// 验收标准与描述是唯一的例外：它们用「字段缺省 = nil，字段给了 = 非 nil」
// 区分，所以 `[]` / `""` 仍然可以清空它们。描述之所以要跟着验收标准一起改成
// 指针，是因为面板式编辑必须能**删掉**描述 —— 普通字符串下「没传这个字段」
// 与「把这个字段清空」都是 ""，用户永远删不掉它。
func (s *CrossRequirementService) Update(id string, req model.UpdateCrossRequirementReq) error {
	current, err := s.requireActive(id)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = current.Title
	}
	description := current.Description
	if req.Description != nil {
		description = *req.Description
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
	current, err := s.requireActive(id)
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

// MoveToTrash soft-deletes a cross requirement: the row stays, deleted_at is
// stamped, and the row disappears from the working list.
//
// 「回收站」不是「归档」的另一个名字 —— 归档是**保留**（收起来不显示），
// 删除是**移走**（可恢复）。两件事分开，status 与 deleted_at 也就分开了。
//
// 三件事刻意**不动**：
//
//   - 子需求 —— 它们继续在各自的项目里活着，开发不中断；
//   - 子需求与本体的关联（cross_requirement_id 不变）—— 所以「恢复」是
//     零成本的：整体方案 / 阶段 / 依赖关系全都还在原处；
//   - worktree —— 恢复之后还要用同一份基线读代码。清理留给 Purge。
func (s *CrossRequirementService) MoveToTrash(id string) error {
	now := time.Now()
	res, err := s.db.Exec(
		`UPDATE cross_requirements SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 0 行 = 要么行不存在，要么它已经在回收站里了。回读一次区分这两种
		// 情况 —— 调用方需要知道是「没这条」还是「已经删过了」。
		if _, err := s.Get(id); err != nil {
			return err
		}
		return ErrCrossRequirementInTrash
	}
	return nil
}

// Restore brings a cross requirement back from the trash.
//
// 只清 deleted_at，**不碰 status / status_locked**：
//
//   - 人工锁定过的状态（手动标 done / 归档）继续有效 —— 那是人的判断；
//   - 没锁的行由读路径的派生逻辑按**当前事实**重算（子需求可能在回收站期间
//     推进过，恢复时按旧状态显示才是错的）。
//
// updated_at 会刷新：恢复回来的行应该排在「进行中」列表的前面，而不是回到
// 它被删时的那一天。
func (s *CrossRequirementService) Restore(id string) error {
	now := time.Now()
	res, err := s.db.Exec(
		`UPDATE cross_requirements SET deleted_at=NULL, updated_at=? WHERE id=? AND deleted_at IS NOT NULL`,
		now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.Get(id); err != nil {
			return err
		}
		return ErrCrossRequirementNotInTrash
	}
	return nil
}

// Purge permanently removes a cross requirement and detaches (never deletes)
// its children.
//
// 只有**回收站里的行**才能 purge（防御性闸门，抄 projects 的 NOT_IN_TRASH）：
// 误点在「进行中」的行上点「彻底删除」时，应该被挡下来并引导用户先走软删。
//
// 删跨项目需求 ≠ 删子需求：子需求背后可能已经产出了代码、分支和 PR，级联删
// 掉会造成不可恢复的损失。所以这里只把 cross_requirement_id 置空（子需求
// 变成普通需求），再删映射 / 阶段 / 阶段成员 / 实体行。
func (s *CrossRequirementService) Purge(id string) error {
	current, err := s.Get(id)
	if err != nil {
		return err
	}
	if current.DeletedAt == "" {
		return ErrCrossRequirementNotInTrash
	}

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
	if _, err := tx.Exec(`DELETE FROM cross_requirement_bases WHERE cross_requirement_id=?`, id); err != nil {
		return err
	}
	// 附件行必须显式删：schema 声明了 ON DELETE CASCADE，但 SQLite 从不开
	// PRAGMA foreign_keys，级联是装饰 —— 留着就是孤儿行（与
	// service.Requirement.Delete 对 requirement_attachments 的处理同构）。
	// 磁盘上的文件不删（需求删除也是同一口径：泄漏的是磁盘空间，没有功能
	// 后果）。
	if _, err := tx.Exec(`DELETE FROM cross_requirement_attachments WHERE cross_requirement_id=?`, id); err != nil {
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
		return nil, ErrCrossRequirementNoProject
	}
	return out, nil
}

// normalizeCreateProjects resolves the participating projects of a NEW cross
// requirement.
//
// 两个来源，只取其一：
//
//   - req.Projects —— 完整描述（id + 角色 + 基础分支）。优先。
//   - req.ProjectIDs —— 只有 id（老客户端）。基础分支留空。
//
// 基础分支在这里被校验（合法分支名），非法值整批拒绝 —— 它会被塞进 git
// 命令，脏值必须在这里挡住。
func (s *CrossRequirementService) normalizeCreateProjects(req model.CreateCrossRequirementReq) ([]model.CrossRequirementProjectInput, error) {
	if len(req.Projects) == 0 {
		return s.normalizeProjectInputs(req.ProjectIDs)
	}
	out := []model.CrossRequirementProjectInput{}
	seen := map[string]struct{}{}
	for _, p := range req.Projects {
		pid := strings.TrimSpace(p.ProjectID)
		if pid == "" {
			continue
		}
		if _, dup := seen[pid]; dup {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateProject, pid)
		}
		seen[pid] = struct{}{}
		base, err := NormalizeBaseBranch(p.BaseBranch)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", pid, err)
		}
		out = append(out, model.CrossRequirementProjectInput{
			ProjectID:  pid,
			Role:       strings.TrimSpace(p.Role),
			BaseBranch: base,
		})
	}
	if len(out) == 0 {
		return nil, ErrCrossRequirementNoProject
	}
	return out, nil
}

// NormalizeBaseBranch cleans a user-supplied base branch name.
//
// 空 = 不指定（子需求沿用项目默认分支）。非空时必须是一个**合法**的分支名 ——
// 它会直接进 git 命令（`git checkout <base>`），所以空格 / 控制字符 / 路径
// 分隔符一律拒绝，而不是让 git 在运行时炸出一条看不懂的错。
//
// 导出的原因：跨项目作业（整体方案 / 拆分）会把这条分支名带进 git 命令，
// 读路径上要**再校验一次** —— 写进来时合法，不代表库里的旧值还合法。
func NormalizeBaseBranch(raw string) (string, error) {
	b := strings.TrimSpace(raw)
	if b == "" {
		return "", nil
	}
	if len(b) > 255 {
		return "", fmt.Errorf("base branch name is too long")
	}
	if strings.ContainsAny(b, " \t\r\n") {
		return "", fmt.Errorf("base branch name must not contain whitespace")
	}
	if strings.ContainsAny(b, "~^:?*[\\") {
		return "", fmt.Errorf("base branch name contains an invalid character")
	}
	if strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") {
		return "", fmt.Errorf("base branch name must not start with '-' or '/' and must not end with '/'")
	}
	if strings.Contains(b, "..") || strings.Contains(b, "//") {
		return "", fmt.Errorf("base branch name must not contain '..'")
	}
	if strings.HasSuffix(b, ".lock") {
		return "", fmt.Errorf("base branch name must not end with '.lock'")
	}
	return b, nil
}

// SetProjects replaces the participating project set.
//
// REPLACE (not append) semantics: the user edits a full list in the UI, so a
// merge would leave removed projects behind. Project ids that do not exist
// are rejected — a mapping to a non-existent project would produce a detail
// page that can never resolve its project name.
//
// 每个参与项目可以带一个**基础分支**：这个仓库里的子需求从哪条分支开发。
// 非法分支名整批拒绝（它会进 git 命令，脏值必须在这里挡住）。
func (s *CrossRequirementService) SetProjects(id string, projects []model.CrossRequirementProjectInput) error {
	if _, err := s.requireActive(id); err != nil {
		return err
	}
	if len(projects) == 0 {
		return ErrCrossRequirementNoProject
	}
	seen := map[string]struct{}{}
	normalized := make([]model.CrossRequirementProjectInput, 0, len(projects))
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
		base, err := NormalizeBaseBranch(p.BaseBranch)
		if err != nil {
			return fmt.Errorf("project %s: %w", pid, err)
		}
		normalized = append(normalized, model.CrossRequirementProjectInput{
			ProjectID:  pid,
			Role:       strings.TrimSpace(p.Role),
			BaseBranch: base,
		})
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
	for i, p := range normalized {
		if _, err := tx.Exec(
			`INSERT INTO cross_requirement_projects (id,cross_requirement_id,project_id,role,base_branch,sort_order,created_at)
			 VALUES (?,?,?,?,?,?,?)`,
			util.NewID("xrp"), id, p.ProjectID, p.Role, p.BaseBranch, i+1, now); err != nil {
			return err
		}
		// 已经拆出来的子需求跟着改基础分支。
		//
		// 只改「继承来的」那些（base_branch_source == 'cross_project'）——
		// 用户在子需求上自己选过基础分支的（'explicit'）不动，那是用户的
		// 判断。同样只改**还没开始开发**的（没有分支 / 没有工作区）：代码已经
		// 落在某条线上时再换基础分支会让 PR 的目标漂移。
		baseSource := ""
		if p.BaseBranch != "" {
			baseSource = model.BaseSourceCrossProject
		}
		if _, err := tx.Exec(
			`UPDATE requirements SET base_branch=?,base_branch_source=?,updated_at=?
			 WHERE cross_requirement_id=? AND project_id=?
			   AND base_branch_source='cross_project'
			   AND COALESCE(branch_name,'')='' AND COALESCE(worktree_path,'')=''`,
			p.BaseBranch, baseSource, now, id, p.ProjectID); err != nil {
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
		`SELECT m.id,m.cross_requirement_id,m.project_id,COALESCE(p.name,''),m.role,m.base_branch,m.sort_order
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
		if err := rows.Scan(&p.ID, &p.CrossRequirementID, &p.ProjectID, &p.ProjectName, &p.Role, &p.BaseBranch, &p.SortOrder); err != nil {
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
//
// 前置依赖随子需求**一次性**取回（一条 JOIN），不再为每个子需求单跑一次查询
// —— 那正是列表页 N+1 的根因。
func (s *CrossRequirementService) ListChildren(id string) ([]model.CrossRequirementChild, error) {
	agg, err := s.loadChildAggregate([]string{id})
	if err != nil {
		return nil, err
	}
	return agg.Children[id], nil
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
//
// 子需求状态与前置依赖一次性取回（批量聚合），语句数与子需求个数无关。
func (s *CrossRequirementService) Progress(id string) (model.CrossRequirementProgress, error) {
	agg, err := s.loadChildAggregate([]string{id})
	if err != nil {
		return model.CrossRequirementProgress{
			Projects:     []model.CrossRequirementProjectProgress{},
			StatusCounts: map[string]int{},
		}, err
	}
	return agg.Progress[id], nil
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
	//
	// 回收站里的行同样早退：派生刷新会写 updated_at，而回收站按删除时间排序
	// —— 让读路径持续改写一条已经收起来的行没有意义。
	if x.Status == model.XReqStatusDesigning || x.Status == model.XReqStatusArchived || x.DeletedAt != "" {
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
	// 回收站里的行不刷新：它的状态冻结在「被移走的那一刻」，恢复之后由派生
	// 逻辑按当前事实重算（见 Restore）。
	if x.DeletedAt != "" {
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
	if _, err := s.requireActive(id); err != nil {
		return err
	}
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
//
// 阶段成员一次性取回（一条 JOIN），不再为每个阶段单跑一次查询。
func (s *CrossRequirementService) ListPhases(id string) ([]model.CrossRequirementPhase, error) {
	phases, err := s.loadPhaseAggregate([]string{id})
	if err != nil {
		return nil, err
	}
	return phases[id], nil
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
	if _, err := s.requireActive(id); err != nil {
		return nil, err
	}
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
	if _, err := s.requireActive(id); err != nil {
		return nil, err
	}
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
	if _, err := s.requireActive(id); err != nil {
		return err
	}
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
//
// 子需求、进展、阶段来自**同一份**批量聚合 —— 不再把 Progress 与
// ListChildren 各跑一遍（那是详情页的第二个 N+1）。
func (s *CrossRequirementService) Detail(id string) (*model.CrossRequirementDetail, error) {
	x, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	agg, err := s.LoadAggregate([]string{id})
	if err != nil {
		return nil, err
	}
	deps, err := s.ListDependencies(id)
	if err != nil {
		return nil, err
	}
	bases, err := s.ListBases(id)
	if err != nil {
		return nil, err
	}

	return &model.CrossRequirementDetail{
		CrossRequirement: *x,
		Projects:         agg.Progress[id].Projects,
		Children:         agg.Children[id],
		Dependencies:     deps,
		Phases:           agg.Phases[id],
		Progress:         agg.Progress[id],
		Bases:            bases,
	}, nil
}

// ── 基线台账 ───────────────────────────────────────────────────────────────

// UpsertBases records the baseline each participating repository was locked
// to during an overall-design / split run.
//
// One row per (cross requirement, project). The write is an upsert rather
// than a replace so a run that only reached some of the repositories does not
// erase what the previous run learned about the others — but the rows it DID
// reach are overwritten, because "the baseline moved" is exactly the fact
// that must not go stale.
func (s *CrossRequirementService) UpsertBases(xreqID string, bases []model.CrossRequirementBase) error {
	if strings.TrimSpace(xreqID) == "" {
		return fmt.Errorf("cross requirement id is empty")
	}
	now := time.Now()
	for _, b := range bases {
		pid := strings.TrimSpace(b.ProjectID)
		if pid == "" {
			continue
		}
		degraded := 0
		if b.Degraded {
			degraded = 1
		}
		if _, err := s.db.Exec(
			`INSERT INTO cross_requirement_bases (id,cross_requirement_id,project_id,base_branch,base_sha,worktree_path,degraded,updated_at)
			 VALUES (?,?,?,?,?,?,?,?)`+
				s.db.OnConflict("cross_requirement_id,project_id",
					"base_branch=excluded.base_branch, base_sha=excluded.base_sha, worktree_path=excluded.worktree_path, degraded=excluded.degraded, updated_at=excluded.updated_at"),
			util.NewID("xbase"), xreqID, pid,
			strings.TrimSpace(b.BaseBranch), strings.TrimSpace(b.BaseSHA),
			strings.TrimSpace(b.WorktreePath), degraded, now); err != nil {
			return err
		}
	}
	return nil
}

// ListBases returns the recorded baselines, ordered by the participating
// project's sort order so the UI can line them up with the project table.
//
// Projects that were never reached (no baseline row) are simply absent —
// the caller maps them onto its own project list.
func (s *CrossRequirementService) ListBases(xreqID string) ([]model.CrossRequirementBase, error) {
	if strings.TrimSpace(xreqID) == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT b.project_id,b.base_branch,b.base_sha,b.worktree_path,b.degraded
		   FROM cross_requirement_bases b
		  WHERE b.cross_requirement_id = ?
		  ORDER BY b.project_id ASC`, xreqID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.CrossRequirementBase{}
	for rows.Next() {
		var b model.CrossRequirementBase
		var degraded int
		if err := rows.Scan(&b.ProjectID, &b.BaseBranch, &b.BaseSHA, &b.WorktreePath, &degraded); err != nil {
			return nil, err
		}
		b.Degraded = degraded != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBases drops the baseline ledger of a cross requirement.
//
// Called when the cross requirement is deleted — the rows are meaningless
// without it, and the worktree paths they carry are cleaned up by the
// handler (the service layer does not touch the filesystem).
func (s *CrossRequirementService) DeleteBases(xreqID string) error {
	_, err := s.db.Exec(`DELETE FROM cross_requirement_bases WHERE cross_requirement_id=?`, xreqID)
	return err
}
