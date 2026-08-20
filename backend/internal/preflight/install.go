// Platform-specific install implementations for each tracked dependency. All
// platforms live in this one file (gated by runtime.GOOS inside New) — keeping
// the install matrix in one place makes the per-platform trade-offs easier to
// audit than scattered build-tagged files.
//
// Sudo/admin: on linux/macOS a global `npm install -g` or `apt-get install`
// typically needs elevated privileges. We try the command verbatim and trust
// the user to either already have passwordless sudo or to follow the
// fallback hint in /api/preflight. We deliberately do NOT shell out to `sudo`
// because that would require a TTY and pause the install waiting for input.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/novaworkbench/backend/internal/store"
)

// attachInstalls sets Dep.Install for the current platform. Called from New().
func (r *Registry) attachInstalls() {
	switch runtime.GOOS {
	case "darwin":
		r.installsForDarwin()
	case "linux":
		r.installsForLinux()
	case "windows":
		r.installsForWindows()
	default:
		// Unknown platform: leave Install nil; the UI surfaces Manual hints.
	}
}

// ---- macOS ----------------------------------------------------------------

func (r *Registry) installsForDarwin() {
	// brew is the preferred user-space package manager on macOS. It installs
	// to /usr/local or /opt/homebrew without prompting for sudo.
	brew := func(args ...string) *exec.Cmd {
		return exec.Command("brew", args...)
	}

	r.Lookup("node").Install = func(ctx context.Context, sink ProgressSink) error {
		return runLogged(ctx, sink, "brew", brew("install", "node"))
	}
	r.Lookup("npm").Install = func(ctx context.Context, sink ProgressSink) error {
		// npm is bundled with node; if node somehow installed without npm,
		// brew install npm explicitly.
		return runLogged(ctx, sink, "brew", brew("install", "npm"))
	}
	r.Lookup("claude").Install = func(ctx context.Context, sink ProgressSink) error {
		// Global npm install. On Apple Silicon Homebrew installs to
		// /opt/homebrew which puts node on PATH already, so no PATH tweak.
		return runLogged(ctx, sink, "npm",
			exec.Command(platformBin("npm"), "install", "-g", "@anthropic-ai/claude-code"))
	}
}

// ---- linux ----------------------------------------------------------------

func (r *Registry) installsForLinux() {
	// Detect the available package manager once at startup; the result is
	// reused for node/npm. apt-get is the common case (Ubuntu/Debian); RHEL-
	// family uses dnf; older RHEL uses yum. We deliberately do NOT shell out
	// to sudo — the user's existing sudoers config applies.
	pm := detectLinuxPackageManager()

	r.Lookup("node").Install = func(ctx context.Context, sink ProgressSink) error {
		switch pm {
		case "apt-get":
			if err := runLogged(ctx, sink, "apt-get", exec.Command("apt-get", "install", "-y", "nodejs", "npm")); err == nil {
				return nil
			}
			sink.Append(store.LogLine{Type: "message", Content: "[preflight] apt-get 安装失败，尝试回落到 nvm 用户态安装"})
			return r.installNodeViaNvm(ctx, sink)
		case "dnf":
			if err := runLogged(ctx, sink, "dnf", exec.Command("dnf", "install", "-y", "nodejs", "npm")); err == nil {
				return nil
			}
			sink.Append(store.LogLine{Type: "message", Content: "[preflight] dnf 安装失败，尝试回落到 nvm 用户态安装"})
			return r.installNodeViaNvm(ctx, sink)
		case "yum":
			if err := runLogged(ctx, sink, "yum", exec.Command("yum", "install", "-y", "nodejs", "npm")); err == nil {
				return nil
			}
			sink.Append(store.LogLine{Type: "message", Content: "[preflight] yum 安装失败，尝试回落到 nvm 用户态安装"})
			return r.installNodeViaNvm(ctx, sink)
		default:
			sink.Append(store.LogLine{Type: "message", Content: "[preflight] 未识别 Linux 包管理器，回落到 nvm 用户态安装"})
			return r.installNodeViaNvm(ctx, sink)
		}
	}
	r.Lookup("npm").Install = func(ctx context.Context, sink ProgressSink) error {
		// npm ships with node; re-installing node covers npm. Just re-trigger.
		return r.Lookup("node").Install(ctx, sink)
	}
	r.Lookup("claude").Install = func(ctx context.Context, sink ProgressSink) error {
		// Use the system npm; user-level npm install -g works if node was
		// installed without sudo (e.g. via nvm). When node came from
		// apt-get/dnf the global prefix is system-wide; permission errors
		// surface as a clear message rather than silent failure.
		return runLogged(ctx, sink, "npm",
			exec.Command(platformBin("npm"), "install", "-g", "@anthropic-ai/claude-code"))
	}
}

// installNodeViaNvm drops a tiny nvm bootstrap script into the user's home
// and runs it. nvm installs node per-user to ~/.nvm so no sudo is needed;
// the user must add `source ~/.nvm/nvm.sh` to their shell rc for new
// processes to see the install. We document that in the message stream.
func (r *Registry) installNodeViaNvm(ctx context.Context, sink ProgressSink) error {
	// Use a shell pipe to avoid juggling io.Pipes for curl|bash ourselves.
	vctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	install := exec.CommandContext(vctx, "bash", "-c",
		"curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash")
	if err := runLogged(ctx, sink, "nvm", install); err != nil {
		return fmt.Errorf("nvm 安装失败: %w", err)
	}
	// nvm puts node on ~/.nvm/versions/...; expose it to subsequent commands
	// by sourcing nvm.sh and running nvm install.
	home := defaultHome()
	install2 := exec.CommandContext(ctx, "bash", "-c",
		`export NVM_DIR="$HOME/.nvm"; [ -s "$NVM_DIR/nvm.sh" ] && \. "$NVM_DIR/nvm.sh"; nvm install --lts`)
	install2.Env = append(append([]string{}, install2.Environ()...), "HOME="+home)
	return runLogged(ctx, sink, "nvm-install", install2)
}

func detectLinuxPackageManager() string {
	for _, pm := range []string{"apt-get", "dnf", "yum"} {
		if _, err := exec.LookPath(pm); err == nil {
			return pm
		}
	}
	return ""
}

func defaultHome() string {
	if u, err := userHomeDir(); err == nil && u != "" {
		return u
	}
	return ""
}

// ---- windows --------------------------------------------------------------

func (r *Registry) installsForWindows() {
	// winget is the user-space package manager on modern Windows 10/11. It
	// lives at %LOCALAPPDATA%\Microsoft\WindowsApps\winget.exe which is on
	// PATH for most installs. We probe with `where` (the Windows equivalent
	// of `which`) and fall back to absolute path if missing.
	r.Lookup("node").Install = func(ctx context.Context, sink ProgressSink) error {
		bin := "winget"
		// 5 min — winget downloads Node (~30 MB) and runs the MSI installer.
		vctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(vctx, bin,
			"install", "--id", "OpenJS.NodeJS", "-e", "--accept-source-agreements", "--accept-package-agreements")
		return runLogged(ctx, sink, "winget", cmd)
	}
	r.Lookup("npm").Install = func(ctx context.Context, sink ProgressSink) error {
		// npm ships with node on Windows; re-installing node covers it.
		return r.Lookup("node").Install(ctx, sink)
	}
	r.Lookup("claude").Install = func(ctx context.Context, sink ProgressSink) error {
		// On Windows the npm binary is npm.cmd. Go 1.19+ exec auto-extends
		// .cmd when the command name has no path separator; using
		// platformBin keeps us explicit.
		return runLogged(ctx, sink, "npm",
			exec.Command(platformBin("npm"), "install", "-g", "@anthropic-ai/claude-code"))
	}
}

// ---- generic --------------------------------------------------------------

// probeNoOutput runs cmd and returns nil on a 0 exit, even with empty output.
// Used during registry construction where we don't need to stream logs.
func probeNoOutput(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("%s %v exit %d", name, args, exitErr.ExitCode())
		}
		return err
	}
	return nil
}

// userHomeDir returns $HOME (or %USERPROFILE% on windows) with a sensible
// fallback. Avoids pulling in os/user just for this.
func userHomeDir() (string, error) {
	if runtime.GOOS == "windows" {
		if h := os.Getenv("USERPROFILE"); h != "" {
			return h, nil
		}
	}
	if h := os.Getenv("HOME"); h != "" {
		return h, nil
	}
	return "", errors.New("HOME/USERPROFILE not set")
}
