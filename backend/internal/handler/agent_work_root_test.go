package handler

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoteWorkRootProbeScript guards the shell contract behind install-time
// work-root provisioning: the script must create the directory, prove the
// current SSH user can write into it, and report the account so the Go error
// can tell the operator exactly whom to chown. Dropping any step would let a
// read-only root slip through and surface as `git clone` EACCES during a
// development run.
func TestRemoteWorkRootProbeScript(t *testing.T) {
	s := remoteWorkRootProbeScript("/var/lib/nova-agent")

	for _, want := range []string{
		`root='/var/lib/nova-agent'`, // path is shell-quoted
		"id -un",                     // names the account in the failure guidance
		"mkdir -p",                   // creates a missing root
		"exit 11",                    // mkdir failure
		"exit 12",                    // exists but not a directory
		"nova-write-probe",           // write test
		"exit 13",                    // not writable
		"ls -ld",                     // owner/mode diagnostics on success
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("remoteWorkRootProbeScript missing %q;\ngot:\n%s", want, s)
		}
	}
}

// TestRemoteWorkRootProbeScriptQuotesRoot makes sure a root containing a
// single quote cannot break out of the shell quoting.
func TestRemoteWorkRootProbeScriptQuotesRoot(t *testing.T) {
	s := remoteWorkRootProbeScript("/data/it's-here")
	if !strings.Contains(s, `root='/data/it'\''s-here'`) {
		t.Fatalf("root is not safely quoted;\ngot:\n%s", s)
	}
}

// TestRemoteWorkRootProbeScriptExec runs the generated shell against a real
// local filesystem: a missing directory is created and verified, and a plain
// file sitting at the configured path is rejected with exit 12 instead of
// being treated as a usable root.
func TestRemoteWorkRootProbeScriptExec(t *testing.T) {
	tmp := t.TempDir()

	root := filepath.Join(tmp, "nested", "nova-agent")
	out, err := exec.Command("sh", "-c", remoteWorkRootProbeScript(root)).CombinedOutput()
	if err != nil {
		t.Fatalf("probe script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "NOVA_USER=") {
		t.Fatalf("probe script missing NOVA_USER marker:\n%s", out)
	}
	if fi, statErr := os.Stat(root); statErr != nil || !fi.IsDir() {
		t.Fatalf("work root was not created: err=%v", statErr)
	}

	notDir := filepath.Join(tmp, "not-a-dir")
	if werr := os.WriteFile(notDir, []byte("x"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	out, err = exec.Command("sh", "-c", remoteWorkRootProbeScript(notDir)).CombinedOutput()
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 12 {
		t.Fatalf("expected exit 12 for a non-directory root, got err=%v\n%s", err, out)
	}
}

// TestPreCreateWorkRootScriptOnlyTouchesMissingRoots pins the safety property
// of the root-phase pre-create: an existing directory is never re-owned (it
// may point at shared storage), and only a freshly created one is handed to
// the worker user.
func TestPreCreateWorkRootScriptOnlyTouchesMissingRoots(t *testing.T) {
	s := preCreateWorkRootScript("/var/lib/nova-agent", "nova")

	for _, want := range []string{
		`root='/var/lib/nova-agent'`,
		`[ -e "$root" ]`, // existing path short-circuits before mkdir/chown
		"mkdir -p",
		"chown 'nova':'nova'",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("preCreateWorkRootScript missing %q;\ngot:\n%s", want, s)
		}
	}
	if strings.Index(s, `[ -e "$root" ]`) > strings.Index(s, "chown") {
		t.Fatalf("existence check must precede chown;\ngot:\n%s", s)
	}
}
