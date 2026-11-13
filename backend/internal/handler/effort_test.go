package handler

import (
	"testing"

	"github.com/novaworkbench/backend/internal/llm"
)

// stubEffortProvider is a scripted llm.EffortProvider.
type stubEffortProvider struct {
	enabled                       bool
	design, coding, commit, other string
}

func (s stubEffortProvider) EffortDefaults() (bool, string, string, string, string, bool) {
	return s.enabled, s.design, s.coding, s.commit, s.other, true
}

// TestResolveEffortUsesTheConfiguredStageDefault pins the contract the
// frontend depends on.
//
// The frontend sends a reasoning-effort tier ONLY when the user actually
// picked one — an unpicked request leaves the field off. That is only
// correct because "no tier on the wire" resolves to the CONFIGURED stage
// default, which is the operator's setting.
//
// If this ever regressed to the BUILT-IN default, every unpicked run would
// silently ignore the configured policy — the frontend would look right
// (it shows the configured tier) and the backend would run something else.
// That is the exact split-brain this chain exists to prevent.
func TestResolveEffortUsesTheConfiguredStageDefault(t *testing.T) {
	gw := llm.New(nil, nil)
	gw.SetEffortProvider(stubEffortProvider{
		enabled: true,
		design:  llm.EffortMax,
		coding:  llm.EffortLow,
		commit:  llm.EffortMedium,
		other:   llm.EffortXHigh,
	})

	cases := map[string]struct {
		stage string
		want  string
	}{
		"design": {llm.StageDesign, llm.EffortMax},
		"coding": {llm.StageCoding, llm.EffortLow},
		"commit": {llm.StageCommit, llm.EffortMedium},
		"other":  {llm.StageOther, llm.EffortXHigh},
	}
	for name, c := range cases {
		// An EMPTY pick is what an omitted request field decodes to.
		if got := resolveEffort(gw, "", c.stage); got != c.want {
			t.Errorf("%s: resolveEffort(\"\", %q) = %q, want the configured %q",
				name, c.stage, got, c.want)
		}
	}
}

// TestResolveEffortKeepsTheExplicitPick pins the other half of the
// contract: a tier the user DID pick still wins over the configured
// default. The frontend's "omit unless picked" rule only preserves the
// backend's authority because the pick still outranks it.
func TestResolveEffortKeepsTheExplicitPick(t *testing.T) {
	gw := llm.New(nil, nil)
	gw.SetEffortProvider(stubEffortProvider{enabled: true, design: llm.EffortMax})

	if got := resolveEffort(gw, llm.EffortLow, llm.StageDesign); got != llm.EffortLow {
		t.Errorf("resolveEffort(low, design) = %q, want %q", got, llm.EffortLow)
	}
}

// TestResolveEffortNilGatewayFallsBackToTheBuiltInDefault pins that a
// partially-wired handler degrades instead of panicking.
//
// Several handlers are constructed in tests without a gateway. A nil
// dereference there would turn a missing dependency into a 500 on every
// request; the built-in default is the right degradation.
func TestResolveEffortNilGatewayFallsBackToTheBuiltInDefault(t *testing.T) {
	cases := map[string]string{
		llm.StageDesign: llm.DefaultEffortDesign,
		llm.StageCoding: llm.DefaultEffortCoding,
		llm.StageCommit: llm.DefaultEffortCommit,
		llm.StageOther:  llm.DefaultEffortOther,
	}
	for stage, want := range cases {
		if got := resolveEffort(nil, "", stage); got != want {
			t.Errorf("resolveEffort(nil, \"\", %q) = %q, want the built-in %q", stage, got, want)
		}
	}
}
