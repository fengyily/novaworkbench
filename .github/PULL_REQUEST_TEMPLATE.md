<!--
NovaWorkbench 使用约定式提交 + squash merge。PR 标题会成为 squash commit 主题
（Release Please 读它决定版本号 bump 类型）。本 PR 模板的 body 部分会成为 squash
commit body，其中 ## 🚨 Breaking Change 段是 Release Please 唯一识别 BREAKING
CHANGE 的来源；删除整段即视为非破坏性变更。
-->

## Summary
<!-- 一两句话 -->

## Changes
<!-- 用户/开发者可见的改动列表，会进入 CHANGELOG.md -->

## 🚨 Breaking Change
<!-- 如有破坏性变更：API 移除、schema 迁移、配置改名、环境变量改名、Docker 挂载变更等。
     用 `BREAKING CHANGE: <desc>` 多行格式描述。Release Please 读到这里会触发 major bump。
     没有破坏性变更：删除本段（连同标题）。 -->

## Testing
<!-- 验证方式 -->
