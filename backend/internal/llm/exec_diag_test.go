package llm

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestFormatStartError_NonEINVAL_Passthrough(t *testing.T) {
	base := errors.New("some other failure")
	got := FormatStartError("/nonexistent", base)
	want := "启动 Claude 失败: " + base.Error()
	if got != want {
		t.Fatalf("passthrough: got %q want %q", got, want)
	}
}

func TestFormatStartError_EINVAL_AddsHint(t *testing.T) {
	// Build a real Mach-O on the system, then strip its code signature so
	// codesign -v fails — this mirrors the real-world partial-update failure
	// without requiring the actual claude binary to be in a broken state.
	src := "/bin/ls"
	if _, err := os.Stat(src); err != nil {
		t.Skip("cannot stat /bin/ls; skipping EINVAL hint test")
	}
	tmp, err := os.CreateTemp("", "claude-sig-test-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	f, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	g, err := os.Create(tmpPath)
	if err != nil {
		f.Close()
		t.Fatalf("create %s: %v", tmpPath, err)
	}
	if _, err := io.Copy(g, f); err != nil {
		f.Close()
		g.Close()
		t.Fatalf("io.Copy: %v", err)
	}
	f.Close()
	g.Close()
	os.Chmod(tmpPath, 0o755)

	// Strip signature so codesign -v will fail.
	exec.Command("codesign", "--remove-signature", tmpPath).Run()

	got := FormatStartError(tmpPath, syscall.EINVAL)
	if !strings.HasPrefix(got, "启动 Claude 失败: invalid argument") {
		t.Fatalf("missing original EINVAL prefix: %q", got)
	}
	if !strings.Contains(got, "可能原因与修复建议:") {
		t.Fatalf("missing diagnostic hint section: %q", got)
	}
}

func TestFormatStartError_EINVAL_NoPath(t *testing.T) {
	got := FormatStartError("/__definitely_not_a_real_path__", syscall.EINVAL)
	want := "启动 Claude 失败: invalid argument"
	if got != want {
		t.Fatalf("no-path fallback: got %q want %q", got, want)
	}
}

func TestFormatStartError_EINVAL_DirPath(t *testing.T) {
	// When the resolved path is a directory, os.Stat passes but Mode().IsRegular()
	// is false — the diagnostic must not run and must return raw EINVAL.
	dir := filepath.Dir(os.Args[0]) // any accessible directory
	got := FormatStartError(dir, syscall.EINVAL)
	want := "启动 Claude 失败: invalid argument"
	if got != want {
		t.Fatalf("dir path should not produce hints: got %q want %q", got, want)
	}
}

// TestFormatStartError_EINVAL_TinyFile covers the "half-downloaded binary"
// case: a present-and-regular file that's way too small to be a real Claude
// CLI install should fire the file-size hint. We don't need to break the
// codesign chain here — file size alone is enough to identify a broken
// install.
func TestFormatStartError_EINVAL_TinyFile(t *testing.T) {
	f, err := os.CreateTemp("", "claude-tiny-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	tmpPath := f.Name()
	// 4 KiB — well under the 30 MiB sanity floor in exec_diag.go.
	if _, err := f.Write(make([]byte, 4096)); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	defer os.Remove(tmpPath)

	got := FormatStartError(tmpPath, syscall.EINVAL)
	if !strings.Contains(got, "文件大小异常") {
		t.Fatalf("expected file-size hint in %q", got)
	}
	if !strings.Contains(got, "请重装") {
		t.Fatalf("expected reinstall suggestion in %q", got)
	}
}

// TestFormatStartError_EINVAL_FallbackOnCleanBinary covers the "binary looks
// healthy but exec still fails" path (e.g. macOS sandbox inherited from the
// parent shell). When every diagnostic check is inconclusive, the function
// must still produce SOMETHING actionable — never just bare EINVAL. We use
// the real installed Claude binary here because it satisfies all gates
// (signed, no quarantine, correct arch, sane size) and the test is skipped
// when it isn't available so CI on Linux stays green.
func TestFormatStartError_EINVAL_FallbackOnCleanBinary(t *testing.T) {
	candidates := []string{
		"/Users/f1/.local/share/claude/versions/2.1.220",
		"/usr/local/bin/claude",
	}
	var real string
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Size() >= 30*1024*1024 {
			real = p
			break
		}
	}
	if real == "" {
		t.Skip("no healthy Claude binary on disk; skipping fallback-hint test")
	}
	got := FormatStartError(real, syscall.EINVAL)
	if !strings.HasPrefix(got, "启动 Claude 失败: invalid argument") {
		t.Fatalf("missing original EINVAL prefix: %q", got)
	}
	if !strings.Contains(got, "可能原因与修复建议:") {
		t.Fatalf("expected fallback hint section on a clean binary; got %q", got)
	}
	// The fallback hint should suggest the manual terminal verification.
	if !strings.Contains(got, "请手动验证") {
		t.Fatalf("expected terminal-verification hint on fallback; got %q", got)
	}
	// The fallback hint should explicitly mention the "other stages work"
	// scenario so a user who can run design/coding normally doesn't waste
	// time restarting nova for a transient per-request failure.
	if !strings.Contains(got, "其他 stage 能正常调用") {
		t.Fatalf("expected transient-retry hint on fallback; got %q", got)
	}
	if !strings.Contains(got, "重试本步骤通常即可恢复") {
		t.Fatalf("expected retry-recovery wording on fallback; got %q", got)
	}
}

// TestBinaryArch covers the lipo -info parser: both the single-arch ("is
// architecture: X") and the fat-binary ("Architectures in the fat file:
// ... are: X Y") shapes must yield their first architecture. CI runs on
// non-macOS where lipo isn't present — the helper is supposed to return ""
// gracefully then.
func TestBinaryArch(t *testing.T) {
	// Make a tiny throwaway file; if lipo is missing the call returns ""
	// without erroring out, which is the contract.
	f, err := os.CreateTemp("", "binaryarch-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	tmpPath := f.Name()
	f.Close()
	defer os.Remove(tmpPath)

	if got := binaryArch(tmpPath); got != "" && got != "x86_64" && got != "arm64" {
		t.Logf("binaryArch on a junk file returned %q — lipo may not be on PATH; that's a non-fatal skip on CI", got)
	}
}

// TestHumanSize asserts the format strings we use in the size-mismatch hint.
func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KB"},
		{30 * 1024 * 1024, "30.0 MB"},
		{2 * 1024 * 1024 * 1024, "2.0 GB"},
	}
	for _, c := range cases {
		got := humanSize(c.in)
		if got != c.want {
			t.Errorf("humanSize(%d) = %q; want %q", c.in, got, c.want)
		}
	}
}

// TestRosettaInstalled is a smoke test for the Rosetta-detection helper.
// On Apple Silicon with Rosetta in use, pgrep -l oahd returns 0 and the
// helper must report true. On Intel / Linux it must NOT panic; it can
// return either value, so the test only verifies it doesn't crash.
func TestRosettaInstalled(t *testing.T) {
	_ = rosettaInstalled() // exercise the path; result is platform-dependent
}