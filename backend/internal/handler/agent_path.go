package handler

// agent_path.go carries the PATH contract for shell scripts that we push to
// an Agent Server (and for the same scripts executed locally). It is
// deliberately separate from agent_paths.go, which owns the *worktree root*
// path policy (NOVA_AGENT_WORKTREE_ROOT / agent_servers.worktree_root) —
// that one decides WHERE work happens, this one decides WHICH BINARIES the
// remote shell can see.

// shellPathPrelude is the PATH prefix every shell script we push to an
// Agent Server must start with.
//
// Why: `ssh.Client.Exec` runs the command through `sess.Start(cmd)`, i.e.
// the remote `$SHELL -c "<cmd>"` — a NON-LOGIN, NON-INTERACTIVE shell.
// It never sources ~/.zshrc / ~/.bashrc, so it inherits sshd's default
// PATH (on macOS: /usr/bin:/bin:/usr/sbin:/sbin). A Homebrew-installed
// gpg at /opt/homebrew/bin/gpg is therefore invisible, and the script
// dies with `gpg: command not found` (exit 127).
//
// The dependency probe in agent_server.go (runCheck) already widens PATH
// with exactly these three directories before `command -v gpg`, which is
// why the environment check reports "✓ gpg gpg (GnuPG) 2.5.24" while
// the GPG provision script cannot find the very same binary. Emitting
// the SAME prefix here keeps the two in lockstep: the script always
// resolves the gpg the check validated, never a different one.
//
// Keep this list byte-identical to the probe's PATH prefix
// (agent_server.go, runCheck) — diverging would let the script pick a
// binary the check never validated.
const shellPathPrelude = "export PATH=\"$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:$PATH\"\n"

// withShellPath prefixes a shell command string with shellPathPrelude.
//
// The prelude ends with a newline, so the wrapped command lands on its own
// line: the `export` is guaranteed to run before the command it prefixes
// (a `;` separator would work too, but a newline keeps multi-line command
// strings readable in the job log).
func withShellPath(cmd string) string { return shellPathPrelude + cmd }
