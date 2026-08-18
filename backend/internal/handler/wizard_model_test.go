package handler

import "testing"

func TestEffectiveModelFromValues(t *testing.T) {
	cases := []struct {
		name             string
		roleModel        string
		configDefault    string
		want             string
		wantIsCLIDefault bool
	}{
		{"both empty -> 默认模型 literal", "", "", DefaultModelLabel, true},
		{"role empty, config set -> config default", "", "claude-sonnet-4-5", "claude-sonnet-4-5", false},
		{"role set, config empty -> role override", "claude-opus-4-1", "", "claude-opus-4-1", false},
		{"both set -> role wins", "claude-opus-4-1", "claude-sonnet-4-5", "claude-opus-4-1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveModelFromValues(c.roleModel, c.configDefault)
			if got != c.want {
				t.Fatalf("effectiveModelFromValues(%q,%q) = %q, want %q", c.roleModel, c.configDefault, got, c.want)
			}
			// The persisted/displayed value must map back to the right CLI arg:
			// the "默认模型" sentinel -> "" (no --model), everything else verbatim.
			cliArg := cliModelArg(got)
			if c.wantIsCLIDefault && cliArg != "" {
				t.Fatalf("cliModelArg(%q) = %q, want \"\" (no --model flag)", got, cliArg)
			}
			if !c.wantIsCLIDefault && cliArg != c.want {
				t.Fatalf("cliModelArg(%q) = %q, want %q", got, cliArg, c.want)
			}
		})
	}
}

// TestDefaultModelLabelStable guards the display literal — changing it would
// silently corrupt persisted DB values already written as "默认模型".
func TestDefaultModelLabelStable(t *testing.T) {
	if DefaultModelLabel != "默认模型" {
		t.Fatalf("DefaultModelLabel = %q, want \"默认模型\"", DefaultModelLabel)
	}
}
