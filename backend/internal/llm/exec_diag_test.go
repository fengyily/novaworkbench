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