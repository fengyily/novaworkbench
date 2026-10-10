package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// TestScheduleCreateWikiKindGate pins the BIDIRECTIONAL kind ↔ task_type
// gate added when kind=wiki gained scheduled runs:
//
//	kind=wiki        + task_type=wiki    → 201 (the only legal wiki combo)
//	kind=wiki        + task_type=design  → 400 WIKI_ONLY
//	kind=requirement + task_type=wiki    → 400 NOT_WIKI
//	kind=wiki        + task_type=wiki ×2 → 409 ALREADY_SCHEDULED
//
// Why both directions matter: a `design` row on a wiki requirement would
// run ArchitectDesign and write design_docs, which the detail page never
// renders for kind=wiki — the run looks successful and produces nothing
// visible. A `wiki` row on a non-wiki requirement would fail hours later
// inside prepareWikiDoc's kind guard instead of at creation time.
func TestScheduleCreateWikiKindGate(t *testing.T) {
	dir, _ := os.MkdirTemp("", "sched-wiki-")
	defer os.RemoveAll(dir)
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	wikiReqID := seedKindedRequirement(t, d, "req_wiki_sched", service.KindWiki)
	plainReqID := seedKindedRequirement(t, d, "req_plain_sched", "requirement")

	h := NewScheduleHandler(service.NewScheduledTaskService(d), service.NewRequirementService(d, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/schedules", h.Create)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(t *testing.T, reqID, taskType string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"requirement_id": reqID,
			"task_type":      taskType,
			"run_at":         time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339),
		})
		resp, err := http.Post(srv.URL+"/api/schedules", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	errCode := func(t *testing.T, raw string) string {
		t.Helper()
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatalf("decode envelope %q: %v", raw, err)
		}
		return env.Error.Code
	}

	t.Run("wiki kind + wiki task is accepted", func(t *testing.T) {
		code, raw := post(t, wikiReqID, model.SchedTypeWiki)
		if code != 201 {
			t.Fatalf("expected 201, got %d body=%s", code, raw)
		}
	})

	t.Run("second pending wiki row conflicts with itself", func(t *testing.T) {
		code, raw := post(t, wikiReqID, model.SchedTypeWiki)
		if code != 409 || errCode(t, raw) != "ALREADY_SCHEDULED" {
			t.Fatalf("expected 409 ALREADY_SCHEDULED, got %d body=%s", code, raw)
		}
	})

	t.Run("wiki kind rejects a design task", func(t *testing.T) {
		code, raw := post(t, wikiReqID, model.SchedTypeDesign)
		if code != 400 || errCode(t, raw) != "WIKI_ONLY" {
			t.Fatalf("expected 400 WIKI_ONLY, got %d body=%s", code, raw)
		}
	})

	t.Run("wiki kind still rejects coding with the pre-existing code", func(t *testing.T) {
		// The older WIKI_NOT_DEVELOPABLE guard runs first and keeps its own
		// error code — the frontend already maps it, so WIKI_ONLY must not
		// swallow it.
		code, raw := post(t, wikiReqID, model.SchedTypeCoding)
		if code != 400 || errCode(t, raw) != "WIKI_NOT_DEVELOPABLE" {
			t.Fatalf("expected 400 WIKI_NOT_DEVELOPABLE, got %d body=%s", code, raw)
		}
	})

	t.Run("non-wiki kind rejects a wiki task", func(t *testing.T) {
		code, raw := post(t, plainReqID, model.SchedTypeWiki)
		if code != 400 || errCode(t, raw) != "NOT_WIKI" {
			t.Fatalf("expected 400 NOT_WIKI, got %d body=%s", code, raw)
		}
	})

	t.Run("non-wiki kind still accepts a design task", func(t *testing.T) {
		code, raw := post(t, plainReqID, model.SchedTypeDesign)
		if code != 201 {
			t.Fatalf("expected 201, got %d body=%s", code, raw)
		}
	})
}

// TestResolveLaunchTaskTypeWiki pins that the create-page 启动计划 maps a
// wiki requirement to the wiki task type in BOTH modes. The ordering
// matters: CreateRequirementForm forces skip_design=true for wiki, so a
// kind check placed after the SkipDesign branch would silently route wiki
// rows onto the coding path.
func TestResolveLaunchTaskTypeWiki(t *testing.T) {
	for _, mode := range []string{"immediate", "scheduled"} {
		t.Run(mode, func(t *testing.T) {
			got, af := resolveLaunchTaskType(&model.Requirement{
				Kind:        service.KindWiki,
				SkipAnalysis: true,
				SkipDesign:   true,
			}, mode)
			if af != nil {
				t.Fatalf("unexpected failure: %+v", af)
			}
			if got != model.SchedTypeWiki {
				t.Fatalf("got %q, want %q", got, model.SchedTypeWiki)
			}
		})
	}
}

// seedKindedRequirement inserts the project + requirement FK chain with an
// explicit kind, bypassing the services the way seedRequirementForSchedule
// does (the schedule flow only needs the row to resolve through
// RequirementService.Get). Each call makes its own project so the two
// requirements in one test don't collide on the project PK.
func seedKindedRequirement(t *testing.T, d *db.DB, reqID, kind string) string {
	t.Helper()
	projID := "proj_" + reqID
	// local_path is UNIQUE, so derive it from the id — the two requirements
	// in one test each get their own project row.
	if _, err := d.Exec(`INSERT INTO projects (id, name, local_path, status, default_branch) VALUES (?, ?, ?, ?, ?)`,
		projID, "Seed", "/tmp/seed/"+reqID, "ready", "main"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO requirements (id, project_id, title, kind) VALUES (?, ?, ?, ?)`,
		reqID, projID, "Seed req", kind); err != nil {
		t.Fatalf("insert requirement: %v", err)
	}
	return reqID
}
