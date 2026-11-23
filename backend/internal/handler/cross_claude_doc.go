package handler

import (
	"log"
	"strings"

	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
)

// 跨项目作业的「项目约定」收集。
//
// 跨项目作业与单项目阶段有两处本质差别：
//
//  1. 工作目录是一个**临时目录**（不属于任何仓库），参与仓库只以只读
//     --add-dir 授权。CLI 的项目记忆通道只从**工作目录**读 CLAUDE.md —— 所以
//     单项目阶段靠原生通道送达的约定，在跨项目作业里根本到不了模型面前。
//  2. 跨项目方案要按仓库分工，模型必须知道**哪份约定约束哪一部分设计**。
//     原生通道一次只带一份受管文档（llm.StreamOpts.ClaudeMdDir）且不标注归属，
//     表达不了「N 个仓库各一份约定」。
//
// 所以约定随提示词一起送达，每个参与仓库一节、标题带仓库名。
//
// 来源优先级与全仓库一致（见 claude_doc_injection.go）：NovaWorkbench 维护的
// **受管副本**优先；项目没有受管副本时，仓库目录里自带的 CLAUDE.md 是唯一的
// 约定来源（单项目阶段由 CLI 从 checkout 直接加载，跨项目阶段工作目录是临时
// 目录，CLI 加载不到，只能由我们带过去）。
//
// 不会重复：CLI 不会自行加载 --add-dir 仓库里的 CLAUDE.md（那需要
// CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD 环境变量，只有原生通道才会设
// 置），所以上下文里只有这一份约定。

// crossProjectDocSource — 收集约定时用的仓库引用。
type crossProjectDocSource struct {
	// ProjectID — 项目 id（受管副本的键）。
	ProjectID string
	// ProjectName — 项目显示名（约定块的归属标签）。
	ProjectName string
	// RepoPath — 仓库在磁盘上的路径（仓库自带 CLAUDE.md 的位置）。
	RepoPath string
}

// crossProjectDoc 收集一个仓库的项目约定。
//
// 返回 ok=false 表示这个仓库没有约定（受管副本与仓库自带文件都没有）。
func (h *CrossRequirementHandler) crossProjectDoc(src crossProjectDocSource) (prompt.CrossProjectDoc, bool) {
	doc := prompt.CrossProjectDoc{ProjectName: src.ProjectName}
	if h.projectSvc == nil || strings.TrimSpace(src.ProjectID) == "" {
		return doc, false
	}
	if managed, err := h.projectSvc.ClaudeDoc(src.ProjectID); err != nil {
		log.Printf("[cross-requirement] load managed claude doc for project %s: %v", src.ProjectID, err)
	} else if managed != nil && strings.TrimSpace(managed.Content) != "" {
		doc.Content = managed.Content
		doc.Managed = true
		// 仓库自带的副本仍在磁盘上：受管副本是权威版本，冲突时以它为准。
		doc.DiskExists = service.DiskClaudeDocExists(src.RepoPath)
		return doc, true
	}
	// 没有受管副本：仓库自带的 CLAUDE.md 就是约定来源。
	if content, exists, err := h.projectSvc.ReadDiskClaudeDoc(src.RepoPath); err != nil {
		log.Printf("[cross-requirement] read disk claude doc for project %s: %v", src.ProjectID, err)
	} else if exists && strings.TrimSpace(content) != "" {
		doc.Content = content
		return doc, true
	}
	return doc, false
}

// crossProjectDocs 收集一组仓库的项目约定（按给定顺序）。
func (h *CrossRequirementHandler) crossProjectDocs(srcs []crossProjectDocSource) []prompt.CrossProjectDoc {
	out := make([]prompt.CrossProjectDoc, 0, len(srcs))
	for _, src := range srcs {
		if doc, ok := h.crossProjectDoc(src); ok {
			out = append(out, doc)
		}
	}
	return out
}

// crossConventions collects the project-level CLAUDE.md of every repository
// participating in a cross requirement.
//
// The whole requirement's participant set — the design and the split both
// span all of them.
func (h *CrossRequirementHandler) crossConventions(xreqID string) []prompt.CrossProjectDoc {
	projects, err := h.svc.ListProjects(xreqID)
	if err != nil {
		log.Printf("[cross-requirement] list projects of %s: %v", xreqID, err)
		return nil
	}
	srcs := make([]crossProjectDocSource, 0, len(projects))
	for _, p := range projects {
		src := crossProjectDocSource{ProjectID: p.ProjectID, ProjectName: p.ProjectName}
		if proj, pErr := h.projectSvc.Get(p.ProjectID); pErr == nil && proj != nil {
			src.RepoPath = proj.LocalPath
		}
		srcs = append(srcs, src)
	}
	return h.crossProjectDocs(srcs)
}

// crossPhaseConventions collects the project-level CLAUDE.md of the
// repositories a phase's deliverables live in.
//
// The phase's members — not the whole requirement: a phase verifies one
// slice of the work, and handing it the conventions of repositories it does
// not touch invites the model to judge them by the wrong rules.
func (h *CrossRequirementHandler) crossPhaseConventions(members []prompt.CrossPhaseMember) []prompt.CrossProjectDoc {
	srcs := make([]crossProjectDocSource, 0, len(members))
	seen := map[string]bool{}
	for _, m := range members {
		if seen[m.ProjectID] {
			continue
		}
		seen[m.ProjectID] = true
		srcs = append(srcs, crossProjectDocSource{
			ProjectID:   m.ProjectID,
			ProjectName: m.ProjectName,
			RepoPath:    m.ProjectPath,
		})
	}
	return h.crossProjectDocs(srcs)
}
