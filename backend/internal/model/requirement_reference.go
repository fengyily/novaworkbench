package model

// 需求之间的「引用」关系（区别于 parent_id 的主/子层级与
// source_requirement_id 的转需求溯源）。
//
// 用户在新建 / 修改需求时可以勾选若干「关联需求」，方案设计时这些需求的
// **描述 + 既有方案** 会作为上下文注入到架构师的提示词里，并在最终方案中
// 固化成「关联需求上下文」章节。
//
// 三条语义约定贯穿本文件：
//
//  1. **注入内容只含描述 + 方案**。标题 / 项目 / 状态 / 类型 / 附件清单一律
//     不进提示词 —— 那是给人看的标签，描述已经承载完整语义。
//  2. **Origin 区分来源**。direct = 用户在本需求上直接勾选；parent = 从
//     主需求继承而来（子需求自动带上主需求的引用集合）。
//  3. **Title 等展示字段只服务前端**，不会出现在写给模型的提示词里。

// RequirementReferenceInput 是客户端提交的一条引用（写入 JSON 列的形状）。
type RequirementReferenceInput struct {
	// RequirementID 是「被引用需求」的 id（req_*），不是引用方的 id。
	RequirementID string `json:"requirement_id"`
	// Note 是用户写的「引用原因」，随内容一起注入方案提示词，用于提供相关性。
	Note string `json:"note"`
}

// RequirementReference 是解析后的引用视图（API 返回形状，不含正文）。
//
// 注意：Title / Status / Kind / ProjectID / ProjectName 只服务于**前端展示**
// （选择器候选列表、详情页 chip 行），**不会被注入提示词**。
type RequirementReference struct {
	RequirementID string `json:"requirement_id"` // 被引用需求的 id
	Note          string `json:"note"`           // 引用原因
	Origin        string `json:"origin"`         // "direct" 直接引用 | "parent" 继承自主需求
	Title         string `json:"title"`          // 仅前端展示，不进提示词
	Status        string `json:"status"`         // 仅前端展示，不进提示词
	Kind          string `json:"kind"`           // 仅前端展示，不进提示词
	ProjectID     string `json:"project_id"`     // 仅前端展示，不进提示词
	ProjectName   string `json:"project_name"`   // 仅前端展示，不进提示词
	HasDesign     bool   `json:"has_design"`     // 被引用需求是否已有方案（前端提示用）
}

// RequirementReferenceContent 在视图基础上补正文与方案，仅用于提示词注入，
// 不出浏览器。
//
// 注入时**只用 Description + DesignDocs**（外加序号 / ID / Note / Origin 作
// 锚点），Title 不参与注入。
type RequirementReferenceContent struct {
	RequirementReference
	Description string
	DesignDocs  string
}

// 引用来源常量。Origin 字段的取值集合。
const (
	// ReferenceOriginDirect — 用户在本需求上直接勾选的引用。
	ReferenceOriginDirect = "direct"
	// ReferenceOriginParent — 从主需求继承而来的引用。
	ReferenceOriginParent = "parent"
)
