package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// 需求「引用」关系的解析与写入。
//
// 存储沿用 tags / marks 的既有范式：JSON 数组存 TEXT 列 + 一个整体替换的
// REST 端点 + 服务层归一化。列的语义见 db/schema.go 的
// `referenced_requirements` 注释。
//
// 解析分两段：
//
//	① 直接引用（本需求自己勾选的）+ 继承自主需求的引用（子需求自动带上
//	   主需求的引用集合）；
//	② 一次性把目标需求的「描述 + 方案 + 展示字段」查出来。
//
// 两条窄查询，不碰 List/Get 那张 80 列的大 SELECT —— 本列刻意不放进
// model.Requirement 载荷，避免位置式扫描错位。

// 引用集合的边界。条数上限与选择器 UI 一致；引用原因的长度上限按 rune
// 截断（按字节会劈开中文字符产生乱码）。
const (
	maxReferenceCount     = 8
	maxReferenceNoteRunes = 120
)

// 引用写入的校验错误（由 handler 原样透出给前端）。
var (
	// ErrReferenceSelf — 需求不能引用自己。
	ErrReferenceSelf = errors.New("a requirement cannot reference itself")
	// ErrReferenceNotFound — 引用的目标需求不存在。
	ErrReferenceNotFound = errors.New("referenced requirement not found")
)

// normalizeReferences trims / dedupes / caps the caller-supplied reference
// list so the stored JSON array is always well-formed and bounded.
//
// Order of operations mirrors normalizeTags: first-seen wins, so the user's
// curated order survives a round-trip. The note is truncated by rune so a
// Chinese sentence never gets cut mid-character.
//
// Self-reference is rejected outright rather than silently dropped — the user
// explicitly asked for something impossible and deserves to be told.
func normalizeReferences(raw []model.RequirementReferenceInput, selfID string) ([]model.RequirementReferenceInput, error) {
	out := make([]model.RequirementReferenceInput, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, r := range raw {
		id := strings.TrimSpace(r.RequirementID)
		if id == "" {
			continue
		}
		if selfID != "" && id == selfID {
			return nil, ErrReferenceSelf
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		note := strings.TrimSpace(r.Note)
		if len([]rune(note)) > maxReferenceNoteRunes {
			note = string([]rune(note)[:maxReferenceNoteRunes])
		}
		out = append(out, model.RequirementReferenceInput{RequirementID: id, Note: note})
		if len(out) >= maxReferenceCount {
			break
		}
	}
	return out, nil
}

// SetReferences replaces the requirement's **direct** reference set with the
// caller-supplied values. Inherited (parent-origin) references are derived at
// read time and are never stored on this row.
//
// The targets are validated to exist, but deliberately NOT to live in the same
// project: a reference is an explicit user choice ("参考另一个项目里已定稿的
// 方案"), and cross-project reuse is the whole point of the feature. This is
// intentionally different from parent_id's same-project rule, which encodes a
// hierarchy rather than a reference.
func (s *RequirementService) SetReferences(id string, refs []model.RequirementReferenceInput) ([]model.RequirementReference, error) {
	owner, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	cleaned, err := normalizeReferences(refs, owner.ID)
	if err != nil {
		return nil, err
	}
	// Every target must exist. A dangling id would otherwise be silently
	// dropped at read time and the user would think their reference stuck.
	for _, r := range cleaned {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(1) FROM requirements WHERE id = ?", r.RequirementID).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("%w: %s", ErrReferenceNotFound, r.RequirementID)
		}
	}
	blob := "[]"
	if len(cleaned) > 0 {
		raw, mErr := json.Marshal(cleaned)
		if mErr != nil {
			return nil, fmt.Errorf("marshal references: %w", mErr)
		}
		blob = string(raw)
	}
	if _, err := s.db.Exec(
		"UPDATE requirements SET referenced_requirements=?, updated_at=? WHERE id=?",
		blob, time.Now(), id,
	); err != nil {
		return nil, err
	}
	return s.ListReferences(id)
}

// ListReferences returns the resolved reference view for a requirement: the
// direct references plus the ones inherited from its master requirement.
// No body content — this is the shape the picker / detail page renders.
func (s *RequirementService) ListReferences(id string) ([]model.RequirementReference, error) {
	resolved, err := s.resolveReferences(id)
	if err != nil {
		return nil, err
	}
	out := make([]model.RequirementReference, 0, len(resolved))
	for _, r := range resolved {
		out = append(out, r.RequirementReference)
	}
	return out, nil
}

// ResolveReferenceContent returns the resolved references **with** the target's
// description and existing plan. This is the only shape that may be injected
// into a prompt — and even then only Description + DesignDocs are used (the
// caller is prompt.BuildReferencedContextBlock).
func (s *RequirementService) ResolveReferenceContent(id string) ([]model.RequirementReferenceContent, error) {
	resolved, err := s.resolveReferences(id)
	if err != nil {
		return nil, err
	}
	out := make([]model.RequirementReferenceContent, 0, len(resolved))
	for _, r := range resolved {
		out = append(out, model.RequirementReferenceContent{
			RequirementReference: r.RequirementReference,
			Description:          r.Description,
			DesignDocs:           r.DesignDocs,
		})
	}
	return out, nil
}

// resolvedReference is the internal, fully-hydrated shape: the API view plus
// the two body fields the prompt layer needs.
type resolvedReference struct {
	model.RequirementReference
	Description string
	DesignDocs  string
}

// resolveReferences is the single source of truth for "which requirements does
// this one reference". Both ListReferences and ResolveReferenceContent call it.
//
// Resolution order is fixed so the result is reproducible:
//
//  1. direct references first, then the ones inherited from the master
//     requirement;
//  2. the master's validity is checked exactly like buildParentBlock does
//     (exists + same project + the master is not itself a child) — an invalid
//     master means "don't inherit", never an error;
//  3. the requirement itself and its master are dropped from the set (the
//     master's content is already fully covered by the parent-context block, so
//     repeating it would waste prompt budget and confuse the model);
//  4. duplicates collapse, direct wins;
//  5. targets that no longer exist are skipped (a deleted requirement leaves a
//     dangling id behind — degrading beats erroring, and the stale id clears
//     itself on the next save);
//  6. the set is capped at maxReferenceCount.
func (s *RequirementService) resolveReferences(id string) ([]resolvedReference, error) {
	self, err := s.referenceRow(id)
	if err != nil {
		return nil, err
	}
	if self == nil {
		return nil, ErrRequirementNotFound
	}

	type refEntry struct {
		id     string
		note   string
		origin string
	}
	var direct []refEntry
	for _, r := range decodeReferenceColumn(self.referenced) {
		direct = append(direct, refEntry{id: r.RequirementID, note: r.Note, origin: model.ReferenceOriginDirect})
	}

	var inherited []refEntry
	if strings.TrimSpace(self.parentID) != "" {
		parent, perr := s.referenceRow(self.parentID)
		switch {
		case perr != nil:
			log.Printf("[requirement-references] parent lookup failed for %s (parent=%s): %v — continuing without inherited references", self.id, self.parentID, perr)
		case parent == nil:
			log.Printf("[requirement-references] parent %s of %s no longer exists — continuing without inherited references", self.parentID, self.id)
		case parent.projectID != self.projectID:
			log.Printf("[requirement-references] parent %s is in project %s but child %s is in %s — skipping inherited references", parent.id, parent.projectID, self.id, self.projectID)
		case strings.TrimSpace(parent.parentID) != "":
			log.Printf("[requirement-references] parent %s is itself a child (parent_id=%s) — nesting is one level deep, skipping inherited references", parent.id, parent.parentID)
		default:
			for _, r := range decodeReferenceColumn(parent.referenced) {
				inherited = append(inherited, refEntry{id: r.RequirementID, note: r.Note, origin: model.ReferenceOriginParent})
			}
		}
	}

	// Merge + filter. Direct first so it wins a collision.
	seen := make(map[string]struct{})
	var ordered []refEntry
	for _, list := range [][]refEntry{direct, inherited} {
		for _, e := range list {
			if e.id == "" || e.id == self.id || e.id == self.parentID {
				continue
			}
			if _, dup := seen[e.id]; dup {
				continue
			}
			seen[e.id] = struct{}{}
			ordered = append(ordered, e)
			if len(ordered) >= maxReferenceCount {
				break
			}
		}
		if len(ordered) >= maxReferenceCount {
			break
		}
	}
	if len(ordered) == 0 {
		return []resolvedReference{}, nil
	}

	// One IN query for all the target bodies + display fields.
	args := make([]any, 0, len(ordered))
	for _, e := range ordered {
		args = append(args, e.id)
	}
	rows, err := s.db.Query(
		"SELECT r.id, r.project_id, r.title, r.description, r.status, r.kind, r.design_docs, COALESCE(p.name,'') "+
			"FROM requirements r LEFT JOIN projects p ON p.id = r.project_id "+
			"WHERE r.id IN ("+sqlPlaceholders(len(ordered))+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := make(map[string]resolvedReference, len(ordered))
	for rows.Next() {
		var r resolvedReference
		var design string
		if err := rows.Scan(&r.RequirementID, &r.ProjectID, &r.Title, &r.Description, &r.Status, &r.Kind, &design, &r.ProjectName); err != nil {
			return nil, err
		}
		r.DesignDocs = design
		r.HasDesign = designHasContent(design)
		byID[r.RequirementID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Re-order by the merge order (the IN query returns rows in whatever order
	// the engine likes) and drop the targets that no longer exist.
	out := make([]resolvedReference, 0, len(ordered))
	for _, e := range ordered {
		r, ok := byID[e.id]
		if !ok {
			// Dangling reference — the target was deleted. Skip it (degrade,
			// don't fail the design run).
			continue
		}
		r.Note = e.note
		r.Origin = e.origin
		out = append(out, r)
	}
	return out, nil
}

// referenceRow is the narrow owner/parent lookup: five columns, no join.
func (s *RequirementService) referenceRow(id string) (*referenceRow, error) {
	var r referenceRow
	err := s.db.QueryRow(
		"SELECT id, project_id, title, parent_id, referenced_requirements FROM requirements WHERE id = ?", id).
		Scan(&r.id, &r.projectID, &r.title, &r.parentID, &r.referenced)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// referenceRow is the narrow projection of a requirement row used by the
// reference resolver.
type referenceRow struct {
	id         string
	projectID  string
	title      string
	parentID   string
	referenced string
}

// decodeReferenceColumn parses the JSON column into a slice. A malformed or
// empty column degrades to "no references" rather than failing the design run
// — the column is advisory context, never a hard dependency.
func decodeReferenceColumn(raw string) []model.RequirementReferenceInput {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "[]" || trimmed == "null" {
		return nil
	}
	var out []model.RequirementReferenceInput
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		log.Printf("[requirement-references] malformed referenced_requirements column: %v", err)
		return nil
	}
	return out
}

// designHasContent reports whether a stored design blob carries an actual plan.
// Legacy rows store design_docs as a JSON array, so an untouched row reads as
// "[]" rather than an empty string.
func designHasContent(d string) bool {
	switch strings.TrimSpace(d) {
	case "", "[]", "{}", "null":
		return false
	}
	return true
}

// sqlPlaceholders builds "?,?,?" for an IN list. The count is always > 0 at
// every call site (the caller skips the query for an empty list) — an empty
// list would produce the illegal "IN ()".
func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
