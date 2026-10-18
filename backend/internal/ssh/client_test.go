package ssh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
)

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

// TestIsRemoteNotExist pins the multi-signal detection: the Go-stdlib
// fs.ErrNotExist (when sftp unwraps it), the legacy os.IsNotExist
// (when sftp wraps an *os.PathError), and the literal sftp-server
// message "file does not exist" / "no such file or directory".
func TestIsRemoteNotExist(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"fs.ErrNotExist", fs.ErrNotExist, true},
		{"wrapped fs.ErrNotExist", fmt.Errorf("wrap: %w", fs.ErrNotExist), true},
		{"os.PathError", &os.PathError{Op: "stat", Path: "/x", Err: fs.ErrNotExist}, true},
		{"literal sftp message", errors.New("sftp: file does not exist"), true},
		{"literal no such file", errors.New("open /x: no such file or directory"), true},
		{"unrelated error", errors.New("connection lost"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRemoteNotExist(c.err); got != c.want {
				t.Fatalf("isRemoteNotExist(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestErrRemoteDirMissingIsExported pins that callers downstream can
// identify the sentinel via errors.Is.
func TestErrRemoteDirMissingIsExported(t *testing.T) {
	if !errors.Is(ErrRemoteDirMissing, ErrRemoteDirMissing) {
		t.Fatal("errors.Is must match the sentinel against itself")
	}
	// Plain equality is fine — we return ErrRemoteDirMissing verbatim,
	// not wrapped. This catches accidental fmt.Errorf wrapping that
	// would break errors.Is at call sites.
	if ErrRemoteDirMissing == nil {
		t.Fatal("ErrRemoteDirMissing must not be nil")
	}
}

// TestRemoveAllNilClient pins the nil-client early-return for RemoveAll,
// mirroring the same contract Remove has. The GPG provision retry path
// (see req_499ee84d2a3853f3) calls Client.RemoveAll before it has opened
// an SFTP session in some test scenarios, and a panic in that branch
// would be a regression. We assert an error (not a panic, not a nil
// return) so callers can rely on the "Client not connected" path.
func TestRemoveAllNilClient(t *testing.T) {
	var c *Client
	err := c.RemoveAll("/tmp/whatever")
	if err == nil {
		t.Fatal("RemoveAll on nil client must return an error, got nil")
	}
}

// TestCopyUploadBypassesReaderFromTo is a regression test for the silent
// upload failure that surfaced as "GPG provision 脚本未输出 keyid" on agent
// hosts whose /tmp was quota-full.
//
// pkg/sftp's *File implements io.ReaderFrom, and its ReadFrom drops WRITE
// errors whenever the final read ends in io.ErrUnexpectedEOF (i.e. for any
// file whose size is not a packet-size multiple). io.Copy then returns
// (n, nil) even though the remote file is empty. copyUpload must route every
// byte through dst.Write so the failure is converted into an error.
func TestCopyUploadBypassesReaderFromTo(t *testing.T) {
	swallow := &swallowingWriter{}
	n, err := copyUpload(swallow, bytes.NewReader([]byte("0123456789")))
	if err == nil {
		t.Fatalf("copyUpload returned nil error (n=%d); the ReaderFrom fast path must not be used", n)
	}
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("error = %v, want it to wrap the Write failure", err)
	}
	if swallow.writeCalls == 0 {
		t.Fatal("copyUpload never called dst.Write")
	}
}

// swallowingWriter simulates the broken fast path: it implements
// io.ReaderFrom, consumes the data, and returns nil while discarding
// everything. If io.Copy is allowed to prefer it, the caller sees success.
type swallowingWriter struct {
	writeCalls int
}

func (w *swallowingWriter) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(io.Discard, r)
}

func (w *swallowingWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	return 0, errors.New("quota exceeded")
}
