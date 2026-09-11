package util

import "strings"

// Claude CLI stores per-session JSONL under ~/.claude/projects/<slug>/ where
// <slug> is an encoding of the working directory path: replace every "/" and
// every "." with "-", keeping other characters as-is. The leading "/" is
// also replaced, so an absolute path begins with a single "-". Examples
// observed on disk:
//
//	/Users/f1                              -> -Users-f1
//	/Users/f1/.novaworkbench/.../req_xxx   -> -Users-f1--novaworkbench-...-req_xxx
//	                ^^^                                ^^^
//	      both slashes and the dot become dashes, producing the double dash
//	      between "f1" and "novaworkbench" that distinguishes Claude's
//	      encoding from a naive "/"-only replacement.
//
// EncodeClaudeSlug reproduces that encoding so we can compute the remote
// (Agent Server) cwd's slug deterministically and map local ↔ remote session
// directories by name.
//
// MatchSlugToPath compares in the encoded space — the decode is lossy
// (both "/" and "." collapse to "-" in the slug) so we re-encode the
// candidate project path and check whether the slug matches it directly or
// ends with it (so a project-root slug also covers any worktree descendent).

// EncodeClaudeSlug converts an absolute path into the Claude CLI slug form.
//
//	path = /Users/f1/x           -> -Users-f1-x
//	path = /a.b/c                -> -a-b-c          (dot becomes dash too)
//	path = /Users/f1/.nb/x/y     -> -Users-f1--nb-x-y
func EncodeClaudeSlug(path string) string {
	s := strings.ReplaceAll(path, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}

// DecodeClaudeSlug reverses EncodeClaudeSlug. The slug is treated as
// "/" replaced with "-" and "." replaced with "-". The original "." and
// "/" both collapse to "-" so the inverse cannot fully recover the input;
// it is only used for diagnostic logging.
func DecodeClaudeSlug(slug string) string {
	s := strings.TrimPrefix(slug, "-")
	return strings.ReplaceAll(s, "-", "/")
}

// MatchSlugToPath reports whether slug corresponds to projectPath. The
// comparison runs in the encoded space (both sides re-encoded) so the
// lossy "/" / "." collapse does not produce false negatives.
//
// Returns true when the encoded projectPath equals the slug, or when the
// slug ends with the encoded projectPath (so a project-root slug matches
// any of its worktree descendent paths).
func MatchSlugToPath(slug, projectPath string) bool {
	if slug == "" || projectPath == "" {
		return false
	}
	encoded := EncodeClaudeSlug(projectPath)
	return slug == encoded || strings.HasSuffix(slug, encoded)
}