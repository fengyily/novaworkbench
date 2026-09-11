package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
)

// provisionRemoteGPG uploads an armored GPG private key + passphrase to a
// per-run GNUPGHOME on the Agent Server, runs buildGPGProvisionScript via
// SSH, parses the key id (and any --worktree fallback marker) out of the
// script stdout, and returns an idempotent cleanup closure the caller
// must `defer`.
//
// Layout invariants enforced here (see the matching warnings in the
// technical-design doc):
//
//   - GNUPGHOME is provided by the caller as a sibling of the worktree
//     (`/tmp/nova-agent/<projectID>/<reqID>.gnupg`), NEVER inside the
//     worktree itself. If a future refactor passes a path under wtPath,
//     `git add -A` in Step 7 would happily commit the armored key
//     alongside the user's code changes.
//
//   - All three uploaded files have their mode pinned by WriteFile (0600
//     for key.asc / passphrase, 0700 for the wrapper). The script body
//     also chmod 0700's GNUPGHOME as belt-and-braces in case some other
//     agent on the host created it first.
//
//   - cleanup() is idempotent. It is safe to call multiple times
//     (subsequent calls are a no-op via the sync.Once guard) and safe
//     to invoke after `client` has been closed (we just rm -rf via a
//     fresh SSH exec; if that fails we record a warning in the job log
//     but never return an error to the caller — leaking the temp dir
//     is non-fatal).
//
//   - We deliberately do NOT call `gpgconf --kill gpg-agent`. On macOS
//     the agent is shared with the interactive user session; killing it
//     would surface as "gpg: can't connect to the agent" in unrelated
//     user shells.
func provisionRemoteGPG(
	ctx context.Context,
	client *gossh.Client,
	job *store.Job,
	gnupgHome, wtPath, baseRepo,
	armoredKey, passphrase, gitName, gitEmail string,
) (keyID string, cleanup func(), err error) {
	// 1. Create the per-run home dir on the agent host.
	if mkErr := client.Mkdirp(gnupgHome); mkErr != nil {
		return "", noopCleanup, mkErr
	}

	// 2. Upload the three artefacts. The passphrase file is written
	//    unconditionally (even when empty) because the wrapper uses
	//    `[ -s "$GNUPGHOME/passphrase" ]` to decide whether to forward
	//    --passphrase-file at all — an unprotected key must not see
	//    --passphrase-file with an empty argument.
	if wfErr := client.WriteFile(gnupgHome+"/key.asc", []byte(armoredKey), 0600); wfErr != nil {
		return "", noopCleanup, wfErr
	}
	if wfErr := client.WriteFile(gnupgHome+"/passphrase", []byte(passphrase), 0600); wfErr != nil {
		return "", noopCleanup, wfErr
	}
	if wfErr := client.WriteFile(gnupgHome+"/git-gpg-wrapper", []byte(buildGPGWrapperScript(gnupgHome)), 0700); wfErr != nil {
		return "", noopCleanup, wfErr
	}

	// 3. Run the provision script. We collect stdout in a Buffer rather
	//    than letting it flow straight into the job log: the marker line
	//    (`NOVA_GPG_KEYID=...`) is a control string, not a status update
	//    for the user. We DO also tee it into the job via a jobWriter
	//    so a human watching the live log still sees "gpg: key ABCDEF...:
	//    secret key imported" on success — useful confirmation that
	//    gpg actually parsed the armored block.
	//
	//    We never tee the key.asc / passphrase file contents — only the
	//    script's stdout, which is just `gpg` status output and our own
	//    NOVA_GPG_* marker.
	script := buildGPGProvisionScript(gnupgHome, wtPath, baseRepo, gitName, gitEmail)
	// Debug breadcrumb #1: log a hash + length of the script body so
	// we can confirm the binary actually shipped the new marker / set
	// -eu rules. A different hash than the one in `make build`'s
	// git-tracked source means the running server is stale.
	if job != nil {
		job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("🛠 [nova-gpg-debug] script body len=%d sha256=%s", len(script), shortSHA256(script))})
		// Also echo the LAST few lines (where the KEYID marker lives)
		// so an operator can spot a corrupted / truncated upload at a
		// glance without scrolling the SSE panel.
		tail := tailLines(script, 6)
		job.Append(store.LogLine{Type: "message", Content: "🛠 [nova-gpg-debug] script tail (last 6 lines):\n" + tail})
	}
	var stdoutBuf bytes.Buffer
	out := io.MultiWriter(&stdoutBuf, &jobWriter{job: job})

	exit, runErr := client.RunScript(ctx, script, "gpg-provision", nil, out)
	// Debug breadcrumb #2: surface the exit code + runErr + captured
	// output size up-front. A non-zero exit here explains why the
	// marker would never appear; an empty stdoutBuf on exit=0
	// explains why the marker is in the script but not in our buffer.
	if job != nil {
		job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("🛠 [nova-gpg-debug] RunScript exit=%d runErr=%v stdoutBuf.Len=%d", exit, runErr, stdoutBuf.Len())})
		if stdoutBuf.Len() > 0 {
			job.Append(store.LogLine{Type: "message", Content: "🛠 [nova-gpg-debug] captured stdout/stderr (truncated to 4 KiB):\n" + truncateStr(stdoutBuf.String(), 4096)})
		} else {
			job.Append(store.LogLine{Type: "message", Content: "🛠 [nova-gpg-debug] stdoutBuf is EMPTY — pump did not receive any output. Possible causes: (1) SSH exec failed before Start(), (2) sh exited before printing anything, (3) the script's stdout was redirected away by a shell rc file (e.g. ~/.bashrc on the agent host, which only matters if /bin/sh is bash)."})
		}
	}
	if exit != 0 || runErr != nil {
		// Prefer the import-failure message (which truncates gpg's
		// verbose packet dump to a readable size), fall back to the
		// git-sign classifier if it looks like a config-write step
		// that happened to hit a signing-adjacent error.
		msg := gpgImportErrorMessage(stdoutBuf.String())
		if msg == "" {
			msg = classifyGitSignFailure(stdoutBuf.String())
		}
		if msg == "" {
			msg = "❌ GPG 配置失败（exit=" + fmtInt(exit) + "）"
		}
		return "", noopCleanup, &gpgProvisionError{msg: msg, cause: runErr}
	}

	// 4. Parse the marker line out of the script output.
	captured := stdoutBuf.String()
	parsedKeyID, worktreeFallback, parseSummary := parseKeyIDFromScriptOutputDebug(captured)
	if job != nil {
		job.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("🛠 [nova-gpg-debug] parser: keyID=%q worktreeFallback=%v summary=%s", parsedKeyID, worktreeFallback, parseSummary)})
	}
	if parsedKeyID == "" {
		return "", noopCleanup, &gpgProvisionError{msg: "GPG provision 脚本未输出 keyid，请检查 Agent 服务器 gpg 是否能正常运行"}
	}
	if worktreeFallback {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 当前 git 不支持 worktree 级配置，已回落到仓库级（同项目并发开发时可能相互影响）"})
	}

	// 5. Build the idempotent cleanup closure. We capture job + path
	//    by value; the sync.Once guard makes it safe for the caller to
	//    `defer cleanup()` AND for a ctx.Done goroutine to call the
	//    same function — only the first invocation does real work.
	cleanup = makeRemoteGPGCleanup(client, job, gnupgHome)
	return parsedKeyID, cleanup, nil
}

// makeRemoteGPGCleanup returns an idempotent rm-rf closure for the
// given remote GNUPGHOME. Designed to be safe to call once via defer and
// again via a ctx.Done() goroutine (so a panic or client disconnects
// mid-run still wipes the secret-bearing directory).
func makeRemoteGPGCleanup(client *gossh.Client, job *store.Job, gnupgHome string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			// Try the rm via SSH. We deliberately swallow the error
			// because (a) the SSH session may already be torn down by
			// the time we get here, and (b) leaking a /tmp dir is
			// recoverable; the user can `rm -rf` it themselves. We
			// log a single warning so the failure isn't silent.
			if client == nil {
				return
			}
			if _, err := client.Exec(context.Background(),
				"rm -rf "+shellQuoteSingle(gnupgHome),
				"", nil, io.Discard, nil); err != nil {
				if job != nil {
					job.Append(store.LogLine{Type: "message", Content: "⚠️ GPG 临时目录清理失败：" + gnupgHome})
				}
			}
		})
	}
}

// noopCleanup is returned alongside a non-nil err so callers can still
// `defer cleanup()` unconditionally without worrying about a nil func.
func noopCleanup() {}

// provisionRemoteGitIdentity writes ONLY user.name / user.email into the
// given worktree's per-worktree git config, without setting up GPG. Used
// when the project's platform token has GPG disabled but still carries a
// git identity that needs to land in the remote worktree (the remote
// path previously had no identity injection at all — a regression from
// the local path's GIT_AUTHOR_* env vars). Same `--worktree` + fallback
// rule as the full provision script; this function is essentially the
// same body with the gpg block stripped out.
//
// Empty name/email are skipped (the helper matches the convention in
// buildGPGProvisionScript). Errors from the SSH exec are surfaced so the
// caller can decide whether to abort or just log a warning.
func provisionRemoteGitIdentity(ctx context.Context, client *gossh.Client, job *store.Job, wtPath, baseRepo, gitName, gitEmail string) error {
	if gitName == "" && gitEmail == "" {
		return nil
	}
	// Mirror the --worktree-then-fallback pattern from gpg.go without
	// re-using the full provision script (which would also chmod the
	// GNUPGHOME dir etc. and fail on the missing key.asc).
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n")
	b.WriteString("git -C " + shellQuoteSingle(baseRepo) + " config extensions.worktreeConfig true || true\n")
	writePair := func(key, val string) {
		if val == "" {
			return
		}
		b.WriteString("if ! git -C " + shellQuoteSingle(wtPath) + " config --worktree " + key + " " + shellQuoteSingle(val) + " 2>/dev/null; then\n")
		b.WriteString("  echo \"[nova-gpg] --worktree config 失败，回落到 --local（key=" + key + "）\" >&2\n")
		b.WriteString("  echo \"NOVA_GPG_WORKTREE_FALLBACK=1\"\n")
		b.WriteString("  git -C " + shellQuoteSingle(wtPath) + " config --local " + key + " " + shellQuoteSingle(val) + "\n")
		b.WriteString("fi\n")
	}
	writePair("user.name", gitName)
	writePair("user.email", gitEmail)
	// Also tee the stdout into the job for live visibility (matches the
	// full provision path).
	var stdoutBuf bytes.Buffer
	out := io.MultiWriter(&stdoutBuf, &jobWriter{job: job})
	exit, err := client.RunScript(ctx, b.String(), "gpg-identity", nil, out)
	if exit != 0 || err != nil {
		return fmt.Errorf("远程写入 git 身份失败（exit=%d）：%w", exit, err)
	}
	if _, fb := parseKeyIDFromScriptOutput(stdoutBuf.String()); fb {
		job.Append(store.LogLine{Type: "message", Content: "⚠️ 当前 git 不支持 worktree 级配置，已回落到仓库级（同项目并发开发时可能相互影响）"})
	}
	return nil
}

// gpgProvisionError wraps the (possibly nil) SSH error with the
// Chinese user-facing message produced by classifyGitSignFailure /
// gpgImportErrorMessage. The handler maps Error() straight into
// claudeStreamOutcome.errMsg.
type gpgProvisionError struct {
	msg   string
	cause error
}

func (e *gpgProvisionError) Error() string {
	if e.cause == nil {
		return e.msg
	}
	return e.msg + "（" + e.cause.Error() + "）"
}
func (e *gpgProvisionError) Unwrap() error { return e.cause }
