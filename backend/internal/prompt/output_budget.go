package prompt

// OutputBudgetClause is the "keep a single response small" block appended to
// every prompt that drives the claude child to WRITE files (the coding
// stages, the direct-agent path, the sub-task executor).
//
// Why it exists: the claude CLI aborts a turn whose reply exceeds the model's
// per-response output limit (the CLI reports "Claude's response exceeded the
// 128000 output token maximum"). The usual trigger is the model trying to
// emit one huge file in a single Write / Edit call — the reply is truncated
// mid-file and the whole turn dies, losing everything after it.
//
// This block is the PREVENTION layer. The RECOVERY layer (recognising the
// failure, labelling it, resuming the session) lives in the handler package;
// the two are complementary: this clause makes the failure less likely, the
// recovery makes it survivable.
//
// It is deliberately a plain constant rather than a per-stage knob: the
// constraint is correct for every code-writing prompt, and a single wording
// keeps the instruction the model sees consistent across entry points.
//
// Appended text only — every existing prompt keeps its current wording and
// gains this block at the end.
const OutputBudgetClause = `## 输出体积约束（强制）

模型单次回复有输出 token 上限，超过会被直接截断，整轮任务失败。请遵守：

- 单次 Write / Edit 的新增内容控制在 300 行以内。
- 大文件必须分段写入：先写文件骨架（结构 + 函数签名 + 关键注释），再用 Edit 逐段追加具体实现。
- 优先用 Edit 做小步改动，避免一次性重写整个文件。
- 不要在回复正文里粘贴文件全文或大段代码，只说明改了什么、改在哪个文件。
- 如果上一轮回复被截断，从工作区当前状态继续，不要重复已经完成的部分。
`
