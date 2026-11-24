package llm

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// ── CLAUDE.md delivery: settings keys and the measured CLI contract ───────
//
// This file is the SOURCE OF TRUTH for how the Claude CLI loads project
// memory, because the answer was measured — not inferred. The plan this
// change implements rests on two claims about the CLI that turned out to be
// wrong or incomplete, so the measurements are recorded here verbatim and
// the implementation follows the measurements.
//
// ── How the measurements were taken ───────────────────────────────────────
//
// `claude -p` was pointed at a local mock /v1/messages endpoint (via the
// --settings env block) that captures the request payload and answers with a
// minimal SSE stream. The captured payload IS the context the model would
// receive, so every claim below is observed, not inferred. The probe script
// lives at scripts/probe-claude-md.sh.
//
// The CLI under test: claude 2.1.328 (darwin/arm64), clean environment
// (no CLAUDE_CODE_* leftovers from the host shell).
//
// ── Measured results ──────────────────────────────────────────────────────
//
//	V1  --append-system-prompt         -> the repo's CLAUDE.md IS loaded
//	V2  --system-prompt (full replace)  -> the repo's CLAUDE.md IS **STILL**
//	                                      loaded. The default CLI system
//	                                      prompt is replaced; the project
//	                                      memory is NOT affected.
//	V3  --add-dir <dir>                 -> <dir>/CLAUDE.md is NOT loaded
//	V3b --add-dir <dir> PLUS the env var
//	    CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1
//	                                    -> <dir>/CLAUDE.md IS loaded
//	V4  --settings {"claudeMdExcludes":[<abs path>]}
//	                                    -> that file IS excluded
//	V5  V3b + V4 together               -> only the --add-dir copy remains
//
// ── What the measurements change ──────────────────────────────────────────
//
// 1. The managed copy does NOT need the persona to move from
//    --system-prompt to --append-system-prompt. V2 shows the CLI loads the
//    repo's CLAUDE.md either way: the project memory travels in the FIRST
//    USER MESSAGE as a <system-reminder> ("Codebase and user instructions
//    are shown below…"), NOT as a section of the system prompt. So the
//    "injection layer mismatch" this change set out to fix does not exist
//    in this CLI build — both the managed copy and the CLI's own copy arrive
//    in the user turn. What DOES differ is the framing: the CLI's copy is
//    introduced as authoritative codebase instructions ("These instructions
//    OVERRIDE any default behavior"), while NovaWorkbench's copy was plain
//    prompt text with a "以本块为准" sentence asking for precedence.
//
// 2. The native channel needs the env var as well as --add-dir (V3 vs V3b).
//    --add-dir alone grants READ access to a directory; it does not make the
//    CLI read that directory's CLAUDE.md. The env var is what turns the
//    granted directories into "CLAUDE.md dirs" — which is exactly what the
//    CLI's own --help calls them ("--add-dir (CLAUDE.md dirs)").
//
// 3. Exclusion works through the plain inline --settings JSON (V4/V5), so
//    no managed/policy settings file is needed on the target host.
//
// ── The remaining coupling ───────────────────────────────────────────────
//
// The env var is process-wide: it makes EVERY --add-dir directory a
// "CLAUDE.md dir". NovaWorkbench grants other directories through the same
// flag (authorized requirement dirs, attachment dirs), so the exclusion
// list has to cover those too — otherwise an unrelated CLAUDE.md would
// join the context and break the single-source guarantee. That is why the
// exclusion list is computed over the working directory AND every granted
// directory.

// ClaudeMdExcludesKey is the top-level --settings key the CLI reads to
// exclude specific CLAUDE.md files from loading.
//
// The CLI matches the values as picomatch patterns against ABSOLUTE file
// paths, and only applies the exclusion to the "User", "Project" and
// "Local" memory types (managed/policy memory cannot be excluded).
const ClaudeMdExcludesKey = "claudeMdExcludes"

// ClaudeMdAdditionalDirsEnv is the environment variable that makes the
// directories granted via --add-dir count as "CLAUDE.md dirs".
//
// Without it the CLI only grants READ access to those directories; with it
// the CLI additionally loads <dir>/CLAUDE.md (and <dir>/.claude/CLAUDE.md)
// from each of them. Measured — see the file header.
const ClaudeMdAdditionalDirsEnv = "CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD"

// ClaudeMdFamilyNames are the file names the CLI loads as project memory
// from a directory.
//
// The CLAUDE.md family is the project's own convention document. AGENTS.md
// is the CLI's FALLBACK for it: while CLAUDE.md is present AGENTS.md is
// ignored, but excluding CLAUDE.md alone silently promotes AGENTS.md into
// the context (measured). Both therefore have to go for the managed copy to
// be the single source.
//
// .claude/rules/ is deliberately absent: rules are a separate mechanism
// with a separate purpose, and the managed document does not stand in for
// them.
var ClaudeMdFamilyNames = []string{
	"CLAUDE.md",
	"CLAUDE.local.md",
	"AGENTS.md",
	filepath.Join(".claude", "CLAUDE.md"),
	filepath.Join(".claude", "AGENTS.md"),
}

// ClaudeMdFamilyPaths returns the CLAUDE.md-family files the CLI may load
// from dir.
//
// The paths are returned regardless of whether they exist: excluding a file
// that is not there is a no-op for the CLI, and listing it unconditionally
// is what makes the set correct on a host whose filesystem we cannot
// inspect (an Agent Server running the CLI against a worktree that only
// exists there).
//
// Both the literal path and its symlink-resolved form are returned. The CLI
// compares the patterns against the RESOLVED path of the file it loaded
// (measured: a symlinked working directory — macOS /var → /private/var —
// matches the resolved form), and the literal form is what a directory
// reached without a symlink resolves to. Emitting both makes the exclusion
// work regardless of how the path is spelled.
func ClaudeMdFamilyPaths(dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	out := make([]string, 0, len(ClaudeMdFamilyNames)*2)
	for _, name := range ClaudeMdFamilyNames {
		p := filepath.Join(dir, name)
		out = append(out, p)
		if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != p {
			out = append(out, resolved)
		}
	}
	return out
}

// ClaudeMdExcludePatternsForDirs returns the exclusion patterns covering
// every CLAUDE.md-family file under the given directories.
//
// keepDir names the one directory whose files must stay loadable — the
// directory holding the managed copy. It is skipped so the managed document
// itself is never excluded.
func ClaudeMdExcludePatternsForDirs(dirs []string, keepDir string) []string {
	keep := ""
	if keepDir != "" {
		keep = filepath.Clean(resolveDirForCompare(keepDir))
	}
	out := make([]string, 0, len(dirs)*len(ClaudeMdFamilyNames))
	for _, d := range dirs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		if keep != "" && filepath.Clean(resolveDirForCompare(d)) == keep {
			continue
		}
		out = append(out, ClaudeMdFamilyPaths(d)...)
	}
	return ClaudeMdExcludesPatterns(out)
}

// resolveDirForCompare resolves a directory through symlinks so two
// spellings of the same directory compare equal.
func resolveDirForCompare(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != "" {
		return resolved
	}
	return dir
}

// ClaudeMdExcludesPatterns normalizes the exclusion list for the CLI.
//
// The CLI treats every value as a glob pattern, so the paths are passed
// through unchanged (absolute paths are what the CLI documents and what the
// measurement used). The list is de-duplicated and sorted so the rendered
// --settings JSON is stable across runs — a stable argv/settings string
// keeps the logged command line diffable and the tests deterministic.
func ClaudeMdExcludesPatterns(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// claudeMdExcludesSettings renders the top-level --settings fragment for the
// exclusion list. An empty list renders nothing at all, so the caller can
// leave --settings out entirely when there is nothing to exclude.
func claudeMdExcludesSettings(paths []string) (json.RawMessage, bool) {
	patterns := ClaudeMdExcludesPatterns(paths)
	if len(patterns) == 0 {
		return nil, false
	}
	b, err := json.Marshal(patterns)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(b), true
}

// ClaudeMdEnvPairs returns the environment entries that turn the granted
// --add-dir directories into "CLAUDE.md dirs".
//
// This is the second half of the native channel: --add-dir grants READ
// access, and the env var is what makes the CLI also LOAD <dir>/CLAUDE.md
// from each granted directory (measured — see the file header). Without it
// the managed copy would be readable but never loaded.
//
// The pairs are returned rather than set globally because the variable is
// process-wide: it applies to every granted directory. The caller grants
// the memory directory precisely so the CLI loads the managed copy, and the
// exclusion list keeps every OTHER granted directory's own files out.
func ClaudeMdEnvPairs() []string {
	return []string{ClaudeMdAdditionalDirsEnv + "=1"}
}

// PersonaModeProvider supplies the persona delivery mode.
//
// The mode decides whether the role persona REPLACES the CLI's default system
// prompt (--system-prompt) or is APPENDED to it
// (--append-system-prompt). The pre-change code always replaced it, which
// also removed the CLI's own operating instructions.
//
// It is a provider (rather than a field on StreamOpts) so every stage gets
// the same answer without each call site having to know about it — the same
// shape as EffortProvider.
type PersonaModeProvider interface {
	// PersonaMode returns "append" or "replace". An empty or unknown value
	// means "replace" (the pre-change behaviour).
	PersonaMode() string
}

// Persona mode values.
const (
	// PersonaModeAppend keeps the CLI's default system prompt and appends
	// the persona to it.
	PersonaModeAppend = "append"
	// PersonaModeReplace replaces the CLI's default system prompt with the
	// persona (the pre-change behaviour).
	PersonaModeReplace = "replace"
)

// NormalizePersonaMode maps a stored value onto a known mode, defaulting to
// the pre-change behaviour so a missing or malformed setting cannot change
// how the persona is delivered.
func NormalizePersonaMode(raw string) string {
	switch strings.TrimSpace(raw) {
	case PersonaModeAppend:
		return PersonaModeAppend
	default:
		return PersonaModeReplace
	}
}

// personaMode returns the configured persona delivery mode.
//
// The default is "replace" — the pre-change behaviour — so a gateway
// without a provider wired (unit tests, standalone embedding) behaves
// exactly as before.
func (g *Gateway) personaMode() string {
	if g.personaCfg == nil {
		return PersonaModeReplace
	}
	return NormalizePersonaMode(g.personaCfg.PersonaMode())
}

// SetPersonaModeProvider wires the persona delivery mode into the gateway.
//
// A setter rather than a New() argument, for the same reason as
// SetEffortProvider: llm.New's signature is called by a dozen tests and
// changing it would ripple through all of them.
func (g *Gateway) SetPersonaModeProvider(p PersonaModeProvider) {
	g.personaCfg = p
}
