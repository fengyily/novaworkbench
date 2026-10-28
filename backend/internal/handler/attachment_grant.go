package handler

import (
	"log"
	"path/filepath"
	"strings"

	"github.com/novaworkbench/backend/internal/service"
)

// attachmentGrant 描述一个要交给 claude CLI 的附件目录。
//
// ownerID 是附件所属的需求 id（req_*）。dir 是「Claude 实际看到的那个目录」：
//
//   - ""     → 本地 NovaWorkbench 附件根目录
//              (filepath.Join(service.AttachmentRoot(), ownerID))。
//              这与 attachManifestFor 的空值回退**完全同义**，两处口径必须一致。
//   - 非空   → 显式目录（例如同步到 Agent Server 上的镜像目录）。
//
// 不变量：凡是出现在交给 Claude 的附件清单里的目录，都必须同时出现在 --add-dir
// 列表里。否则清单指向会话工作目录之外的路径，plan 模式（非交互 -p 运行）没有人
// 能批准这次读取，附件就是「看不见」的 —— 这正是本文件存在的原因。
type attachmentGrant struct {
	ownerID string
	dir     string
}

// localAttachmentGrant 返回「本地存储目录」这一档的授权。dir 留空，由合成器回退
// 到 service.AttachmentRoot()/ownerID —— 不要在调用点自己拼路径，两处口径会漂移。
func localAttachmentGrant(ownerID string) attachmentGrant {
	return attachmentGrant{ownerID: ownerID, dir: ""}
}

// attachGrantDirs 把用户手工授权的目录与附件目录合并成一份 --add-dir 列表。
//
//   - userDirs 原样保留、顺序不变（用户授权在前）。
//   - 每个 grant：attachSvc 为 nil / ownerID 为空 → 跳过；attachSvc.List(ownerID)
//     为空（该 owner 确实没有附件）→ 跳过；否则把 dir 追加进来（dir 为空则回退到
//     filepath.Join(service.AttachmentRoot(), ownerID)）。
//   - 全程去重：用户手工授权过同一个路径时不重复出现。
//
// 只授权**单个 owner 的目录**，绝不授权 service.AttachmentRoot() 整个根目录 ——
// 避免跨需求越权，也避免 Claude 的 Glob/Grep 搜索面失控。
//
// 附件查找失败（老库缺表等）只记日志并跳过该 grant，绝不阻塞本次运行：附件是
// 加分项，不是执行前置条件。
//
// 结果为空时返回 nil（不是空切片），保持与「用户没有授权任何目录」时 AddDirs == nil
// 的既有行为一致。
func attachGrantDirs(userDirs []string, attachSvc *service.AttachmentService, grants ...attachmentGrant) []string {
	out := make([]string, 0, len(userDirs)+len(grants))
	seen := make(map[string]bool, len(userDirs)+len(grants))
	for _, d := range userDirs {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	for _, g := range grants {
		owner := strings.TrimSpace(g.ownerID)
		if owner == "" || attachSvc == nil {
			continue
		}
		atts, err := attachSvc.List(owner)
		if err != nil {
			log.Printf("[attachments] grant lookup for %s failed: %v — skipping its attachment dir", owner, err)
			continue
		}
		if len(atts) == 0 {
			continue
		}
		dir := strings.TrimSpace(g.dir)
		if dir == "" {
			dir = filepath.Join(service.AttachmentRoot(), owner)
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
