package handler

import (
	"errors"
	"testing"
)

// TestShouldRetryProvision pins the retry trigger for the "首次 provision
// 未输出 keyid" cold-start failure mode (see the technical plan for
// req_499ee84d2a3853f3). The retry fires ONLY when the first attempt
// exited cleanly (exitCode == 0, no transport error) but the marker parser
// could not find a 16+ hex keyid — i.e. the "重做就有" case. Every other
// failure mode (import failure, git config failure, SSH error) must NOT
// trigger a retry, because a retry will not paper over a real gpg or git
// error and would just delay surfacing it to the user.
func TestShouldRetryProvision(t *testing.T) {
	cases := []struct {
		name     string
		keyID    string
		exitCode int
		runErr   error
		want     bool
	}{
		{
			name:     "exit0 + empty keyid + no error → retry (cold-start race)",
			keyID:    "",
			exitCode: 0,
			runErr:   nil,
			want:     true,
		},
		{
			name:     "exit0 + valid keyid → no retry (happy path)",
			keyID:    "ABCDEF0123456789ABCDEF0123456789",
			exitCode: 0,
			runErr:   nil,
			want:     false,
		},
		{
			name:     "exit1 + empty keyid → no retry (gpg import or git config error)",
			keyID:    "",
			exitCode: 1,
			runErr:   nil,
			want:     false,
		},
		{
			name:     "exit2 + empty keyid → no retry (real failure, not race)",
			keyID:    "",
			exitCode: 2,
			runErr:   nil,
			want:     false,
		},
		{
			name:     "exit0 + empty keyid + transport error → no retry (transport issue, not race)",
			keyID:    "",
			exitCode: 0,
			runErr:   errors.New("ssh: connection closed"),
			want:     false,
		},
		{
			name:     "ctx cancelled → no retry (don't keep poking a dead session)",
			keyID:    "",
			exitCode: 0,
			runErr:   errors.New("context canceled"),
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRetryProvision(tc.keyID, tc.exitCode, tc.runErr); got != tc.want {
				t.Fatalf("shouldRetryProvision(keyID=%q exitCode=%d runErr=%v) = %v, want %v",
					tc.keyID, tc.exitCode, tc.runErr, got, tc.want)
			}
		})
	}
}
