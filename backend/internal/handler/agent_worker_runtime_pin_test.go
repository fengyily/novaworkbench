package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
)

// Regression coverage for the "runtime paths detected in the UI but remote
// runs still die with `spawn claude ENOENT`" bug: agent_servers.claude_bin /
// .extra_paths were persisted but never sent to the worker, which resolved
// `claude` purely through its own launch-time PATH.

func TestSplitExtraPathLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "the shape captureInstallRuntimeFacts writes for an nvm host",
			in:   "/home/ubuntu/.nvm/versions/node/v24.21.0/bin\n",
			want: []string{"/home/ubuntu/.nvm/versions/node/v24.21.0/bin"},
		},
		{
			name: "blank lines, comments and surrounding whitespace are dropped",
			in:   "\n  /opt/homebrew/bin  \n# a comment\n\n/usr/local/bin\n",
			want: []string{"/opt/homebrew/bin", "/usr/local/bin"},
		},
		{
			name: "relative entries are rejected: they cannot locate a binary for a process whose cwd we do not control",
			in:   "bin\n./node_modules/.bin\n/usr/local/bin",
			want: []string{"/usr/local/bin"},
		},
		{
			name: "duplicates collapse so the joined PATH stays short",
			in:   "/usr/local/bin\n/usr/local/bin\n/opt/homebrew/bin",
			want: []string{"/usr/local/bin", "/opt/homebrew/bin"},
		},
		{
			name: "empty input yields no entries rather than one empty string",
			in:   "\n\n  \n",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitExtraPathLines(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("index %d: got %q, want %q (full: %#v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// An empty extra_paths column must not produce ExtraPaths:":" or a stray
// leading/trailing colon — an empty PATH segment means "current directory"
// to execvp, which would be a quiet security downgrade on the agent host.
func TestSplitExtraPathLines_NoEmptySegmentsWhenJoined(t *testing.T) {
	joined := strings.Join(splitExtraPathLines("\n\n#only comments\n"), ":")
	if joined != "" {
		t.Fatalf("expected empty join, got %q", joined)
	}
	joined = strings.Join(splitExtraPathLines("/a\n\n/b\n"), ":")
	if joined != "/a:/b" {
		t.Fatalf("expected %q, got %q", "/a:/b", joined)
	}
}

func TestWorkerRunRequest_PinsRuntimePathsFromAgentServer(t *testing.T) {
	srv := &model.AgentServer{
		ID:         "agent_b87c8eeccb6a3701",
		ClaudeBin:  "/home/ubuntu/.nvm/versions/node/v24.21.0/bin/claude",
		ExtraPaths: "/home/ubuntu/.nvm/versions/node/v24.21.0/bin\n/usr/local/bin\n",
	}
	body := workerRunRequest(llm.StreamOpts{WorkDir: "/tmp/x", Prompt: "hi"}, nil, srv)

	if body.ClaudeBin != srv.ClaudeBin {
		t.Errorf("ClaudeBin: got %q, want %q", body.ClaudeBin, srv.ClaudeBin)
	}
	// Newline-separated in the DB, colon-separated on the wire.
	want := "/home/ubuntu/.nvm/versions/node/v24.21.0/bin:/usr/local/bin"
	if body.ExtraPaths != want {
		t.Errorf("ExtraPaths: got %q, want %q", body.ExtraPaths, want)
	}
}

// Backward compatibility: with nothing recorded (or no server at all) the two
// fields must be absent from the JSON entirely, so an older worker — which
// destructures a fixed field list and would otherwise see claudeBin:"" — keeps
// resolving `claude` off its own PATH exactly as before.
func TestWorkerRunRequest_OmitsRuntimePathsWhenUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  *model.AgentServer
	}{
		{"nil server", nil},
		{"columns never populated", &model.AgentServer{ID: "agent_x"}},
		{"columns hold only whitespace", &model.AgentServer{ID: "agent_x", ClaudeBin: "  ", ExtraPaths: "\n  \n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := workerRunRequest(llm.StreamOpts{WorkDir: "/tmp/x", Prompt: "hi"}, nil, tc.srv)
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(raw), "claudeBin") {
				t.Errorf("claudeBin leaked into the wire format: %s", raw)
			}
			if strings.Contains(string(raw), "extraPaths") {
				t.Errorf("extraPaths leaked into the wire format: %s", raw)
			}
		})
	}
}

func TestWorkerHealth_ClaudeResolvable(t *testing.T) {
	cases := []struct {
		name  string
		given workerHealth
		want  bool
	}{
		{"worker ran claude --version fine", workerHealth{ClaudeVersion: "1.2.3 (Claude Code)"}, true},
		{"worker could not spawn claude", workerHealth{ClaudeVersion: "unknown"}, false},
		{"pre-0.4.0 worker omits the field", workerHealth{}, false},
		{"whitespace-only is not a version", workerHealth{ClaudeVersion: "   "}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.given.ClaudeResolvable(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The nohup launch line is the only place the corrected PATH reaches a
// worker that systemd isn't managing, so assert the env plumbing survives
// refactors — and that killOld=false (the plain revive path) does not carry
// a pkill that would take down a healthy worker.
func TestNohupLaunchWorker(t *testing.T) {
	const dir = "/home/ubuntu/nova-agent-worker"
	const path = "/home/ubuntu/.nvm/versions/node/v24.21.0/bin:/usr/bin"

	withKill := nohupLaunchWorker(dir, path, true)
	for _, want := range []string{
		"NOVA_AGENT_WORKER_EXTRA_PATHS='" + path + "'",
		"PATH='" + path + "'",
		"pgrep -f nova-agent-worker/server.mjs",
		dir + "/server.mjs",
	} {
		if !strings.Contains(withKill, want) {
			t.Errorf("killOld=true launch line missing %q:\n%s", want, withKill)
		}
	}

	noKill := nohupLaunchWorker(dir, path, false)
	if strings.Contains(noKill, "kill ") {
		t.Errorf("killOld=false must not kill an existing process:\n%s", noKill)
	}
	if !strings.Contains(noKill, "PATH='"+path+"'") {
		t.Errorf("killOld=false launch line lost its PATH:\n%s", noKill)
	}
}

// A PATH containing sed's replacement metacharacters must not be able to
// corrupt the systemd unit repairWorkerPATH rewrites in place.
func TestSedReplacementEscape(t *testing.T) {
	cases := map[string]string{
		"/usr/bin:/bin":      "/usr/bin:/bin",
		`/opt/a|b/bin`:       `/opt/a\|b/bin`,
		`/opt/a&b/bin`:       `/opt/a\&b/bin`,
		`/opt/a\b/bin`:       `/opt/a\\b/bin`,
		`/x&/y|z/w\ command`: `/x\&/y\|z/w\\ command`,
	}
	for in, want := range cases {
		if got := sedReplacementEscape(in); got != want {
			t.Errorf("sedReplacementEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
