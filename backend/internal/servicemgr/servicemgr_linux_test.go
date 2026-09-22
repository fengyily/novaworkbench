//go:build linux

package servicemgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withInstallPath 临时把 canonicalInstallPath 指向 dir 下的 binary 路径，
// 避免单元测试写主机 /usr/bin。返回还原函数。
func withInstallPath(t *testing.T, dir string) (string, func()) {
	t.Helper()
	target := filepath.Join(dir, "nova")
	orig := canonicalInstallPath
	canonicalInstallPath = target
	return target, func() { canonicalInstallPath = orig }
}

func TestEnsureBinaryInstalled_CopiesAndChmod(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "fake-src")
	if err := os.WriteFile(src, []byte("fake-binary-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, restore := withInstallPath(t, dir)
	defer restore()

	if err := ensureBinaryInstalled(src); err != nil {
		t.Fatalf("ensureBinaryInstalled: %v", err)
	}

	dst := filepath.Join(dir, "nova")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("dst mode = %o, want 0755", perm)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "fake-binary-bytes" {
		t.Fatalf("dst content = %q, want %q", got, "fake-binary-bytes")
	}
}

func TestEnsureBinaryInstalled_Idempotent_SamePath(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "nova")
	if err := os.WriteFile(dst, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, restore := withInstallPath(t, dir)
	defer restore()

	// source == canonicalInstallPath,只走 chmod 分支
	if err := ensureBinaryInstalled(dst); err != nil {
		t.Fatalf("ensureBinaryInstalled: %v", err)
	}
	info, _ := os.Stat(dst)
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("dst mode = %o, want 0755", perm)
	}
}

func TestEnsureBinaryInstalled_OverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new-src")
	dst := filepath.Join(dir, "nova")

	if err := os.WriteFile(src, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, restore := withInstallPath(t, dir)
	defer restore()

	if err := ensureBinaryInstalled(src); err != nil {
		t.Fatalf("ensureBinaryInstalled: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "v2" {
		t.Fatalf("content = %q, want %q", got, "v2")
	}
	info, _ := os.Stat(dst)
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("dst mode = %o, want 0755", perm)
	}
}

func TestRenderUnit_ExecStartAlwaysCanonical(t *testing.T) {
	// renderUnit(opts) 输出的 ExecStart 必须是 opts.ExecStart 的内容。
	// 由于 Install() 已经把 ExecStart 强制写为 canonicalInstallPath,
	// 这里通过传入任意路径然后验证:renderUnit 只透传 opts.ExecStart。
	opts := InstallOptions{User: "nova", Port: "9527", ExecStart: "/anything"}
	unit := renderUnit(opts)
	if !strings.Contains(unit, "ExecStart=/anything\n") {
		t.Fatalf("renderUnit should echo opts.ExecStart verbatim, got:\n%s", unit)
	}
	for _, want := range []string{"User=nova", "ExecStart=/anything", "Restart=always", "Environment=NOVA_PORT=9527"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("renderUnit missing %q in:\n%s", want, unit)
		}
	}
}
