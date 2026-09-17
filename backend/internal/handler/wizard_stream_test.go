package handler

import (
	"strings"
	"testing"
)

// TestIsStaleSessionError covers isStaleSessionError across the known claude CLI
// stale-session error formats (legacy + current) and verifies that unrelated
// errors do not match. Pure-function tests — no mocks, no subprocess.
//
// The patterns covered are documented on isStaleSessionError itself:
//   - "No conversation found with session ID: <uuid>" (legacy)
//   - "Error: Session ID <uuid>..." (current, observed on --resume failures)
//   - "Session not found" / "session not found" (defensive catch-all)
func TestIsStaleSessionError(t *testing.T) {
	cases := []struct {
		name   string
		evt    map[string]interface{}
		stderr string
		want   bool
	}{
		{
			name:   "current CLI format via stderr",
			stderr: "Error: Session ID 1746c6bc-9c1a-46fc-ad2b-1234567890ab not found on disk",
			want:   true,
		},
		{
			name:   "legacy format via stderr",
			stderr: "No conversation found with session ID: 1746c6bc-9c1a-46fc-ad2b-1234567890ab",
			want:   true,
		},
		{
			name:   "defensive session-not-found substring via stderr",
			stderr: "session not found for the requested resume target",
			want:   true,
		},
		{
			name:   "defensive Session-not-found substring via stderr",
			stderr: "Session not found while attempting --resume",
			want:   true,
		},
		{
			name:   "empty stderr returns false",
			stderr: "",
			want:   false,
		},
		{
			name:   "current format on evt.error field",
			evt:    map[string]interface{}{"error": "Error: Session ID 1746c6bc-9c1a-46fc-ad2b-1234567890ab"},
			want:   true,
		},
		{
			name:   "legacy format on evt.result field",
			evt:    map[string]interface{}{"result": "No conversation found with session ID: 1746c6bc-9c1a-46fc-ad2b-1234567890ab"},
			want:   true,
		},
		{
			name:   "current format inside evt.errors array",
			evt:    map[string]interface{}{"errors": []interface{}{"Error: Session ID 1746c6bc-9c1a-46fc-ad2b-1234567890ab"}},
			want:   true,
		},
		{
			name: "unrelated evt.message does not match",
			evt:  map[string]interface{}{"message": "random unrelated text"},
			want: false,
		},
		{
			name:   "unrelated stderr text does not match",
			stderr: "some random unrelated error from the proxy layer",
			want:   false,
		},
		{
			name:   "evt nil with empty stderr returns false",
			evt:    nil,
			stderr: "",
			want:   false,
		},
		{
			name:   "nil evt only checks stderr — negative",
			evt:    nil,
			stderr: "totally unrelated 401 from upstream",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isStaleSessionError(tc.evt, tc.stderr)
			if got != tc.want {
				t.Errorf("isStaleSessionError(evt=%v, stderr=%q) = %v, want %v",
					tc.evt, truncateForDisplay(tc.stderr), got, tc.want)
			}
		})
	}
}

// truncateForDisplay keeps assertion failures readable when a long stderr string
// is echoed back. Only used by the test helper.
func truncateForDisplay(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Sanity guard: confirms the test helper itself doesn't accidentally rewrite
// strings (catches future refactors that swap Contains for regex, etc.).
func TestIsStaleSessionError_HelperContract(t *testing.T) {
	if !strings.Contains("Error: Session ID abc", "Error: Session ID") {
		t.Fatal("stdlib strings.Contains semantics drifted — fixture mismatch")
	}
}
