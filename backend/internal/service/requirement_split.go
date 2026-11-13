package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// 「方案拆分」的服务层：把一批候选子需求**原子地**落库为 requirements 行，
// 并把子需求之间的先后顺序落进独立的依赖边表。
//
// 三条不变量（改这块代码前先读一遍，它们是整套设计的骨架）：
//
//  1. **平级**：所有子需求的 parent_id 都等于主需求 id，互相不是父子。
//     层级只有一层，依赖不参与层级判定。
//  2. **基线 = 主需求分支（堆叠 PR）**：每个子需求的 base_branch ≡ 主需求的
//     **开发分支**（feat/req_<master>），base_branch_source 恒为
//     model.BaseSourceParent（绝不是 explicit —— 用户是在主需求上选的，
//     子需求只是跟随）。
//
//     子需求从主需求的开发分支上开枝，于是它们的 PR 全部打到
//     feat/req_<master>，整摞子需求形成一条自底向上的堆叠栈：先合主需求
//     分支，再合各子需求分支。主需求分支本身在拆分时会被创建（从用户选的
//     来源分支 checkout 出来）并推送到远端 —— 堆叠 PR 的 base 必须存在于
//     origin 上，否则子需求的 PR 全部无法创建。
//  3. **原子**：子需求行 + 依赖边 + 主需求 dev_delegated=1 在同一个事务
//     里落库。任一步失败整体回滚 —— 半截的拆分比不拆分更糟。
//
// 依赖成环在落库阶段用 Kahn 拓扑排序挡掉（不只信模型输出的 depends_on），
// 成环 ⇒ ErrDependencyCycle ⇒ 事务回滚。

// ErrDependencyCycle — 依赖边成环。
var ErrDependencyCycle = errors.New("requirement dependency cycle detected")

// OrderSplitTasks 把候选子需求整理成**展示顺序**，并把序号归一成连续的
// 1..N（批次内序号空间）。
//
// 返回值：
//   - ordered —— 按展示顺序排好的副本，每条任务的 Order 已被改写成它的新
//     序号（1 起算、连续、无重复）。
//   - remap —— **旧序号 → 新序号**的映射。调用方用它把「按序号表达」的
//     引用翻译到新序号空间：depends_on（前置依赖）与 references（互相参考）。
//     旧序号不存在于映射里 = 这个引用指向了不存在的候选，调用方应当丢掉它。
//
// 规则：
//   - Order > 0 的按 Order 排序（稳定排序：Order 相同者保持输入顺序）。
//   - Order 缺失 / 非正的，按**输入位置**补一个序号（与 prompt 归一化层
//     和前端的处理口径一致）。
//   - 排序后统一重编号为 1..N —— 于是「序号」在整条链路上只有一个坐标系：
//     模型输出 → 前端候选表 → 落库顺序 → 依赖标记，全都指向同一批行。
//
// 输入里出现**重复**的 Order 是自相矛盾的输入（同一个序号指向两行）：
// 映射按**首次出现**为准，另一行指向的引用会被丢弃。真实链路不会走到
// 这条分支 —— prompt 归一化层与前端都保证序号连续唯一。
func OrderSplitTasks(tasks []model.SplitTaskInput) ([]model.SplitTaskInput, map[int]int) {
	ordered := make([]model.SplitTaskInput, len(tasks))
	copy(ordered, tasks)

	// 缺失的序号按输入位置补齐。
	for i := range ordered {
		if ordered[i].Order <= 0 {
			ordered[i].Order = i + 1
		}
	}
	// 稳定排序：Order 相同者保持输入顺序。
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })

	// 重编号为 1..N，并记录旧序号 → 新序号。
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

// RemapSplitOrdinals 把一批「按序号表达」的引用翻译到新的序号空间。
//
// 指向不存在序号的引用会被丢掉（返回的切片里没有它）—— 一个指向虚空的
// 前置依赖会让子需求永远卡在「前置未完成」，比没有依赖更糟。
func RemapSplitOrdinals(in []int, remap map[int]int) []int {
	if len(in) == 0 {
		return nil
	}
	out := make([]int, 0, len(in))
	seen := map[int]struct{}{}
	for _, ord := range in {
		next, ok := remap[ord]
		if !ok {
			continue
		}
		if _, dup := seen[next]; dup {
			continue
		}
		seen[next] = struct{}{}
		out = append(out, next)
	}
	sort.Ints(out)
	return out
}

// SplitCreateOpts — CreateChildren 的可选参数。
type SplitCreateOpts struct {
	// BatchID — 本次拆分的批次 id。留空则新铸一个 spb_<hex>。
	BatchID string
	// SkipAnalysis — 子需求是否跳过需求分析阶段。nil = 默认 true：拆分产物
	// 已经过主需求的分析与方案，不必再跑一遍 analyst（architect 阶段的
	// 「无分析会话」分支本来就只在 !skipAnalysis 时才要求会话存在）。
	SkipAnalysis *bool
	// BaseBranch — 子需求共同继承的基线（堆叠 PR 的栈底，即主需求的开发
	// 分支 feat/req_<master>）。留空则由 splitBaseBranch 按主需求自身的
	// 分支 / 基线 / 项目默认推导。
	//
	// 调用方（handler）在拆分时已经把这个分支创建出来并推送到了远端，
	// 所以这里显式传下来，避免服务层再推导一次推导出另一条线。
	BaseBranch string
}

// splitMaxTasks — 单次拆分的子需求数量上限。契约里写死 2–12，超出截断。
const splitMaxTasks = 12

// CreateChildren —— 在一个事务里把候选子需求落库。
//
// parent 必须是主需求（顶层需求）。tasks 是候选；deps 是依赖边（调用方负责
// 把「批次内序号」翻译成真实 id —— 边必须和子需求行在同一个事务里插，所以
// id 要先铸好）。
//
// **顺序**：函数内部用 OrderSplitTasks 把候选排成展示顺序，并把这一顺序
// 写进 requirements.split_order（1 起算）。调用方**必须**用同一个
// OrderSplitTasks 排过一遍再铸 id —— 排序是幂等的（对已排好的输入是恒等
// 变换），所以两边结果一致，id 与序号永远对齐。
//
// 返回落库后的子需求行（读回，保证 base_branch / split_batch_id 等字段完整）。
func (s *RequirementService) CreateChildren(
	parent *model.Requirement,
	tasks []model.SplitTaskInput,
	deps []model.DependencyEdge,
	opts SplitCreateOpts,
) ([]*model.Requirement, error) {
	if parent == nil {
		return nil, fmt.Errorf("split: parent requirement is nil")
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("split: no sub-requirements to create")
	}
	if len(tasks) > splitMaxTasks {
		tasks = tasks[:splitMaxTasks]
	}

	// Same rules as "create a child" / "attach an existing requirement":
	// same project, one level only, master not archived.
	if err := s.validateParentTarget(parent.ProjectID, parent.ID, true); err != nil {
		return nil, err
	}

	// 展示顺序 = 落库顺序。序号在这里被归一成连续的 1..N，并写进
	// split_order —— 一批子需求共用同一个 created_at，没有这一列的话顺序
	// 只能靠随机 id 排，回来就是乱的。
	tasks, _ = OrderSplitTasks(tasks)

	base := strings.TrimSpace(opts.BaseBranch)
	if base == "" {
		base = s.splitBaseBranch(parent)
	}

	batchID := strings.TrimSpace(opts.BatchID)
	if batchID == "" {
		batchID = util.NewID("spb")
	}
	skipAnalysis := true
	if opts.SkipAnalysis != nil {
		skipAnalysis = *opts.SkipAnalysis
	}

	// Pre-mint the child ids so the dependency edges (which reference real
	// ids) can be inserted in the SAME transaction as the rows themselves.
	// The caller may supply the ids (the split handler mints them before
	// translating the batch-local ordinals into edges); otherwise we mint.
	ids := make([]string, len(tasks))
	for i := range tasks {
		if id := strings.TrimSpace(tasks[i].ID); id != "" {
			ids[i] = id
			continue
		}
		ids[i] = util.NewID("req")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now()
	for i, task := range tasks {
		criteria := "[]"
		if len(task.Acceptance) > 0 {
			if raw, mErr := json.Marshal(task.Acceptance); mErr == nil {
				criteria = string(raw)
			}
		}
		if _, err := tx.Exec(
			`INSERT INTO requirements (id,project_id,title,description,status,priority,kind,acceptance_criteria,design_docs,conversation_ids,created_by,source_requirement_id,skip_analysis,skip_design,created_at,updated_at,parent_id,base_branch,base_branch_source,split_batch_id,dev_delegated,split_order)
			 VALUES (?,?,?,?,'draft','medium','requirement',?,'[]','[]','user','',?,0,?,?,?,?,?,?,0,?)`,
			ids[i], parent.ProjectID, strings.TrimSpace(task.Title), buildSplitDescription(task),
			criteria, boolToInt(skipAnalysis), now, now, parent.ID,
			base, model.BaseSourceParent, batchID, i+1); err != nil {
			return nil, fmt.Errorf("split: insert sub-requirement %d: %w", i, err)
		}
	}

	for _, edge := range deps {
		if edge.RequirementID == "" || edge.DependsOnID == "" {
			continue
		}
		if edge.RequirementID == edge.DependsOnID {
			return nil, fmt.Errorf("%w: a requirement cannot depend on itself", ErrDependencyCycle)
		}
		if _, err := tx.Exec(
			`INSERT INTO requirement_dependencies (id,requirement_id,depends_on_id,batch_id,created_at) VALUES (?,?,?,?,?)`,
			util.NewID("dep"), edge.RequirementID, edge.DependsOnID, batchID, now); err != nil {
			return nil, fmt.Errorf("split: insert dependency edge: %w", err)
		}
	}

	// Topology check BEFORE the master's dev_delegated flip: a cycle must
	// abort the whole batch, not leave a half-committed split behind.
	if err := checkDependencyAcyclic(ids, deps); err != nil {
		return nil, err
	}

	// Development is handed down to the children from now on. The coding
	// gate refuses to promote this requirement into "developing".
	if _, err := tx.Exec("UPDATE requirements SET dev_delegated=1, updated_at=? WHERE id=?", now, parent.ID); err != nil {
		return nil, fmt.Errorf("split: flag master dev_delegated: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	out := make([]*model.Requirement, 0, len(ids))
	for _, id := range ids {
		row, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// splitBaseBranch resolves the base branch every child inherits.
//
// The mapping is the identity on the master's DEVELOPMENT branch: every
// requirement that enters development lands on feat/req_<id>, and a split
// master keeps that branch as the stack bottom its children branch off.
// Fallbacks: master development branch → master base → project default →
// "main".
//
// The source is ALWAYS model.BaseSourceParent — never explicit. "explicit"
// means "the user picked this on THIS requirement", and the user did not:
// they picked it on the master, and the child merely follows.
func (s *RequirementService) splitBaseBranch(parent *model.Requirement) string {
	if b := strings.TrimSpace(parent.BranchName); b != "" {
		return b
	}
	if b := strings.TrimSpace(parent.BaseBranch); b != "" {
		return b
	}
	var projectDefault string
	if err := s.db.QueryRow("SELECT default_branch FROM projects WHERE id = ?", parent.ProjectID).Scan(&projectDefault); err == nil {
		if b := strings.TrimSpace(projectDefault); b != "" {
			return b
		}
	}
	return "main"
}

// buildSplitDescription assembles a child's description **deterministically**
// from the split candidate. The model's free text is only used for the "目标"
// paragraph; the scope / acceptance / boundary sections are rendered from
// their own fields so the child body always has the same shape.
//
// The body must be self-contained: the child's execution agent never sees the
// master's design docs — it only sees its own description plus the L1 master
// context block.
func buildSplitDescription(task model.SplitTaskInput) string {
	var b strings.Builder
	b.WriteString("## 目标\n")
	if desc := strings.TrimSpace(task.Description); desc != "" {
		b.WriteString(desc)
		b.WriteString("\n")
	}
	if scope := cleanList(task.Scope); len(scope) > 0 {
		b.WriteString("\n## 涉及范围\n")
		for _, s := range scope {
			b.WriteString("- " + s + "\n")
		}
	}
	if acc := cleanList(task.Acceptance); len(acc) > 0 {
		b.WriteString("\n## 验收标准\n")
		for _, a := range acc {
			b.WriteString("- " + a + "\n")
		}
	}
	if boundary := strings.TrimSpace(task.Boundary); boundary != "" {
		b.WriteString("\n## 与其它子需求的边界\n")
		b.WriteString(boundary)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// cleanList trims each entry and drops the empty ones.
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// boolToInt maps a bool onto the INTEGER(0/1) convention used by the
// requirements table (no_branch / dev_delegated / skip_analysis).

// checkDependencyAcyclic runs Kahn's algorithm over the batch's edges.
//
// Nodes are the batch's child ids; an edge goes depends_on_id →
// requirement_id (prerequisite → dependent). If not every node can be
// emitted, the graph has a cycle.
//
// The check runs on the in-memory edge list (not on the DB) so a cycle is
// detected BEFORE the transaction commits — the caller rolls back instead of
// discovering the cycle after the rows landed.
func checkDependencyAcyclic(nodeIDs []string, deps []model.DependencyEdge) error {
	inBatch := make(map[string]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		inBatch[id] = struct{}{}
	}
	// adj[prerequisite] = the dependents it unblocks.
	adj := make(map[string][]string, len(nodeIDs))
	indegree := make(map[string]int, len(nodeIDs))
	for _, id := range nodeIDs {
		indegree[id] = 0
	}
	seen := make(map[string]struct{})
	for _, e := range deps {
		if _, ok := inBatch[e.RequirementID]; !ok {
			continue
		}
		if _, ok := inBatch[e.DependsOnID]; !ok {
			continue
		}
		key := e.RequirementID + "\x00" + e.DependsOnID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		adj[e.DependsOnID] = append(adj[e.DependsOnID], e.RequirementID)
		indegree[e.RequirementID]++
	}

	queue := make([]string, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	emitted := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		emitted++
		for _, next := range adj[cur] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if emitted != len(nodeIDs) {
		return fmt.Errorf("%w: %d of %d sub-requirements are in a cycle", ErrDependencyCycle, len(nodeIDs)-emitted, len(nodeIDs))
	}
	return nil
}

// ListChildren returns the direct children of a requirement, in the order the
// user arranged them.
//
// 排序规则：**批次序号优先**。split_order > 0 的行（拆分产物）按序号升序，
// split_order = 0 的行（人工创建 / 历史数据）排在后面并按创建时间兜底。
//
// 为什么不能只按 created_at：一次拆分的一批子需求**共用同一个 created_at**
//（一个事务、一个 now），所以「按创建时间排」在批内是未定义的 —— 实际退化成
// 按随机 req_<hex> id 排，子需求回来时顺序是乱的，依赖标记（「依赖 ②」）也
// 跟着指错行。split_order 是批次顺序唯一的持久化落点。
func (s *RequirementService) ListChildren(parentID string) ([]*model.Requirement, error) {
	if strings.TrimSpace(parentID) == "" {
		return []*model.Requirement{}, nil
	}
	rows, err := s.db.Query(
		`SELECT id FROM requirements WHERE parent_id = ?
		 ORDER BY CASE WHEN split_order > 0 THEN 0 ELSE 1 END ASC, split_order ASC, created_at ASC, id ASC`, parentID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]*model.Requirement, 0, len(ids))
	for _, id := range ids {
		row, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// UnmetDependencies returns the prerequisites of a requirement that are not
// done yet. The coding gate uses it to refuse work whose inputs are missing.
//
// A prerequisite that was deleted is filtered out by the JOIN (its row is
// gone) — and the delete path cleans both edge directions, so a dangling edge
// cannot block the gate forever.
//
// 前置依赖按**批次序号**排序（split_order），与子需求列表同序 —— 用户在
// 拆分预览里看到的「① → ② → ③」在「还差哪些前置」里也是同一个顺序。
func (s *RequirementService) UnmetDependencies(reqID string) ([]*model.Requirement, error) {
	rows, err := s.db.Query(
		`SELECT r.id FROM requirements r
		 JOIN requirement_dependencies d ON d.depends_on_id = r.id
		 WHERE d.requirement_id = ? AND r.status != 'done'
		 ORDER BY CASE WHEN r.split_order > 0 THEN 0 ELSE 1 END ASC, r.split_order ASC, r.created_at ASC, r.id ASC`, reqID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]*model.Requirement, 0, len(ids))
	for _, id := range ids {
		row, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// ListDependencies returns a requirement's prerequisites and dependents.
//
// pre = requirements this one waits for; post = requirements that wait for
// this one. Both carry the other side's title / status so the UI can render
// "依赖 ①" / "被 ② 依赖" without a second round trip.
func (s *RequirementService) ListDependencies(reqID string) (pre []model.RequirementDependency, post []model.RequirementDependency, err error) {
	pre = []model.RequirementDependency{}
	post = []model.RequirementDependency{}

	preRows, err := s.db.Query(
		`SELECT d.depends_on_id, d.batch_id, COALESCE(r.title,''), COALESCE(r.status,'')
		 FROM requirement_dependencies d
		 LEFT JOIN requirements r ON r.id = d.depends_on_id
		 WHERE d.requirement_id = ? ORDER BY d.created_at, d.id`, reqID)
	if err != nil {
		return nil, nil, err
	}
	for preRows.Next() {
		var dep model.RequirementDependency
		dep.RequirementID = reqID
		if err := preRows.Scan(&dep.DependsOnID, &dep.BatchID, &dep.Title, &dep.Status); err != nil {
			preRows.Close()
			return nil, nil, err
		}
		pre = append(pre, dep)
	}
	preRows.Close()
	if err := preRows.Err(); err != nil {
		return nil, nil, err
	}

	postRows, err := s.db.Query(
		`SELECT d.requirement_id, d.batch_id, COALESCE(r.title,''), COALESCE(r.status,'')
		 FROM requirement_dependencies d
		 LEFT JOIN requirements r ON r.id = d.requirement_id
		 WHERE d.depends_on_id = ? ORDER BY d.created_at, d.id`, reqID)
	if err != nil {
		return nil, nil, err
	}
	for postRows.Next() {
		var dep model.RequirementDependency
		dep.DependsOnID = reqID
		if err := postRows.Scan(&dep.RequirementID, &dep.BatchID, &dep.Title, &dep.Status); err != nil {
			postRows.Close()
			return nil, nil, err
		}
		post = append(post, dep)
	}
	postRows.Close()
	if err := postRows.Err(); err != nil {
		return nil, nil, err
	}

	return pre, post, nil
}
