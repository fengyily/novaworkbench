package service

import (
	"testing"

	"github.com/novaworkbench/backend/internal/llm"
)

// Compile-time proof that SettingService is the gateway's effort provider.
//
// The wiring in main.go is `llmGateway.SetEffortProvider(settingSvc)` — a
// runtime call that silently degrades to the built-in defaults if the two
// types ever drift apart. This assertion makes the drift a COMPILE error
// instead of a silent behaviour change.
var _ llm.EffortProvider = (*SettingService)(nil)

// TestEffortConfigDefaults pins the shipped policy.
//
// The shipped values ARE the product requirement: 方案设计 high /
// 开发实现 medium / 提交推送 low. The fourth (其他任务) is high because that
// is the CLI's own default level — so an install that never visits the
// settings page behaves exactly as it did before --effort existed.
func TestEffortConfigDefaults(t *testing.T) {
	d := newTestDB(t)
	svc := NewSettingService(d)

	cfg, err := svc.EffortConfig()
	if err != nil {
		t.Fatalf("EffortConfig: %v", err)
	}
	if !cfg.Enabled {
		t.Error("effort.enabled = false, want true (the master switch ships on)")
	}
	if cfg.Design != llm.EffortHigh {
		t.Errorf("effort.design = %q, want %q", cfg.Design, llm.EffortHigh)
	}
	if cfg.Coding != llm.EffortMedium {
		t.Errorf("effort.coding = %q, want %q", cfg.Coding, llm.EffortMedium)
	}
	if cfg.Commit != llm.EffortLow {
		t.Errorf("effort.commit = %q, want %q", cfg.Commit, llm.EffortLow)
	}
	if cfg.Other != llm.EffortHigh {
		t.Errorf("effort.other = %q, want %q", cfg.Other, llm.EffortHigh)
	}
}

// TestEffortConfigRoundTrip pins the write → read cycle.
func TestEffortConfigRoundTrip(t *testing.T) {
	d := newTestDB(t)
	svc := NewSettingService(d)

	if err := svc.SetEffortConfig(EffortConfig{
		Enabled: false,
		Design:  llm.EffortMax,
		Coding:  llm.EffortLow,
		Commit:  llm.EffortMedium,
		Other:   llm.EffortXHigh,
	}); err != nil {
		t.Fatalf("SetEffortConfig: %v", err)
	}

	cfg, err := svc.EffortConfig()
	if err != nil {
		t.Fatalf("EffortConfig: %v", err)
	}
	if cfg.Enabled {
		t.Error("effort.enabled = true, want false")
	}
	if cfg.Design != llm.EffortMax {
		t.Errorf("effort.design = %q, want %q", cfg.Design, llm.EffortMax)
	}
	if cfg.Coding != llm.EffortLow {
		t.Errorf("effort.coding = %q, want %q", cfg.Coding, llm.EffortLow)
	}
	if cfg.Commit != llm.EffortMedium {
		t.Errorf("effort.commit = %q, want %q", cfg.Commit, llm.EffortMedium)
	}
	if cfg.Other != llm.EffortXHigh {
		t.Errorf("effort.other = %q, want %q", cfg.Other, llm.EffortXHigh)
	}
}

// TestSetEffortConfigRejectsUnknownTier pins the write-side validation.
//
// An unknown tier falls back to that stage's SHIPPED default rather than
// being persisted. Persisting it would put a value on the wire that the
// run sites would each have to defend against, and the CLI aborts the
// whole run on an unknown --effort value.
func TestSetEffortConfigRejectsUnknownTier(t *testing.T) {
	d := newTestDB(t)
	svc := NewSettingService(d)

	if err := svc.SetEffortConfig(EffortConfig{
		Enabled: true,
		Design:  "ultra",
		Coding:  "",
		Commit:  "",
		Other:   "",
	}); err != nil {
		t.Fatalf("SetEffortConfig: %v", err)
	}

	cfg, err := svc.EffortConfig()
	if err != nil {
		t.Fatalf("EffortConfig: %v", err)
	}
	if cfg.Design != llm.DefaultEffortDesign {
		t.Errorf("effort.design = %q, want the shipped default %q", cfg.Design, llm.DefaultEffortDesign)
	}
	if cfg.Coding != llm.DefaultEffortCoding {
		t.Errorf("effort.coding = %q, want the shipped default %q", cfg.Coding, llm.DefaultEffortCoding)
	}
	if cfg.Commit != llm.DefaultEffortCommit {
		t.Errorf("effort.commit = %q, want the shipped default %q", cfg.Commit, llm.DefaultEffortCommit)
	}
	if cfg.Other != llm.DefaultEffortOther {
		t.Errorf("effort.other = %q, want the shipped default %q", cfg.Other, llm.DefaultEffortOther)
	}
}

// TestEffortDefaultsFeedsTheGateway pins the provider contract the gateway
// consumes.
//
// The gateway's decision chain is "user pick > configured stage default >
// built-in default", and the master switch short-circuits everything. The
// provider therefore has to hand the gateway BOTH the switch and the four
// per-stage tiers, and it has to report ok=true when it could read them.
func TestEffortDefaultsFeedsTheGateway(t *testing.T) {
	d := newTestDB(t)
	svc := NewSettingService(d)

	if err := svc.SetEffortConfig(EffortConfig{
		Enabled: true,
		Design:  llm.EffortXHigh,
		Coding:  llm.EffortLow,
		Commit:  llm.EffortMedium,
		Other:   llm.EffortMax,
	}); err != nil {
		t.Fatalf("SetEffortConfig: %v", err)
	}

	gw := llm.New(nil, nil)
	gw.SetEffortProvider(svc)

	// The configured stage default applies when nothing was picked.
	if got := gw.ResolveEffort("", llm.StageDesign); got != llm.EffortXHigh {
		t.Errorf("design tier = %q, want %q", got, llm.EffortXHigh)
	}
	if got := gw.ResolveEffort("", llm.StageCoding); got != llm.EffortLow {
		t.Errorf("coding tier = %q, want %q", got, llm.EffortLow)
	}
	if got := gw.ResolveEffort("", llm.StageCommit); got != llm.EffortMedium {
		t.Errorf("commit tier = %q, want %q", got, llm.EffortMedium)
	}
	if got := gw.ResolveEffort("", llm.StageOther); got != llm.EffortMax {
		t.Errorf("other tier = %q, want %q", got, llm.EffortMax)
	}
	// An explicit pick still wins.
	if got := gw.ResolveEffort(llm.EffortHigh, llm.StageDesign); got != llm.EffortHigh {
		t.Errorf("explicit pick = %q, want %q", got, llm.EffortHigh)
	}

	// Turning the master switch off suppresses EVERYTHING, including an
	// explicit pick — it is the single escape hatch.
	if err := svc.SetEffortConfig(EffortConfig{
		Enabled: false,
		Design:  llm.EffortXHigh,
		Coding:  llm.EffortLow,
		Commit:  llm.EffortMedium,
		Other:   llm.EffortMax,
	}); err != nil {
		t.Fatalf("SetEffortConfig: %v", err)
	}
	if got := gw.ResolveEffort(llm.EffortHigh, llm.StageDesign); got != "" {
		t.Errorf("tier with the switch off = %q, want \"\"", got)
	}
}
