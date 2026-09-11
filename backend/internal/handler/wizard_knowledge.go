package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/novaworkbench/backend/internal/store"
)

// buildKnowledgeBlock loads the project knowledge most relevant to a
// requirement and renders it as a prompt section (## 项目知识库). It returns an
// empty block when nothing relevant exists (the caller then leaves the prompt
// unchanged). Per-entry content is capped at ~8KB and the whole block at ~60KB
// so a large knowledge base can't blow up the prompt budget. The returned
// titles drive the SSE "knowledge" event so the UI can show what was read.
func (h *WizardHandler) buildKnowledgeBlock(projectID, requirementTitle string) (block string, titles []string) {
	if h.knowledgeSvc == nil {
		return "", nil
	}
	items, err := h.knowledgeSvc.ListForRequirement(projectID, requirementTitle, 20)
	if err != nil || len(items) == 0 {
		if err != nil {
			log.Printf("[wizard] load knowledge %s: %v", projectID, err)
		}
		return "", nil
	}
	const (
		perItem  = 8 * 1024
		maxBlock = 60 * 1024
	)
	var b strings.Builder
	b.WriteString("## 项目知识库\n\n以下是与本需求相关的项目知识库内容，请先阅读再进行分析：\n\n")
	titles = make([]string, 0, len(items))
	total := 0
	omitted := 0
	for _, k := range items {
		content := k.Content
		if len(content) > perItem {
			content = content[:perItem] + "\n…（已截断）"
		}
		if total+len(content) > maxBlock {
			omitted++
			continue
		}
		b.WriteString(fmt.Sprintf("### %s\n%s\n\n", k.Title, content))
		titles = append(titles, k.Title)
		total += len(content)
	}
	if omitted > 0 {
		b.WriteString(fmt.Sprintf("…（知识库内容较多，略去 %d 条）\n", omitted))
	}
	block = b.String()
	if block == "" {
		block = "(项目知识库为空)"
	}
	return block, titles
}

// emitKnowledgeEvent writes the wizard's SSE "knowledge" event into a JobStore
// job so subscribers can render the entries read before the stage started. The
// event carries only titles (full content goes into the claude prompt, not the
// SSE stream). Emit it BEFORE invoking claude, once per job.
func emitKnowledgeEvent(job *store.Job, titles []string) {
	items := make([]map[string]string, 0, len(titles))
	for _, t := range titles {
		items = append(items, map[string]string{"title": t})
	}
	data, _ := json.Marshal(map[string]interface{}{
		"type":  "knowledge",
		"count": len(titles),
		"items": items,
	})
	job.Append(store.LogLine{Type: "knowledge", Content: string(data)})
}

// inputToolPath extracts the file path / search pattern a tool_use input
// targets, for the "was the injected knowledge actually used?" evaluation.
// Read/Write/Edit carry file_path; Grep/Glob a pattern. Bash is skipped (its
// command is too noisy). Empty string means "nothing path-like".
func inputToolPath(name string, input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	switch name {
	case "Read", "Write", "Edit":
		if p, ok := input["file_path"].(string); ok {
			return p
		}
	case "Glob":
		if p, ok := input["pattern"].(string); ok {
			return p
		}
	case "Grep":
		if p, ok := input["pattern"].(string); ok {
			return p
		}
	}
	return ""
}

// knowledgeUseItem is one evaluated knowledge entry: did the run actually
// touch the file the entry describes, or mention the entry's title in its
// final output?
type knowledgeUseItem struct {
	Title string `json:"title"`
	Used  bool   `json:"used"`
}

// evaluateKnowledgeUse marks whether each read knowledge entry left a trace of
// actual use in the run: (1) a tool call touched a path whose basename matches
// the entry title (e.g. knowledge "CLAUDE.md" and the CLI Read'ed CLAUDE.md),
// or (2) the entry title appears in the final result text. This is a cheap
// signal derived from events already captured — NO extra LLM call. It is
// intentionally conservative: a not-marked entry isn't proof it was useless
// (e.g. Project Structure informs behavior without being re-read or named).
func evaluateKnowledgeUsage(titles []string, toolFiles []string, resultText string) (items []knowledgeUseItem, usedCount int) {
	if len(titles) == 0 {
		return nil, 0
	}
	lowerResult := strings.ToLower(resultText)
	tools := make([]string, 0, len(toolFiles))
	for _, f := range toolFiles {
		tools = append(tools, strings.ToLower(filepath.ToSlash(f)))
	}
	items = make([]knowledgeUseItem, 0, len(titles))
	for _, t := range titles {
		normTitle := strings.TrimSpace(strings.TrimSuffix(t, "/"))
		used := false
		if normTitle == "" {
			items = append(items, knowledgeUseItem{Title: t, Used: false})
			continue
		}
		lower := strings.ToLower(normTitle)
		for _, tf := range tools {
			base := tf
			if i := strings.LastIndexByte(tf, '/'); i >= 0 {
				base = tf[i+1:]
			}
			// Basename equality, or a direct basename containment (covers
			// tool paths like ".../docs/CLAUDE.md" against title "CLAUDE.md").
			if base == lower || (len(lower) >= 3 && strings.Contains(base, lower)) {
				used = true
				break
			}
		}
		if !used && lowerResult != "" && strings.Contains(lowerResult, lower) {
			used = true
		}
		if used {
			usedCount++
		}
		items = append(items, knowledgeUseItem{Title: t, Used: used})
	}
	return items, usedCount
}

// emitKnowledgeResultEvent closes the "读取项目知识库" loop with the evaluation
// of each entry's actual use (tool trace + result-text mentions). Emitted once
// from the stage that performed the read, on the success path, so the UI can
// mark each entry as 已引用 / 未直接引用.
func emitKnowledgeResultEvent(job *store.Job, items []knowledgeUseItem, usedCount int) {
	if len(items) == 0 {
		return
	}
	jsonItems := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		jsonItems = append(jsonItems, map[string]interface{}{"title": it.Title, "used": it.Used})
	}
	data, _ := json.Marshal(map[string]interface{}{
		"type":  "knowledge_result",
		"total": len(items),
		"used":  usedCount,
		"items": jsonItems,
	})
	job.Append(store.LogLine{Type: "knowledge_result", Content: string(data)})
}