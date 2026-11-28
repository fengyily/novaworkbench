package service

import "testing"

// TestOutputLimitAutoResumeDefault pins the shipped budget.
//
// One automatic resume is the shipped behaviour: enough to recover from a
// single oversized Write, small enough that a model that keeps trying to
// emit one huge file cannot loop.
func TestOutputLimitAutoResumeDefault(t *testing.T) {
	d := newTestDB(t)
	svc := NewSettingService(d)

	n, err := svc.OutputLimitAutoResume()
	if err != nil {
		t.Fatalf("OutputLimitAutoResume: %v", err)
	}
	if n != DefaultOutputLimitAutoResume {
		t.Errorf("default = %d, want %d", n, DefaultOutputLimitAutoResume)
	}
}

// TestOutputLimitAutoResumeRoundTrip pins the write → read cycle for the
// values an operator can pick in the UI.
func TestOutputLimitAutoResumeRoundTrip(t *testing.T) {
	d := newTestDB(t)
	svc := NewSettingService(d)

	for _, want := range []int{0, 1, 2, 3} {
		if err := svc.SetOutputLimitAutoResume(want); err != nil {
			t.Fatalf("SetOutputLimitAutoResume(%d): %v", want, err)
		}
		got, err := svc.OutputLimitAutoResume()
		if err != nil {
			t.Fatalf("OutputLimitAutoResume: %v", err)
		}
		if got != want {
			t.Errorf("round trip = %d, want %d", got, want)
		}
	}
}

// TestOutputLimitAutoResumeZeroDisables pins that 0 is a MEANINGFUL value
// (recovery off), not "unset" — otherwise an operator who turns the recovery
// off would silently get the default back.
func TestOutputLimitAutoResumeZeroDisables(t *testing.T) {
	d := newTestDB(t)
	svc := NewSettingService(d)

	if err := svc.SetOutputLimitAutoResume(0); err != nil {
		t.Fatalf("SetOutputLimitAutoResume(0): %v", err)
	}
	got, err := svc.OutputLimitAutoResume()
	if err != nil {
		t.Fatalf("OutputLimitAutoResume: %v", err)
	}
	if got != 0 {
		t.Errorf("value = %d, want 0 (0 = recovery off)", got)
	}
}

// TestOutputLimitAutoResumeClamps pins the [0, 3] envelope at read time:
// a row written by an older / hand-edited install must not be able to make
// the coding stage resume a turn an unbounded number of times.
func TestOutputLimitAutoResumeClamps(t *testing.T) {
	d := newTestDB(t)
	svc := NewSettingService(d)

	// Write past the ceiling directly, bypassing the setter's clamp.
	if err := svc.Set(settingCodingOutputLimitAutoResume, "99"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := svc.OutputLimitAutoResume()
	if err != nil {
		t.Fatalf("OutputLimitAutoResume: %v", err)
	}
	if got != maxOutputLimitAutoResume {
		t.Errorf("value = %d, want %d (clamped)", got, maxOutputLimitAutoResume)
	}

	// A garbage value falls back to the shipped default rather than 0 —
	// "unreadable" must not be mistaken for "recovery off".
	if err := svc.Set(settingCodingOutputLimitAutoResume, "abc"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err = svc.OutputLimitAutoResume()
	if err != nil {
		t.Fatalf("OutputLimitAutoResume: %v", err)
	}
	if got != DefaultOutputLimitAutoResume {
		t.Errorf("value = %d, want %d (default on garbage)", got, DefaultOutputLimitAutoResume)
	}
}
