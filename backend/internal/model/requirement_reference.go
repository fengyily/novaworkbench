package model

import (
	"encoding/json"
	"fmt"
)

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
//     主需求继承而来（子需求自动带上主需求的引用集合）；reverse = 反向边
//     —— 对方引用了本需求（引用是无向的，两侧必须互相可见）。
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
	Origin        string `json:"origin"`         // "direct" 直接引用 | "parent" 继承自主需求 | "reverse" 被对方引用
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
	// ReferenceOriginReverse — 反向边：对方引用了本需求。
	//
	// 引用在语义上是**无向**的（「互相参考」），但存储只有出边 —— 只写一侧
	// 会让「被引用方」在方案设计时看不到引用方的方案。读路径按无向语义补齐
	// 入边，用这个 origin 标记，以便前端把这类 chip 渲染成只读。
	ReferenceOriginReverse = "reverse"
)

// SplitReferencePair 是「拆分确认」请求体里的一条互相参考关系。
//
// 两个序号都是**批次内序号**（1 起算），不是数据库 id —— 落库时才知道真实
// id。Reason 是模型以架构师视角给出的判断依据。
//
// UnmarshalJSON 同时接受两种形态：
//
//	[1, 2]                                —— 旧形态，只有序号对
//	{"from": 1, "to": 2, "reason": "…"}  —— 新形态，带判断依据
//
// 旧形态必须继续可解析：前端标签页可能停留在拆分预览上跨过一次部署。
type SplitReferencePair struct {
	From   int    `json:"from"`
	To     int    `json:"to"`
	Reason string `json:"reason"`
}

// UnmarshalJSON accepts both the legacy "[from, to]" array form and the
// object form that carries the model's reason.
func (p *SplitReferencePair) UnmarshalJSON(data []byte) error {
	// Object form first: {"from":1,"to":2,"reason":"…"}.
	var obj struct {
		From   int    `json:"from"`
		To     int    `json:"to"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		p.From, p.To, p.Reason = obj.From, obj.To, obj.Reason
		return nil
	}
	// Legacy array form: [1, 2].
	var pair []int
	if err := json.Unmarshal(data, &pair); err != nil {
		return err
	}
	if len(pair) < 2 {
		return fmt.Errorf("reference pair needs 2 elements, got %d", len(pair))
	}
	p.From, p.To = pair[0], pair[1]
	return nil
}
