package util

import (
	"fmt"
	"os"
	"path/filepath"
)

// DiagramsDir returns ~/.novaworkbench/diagrams/. Created lazily (mode 0o755)
// on first call so callers don't have to MkdirAll before writing.
//
// Sole purpose: 给 mermaid 预览 / SVG 烘焙 / AI 多版本对照 等临时产物一个
// 落地位置。NOT 用于归档（archival 是知识库 SQLite 的事情）—— diagram 临时
// 目录里的产物永远是 transient，**用户手动清理**；我们从未/不会自动 rm。
//
// 重要约束：**永远不要写到需求所在的项目根目录**。这条 helper 是这个约束的
// 唯一合法物理出口；任何"先把 mermaid SVG 落盘再读回去"的工作流都走它。
func DiagramsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home for diagrams dir: %w", err)
	}
	dir := filepath.Join(home, ".novaworkbench", "diagrams")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir diagrams dir %s: %w", dir, err)
	}
	return dir, nil
}
