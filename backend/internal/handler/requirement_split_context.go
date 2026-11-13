package handler

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/prompt"
)

// 「方案拆分」的上下文装配与授权继承。
//
// 这一文件是拆分阶段与「方案设计」阶段之间那块缺失的胶水：设计阶段早就把
// 附件清单、授权目录清单、关联需求上下文喂给了模型，并且清单与 `--add-dir`
// 授权**同源**（handler/attachment_grant.go 里写死的不变量：清单指向哪个
// 目录，授权就必须包含哪个目录，否则非交互运行里没人能批准那次读取，附件
// 就是「看不见」的）。拆分阶段此前完全没有这一套，于是 AI 连附件的文件名都
// 看不到，只能凭空发明接口。
//
// 装配集中在这里（一处），而不是散落在本机 / 远端两条执行路径里 —— 两条
// 路径必须看到**同一份**上下文块、**同一份**授权清单。

// splitRuntime 承载拆分这一轮的执行期素材。
//
// 单独抽成结构体而不是继续给已经很长的参数列表加位置参数：拆分这条链路
// 上已经挂着 prompt / systemPrompt / model / configID / usage 五六个参数，
// 再加三个只会让调用点更难读。
type splitRuntime struct {
	// AttachDir — 本次执行环境下的附件目录。本机执行 = 本地附件根目录；
	// 远端执行 = 同步到 Agent 主机后的镜像目录（resolveAttachDirFor 会
	// 先同步再返回远端路径）。清单与授权都由它派生，两边不会漂移。
	AttachDir string
	// AddDirs — 交给 claude CLI 的 `--add-dir` 授权清单：用户手工授权的
	// 目录 + 本需求的附件目录。
	AddDirs []string
	// ContextBlock — 已渲染的「## 需求上下文」块。本机 CLI 路径与 HTTP
	// 降级通道共用同一块。
	ContextBlock string
}

// buildSplitContext 装配拆分这一轮的上下文块与授权。
//
// agentServerID 非空表示这次跑在 Agent Server 上：附件会先同步到那台机器，
// 清单里的路径随之变成远端路径，用户手工授权的目录则仍按本机记录的绝对
// 路径下发（CLI 会忽略解析不到的 --add-dir，不会让运行失败）。
//
// 降级而非失败是这里的一贯原则：附件同步失败、引用查询失败都只影响「模型
// 能看到多少素材」，绝不让一次拆分直接报错 —— 拆分的价值在方案本身。
func (h *WizardHandler) buildSplitContext(ctx context.Context, reqID, agentServerID string) splitRuntime {
	run := splitRuntime{}
	agentServerID = strings.TrimSpace(agentServerID)
	remote := agentServerID != ""

	// The attachment directory comes first and is used for BOTH the manifest
	// and the --add-dir grant. If the two were derived separately the
	// manifest could point at a directory the run cannot read — the exact
	// failure the attachment_grant.go invariant exists to prevent.
	attachDir := h.resolveAttachDir(ctx, reqID, agentServerID)
	run.AttachDir = attachDir

	manifest := h.attachManifestFor(reqID, attachDir)
	// The split disallows Bash, so the manifest must not suggest `ls`.
	dirs := dirsManifestWith(h.dirSvc, reqID, false)
	refs := h.resolveSplitReferences(reqID)

	run.ContextBlock = prompt.BuildSplitContextBlock(prompt.SplitContext{
		AttachManifest: manifest,
		DirsManifest:   dirs,
		Referenced:     refs,
		Remote:         remote,
	})

	run.AddDirs = h.grantDirsFor(reqID, attachDir)
	return run
}

// resolveSplitReferences resolves the requirement's referenced requirements
// (description + existing plan) for the split prompt.
//
// A failure degrades to "no references" rather than blocking the split — the
// referenced requirements enrich the split, they are not a precondition for
// it.
func (h *WizardHandler) resolveSplitReferences(reqID string) []model.RequirementReferenceContent {
	if h.reqSvc == nil || strings.TrimSpace(reqID) == "" {
		return nil
	}
	refs, err := h.reqSvc.ResolveReferenceContent(reqID)
	if err != nil {
		log.Printf("[split] %s: reference lookup failed: %v — continuing without the referenced requirements", reqID, err)
		return nil
	}
	return refs
}

// splitInheritedDirAddedBy — 复制给子需求的目录在 requirement_dirs.added_by
// 上记这个值，便于日后区分「用户自己授权的」与「从主需求继承来的」。
const splitInheritedDirAddedBy = "inherited"

// inheritSplitDirs copies the master's granted directories onto each child.
//
// Why copy rather than resolve at runtime: requirement_dirs is keyed by
// requirement, so a child starts with an empty set. The master's attachment
// directory is already granted to the child by the design stage (the
// parentGrant in wizard_architect.go), but the directories the USER
// authorized were not — the grant simply disappeared the moment development
// was delegated to the children.
//
// Why copy rather than a runtime lookup up the parent chain: a runtime lookup
// would have to change resolveAddDirs, which feeds six existing stages
// (analysis / design / coding / Q&A / docs / sub-tasks). A copy touches
// exactly one table and one call site, and the child shows the inherited
// rows in its own 「已授权目录」 panel where the user can revoke them one by
// one.
//
// The copies are snapshots: revoking a grant on the master does NOT remove
// the copies on the children. That is deliberate — the children are the
// primary fact, and a child that silently lost its read access mid-flight
// would fail for a reason the user never sees.
func (h *WizardHandler) inheritSplitDirs(parentID string, children []*model.Requirement) error {
	if h.dirSvc == nil || strings.TrimSpace(parentID) == "" || len(children) == 0 {
		return nil
	}
	dirs, err := h.dirSvc.List(parentID)
	if err != nil {
		return fmt.Errorf("list master directories: %w", err)
	}
	if len(dirs) == 0 {
		return nil
	}
	for _, c := range children {
		if c == nil || strings.TrimSpace(c.ID) == "" {
			continue
		}
		// The child's own grants win: a path the child already holds keeps
		// the child's label (Add would otherwise overwrite it with the
		// master's). Children are created fresh by the split, so this is
		// normally empty — the guard is for the repeated-confirm case.
		existing := map[string]struct{}{}
		if own, lerr := h.dirSvc.List(c.ID); lerr == nil {
			for _, d := range own {
				existing[d.Path] = struct{}{}
			}
		}
		for _, d := range dirs {
			if _, dup := existing[d.Path]; dup {
				continue
			}
			// Add is idempotent: granting the same (requirement, path) twice
			// returns the existing row, so re-confirming a split never
			// duplicates a grant.
			if _, aerr := h.dirSvc.Add(c.ID, d.Path, d.Label, splitInheritedDirAddedBy); aerr != nil {
				return fmt.Errorf("inherit directory %s onto %s: %w", d.Path, c.ID, aerr)
			}
		}
	}
	return nil
}
