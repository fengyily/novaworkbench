//go:build linux

// Package servicemgr wires the `nova` binary into a host's init system so that
// end-users can install and uninstall NovaWorkbench as a long-running service
// with a single `sudo nova install` / `sudo nova uninstall`.
//
// Today only systemd is supported (Linux + majority of server distros). The
// package is intentionally tiny: it writes a unit file, shells out to
// systemctl, and cleans up on the way out. All it does NOT do is also matter:
//
//   - It does not create the runtime user (`nova`). Building a system user
//     implicitly during package install is the apt postinst's job (or the
//     README's `useradd` one-liner); the install subcommand refuses to
//     proceed if the user is missing so the user understands the split.
//   - It does not migrate or touch data under `$HOME/.novaworkbench` on
//     uninstall — the database is precious and the unit must not be the
//     thing that wipes it.
//
// Build tags isolate the Linux implementation from the stub in
// servicemgr_other.go so cross-compiling for Windows/macOS keeps working.
package servicemgr

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/novaworkbench/backend/internal/version"
)

// unitPath is the canonical location for the system unit written by Install.
// System units (as opposed to user units under ~/.config/systemd/user/) are
// required because nova must keep running across user sessions / reboots
// without needing a logged-in user; see agent-worker/systemd for an example of
// the user-side counterpart.
const unitPath = "/etc/systemd/system/nova.service"

// InstallOptions controls how the systemd unit is rendered.
type InstallOptions struct {
	// User is the system account the service will run as. Defaults to "nova".
	User string
	// Port is the HTTP listen port baked into the unit as Environment=NOVA_PORT.
	// Defaults to "9527" to match internal/config defaults.
	Port string
	// ExecStart is the absolute path to the nova binary written into the unit's
	// ExecStart. Derived from os.Executable() by RunInstall; exposed as a
	// field so tests can pin it deterministically.
	ExecStart string
}

// UninstallOptions controls the cleanup path.
type UninstallOptions struct {
	// User is the runtime user whose data directory we should mention as
	// "preserved" in the post-uninstall notice. Defaults to "nova".
	User string
	// UnitPath overrides the default /etc/systemd/system/nova.service target.
	// Mostly useful for tests; leave empty in production.
	UnitPath string
}

// RunInstall is the main()-facing entry point. It parses install-specific
// flags (independent flag.FlagSet so it does not collide with the server's
// own -migrate/-port flags) and calls Install.
func RunInstall(args []string) error {
	opts := InstallOptions{User: "nova", Port: "9527"}
	fs := flag.NewFlagSet("nova-install", flag.ContinueOnError)
	fs.StringVar(&opts.User, "user", opts.User, "system user to run the nova service as")
	fs.StringVar(&opts.Port, "port", opts.Port, "NOVA_PORT value baked into the unit")
	if err := fs.Parse(args); err != nil {
		// ContinueOnError already prints usage; surface a non-zero exit.
		return err
	}
	return Install(context.Background(), opts)
}

// RunUninstall mirrors RunInstall for the teardown path.
func RunUninstall(args []string) error {
	opts := UninstallOptions{User: "nova", UnitPath: unitPath}
	fs := flag.NewFlagSet("nova-uninstall", flag.ContinueOnError)
	fs.StringVar(&opts.User, "user", opts.User, "runtime user whose data directory should be preserved")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if opts.UnitPath == "" {
		opts.UnitPath = unitPath
	}
	return Uninstall(context.Background(), opts)
}

// RunVersion prints the binary's build identity in a stable format so users
// can copy/paste it into bug reports. Format mirrors `git describe`-style:
// `nova <version> (<commit>, <buildDate>)`.
func RunVersion() {
	fmt.Printf("nova %s (%s, %s)\n", version.Version, version.Commit, version.BuildDate)
}

// Install writes the nova.service unit, reloads systemd, and enables the
// service. Steps are ordered so a partial failure leaves the host in a
// recoverable state (a stale unit file is overwritten on the next retry).
func Install(ctx context.Context, opts InstallOptions) error {
	if opts.User == "" {
		opts.User = "nova"
	}
	if opts.Port == "" {
		opts.Port = "9527"
	}
	if opts.ExecStart == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve current executable: %w", err)
		}
		// Resolve any symlinks so the unit's ExecStart is stable across
		// re-runs (apt installs land at /usr/bin/nova, but a developer who
		// runs `sudo ./nova install` from a build dir wants the unit to keep
		// pointing at the same binary too).
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		opts.ExecStart = exe
	}

	if os.Geteuid() != 0 {
		return errors.New("nova install must be run as root (try `sudo nova install`)")
	}

	if _, err := user.Lookup(opts.User); err != nil {
		return fmt.Errorf(
			"runtime user %q does not exist; create it first:\n"+
				"  sudo useradd --create-home --home-dir /home/%[1]s --shell /bin/bash %[1]s\n"+
				"then re-run `sudo nova install`",
			opts.User)
	}

	unit := renderUnit(opts)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitPath, err)
	}

	if err := runSystemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := runSystemctl(ctx, "enable", "--now", "nova"); err != nil {
		return fmt.Errorf("systemctl enable --now nova: %w", err)
	}

	fmt.Println("✓ nova.service installed and started.")
	if opts.ExecStart != "/usr/bin/nova" {
		fmt.Printf("  ExecStart=%s (note: not /usr/bin/nova; apt install will normalize this)\n", opts.ExecStart)
	}
	fmt.Println()
	// Best-effort status snapshot. We never want install to fail because
	// `systemctl status` printed a non-active line right after enable --now
	// (e.g. unit briefly in "activating" state).
	if out, err := exec.Command("systemctl", "status", "nova", "--no-pager", "-n", "5").CombinedOutput(); err == nil {
		fmt.Println(string(out))
	} else {
		fmt.Println("  (could not read systemctl status, the service is likely starting up)")
	}
	fmt.Println("Next: tail logs with `journalctl -u nova -f`, open http://localhost:" + opts.Port + "/")
	return nil
}

// Uninstall disables + removes the unit. The user's data directory is left
// intact on purpose; nuking $HOME/.novaworkbench from a subcommand would be a
// foot-gun.
func Uninstall(ctx context.Context, opts UninstallOptions) error {
	if opts.User == "" {
		opts.User = "nova"
	}
	if opts.UnitPath == "" {
		opts.UnitPath = unitPath
	}

	if os.Geteuid() != 0 {
		return errors.New("nova uninstall must be run as root (try `sudo nova uninstall`)")
	}

	// disable --now may fail with "Unit nova.service not loaded" if the unit
	// was never installed or was already removed. Treat that as success —
	// subsequent steps are idempotent anyway.
	if err := runSystemctlIgnoreNotLoaded(ctx, "disable", "--now", "nova"); err != nil {
		return fmt.Errorf("systemctl disable --now nova: %w", err)
	}

	if err := os.Remove(opts.UnitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", opts.UnitPath, err)
	}

	if err := runSystemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	// Reset failed state so a previously broken unit does not haunt the next
	// install (systemd keeps failed state per-unit across removals in some
	// distros; `reset-failed` is the documented escape hatch).
	_ = runSystemctlIgnoreNotLoaded(ctx, "reset-failed", "nova")

	fmt.Println("✓ nova.service removed.")
	fmt.Printf("  Data directory /home/%s/.novaworkbench was preserved. To wipe it: sudo rm -rf /home/%s/.novaworkbench\n", opts.User, opts.User)
	return nil
}

// renderUnit assembles the systemd unit file. Kept as a pure function so it
// is trivial to unit-test the field substitution logic without touching the
// filesystem.
func renderUnit(opts InstallOptions) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=NovaWorkbench\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("User=" + opts.User + "\n")
	b.WriteString("ExecStart=" + opts.ExecStart + "\n")
	b.WriteString("Environment=NOVA_PORT=" + opts.Port + "\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=3\n")
	b.WriteString("\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

// runSystemctl executes systemctl and wraps stderr so callers see what went
// wrong without having to re-run the command themselves.
func runSystemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runSystemctlIgnoreNotLoaded runs systemctl but treats "unit not loaded" as a
// non-error. Used by the uninstall path so it can be safely re-run on a host
// that never had nova.service installed.
func runSystemctlIgnoreNotLoaded(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	lowered := strings.ToLower(string(out))
	if strings.Contains(lowered, "not loaded") ||
		strings.Contains(lowered, "not loaded unit") ||
		strings.Contains(lowered, "does not exist") {
		return nil
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
}
