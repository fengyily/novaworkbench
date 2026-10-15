package handler

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The agent host can only resolve what origin knows, so an unpushed branch is
// carried over as a git bundle. These tests pin the two properties the remote
// review depends on: the incremental bundle is fetchable by a clone of the
// same origin (that clone IS the agent host's scratch repo), and the
// self-contained fallback covers the cases where it isn't.

// gitOut runs git and returns trimmed stdout plus the error, so a test can
// probe calls that are expected to fail.
func gitOut(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(out.String()), err
	}
	return strings.TrimSpace(out.String()), nil
}

// originClone stands in for the agent host's /tmp/nova-agent/<pid>/base: a
// plain clone of the project's origin, nothing else.
func originClone(t *testing.T, work string) string {
	t.Helper()
	root := filepath.Dir(work)
	agent := filepath.Join(root, "agent")
	gitIn(t, "", "clone", filepath.Join(root, "origin.git"), agent)
	return agent
}

func TestCreateReviewBundle_IncrementalLandsOnOriginClone(t *testing.T) {
	work := newRefsFixture(t)
	commitOnBranch(t, work, "feat/local", "a.txt")
	agent := originClone(t, work)

	bundle := filepath.Join(t.TempDir(), "r.bundle")
	if err := createReviewBundle(work, bundle, []string{"refs/heads/feat/local"}, true); err != nil {
		t.Fatalf("create incremental bundle: %v", err)
	}

	const remoteRef = "refs/nova-review/crv_test/head"
	if _, err := gitOut(t, agent, "fetch", bundle, "+refs/heads/feat/local:"+remoteRef); err != nil {
		t.Fatalf("agent clone must be able to fetch the incremental bundle: %v", err)
	}
	got, err := gitOut(t, agent, "rev-parse", remoteRef)
	if err != nil {
		t.Fatalf("rev-parse on agent: %v", err)
	}
	want, err := gitOut(t, work, "rev-parse", "feat/local")
	if err != nil {
		t.Fatalf("rev-parse locally: %v", err)
	}
	if got != want {
		t.Fatalf("shipped ref = %s, want %s", got, want)
	}

	// The agent clone's own branches must be untouched — we never fetch into
	// refs/heads/*, so HEAD and the checked-out branch stay where they were.
	if _, err := gitOut(t, agent, "rev-parse", "--verify", "refs/heads/feat/local"); err == nil {
		t.Fatal("bundle fetch must not create refs/heads/feat/local on the agent clone")
	}
}

// Incremental means incremental: the bundle carries only what origin lacks,
// so a repo without origin's history cannot consume it. That is exactly why
// the caller falls back to a self-contained bundle when the remote fetch
// fails.
func TestCreateReviewBundle_IncrementalNeedsOriginHistory(t *testing.T) {
	work := newRefsFixture(t)
	commitOnBranch(t, work, "feat/local", "a.txt")

	bundle := filepath.Join(t.TempDir(), "r.bundle")
	if err := createReviewBundle(work, bundle, []string{"refs/heads/feat/local"}, true); err != nil {
		t.Fatalf("create incremental bundle: %v", err)
	}

	empty := filepath.Join(t.TempDir(), "empty")
	gitIn(t, "", "init", "--initial-branch=main", empty)
	if _, err := gitOut(t, empty, "fetch", bundle, "+refs/heads/feat/local:refs/nova-review/x/head"); err == nil {
		t.Fatal("expected the incremental bundle to be unusable without origin's objects")
	}

	// ...and the self-contained shape fixes precisely that.
	full := filepath.Join(t.TempDir(), "full.bundle")
	if err := createReviewBundle(work, full, []string{"refs/heads/feat/local"}, false); err != nil {
		t.Fatalf("create full bundle: %v", err)
	}
	if _, err := gitOut(t, empty, "fetch", full, "+refs/heads/feat/local:refs/nova-review/x/head"); err != nil {
		t.Fatalf("full bundle must be self-contained: %v", err)
	}
}

// A local branch whose commits are all already in origin (renamed branch,
// branch left behind after a merge) yields an empty incremental range, and
// git refuses to bundle that. The caller treats the failure as "retry with
// the full history", so the only thing to pin here is that the full shape
// still produces a usable bundle.
func TestCreateReviewBundle_EmptyIncrementalRangeFallsBackToFull(t *testing.T) {
	work := newRefsFixture(t)
	gitIn(t, work, "branch", "dup", "main")

	bundle := filepath.Join(t.TempDir(), "r.bundle")
	if err := createReviewBundle(work, bundle, []string{"refs/heads/dup"}, true); err == nil {
		t.Fatal("expected git to refuse an empty incremental bundle")
	}

	agent := originClone(t, work)
	full := filepath.Join(t.TempDir(), "full.bundle")
	if err := createReviewBundle(work, full, []string{"refs/heads/dup"}, false); err != nil {
		t.Fatalf("create full bundle: %v", err)
	}
	if _, err := gitOut(t, agent, "fetch", full, "+refs/heads/dup:refs/nova-review/crv_test/head"); err != nil {
		t.Fatalf("fetch full bundle: %v", err)
	}
	got, _ := gitOut(t, agent, "rev-parse", "refs/nova-review/crv_test/head")
	want, _ := gitOut(t, work, "rev-parse", "dup")
	if got != want || got == "" {
		t.Fatalf("shipped ref = %q, want %q", got, want)
	}
}

func TestDirExists(t *testing.T) {
	dir := t.TempDir()
	if !dirExists(dir) {
		t.Fatal("temp dir should exist")
	}
	if dirExists(filepath.Join(dir, "nope")) {
		t.Fatal("missing dir reported as existing")
	}
	if dirExists("") {
		t.Fatal("empty path reported as existing")
	}
}

func TestMissingBranchNames(t *testing.T) {
	got := missingBranchNames([]*reviewRefPlan{{name: "feat/x"}, {name: "main"}})
	if got != "分支 feat/x / 分支 main" {
		t.Fatalf("got %q", got)
	}
}
