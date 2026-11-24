package prompt

import "strings"

// 「跨项目需求」的附件块。
//
// 跨项目作业的提示词里要带上用户为这条跨项目需求上传的附件（截图、日志、
// 接口文档 …），与「新建需求」的三个阶段看到的是同一份东西 —— 用户在需求
// 里附了什么，AI 就该看到什么。
//
// 附件块的内容由 handler.attachManifestFor 生成（它已经带了自己的
// 「## 📎 已附加文件」标题、每个附件的绝对路径与用户写的描述），所以这里
// **原样追加**，不再套一层标题 —— 套两层会出现两个意思相同的标题。

// appendCrossAttachment writes the attachment manifest into the prompt.
//
// manifest 是 handler.attachManifestFor 的输出（可能为空串）。
//
// 空清单 = 该跨项目需求没有附件：一个字节都不写，提示词与「附件这个特性
// 存在之前」逐字相同 —— 这样已有的提示词测试不需要为它改期望值。
func appendCrossAttachment(b *strings.Builder, manifest string) {
	m := strings.TrimSpace(manifest)
	if m == "" {
		return
	}
	b.WriteString(m)
	b.WriteString("\n\n")
}
