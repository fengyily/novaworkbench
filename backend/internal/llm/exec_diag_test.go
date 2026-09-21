package llm

import (
	"errors"
	"os/exec"
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
	if _, err := exec.LookPath("xattr"); err != nil {
		t.Skip("xattr not on PATH; skipping macOS-specific EINVAL test")
	}
	got := FormatStartError("/bin/ls", syscall.EINVAL)
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