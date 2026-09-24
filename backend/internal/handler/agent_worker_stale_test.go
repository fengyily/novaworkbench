package handler

import (
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// Regression coverage for the second half of the "runtime paths detected but
// runs still die with `spawn claude ENOENT`" bug: NovaWorkbench correctly sent
// the absolute claude path, but the agent host was still running a server.mjs
// older than the release that taught the worker to read it. The pin was
// dropped on the floor, the job log announced a fix that never applied, and
// nothing on the run path noticed the version gap.

func TestWorkerCodeStale(t *testing.T) {
	cases := []struct {
		name  string
		given workerHealth
		want  bool
	}{
		{"worker matches this binary", workerHealth{WorkerVersion: agentWorkerVersion}, false},
		{"worker predates the claudeBin pin", workerHealth{WorkerVersion: "0.3.5"}, true},
		{"worker too old to report a version at all", workerHealth{}, true},
		{
			"a worker that resolves claude is still stale if its code is old",
			workerHealth{WorkerVersion: "0.3.5", ClaudeVersion: "1.2.3"},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workerCodeStale(tc.given); got != tc.want {
				t.Errorf("workerCodeStale(%+v) = %v, want %v", tc.given, got, tc.want)
			}
		})
	}
}

func TestRuntimeHintsFor(t *testing.T) {
	cases := []struct {
		name          string
		srv           *model.AgentServer
		wantClaudeBin string
		wantDirs      []string
	}{
		{
			name:     "nil row yields nothing known",
			srv:      nil,
			wantDirs: nil,
		},
		{
			name:     "row predating the runtime columns yields nothing known",
			srv:      &model.AgentServer{ID: "agent_x"},
			wantDirs: nil,
		},
		{
			// The Tencent-SG002 shape. dirname(claude_bin) happens to equal the
			// recorded extra path here; composeWorkerPATH dedupes, so carrying
			// both is harmless and covers hosts where they differ.
			name: "nvm host: claude_bin's directory is folded into the PATH dirs",
			srv: &model.AgentServer{
				ID:         "agent_b87c8eeccb6a3701",
				ClaudeBin:  "/home/ubuntu/.nvm/versions/node/v24.21.0/bin/claude",
				ExtraPaths: "/home/ubuntu/.nvm/versions/node/v24.21.0/bin\n",
			},
			wantClaudeBin: "/home/ubuntu/.nvm/versions/node/v24.21.0/bin/claude",
			wantDirs: []string{
				"/home/ubuntu/.nvm/versions/node/v24.21.0/bin",
				"/home/ubuntu/.nvm/versions/node/v24.21.0/bin",
			},
		},
		{
			// The case the extra-paths file alone cannot cover: claude lives
			// somewhere the install script never wrote down.
			name: "claude_bin contributes a directory extra_paths does not list",
			srv: &model.AgentServer{
				ID:         "agent_x",
				ClaudeBin:  "/opt/custom/bin/claude",
				ExtraPaths: "/usr/local/bin\n",
			},
			wantClaudeBin: "/opt/custom/bin/claude",
			wantDirs:      []string{"/usr/local/bin", "/opt/custom/bin"},
		},
		{
			// A relative / garbage value must not turn into a PATH entry:
			// dirname("claude") is "." and an empty-ish PATH segment means
			// "current directory" to execvp.
			name:          "a non-absolute claude_bin contributes no directory",
			srv:           &model.AgentServer{ID: "agent_x", ClaudeBin: "claude"},
			wantClaudeBin: "claude",
			wantDirs:      nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runtimeHintsFor(tc.srv)
			if got.ClaudeBin != tc.wantClaudeBin {
				t.Errorf("ClaudeBin: got %q, want %q", got.ClaudeBin, tc.wantClaudeBin)
			}
			if len(got.ExtraDirs) != len(tc.wantDirs) {
				t.Fatalf("ExtraDirs: got %#v, want %#v", got.ExtraDirs, tc.wantDirs)
			}
			for i := range got.ExtraDirs {
				if got.ExtraDirs[i] != tc.wantDirs[i] {
					t.Errorf("ExtraDirs[%d]: got %q, want %q (full %#v)", i, got.ExtraDirs[i], tc.wantDirs[i], got.ExtraDirs)
				}
			}
		})
	}
}

// The failure that sent this bug round a second time was unreadable: the job
// log said "改为按已记录的绝对路径执行 /home/.../claude" and the next line was
// still `spawn claude ENOENT`, with nothing to say which of the two the worker
// actually used. These fields (emitted by worker >=0.4.0) close that gap.
func TestExtractStreamError_EchoesWhatTheWorkerActuallySpawned(t *testing.T) {
	msg := extractStreamError(map[string]interface{}{
		"error":             "spawn claude ENOENT",
		"errorCategory":     "cli_not_found",
		"stderr":            "spawn claude ENOENT",
		"code":              "ENOENT",
		"resolvedClaudeBin": "claude",
		"resolvedPath":      "/usr/local/bin:/usr/bin:/bin",
	})
	for _, want := range []string{
		"[worker 实际执行] claude",
		"[worker PATH] /usr/local/bin:/usr/bin:/bin",
		agentWorkerVersion, // the "your server.mjs is too old" diagnosis
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

// When the worker DID honour the pin, the stale-code diagnosis must not fire —
// otherwise it would send the operator to reinstall a worker that is current
// and misdirect them away from the real cause (claude genuinely gone).
func TestExtractStreamError_NoStaleWorkerDiagnosisWhenPinWasHonoured(t *testing.T) {
	msg := extractStreamError(map[string]interface{}{
		"error":             "spawn /home/ubuntu/.nvm/versions/node/v24.21.0/bin/claude ENOENT",
		"errorCategory":     "cli_not_found",
		"resolvedClaudeBin": "/home/ubuntu/.nvm/versions/node/v24.21.0/bin/claude",
	})
	if !strings.Contains(msg, "[worker 实际执行] /home/ubuntu/.nvm/versions/node/v24.21.0/bin/claude") {
		t.Errorf("expected the resolved absolute path to be echoed:\n%s", msg)
	}
	if strings.Contains(msg, "server.mjs 早于") {
		t.Errorf("stale-worker diagnosis fired even though the pin was honoured:\n%s", msg)
	}
}

// Workers older than 0.4.0 don't emit the fields at all; their absence must
// not produce empty "[worker 实际执行]" noise.
func TestExtractStreamError_OmitsResolvedFieldsWhenWorkerDidNotSendThem(t *testing.T) {
	msg := extractStreamError(map[string]interface{}{
		"error":         "spawn claude ENOENT",
		"errorCategory": "cli_not_found",
	})
	if strings.Contains(msg, "[worker 实际执行]") || strings.Contains(msg, "[worker PATH]") {
		t.Errorf("empty diagnostics rendered:\n%s", msg)
	}
}
