package ssh

import "testing"

// TestExpandHome covers the "~" expansion contract that the Agent-server
// session sync depends on. pkg/sftp and single-quoted shell commands both
// treat "~" as a literal directory name, so every consumer of a
// "~"-prefixed path must go through this method first (see RemoteFileExists,
// Mkdirp, SyncDirUpMapped, SyncDirDownMapped).
//
// All cases below either bypass the $HOME resolution entirely (non-"~"
// paths, and "~" itself) or pre-seed Client.homeDir, so no test performs a
// real SSH exec of `echo $HOME`.
func TestExpandHome(t *testing.T) {
	tests := []struct {
		name    string
		homeDir string // pre-seeded Client.homeDir; "" means "unresolved"
		path    string
		want    string
		wantErr bool
	}{
		{
			name: "absolute path passes through unchanged",
			path: "/tmp/a/b",
			want: "/tmp/a/b",
		},
		{
			name: "absolute path under a tmpdir worktree passes through",
			path: "/tmp/nova-agent/proj_123/req_456",
			want: "/tmp/nova-agent/proj_123/req_456",
		},
		{
			name: "bare tilde passes through unchanged",
			// expandHome's len(path) < 2 early-return: a lone "~" is not a
			// prefix we rewrite, so it must come back verbatim rather than
			// being joined onto homeDir.
			path: "~",
			want: "~",
		},
		{
			name:    "tilde-prefixed path is joined onto the cached home",
			homeDir: "/home/u",
			path:    "~/.claude/projects/-tmp-nova-agent-proj_123-req_456",
			want:    "/home/u/.claude/projects/-tmp-nova-agent-proj_123-req_456",
		},
		{
			name:    "tilde-slash-prefixed path is joined onto the cached home",
			homeDir: "/root",
			path:    "~/.claude/settings.json",
			want:    "/root/.claude/settings.json",
		},
		{
			name: "tilde followed by a non-slash is not a home prefix",
			// "~foo" is a username-style path, not our "~/" contract; it must
			// pass through so we never silently rewrite it.
			path: "~foo/bar",
			want: "~foo/bar",
		},
		{
			name: "empty path passes through unchanged",
			path: "",
			want: "",
		},
		{
			name: "relative path without a tilde passes through unchanged",
			path: ".claude/projects/x",
			want: ".claude/projects/x",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A Client with no connection would fail loudly if a case
			// accidentally reached the `echo $HOME` branch, which is exactly
			// the behaviour we want from a unit test.
			c := &Client{homeDir: tc.homeDir}

			got, err := c.ExpandHome(tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ExpandHome(%q) = %q, want error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandHome(%q) returned unexpected error: %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("ExpandHome(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestExpandHomeResolvesOnce pins the caching behaviour the callers rely on:
// with homeDir already populated, expansion must not attempt another SSH
// round trip. A Client with a nil connection is the assertion — reaching the
// exec branch would panic or error instead of returning a path.
func TestExpandHomeResolvesOnce(t *testing.T) {
	c := &Client{homeDir: "/home/cached"}

	first, err := c.ExpandHome("~/.claude/projects/x")
	if err != nil {
		t.Fatalf("first ExpandHome returned error: %v", err)
	}
	second, err := c.ExpandHome("~/.claude/projects/x")
	if err != nil {
		t.Fatalf("second ExpandHome returned error: %v", err)
	}
	if first != second {
		t.Errorf("ExpandHome not stable across calls: %q vs %q", first, second)
	}
	if want := "/home/cached/.claude/projects/x"; first != want {
		t.Errorf("ExpandHome = %q, want %q", first, want)
	}
}
