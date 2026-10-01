package handler

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestVerifyImportedGPGKey_HappyPath spins up a brand-new gpg key in a
// throwaway temp dir, exports the ASCII-armored private block, and feeds
// that block to verifyImportedGPGKey. We expect the same key id / fpr /
// uid to come back (modulo the `--quick-generate-key` UID string gpg
// normalises to `Real Name <email>`).
//
// Skipped when gpg is not on PATH — the CI box without gnupg would
// otherwise produce a permanent "no gpg" failure rather than a clean
// skip that the operator can spot at a glance.
func TestVerifyImportedGPGKey_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed on this host; skipping verifyImportedGPGKey integration test")
	}

	// Build a temporary GNUPGHOME for the setup side. We reuse the
	// buildGPGGenerateScript helper so the test exercises exactly the
	// script body we ship in production.
	setupHome := t.TempDir()
	if err := os.Chmod(setupHome, 0700); err != nil {
		t.Fatalf("chmod setupHome: %v", err)
	}
	setupScript := buildGPGGenerateScript(setupHome, "Verify Test <verify@example.com>")
	t.Logf("script:\n%s", setupScript)
	// Show what env we have for diagnostic
	for _, kv := range os.Environ() {
		if len(kv) > 4 && (kv[:4] == "GPG_" || kv[:4] == "GNUP") {
			t.Logf("INHERITED ENV: %s", kv)
		}
	}
	cmd := exec.Command("/bin/sh", "-c", setupScript)
	// Strip any inherited GNUPGHOME so the new keyring really is fresh.
	cmd.Env = append(filteredEnv("GNUPGHOME"), "GNUPGHOME="+setupHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup --gen-key failed: %v\n%s", err, out)
	}
	setupKeyID, setupFPR, setupUID := parseGPGKeyInfoFromScriptOutput(string(out))
	if setupKeyID == "" {
		t.Fatalf("setup script did not emit NOVA_GPG_KEYID; output:\n%s", out)
	}

	// Read the armored private block the setup produced.
	armored, err := os.ReadFile(setupHome + "/private.asc")
	if err != nil {
		t.Fatalf("read private.asc: %v", err)
	}
	if len(armored) == 0 {
		t.Fatalf("private.asc is empty; quick-generate-key produced no key?")
	}

	// Now feed the armored block into verifyImportedGPGKey. We expect
	// the SAME key id / fpr / uid to come back.
	gotKeyID, gotFPR, gotUID, err := verifyImportedGPGKey(string(armored), "")
	if err != nil {
		t.Fatalf("verifyImportedGPGKey returned error: %v", err)
	}
	if gotKeyID != setupKeyID {
		t.Errorf("key id round-trip mismatch: got %q, want %q", gotKeyID, setupKeyID)
	}
	if gotFPR != setupFPR {
		t.Errorf("fingerprint round-trip mismatch: got %q, want %q", gotFPR, setupFPR)
	}
	if gotUID != setupUID {
		t.Errorf("UID round-trip mismatch: got %q, want %q", gotUID, setupUID)
	}

	// Sanity: key id must be 16 hex chars; fpr must be 40 hex chars.
	if !regexp.MustCompile(`^[0-9A-F]{16}$`).MatchString(gotKeyID) {
		t.Errorf("key id does not look like 16 uppercase hex: %q", gotKeyID)
	}
	if !regexp.MustCompile(`^[0-9A-F]{40}$`).MatchString(gotFPR) {
		t.Errorf("fingerprint does not look like 40 uppercase hex: %q", gotFPR)
	}
}

// TestVerifyImportedGPGKey_CorruptedBlock ensures that an armored block
// with junk in the body (simulating a paste mishap) makes
// verifyImportedGPGKey return a non-nil error rather than silently
// parsing junk.
func TestVerifyImportedGPGKey_CorruptedBlock(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed on this host; skipping verifyImportedGPGKey corruption test")
	}

	// Feed a fake-but-recognisable armored block with the BEGIN/END
	// markers but junk body. gpg's --import should refuse.
	bogus := "-----BEGIN PGP PRIVATE KEY BLOCK-----\n" +
		"this is not a real base64 pgp packet\n" +
		"=\n" +
		"-----END PGP PRIVATE KEY BLOCK-----\n"
	_, _, _, err := verifyImportedGPGKey(bogus, "")
	if err == nil {
		t.Fatalf("verifyImportedGPGKey accepted a bogus armored block")
	}
}

// filteredEnv returns os.Environ() with every entry whose KEY matches
// dropKey removed (case-sensitive). Used by integration tests to scrub
// an inherited GNUPGHOME / GPG_AGENT_INFO that would otherwise steer
// gpg at the developer's interactive keyring.
func filteredEnv(dropKey string) []string {
	out := make([]string, 0, 32)
	prefix := dropKey + "="
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}