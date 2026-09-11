package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// gpg.go is the shared kernel for the GPG-signing provision path used by
// both the remote Agent Server branch (handler/gpg_remote.go) and the
// local coding branch (handler/gpg_local.go). It is deliberately I/O
// free: every public function returns either a shell script string for
// the caller to feed into a RunScript / sh -c, or a parsed value out of
// that script's stdout. That keeps the function unit-testable without
// touching the filesystem or the SSH client, and it forces a single
// source of truth for the script body — so a bug fix in the script
// applies to both provision paths in lock-step.
//
// Two non-obvious invariants the script enforces:
//
//  1. **Repo config goes to `--worktree`, never `--local`.** In a git
//     linked worktree, `git config --local` writes the *base* repo's
//     shared .git/config. With per-project concurrency on a single
//     Agent Server host that means a later req's `gpg.program`
//     overwrites an earlier req's, and the first req's cleanup of its
//     GNUPGHOME makes the other req's commits suddenly fail. Writing
//     `extensions.worktreeConfig = true` on the base repo + then
//     `git config --worktree ...` on the wt puts each key into the
//     per-worktree .git/worktrees/<id>/config.worktree file. If git
//     < 2.20 rejects `--worktree`, we fall back to `--local` and emit
//     NOVA_GPG_WORKTREE_FALLBACK=1 so the caller can warn that
//     concurrent reqs on the same project may interfere.
//
//  2. **No password on `gpg --import`.** The armored private-key block
//     is encrypted *inside* itself; the passphrase is only consulted
//     at use time (via gpg-agent / loopback). So the provision script
//     never touches the passphrase file — the wrapper script
//     (buildGPGWrapperScript) reads it at signature time only, and
//     even then only if the file is non-empty (an unprotected key
//     must not see `--passphrase-file <empty>`).

// buildGPGProvisionScript emits the shell script that, on the target
// host (local or remote), imports the previously-uploaded armored
// private key into a per-run GNUPGHOME and wires the given worktree's
// git config to use it for every commit/tag.
//
// Parameters:
//   - gnupgHome: directory where gpg.conf / gpg-agent.conf /
//     passphrase / git-gpg-wrapper / key.asc live. Must already
//     contain the key.asc (written by the caller) and be 0700. The
//     script will chmod 0700 anyway as a belt-and-braces measure.
//   - wtPath: the linked worktree path where the dev branch lives.
//     `git config --worktree` writes here.
//   - baseRepo: the shared base repo on the Agent Server
//     (`/tmp/nova-agent/<projectID>/base`). Needed for
//     `extensions.worktreeConfig = true`.
//   - gitName / gitEmail: the committer identity to bake into the
//     worktree's user.name / user.email. Either or both may be empty;
//     empty fields are not written at all so git falls back to its own
//     config lookup, preserving dev-machine behavior.
//
// The script never embeds the private key or the passphrase — only
// file paths under GNUPGHOME.
func buildGPGProvisionScript(gnupgHome, wtPath, baseRepo, gitName, gitEmail string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("export GNUPGHOME=" + shellQuoteSingle(gnupgHome) + "\n")
	b.WriteString("mkdir -p \"$GNUPGHOME\" && chmod 0700 \"$GNUPGHOME\"\n")
	b.WriteString("printf 'pinentry-mode loopback\\n' > \"$GNUPGHOME/gpg.conf\"\n")
	b.WriteString("printf 'allow-loopback-pinentry\\n' > \"$GNUPGHOME/gpg-agent.conf\"\n")
	// Import is non-interactive; --pinentry-mode loopback + --batch is
	// the documented way to skip the pinentry prompt entirely on
	// ancient gpg 2.0.x.
	b.WriteString("gpg --batch --no-tty --yes --pinentry-mode loopback --import \"$GNUPGHOME/key.asc\"\n")
	// Wipe the armored key from disk ASAP so a stray `git add -A`
	// against GNUPGHOME can't commit it. The keyring copy inside
	// ~/.gnupg/private-keys-v1.d/ is what matters from now on.
	b.WriteString("rm -f \"$GNUPGHOME/key.asc\"\n")
	// Extract the 16-hex key id from `gpg --list-secret-keys --with-colons`,
	// which prints lines like: sec:u:4096:1:ABCDEF...:...:...
	b.WriteString("keyid=$(gpg --list-secret-keys --with-colons | awk -F: '/^sec:/{print $5; exit}')\n")
	b.WriteString("if [ -z \"$keyid\" ]; then\n")
	b.WriteString("  echo \"[nova-gpg] 未在导入结果中找到私钥，请确认上传的 armored 私钥块完整且未损坏\" >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")
	// Enable per-worktree config on the base repo. Required for
	// `git config --worktree` to actually land in
	// .git/worktrees/<id>/config.worktree instead of being rejected.
	b.WriteString("git -C " + shellQuoteSingle(baseRepo) + " config extensions.worktreeConfig true || true\n")

	// writeWt emits `git -C <wt> config --worktree <key> <val>` with a
	// fallback to `--local` if --worktree is unsupported. The caller
	// reports the situation as a single warning line.
	//
	// `key` is always a hard-coded literal (user.name, commit.gpgsign,
	// etc.) so it gets shellQuoteSingle. `val` may be either a literal
	// (true, user@example.com) or a shell variable reference
	// (`$keyid`, `$GNUPGHOME/git-gpg-wrapper`) — when it starts with
	// `$` we leave it unquoted-double-quoted so shell expansion
	// happens at run time. shellQuoteSingle on `$keyid` would emit
	// `'$keyid'` and break the substitution.
	writeWt := func(key, val string) {
		keyQ := shellQuoteSingle(key)
		var valRendered string
		if strings.HasPrefix(val, "$") {
			valRendered = `"` + val + `"`
		} else {
			valRendered = shellQuoteSingle(val)
		}
		wtQ := shellQuoteSingle(wtPath)
		b.WriteString("if ! git -C " + wtQ + " config --worktree " + keyQ + " " + valRendered + " 2>/dev/null; then\n")
		b.WriteString("  echo \"[nova-gpg] --worktree config 失败，回落到 --local（key=" + key + "）\" >&2\n")
		b.WriteString("  echo \"NOVA_GPG_WORKTREE_FALLBACK=1\"\n")
		b.WriteString("  git -C " + wtQ + " config --local " + keyQ + " " + valRendered + "\n")
		b.WriteString("fi\n")
	}

	if gitName != "" {
		writeWt("user.name", gitName)
	}
	if gitEmail != "" {
		writeWt("user.email", gitEmail)
	}
	writeWt("user.signingkey", "$keyid")
	writeWt("commit.gpgsign", "true")
	writeWt("tag.gpgsign", "true")
	writeWt("gpg.program", "$GNUPGHOME/git-gpg-wrapper")

	// Marker line: parseKeyIDFromScriptOutput greps this out of the
	// combined stdout. Putting it last means a non-zero exit anywhere
	// above skips the marker, which the caller can detect as "did not
	// succeed".
	b.WriteString("echo \"NOVA_GPG_KEYID=$keyid\"\n")
	return b.String()
}

// buildGPGWrapperScript emits the executable that `git` invokes when
// it needs a signature (git calls `gpg.program` with arguments like
// `--status-fd=2 -bsau <keyid>`). The wrapper's job is to:
//
//   - Pin GNUPGHOME so the wrapper works regardless of which directory
//     git was invoked from (git does not propagate env into the
//     gpg.program subprocess in a way we can rely on across hosts).
//   - Suppress pinentry prompts so non-interactive commits don't hang.
//   - Pass the passphrase file *only* if it is non-empty. An
//     unprotected key must not see `--passphrase-file ""` — gpg 2.4.x
//     treats that as a hard failure ("no passphrase given") rather
//     than "no passphrase wanted".
func buildGPGWrapperScript(gnupgHome string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("export GNUPGHOME=" + shellQuoteSingle(gnupgHome) + "\n")
	b.WriteString("if [ -s \"$GNUPGHOME/passphrase\" ]; then\n")
	b.WriteString("  exec gpg --batch --no-tty --pinentry-mode loopback --passphrase-file \"$GNUPGHOME/passphrase\" \"$@\"\n")
	b.WriteString("fi\n")
	b.WriteString("exec gpg --batch --no-tty --pinentry-mode loopback \"$@\"\n")
	return b.String()
}

// parseKeyIDFromScriptOutput extracts the key id and the worktree
// fallback marker from the combined stdout of buildGPGProvisionScript.
// Returns ("", false) when neither marker is present (caller treats
// that as "script did not reach the success path"). Multi-line output
// is supported — line ordering does not matter — because the SSH
// runner concatenates command stdout/stderr into the writer in
// arbitrary chunks.
//
// Remote path note: the SSH `pump` helper prepends `[<label>]` to every
// line (see ssh/client.go `pump`). Without stripping that prefix here
// the KEYID and FALLBACK markers would never match in the remote
// provision flow — every line would look like
// `[gpg-provision] NOVA_GPG_KEYID=…` and `strings.HasPrefix` against
// the bare marker would silently miss.
//
// We use two passes per line:
//
//  1. Strip a leading `[<label>]` token when present; this normalises
//     the remote flow into the same shape the local flow already has
//     (no label).
//  2. As a belt-and-braces fallback, also try to find `NOVA_GPG_KEYID=`
//     anywhere in the (un-stripped) line. This protects against edge
//     cases where the prefix didn't land cleanly (e.g. a stray leading
//     whitespace, a future pump change, or a non-standard label) and
//     was the difference between the original "未输出 keyid" false-
//     positive and a correct parse on the Agent Server path.
//
// worktreeFallback=true means at least one `git config --worktree`
// call failed and the script fell back to `--local`. Concurrent reqs
// against the same base repo may then interfere; the caller should
// surface this as a single warning to the user.
func parseKeyIDFromScriptOutput(out string) (keyID string, worktreeFallback bool) {
	// Match `NOVA_GPG_KEYID=` followed by 16+ hex chars. The leading
	// "NOVA_GPG_KEYID=" prefix is unique to our marker (gpg itself
	// never emits that string), so we don't risk a false positive.
	// Anchoring on a word boundary keeps us from accidentally
	// matching a value that happens to contain the substring
	// "NOVA_GPG_KEYID=" inside a longer identifier.
	keyIDRe := regexp.MustCompile(`(?:^|\s|[\[\(])NOVA_GPG_KEYID=([0-9A-Fa-f]{16,})`)
	for _, line := range strings.Split(out, "\n") {
		stripped := strings.TrimSpace(line)
		// Strip a leading `[label]` token when present (remote path
		// only). The guard refuses to strip on a line that lacks a
		// closing `]`, so a stray `[` in the script's own output
		// cannot mask a real marker.
		strippedForMatch := stripped
		if i := strings.LastIndexByte(strippedForMatch, ']'); i > 0 && strings.HasPrefix(strippedForMatch, "[") {
			strippedForMatch = strings.TrimSpace(strippedForMatch[i+1:])
		}
		// Pass 1: exact prefix match on the (possibly stripped) line —
		// the happy path for both local (no prefix) and remote (label
		// stripped).
		if strings.HasPrefix(strippedForMatch, "NOVA_GPG_KEYID=") {
			keyID = strings.TrimSpace(strings.TrimPrefix(strippedForMatch, "NOVA_GPG_KEYID="))
			continue
		}
		// Pass 2: regex fallback so a stray prefix / mid-line marker
		// still resolves. We search the ORIGINAL line (not the
		// stripped one) because some hosts prefix with extra noise
		// before the [label] token.
		if m := keyIDRe.FindStringSubmatch(line); len(m) == 2 {
			keyID = m[1]
			continue
		}
		// Fallback marker is a single literal line. After stripping
		// `[label]` (if present), equality match is enough; we don't
		// need a regex because the marker has a unique suffix
		// (`=1`) that nothing else in the script outputs.
		if strippedForMatch == "NOVA_GPG_WORKTREE_FALLBACK=1" {
			worktreeFallback = true
		}
	}
	return keyID, worktreeFallback
}

// parseKeyIDFromScriptOutputDebug is the diagnostic twin of
// parseKeyIDFromScriptOutput. Same parsing rules; the extra return
// value is a short Chinese summary describing WHY the keyid ended
// up empty (no stdout at all? every line had a [label] prefix but
// no marker? a stray `[` without `]`?) so the caller can log a
// targeted hint when the provision fails. Keep this in lock-step
// with the pure function above — both must agree on every input.
func parseKeyIDFromScriptOutputDebug(out string) (keyID string, worktreeFallback bool, summary string) {
	keyIDRe := regexp.MustCompile(`(?:^|\s|[\[\(])NOVA_GPG_KEYID=([0-9A-Fa-f]{16,})`)
	var (
		nonEmptyLines int
		strippedLines int
		keyidHits     int
		fallbackHits  int
		oddLabels     int
		lastNonEmpty  string
	)
	for _, line := range strings.Split(out, "\n") {
		raw := line
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		nonEmptyLines++
		lastNonEmpty = raw
		stripped := trimmed
		if i := strings.LastIndexByte(stripped, ']'); i > 0 && strings.HasPrefix(stripped, "[") {
			strippedLines++
			stripped = strings.TrimSpace(stripped[i+1:])
		} else if strings.HasPrefix(trimmed, "[") {
			// Stray `[` without closing `]` — the strip rule refused
			// to touch this line. Worth flagging because that's
			// exactly the shape that hides a real marker behind a
			// half-formed label.
			oddLabels++
		}
		if strings.HasPrefix(stripped, "NOVA_GPG_KEYID=") {
			keyidHits++
			keyID = strings.TrimSpace(strings.TrimPrefix(stripped, "NOVA_GPG_KEYID="))
		} else if m := keyIDRe.FindStringSubmatch(trimmed); len(m) == 2 {
			keyidHits++
			keyID = m[1]
		}
		if stripped == "NOVA_GPG_WORKTREE_FALLBACK=1" {
			fallbackHits++
			worktreeFallback = true
		}
	}
	switch {
	case nonEmptyLines == 0:
		summary = "stdout 为空（pump 没有收到任何输出；可能 SSH exec 在 Start 之前就失败了，或脚本 stdout 被 shell rc 重定向走了）"
	case keyidHits == 0 && strippedLines == nonEmptyLines:
		summary = fmt.Sprintf("全部 %d 行都带 [label] 前缀但没有任何一行包含 NOVA_GPG_KEYID= 标记（脚本可能未到达末尾的 echo 行）", strippedLines)
	case keyidHits == 0 && oddLabels > 0:
		summary = fmt.Sprintf("发现 %d 行带孤立 [ 但缺 ]（label 前缀异常，可能是 SSH 通道截断或 pump 把多行粘成一行）", oddLabels)
	case keyidHits == 0:
		summary = fmt.Sprintf("脚本输出 %d 行但均不含 NOVA_GPG_KEYID=；最后一行: %q", nonEmptyLines, truncateForLog(lastNonEmpty, 120))
	default:
		summary = fmt.Sprintf("命中 NOVA_GPG_KEYID=%d 次", keyidHits)
	}
	return keyID, worktreeFallback, summary
}

// shortSHA256 returns the first 12 hex chars of the SHA-256 of s —
// enough to fingerprint a script body in a single log line without
// making the operator read a full 64-char hash. Used by the GPG
// provision debug breadcrumb so we can tell at a glance whether the
// running server shipped the same script body as the source tree
// (a stale binary that didn't get rebuilt would diverge here).
func shortSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// tailLines returns the last n lines of s. Used for the GPG provision
// debug breadcrumb so an operator can confirm the script's trailing
// `echo "NOVA_GPG_KEYID=$keyid"` survived the SFTP upload without
// scrolling through the whole script body.
func tailLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// classifyGitSignFailure maps the stderr of a failed `git commit` (or
// `git push`) into a user-facing Chinese message. Returns the empty
// string when no specific bucket matches, so the caller can fall back
// to the existing generic "exit=N" wording without double-reporting.
//
// Order matters: `bad passphrase` strings are a substring of several
// unrelated errors (`Inappropriate ioctl` shows up in any program that
// tries to read from a closed tty, including the "no passphrase given"
// path), so we test the most specific phrases first.
func classifyGitSignFailure(stderr string) string {
	s := strings.ToLower(stderr)
	switch {
	// Wrong passphrase typed, or wrong passphrase file contents. We
	// only flag the unambiguous "the passphrase was tried and gpg
	// rejected it" wording — `Inappropriate ioctl` alone is too
	// generic (it can also mean "no tty available", which we surface
	// via the "no secret key" bucket below).
	case strings.Contains(s, "bad passphrase"),
		strings.Contains(s, "decryption failed"):
		return "❌ GPG 私钥密码错误，请到「设置 → 平台 Token」更正后重试"

	case strings.Contains(s, "expired"), strings.Contains(s, "key expired"):
		return "❌ GPG 密钥已过期，请更新密钥后重新上传"

	case strings.Contains(s, "secret key not available"),
		strings.Contains(s, "no secret key"),
		strings.Contains(s, "skipped: no public key"), // unusable subkey
		strings.Contains(s, "inappropriate ioctl for device"):
		return "❌ 未找到可用的 GPG 私钥，请确认已在「设置 → 平台 Token」保存正确的私钥"

	case strings.Contains(s, "gpg: not found"),
		strings.Contains(s, "executable file not found"),
		strings.Contains(s, "no such file or directory") && strings.Contains(s, "gpg"):
		return "❌ Agent 服务器缺少 gpg，请到「设置 → Agent 服务器」点「安装依赖」"

	case strings.Contains(s, "gh006"),
		strings.Contains(s, "protected branch"),
		strings.Contains(s, "commits must be signed"),
		// Narrowing to "verified email" avoids false positives on
		// unrelated log lines like gpg's own "Good signature from
		// …" output (which contains the word "verified" but is the
		// success path, not the failure path).
		strings.Contains(s, "verified email"):
		return "❌ 推送失败：GitHub 拒绝未验证提交。请确认 GPG 密钥 UID 邮箱与 Token 的 Git 邮箱一致，且该邮箱已在 GitHub 验证"
	}
	return ""
}

// gpgImportErrorMessage formats a Chinese "private-key import failed"
// message. Stderr is truncated to 500 runes so a verbose gpg error
// (which can include the entire failed packet) doesn't blow up the
// job log panel.
func gpgImportErrorMessage(stderr string) string {
	const max = 500
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return "❌ GPG 私钥导入失败：未知错误（gpg 无 stderr 输出）"
	}
	if len([]rune(trimmed)) > max {
		trimmed = string([]rune(trimmed)[:max]) + "…"
	}
	return fmt.Sprintf("❌ GPG 私钥导入失败：%s", trimmed)
}