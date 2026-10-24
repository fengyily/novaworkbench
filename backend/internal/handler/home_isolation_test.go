package handler

import (
	"os"
	"path/filepath"
	"testing"
)

// Package-level HOME isolation.
//
// WHY THIS EXISTS
//
// Until 2026-10-24 the handler test suite reached into the developer's REAL
// home directory. Three cases in fs_test.go built
//
//	workspace := filepath.Join(os.UserHomeDir(), "workspace")   // /Users/<me>/workspace
//	os.MkdirAll(workspace, 0755)
//	defer os.RemoveAll(workspace)
//
// so every `go test ./backend/internal/handler/` silently deleted the real
// ~/workspace — including every project checkout registered under it. On
// 2026-10-24 that wiped 7 live repositories (vital-ble-sdk, vivalink-sdk,
// viva-ai, Vivalink_UnifySDK, v2-KB, shield-plugins, videocutbot) in the
// middle of a running sub-task, whose Claude child then spent 11 minutes
// trying to re-clone a repo it could no longer reach.
//
// The fix is two-layered:
//
//  1. This TestMain redirects HOME/USERPROFILE to a throwaway directory for
//     the whole test binary. Any test (current or future) that calls
//     os.UserHomeDir() gets the sandbox, so it is physically incapable of
//     deleting anything the user cares about. It also stops the suite from
//     polluting the real ~/.novaworkbench/worktrees/ tree.
//
//  2. The individual tests that used to construct the real home now call
//     isolatedHome(t) so they stay correct even if this TestMain is removed.
//
// NOTE: os.UserHomeDir() reads $HOME on Unix and %USERPROFILE% on Windows —
// both are set so the sandbox holds on either platform.

func TestMain(m *testing.M) {
	// Capture the developer's real home BEFORE redirecting — the canary test
	// below compares against it to prove the sandbox is in effect.
	realUserHome, _ = os.UserHomeDir()

	home, err := os.MkdirTemp("", "nova-test-home-*")
	if err != nil {
		// Without a sandbox there is no safe way to run the suite; fail loudly
		// rather than let a test touch the developer's real home directory.
		panic("nova: cannot create sandbox HOME for tests: " + err.Error())
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home) // Windows

	code := m.Run()

	_ = os.RemoveAll(home)
	os.Exit(code)
}

// realUserHome is the developer's actual home directory, captured by TestMain
// before HOME was redirected to the sandbox. Empty when TestMain did not run
// (e.g. the test binary was invoked in a way that skipped it).
var realUserHome string

// isolatedHome points HOME/USERPROFILE at a per-test temporary directory and
// returns it. Tests that need to materialise a "home"-shaped tree (e.g.
// ~/workspace) must use this instead of os.UserHomeDir().
//
// The directory is removed automatically when the test ends.
//
// t.Setenv is used rather than os.Setenv so the previous value is restored
// after the test; it also fails the test if the test calls t.Parallel(), which
// is the desired guard (env mutation + parallel tests don't mix).
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows
	return home
}

// TestHomeIsolationIsInEffect is the canary for the net above: if HOME ever
// stops pointing at the sandbox, every other test in this package becomes
// capable of deleting the developer's real ~/workspace again. Failing here
// loudly is much cheaper than discovering it the way we did on 2026-10-24.
func TestHomeIsolationIsInEffect(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if realUserHome == "" {
		t.Fatalf("TestMain did not record the real home — the sandbox cannot be verified")
	}
	if home == realUserHome {
		t.Fatalf("HOME is not redirected: UserHomeDir()=%q (the real home) — the test suite would touch the developer's real data", home)
	}
	// The sandbox must live under the OS temp dir so it is swept by the OS
	// and can never collide with a real path.
	if !isTempDir(home) {
		t.Errorf("sandbox HOME=%q is not under the OS temp dir (%q) — verify TestMain set it", home, os.TempDir())
	}
}

// isTempDir reports whether path lives under the OS temp directory.
func isTempDir(path string) bool {
	tmp := os.TempDir()
	if tmp == "" {
		return false
	}
	rel, err := filepath.Rel(tmp, path)
	if err != nil {
		return false
	}
	return rel != ".." && !filepath.IsAbs(rel) && len(rel) > 0 && rel[0] != '.'
}
