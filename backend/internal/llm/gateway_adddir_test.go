package llm

import (
	"strings"
	"testing"
)

// indexOfArg returns the position of the first argv slot exactly equal to
// want, or -1.
func indexOfArg(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// TestStreamArgsAddDirs pins the --add-dir contract.
//
// Two properties matter and neither is obvious:
//
//  1. The flag is emitted with the granted directories as its values, so the
//     claude session can read them without a permission decision.
//  2. The flag is LAST in argv. `--add-dir` is a variadic option (the CLI
//     takes `--add-dir <dir>...`), so anything after it would be swallowed
//     as another directory value — a `--session-id` or `--settings` landing
//     there would silently become a "directory".
func TestStreamArgsAddDirs(t *testing.T) {
	g := &Gateway{}

	t.Run("absent when nothing granted", func(t *testing.T) {
		args := g.BuildStreamArgs(StreamOpts{Prompt: "p"})
		if indexOfArg(args, "--add-dir") != -1 {
			t.Fatalf("args = %v, want no --add-dir", args)
		}
	})

	t.Run("emitted with the granted paths", func(t *testing.T) {
		dirs := []string{"/abs/one", "/abs/two"}
		args := g.BuildStreamArgs(StreamOpts{Prompt: "p", AddDirs: dirs})
		i := indexOfArg(args, "--add-dir")
		if i == -1 {
			t.Fatalf("args = %v, want --add-dir", args)
		}
		if i+len(dirs) >= len(args) {
			t.Fatalf("args = %v, want %d directory values after --add-dir", args, len(dirs))
		}
		for k, want := range dirs {
			if args[i+1+k] != want {
				t.Fatalf("args[%d] = %q, want %q", i+1+k, args[i+1+k], want)
			}
		}
	})

	t.Run("last in argv", func(t *testing.T) {
		args := g.BuildStreamArgs(StreamOpts{
			Prompt:          "p",
			AddDirs:         []string{"/abs/one"},
			SessionID:       "sess-1",
			PermissionMode:  "plan",
			DisallowedTools: []string{"Write"},
		})
		i := indexOfArg(args, "--add-dir")
		if i == -1 {
			t.Fatalf("args = %v, want --add-dir", args)
		}
		if i != len(args)-2 {
			t.Fatalf("--add-dir at index %d of %d, want it second-to-last (args = %v)", i, len(args), args)
		}
		if args[len(args)-1] != "/abs/one" {
			t.Fatalf("last arg = %q, want the granted directory", args[len(args)-1])
		}
	})

	t.Run("plan mode keeps its own flag", func(t *testing.T) {
		args := g.BuildStreamArgs(StreamOpts{Prompt: "p", PermissionMode: "plan", AddDirs: []string{"/abs/one"}})
		if indexOfArg(args, "--permission-mode") == -1 {
			t.Fatalf("args = %v, want --permission-mode", args)
		}
		if indexOfArg(args, "--dangerously-skip-permissions") != -1 {
			t.Fatalf("args = %v, plan mode must not also skip permissions", args)
		}
	})

	t.Run("a path that looks like a flag is still a value", func(t *testing.T) {
		// The CLI's variadic option stops at the next "-"-prefixed token, so
		// a granted path must never be confused for a flag. Normalized
		// absolute paths always start with "/" — but the gateway doesn't
		// re-validate, so this test pins the ordering contract rather than
		// the path shape.
		args := g.BuildStreamArgs(StreamOpts{Prompt: "p", AddDirs: []string{"/abs/one"}})
		if !strings.HasSuffix(strings.Join(args, " "), "--add-dir /abs/one") {
			t.Fatalf("args = %v, want the directory to trail --add-dir", args)
		}
	})
}
