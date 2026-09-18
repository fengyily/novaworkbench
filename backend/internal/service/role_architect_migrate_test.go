package service

import (
	"strings"
	"testing"
	"time"
)

// The architect persona gained plan-mode guidance (bound the Explore Agent
// budget, write the plan to ~/.claude/plans/<slug>.md) as the prompt-side half
// of the "方案设计完成未入库" fix. MigrateArchitectRole is the only thing that
// carries built-in prompt changes to databases that already have a roles row,
// so these tests pin its propagation + do-not-clobber rules.

// seedArchitectPrompt inserts an architect row carrying the supplied prompt,
// bypassing SeedDefaults so a historical revision can be simulated.
func seedArchitectPrompt(t *testing.T, s *RoleService, prompt string) {
	t.Helper()
	now := time.Now()
	if _, err := s.db.Exec(
		"INSERT INTO roles (id, "+s.db.Ident("key")+", name, description, system_prompt, model, claude_config_id, sort_order, enabled, created_at, updated_at) "+
			"VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		"role_architect", "architect", "架构师", "desc", prompt, "", "", 2, true, now, now,
	); err != nil {
		t.Fatalf("seed architect row: %v", err)
	}
}

func TestMigrateArchitectRole_UpgradesPreviousBuiltIn(t *testing.T) {
	svc := NewRoleService(newTestDB(t))
	// The template-driven prompt as shipped before the plan-mode guidance:
	// it carries the "页面 UI 调整说明" section but not the new "方案落盘" rule.
	previous := strings.Replace(architectMigratedPrompt(),
		"8. **方案落盘**", "8. **占位（旧版没有这条）**", 1)
	if strings.Contains(previous, architectNewPromptSignature) {
		t.Fatalf("fixture still contains %q — the new-prompt signature moved; update this test",
			architectNewPromptSignature)
	}
	seedArchitectPrompt(t, svc, previous)

	migrated, err := svc.MigrateArchitectRole()
	if err != nil {
		t.Fatalf("MigrateArchitectRole: %v", err)
	}
	if !migrated {
		t.Fatal("previous built-in prompt was not migrated; existing installs would never get the plan-mode guidance")
	}
	r, err := svc.GetByKey("architect")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if !strings.Contains(r.SystemPrompt, architectNewPromptSignature) {
		t.Errorf("migrated prompt missing %q", architectNewPromptSignature)
	}
	if !strings.Contains(r.SystemPrompt, "~/.claude/plans/<slug>.md") {
		t.Error("migrated prompt missing the plan-file landing instruction")
	}
}

func TestMigrateArchitectRole_UpgradesLegacyBuiltIn(t *testing.T) {
	svc := NewRoleService(newTestDB(t))
	seedArchitectPrompt(t, svc, "你是一位资深软件架构师，方案应涵盖：整体实现思路、涉及文件、实现步骤。")

	migrated, err := svc.MigrateArchitectRole()
	if err != nil {
		t.Fatalf("MigrateArchitectRole: %v", err)
	}
	if !migrated {
		t.Fatal("legacy prompt was not migrated")
	}
}

func TestMigrateArchitectRole_IdempotentOnCurrentBuiltIn(t *testing.T) {
	svc := NewRoleService(newTestDB(t))
	seedArchitectPrompt(t, svc, architectMigratedPrompt())

	migrated, err := svc.MigrateArchitectRole()
	if err != nil {
		t.Fatalf("MigrateArchitectRole: %v", err)
	}
	if migrated {
		t.Error("current built-in prompt was rewritten; the migration must be a no-op once up to date")
	}
}

func TestMigrateArchitectRole_LeavesCustomPromptAlone(t *testing.T) {
	svc := NewRoleService(newTestDB(t))
	const custom = "我自己写的架构师提示词，不要动它。"
	seedArchitectPrompt(t, svc, custom)

	migrated, err := svc.MigrateArchitectRole()
	if err != nil {
		t.Fatalf("MigrateArchitectRole: %v", err)
	}
	if migrated {
		t.Fatal("a user-customized prompt must never be clobbered")
	}
	r, err := svc.GetByKey("architect")
	if err != nil {
		t.Fatalf("GetByKey: %v", err)
	}
	if r.SystemPrompt != custom {
		t.Errorf("SystemPrompt = %q, want the custom text preserved", r.SystemPrompt)
	}
}

// TestArchitectDefaultPromptCarriesPlanModeGuidance guards the prompt content
// itself: the Explore-budget hint and the plan-file landing instruction are
// what keep the architect from going silent past the watchdog window and
// losing the design, so they must stay in the built-in.
func TestArchitectDefaultPromptCarriesPlanModeGuidance(t *testing.T) {
	prompt := architectMigratedPrompt()
	if prompt == "" {
		t.Fatal("architect role missing from DefaultRoles()")
	}
	for _, want := range []string{
		architectNewPromptSignature,
		"~/.claude/plans/<slug>.md",
		"ExitPlanMode",
		"Explore Agent",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("architect prompt missing %q", want)
		}
	}
}
