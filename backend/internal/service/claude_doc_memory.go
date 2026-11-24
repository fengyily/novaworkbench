package service

import (
	"os"
	"path/filepath"
	"strings"
)

// ── Materializing the managed CLAUDE.md for the CLI's native channel ──────
//
// The managed project-level CLAUDE.md lives in the DB (project_claude_docs)
// and never in the user's repository. The Claude CLI, however, only loads
// project memory from FILES it discovers on disk — a directory passed via
// --add-dir counts as a "CLAUDE.md dir" when the
// CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD environment variable is set
// (measured; see internal/llm/claude_md_settings.go for the full probe
// results).
//
// So the managed copy has to be materialized as a real file somewhere the
// CLI will look. It is written under NovaWorkbench's own data directory —
// never inside the user's checkout — for two reasons:
//
//   - Writing it into the repository would dirty the working tree. The
//     automatic commit-push-PR flow runs `git add -A`, so the managed copy
//     would be committed into the user's branch.
//   - The CLI would then treat it as a tracked project file, and the
//     "excludes" mechanism (which is what keeps a single copy authoritative)
//     would have to exclude the very file we just wrote.
//
// The materialized directory is granted to the session with --add-dir, so
// the model can also read it as a plain file if it wants to.

// claudeDocMemoryRoot is the directory the materialized copies live under.
//
// It is a variable (not a constant) so tests can redirect it — a test that
// materialized into the real ~/.novaworkbench would leave files behind in
// the developer's home directory.
var claudeDocMemoryRoot = defaultClaudeDocMemoryRoot()

// defaultClaudeDocMemoryRoot returns ~/.novaworkbench/memory.
func defaultClaudeDocMemoryRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".novaworkbench", "memory")
}

// SetClaudeDocMemoryRoot overrides the root the managed copies are
// materialized under. Test helper — production never calls it.
func SetClaudeDocMemoryRoot(p string) { claudeDocMemoryRoot = p }

// ClaudeDocMemoryDir returns the directory that holds the materialized
// managed CLAUDE.md for a project.
//
// The path is deterministic per project so repeated runs reuse it: the
// materializer skips the write when the content is unchanged, and the
// directory stays out of the user's checkout.
func ClaudeDocMemoryDir(projectID string) string {
	root := claudeDocMemoryRoot
	if root == "" {
		root = defaultClaudeDocMemoryRoot()
	}
	if root == "" {
		// A missing home directory is fatal for materialization; the caller
		// degrades to the prompt-level injection (see the handler side).
		return ""
	}
	return filepath.Join(root, sanitizeMemoryPathSegment(projectID))
}

// sanitizeMemoryPathSegment turns a project id into a single safe path
// segment.
//
// Project ids are generated ids ("proj_xxxxxxxx"), so they are already
// filesystem-safe — but this function must not assume that: the value can
// also come from a restored database or a hand-written configuration. A path
// separator in the id would otherwise let the materialized file escape the
// memory root.
func sanitizeMemoryPathSegment(projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range projectID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	// "." and ".." would still be traversal-ish; neutralize them.
	if out == "." || out == ".." {
		return "unknown"
	}
	return out
}

// MaterializeClaudeDoc writes the managed document to the project's memory
// directory as CLAUDE.md and returns that directory.
//
// The write is idempotent: when the file already holds exactly this content
// nothing is written, so a run that starts many sessions does not keep
// touching the file (and the mtime stays put, which keeps the CLI's
// "instruction files were re-read" notice quiet).
//
// It returns an empty directory and an error when the write fails; the
// caller degrades to the prompt-level injection rather than failing the run
// — a missing project convention must never block a requirement.
func MaterializeClaudeDoc(projectID, content string) (string, error) {
	dir := ClaudeDocMemoryDir(projectID)
	if dir == "" {
		return "", os.ErrNotExist
	}
	body := strings.TrimSpace(content)
	if body == "" {
		return "", os.ErrInvalid
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "CLAUDE.md")
	if existing, err := os.ReadFile(path); err == nil && string(existing) == body {
		return dir, nil
	}
	// Write via a temp file + rename so a concurrent reader never sees a
	// half-written document.
	tmp, err := os.CreateTemp(dir, ".CLAUDE.md.*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return dir, nil
}

// RemoveClaudeDocMemory drops the materialized copy for a project.
//
// Used when the managed document is deleted: leaving a stale CLAUDE.md on
// disk would keep it in the model's context, which is exactly the drift the
// managed copy exists to prevent.
func RemoveClaudeDocMemory(projectID string) error {
	dir := ClaudeDocMemoryDir(projectID)
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
