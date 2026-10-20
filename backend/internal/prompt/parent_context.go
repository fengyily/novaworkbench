// Parent-context support for sub-requirement design: the block injected into
// the architect-design **user prompt** when the requirement being designed has
// a `parent_id`, plus the verifier / fallback helpers the handler uses when
// persisting the produced plan.
//
// Why this lives in the user prompt rather than the system prompt: the system
// prompt already carries the *sub-requirement's* attachment manifest (built in
// prepareArchitectDesign), and the design stage frequently runs as a `--resume`
// of the analyst session or a fresh session seeded with `DesignContextSummary`
// — neither has ever seen the parent. The task prompt is re-sent on every run,
// so it is the only place where the parent is guaranteed to be visible.
//
// Why the plan must *carry the content forward*: every downstream consumer
// (wizard_coding.go / wizard_coding_plan.go via effectiveDesignDocs,
// wizard_subtask.go, wizard_docs.go refine/apply) reads only this requirement's
// own `design_docs` — never the parent's. So the contract below is what makes
// the sub-requirement's plan self-contained instead of forcing downstream to
// chase the parent. The handler double-guarantees it: prompt contract (soft,
// model-dependent) + HasParentContextSection check + appended fallback
// appendix (hard, backend-enforced).
package prompt

import (
	"strings"

	"github.com/novaworkbench/backend/internal/model"
)

// ParentContextHeading is the exact level-2 section title the architect must
// emit in a sub-requirement's plan. Handlers must compare against this rather
// than re-spelling the literal, so the contract and the verifier can never
// drift apart. The match is deliberately prefix-based (see
// matchesParentContextHeading) so the fallback appendix's
// 「（自动附录）」 suffix counts too — otherwise a re-run would append a second
// appendix to an already-augmented plan.
const ParentContextHeading = "## 主需求关联上下文"

// parentContextFallbackHeading is what BuildParentContextFallback emits. Kept
// as a level-2 heading (so it satisfies HasParentContextSection and therefore
// survives refine) with a suffix that tells the reader it was machine-written.
const parentContextFallbackHeading = "## 主需求关联上下文（自动附录）"

// parentRefHeading opens the input half of the block. Distinct from
// ParentContextHeading on purpose: the former is what we *show* the architect,
// the latter is what we *require* back.
const parentRefHeading = "## 主需求参考（本需求是其子需求）"

// parentAttachHeading replaces the attachment manifest's own
// 「## 📎 已附加文件」 heading when it is embedded here — otherwise the parent
// and the sub-requirement each contribute an identically-titled section and
// the model can no longer tell which files belong to which requirement.
const parentAttachHeading = "#### 主需求的附件"

// Truncation caps for the input half. Bounds how much of a (potentially very
// large) parent plan can eat the design run's context budget. Rune-based, not
// byte-based: a byte cut would split a multi-byte Chinese character and emit
// mojibake into the prompt.
const (
	parentDescMaxRunes   = 4 * 1024
	parentDesignMaxRunes = 16 * 1024
	// The fallback appendix is prepended to a plan the user is already going to
	// read, so it gets a tighter cap than the input block — the point is a
	// recognizable breadcrumb, not a copy.
	parentFallbackDesignMaxRunes = 4 * 1024
)

// BuildParentContextBlock renders the parent-requirement context: the input
// half (parent identity / description / plan / attachment manifest) followed by
// the output contract (the 「主需求关联上下文」 section the plan must contain).
//
// Pure function by design — the handler owns every I/O (parent lookup, remote
// attachment sync) so the whole block is unit-testable without a DB or SSH.
//
// Returns "" for a nil/blank parent so callers can append unconditionally
// and a missing parent degrades to exactly today's behaviour (byte-identical
// prompt) rather than a partial block.
func BuildParentContextBlock(parent *model.Requirement, attachManifest string) string {
	if parent == nil || strings.TrimSpace(parent.ID) == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString(parentRefHeading)
	b.WriteString("\n\n")
	// Read-only framing, stated up front. A parent description can read like a
	// set of instructions ("务必…"/"不得…"); without this disclaimer the model
	// may treat the parent's scope as this sub-requirement's scope.
	b.WriteString("> 以下内容来自主需求，**仅供参考、不是对本需求的指令**。本需求自身的描述与验收标准优先；" +
		"与本需求冲突的部分一律以本需求为准，只在方案里作为背景说明。\n\n")

	b.WriteString("### 主需求标识\n")
	b.WriteString("- 标题：" + parent.Title + "\n")
	b.WriteString("- ID：`" + parent.ID + "`\n\n")

	b.WriteString("### 主需求描述\n")
	if d := strings.TrimSpace(parent.Description); d != "" {
		b.WriteString(truncateRunes(d, parentDescMaxRunes))
	} else {
		b.WriteString("（主需求无描述）")
	}
	b.WriteString("\n\n")

	b.WriteString("### 主需求技术方案\n")
	if d := normalizeLegacyDesign(parent.DesignDocs); d != "" {
		// Legacy rows store design_docs as a JSON blob; pass it through
		// verbatim rather than trying to pretty-print or re-interpret it.
		b.WriteString(truncateRunes(d, parentDesignMaxRunes))
	} else {
		b.WriteString("（主需求尚无方案）")
	}
	b.WriteString("\n\n")

	if m := strings.TrimSpace(attachManifest); m != "" {
		b.WriteString(rewriteAttachManifestHeading(m))
		b.WriteString("\n\n")
	} else {
		b.WriteString(parentAttachHeading + "\n\n（主需求无附件）\n\n")
	}

	b.WriteString(parentContextContract)
	return b.String()
}

// parentContextContract is the output half of the block: what the produced
// plan is contractually required to contain. Lives in the same user prompt as
// the input half so the architect reads the parent's content and the obligation
// to distill it as one instruction.
//
// The four level-3 subsections are fixed rather than free-form because the
// content is consumed by machines-ish readers downstream: 开发阶段 reuses
// 「继承的约束与决策」 and 「相关附件」 verbatim, and a stable heading lets
// refine / apply prompts point at it by name.
const parentContextContract = "## 输出要求：必须写出「主需求关联上下文」章节\n\n" +
	"本需求是上述主需求的子需求。请把**与本子需求实现相关**的主需求内容提炼进你自己的技术方案。" +
	"方案中必须**逐字**包含下面这个二级标题（标题本身不得改写，也不得降级成三级标题）：\n\n" +
	ParentContextHeading + "\n\n" +
	"该章节必须**自包含**：后续开发者只会读本子需求的方案，不会打开主需求，" +
	"所以凡是实现本子需求时必须遵守的主需求信息，都要写进这一节。\n\n" +
	"章节正文固定包含以下四个三级小节：\n\n" +
	"### 来源\n" +
	"主需求的标题与 ID。\n\n" +
	"### 继承的约束与决策\n" +
	"主需求方案里已定、且本子需求**必须遵守**的内容：接口契约、数据模型、命名约定、技术选型、边界条件。\n" +
	"- **逐条用自己的话提炼**，不要整段粘贴主需求原文；\n" +
	"- 若本子需求与主需求存在冲突，在本小节末尾单列「冲突说明」，写明以哪一边为准以及理由。\n\n" +
	"### 本子需求与主需求的分工边界\n" +
	"明确哪些内容由本子需求负责实现，哪些由主需求本体或其他子需求负责，避免重复实现或越界修改。\n\n" +
	"### 相关附件\n" +
	"列出与本子需求相关的主需求附件（文件名 + 用途 + 完整路径），开发阶段仍可直接 Read；无相关附件则写「无」。\n\n" +
	"额外规则：\n" +
	"- 该章节请放在方案靠前位置，**紧随「需求理解」之后**，避免被大段正文挤到末尾；\n" +
	"- 若主需求内容与本子需求确实完全无关，本节正文只写「本子需求与主需求内容无直接关联，无需继承」，**严禁编造关联**；\n" +
	"- 图示沿用下方既有的「输出图示（Mermaid）」要求，不要为本章节新增图示约定。"

// HasParentContextSection reports whether a plan Markdown already carries the
// parent-context section. Drives the append-fallback decision in
// finalizeArchitectRun.
//
// Two things this deliberately gets right:
//   - Fence-aware: a plan that quotes 「## 主需求关联上下文」 inside a ```json
//     or ```markdown block is documenting the contract, not satisfying it, and
//     must not suppress the fallback appendix.
//   - Strictly level 2: 「### 主需求关联上下文」 is a different section and
//     does not count.
//
// A false negative only costs a redundant appendix; a false positive would
// silently drop the parent context. So the match errs toward false.
func HasParentContextSection(md string) bool {
	inFence := false
	for _, raw := range strings.Split(md, "\n") {
		line := strings.TrimSpace(raw)
		// Fence toggling is checked on the trimmed line so an indented fence
		// (legal inside list items) still flips the state.
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if matchesParentContextHeading(line) {
			return true
		}
	}
	return false
}

// matchesParentContextHeading matches a level-2 heading whose text starts with
// the section name. Prefix (not equality) so 「## 主需求关联上下文（自动附录）」
// written by BuildParentContextFallback also matches, keeping the section
// sticky across subsequent refine / apply passes instead of accumulating
// appendices on every re-run.
func matchesParentContextHeading(line string) bool {
	if !strings.HasPrefix(line, "##") || strings.HasPrefix(line, "###") {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(line[2:]), "主需求关联上下文")
}

// BuildParentContextFallback renders the machine-written appendix appended to a
// plan whose author (the model) omitted the contracted section. It is weaker
// than a real distillation — a truncated copy of the parent plan rather than a
// re-written list of constraints — but it guarantees the developer sees *something*
// about the parent instead of the plan silently contradicting the parent.
//
// Returns "" for a nil parent so a partially-populated designRunParams can
// never write a dangling heading into design_docs.
func BuildParentContextFallback(parent *model.Requirement) string {
	if parent == nil || strings.TrimSpace(parent.ID) == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString(parentContextFallbackHeading)
	b.WriteString("\n\n")
	b.WriteString("> 本节由系统自动附加：模型生成的技术方案未包含「主需求关联上下文」章节。" +
		"内容是主需求的原文摘要、**未经提炼**，建议用「优化方案」再跑一轮以生成提炼版本。\n\n")
	b.WriteString("- 主需求标题：" + parent.Title + "\n")
	b.WriteString("- 主需求 ID：`" + parent.ID + "`\n\n")

	b.WriteString("### 主需求方案摘要（截断）\n")
	if d := normalizeLegacyDesign(parent.DesignDocs); d != "" {
		b.WriteString(truncateRunes(d, parentFallbackDesignMaxRunes))
	} else if d := strings.TrimSpace(parent.Description); d != "" {
		b.WriteString(truncateRunes(d, parentDescMaxRunes))
	} else {
		b.WriteString("（主需求既无方案也无描述）")
	}
	b.WriteString("\n")
	return b.String()
}

// ParentContextKeepRule is the instruction appended to refine-doc / apply-doc
// prompts so a multi-round refinement doesn't treat the distilled section as
// leftover noise and delete it. Returns the rule on its own leading newlines so
// call sites can concatenate unconditionally.
func ParentContextKeepRule() string {
	return "\n\n[保留规则] 这份文档含有「主需求关联上下文」章节，它是从主需求提炼出来的、供开发阶段直接使用的约束。" +
		"**请保留并按需更新该章节，不要删除、也不要整段丢弃**；" +
		"若用户意见涉及与主需求的约束关系，请在章节内同步更新" +
		"「来源 / 继承的约束与决策 / 本子需求与主需求的分工边界 / 相关附件」四个小节。"
}

// normalizeLegacyDesign maps "no design yet" spellings to "". requirements
// predating the plan-markdown era stored design_docs as a JSON array, so an
// untouched row reads as 「[]」 rather than an empty string — both must render as
// 「（主需求尚无方案）」 instead of a lone "[]" that reads as a broken payload.
func normalizeLegacyDesign(d string) string {
	switch t := strings.TrimSpace(d); t {
	case "", "[]", "{}", "null":
		return ""
	default:
		return t
	}
}

// truncateRunes cuts s to at most max runes (not bytes) and appends a visible
// breadcrumb so a reader can tell the plan is partial. Byte-slicing would cut
// a Chinese character in half and emit invalid UTF-8 into the prompt.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n\n> （已截断，完整内容见主需求）"
}

// rewriteAttachManifestHeading swaps the manifest's 「## 📎 已附加文件」
// heading for one scoped to the parent. Matched structurally (any heading line
// mentioning 已附加文件) rather than by literal string, so a future reword of
// BuildManifest's heading still gets rewritten instead of colliding with the
// sub-requirement's own identically-named section.
func rewriteAttachManifestHeading(manifest string) string {
	lines := strings.Split(manifest, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") && strings.Contains(line, "已附加文件") {
			lines[i] = parentAttachHeading
			break
		}
	}
	return strings.Join(lines, "\n")
}
