package llm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// FormatStartError produces the user-facing message returned when launching
// the claude CLI subprocess fails. It is deliberately defensive: only the
// EINVAL case (kernel-level rejection of an executable file on POSIX, the
// most common cause of "fork/exec ... invalid argument" on a present-but-
// non-runnable binary) triggers the extra hint generation. Every other
// error path is forwarded unchanged so existing behavior is preserved.
func FormatStartError(binPath string, err error) string {
	if errors.Is(err, syscall.EINVAL) {
		if hint := diagnoseStartEINVAL(binPath); hint != "" {
			return "启动 Claude 失败: " + err.Error() + "\n" + hint
		}
	}
	return "启动 Claude 失败: " + err.Error()
}

// diagnoseStartEINVAL runs the cheap macOS-specific checks that turn the
// bare "invalid argument" into an actionable hint. Returns "" when nothing
// conclusive was found (caller falls back to the raw error) or when the
// host is not macOS.
//
// The checks are ordered roughly cheapest → most informative, so that the
// first positive match is usually the real cause:
//   - quarantine xattr (the Claude self-updater drops every new release
//     with this attribute on first run; Gatekeeper then refuses to exec
//     until the user clears it).
//   - codesign -v (a totally blank signature also produces EINVAL).
//   - codesign -v --strict (catches partial-update damage that basic -v
//     misses — e.g. when the LinkEdit segment is left half-overwritten,
//     basic -v still says "valid on disk" but exec fails).
//   - single-arch vs host mismatch (an x86_64-only binary on Apple Silicon
//     without Rosetta returns EINVAL on exec, not a friendlier error).
//   - file-size sanity (a 256MB binary shrunk to a few KB after a crashed
//     update still passes os.Stat but obviously cannot run).
//   - generic last-resort hint when the binary looks fine on disk: the
//     failure is then almost certainly inherited from the parent process
//     (e.g. a macOS sandbox profile on the shell that launched nova), and
//     the only useful advice is "try running it from a fresh terminal".
func diagnoseStartEINVAL(binPath string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	// Gate: file must exist and be a regular executable, otherwise both
	// xattr and codesign will fail for the wrong reason (file-not-found
	// rather than quarantine/signature-problem), and we should not hint.
	info, err := os.Stat(binPath)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	var hints []string

	// 1. quarantine xattr — Claude self-updater downloads land with this
	//    attribute; Gatekeeper refuses to exec quarantined binaries.
	if out, xerr := exec.Command("xattr", "-p", "com.apple.quarantine", binPath).CombinedOutput(); xerr == nil {
		val := strings.TrimSpace(string(out))
		hints = append(hints, fmt.Sprintf("检测到隔离属性 (quarantine=%q)。请在终端执行: xattr -dr com.apple.quarantine %s", val, binPath))
	}

	// 2. codesign — broken signature (often caused by partial self-update)
	//    also produces EINVAL on exec.
	if cerr := exec.Command("codesign", "-v", binPath).Run(); cerr != nil {
		hints = append(hints, fmt.Sprintf("代码签名校验失败 (%v)。请重装 Claude CLI: npm install -g @anthropic-ai/claude-code", cerr))
	} else if cerr := exec.Command("codesign", "-v", "--strict", binPath).Run(); cerr != nil {
		// 3. Strict check — -v alone can pass on a binary whose LinkEdit
		//    segment was left half-overwritten by a crashed self-update.
		//    --strict demands every page (including LinkEdit) carry a valid
		//    signature hash; partial damage shows up here while -v stays
		//    silent. Re-install is still the only real fix.
		hints = append(hints, fmt.Sprintf("严格代码签名校验失败 (%v)。请重装 Claude CLI: npm install -g @anthropic-ai/claude-code", cerr))
	}

	// 4. Architecture mismatch. An x86_64-only binary on Apple Silicon
	//    fails exec with EINVAL when Rosetta is not installed (the
	//    "posix_spawn" kern return code), and there is no friendlier
	//    message — the user just sees "invalid argument" from Go. Detect
	//    that case and tell them to either install Rosetta or grab the
	//    arm64 build. We only emit this hint when the host actually is
	//    arm64 (other direction is rare; an arm64-only binary on Intel
	//    Mac would surface the same way and Rosetta won't help there).
	if arch := binaryArch(binPath); arch != "" && arch != runtime.GOARCH && arch == "x86_64" && runtime.GOARCH == "arm64" {
		if !rosettaInstalled() {
			hints = append(hints, fmt.Sprintf("Claude 是 x86_64 而本机是 Apple Silicon 且未安装 Rosetta (%s)。请安装 Rosetta (softwareupdate --install-rosetta) 或重装 arm64 版本: npm install -g @anthropic-ai/claude-code", arch))
		}
	}

	// 5. File-size sanity. A working Claude CLI release is well over
	//    100MB; anything under 30MB is the fingerprint of a crashed
	//    download or a half-completed self-update. We catch it here so
	//    the user re-installs instead of chasing the upstream message.
	if info.Size() < 30*1024*1024 {
		hints = append(hints, fmt.Sprintf("Claude 文件大小异常 (%s)，疑似未下载完成。请重装: npm install -g @anthropic-ai/claude-code", humanSize(info.Size())))
	}

	if len(hints) == 0 {
		// 6. Last-resort hint. The binary looks healthy on disk
		//    (signed, unquarantined, right arch, sane size) but exec
		//    still failed. Two scenarios, ordered by likelihood:
		//
		//    a) Per-request transient sandbox / quarantine state. The
		//       nova process can usually spawn claude fine (other wizard
		//       stages like 方案设计 / 开发实现 work) but this one
		//       specific spawn hit a transient kernel denial — retrying
		//       the same wizard stage from the UI typically recovers.
		//
		//    b) The nova process itself is running under a macOS
		//       sandbox profile inherited from the parent shell (e.g.
		//       launched from inside an app that sandboxes its child
		//       processes). In that case ALL stages fail — the only
		//       actionable advice is to verify the binary works
		//       directly from a clean terminal, then restart nova from
		//       a non-sandboxed shell.
		//
		// We tell the user which scenario they're in by the wording of
		// the manual-verification step ("terminal can run it" vs "all
		// stages fail"), so they don't waste time restarting a
		// already-healthy nova for a transient one-shot failure.
		return "可能原因与修复建议:\n" +
			"  - exec 调用在内核层被拒绝 (EINVAL)。如果方案设计 / 开发实现等其他 stage 能正常调用 Claude CLI，本次失败属于瞬态状态——直接重试本步骤通常即可恢复。\n" +
			"  - 若所有 stage 都失败 (新建的需求、重启 Nova 仍然 EINVAL)，说明 Nova 进程本身继承了沙盒限制。请手动验证: " + binPath + " --version；若终端可直接运行，请从一个普通终端 (Terminal.app / iTerm2，而非任何应用的内置终端) 重启 Nova 后端。"
	}
	return "可能原因与修复建议:\n  - " + strings.Join(hints, "\n  - ")
}

// binaryArch returns the Mach-O architecture reported by lipo -info (single
// arch or the only slice of a fat binary). Returns "" on any error so the
// caller can safely skip the hint — we'd rather miss a wrong-arch case than
// fire a false-positive reinstall prompt.
func binaryArch(binPath string) string {
	out, err := exec.Command("lipo", "-info", binPath).CombinedOutput()
	if err != nil {
		return ""
	}
	// "Non-fat file: /path is architecture: arm64"  OR
	// "Architectures in the fat file: /path are: x86_64 arm64"
	line := strings.TrimSpace(string(out))
	if i := strings.LastIndex(line, ": "); i >= 0 {
		rest := line[i+2:]
		// Strip "are: " prefix that lipo adds for fat files.
		rest = strings.TrimPrefix(rest, "are: ")
		// For "is architecture: X" we get "X" directly. For fat files we
		// get a space-separated list — the first arch is fine; if any
		// matches the host the kernel will exec it.
		if fields := strings.Fields(rest); len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

// rosettaInstalled reports whether the Rosetta translation daemon (oahd)
// is running on the host. A long-running daemon is the reliable signal on
// Apple Silicon: launching a translated binary starts oahd on demand and
// keeps it alive, so its mere presence proves Rosetta has been used at
// least once on this host. The check is best-effort — on Intel hosts and
// on Linux CI it returns false but is never consulted (gated by caller).
func rosettaInstalled() bool {
	// pgrep returns 0 if at least one match exists.
	return exec.Command("pgrep", "-l", "oahd").Run() == nil
}

// humanSize formats a byte count as a short, human-readable string. Only
// used in error hints, so we keep it small and dependency-free.
func humanSize(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}