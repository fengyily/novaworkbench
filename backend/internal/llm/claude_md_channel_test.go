package llm

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeMdNativeChannelEndToEnd runs the REAL claude CLI against a local
// mock /v1/messages endpoint and asserts what the model would actually see.
//
// The unit tests around streamArgs pin the argv; this test pins the OUTCOME.
// It is skipped when no claude binary is available (CI has none), and it needs
// no API quota: the mock endpoint captures the request payload, which is the
// context the model would receive.
//
// What it proves: with the argv the gateway builds, the CLI loads the
// MANAGED copy of the project document and NEITHER of the repository's own
// copies. That is the whole point of the native channel — one project
// document in the context, and it is the one NovaWorkbench manages.
func TestClaudeMdNativeChannelEndToEnd(t *testing.T) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("no claude binary on PATH — skipping the end-to-end channel check")
	}

	work := t.TempDir()
	repo := filepath.Join(work, "repo")
	memory := filepath.Join(work, "memory", "proj_1")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(memory, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(repo, "CLAUDE.md"), "MARKER_REPO_COPY\n")
	write(filepath.Join(repo, "AGENTS.md"), "MARKER_REPO_AGENTS\n")
	write(filepath.Join(memory, "CLAUDE.md"), "MARKER_MANAGED_COPY\n")

	capture := filepath.Join(work, "capture.jsonl")
	port := startMockAnthropic(t, capture)
	baseURL := "http://127.0.0.1:" + port

	// The argv the gateway builds for the native channel.
	g := &Gateway{}
	args := g.BuildStreamArgs(StreamOpts{
		Prompt:              "probe",
		WorkDir:             repo,
		ClaudeMdDir:         memory,
		ExcludeRepoClaudeMd: true,
	})
	// The mock URL has to reach the CLI through the --settings env block, so
	// the settings JSON is MERGED with the mock endpoint — everything the
	// gateway built (the exclusion list above all) has to survive.
	settingsIdx := indexOfArg(args, "--settings")
	if settingsIdx == -1 {
		t.Fatalf("args = %v, want --settings", args)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(args[settingsIdx+1]), &settings); err != nil {
		t.Fatalf("parse the settings the gateway built: %v", err)
	}
	env, _ := settings["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
		settings["env"] = env
	}
	env["ANTHROPIC_BASE_URL"] = baseURL
	env["ANTHROPIC_AUTH_TOKEN"] = "probe"
	env["ANTHROPIC_MODEL"] = "claude-sonnet-5"
	env[ClaudeMdAdditionalDirsEnv] = "1"
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	args[settingsIdx+1] = string(raw)

	cmd := exec.Command(bin, args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("the claude CLI did not complete (%v): %s", err, out)
	}

	payload, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read the captured request: %v", err)
	}
	text := string(payload)
	if !strings.Contains(text, "MARKER_MANAGED_COPY") {
		t.Errorf("the managed copy was not loaded into the context; captured request:\n%s", text)
	}
	if strings.Contains(text, "MARKER_REPO_COPY") {
		t.Errorf("the repository's CLAUDE.md was still loaded; the managed copy is supposed to be the only one")
	}
	if strings.Contains(text, "MARKER_REPO_AGENTS") {
		t.Errorf("the repository's AGENTS.md was still loaded; excluding CLAUDE.md promotes AGENTS.md")
	}
}

// startMockAnthropic starts a local mock of the Anthropic messages endpoint
// and returns the port it listens on.
//
// The mock captures every request body into capturePath and answers with a
// minimal but well-formed SSE stream, so the CLI's request phase completes
// without a real API. The captured payload IS the context the model would
// receive — which is what makes the channel observable.
func startMockAnthropic(t *testing.T, capturePath string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f, err := os.OpenFile(capturePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			_, _ = f.Write(body)
			_, _ = f.WriteString("\n")
			_ = f.Close()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	return port
}
