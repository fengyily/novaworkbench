package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEffortProvider is a scripted EffortProvider.
type fakeEffortProvider struct {
	enabled                       bool
	design, coding, commit, other string
	ok                            bool
}

func (f fakeEffortProvider) EffortDefaults() (bool, string, string, string, string, bool) {
	return f.enabled, f.design, f.coding, f.commit, f.other, f.ok
}

// TestNormalizeEffort pins the tier whitelist.
//
// The CLI aborts the whole run on an unknown --effort value, so an
// unrecognised tier must be DROPPED rather than forwarded. That is what
// keeps a typo (or a future client sending a tier this build doesn't know)
// from taking down a whole coding job.
func TestNormalizeEffort(t *testing.T) {
	t.Run("valid tiers pass through", func(t *testing.T) {
		for _, want := range []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax} {
			if got := NormalizeEffort(want); got != want {
				t.Fatalf("NormalizeEffort(%q) = %q, want %q", want, got, want)
			}
		}
	})

	t.Run("whitespace is trimmed", func(t *testing.T) {
		if got := NormalizeEffort("  high  "); got != EffortHigh {
			t.Fatalf("NormalizeEffort(\"  high  \") = %q, want %q", got, EffortHigh)
		}
	})

	t.Run("invalid tiers become empty", func(t *testing.T) {
		// The CLI's whitelist is all-lowercase literals; "HIGH" is not one
		// of them, so it is treated as an unknown tier (dropped) rather
		// than quietly rewritten into a value the CLI never promised to
		// accept.
		for _, bad := range []string{"", "   ", "HIGH", "High", "ultra", "low-medium", "1", "none"} {
			if got := NormalizeEffort(bad); got != "" {
				t.Fatalf("NormalizeEffort(%q) = %q, want \"\"", bad, got)
			}
		}
	})
}

// TestValidEffort mirrors NormalizeEffort for the boolean form.
func TestValidEffort(t *testing.T) {
	if !ValidEffort("xhigh") {
		t.Fatal("ValidEffort(\"xhigh\") = false, want true")
	}
	if ValidEffort("ultra") {
		t.Fatal("ValidEffort(\"ultra\") = true, want false")
	}
}

// TestDefaultEffortForStage pins the shipped per-stage defaults.
//
// These three values ARE the product requirement (方案设计 high / 开发实现
// medium / 提交推送 low); StageOther is high because high is the CLI's own
// default level, which keeps every un-configured stage behaving exactly as
// it did before --effort existed.
func TestDefaultEffortForStage(t *testing.T) {
	cases := map[string]string{
		StageDesign: EffortHigh,
		StageCoding: EffortMedium,
		StageCommit: EffortLow,
		StageOther:  EffortHigh,
		"":          "",
		"unknown":   "",
	}
	for stage, want := range cases {
		if got := DefaultEffortForStage(stage); got != want {
			t.Fatalf("DefaultEffortForStage(%q) = %q, want %q", stage, got, want)
		}
	}
}

// TestResolveEffort pins the decision chain.
//
// The chain is deliberately two layers deep: the user's explicit pick, then
// the configured stage default, then the built-in default. A third
// "per-role default" layer would make the precedence three-segment and
// nobody would remember it.
func TestResolveEffort(t *testing.T) {
	t.Run("explicit pick beats the stage default", func(t *testing.T) {
		g := &Gateway{}
		g.SetEffortProvider(fakeEffortProvider{enabled: true, design: EffortHigh, ok: true})
		if got := g.ResolveEffort(EffortLow, StageDesign); got != EffortLow {
			t.Fatalf("ResolveEffort(low, design) = %q, want %q", got, EffortLow)
		}
	})

	t.Run("empty pick falls back to the configured stage default", func(t *testing.T) {
		g := &Gateway{}
		g.SetEffortProvider(fakeEffortProvider{
			enabled: true,
			design:  EffortMax,
			coding:  EffortLow,
			commit:  EffortMedium,
			other:   EffortXHigh,
			ok:      true,
		})
		cases := map[string]string{
			StageDesign: EffortMax,
			StageCoding: EffortLow,
			StageCommit: EffortMedium,
			StageOther:  EffortXHigh,
		}
		for stage, want := range cases {
			if got := g.ResolveEffort("", stage); got != want {
				t.Fatalf("ResolveEffort(\"\", %q) = %q, want %q", stage, got, want)
			}
		}
	})

	t.Run("provider failure falls back to the built-in default", func(t *testing.T) {
		g := &Gateway{}
		// ok=false means "the provider couldn't read the settings" — the
		// gateway must not treat the (empty) values it got as a
		// deliberate "no tier configured".
		g.SetEffortProvider(fakeEffortProvider{enabled: true, ok: false})
		if got := g.ResolveEffort("", StageCoding); got != DefaultEffortCoding {
			t.Fatalf("ResolveEffort(\"\", coding) = %q, want %q", got, DefaultEffortCoding)
		}
	})

	t.Run("no provider at all falls back to the built-in default", func(t *testing.T) {
		g := &Gateway{}
		if got := g.ResolveEffort("", StageCommit); got != DefaultEffortCommit {
			t.Fatalf("ResolveEffort(\"\", commit) = %q, want %q", got, DefaultEffortCommit)
		}
	})

	t.Run("the master switch wins over everything", func(t *testing.T) {
		g := &Gateway{}
		g.SetEffortProvider(fakeEffortProvider{enabled: false, design: EffortHigh, ok: true})
		// Even an explicit pick is suppressed: the switch is the single
		// escape hatch for a CLI / gateway that doesn't accept the flag.
		if got := g.ResolveEffort(EffortHigh, StageDesign); got != "" {
			t.Fatalf("ResolveEffort(high, design) = %q, want \"\" when disabled", got)
		}
		if got := g.ResolveEffort("", StageDesign); got != "" {
			t.Fatalf("ResolveEffort(\"\", design) = %q, want \"\" when disabled", got)
		}
	})

	t.Run("an invalid configured tier falls back to the built-in default", func(t *testing.T) {
		g := &Gateway{}
		g.SetEffortProvider(fakeEffortProvider{enabled: true, design: "ultra", ok: true})
		if got := g.ResolveEffort("", StageDesign); got != DefaultEffortDesign {
			t.Fatalf("ResolveEffort(\"\", design) = %q, want %q", got, DefaultEffortDesign)
		}
	})
}

// TestStreamArgsEffort pins how the tier reaches argv.
//
// Three properties matter:
//
//  1. A valid tier becomes a real `--effort <tier>` argv element.
//  2. It lands BEFORE --add-dir — that flag is variadic, so anything after
//     it would be swallowed as another directory value.
//  3. An invalid tier, or a CLI that doesn't know the flag, produces NO
//     flag at all (silently degrading beats aborting the whole run).
func TestStreamArgsEffort(t *testing.T) {
	// withProbe swaps the --help probe for the duration of one test and
	// points the gateway at a unique (fake) binary path so the per-path
	// support cache can't leak between cases.
	//
	// usable=false stands for "the probe could not read the binary at
	// all" — the missing binary, a --help that exceeded the timeout, a
	// build that died before printing anything.
	withProbe := func(t *testing.T, supported, usable bool) *Gateway {
		t.Helper()
		orig := effortHelpProbe
		effortHelpProbe = func(bin string) (bool, bool) {
			return supported, usable
		}
		t.Cleanup(func() { effortHelpProbe = orig })
		g := &Gateway{binPath: "/nonexistent/claude-" + t.Name()}
		return g
	}

	t.Run("valid tier is emitted", func(t *testing.T) {
		g := withProbe(t, true, true)
		args := g.BuildStreamArgs(StreamOpts{Prompt: "p", Effort: EffortHigh})
		i := indexOfArg(args, "--effort")
		if i == -1 {
			t.Fatalf("args = %v, want --effort", args)
		}
		if i+1 >= len(args) || args[i+1] != EffortHigh {
			t.Fatalf("args = %v, want --effort %q", args, EffortHigh)
		}
	})

	t.Run("lands before --add-dir", func(t *testing.T) {
		g := withProbe(t, true, true)
		args := g.BuildStreamArgs(StreamOpts{
			Prompt:  "p",
			Effort:  EffortMedium,
			AddDirs: []string{"/abs/one"},
		})
		ei := indexOfArg(args, "--effort")
		di := indexOfArg(args, "--add-dir")
		if ei == -1 || di == -1 {
			t.Fatalf("args = %v, want both --effort and --add-dir", args)
		}
		if ei > di {
			t.Fatalf("--effort at %d, --add-dir at %d: --effort must come first (args = %v)", ei, di, args)
		}
		// The tier must be a VALUE of --effort, not a stray token after the
		// variadic --add-dir (which would make it look like a directory).
		if args[di+1] != "/abs/one" {
			t.Fatalf("args = %v, want the granted directory right after --add-dir", args)
		}
	})

	t.Run("invalid tier is dropped", func(t *testing.T) {
		g := withProbe(t, true, true)
		args := g.BuildStreamArgs(StreamOpts{Prompt: "p", Effort: "ultra"})
		if indexOfArg(args, "--effort") != -1 {
			t.Fatalf("args = %v, want no --effort for an invalid tier", args)
		}
	})

	t.Run("dropped when the CLI doesn't know the flag", func(t *testing.T) {
		g := withProbe(t, false, true)
		args := g.BuildStreamArgs(StreamOpts{Prompt: "p", Effort: EffortHigh})
		if indexOfArg(args, "--effort") != -1 {
			t.Fatalf("args = %v, want no --effort when the CLI lacks the flag", args)
		}
	})

	t.Run("probe failure drops the flag instead of guessing", func(t *testing.T) {
		// A probe that could not read the binary says NOTHING about
		// whether the binary is old — it can be a slow machine, a wrapper
		// that only forwards -p, or a build that exits non-zero without
		// printing usage. Emitting --effort on that guess is exactly the
		// "unknown option '--effort'" run-killer the probe exists to
		// prevent, so the flag is dropped.
		g := withProbe(t, false, false)
		args := g.BuildStreamArgs(StreamOpts{Prompt: "p", Effort: EffortHigh})
		if indexOfArg(args, "--effort") != -1 {
			t.Fatalf("args = %v, want no --effort when the probe could not read the binary", args)
		}
	})

	t.Run("no tier means no flag", func(t *testing.T) {
		g := withProbe(t, true, true)
		args := g.BuildStreamArgs(StreamOpts{Prompt: "p"})
		if indexOfArg(args, "--effort") != -1 {
			t.Fatalf("args = %v, want no --effort", args)
		}
	})
}

// TestEffortSupportCacheIsPerBinary pins the cache's key.
//
// Two Gateways in one process may resolve different claude binaries (the
// CLAUDE_BIN env var differs per test / per agent host), so the probe
// result must not be shared across them.
func TestEffortSupportCacheIsPerBinary(t *testing.T) {
	orig := effortHelpProbe
	effortHelpProbe = func(bin string) (bool, bool) {
		return strings.Contains(bin, "modern"), true
	}
	t.Cleanup(func() { effortHelpProbe = orig })

	modern := &Gateway{binPath: "/nonexistent/claude-modern"}
	legacy := &Gateway{binPath: "/nonexistent/claude-legacy"}
	if !modern.cliSupportsEffort() {
		t.Fatal("modern binary should report support")
	}
	if legacy.cliSupportsEffort() {
		t.Fatal("legacy binary should report no support")
	}
}

// TestEffortHelpProbeReadsTheContent pins how the probe decides.
//
// The exit code is NOT evidence: a claude build may exit non-zero on
// --help while still printing its usage text, and that text is what tells
// us whether --effort exists. A probe that produced no output at all is
// a different case — we could not read the binary — and is reported as
// unusable so the caller can drop the flag instead of guessing.
//
// The fake binaries are real executables, so this exercises the actual
// exec path (exit status, stdout/stderr capture) rather than a stub.
func TestEffortHelpProbeReadsTheContent(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	dir := t.TempDir()

	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}

	t.Run("flag present, exit 0", func(t *testing.T) {
		bin := write("ok-exit0", "#!/bin/sh\necho '  --effort <level>  Effort level'\n")
		supported, usable := effortHelpProbe(bin)
		if !usable {
			t.Fatal("usable = false, want true (the probe read the binary)")
		}
		if !supported {
			t.Fatal("supported = false, want true")
		}
	})

	t.Run("flag present, non-zero exit", func(t *testing.T) {
		// The build prints its usage and exits 1. The usage text is the
		// evidence; the exit code is noise.
		bin := write("ok-exit1", "#!/bin/sh\necho '  --effort <level>  Effort level'\nexit 1\n")
		supported, usable := effortHelpProbe(bin)
		if !usable {
			t.Fatal("usable = false, want true (the probe read the binary)")
		}
		if !supported {
			t.Fatal("supported = false, want true (the usage text mentions --effort)")
		}
	})

	t.Run("flag absent", func(t *testing.T) {
		bin := write("no-flag", "#!/bin/sh\necho '  --model <model>  Model'\n")
		supported, usable := effortHelpProbe(bin)
		if !usable {
			t.Fatal("usable = false, want true (the probe read the binary)")
		}
		if supported {
			t.Fatal("supported = true, want false")
		}
	})

	t.Run("flag absent, non-zero exit", func(t *testing.T) {
		// The dangerous shape: a build that exits non-zero and prints
		// something that is NOT usage text. Content is authoritative →
		// no --effort.
		bin := write("no-flag-exit1", "#!/bin/sh\necho 'some other output'\nexit 3\n")
		supported, usable := effortHelpProbe(bin)
		if !usable {
			t.Fatal("usable = false, want true (the probe read the binary)")
		}
		if supported {
			t.Fatal("supported = true, want false")
		}
	})

	t.Run("no output at all is unusable", func(t *testing.T) {
		// Nothing to read: the binary could not be probed. The caller
		// must drop the flag rather than guess.
		bin := write("silent", "#!/bin/sh\nexit 1\n")
		supported, usable := effortHelpProbe(bin)
		if usable {
			t.Fatal("usable = true, want false")
		}
		if supported {
			t.Fatal("supported = true, want false")
		}
	})

	t.Run("missing binary is unusable", func(t *testing.T) {
		supported, usable := effortHelpProbe(filepath.Join(dir, "does-not-exist"))
		if usable {
			t.Fatal("usable = true, want false")
		}
		if supported {
			t.Fatal("supported = true, want false")
		}
	})
}
