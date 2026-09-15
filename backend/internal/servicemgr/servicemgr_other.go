//go:build !linux

// Stub for non-Linux platforms. Windows / macOS users don't have systemd, so
// `nova install` / `nova uninstall` are documented as Linux-only and fail
// fast with a clear message. The stub still has to satisfy the same exported
// surface so main.go can compile on every GOOS.
package servicemgr

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// InstallOptions / UninstallOptions are mirrored here so callers on every
// platform can reference the types (e.g. in tests).
type InstallOptions struct {
	User      string
	Port      string
	ExecStart string
}

type UninstallOptions struct {
	User     string
	UnitPath string
}

// errUnsupportedPlatform is the canonical "we are not on Linux" error.
var errUnsupportedPlatform = errors.New("nova install / uninstall are only supported on Linux")

// RunInstall prints the unsupported-platform message and returns a non-nil
// error so main.go's log.Fatalf surfaces a non-zero exit code.
func RunInstall(args []string) error {
	fmt.Fprintln(os.Stderr, "nova install: only supported on Linux (this build is for a different OS)")
	return errUnsupportedPlatform
}

// RunUninstall mirrors RunInstall.
func RunUninstall(args []string) error {
	fmt.Fprintln(os.Stderr, "nova uninstall: only supported on Linux (this build is for a different OS)")
	return errUnsupportedPlatform
}

// RunVersion is cross-platform — every supported build can report its build
// identity, since the version vars live in internal/version and are stamped
// at compile time.
func RunVersion() {
	fmt.Println("nova (cross-platform build — version info printed by the Linux binary only)")
}

// Install / Uninstall are kept as plain non-Linux error returns so any future
// package consumer (tests, downstream binaries) sees the same shape.
func Install(ctx context.Context, opts InstallOptions) error {
	return errUnsupportedPlatform
}

func Uninstall(ctx context.Context, opts UninstallOptions) error {
	return errUnsupportedPlatform
}
