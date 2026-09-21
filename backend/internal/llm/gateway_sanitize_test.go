package llm

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSanitizeExecStrings covers the NUL-stripping behavior at the exec
// boundary. A NUL byte anywhere in argv/env would make exec.Cmd.Start()
// fail with EINVAL (syscall.ByteSliceFromString rejects NUL before
// fork), surfacing as "fork/exec <bin>: invalid argument" — an error
// whose text points at the binary, not the payload.
func TestSanitizeExecStrings(t *testing.T) {
	t.Run("clean input is returned unchanged", func(t *testing.T) {
		in := []string{"a", "b", "c"}
		out := sanitizeExecStrings("args", in)
		// Same backing slice — clean input must NOT allocate.
		if len(out) != len(in) {
			t.Fatalf("clean input length changed: got %d want %d", len(out), len(in))
		}
		for i := range out {
			if out[i] != in[i] {
				t.Errorf("clean input slot %d mutated: %q vs %q", i, out[i], in[i])
			}
		}
	})

	t.Run("strips NUL from each slot", func(t *testing.T) {
		in := []string{"ok", "a\x00b", "x\x00\x00y", "clean"}
		out := sanitizeExecStrings("args", in)
		if len(out) != len(in) {
			t.Fatalf("length changed: got %d want %d", len(out), len(in))
		}
		want := []string{"ok", "ab", "xy", "clean"}
		for i := range out {
			if out[i] != want[i] {
				t.Errorf("slot %d: got %q want %q", i, out[i], want[i])
			}
			if strings.ContainsRune(out[i], 0) {
				t.Errorf("slot %d still contains NUL after sanitize: %q", i, out[i])
			}
		}
	})

	t.Run("preserves other content", func(t *testing.T) {
		in := []string{"line1\nline2", "tab\there", "中文 mix"}
		out := sanitizeExecStrings("args", in)
		for i := range out {
			if out[i] != in[i] {
				t.Errorf("slot %d mutated (no NUL present): got %q want %q", i, out[i], in[i])
			}
		}
	})

	t.Run("strips NUL from env-style key=value entries", func(t *testing.T) {
		in := []string{"FOO=bar", "BAZ=with\x00nul", "PATH=/usr/bin"}
		out := sanitizeExecStrings("env", in)
		if strings.ContainsRune(out[1], 0) {
			t.Errorf("env[1] still contains NUL: %q", out[1])
		}
		if out[0] != "FOO=bar" || out[2] != "PATH=/usr/bin" {
			t.Errorf("clean env entries mutated: %q / %q", out[0], out[2])
		}
	})
}

// TestStreamCmdArgsNULFree is the end-to-end regression assertion: a
// StreamOpts whose Prompt contains a NUL byte must STILL produce a
// startable *exec.Cmd. Before the fix this would EINVAL out of cmd.Start
// before any CLI binary even ran. We override Path/Args to /bin/echo so
// the test doesn't depend on a real claude installation.
func TestStreamCmdArgsNULFree(t *testing.T) {
	if _, err := exec.LookPath("/bin/echo"); err != nil {
		t.Skipf("/bin/echo not available: %v", err)
	}

	gw := &Gateway{
		binPath: "/bin/echo",
		rawBin:  "/bin/echo",
		timeout: 30 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := gw.StreamCmd(ctx, StreamOpts{
		Prompt:  "user said: a\x00b and more",
		WorkDir: t.TempDir(),
	})

	// Sanity: the cmd the gateway constructed must itself be NUL-free in
	// argv and env. (We don't actually Start() the claude binary in tests;
	// we only check that what StreamCmd returned is valid.)
	for i, a := range cmd.Args {
		if strings.ContainsRune(a, 0) {
			t.Errorf("cmd.Args[%d] still contains NUL after sanitize: %q", i, a)
		}
	}
	for i, e := range cmd.Env {
		if strings.ContainsRune(e, 0) {
			t.Errorf("cmd.Env[%d] still contains NUL after sanitize: %q", i, e)
		}
	}

	// Pin the cmd to /bin/echo so we can Start() it deterministically.
	cmd.Path = "/bin/echo"
	cmd.Args[0] = "/bin/echo"
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start failed (regression of EINVAL fix): %v", err)
	}

	var buf bytes.Buffer
	doneCh := make(chan error, 1)
	go func() {
		_, _ = buf.ReadFrom(stdout)
		doneCh <- cmd.Wait()
	}()

	select {
	case <-doneCh:
		// /bin/echo prints the args as-is, with the -p prefix first.
		if !strings.HasPrefix(buf.String(), "-p ") {
			t.Errorf("echo didn't receive -p flag; output=%q", buf.String())
		}
	case <-ctx.Done():
		t.Fatalf("cmd did not exit before context deadline")
	}
}

// TestResolveBinSelfHeal: after the cached binPath vanishes from the
// filesystem, resolveBin must re-resolve from rawBin and return a fresh
// path. This is the secondary hardening for the claude CLI self-update
// scenario (Nova process holds a dangling version-pinned path).
func TestResolveBinSelfHeal(t *testing.T) {
	// Create a temporary file to stand in for the resolved claude binary.
	bin := t.TempDir() + "/fake-claude"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	gw := &Gateway{
		binPath: bin,
		rawBin:  bin,
		timeout: 30 * time.Second,
	}

	// Fast path: file exists.
	if got := gw.resolveBin(); got != bin {
		t.Fatalf("fast path: got %q want %q", got, bin)
	}

	// Delete the cached file → resolveBin should re-resolve from rawBin
	// and recover the path (since rawBin points at the same file path
	// we deleted, this verifies the re-resolution code runs without
	// panic; the LookPath branch handles the case where the new path
	// differs).
	if err := os.Remove(bin); err != nil {
		t.Fatalf("remove cached bin: %v", err)
	}
	_ = gw.resolveBin() // must not panic; on look-up failure it keeps prior value

	// Now exercise the LookPath recovery branch: rawBin points at an
	// existing file, cached binPath is stale.
	fresh := t.TempDir() + "/fresh-claude"
	if err := os.WriteFile(fresh, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fresh binary: %v", err)
	}
	gw2 := &Gateway{
		binPath: bin, // dangling
		rawBin:  fresh,
		timeout: 30 * time.Second,
	}
	got := gw2.resolveBin()
	// macOS temp dirs live under /var/folders/... but Go resolves them to
	// the real /private/var/folders/... path. EvalSymlinks on the
	// expected value normalizes both forms to the same canonical path.
	wantReal, err := filepath.EvalSymlinks(fresh)
	if err != nil {
		t.Fatalf("eval fresh: %v", err)
	}
	if got != wantReal {
		t.Fatalf("self-heal: got %q want %q", got, wantReal)
	}
}
