package llm

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
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
func diagnoseStartEINVAL(binPath string) string {
	if runtime.GOOS != "darwin" {
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
	}

	if len(hints) == 0 {
		return ""
	}
	return "可能原因与修复建议:\n  - " + strings.Join(hints, "\n  - ")
}