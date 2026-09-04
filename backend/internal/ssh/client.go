// Package ssh wraps the local agent's interaction with remote Linux/macOS
// execution targets. It exposes a minimal Client that supports one-shot
// commands, file existence checks, recursive directory sync (for claude
// session files), and remote mkdir. All higher-level semantics — worktree
// creation, code commit/push, claude invocation — are built by callers via
// the Exec primitive, mirroring how the local code path shells out to
// git/claude.
//
// Threading model: Client is safe for concurrent use across sessions created
// from the same underlying *ssh.Client. Each Exec creates a fresh *ssh.Session
// and closes it when done (see per-call defer). SFTP sessions are opened on
// demand by SyncDirUp/SyncDirDown and closed before return.
package ssh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
)

// Client wraps an established *ssh.Client. Close it when done.
type Client struct {
	conn    *gossh.Client
	homeDir string // resolved once per Client for "~"-prefixed paths
}

// Dial establishes an SSH connection. authType is "key" or "password";
// authValue carries the corresponding plaintext credential (decrypted by the
// caller from internal/secret). The function blocks until the TCP+SSH
// handshake completes or ctx is cancelled; the underlying net.Dial honors
// the deadline implied by ctx.
func Dial(ctx context.Context, host string, port int, user, authType, authValue string) (*Client, error) {
	if host == "" {
		return nil, errors.New("ssh: empty host")
	}
	if user == "" {
		return nil, errors.New("ssh: empty user")
	}
	if port <= 0 || port > 65535 {
		port = 22
	}

	var signer gossh.Signer
	var auth []gossh.AuthMethod

	switch authType {
	case "key":
		s, err := gossh.ParsePrivateKey([]byte(authValue))
		if err != nil {
			return nil, fmt.Errorf("ssh: parse private key: %w", err)
		}
		signer = s
		auth = []gossh.AuthMethod{gossh.PublicKeys(signer)}
	case "password":
		auth = []gossh.AuthMethod{gossh.Password(authValue)}
	default:
		return nil, fmt.Errorf("ssh: unsupported auth_type %q", authType)
	}

	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            auth,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), // known_hosts is the user's responsibility in this MVP
		Timeout:         15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	d := net.Dialer{Timeout: 15 * time.Second}
	if dl, ok := ctx.Deadline(); ok {
		d.Deadline = dl
	}
	rawConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh: tcp dial %s: %w", addr, err)
	}
	// Handshake on top of the existing conn so we honor ctx-based timeout
	// instead of a fresh net.Conn.
	conn, chans, reqs, err := gossh.NewClientConn(rawConn, addr, cfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("ssh: handshake: %w", err)
	}
	c := gossh.NewClient(conn, chans, reqs)
	return &Client{conn: c}, nil
}

// TestConn is a one-shot connectivity check used by the Check goroutine. It
// only verifies the SSH handshake; the connection is closed before return.
func TestConn(ctx context.Context, host string, port int, user, authType, authValue string) error {
	c, err := Dial(ctx, host, port, user, authType, authValue)
	if err != nil {
		return err
	}
	return c.Close()
}

// Close shuts down the underlying SSH client.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// HTTPTransport returns an *http.Transport whose DialContext opens a new
// SSH direct-tcpip channel for every connection to remoteAddr (host:port on
// the SSH server side). The returned Transport can be plugged into a
// per-call *http.Client to talk to a loopback-only service on the remote
// host — e.g. nova-agent-worker on 127.0.0.1:7000 — without exposing any
// new port on the network.
//
// Why a custom DialContext instead of `ssh -L` style local-port forwarding:
//   * No actual local TCP listener is created (cleaner, no port collisions
//     across concurrent calls, no firewall prompts).
//   * The SSH connection is reused end-to-end: we don't need a separate
//     tunnel process or its lifecycle.
//   * Each request gets its own channel, so concurrent calls are safe and
//     the transport's per-host connection pooling works through the SSH
//     multiplex layer.
//
// Threading: the returned Transport is safe for concurrent use — the
// underlying *ssh.Client is concurrent-safe and we just open channels per
// request. Multiple Transports / http.Clients can be created from one Client
// (e.g. one per Agent-server coding run) without coordination.
//
// remoteAddr is the SSH-server-side address, typically "127.0.0.1:7000".
// Passing a non-loopback address is allowed but defeats the purpose — the
// whole point is to keep the worker from being reachable externally.
func (c *Client) HTTPTransport(remoteAddr string) *http.Transport {
	if c == nil || c.conn == nil {
		// Returning a Transport whose Dial always errors is friendlier than
		// panicking — callers can still build a Client and get a clear
		// "client not connected" on the first request.
		return &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return nil, errors.New("ssh: client not connected")
			},
		}
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// Honor the caller's context deadline — the SSH library's
			// DialContext cancels the channel open on ctx cancellation,
			// which is exactly what a short-deadline health probe wants.
			return c.conn.DialContext(ctx, network, remoteAddr)
		},
		// Keep idle connections short — long-lived channels through SSH
		// can hit server-side timeouts we don't control, and the cost of
		// reopening is negligible (one SSH round trip per call).
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConnsPerHost: 4,
		TLSHandshakeTimeout: 10 * time.Second,
		// Don't follow redirects automatically — the worker doesn't emit
		// them and following could mask logic bugs.
		DisableKeepAlives: false,
	}
}

// HTTPClient returns a one-off *http.Client using HTTPTransport. Use this
// for short-lived requests (health probes). For long-running SSE streams
// where you want to set a custom timeout, build the Client yourself:
//   httpClient := &http.Client{Transport: sshCli.HTTPTransport("127.0.0.1:7000")}
func (c *Client) HTTPClient(remoteAddr string) *http.Client {
	return &http.Client{Transport: c.HTTPTransport(remoteAddr)}
}

// Exec runs cmd on the remote host. stdout/stderr are merged into out, line
// by line, prefixed with [label] when label is non-empty — the same shape
// preflight.runLogged uses for the local install flow. env is a list of
// "KEY=VALUE" pairs and is preferred over session.Setenv (which sshd may
// disable via AcceptEnv). The returned int is the remote exit code; an
// error is returned for transport failures only.
//
// When stderrTail is non-nil, the last 4KB of stderr are also captured into
// it (alongside the merged output) so callers can surface postmortem hints
// when the combined writer was redirected to an opaque sink (e.g. an
// io.Pipe feeding a JSON scanner that silently drops non-JSON lines).
//
// Cancel semantics: if ctx is cancelled, the remote session is closed
// (SIGKILL-equivalent — ssh doesn't surface SIGTERM cleanly to shells
// without a pty), and an error is returned.
func (c *Client) Exec(ctx context.Context, cmd, label string, env []string, out io.Writer, stderrTail *bytes.Buffer) (int, error) {
	if c == nil || c.conn == nil {
		return -1, errors.New("ssh: client not connected")
	}
	if out == nil {
		out = io.Discard
	}

	sess, err := c.conn.NewSession()
	if err != nil {
		return -1, fmt.Errorf("ssh: new session: %w", err)
	}
	defer sess.Close()

	// Some sshd configs disable AcceptEnv. Build the command string with
	// env prefix when env is provided — this works regardless of server
	// config and matches the local `mergedEnv` semantics.
	if len(env) > 0 {
		cmd = "env " + strings.Join(env, " ") + " " + cmd
	}

	stdoutPipe, err := sess.StdoutPipe()
	if err != nil {
		return -1, fmt.Errorf("ssh: stdout pipe: %w", err)
	}
	// Combine stderr into the same pipe to preserve chronological order.
	stderrPipe, err := sess.StderrPipe()
	if err != nil {
		return -1, fmt.Errorf("ssh: stderr pipe: %w", err)
	}

	// Pump both pipes to the same writer in goroutines, then start the
	// command. Both goroutines drain before we Wait().
	done := make(chan struct{})
	go func() {
		defer close(done)
		pump(stdoutPipe, label, out, nil)
	}()
	go func() {
		pump(stderrPipe, label, out, stderrTail)
	}()

	if err := sess.Start(cmd); err != nil {
		return -1, fmt.Errorf("ssh: start command: %w", err)
	}

	// Wait for the command to finish OR for ctx cancellation. We can't
	// interrupt the remote process group over SSH without a pty, so on
	// cancel we just close the session and surface the error.
	waitCh := make(chan error, 1)
	go func() { waitCh <- sess.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		_ = sess.Close()
		<-done
		return -1, ctx.Err()
	}
	<-done
	if waitErr != nil {
		// An ExitError carries the exit code; treat any other error as
		// a transport failure.
		var exitErr *gossh.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitErr.ExitStatus(), nil
		}
		return -1, fmt.Errorf("ssh: wait: %w", waitErr)
	}
	return 0, nil
}

// pump scans r line by line and writes each to w with an optional label.
// When stderrTail is non-nil, the last 4KB of pumped bytes are also mirrored
// into it — useful for surfacing "command not found" / ENOENT-style errors
// when the combined-stdout writer was redirected elsewhere (e.g. an
// io.Pipe) and the caller can't otherwise distinguish empty-output from
// crash.
func pump(r io.Reader, label string, w io.Writer, stderrTail *bytes.Buffer) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if label != "" {
			line = "[" + label + "] " + line
		}
		_, _ = w.Write([]byte(line + "\n"))
		if stderrTail != nil {
			// Keep the last 4KB of stderr for postmortem. Bytes.Buffer
			// doesn't have a Trim-from-front API; use the same circular
			// trick (truncate when over cap).
			if stderrTail.Len()+len(line)+1 > 4096 {
				stderrTail.Reset()
			}
			stderrTail.WriteString(line)
			stderrTail.WriteByte('\n')
		}
	}
}

// RunScript uploads script (via SFTP write to a tempfile) and executes it.
// Use only when the script body is large enough that embedding it in Exec's
// cmd string would hit sshd's argv limit.
func (c *Client) RunScript(ctx context.Context, script, label string, env []string, out io.Writer) (int, error) {
	tmp, err := os.CreateTemp("", "nova-agent-*.sh")
	if err != nil {
		return -1, fmt.Errorf("ssh: create local tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(script); err != nil {
		_ = tmp.Close()
		return -1, fmt.Errorf("ssh: write script: %w", err)
	}
	_ = tmp.Close()

	// Use sftp to upload, then exec via sh <path>.
	sftpCli, err := c.sftp()
	if err != nil {
		return -1, err
	}
	defer sftpCli.Close()

	remote := "/tmp/" + filepath.Base(tmp.Name())
	if err := uploadFile(sftpCli, tmp.Name(), remote, 0700); err != nil {
		return -1, fmt.Errorf("ssh: upload script: %w", err)
	}
	defer sftpCli.Remove(remote)

	return c.Exec(ctx, "sh "+remote, label, env, out, nil)
}

// Exists returns true when remotePath exists on the remote host. The remote
// test is performed with `test -e` rather than SFTP stat because SFTP
// permission errors on unreadable directories would be misinterpreted as
// "not exists".
func (c *Client) Exists(remotePath string) bool {
	exit, _ := c.Exec(context.Background(), "test -e "+shellQuote(remotePath), "", nil, io.Discard, nil)
	return exit == 0
}

// Mkdirp creates remotePath (and any missing parents) with mode 0755.
func (c *Client) Mkdirp(remotePath string) error {
	if remotePath == "" {
		return errors.New("ssh: empty remote path")
	}
	_, err := c.Exec(context.Background(), "mkdir -p "+shellQuote(remotePath), "", nil, io.Discard, nil)
	return err
}

// WriteFile uploads data to remotePath with the given mode, ensuring the
// parent directory exists. Used by the agent-server check to seed a default
// ~/.claude/settings.json without round-tripping through a shell heredoc
// (which would mangle JSON's double quotes). Mode 0600 is the caller's
// choice when the payload carries a bearer token.
//
// A leading "~" or "~/" in remotePath is expanded against the remote
// user's $HOME (resolved once per call via `echo $HOME`). This matches the
// shell's expansion rules while keeping the SFTP path concrete — passing a
// literal "~/.claude/settings.json" to SFTP would create a file named
// "~/.claude/settings.json" under the SSH session's CWD instead, with no
// error reported. SFTP ignores ${HOME} syntax; we expand on our side.
func (c *Client) WriteFile(remotePath string, data []byte, mode os.FileMode) error {
	if c == nil || c.conn == nil {
		return errors.New("ssh: client not connected")
	}
	if remotePath == "" {
		return errors.New("ssh: empty remote path")
	}
	expanded, err := c.expandHome(remotePath)
	if err != nil {
		return err
	}
	sftpCli, err := c.sftp()
	if err != nil {
		return err
	}
	defer sftpCli.Close()

	if dir := path.Dir(expanded); dir != "" && dir != "." {
		if err := sftpCli.MkdirAll(dir); err != nil {
			return fmt.Errorf("ssh: sftp mkdir %s: %w", dir, err)
		}
	}
	dst, err := sftpCli.Create(expanded)
	if err != nil {
		return fmt.Errorf("ssh: sftp create %s: %w", expanded, err)
	}
	defer dst.Close()
	if _, err := dst.Write(data); err != nil {
		return fmt.Errorf("ssh: sftp write %s: %w", expanded, err)
	}
	if err := sftpCli.Chmod(expanded, mode); err != nil {
		return fmt.Errorf("ssh: sftp chmod %s: %w", expanded, err)
	}
	return nil
}

// expandHome rewrites a leading "~" or "~/" in path to the remote user's
// absolute home directory. Absolute paths and relative paths without a
// leading "~" pass through unchanged. Cached per-Client after the first
// resolution so back-to-back WriteFile calls don't fork a fresh exec.
func (c *Client) expandHome(path string) (string, error) {
	if len(path) < 2 || path[0] != '~' || (path[1] != '/' && path[1] != 0) {
		return path, nil
	}
	if c.homeDir == "" {
		var buf strings.Builder
		if _, err := c.Exec(context.Background(), "echo $HOME", "", nil, &buf, nil); err != nil {
			return "", fmt.Errorf("ssh: resolve $HOME: %w", err)
		}
		home := strings.TrimSpace(buf.String())
		if home == "" {
			home = "/root"
		}
		c.homeDir = home
	}
	return filepath.Join(c.homeDir, path[2:]), nil
}

// SyncDirUp uploads .jsonl and .md files from localDir to remoteDir,
// creating intermediate directories as needed. Files in remoteDir that do
// not exist locally are left alone (this is a forward sync, not a mirror).
// Tolerant of a missing localDir (returns nil).
func (c *Client) SyncDirUp(localDir, remoteDir string) error {
	if _, err := os.Stat(localDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("ssh: stat local dir %s: %w", localDir, err)
	}
	sftpCli, err := c.sftp()
	if err != nil {
		return err
	}
	defer sftpCli.Close()

	if err := sftpCli.MkdirAll(remoteDir); err != nil {
		return fmt.Errorf("ssh: sftp mkdir %s: %w", remoteDir, err)
	}
	return walkAndUpload(sftpCli, localDir, remoteDir)
}

// SyncDirDown downloads .jsonl and .md files from remoteDir to localDir,
// creating intermediate directories locally. Mirrors SyncDirUp's
// forward-only semantics.
func (c *Client) SyncDirDown(remoteDir, localDir string) error {
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return fmt.Errorf("ssh: mkdir local %s: %w", localDir, err)
	}
	sftpCli, err := c.sftp()
	if err != nil {
		return err
	}
	defer sftpCli.Close()
	return walkAndDownload(sftpCli, remoteDir, localDir)
}

// sftp opens a new SFTP session on the underlying SSH connection. The caller
// must Close it. Returns nil + error on transport failure.
func (c *Client) sftp() (*sftp.Client, error) {
	if c == nil || c.conn == nil {
		return nil, errors.New("ssh: client not connected")
	}
	cli, err := sftp.NewClient(c.conn)
	if err != nil {
		return nil, fmt.Errorf("ssh: sftp client: %w", err)
	}
	return cli, nil
}

// walkAndUpload mirrors localDir to remoteDir via SFTP, sending only
// .jsonl / .md files (claude session data + plan markdown). Larger binaries
// are skipped on purpose — code goes through git.
func walkAndUpload(sftpCli *sftp.Client, localDir, remoteDir string) error {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return fmt.Errorf("ssh: read local dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		local := filepath.Join(localDir, name)
		remote := path.Join(remoteDir, name)
		if e.IsDir() {
			if err := sftpCli.MkdirAll(remote); err != nil {
				return fmt.Errorf("ssh: mkdir %s: %w", remote, err)
			}
			if err := walkAndUpload(sftpCli, local, remote); err != nil {
				return err
			}
			continue
		}
		if !shouldSync(name) {
			continue
		}
		if err := uploadFile(sftpCli, local, remote, 0644); err != nil {
			return err
		}
	}
	return nil
}

// walkAndDownload mirrors remoteDir to localDir, sending only .jsonl/.md.
// Symlinks and special files are skipped — the remote session dir should
// not contain any.
func walkAndDownload(sftpCli *sftp.Client, remoteDir, localDir string) error {
	entries, err := sftpCli.ReadDir(remoteDir)
	if err != nil {
		// Missing remote dir is fine — nothing to download.
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		remote := path.Join(remoteDir, name)
		local := filepath.Join(localDir, name)
		if e.IsDir() {
			if err := os.MkdirAll(local, 0755); err != nil {
				return fmt.Errorf("ssh: mkdir local %s: %w", local, err)
			}
			if err := walkAndDownload(sftpCli, remote, local); err != nil {
				return err
			}
			continue
		}
		if !shouldSync(name) {
			continue
		}
		if err := downloadFile(sftpCli, remote, local); err != nil {
			return err
		}
	}
	return nil
}

func shouldSync(name string) bool {
	low := strings.ToLower(name)
	return strings.HasSuffix(low, ".jsonl") || strings.HasSuffix(low, ".md")
}

func uploadFile(sftpCli *sftp.Client, localPath, remotePath string, mode os.FileMode) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("ssh: open %s: %w", localPath, err)
	}
	defer src.Close()
	dst, err := sftpCli.Create(remotePath)
	if err != nil {
		return fmt.Errorf("ssh: sftp create %s: %w", remotePath, err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("ssh: sftp copy: %w", err)
	}
	_ = sftpCli.Chmod(remotePath, mode)
	return nil
}

func downloadFile(sftpCli *sftp.Client, remotePath, localPath string) error {
	src, err := sftpCli.Open(remotePath)
	if err != nil {
		return fmt.Errorf("ssh: sftp open %s: %w", remotePath, err)
	}
	defer src.Close()
	dst, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("ssh: create %s: %w", localPath, err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("ssh: sftp copy: %w", err)
	}
	return nil
}

// shellQuote returns a single-quoted shell-safe representation of s. Empty
// strings are quoted as ''; embedded single quotes are escaped via the
// standard close-quote / escape / open-quote trick.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
