package handler

import (
	"strings"
	"testing"
)

func TestBuildRemoteOriginScriptCoversBothRemoteBranches(t *testing.T) {
	script := buildRemoteOriginScript("/tmp/nova-agent/proj_1/base", "https://tok@github.com/o/r.git", true)

	for _, want := range []string{
		"remote get-url origin",
		"remote set-url origin",
		"remote add origin",
		"set -eu",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n---\n%s", want, script)
		}
	}
	// The existence probe must not abort the script under `set -e`.
	if !strings.Contains(script, "remote get-url origin >/dev/null 2>&1") {
		t.Errorf("get-url probe is not silenced:\n%s", script)
	}
	if !strings.Contains(script, "chmod 600 '/tmp/nova-agent/proj_1/base/.git/config'") {
		t.Errorf("script does not tighten .git/config perms:\n%s", script)
	}
}

func TestBuildRemoteOriginScriptHostHelperGating(t *testing.T) {
	const helper = "config --local credential.helper ''"

	authed := buildRemoteOriginScript("/repo", "https://tok@github.com/o/r.git", true)
	if !strings.Contains(authed, helper) {
		t.Errorf("authed script should sever the host credential helper:\n%s", authed)
	}

	// Without a project token the host helper is the only credential that
	// could make a push work — cutting it would be a regression.
	unauthed := buildRemoteOriginScript("/repo", "https://github.com/o/r.git", false)
	if strings.Contains(unauthed, helper) {
		t.Errorf("unauthed script must keep the host credential helper:\n%s", unauthed)
	}
	if !strings.Contains(unauthed, "remote set-url origin 'https://github.com/o/r.git'") {
		t.Errorf("unauthed script should still normalise origin:\n%s", unauthed)
	}
}

func TestBuildRemoteOriginScriptQuotesShellMetacharacters(t *testing.T) {
	// A token containing a single quote would break out of the quoting and
	// let the rest of the token run as shell commands.
	script := buildRemoteOriginScript("/tmp/na g/base", "https://o'auth2:p'w$x@git.example.com/a b/r.git", true)

	if !strings.Contains(script, `'https://o'\''auth2:p'\''w$x@git.example.com/a b/r.git'`) {
		t.Errorf("origin URL not single-quote escaped:\n%s", script)
	}
	if !strings.Contains(script, `'/tmp/na g/base'`) {
		t.Errorf("baseRepo path not quoted:\n%s", script)
	}
	// No bare `$x` / unquoted space may survive outside a quoted region.
	for _, line := range strings.Split(script, "\n") {
		if strings.Count(line, "'")%2 != 0 {
			t.Errorf("unbalanced quotes in line: %q", line)
		}
	}
}

func TestRedactOriginForLogHidesToken(t *testing.T) {
	got := redactOriginForLog("https://user:glpat-secret@gitlab.example.com/p.git")
	if strings.Contains(got, "glpat-secret") || strings.Contains(got, "user") {
		t.Errorf("token leaked into log form: %q", got)
	}
	if got != "https://<redacted>@gitlab.example.com/p.git" {
		t.Errorf("unexpected redaction: %q", got)
	}
}

func TestRedactRemoteScriptOutputHandlesBothUserinfoShapes(t *testing.T) {
	in := "fatal: https://ghp-abc@github.com/o/r.git rejected\n" +
		"hint: try https://oauth2:glpat-xyz@gitlab.example.com/o/r.git\n"
	got := redactRemoteScriptOutput(in)

	for _, secret := range []string{"ghp-abc", "glpat-xyz", "oauth2"} {
		if strings.Contains(got, secret) {
			t.Errorf("%q leaked through redaction: %q", secret, got)
		}
	}
	if !strings.Contains(got, "https://<redacted>@github.com/o/r.git") ||
		!strings.Contains(got, "https://<redacted>@gitlab.example.com/o/r.git") {
		t.Errorf("hosts should survive redaction: %q", got)
	}
}
