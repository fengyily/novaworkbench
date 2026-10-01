package handler

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
)

// provisionLocalGPG mirrors provisionRemoteGPG for the local coding path:
// it lays down a per-run GNUPGHOME (sibling to the worktree, NEVER inside
// it — otherwise `git add -A` would happily commit the armored key),
// imports the armored private key via the shared buildGPGProvisionScript,
// and returns the parsed key id plus an idempotent cleanup closure.
//
// Three rules keep this from going sideways:
//
//  1. GNUPGHOME comes from os.MkdirTemp("", "nova-gpg-") — a sibling of
//     the worktree under the OS temp dir (e.g. /tmp/nova-gpg-XXXXXX).
//     The caller passes the *worktree* path; we deliberately don't nest
//     the gpg home inside it.
//
//  2. All three artefacts are 0600 / 0700 respectively so a `find` from
//     the user's tooling can't pick up the passphrase file by accident,
//     and so a partial crash doesn't expose the armored key on disk for
//     long. The script itself chmod 0700's GNUPGHOME as a belt-and-braces
//     measure (in case some other process created the dir first).
//
//  3. cleanup() is idempotent (sync.Once) and safe to call via defer even
//     when the run panicked mid-way. The cleanup only does `os.RemoveAll`
//     locally — we deliberately do NOT run `gpgconf --kill gpg-agent`,
//     because on macOS the agent is shared with the interactive user
//     session and killing it surfaces as "can't connect to the agent" in
//     unrelated shells.
//
// Windows note: buildGPGProvisionScript targets /bin/sh, which doesn't
// ship on Windows by default. We surface a typed error here rather than
// silently falling back to cmd.exe — the script body would not
// translate cleanly, and silently degrading to "no signing" would defeat
// the whole point of having an opt-in checkbox. Callers are expected to
// log the error as a warning and continue without signing (matching the
// existing "local development is best-effort" stance in this handler).
func provisionLocalGPG(
	wtPath, baseRepo string,
	armoredKey, passphrase, gitName, gitEmail string,
) (gnupgHome, keyID string, cleanup func(), err error) {
	if runtime.GOOS == "windows" {
		return "", "", noopCleanup, fmt.Errorf("本地 GPG 签名暂不支持 Windows（缺少 /bin/sh），本次提交将不签名")
	}

	// 1. Create the per-run home dir. MkdirTemp creates with 0700 on
	//    POSIX (per the docs), but we chmod again so behaviour is
	//    identical even on filesystems / Go runtimes that don't honour
	//    the umask.
	home, mkErr := os.MkdirTemp("", "nova-gpg-")
	if mkErr != nil {
		return "", "", noopCleanup, fmt.Errorf("创建 GPG 临时目录失败：%w", mkErr)
	}
	if chmodErr := os.Chmod(home, 0700); chmodErr != nil {
		_ = os.RemoveAll(home)
		return "", "", noopCleanup, fmt.Errorf("设置 GPG 临时目录权限失败：%w", chmodErr)
	}

	// 2. Write the three artefacts. The passphrase file is written
	//    unconditionally (even when empty) because the wrapper uses
	//    `[ -s "$GNUPGHOME/passphrase" ]` to decide whether to forward
	//    --passphrase-file at all — an unprotected key must not see
	//    --passphrase-file with an empty argument (gpg 2.4.x treats that
	//    as a hard "no passphrase given" failure).
	if wfErr := os.WriteFile(home+"/key.asc", []byte(armoredKey), 0600); wfErr != nil {
		_ = os.RemoveAll(home)
		return "", "", noopCleanup, fmt.Errorf("写入 GPG 私钥文件失败：%w", wfErr)
	}
	if wfErr := os.WriteFile(home+"/passphrase", []byte(passphrase), 0600); wfErr != nil {
		_ = os.RemoveAll(home)
		return "", "", noopCleanup, fmt.Errorf("写入 GPG 密码文件失败：%w", wfErr)
	}
	if wfErr := os.WriteFile(home+"/git-gpg-wrapper", []byte(buildGPGWrapperScript(home)), 0700); wfErr != nil {
		_ = os.RemoveAll(home)
		return "", "", noopCleanup, fmt.Errorf("写入 GPG wrapper 脚本失败：%w", wfErr)
	}

	// 3. Run the provision script. We capture stdout in a Buffer so the
	//    NOVA_GPG_KEYID= marker can be parsed out without the gpg status
	//    noise. Unlike the remote path, we do NOT tee into a live job log
	//    here: the local caller already runs in the same goroutine as the
	//    SSE writer and the script completes in a few hundred ms, so the
	//    job log will instead get one consolidated "✅ 本地 GPG 已就绪
	//    (keyid=…)" / "⚠️ …" line — which is what users actually want.
	//
	//    The script only echoes gpg status text and our own NOVA_GPG_*
	//    marker; it never references the key.asc / passphrase file
	//    contents, so even if we DID tee to the job log nothing secret
	//    would leak.
	script := buildGPGProvisionScript(home, wtPath, baseRepo, gitName, gitEmail)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
	var stdoutBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stdoutBuf
	runErr := cmd.Run()
	if runErr != nil {
		// Prefer the import-failure message (which truncates gpg's
		// verbose packet dump to a readable size), fall back to the
		// git-sign classifier if it looks like a config-write step that
		// happened to hit a signing-adjacent error.
		msg := gpgImportErrorMessage(stdoutBuf.String())
		if msg == "" {
			msg = classifyGitSignFailure(stdoutBuf.String())
		}
		if msg == "" {
			msg = "❌ GPG 配置失败（exit=" + exitCodeString(cmd) + "）"
		}
		_ = os.RemoveAll(home)
		return "", "", noopCleanup, &gpgProvisionError{msg: msg, cause: runErr}
	}

	// 4. Parse the marker line out of the script output.
	parsedKeyID, _, _, worktreeFallback := parseKeyIDFromScriptOutput(stdoutBuf.String())
	if parsedKeyID == "" {
		_ = os.RemoveAll(home)
		return "", "", noopCleanup, &gpgProvisionError{msg: "GPG provision 脚本未输出 keyid，请检查本机 gpg 是否能正常运行"}
	}
	_ = worktreeFallback // surfaced by caller via job log when needed

	cleanup = makeLocalGPGCleanup(home)
	return home, parsedKeyID, cleanup, nil
}

// makeLocalGPGCleanup returns an idempotent rm-rf closure for the local
// GNUPGHOME. Safe to call multiple times (sync.Once) so a defer + a
// panic-recovery defer in the caller both work without doubling the work.
func makeLocalGPGCleanup(home string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			// Swallow the error — a leftover /tmp dir is recoverable
			// (the OS cleans it eventually) and we never want a cleanup
			// failure to mask a more important upstream error.
			_ = os.RemoveAll(home)
		})
	}
}

// exitCodeString returns cmd.ProcessState.ExitCode() as a string, or "?"
// when the process state is nil (e.g. the cmd never actually ran — we
// only get here after Run() returned a non-nil error, so the state
// should normally be populated, but be defensive).
func exitCodeString(cmd *exec.Cmd) string {
	if cmd == nil || cmd.ProcessState == nil {
		return "?"
	}
	return fmt.Sprintf("%d", cmd.ProcessState.ExitCode())
}

// verifyImportedGPGKey performs a dry-run import of an ASCII-armored
// GPG private key into a fresh GNUPGHOME and returns the parsed
// key id / fingerprint / UID. It deliberately does NOT touch git
// config or run any signature operations — the caller's only goal is
// to confirm that gpg itself can parse the supplied block, before we
// persist it as AES-256-GCM ciphertext on platform_tokens.
//
// Why this lives in the handler package and not in service/ — it
// shells out to the local `gpg` binary, which is exactly what
// provisionLocalGPG does. Keeping the two together means the temp-home
// choreography (MkdirTemp + chmod 0700 + sync.Once cleanup + Windows
// guard) stays in lock-step.
//
// Windows note: same stance as provisionLocalGPG — /bin/sh is not on
// PATH by default, so we surface a typed error rather than silently
// falling back to cmd.exe.
//
// Passphrase handling: the armored block's passphrase protection only
// matters at use time (gpg-agent / loopback prompt at signature time).
// Wrong passphrase therefore does NOT cause this function to fail —
// classifyGitSignFailure covers the runtime signature failure. Adding a
// passphrase check here would require either a `gpgconf --kill gpg-
// agent` race against the user's interactive agent, or a heavyweight
// `gpg --decrypt <dummy>` call; both are out of scope for a save-time
// dry-run.
func verifyImportedGPGKey(armoredKey, passphrase string) (keyID, fingerprint, uid string, err error) {
	_ = passphrase // documented unused: see function comment

	if runtime.GOOS == "windows" {
		return "", "", "", fmt.Errorf("GPG 私钥验证暂不支持 Windows（缺少 /bin/sh）")
	}

	// 1. Temp GNUPGHOME — sibling-of-worktree pattern, MkdirTemp picks
	//    a unique name under the OS temp dir; we chmod 0700 as
	//    belt-and-braces even though MkdirTemp already does it on POSIX.
	home, mkErr := os.MkdirTemp("", "nova-gpg-verify-")
	if mkErr != nil {
		return "", "", "", fmt.Errorf("创建 GPG 临时目录失败：%w", mkErr)
	}
	if chmodErr := os.Chmod(home, 0700); chmodErr != nil {
		_ = os.RemoveAll(home)
		return "", "", "", fmt.Errorf("设置 GPG 临时目录权限失败：%w", chmodErr)
	}

	// 2. Write the armored block to a 0600 file inside GNUPGHOME.
	//    The script reads from $GNUPGHOME/key.asc.
	if wfErr := os.WriteFile(home+"/key.asc", []byte(armoredKey), 0600); wfErr != nil {
		_ = os.RemoveAll(home)
		return "", "", "", fmt.Errorf("写入 GPG 私钥文件失败：%w", wfErr)
	}

	// 3. Run the verify script. We capture stdout/stderr into the same
	//    Buffer so the NOVA_GPG_* markers can be parsed without losing
	//    any diagnostic context. The script completes in a few hundred
	//    ms on a healthy host — we do NOT need a timeout here; a
	//    genuinely hung gpg --import is itself the error path.
	script := buildGPGVerifyScript(home, home+"/key.asc")
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if runErr := cmd.Run(); runErr != nil {
		msg := gpgImportErrorMessage(buf.String())
		if msg == "" {
			msg = "❌ GPG 私钥导入失败（exit=" + exitCodeString(cmd) + "）"
		}
		_ = os.RemoveAll(home)
		return "", "", "", fmt.Errorf("%s", msg)
	}

	// 4. Parse the markers — parseKeyIDFromScriptOutput handles the
	//    KEYID / FPR / UID trio; if KEYID is empty the script reported
	//    "未产出 sec 行" and exited non-zero, but we guard against it
	//    here in case a future script variant emits FPR/UID without
	//    KEYID (unlikely, but cheap to defend).
	parsedKeyID, parsedFPR, parsedUID, _ := parseKeyIDFromScriptOutput(buf.String())
	if parsedKeyID == "" {
		_ = os.RemoveAll(home)
		return "", "", "", fmt.Errorf("GPG 私钥导入失败：脚本未输出 keyid，armored 私钥块可能损坏")
	}

	// 5. Idempotent cleanup. The caller doesn't hold the cleanup
	//    closure — this is a one-shot dry-run, the temp dir's lifetime
	//    is exactly the duration of the script run.
	_ = os.RemoveAll(home)
	return parsedKeyID, parsedFPR, parsedUID, nil
}
