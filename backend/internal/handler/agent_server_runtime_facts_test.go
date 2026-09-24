package handler

import (
	"strings"
	"testing"
)

// TestParseRuntimeFactsOutput_ExtraPathsKept pins the regression where
// the parser wiped already-appended extra-paths content the moment it
// saw the __RUNTIME_BIN__ sentinel. The earlier switch-based code reset
// extraPathsLines = []string{} on the sentinel case, which discarded the
// preceding PATH dirs — confirmed in production by Tencent-SG / Tencent-SG002
// where every successful Check / Install persisted claude_bin / node_bin
// but never extra_paths (the column stayed at 0 bytes).
//
// The fix uses a flag (sawSeparator) so the parser stops accumulating
// once the sentinel is seen, instead of clearing an already-populated
// slice. These tests guard both the happy path and the edge cases:
// empty extra-paths, multi-line content, missing sentinel, missing
// marker, and stray content past the marker.
func TestParseRuntimeFactsOutput_ExtraPathsKept(t *testing.T) {
	tests := []struct {
		name             string
		input            string
		wantClaude       string
		wantNode         string
		wantExtraPaths   string
		wantExtraLineCnt int
	}{
		{
			name: "single PATH dir + marker",
			input: "/usr/local/bin\n" +
				"__RUNTIME_BIN__\n" +
				"[nova-agent] RUNTIME_BIN CLAUDE_BIN=/usr/local/bin/claude NODE_BIN=/usr/bin/node",
			wantClaude:       "/usr/local/bin/claude",
			wantNode:         "/usr/bin/node",
			wantExtraPaths:   "/usr/local/bin",
			wantExtraLineCnt: 1,
		},
		{
			// The actual regression — multi-line extra-paths, single
			// marker. The pre-fix parser returned empty extraPaths here.
			name: "multi-line extra-paths preserved",
			input: "/root/.npm-global/bin\n" +
				"/root/.nvm/versions/node/22.0.0/bin\n" +
				"__RUNTIME_BIN__\n" +
				"[nova-agent] RUNTIME_BIN CLAUDE_BIN=/root/.npm-global/bin/claude NODE_BIN=/root/.nvm/versions/node/22.0.0/bin/node",
			wantClaude:       "/root/.npm-global/bin/claude",
			wantNode:         "/root/.nvm/versions/node/22.0.0/bin/node",
			wantExtraPaths:   "/root/.npm-global/bin\n/root/.nvm/versions/node/22.0.0/bin",
			wantExtraLineCnt: 2,
		},
		{
			name: "no extra-paths file (missing cat output)",
			input: "__RUNTIME_BIN__\n" +
				"[nova-agent] RUNTIME_BIN CLAUDE_BIN=/usr/local/bin/claude NODE_BIN=/usr/bin/node",
			wantClaude:       "/usr/local/bin/claude",
			wantNode:         "/usr/bin/node",
			wantExtraPaths:   "",
			wantExtraLineCnt: 0,
		},
		{
			name: "empty marker (claude missing)",
			input: "__RUNTIME_BIN__\n" +
				"[nova-agent] RUNTIME_BIN CLAUDE_BIN= NODE_BIN=/usr/bin/node",
			wantClaude:       "",
			wantNode:         "/usr/bin/node",
			wantExtraPaths:   "",
			wantExtraLineCnt: 0,
		},
		{
			// Stray content past the marker must not leak into extraPaths.
			// If the SSH session appended a debug echo AFTER the marker,
			// the parser should drop it.
			name: "stray content past marker is ignored",
			input: "/usr/local/bin\n" +
				"__RUNTIME_BIN__\n" +
				"[nova-agent] RUNTIME_BIN CLAUDE_BIN=/usr/local/bin/claude NODE_BIN=/usr/bin/node\n" +
				"some random trailing noise that should NOT end up in extraPaths\n",
			wantClaude:       "/usr/local/bin/claude",
			wantNode:         "/usr/bin/node",
			wantExtraPaths:   "/usr/local/bin",
			wantExtraLineCnt: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			claudeBin, nodeBin, extraPaths := parseRuntimeFactsOutput(tc.input)
			if claudeBin != tc.wantClaude {
				t.Errorf("claudeBin = %q, want %q", claudeBin, tc.wantClaude)
			}
			if nodeBin != tc.wantNode {
				t.Errorf("nodeBin = %q, want %q", nodeBin, tc.wantNode)
			}
			if extraPaths != tc.wantExtraPaths {
				t.Errorf("extraPaths = %q, want %q", extraPaths, tc.wantExtraPaths)
			}
			var gotLineCnt int
			if extraPaths != "" {
				gotLineCnt = strings.Count(extraPaths, "\n") + 1
			}
			if gotLineCnt != tc.wantExtraLineCnt {
				t.Errorf("extraPaths line count = %d, want %d", gotLineCnt, tc.wantExtraLineCnt)
			}
		})
	}
}