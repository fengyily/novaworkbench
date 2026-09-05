package handler

import (
	"strings"
	"testing"
)

// TestNonRootProvisionScript guards the shell contract behind the one-click
// "run the agent worker as non-root" fix. provisionNonRootUser is executed as
// root over SSH; if the script drops any of these steps the re-dial as the new
// user fails (no authorized_keys / no linger / wrong owner) and the install
// surfaces a confusing "switch user failed" instead of the actual missing step.
func TestNonRootProvisionScript(t *testing.T) {
	s := nonRootProvisionScript("nova")

	for _, want := range []string{
		"useradd",                // primary creation path
		"adduser",                // Debian-family fallback
		"nova",                   // the username is baked into every command
		"enable-linger",          // keeps systemd --user alive across SSH sessions
		"authorized_keys",        // hands the new user the same key root accepts
		"chown",                  // makes the new user own its ~/.ssh tree
		"getent passwd",          // resolves the new user's real home dir
		"mkdir -p",               // stages ~/.ssh before the copy
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("nonRootProvisionScript(\"nova\") missing %q;\ngot:\n%s", want, s)
		}
	}
}
