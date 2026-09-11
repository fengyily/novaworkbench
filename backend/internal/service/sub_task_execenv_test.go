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
}
