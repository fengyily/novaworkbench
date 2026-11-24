package service

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// The /projects list renders 需求数 / 问题数 / 想法数 from
// model.Project.RequirementCount / IssueCount / IdeaCount. These used to be
// denormalized columns on `projects` that NOTHING ever wrote, so every
// project rendered 0 / 0 / 0. The counts are now derived from `requirements`
// (see requirementCountsExpr) — this test locks the bucketing rules down:
//
//	kind='requirement' → 需求数, kind='issue' → 问题数, kind='idea' → 想法数
//	kind=''            → 需求数 (legacy row written before the kind column)
//	kind='wiki'        → counted in none of the three
//	status             → not filtered; an archived requirement still counts
//
// It also pins the read paths to the same numbers (ListForUser / Get /
// getAny / ListTrash) so the API cannot drift back into "one endpoint knows,
// the other doesn't".
func TestProjectRequirementCounts(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "t.db")})
	if err != nil {
		t.Fatal(err)
	}
	s := NewProjectService(d, nil, nil)

	for _, id := range []string{"proj_a", "proj_b", "proj_c"} {
		if _, err := d.Exec(`INSERT INTO projects (id, name, local_path) VALUES (?,?,?)`,
			id, id, filepath.Join(dir, id)); err != nil {
			t.Fatal(err)
		}
	}
	// proj_a: 2 requirement (one archived) + 1 legacy empty-kind row → 需求数 3;
	// 1 issue → 问题数 1; 1 idea → 想法数 1; 2 wiki → 计入 0 个桶。
	// proj_b: 没有需求 → 三个 0（不能是 NULL / 不能报错）。
	// proj_c: 软删除（在回收站），需求数 1。
	rows := []struct {
		id     string
		proj   string
		kind   string
		status string
	}{
		{"req_1", "proj_a", "requirement", "draft"},
		{"req_2", "proj_a", "requirement", "archived"},
		{"req_3", "proj_a", "", "draft"}, // legacy row: empty kind means "requirement"
		{"req_4", "proj_a", "issue", "draft"},
		{"req_5", "proj_a", "idea", "draft"},
		{"req_6", "proj_a", "wiki", "designed"},
		{"req_7", "proj_a", "wiki", "draft"},
		{"req_8", "proj_c", "requirement", "draft"},
	}
	for _, r := range rows {
		if _, err := d.Exec(
			`INSERT INTO requirements (id, project_id, title, kind, status) VALUES (?,?,?,?,?)`,
			r.id, r.proj, r.id, r.kind, r.status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(`UPDATE projects SET deleted_at = ? WHERE id = 'proj_c'`, "2026-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	byID := func(list []model.Project, id string) *model.Project {
		for i := range list {
			if list[i].ID == id {
				return &list[i]
			}
		}
		return nil
	}
	want := func(req, issue, idea int) func(*model.Project) error {
		return func(p *model.Project) error {
			if p.RequirementCount != req || p.IssueCount != issue || p.IdeaCount != idea {
				return fmt.Errorf("project %s: want requirement/issue/idea = %d/%d/%d, got %d/%d/%d",
					p.ID, req, issue, idea, p.RequirementCount, p.IssueCount, p.IdeaCount)
			}
			return nil
		}
	}

	// --- ListForUser (admin) ---
	// NOTE: ListForUser("", true) returns an EMPTY list by design (the
	// "no user and not admin" guard), so a non-empty userID is required.
	list, err := s.ListForUser("usr_1", true)
	if err != nil {
		t.Fatal(err)
	}
	if p := byID(list, "proj_a"); p == nil {
		t.Fatal("proj_a missing from ListForUser")
	} else if err := want(3, 1, 1)(p); err != nil {
		t.Fatal(err)
	}
	if p := byID(list, "proj_b"); p == nil {
		t.Fatal("proj_b missing from ListForUser")
	} else if err := want(0, 0, 0)(p); err != nil {
		t.Fatal(err)
	}
	// soft-deleted projects are not in the active list
	if byID(list, "proj_c") != nil {
		t.Fatal("proj_c is soft-deleted and must not appear in the active list")
	}

	// --- Get: same numbers as the list ---
	got, err := s.Get("proj_a")
	if err != nil {
		t.Fatal(err)
	}
	if err := want(3, 1, 1)(got); err != nil {
		t.Fatal(err)
	}

	// --- getAny: same numbers, and it also sees soft-deleted projects ---
	any, err := s.getAny("proj_c")
	if err != nil {
		t.Fatal(err)
	}
	if err := want(1, 0, 0)(any); err != nil {
		t.Fatal(err)
	}

	// --- ListTrash: the soft-deleted project keeps its counts ---
	trash, err := s.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if p := byID(trash, "proj_c"); p == nil {
		t.Fatal("proj_c missing from ListTrash")
	} else if err := want(1, 0, 0)(p); err != nil {
		t.Fatal(err)
	}

	// --- non-admin sees only the projects user_projects grants ---
	if _, err := d.Exec(`INSERT INTO users (id, username) VALUES ('usr_2','usr_2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO user_projects (user_id, project_id) VALUES ('usr_2','proj_b')`); err != nil {
		t.Fatal(err)
	}
	vis, err := s.ListForUser("usr_2", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(vis) != 1 || vis[0].ID != "proj_b" {
		t.Fatalf("want only proj_b visible for usr_2, got %+v", vis)
	}
	if err := want(0, 0, 0)(&vis[0]); err != nil {
		t.Fatal(err)
	}
}
