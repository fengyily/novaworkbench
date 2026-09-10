package handler

import (
	"strings"
	"testing"
)

// TestBuildGPGProvisionScript_HappyWorktree asserts the standard
// (non-fallback) shape of the script: every config write uses
// `--worktree`, the per-run marker line is present, no `config
// --local` appears in the body, and the GNUPGHOME/quoted paths round
// trip through shellQuoteSingle without losing the leading slash or
// the path.
func TestBuildGPGProvisionScript_HappyWorktree(t *testing.T) {
	script := buildGPGProvisionScript(
		"/tmp/nova-agent/proj_abc/req_xyz.gnupg",
		"/tmp/nova-agent/proj_abc/req_xyz",
		"/tmp/nova-agent/proj_abc/base",
		"Zhang San",
		"zhangsan@example.com",
	)

	for _, want := range []string{
		"#!/bin/sh",
		"set -eu",
		"export GNUPGHOME='/tmp/nova-agent/proj_abc/req_xyz.gnupg'",
		"chmod 0700 \"$GNUPGHOME\"",
		"pinentry-mode loopback",     // gpg.conf
		"allow-loopback-pinentry",    // gpg-agent.conf (compat)
		`gpg --batch --no-tty --yes --pinentry-mode loopback --import "$GNUPGHOME/key.asc"`,
		`rm -f "$GNUPGHOME/key.asc"`,
		`awk -F: '/^sec:/{print $5; exit}'`,
		"git -C '/tmp/nova-agent/proj_abc/base' config extensions.worktreeConfig true",
		// Six `git config --worktree` calls + NO `--local` in the
		// happy path:
		"git -C '/tmp/nova-agent/proj_abc/req_xyz' config --worktree 'user.name' 'Zhang San'",
		"git -C '/tmp/nova-agent/proj_abc/req_xyz' config --worktree 'user.email' 'zhangsan@example.com'",
		"git -C '/tmp/nova-agent/proj_abc/req_xyz' config --worktree 'user.signingkey' \"$keyid\"",
		"git -C '/tmp/nova-agent/proj_abc/req_xyz' config --worktree 'commit.gpgsign' 'true'",
		"git -C '/tmp/nova-agent/proj_abc/req_xyz' config --worktree 'tag.gpgsign' 'true'",
		"git -C '/tmp/nova-agent/proj_abc/req_xyz' config --worktree 'gpg.program' \"$GNUPGHOME/git-gpg-wrapper\"",
		`echo "NOVA_GPG_KEYID=$keyid"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("provision script missing expected fragment:\n  %q\nfull script:\n%s", want, script)
		}
	}
}

// TestBuildGPGProvisionScript_NoLocalInHappyPath asserts that
// `--local` only ever appears inside the fallback branch (and never
// inside the per-key writes in the happy path). The earlier test
// already proves the happy path writes all keys with `--worktree`;
// this one is the belt-and-braces check that no regression sneaks
// `config --local` back into the main path.
//
// The script is structurally `if ! ... --worktree ...; then
//   echo FALLBACK
//   git ... --local ...
// fi`, so any `config --local` line that is *not* 2-space indented
// (i.e. sits at top level) is a bug.
func TestBuildGPGProvisionScript_NoLocalInHappyPath(t *testing.T) {
	script := buildGPGProvisionScript(
		"/tmp/g", "/tmp/wt", "/tmp/base", "Name", "n@e.com",
	)

	lines := strings.Split(script, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "git ") || !strings.Contains(line, "config --local") {
			continue
		}
		// Fallback `config --local` lines are indented with two
		// spaces; top-level ones are not. A regression that emits
		// `--local` outside the fallback will have a 0-indent line
		// here.
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("line %d emits --local at top level (outside fallback): %q", i, line)
		}
	}

	// Structural sanity: exactly six `if ! ... --worktree ...`
	// blocks, one per writeWt call (user.name/email/signingkey/
	// commit.gpgsign/tag.gpgsign/gpg.program). Each block must
	// contain exactly one `git config --local` line.
	if got := strings.Count(script, "config --local"); got != 6 {
		t.Errorf("expected 6 `config --local` lines (one per fallback), got %d", got)
	}

	// And NOVA_GPG_WORKTREE_FALLBACK=1 must be inside an `if !` block
	// (i.e. on a 2-space indented `echo` line).
	fallbackEcho := false
	for _, line := range lines {
		if strings.Contains(line, "NOVA_GPG_WORKTREE_FALLBACK=1") {
			if strings.HasPrefix(line, "  ") {
				fallbackEcho = true
				break
			}
			t.Errorf("NOVA_GPG_WORKTREE_FALLBACK marker emitted at top level: %q", line)
		}
	}
	if !fallbackEcho {
		t.Errorf("NOVA_GPG_WORKTREE_FALLBACK marker not found in fallback branch")
	}
}

// TestBuildGPGProvisionScript_EmptyIdentity verifies that an empty
// gitName / gitEmail does NOT emit a user.name / user.email write.
// The other five writes (signingkey, commit.gpgsign, tag.gpgsign,
// gpg.program, base extensions.worktreeConfig) must still be there.
func TestBuildGPGProvisionScript_EmptyIdentity(t *testing.T) {
	script := buildGPGProvisionScript(
		"/tmp/g", "/tmp/wt", "/tmp/base", "", "",
	)
	if strings.Contains(script, "'user.name'") {
		t.Errorf("empty gitName must not write user.name; got:\n%s", script)
	}
	if strings.Contains(script, "'user.email'") {
		t.Errorf("empty gitEmail must not write user.email; got:\n%s", script)
	}
	for _, want := range []string{
		"'user.signingkey'",
		"'commit.gpgsign'",
		"'tag.gpgsign'",
		"'gpg.program'",
		"extensions.worktreeConfig",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing expected fragment %q", want)
		}
	}
}

// TestBuildGPGProvisionScript_ShellQuoteSafe verifies that a
// GNUPGHOME path containing a single quote (a pathological but legal
// value) still survives shellQuoteSingle escaping. Single quotes
// inside the path must be encoded as `'\''` rather than terminating
// the surrounding quoted string prematurely.
func TestBuildGPGProvisionScript_ShellQuoteSafe(t *testing.T) {
	// Path with embedded single quote: /tmp/o'connor/.gnupg
	script := buildGPGProvisionScript(
		"/tmp/o'connor/.gnupg",
		"/tmp/wt", "/tmp/base", "n", "e@x",
	)
	wantExport := `export GNUPGHOME='/tmp/o'\''connor/.gnupg'`
	if !strings.Contains(script, wantExport) {
		t.Errorf("expected shell-quoted export line %q in:\n%s", wantExport, script)
	}
}

// TestBuildGPGProvisionScript_NoSecretMaterial guards the most
// important invariant: the script must NEVER embed the private key
// or passphrase. We can't reach them through normal call sites (the
// callers don't pass them in), but a future refactor that inlines the
// armored key for convenience would. We assert against the empty
// form here as a tripwire.
func TestBuildGPGProvisionScript_NoSecretMaterial(t *testing.T) {
	script := buildGPGProvisionScript(
		"/tmp/g", "/tmp/wt", "/tmp/base", "n", "e@x",
	)
	for _, banned := range []string{
		"-----BEGIN PGP PRIVATE KEY BLOCK-----",
		"--passphrase",
		"passphrase-file",
	} {
		if strings.Contains(script, banned) {
			t.Errorf("provision script must not embed secret material; found %q", banned)
		}
	}
}

// TestBuildGPGWrapperScript covers both branches of the
// `[ -s passphrase ]` guard. The wrapper must:
//   - Set GNUPGHOME via single-quote shell escaping.
//   - Only pass --passphrase-file when the file is non-empty.
//   - End with a fallback `exec gpg … "$@"` for unprotected keys.
func TestBuildGPGWrapperScript(t *testing.T) {
	script := buildGPGWrapperScript("/tmp/g")

	for _, want := range []string{
		"#!/bin/sh",
		"set -eu",
		`export GNUPGHOME='/tmp/g'`,
		`if [ -s "$GNUPGHOME/passphrase" ]; then`,
		`exec gpg --batch --no-tty --pinentry-mode loopback --passphrase-file "$GNUPGHOME/passphrase" "$@"`,
		// The unconditional fallback (unprotected key) must NOT
		// carry --passphrase-file. We assert it exists as a
		// standalone `exec gpg …` line without that flag.
		`exec gpg --batch --no-tty --pinentry-mode loopback "$@"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("wrapper script missing fragment %q\nfull script:\n%s", want, script)
		}
	}

	// Belt-and-braces: exactly one occurrence of --passphrase-file,
	// inside the `[ -s ]` branch.
	if got := strings.Count(script, "--passphrase-file"); got != 1 {
		t.Errorf("expected exactly 1 --passphrase-file (inside [ -s ] branch), got %d", got)
	}
}

// TestBuildGPGWrapperScript_QuotedPath verifies the GNUPGHOME export
// survives single-quote escaping for a path containing a quote.
func TestBuildGPGWrapperScript_QuotedPath(t *testing.T) {
	script := buildGPGWrapperScript("/tmp/o'connor/g")
	if !strings.Contains(script, `export GNUPGHOME='/tmp/o'\''connor/g'`) {
		t.Errorf("wrapper did not shell-quote the embedded quote; script:\n%s", script)
	}
}

// TestParseKeyIDFromScriptOutput covers the happy path, the
// no-marker path, the worktree-fallback path, and a multi-line
// scrambled-ordering path (the SSH writer may interleave stderr
// chunks, so we don't assume line order).
func TestParseKeyIDFromScriptOutput(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantKey   string
		wantFall  bool
	}{
		{
			name:    "happy path",
			in:      "...stuff...\nNOVA_GPG_KEYID=ABCDEF1234567890\n...more...",
			wantKey: "ABCDEF1234567890",
			wantFall: false,
		},
		{
			name:    "no markers at all",
			in:      "gpg: imported: 1\nNOVA_GPG_FALLBACK_ON_USER_FLAG=1",
			wantKey: "",
			wantFall: false,
		},
		{
			name:    "fallback marker only",
			in:      "NOVA_GPG_WORKTREE_FALLBACK=1\nother log",
			wantKey: "",
			wantFall: true,
		},
		{
			name:    "both markers, fallback printed before keyid",
			in:      "NOVA_GPG_WORKTREE_FALLBACK=1\ngpg: ok\nNOVA_GPG_KEYID=DEADBEEFCAFEBABE",
			wantKey: "DEADBEEFCAFEBABE",
			wantFall: true,
		},
		{
			name:    "whitespace tolerance",
			in:      "   NOVA_GPG_KEYID=  \t  0123ABC \n",
			wantKey: "0123ABC",
			wantFall: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotKey, gotFall := parseKeyIDFromScriptOutput(tc.in)
			if gotKey != tc.wantKey {
				t.Errorf("keyID = %q, want %q", gotKey, tc.wantKey)
			}
			if gotFall != tc.wantFall {
				t.Errorf("worktreeFallback = %v, want %v", gotFall, tc.wantFall)
			}
		})
	}
}

// TestClassifyGitSignFailure covers every bucket in the classifier
// plus the empty-string fallback (no specific bucket matched).
func TestClassifyGitSignFailure(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{
			name:   "bad passphrase",
			stderr: "gpg: signing failed: Bad passphrase",
			want:   "❌ GPG 私钥密码错误，请到「设置 → 平台 Token」更正后重试",
		},
		{
			name:   "decryption failed (alternative phrasing)",
			stderr: "gpg: decryption failed",
			want:   "❌ GPG 私钥密码错误，请到「设置 → 平台 Token」更正后重试",
		},
		{
			name:   "key expired",
			stderr: "gpg: signing failed: Key expired at 2024-01-01",
			want:   "❌ GPG 密钥已过期，请更新密钥后重新上传",
		},
		{
			name:   "lowercase expired keyword",
			stderr: "the signing key is expired",
			want:   "❌ GPG 密钥已过期，请更新密钥后重新上传",
		},
		{
			name:   "no secret key",
			stderr: "gpg: signing failed: No secret key",
			want:   "❌ 未找到可用的 GPG 私钥，请确认已在「设置 → 平台 Token」保存正确的私钥",
		},
		{
			name:   "secret key not available",
			stderr: "gpg: secret key not available",
			want:   "❌ 未找到可用的 GPG 私钥，请确认已在「设置 → 平台 Token」保存正确的私钥",
		},
		{
			name:   "ioctl tty missing",
			stderr: "gpg: cannot open '/dev/tty': Inappropriate ioctl for device",
			want:   "❌ 未找到可用的 GPG 私钥，请确认已在「设置 → 平台 Token」保存正确的私钥",
		},
		{
			name:   "gpg binary missing",
			stderr: "/bin/sh: gpg: not found",
			want:   "❌ Agent 服务器缺少 gpg，请到「设置 → Agent 服务器」点「安装依赖」",
		},
		{
			name:   "gpg executable not found",
			stderr: "fork/exec /usr/bin/gpg: executable file not found in $PATH",
			want:   "❌ Agent 服务器缺少 gpg，请到「设置 → Agent 服务器」点「安装依赖」",
		},
		{
			name:   "github GH006 branch protection",
			stderr: "remote: error: GH006: Protected branch update failed for main.",
			want:   "❌ 推送失败：GitHub 拒绝未验证提交。请确认 GPG 密钥 UID 邮箱与 Token 的 Git 邮箱一致，且该邮箱已在 GitHub 验证",
		},
		{
			name:   "github unverified phrasing",
			stderr: "remote: Commit signatures require a verified email",
			want:   "❌ 推送失败：GitHub 拒绝未验证提交。请确认 GPG 密钥 UID 邮箱与 Token 的 Git 邮箱一致，且该邮箱已在 GitHub 验证",
		},
		{
			name:   "unrelated stderr -> empty (caller falls back)",
			stderr: "fatal: could not lock ref 'refs/heads/requirement-xyz'",
			want:   "",
		},
		{
			name:   "case-insensitive match",
			stderr: "GPG: BAD PASSPHRASE",
			want:   "❌ GPG 私钥密码错误，请到「设置 → 平台 Token」更正后重试",
		},
		{
			name:   "empty stderr",
			stderr: "",
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyGitSignFailure(tc.stderr)
			if got != tc.want {
				t.Errorf("classifyGitSignFailure(%q) = %q, want %q", tc.stderr, got, tc.want)
			}
		})
	}
}

// TestClassifyGitSignFailure_PassphraseBeatsIoctl documents a subtle
// ordering invariant: when gpg reports a bad passphrase, the stderr
// frequently also contains "Inappropriate ioctl for device" because
// pinentry-loopback falls back through that ioctl path. The classifier
// must NOT mis-route that to "no secret key" — the user needs to
// correct their passphrase, not their key.
func TestClassifyGitSignFailure_PassphraseBeatsIoctl(t *testing.T) {
	stderr := `gpg: signing failed: Bad passphrase
gpg: Inappropriate ioctl for device`
	got := classifyGitSignFailure(stderr)
	want := "❌ GPG 私钥密码错误，请到「设置 → 平台 Token」更正后重试"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestClassifyGitSignFailure_NoFalsePositiveOnVerifiedSignature is a
// regression test for a subtle bug: gpg's own *success* output for a
// verified signature includes the word "verified" (e.g. "gpg: Good
// signature from … [verified]"). When such a line slipped into the
// captured stderr (it shouldn't, but SSH writers concatenate stderr
// into stdout sometimes), the classifier must NOT route it to the
// GitHub "unverified commit" bucket. The empty-string return lets the
// caller fall back to the generic exit-code message.
func TestClassifyGitSignFailure_NoFalsePositiveOnVerifiedSignature(t *testing.T) {
	stderr := `gpg: Good signature from "Test <t@example.com>" [verified]
some other unrelated line`
	if got := classifyGitSignFailure(stderr); got != "" {
		t.Errorf("expected empty (no false positive), got %q", got)
	}
}

// TestGPGImportErrorMessage covers the empty / populated / over-long
// truncation cases.
func TestGPGImportErrorMessage(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{
			name:   "empty stderr",
			stderr: "",
			want:   "❌ GPG 私钥导入失败：未知错误（gpg 无 stderr 输出）",
		},
		{
			name:   "whitespace only",
			stderr: "   \n  \t ",
			want:   "❌ GPG 私钥导入失败：未知错误（gpg 无 stderr 输出）",
		},
		{
			name:   "short stderr",
			stderr: "gpg: key block is corrupt",
			want:   "❌ GPG 私钥导入失败：gpg: key block is corrupt",
		},
		{
			name:   "long stderr truncated with ellipsis",
			stderr: strings.Repeat("x", 800),
			// 800 runes > 500; output should be 500 x's + ellipsis.
			want: "❌ GPG 私钥导入失败：" + strings.Repeat("x", 500) + "…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gpgImportErrorMessage(tc.stderr)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestShellQuoteSingle confirms the helper we depend on behaves the
// way the test assertions assume (single-quotes wrap the value, embedded
// quotes become `'\''`). gpg_test.go is colocated with shellQuoteSingle
// in the handler package, so the test sees the unexported function.
func TestShellQuoteSingle(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"/tmp/g", "'/tmp/g'"},
		{"o'connor", `'o'\''connor'`},
		{"plain", "'plain'"},
	}
	for _, tc := range cases {
		if got := shellQuoteSingle(tc.in); got != tc.want {
			t.Errorf("shellQuoteSingle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}