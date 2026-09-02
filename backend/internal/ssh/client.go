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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
	conn *gossh.Client
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

// Exec runs cmd on the remote host. stdout/stderr are merged into out, line
// by line, prefixed with [label] when label is non-empty — the same shape
// preflight.runLogged uses for the local install flow. env is a list of
// "KEY=VALUE" pairs and is preferred over session.Setenv (which sshd may
// disable via AcceptEnv). The returned int is the remote exit code; an
// error is returned for transport failures only.
//
// Cancel semantics: if ctx is cancelled, the remote session is closed
// (SIGKILL-equivalent — ssh doesn't surface SIGTERM cleanly to shells
// without a pty), and an error is returned.
func (c *Client) Exec(ctx context.Context, cmd, label string, env []string, out io.Writer) (int, error) {
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
		pump(stdoutPipe, label, out)
	}()
	go func() {
		pump(stderrPipe, label, out)
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
func pump(r io.Reader, label string, w io.Writer) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if label != "" {
			line = "[" + label + "] " + line
		}
		_, _ = w.Write([]byte(line + "\n"))
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

	return c.Exec(ctx, "sh "+remote, label, env, out)
}

// Exists returns true when remotePath exists on the remote host. The remote
// test is performed with `test -e` rather than SFTP stat because SFTP
// permission errors on unreadable directories would be misinterpreted as
// "not exists".
func (c *Client) Exists(remotePath string) bool {
	exit, _ := c.Exec(context.Background(), "test -e "+shellQuote(remotePath), "", nil, io.Discard)
	return exit == 0
}

// Mkdirp creates remotePath (and any missing parents) with mode 0755.
func (c *Client) Mkdirp(remotePath string) error {
	if remotePath == "" {
		return errors.New("ssh: empty remote path")
	}
	_, err := c.Exec(context.Background(), "mkdir -p "+shellQuote(remotePath), "", nil, io.Discard)
	return err
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
