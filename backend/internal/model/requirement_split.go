package model

// 需求拆分的输入 / 输出形状。
//
// 一次「方案拆分」会产出一批**平级**子需求（全部 parent_id 指向同一个主需求）
// 以及它们之间的**依赖边**（先后顺序）。层级与依赖是两件事：层级走
// requirements.parent_id（仅一层），依赖走独立的 requirement_dependencies 边表
// （DAG），两者互不干扰 —— 这就是「子需求平级但有依赖顺序」的落点。

// SplitTaskInput — 一次拆分落库的单个子需求。
//
// Description 是**后端确定性拼装**好的完整正文（目标 / 涉及范围 / 验收标准 /
// 与其它子需求的边界），不是模型自由文本：子需求的执行 Agent 看不到主需求
// 方案，只有自己的描述 + 主需求上下文块，所以这段正文必须自包含。
type SplitTaskInput struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// ID — optional pre-minted row id. The split handler mints the ids
	// BEFORE calling CreateChildren so the dependency edges (which reference
	// real ids) can be inserted in the same transaction as the rows. Empty
	// means "mint one for me". Not settable from the wire — the client sends
	// batch-local ordinals, never ids.
	ID string `json:"-"`
	// Scope — 涉及的文件 / 模块路径。尽量不与其他子需求重叠，否则并行 PR
	// 会在 review 阶段冲突。
	Scope []string `json:"scope"`
	// Acceptance — 可核对的验收点。同时写入 requirements.acceptance_criteria。
	Acceptance []string `json:"acceptance"`
	// Boundary — 与兄弟子需求的分工边界 + 双方约定的接口契约。写契约，不写
	// 实现级耦合（「必须等 X 的代码合并后我才能编译」与「同基线」冲突）。
	Boundary string `json:"boundary"`
	// Order — 批次内的展示 / 排序序号（1-based）。
	Order int `json:"order"`
	// DependsOn — 本子需求依赖的**同批次序号**（1-based），不是数据库 id ——
	// 落库时才知道真实 id。只能引用 Order 更小的序号。
	DependsOn []int `json:"depends_on"`
}

// DependencyEdge — 一条依赖边：requirement_id 依赖 depends_on_id。
type DependencyEdge struct {
	RequirementID string `json:"requirement_id"`
	DependsOnID   string `json:"depends_on_id"`
	BatchID       string `json:"batch_id"`
}

// RequirementDependency — 读路径返回的依赖边（带对端标题 / 状态，方便前端
// 直接渲染「依赖 ①」与「被 ② 依赖」标记）。
type RequirementDependency struct {
	RequirementID string `json:"requirement_id"`
	DependsOnID   string `json:"depends_on_id"`
	BatchID       string `json:"batch_id"`
	Title         string `json:"title"`
	Status        string `json:"status"`
}
