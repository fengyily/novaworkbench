package service

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
)

// TestExecEnvSanity exercises the new agent_server_id plumbing end-to-end
// against a real (temp) SQLite DB: schema migration adds the column, sub-task
// inserts carry the environment, the correlated-subquery name join resolves,
// and NULL (legacy) vs explicit-empty (本地) are distinguished via
// AgentServerIDSet. Also checks requirements List/Get read back
// design_agent_server_id + name. Regression harness for the exec-env feature.
func TestExecEnvSanity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sanity.db")
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: dbPath})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	defer d.Close()

	now := time.Now()
	// Seed an agent_servers row + a project + a requirement bound to it.
	if _, err := d.Exec(`INSERT INTO agent_servers (id, name, host, port, username, auth_type, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		"as_1", "Prod Box", "10.0.0.1", 22, "root", "key", now, now); err != nil {
		t.Fatalf("insert agent_server: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO requirements (id,project_id,title,description,status,priority,kind,acceptance_criteria,design_docs,conversation_ids,created_by,skip_analysis,skip_design,agent_server_id,design_agent_server_id,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"req_1", "proj_1", "R", "", "developing", "medium", "requirement", "[]", "[]", "[]", "user", false, false, "as_1", "as_1", now, now); err != nil {
		t.Fatalf("insert requirement: %v", err)
	}

	reqSvc := NewRequirementService(d)
	got, err := reqSvc.Get("req_1")
	if err != nil {
		t.Fatalf("req Get: %v", err)
	}
	if got.DesignAgentServerID != "as_1" || got.DesignAgentServerName != "Prod Box" {
		t.Fatalf("design env not read back: id=%q name=%q", got.DesignAgentServerID, got.DesignAgentServerName)
	}
	if got.AgentServerName != "Prod Box" {
		t.Fatalf("dev server name not read back: %q", got.AgentServerName)
	}
	list, err := reqSvc.List("proj_1", "", "", "")
	if err != nil || len(list) != 1 {
		t.Fatalf("req List: %v n=%d", err, len(list))
	}
	if list[0].DesignAgentServerName != "Prod Box" {
		t.Fatalf("List design name not attached: %q", list[0].DesignAgentServerName)
	}

	stSvc := NewSubTaskService(d)
	// Explicit remote sub-task.
	remote, err := stSvc.Create("req_1", "remote task", "do X", "", "", "", 0, "as_1")
	if err != nil {
		t.Fatalf("create remote: %v", err)
	}
	if !remote.AgentServerIDSet || remote.AgentServerID != "as_1" {
		t.Fatalf("remote row: set=%v id=%q", remote.AgentServerIDSet, remote.AgentServerID)
	}
	// Explicit local sub-task (empty string, NOT null).
	local, err := stSvc.Create("req_1", "local task", "do Y", "", "", "", 0, "")
	if err != nil {
		t.Fatalf("create local: %v", err)
	}
	if !local.AgentServerIDSet || local.AgentServerID != "" {
		t.Fatalf("local row: set=%v id=%q", local.AgentServerIDSet, local.AgentServerID)
	}

	// Read back via Get: name join resolves for the remote row.
	gr, err := stSvc.Get(remote.ID)
	if err != nil {
		t.Fatalf("st Get remote: %v", err)
	}
	if gr.AgentServerID != "as_1" || gr.AgentServerName != "Prod Box" || !gr.AgentServerIDSet {
		t.Fatalf("remote read-back: id=%q name=%q set=%v", gr.AgentServerID, gr.AgentServerName, gr.AgentServerIDSet)
	}
	gl, err := stSvc.Get(local.ID)
	if err != nil {
		t.Fatalf("st Get local: %v", err)
	}
	if gl.AgentServerID != "" || gl.AgentServerName != "" || !gl.AgentServerIDSet {
		t.Fatalf("local read-back: id=%q name=%q set=%v", gl.AgentServerID, gl.AgentServerName, gl.AgentServerIDSet)
	}

	// Simulate a legacy row: NULL agent_server_id (predates the column).
	if _, err := d.Exec(`INSERT INTO sub_tasks (id, requirement_id, title, prompt, status, agent_server_id, created_at, updated_at) VALUES (?,?,?,?,?,NULL,?,?)`,
		"st_legacy", "req_1", "legacy", "z", model.SubTaskStatusDone, now, now); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	leg, err := stSvc.Get("st_legacy")
	if err != nil {
		t.Fatalf("st Get legacy: %v", err)
	}
	if leg.AgentServerIDSet {
		t.Fatalf("legacy row should scan as unset (NULL), got set=true")
	}
	// The raw column must stay untouched even after effective-env resolution —
	// the frontend's "已覆盖默认" hint keys off the user's own override, not the
	// resolved value.
	if leg.AgentServerID != "" {
		t.Fatalf("legacy raw AgentServerID should stay empty, got %q", leg.AgentServerID)
	}
	// Get() must resolve the effective environment too (not just List): a
	// legacy NULL row under a remote parent resolves TO the parent's server.
	if leg.EffectiveAgentServerID != "as_1" {
		t.Fatalf("legacy Get effective env: want %q (parent fallback), got %q", "as_1", leg.EffectiveAgentServerID)
	}
	if leg.EffectiveAgentServerName != "Prod Box" {
		t.Fatalf("legacy Get effective name: want %q, got %q", "Prod Box", leg.EffectiveAgentServerName)
	}

	// ── Effective environment (the display/exec source of truth) ──────────
	// List() must resolve every row: the legacy NULL row falls back to the
	// remote parent, while an explicit 本地 pick under that same remote parent
	// must NOT be redirected back to it. This is the exact distinction the
	// frontend cannot re-derive (AgentServerIDSet is json:"-"), so a
	// regression here would silently re-break the card badge + Stop button.
	stList, err := stSvc.List("req_1")
	if err != nil {
		t.Fatalf("st List: %v", err)
	}
	if len(stList) != 3 {
		t.Fatalf("st List: want 3 rows (remote, local, legacy), got %d", len(stList))
	}
	byID := map[string]model.SubTask{}
	for _, st := range stList {
		byID[st.ID] = st
	}
	// a) legacy NULL row → inherits the remote parent's environment.
	stRow := byID["st_legacy"]
	if stRow.EffectiveAgentServerID != "as_1" || stRow.EffectiveAgentServerName != "Prod Box" {
		t.Fatalf("legacy List effective env: want as_1/Prod Box, got %q/%q",
			stRow.EffectiveAgentServerID, stRow.EffectiveAgentServerName)
	}
	// b) explicit 本地 row → stays local, no parent fallback.
	stRow = byID[local.ID]
	if stRow.EffectiveAgentServerID != "" {
		t.Fatalf("explicit-local effective env must stay empty (no parent fallback), got %q", stRow.EffectiveAgentServerID)
	}
	if stRow.EffectiveAgentServerName != "" {
		t.Fatalf("explicit-local effective name must stay empty, got %q", stRow.EffectiveAgentServerName)
	}
	// c) explicit remote row → itself.
	stRow = byID[remote.ID]
	if stRow.EffectiveAgentServerID != "as_1" || stRow.EffectiveAgentServerName != "Prod Box" {
		t.Fatalf("remote List effective env: want as_1/Prod Box, got %q/%q",
			stRow.EffectiveAgentServerID, stRow.EffectiveAgentServerName)
	}
	// d) Get() must apply the same rule as List() (both read paths call
	// attachEffectiveEnv; a divergence would make the card badge disagree with
	// the Stop button's backend check).
	glEffective, err := stSvc.Get(local.ID)
	if err != nil {
		t.Fatalf("st Get local (effective): %v", err)
	}
	if glEffective.EffectiveAgentServerID != "" || glEffective.EffectiveAgentServerName != "" {
		t.Fatalf("explicit-local Get effective env: want empty, got %q/%q",
			glEffective.EffectiveAgentServerID, glEffective.EffectiveAgentServerName)
	}
}

// TestAttachEffectiveEnvLocalParent covers the mirror case of
// TestExecEnvSanity's remote parent: a LOCAL requirement that owns both a
// legacy NULL row and an explicitly-remote row. It pins down that the fallback
// keeps working in both directions (NULL → parent = local = "", explicit
// remote → the chosen server) and that a batch read applies the same rule.
func TestAttachEffectiveEnvLocalParent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "execenv_local.db")
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: dbPath})
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	defer d.Close()

	now := time.Now()
	if _, err := d.Exec(`INSERT INTO agent_servers (id, name, host, port, username, auth_type, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		"as_2", "Edge Box", "10.0.0.2", 22, "root", "key", now, now); err != nil {
		t.Fatalf("insert agent_server: %v", err)
	}
	// Local parent: agent_server_id = '' (the default). Children must not be
	// dragged onto a server just because one exists in agent_servers.
	if _, err := d.Exec(`INSERT INTO requirements (id,project_id,title,description,status,priority,kind,acceptance_criteria,design_docs,conversation_ids,created_by,skip_analysis,skip_design,agent_server_id,design_agent_server_id,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"req_2", "proj_2", "R2", "", "developing", "medium", "requirement", "[]", "[]", "[]", "user", false, false, "", "", now, now); err != nil {
		t.Fatalf("insert requirement: %v", err)
	}
	stSvc := NewSubTaskService(d)
	legacy, err := stSvc.Create("req_2", "legacy task", "do L", "", "", "", 0, "")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// Force the row back to NULL to emulate a pre-column row.
	if _, err := d.Exec(`UPDATE sub_tasks SET agent_server_id=NULL WHERE id=?`, legacy.ID); err != nil {
		t.Fatalf("null out agent_server_id: %v", err)
	}
	remote, err := stSvc.Create("req_2", "remote task", "do R", "", "", "", 0, "as_2")
	if err != nil {
		t.Fatalf("create remote child: %v", err)
	}

	got, err := stSvc.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get legacy: %v", err)
	}
	if got.AgentServerIDSet {
		t.Fatalf("expected NULL row to scan as unset")
	}
	if got.EffectiveAgentServerID != "" || got.EffectiveAgentServerName != "" {
		t.Fatalf("NULL row under a LOCAL parent must resolve to local, got %q/%q",
			got.EffectiveAgentServerID, got.EffectiveAgentServerName)
	}
	gotRemote, err := stSvc.Get(remote.ID)
	if err != nil {
		t.Fatalf("Get remote: %v", err)
	}
	if gotRemote.EffectiveAgentServerID != "as_2" || gotRemote.EffectiveAgentServerName != "Edge Box" {
		t.Fatalf("explicit remote under a local parent: want as_2/Edge Box, got %q/%q",
			gotRemote.EffectiveAgentServerID, gotRemote.EffectiveAgentServerName)
	}

	// ListByBatch is the third read path (OrchestrationQueue enumerates a
	// batch's children through it, see wizard_orchestration.go) and must apply
	// the SAME resolution rule as List/Get — otherwise an auto-orchestrated
	// child's card and its dispatch could disagree. No orchestration_batches
	// row is required: sub_tasks.batch_id carries no FK.
	batchedLegacy, err := stSvc.Create("req_2", "batched legacy", "do B", "", "", "batch_1", 1, "")
	if err != nil {
		t.Fatalf("create batched legacy child: %v", err)
	}
	if _, err := d.Exec(`UPDATE sub_tasks SET agent_server_id=NULL WHERE id=?`, batchedLegacy.ID); err != nil {
		t.Fatalf("null out batched agent_server_id: %v", err)
	}
	if _, err := stSvc.Create("req_2", "batched remote", "do BR", "", "", "batch_1", 2, "as_2"); err != nil {
		t.Fatalf("create batched remote child: %v", err)
	}
	batchRows, err := stSvc.ListByBatch("batch_1")
	if err != nil {
		t.Fatalf("ListByBatch: %v", err)
	}
	if len(batchRows) != 2 {
		t.Fatalf("ListByBatch: want 2 rows, got %d", len(batchRows))
	}
	for _, st := range batchRows {
		if st.ID == batchedLegacy.ID {
			if st.EffectiveAgentServerID != "" || st.EffectiveAgentServerName != "" {
				t.Fatalf("batched NULL row under a LOCAL parent must resolve to local, got %q/%q",
					st.EffectiveAgentServerID, st.EffectiveAgentServerName)
			}
			continue
		}
		if st.EffectiveAgentServerID != "as_2" || st.EffectiveAgentServerName != "Edge Box" {
			t.Fatalf("batched remote row: want as_2/Edge Box, got %q/%q",
				st.EffectiveAgentServerID, st.EffectiveAgentServerName)
		}
	}
}
