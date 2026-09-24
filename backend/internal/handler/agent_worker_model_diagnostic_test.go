package handler

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression coverage for "每次远端执行都报 preflight 失败（unrecognized_model），
// exit_code 为空".
//
// Claude Code writes
//
//	[claude-code:unrecognized_model] {"model":"MiniMax-M3","query_source":"sdk"}
//
// to stderr for EVERY request whose model id is not in its local catalog — i.e.
// every request for a custom model behind a private base URL. It is a
// diagnostic, not a rejection: verified against a stub Anthropic endpoint,
// `claude --print ping` prints that line, streams the answer and exits 0.
// nova-agent-worker's preflight used to fast-fail on any classified stderr
// chunk, so it SIGTERM'd that healthy probe and reported a failure with a null
// exit code — every remote run died before the real command ever spawned.
//
// These tests exercise the classifier inside the EMBEDDED worker source (the
// copy a packaged NovaWorkbench uploads), by extracting classifyError and its
// helpers and running them under node. They need node on PATH; without it the
// test skips rather than passing vacuously.

// classifierSource slices classifyError + its helpers out of the embedded
// worker. The markers are the function header and the comment that opens the
// next function; if either moves, this fails loudly instead of silently
// testing nothing.
func classifierSource(t *testing.T) string {
	t.Helper()
	const startMarker = "function classifyError(err, stderr) {"
	const endMarker = "// serializeCLIError shapes"
	start := strings.Index(agentWorkerServerMJS, startMarker)
	if start < 0 {
		t.Fatalf("marker %q not found in embedded worker; update this test alongside server.mjs", startMarker)
	}
	end := strings.Index(agentWorkerServerMJS[start:], endMarker)
	if end < 0 {
		t.Fatalf("marker %q not found after classifyError; update this test alongside server.mjs", endMarker)
	}
	return agentWorkerServerMJS[start : start+end]
}

// runClassifier evaluates the extracted source under node and returns the
// category each of classifyError / classifyFatalError assigns to stderr.
func runClassifier(t *testing.T, stderr string) (regular, fatal string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping worker classifier test")
	}
	payload, err := json.Marshal(stderr)
	if err != nil {
		t.Fatalf("marshal stderr: %v", err)
	}
	script := classifierSource(t) + "\nconst stderr = " + string(payload) + ";\n" +
		"console.log(JSON.stringify({regular: classifyError(null, stderr), fatal: classifyFatalError(null, stderr)}));\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "classify.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	var got struct {
		Regular string `json:"regular"`
		Fatal   string `json:"fatal"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("decode node output %q: %v", out, err)
	}
	return got.Regular, got.Fatal
}

const modelDiagnosticLine = `[claude-code:unrecognized_model] {"model":"MiniMax-M3","query_source":"sdk"}`

func TestWorkerClassifierIgnoresModelDiagnosticWhileRunning(t *testing.T) {
	cases := []struct {
		name string
		// stderr as the preflight probe accumulates it.
		stderr string
		// what the live fast-fail path must see ("unknown" = keep waiting).
		wantFatal string
		// what a post-mortem (non-zero exit) classification must see.
		wantRegular string
	}{
		{
			name:        "diagnostic alone must not fast-fail: the probe is alive and about to exit 0",
			stderr:      modelDiagnosticLine + "\n",
			wantFatal:   "unknown",
			wantRegular: "unrecognized_model",
		},
		{
			name:        "diagnostic must not shadow the real failure that follows it",
			stderr:      modelDiagnosticLine + "\nAPI Error: 401 Unauthorized\n",
			wantFatal:   "auth_failed",
			wantRegular: "auth_failed",
		},
		{
			name:        "diagnostic must not shadow a network failure either",
			stderr:      modelDiagnosticLine + "\nError: getaddrinfo ENOTFOUND api.minimax.cn\n",
			wantFatal:   "dns_unresolved",
			wantRegular: "dns_unresolved",
		},
		{
			name: "the CLI's real model rejection still classifies as unrecognized_model",
			stderr: modelDiagnosticLine + "\n" +
				"There's an issue with the selected model (MiniMax-M3). It may not exist or you may not have access to it.\n",
			wantFatal:   "unrecognized_model",
			wantRegular: "unrecognized_model",
		},
		{
			name:        "clean stderr stays unknown",
			stderr:      "",
			wantFatal:   "unknown",
			wantRegular: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			regular, fatal := runClassifier(t, tc.stderr)
			if fatal != tc.wantFatal {
				t.Errorf("classifyFatalError = %q, want %q", fatal, tc.wantFatal)
			}
			if regular != tc.wantRegular {
				t.Errorf("classifyError = %q, want %q", regular, tc.wantRegular)
			}
		})
	}
}

// workerSourceBetween slices the embedded worker between two markers, so the
// tests below run the shipped code rather than a paraphrase of it.
func workerSourceBetween(t *testing.T, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(agentWorkerServerMJS, startMarker)
	if start < 0 {
		t.Fatalf("marker %q not found in embedded worker; update this test alongside server.mjs", startMarker)
	}
	end := strings.Index(agentWorkerServerMJS[start:], endMarker)
	if end < 0 {
		t.Fatalf("marker %q not found after %q; update this test alongside server.mjs", endMarker, startMarker)
	}
	return agentWorkerServerMJS[start : start+end]
}

// runWorkerPreflight drives the worker's real preflight() against a stub
// `claude` and returns its resolved result plus how long it took.
func runWorkerPreflight(t *testing.T, stubScript string) (map[string]any, time.Duration) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping worker preflight test")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	if err := os.WriteFile(stub, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub claude: %v", err)
	}
	driver := "import { spawn } from 'node:child_process';\n" +
		classifierSource(t) + "\n" +
		workerSourceBetween(t, "const PREFLIGHT_TIMEOUT_MS", "// resolveTmpdir returns") + "\n" +
		"preflight(process.cwd(), { cmd: process.argv[2], env: process.env }, '').then((r) => {\n" +
		"  console.log(JSON.stringify(r));\n" +
		"  process.exit(0);\n" +
		"});\n"
	path := filepath.Join(dir, "preflight.mjs")
	if err := os.WriteFile(path, []byte(driver), 0o600); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	started := time.Now()
	out, err := exec.Command(node, path, stub).Output()
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("decode node output %q: %v", out, err)
	}
	return got, elapsed
}

// The bug as the user hit it: a claude that prints the model diagnostic and
// then answers normally. Before the fix the first stderr chunk SIGTERM'd the
// probe and the whole run died with "preflight 失败（unrecognized_model）"
// and an empty exit code.
func TestWorkerPreflightSurvivesModelDiagnostic(t *testing.T) {
	got, _ := runWorkerPreflight(t, "#!/bin/sh\n"+
		"echo '"+modelDiagnosticLine+"' >&2\n"+
		"sleep 0.2\n"+
		"echo pong\n"+
		"exit 0\n")
	if ok, _ := got["ok"].(bool); !ok {
		t.Fatalf("preflight must pass when the CLI only printed the diagnostic and exited 0; got %#v", got)
	}
	if w, _ := got["warning"].(string); w == "" {
		t.Errorf("preflight should carry a warning so the job panel explains the diagnostic line; got %#v", got)
	}
}

// The fast-fail path must still work: a real failure that arrives on the same
// stderr as the diagnostic has to resolve immediately (and with the right
// category) instead of waiting out PREFLIGHT_TIMEOUT_MS.
func TestWorkerPreflightStillFastFailsOnRealError(t *testing.T) {
	got, elapsed := runWorkerPreflight(t, "#!/bin/sh\n"+
		"echo '"+modelDiagnosticLine+"' >&2\n"+
		"echo 'API Error: 401 Unauthorized' >&2\n"+
		"sleep 60\n")
	if ok, _ := got["ok"].(bool); ok {
		t.Fatalf("preflight must fail on a 401; got %#v", got)
	}
	if cat, _ := got["errorCategory"].(string); cat != "auth_failed" {
		t.Errorf("errorCategory = %v, want auth_failed (the diagnostic must not shadow it); got %#v", got["errorCategory"], got)
	}
	if elapsed > 10*time.Second {
		t.Errorf("fast-fail took %s; it should resolve as soon as the 401 hits stderr, not wait for the timeout", elapsed)
	}
}

// The fix only reaches an already-provisioned agent host if the version bump
// marks its running worker stale (isStaleWorker / the Check flow compare
// against agentWorkerVersion), so pin the floor.
func TestAgentWorkerVersionCoversModelDiagnosticFix(t *testing.T) {
	if agentWorkerVersion == "0.4.0" {
		t.Fatalf("agentWorkerVersion must be bumped past 0.4.0 so hosts running the fast-failing preflight are hot-upgraded")
	}
	if !strings.Contains(agentWorkerServerMJS, "classifyFatalError") {
		t.Fatalf("embedded worker is out of sync with agent-worker/server.mjs; run scripts/sync-agent-worker.sh")
	}
}
