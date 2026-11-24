package llm

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeMdFamilyPaths pins the set of files the CLI loads as project
// memory from a directory.
//
// The list has to be exact: a file that is missing from it stays in the
// context, and a file that is in it but not loaded is harmless. The
// interesting member is AGENTS.md — the CLI treats it as the ALTERNATIVE
// for CLAUDE.md, so excluding CLAUDE.md without excluding AGENTS.md would
// swap one project document for another instead of removing it.
func TestClaudeMdFamilyPaths(t *testing.T) {
	dir := t.TempDir()
	paths := ClaudeMdFamilyPaths(dir)
	want := map[string]bool{
		filepath.Join(dir, "CLAUDE.md"):            true,
		filepath.Join(dir, "CLAUDE.local.md"):      true,
		filepath.Join(dir, "AGENTS.md"):            true,
		filepath.Join(dir, ".claude", "CLAUDE.md"): true,
		filepath.Join(dir, ".claude", "AGENTS.md"): true,
	}
	if len(paths) != len(want) {
		t.Fatalf("family paths = %v, want %d entries", paths, len(want))
	}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unexpected family path %q", p)
		}
	}
}

func TestClaudeMdFamilyPathsEmptyDir(t *testing.T) {
	if got := ClaudeMdFamilyPaths(""); got != nil {
		t.Errorf("empty dir -> %v, want nil", got)
	}
}

// TestClaudeMdExcludePatternsForDirs keeps the managed directory loadable.
//
// The managed copy lives in its own directory and is granted with --add-dir.
// Excluding it would leave the model with NO project document at all — the
// exact opposite of the intent.
func TestClaudeMdExcludePatternsForDirs(t *testing.T) {
	memDir := "/tmp/nova/memory/proj_1"
	repoDir := "/tmp/nova/repo"

	patterns := ClaudeMdExcludePatternsForDirs([]string{repoDir, memDir}, memDir)
	for _, p := range patterns {
		if strings.HasPrefix(p, memDir) {
			t.Errorf("the managed directory must not be excluded, got %q", p)
		}
	}
	found := false
	for _, p := range patterns {
		if p == filepath.Join(repoDir, "CLAUDE.md") {
			found = true
		}
	}
	if !found {
		t.Errorf("the repository's CLAUDE.md is not excluded: %v", patterns)
	}
}

// TestClaudeMdExcludePatternsForDirsIsSortedAndDeduped pins the stability
// of the rendered --settings JSON: the same inputs must produce the same
// string, or the logged command line churns between runs.
func TestClaudeMdExcludePatternsForDirsIsSortedAndDeduped(t *testing.T) {
	dir := "/tmp/nova/repo"
	first := ClaudeMdExcludePatternsForDirs([]string{dir, dir}, "")
	second := ClaudeMdExcludePatternsForDirs([]string{dir}, "")
	if len(first) != len(second) {
		t.Fatalf("duplicate directory changed the list length: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("duplicate directory changed the list: %v vs %v", first, second)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1] > first[i] {
			t.Fatalf("list is not sorted: %v", first)
		}
	}
}

// TestStreamArgsClaudeMdNativeChannel pins the argv of the native channel.
//
// Three things have to hold at once, and each of them is easy to break:
//   - the memory directory is granted via --add-dir;
//   - --add-dir stays LAST (it is variadic — a later flag would be
//     swallowed as another directory value);
//   - the "CLAUDE.md dirs" env var travels in the --settings env block,
//     because without it --add-dir only grants READ access and the CLI
//     never loads <dir>/CLAUDE.md.
func TestStreamArgsClaudeMdNativeChannel(t *testing.T) {
	g := &Gateway{}
	memDir := "/tmp/nova/memory/proj_1"
	opts := StreamOpts{
		Prompt:              "p",
		WorkDir:             "/tmp/nova/repo",
		ClaudeMdDir:         memDir,
		ClaudeMdExcludes:    []string{filepath.Join("/tmp/nova/repo", "CLAUDE.md")},
		ExcludeRepoClaudeMd: true,
	}
	args := g.BuildStreamArgs(opts)

	// --add-dir carries the memory directory, and it is the LAST flag.
	idx := indexOfArg(args, "--add-dir")
	if idx == -1 {
		t.Fatalf("args = %v, want --add-dir", args)
	}
	if args[len(args)-1] != memDir {
		t.Errorf("last arg = %q, want the memory directory %q", args[len(args)-1], memDir)
	}

	// The env var makes the granted directory a "CLAUDE.md dir".
	settings := settingsArgValue(t, args)
	if settings.Env[ClaudeMdAdditionalDirsEnv] != "1" {
		t.Errorf("settings env = %v, want %s=1", settings.Env, ClaudeMdAdditionalDirsEnv)
	}

	// The exclusion list is a TOP-LEVEL settings key, not an env var.
	if len(settings.Excludes) == 0 {
		t.Errorf("settings = %+v, want a non-empty claudeMdExcludes", settings)
	}
	joined := strings.Join(settings.Excludes, ",")
	if !strings.Contains(joined, "CLAUDE.md") {
		t.Errorf("excludes = %v, want the repository's CLAUDE.md", settings.Excludes)
	}
}

// TestStreamArgsNoClaudeMdChannelLeavesTheCLIAlone pins the no-op: a run
// without a managed document must not turn on the process-wide env var and
// must not exclude anything.
func TestStreamArgsNoClaudeMdChannelLeavesTheCLIAlone(t *testing.T) {
	g := &Gateway{}
	args := g.BuildStreamArgs(StreamOpts{Prompt: "p", WorkDir: "/tmp/nova/repo"})
	if indexOfArg(args, "--add-dir") != -1 {
		t.Errorf("args = %v, want no --add-dir", args)
	}
	// With nothing to pin there is no --settings at all, so the CLI keeps
	// its defaults — including not seeing the env var.
	if indexOfArg(args, "--settings") != -1 {
		t.Errorf("args = %v, want no --settings", args)
	}
}

// TestStreamArgsExcludesRepoClaudeMd pins the repository exclusion on its
// own (the prompt-mode path uses it: the document is in the prompt, the
// repository's copy is kept out of the CLI's load set).
func TestStreamArgsExcludesRepoClaudeMd(t *testing.T) {
	g := &Gateway{}
	args := g.BuildStreamArgs(StreamOpts{
		Prompt:              "p",
		WorkDir:             "/tmp/nova/repo",
		ExcludeRepoClaudeMd: true,
	})
	settings := settingsArgValue(t, args)
	if len(settings.Excludes) == 0 {
		t.Fatalf("settings = %+v, want the repository's instruction files excluded", settings)
	}
	joined := strings.Join(settings.Excludes, ",")
	for _, want := range []string{"CLAUDE.md", "AGENTS.md"} {
		if !strings.Contains(joined, want) {
			t.Errorf("excludes = %v, want %s among them", settings.Excludes, want)
		}
	}
}

// TestPersonaModeProvider pins the persona delivery switch.
//
// "append" keeps the CLI's default system prompt (the persona is added to
// it); "replace" substitutes the persona for it — the pre-change behaviour.
// An unknown value must fall back to "replace" so a malformed setting
// cannot silently change how the persona is delivered.
func TestPersonaModeProvider(t *testing.T) {
	g := &Gateway{}
	if got := g.personaMode(); got != PersonaModeReplace {
		t.Errorf("without a provider: %q, want %q", got, PersonaModeReplace)
	}
	g.SetPersonaModeProvider(staticPersonaMode("append"))
	if got := g.personaMode(); got != PersonaModeAppend {
		t.Errorf("with an append provider: %q, want %q", got, PersonaModeAppend)
	}
	g.SetPersonaModeProvider(staticPersonaMode("nonsense"))
	if got := g.personaMode(); got != PersonaModeReplace {
		t.Errorf("with a malformed provider: %q, want %q", got, PersonaModeReplace)
	}
}

// TestStreamArgsPersonaMode pins the argv of both persona modes.
//
// "append" emits --append-system-prompt and keeps the CLI's default system
// prompt; "replace" emits --system-prompt. The two must never both appear:
// the CLI would take the last one and the outcome would depend on argv
// order.
func TestStreamArgsPersonaMode(t *testing.T) {
	g := &Gateway{}
	g.SetPersonaModeProvider(staticPersonaMode("append"))
	args := g.BuildStreamArgs(StreamOpts{Prompt: "p", SystemPrompt: "PERSONA"})
	if indexOfArg(args, "--append-system-prompt") == -1 {
		t.Errorf("args = %v, want --append-system-prompt", args)
	}
	if indexOfArg(args, "--system-prompt") != -1 {
		t.Errorf("args = %v, want no --system-prompt", args)
	}

	g.SetPersonaModeProvider(staticPersonaMode("replace"))
	args = g.BuildStreamArgs(StreamOpts{Prompt: "p", SystemPrompt: "PERSONA"})
	if indexOfArg(args, "--system-prompt") == -1 {
		t.Errorf("args = %v, want --system-prompt", args)
	}
	if indexOfArg(args, "--append-system-prompt") != -1 {
		t.Errorf("args = %v, want no --append-system-prompt", args)
	}
}

// staticPersonaMode is a PersonaModeProvider returning a fixed mode.
type staticPersonaMode string

func (s staticPersonaMode) PersonaMode() string { return string(s) }

// parsedSettings is the shape of the --settings JSON the gateway renders.
type parsedSettings struct {
	Env      map[string]string
	Excludes []string
}

// settingsArgValue parses the --settings JSON out of an argv list.
func settingsArgValue(t *testing.T, args []string) parsedSettings {
	t.Helper()
	idx := indexOfArg(args, "--settings")
	if idx == -1 {
		t.Fatalf("args = %v, want --settings", args)
	}
	var raw struct {
		Env              map[string]string `json:"env"`
		ClaudeMdExcludes []string          `json:"claudeMdExcludes"`
	}
	if err := json.Unmarshal([]byte(args[idx+1]), &raw); err != nil {
		t.Fatalf("parse --settings %q: %v", args[idx+1], err)
	}
	return parsedSettings{Env: raw.Env, Excludes: raw.ClaudeMdExcludes}
}
