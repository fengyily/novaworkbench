package prompt

import (
	"strconv"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// The contract is the whole contract between the model and the backend: if a
// rule silently disappears from it, the model starts violating it and nobody
// notices until a child requirement comes out wrong. Pin the load-bearing
// rules.
func TestSplitJSONContract_ContainsTheHardRules(t *testing.T) {
	c := SplitJSONContract
	mustContain := []string{
		"自包含",      // description must be self-contained
		"禁止指代",     // no "see above" style references
		"order 更小", // depends_on may only point backwards
		"最后一段",     // the json block must come last
		"2 到 12",   // the task count bound
		"禁止假设",     // no assuming non-existent interfaces
		"接口契约",     // boundary is contract-level, not implementation-level
	}
	for _, want := range mustContain {
		if !strings.Contains(c, want) {
			t.Errorf("SplitJSONContract is missing %q", want)
		}
	}
}

func TestBuildSplitPrompt_IncludesMasterPlanAndContract(t *testing.T) {
	req := &model.Requirement{ID: "req_p", Title: "大需求", Kind: "requirement", Description: "描述"}
	p := BuildSplitPrompt(req, "# 技术方案\n\n正文", nil, "")
	if !strings.Contains(p, "大需求") {
		t.Error("prompt is missing the master title")
	}
	if !strings.Contains(p, "技术方案") {
		t.Error("prompt is missing the design docs")
	}
	if !strings.Contains(p, "json") {
		t.Error("prompt is missing the json contract")
	}
	if BuildSplitPrompt(nil, "x", nil, "") != "" {
		t.Error("a nil requirement must degrade to an empty prompt")
	}
}

// Existing children are listed so the model does not re-split work that was
// already broken out.
func TestBuildSplitPrompt_ListsExistingChildren(t *testing.T) {
	req := &model.Requirement{ID: "req_p", Title: "大需求"}
	existing := []*model.Requirement{{ID: "req_c1", Title: "已有子需求", Status: "draft"}}
	p := BuildSplitPrompt(req, "docs", existing, "")
	if !strings.Contains(p, "已有子需求") {
		t.Error("prompt does not mention the existing children")
	}
}

// The context block is optional: a requirement without attachments, granted
// directories or references must produce EXACTLY the prompt it produced
// before the feature existed. Pin the bytes — this is the regression that
// keeps every other split stage unchanged.
func TestBuildSplitPrompt_EmptyContextIsByteIdentical(t *testing.T) {
	req := &model.Requirement{ID: "req_p", Title: "大需求", Kind: "requirement", Description: "描述"}
	want := "请把下面这份技术方案拆分成若干个**可独立交付、可独立提 PR**的子需求。\n\n" +
		"## 主需求\n" +
		"- 标题：大需求\n" +
		"- 类型：requirement\n" +
		"- 描述：描述\n" +
		"\n" +
		"## 技术方案\n\n" +
		"# 方案\n\n" +
		SplitJSONContract +
		splitContractTail
	got := BuildSplitPrompt(req, "# 方案", nil, "")
	if got != want {
		t.Errorf("the prompt changed:\ngot:\n%q\nwant:\n%q", got, want)
	}
	// A blank context block is the same as no context block.
	if BuildSplitPrompt(req, "# 方案", nil, "   \n ") != want {
		t.Error("a blank context block must not change the prompt")
	}
}

// With a context block the material lands between 「主需求」 and 「技术方案」,
// so the model reads the requirement's material BEFORE the plan it has to
// cut up.
func TestBuildSplitPrompt_InsertsTheContextBlock(t *testing.T) {
	req := &model.Requirement{ID: "req_p", Title: "大需求"}
	p := BuildSplitPrompt(req, "# 方案", nil, "## 需求上下文\n\n### 📎 已附加文件\n")
	if !strings.Contains(p, "## 需求上下文") {
		t.Fatal("the context block is missing from the prompt")
	}
	idxCtx := strings.Index(p, "## 需求上下文")
	idxMaster := strings.Index(p, "## 主需求")
	idxPlan := strings.Index(p, "## 技术方案")
	if !(idxMaster < idxCtx && idxCtx < idxPlan) {
		t.Errorf("the context block must sit between 主需求 and 技术方案 (master=%d ctx=%d plan=%d)", idxMaster, idxCtx, idxPlan)
	}
}

func TestNormalizeSplitTasks_ParsesFencedJSON(t *testing.T) {
	raw := "拆分说明文字\n\n```json\n" +
		`{"tasks":[{"title":"A","description":"d","scope":["a.go"],"acceptance":["ok"],"boundary":"b","order":1,"depends_on":[]}]}` +
		"\n```"
	tasks := NormalizeSplitTasks(raw)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].Title != "A" {
		t.Errorf("title = %q, want A", tasks[0].Title)
	}
	if len(tasks[0].Scope) != 1 || tasks[0].Scope[0] != "a.go" {
		t.Errorf("scope = %v", tasks[0].Scope)
	}
}

func TestNormalizeSplitTasks_DropsUntititledTasks(t *testing.T) {
	raw := `{"tasks":[{"title":"  "},{"title":"B"}]}`
	tasks := NormalizeSplitTasks(raw)
	if len(tasks) != 1 || tasks[0].Title != "B" {
		t.Fatalf("tasks = %v, want just the titled one", tasks)
	}
}

func TestNormalizeSplitTasks_FillsEmptyAcceptance(t *testing.T) {
	raw := `{"tasks":[{"title":"A","acceptance":[]}]}`
	tasks := NormalizeSplitTasks(raw)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if len(tasks[0].Acceptance) != 1 || tasks[0].Acceptance[0] == "" {
		t.Fatalf("acceptance = %v, want a non-empty placeholder", tasks[0].Acceptance)
	}
}

func TestNormalizeSplitTasks_CapsAtTwelve(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"tasks":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"title":"T","order":1}`)
	}
	b.WriteString(`]}`)
	tasks := NormalizeSplitTasks(b.String())
	if len(tasks) != 12 {
		t.Fatalf("got %d tasks, want the 12-task cap", len(tasks))
	}
}

func TestNormalizeSplitTasks_NonJSONReturnsEmpty(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json at all", "{ broken", `{"tasks":[]}`} {
		tasks := NormalizeSplitTasks(raw)
		if tasks == nil {
			t.Fatalf("NormalizeSplitTasks(%q) returned nil, want an empty slice", raw)
		}
		if len(tasks) != 0 {
			t.Fatalf("NormalizeSplitTasks(%q) = %v, want empty", raw, tasks)
		}
	}
}

// A forward or self reference would be a cycle — it must be dropped before
// the batch ever reaches the database.
func TestNormalizeSplitTasks_DropsForwardAndSelfDependsOn(t *testing.T) {
	raw := `{"tasks":[
		{"title":"A","order":1,"depends_on":[1]},
		{"title":"B","order":2,"depends_on":[1,3]},
		{"title":"C","order":3,"depends_on":[0,-1,2]}
	]}`
	tasks := NormalizeSplitTasks(raw)
	if len(tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(tasks))
	}
	if len(tasks[0].DependsOn) != 0 {
		t.Errorf("A depends_on = %v, want empty (self reference)", tasks[0].DependsOn)
	}
	if len(tasks[1].DependsOn) != 1 || tasks[1].DependsOn[0] != 1 {
		t.Errorf("B depends_on = %v, want [1]", tasks[1].DependsOn)
	}
	if len(tasks[2].DependsOn) != 1 || tasks[2].DependsOn[0] != 2 {
		t.Errorf("C depends_on = %v, want [2]", tasks[2].DependsOn)
	}
}

func TestNormalizeSplitTasks_ReassignsMissingOrder(t *testing.T) {
	raw := `{"tasks":[{"title":"A"},{"title":"B"}]}`
	tasks := NormalizeSplitTasks(raw)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].Order != 1 || tasks[1].Order != 2 {
		t.Errorf("orders = %d,%d want 1,2", tasks[0].Order, tasks[1].Order)
	}
}

// The ordinal space must be CONTIGUOUS — the model may hand out 1, 3, 7, but
// "depends_on: [3]" then has no single meaning: the model counts by its own
// `order`, the frontend counts by position, the database stores by position.
// Renumbering to 1..N (and translating the references along) is what keeps
// the three in agreement.
func TestNormalizeSplitOutput_RenumbersToContiguousOrdinals(t *testing.T) {
	raw := `{"tasks":[
		{"title":"C","order":7,"depends_on":[5]},
		{"title":"A","order":2,"depends_on":[]},
		{"title":"B","order":5,"depends_on":[2]}
	],"references":[{"from":5,"to":7,"reason":"B 与 C 共享同一份协议"}]}`
	out := NormalizeSplitOutput(raw)
	if len(out.Tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(out.Tasks))
	}
	wantTitles := []string{"A", "B", "C"}
	for i, task := range out.Tasks {
		if task.Title != wantTitles[i] {
			t.Errorf("task %d: title = %q, want %q", i, task.Title, wantTitles[i])
		}
		if task.Order != i+1 {
			t.Errorf("task %d: order = %d, want %d (contiguous 1..N)", i, task.Order, i+1)
		}
	}
	// B (old order 5 → new 2) depended on A (old order 2 → new 1).
	if len(out.Tasks[1].DependsOn) != 1 || out.Tasks[1].DependsOn[0] != 1 {
		t.Errorf("B depends_on = %v, want [1]", out.Tasks[1].DependsOn)
	}
	// C (old order 7 → new 3) depended on B (old order 5 → new 2).
	if len(out.Tasks[2].DependsOn) != 1 || out.Tasks[2].DependsOn[0] != 2 {
		t.Errorf("C depends_on = %v, want [2]", out.Tasks[2].DependsOn)
	}
	// The reference pair follows the same renumbering (5,7 → 2,3).
	if len(out.References) != 1 {
		t.Fatalf("references = %v, want 1", out.References)
	}
	if out.References[0].From != 2 || out.References[0].To != 3 {
		t.Errorf("reference = %d→%d, want 2→3", out.References[0].From, out.References[0].To)
	}
}

// The assembled description is what the child's execution agent reads. It
// must carry every section, and empty sections must be omitted rather than
// left as dangling headings.
func TestSplitTaskDescription_RendersAllSections(t *testing.T) {
	task := SplitTask{
		Title:       "A",
		Description: "做后端接口",
		Scope:       []string{"a.go"},
		Acceptance:  []string{"返回 200"},
		Boundary:    "A 提供 /api/a",
	}
	d := SplitTaskDescription(task)
	for _, want := range []string{"## 目标", "做后端接口", "## 涉及范围", "- a.go", "## 验收标准", "- 返回 200", "## 与其它子需求的边界", "A 提供 /api/a"} {
		if !strings.Contains(d, want) {
			t.Errorf("description is missing %q:\n%s", want, d)
		}
	}

	bare := SplitTaskDescription(SplitTask{Title: "A", Description: "只有目标"})
	if strings.Contains(bare, "## 涉及范围") || strings.Contains(bare, "## 验收标准") {
		t.Errorf("empty sections must be omitted entirely:\n%s", bare)
	}
}

func TestSplitTaskToInput(t *testing.T) {
	in := SplitTaskToInput(SplitTask{
		Title:       "A",
		Description: "做后端接口",
		Scope:       []string{"a.go"},
		Acceptance:  []string{"返回 200"},
		Boundary:    "边界",
		Order:       2,
		DependsOn:   []int{1},
	})
	if in.Title != "A" || in.Order != 2 {
		t.Errorf("input = %+v", in)
	}
	if len(in.DependsOn) != 1 || in.DependsOn[0] != 1 {
		t.Errorf("depends_on = %v", in.DependsOn)
	}
	if !strings.Contains(in.Description, "## 目标") {
		t.Errorf("description was not assembled: %q", in.Description)
	}
}

// ── 互相参考（references）──────────────────────────────────────────────────
//
// The "互相参考" layer is the model's architectural judgement: the user
// cannot tell which two candidates genuinely share context. The contract
// therefore asks for it explicitly, and the parser must not let a sloppy
// payload through (self-references, out-of-range ordinals, mirrored pairs).

// The contract must ask for the references AND say that they are the model's
// architectural judgement — otherwise the model treats them as an optional
// formality and returns an empty array.
func TestSplitJSONContract_AsksForTheArchitectsReferenceJudgement(t *testing.T) {
	c := SplitJSONContract
	mustContain := []string{
		"references",     // the field itself
		"架构师视角",      // the model must judge it, not the user
		"互相参考 ≠ 前置依赖", // references are NOT a gate
		"宁可少连",         // sparse, not an all-pairs matrix
		"reason",         // every pair carries a justification
	}
	for _, want := range mustContain {
		if !strings.Contains(c, want) {
			t.Errorf("SplitJSONContract is missing %q", want)
		}
	}
}

// The references ride along with the candidates in the same envelope.
func TestNormalizeSplitOutput_ParsesReferences(t *testing.T) {
	raw := `{
	  "tasks": [
	    {"title": "A", "description": "d", "order": 1},
	    {"title": "B", "description": "d", "order": 2}
	  ],
	  "references": [
	    {"from": 1, "to": 2, "reason": "2 的上报格式由 1 定义"}
	  ]
	}`
	out := NormalizeSplitOutput(raw)
	if len(out.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(out.Tasks))
	}
	if len(out.References) != 1 {
		t.Fatalf("references = %+v, want 1", out.References)
	}
	ref := out.References[0]
	if ref.From != 1 || ref.To != 2 {
		t.Errorf("reference = %+v, want from=1 to=2", ref)
	}
	if ref.Reason != "2 的上报格式由 1 定义" {
		t.Errorf("reason = %q", ref.Reason)
	}
}

// References are UNDIRECTED: (2,1) and (1,2) are the same pair. The parser
// normalizes to from < to and de-duplicates, keeping the first reason.
func TestNormalizeSplitOutput_ReferencesAreUndirectedAndDeduped(t *testing.T) {
	raw := `{
	  "tasks": [{"title": "A", "order": 1}, {"title": "B", "order": 2}],
	  "references": [
	    {"from": 2, "to": 1, "reason": "先出现的理由"},
	    {"from": 1, "to": 2, "reason": "后出现的理由"}
	  ]
	}`
	out := NormalizeSplitOutput(raw)
	if len(out.References) != 1 {
		t.Fatalf("references = %+v, want exactly 1 (mirrored pairs collapse)", out.References)
	}
	if out.References[0].From != 1 || out.References[0].To != 2 {
		t.Errorf("reference = %+v, want from=1 to=2", out.References[0])
	}
	if out.References[0].Reason != "先出现的理由" {
		t.Errorf("reason = %q, want the first reason to win", out.References[0].Reason)
	}
}

// A self-reference or an out-of-range ordinal is nonsense — drop it rather
// than handing the UI a pair that points at a candidate that does not exist.
func TestNormalizeSplitOutput_DropsInvalidReferences(t *testing.T) {
	raw := `{
	  "tasks": [{"title": "A", "order": 1}, {"title": "B", "order": 2}],
	  "references": [
	    {"from": 1, "to": 1, "reason": "自引用"},
	    {"from": 0, "to": 2, "reason": "序号从 0 开始"},
	    {"from": 1, "to": 5, "reason": "越界"},
	    {"from": 1, "to": 2, "reason": "有效"}
	  ]
	}`
	out := NormalizeSplitOutput(raw)
	if len(out.References) != 1 {
		t.Fatalf("references = %+v, want only the valid one", out.References)
	}
	if out.References[0].From != 1 || out.References[0].To != 2 {
		t.Errorf("reference = %+v, want the valid pair", out.References[0])
	}
}

// The count is capped — a model that connects everything would drown each
// candidate's context in irrelevant material.
func TestNormalizeSplitOutput_CapsTheReferenceCount(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"tasks": [`)
	for i := 1; i <= 6; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		b.WriteString(`{"title": "T`)
		b.WriteString(strings.Repeat("x", i))
		b.WriteString(`", "order": `)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`}`)
	}
	b.WriteString(`], "references": [`)
	for i := 1; i <= 6; i++ {
		for j := i + 1; j <= 6; j++ {
			if i > 1 || j > 2 {
				b.WriteString(",")
			}
			b.WriteString(`{"from": `)
			b.WriteString(strconv.Itoa(i))
			b.WriteString(`, "to": `)
			b.WriteString(strconv.Itoa(j))
			b.WriteString(`}`)
		}
	}
	b.WriteString(`]}`)

	out := NormalizeSplitOutput(b.String())
	if len(out.Tasks) != 6 {
		t.Fatalf("tasks = %d, want 6", len(out.Tasks))
	}
	// 6 candidates have 15 pairs; the cap trims it down.
	if len(out.References) > splitMaxReferences {
		t.Errorf("references = %d, want at most %d", len(out.References), splitMaxReferences)
	}
	if len(out.References) == 0 {
		t.Errorf("references is empty — the valid pairs should survive")
	}
}

// NormalizeSplitTasks stays a thin wrapper over the full parse, so the two
// entry points can never disagree about the task normalization.
func TestNormalizeSplitTasks_AgreesWithNormalizeSplitOutput(t *testing.T) {
	raw := `{"tasks": [{"title": "A", "order": 1}, {"title": "B", "order": 2}]}`
	tasks := NormalizeSplitTasks(raw)
	out := NormalizeSplitOutput(raw)
	if len(tasks) != len(out.Tasks) {
		t.Fatalf("NormalizeSplitTasks = %d tasks, NormalizeSplitOutput = %d", len(tasks), len(out.Tasks))
	}
	for i := range tasks {
		if tasks[i].Title != out.Tasks[i].Title || tasks[i].Order != out.Tasks[i].Order {
			t.Errorf("task %d differs: %+v vs %+v", i, tasks[i], out.Tasks[i])
		}
	}
}
