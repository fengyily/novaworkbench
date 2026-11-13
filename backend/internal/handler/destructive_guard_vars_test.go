package handler

import (
	"os"
	"strings"
	"testing"
)

// The variable-resolution rules of the destructive-command guard.
//
// The guard used to fail closed on EVERY operand containing a variable
// expansion, because it could not know what the variable would hold. That is
// safe but unusable: the scratch-directory idiom is everywhere, and a
// sub-task that builds a fake tree under /tmp and then deletes it got its
// whole run killed.
//
//	req_0ee0bfd8de6f5da6 / st_1ade1c126af93917 ("[P1-2] …"):
//	TREE=/tmp/p1repro/tree
//	rm -rf "$TREE"        ← vetoed: "禁止删除工作目录或其父目录。"
//
// The fix resolves the variable and judges the RESOLVED path by the same
// rules as any other operand — so the relaxation is not a hole, and the next
// spelling of the idiom is not blocked either.

// TestDestructiveCommandReason_ResolvesLiteralAssignment covers the case
// that motivated the fix, and the shapes around it.
func TestDestructiveCommandReason_ResolvesLiteralAssignment(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	allowed := []struct {
		name string
		cmd  string
	}{
		{"plain literal", "TREE=/tmp/p1repro/tree\nrm -rf \"$TREE\""},
		{"unquoted reference", "TREE=/tmp/p1repro/tree\nrm -rf $TREE"},
		{"braced reference", "TREE=/tmp/p1repro/tree\nrm -rf \"${TREE}\""},
		{"subdirectory of the literal", "TREE=/tmp/p1repro/tree\nrm -rf \"$TREE/src\""},
		{"export form", "export TREE=/tmp/p1repro/tree\nrm -rf \"$TREE\""},
		{"other variable name", "DIR=/tmp/scratch\nrm -rf \"$DIR\""},
		{"other variable name, braced", "SCRATCH=/tmp/scratch\nrm -rf \"${SCRATCH}\""},
		{"literal with a space", "TREE=\"/tmp/a b\"\nrm -rf \"$TREE\""},
		{"relative literal (a descendant)", "TREE=build\nrm -rf \"$TREE\""},
		{"reassigned to a safe literal", "TREE=/tmp/x\nTREE=/tmp/y\nrm -rf \"$TREE\""},
		{"the real failing command", "set -u\ncd " + wd + "\ncp a.c /tmp/p1repro/host_stubs.c.bak\n" +
			"TREE=/tmp/p1repro/tree\nrm -rf \"$TREE\"; mkdir -p \"$TREE/src\"\n" +
			"cat > \"$TREE/src/leak.c\" <<'EOF'\nint x;\nEOF\n" +
			"./test/host/tools/check_device_isolation.sh --device-root \"$TREE\" 2>&1 | tail -5\n" +
			"cp /tmp/p1repro/host_stubs.c.bak test/host/stubs/host_stubs.c\n" +
			"git diff --stat -- test/host/stubs/host_stubs.c"},
	}
	for _, c := range allowed {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got != "" {
				t.Errorf("command %q must be allowed, got blocked with: %s", c.cmd, got)
			}
		})
	}
}

// TestDestructiveCommandReason_LiteralAssignmentStillVetoesTheWorkDir is the
// other half of the contract: the resolved path is JUDGED, not trusted. A
// variable that holds the work dir — or anything that can reach it — is
// still vetoed.
func TestDestructiveCommandReason_LiteralAssignmentStillVetoesTheWorkDir(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	blocked := []struct {
		name string
		cmd  string
	}{
		{"the work dir itself", "TREE=" + wd + "\nrm -rf \"$TREE\""},
		{"an ancestor of the work dir", "TREE=/Users/dev/.novaworkbench/worktrees/proj\nrm -rf \"$TREE\""},
		{"the parent, relatively", "TREE=..\nrm -rf \"$TREE\""},
		{"the work dir via a .. climb", "TREE=/tmp/x\nrm -rf \"$TREE/../..\""},
		{"reassigned to the work dir", "TREE=/tmp/x\nTREE=" + wd + "\nrm -rf \"$TREE\""},
		{"the work dir, mv", "TREE=" + wd + "\nmv \"$TREE\" /tmp/elsewhere"},
		{"the work dir, git worktree remove", "TREE=" + wd + "\ngit worktree remove \"$TREE\""},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}
}

// TestDestructiveCommandReason_LiteralAssignmentRespectsShellOrder pins the
// ordering semantics. These are the rules that make the resolution sound
// rather than a guess:
//
//   - a prefix assignment (`VAR=x cmd`) does NOT affect how `cmd` expands
//     its own arguments, so it cannot vouch for the operand;
//   - an assignment AFTER the use cannot have influenced it;
//   - the LAST assignment before the use wins, as in the shell.
func TestDestructiveCommandReason_LiteralAssignmentRespectsShellOrder(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	blocked := []struct {
		name string
		cmd  string
	}{
		{"prefix assignment is not in effect", "TREE=/tmp/x rm -rf \"$TREE\""},
		{"assignment after the use", "rm -rf \"$TREE\"\nTREE=/tmp/x"},
		{"prefix assignment of the work dir", "TREE=" + wd + " rm -rf \"$TREE\""},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}
}

// TestDestructiveCommandReason_UnresolvableAssignmentStaysVetoed keeps the
// fail-closed rule for values the shell would expand. The guard cannot know
// what they become, so it assumes the worst.
func TestDestructiveCommandReason_UnresolvableAssignmentStaysVetoed(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	blocked := []struct {
		name string
		cmd  string
	}{
		{"another variable", "TREE=$HOME/x\nrm -rf \"$TREE\""},
		{"a command substitution", "TREE=$(pwd)\nrm -rf \"$TREE\""},
		{"a glob", "TREE=/tmp/*\nrm -rf \"$TREE\""},
		{"a tilde", "TREE=~/x\nrm -rf \"$TREE\""},
		{"no assignment at all", "rm -rf \"$TREE\""},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}
}

// TestDestructiveCommandReason_MktempCarveOutRespectsOrder keeps the
// existing mktemp carve-out, now order-aware: the carve-out applies only
// while the variable still holds the scratch directory.
func TestDestructiveCommandReason_MktempCarveOutRespectsOrder(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	allowed := []string{
		"TMP=$(mktemp -d)\nrm -rf \"$TMP\"",
		"TMP=$(mktemp -d)\nrm -rf \"$TMP/tree\"",
		"TMP=$(mktemp -d)\nmkdir -p \"$TMP/tree\"\ncd \"$TMP\" && rm -rf \"$TMP/tree\"",
	}
	for _, cmd := range allowed {
		t.Run(cmd, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", cmd, wd); got != "" {
				t.Errorf("command %q must be allowed, got blocked with: %s", cmd, got)
			}
		})
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"the assignment comes later", "rm -rf \"$TMP\"\nTMP=$(mktemp -d)"},
		{"TMPDIR may point anywhere", "TMPDIR=" + wd + "\nTMP=$(mktemp -d)\nrm -rf \"$TMP\""},
		{"reassigned to the work dir", "TMP=$(mktemp -d)\nTMP=\"" + wd + "\"\nrm -rf \"$TMP\""},
		{"a .. climb escapes the scratch dir", "TMP=$(mktemp -d)\nrm -rf \"$TMP/../..\""},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}
}

// TestDestructiveCommandReason_ClosesEquivalentSpellings covers the forms
// that reach the SAME deletion by a different spelling. The guard is the
// last line of defence, so a form that gets the same result through the back
// door is the same hazard.
//
// Each of these was confirmed to slip past the guard before this change.
func TestDestructiveCommandReason_ClosesEquivalentSpellings(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	blocked := []struct {
		name string
		cmd  string
	}{
		{"command substitution", "echo $(rm -rf " + wd + ")"},
		{"command substitution, quoted", "echo \"$(rm -rf " + wd + ")\""},
		{"command substitution, assigned", "VAR=$(rm -rf " + wd + ")"},
		{"backticks", "echo `rm -rf " + wd + "`"},
		{"an absolute path to rm", "/bin/rm -rf " + wd},
		{"an absolute path to rm, in /usr/bin", "/usr/bin/rm -rf " + wd},
		{"rm -r without -f", "rm -r " + wd},
		{"rm -r of the cwd", "rm -r ."},
		{"find -delete", "find . -delete"},
		{"find -delete of the work dir", "find " + wd + " -delete"},
		{"find -exec rm", "find . -exec rm -rf {} +"},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}

	// The close relatives that must keep working: the targeted cleanup, and
	// the spelling that is only a mention of the binary.
	allowed := []string{
		"find . -name '*.o' -delete",
		"find build -delete",
		"ls /bin/rm",
		"git commit -m \"/bin/rm\"",
		"rm -r build/",
		"rm -rf /tmp/nova-agent",
	}
	for _, cmd := range allowed {
		t.Run("allowed: "+cmd, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", cmd, wd); got != "" {
				t.Errorf("command %q must be allowed, got blocked with: %s", cmd, got)
			}
		})
	}
}

// TestDestructiveCommandReason_CdMovesTheBase covers the relative-operand
// rules. A `cd` changes what a later relative path refers to, so the guard
// follows it: `cd .. && rm -rf <name>` aims at the work dir even though the
// bare name looks harmless.
//
// The directories are REAL, because a `cd` to a path that does not exist
// FAILS in the shell — the cwd stays put, and the guard must treat it that
// way too.
func TestDestructiveCommandReason_CdMovesTheBase(t *testing.T) {
	// root
	//   proj/            <- the parent
	//     req_abc/      <- the work dir
	//       sub/        <- a subdir of the work dir
	root := t.TempDir()
	parent := root + "/proj"
	wd := parent + "/req_abc"
	sub := wd + "/sub"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	blocked := []struct {
		name string
		cmd  string
	}{
		{"cd to the parent, then rm the work dir by name", "cd " + parent + "\nrm -rf req_abc"},
		{"cd to the parent, then rm .", "cd " + parent + "\nrm -rf ."},
		{"cd to the parent, then rm ..", "cd " + parent + "\nrm -rf .."},
		{"cd to a subdir, then rm ..", "cd " + sub + "\nrm -rf .."},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}

	// A cd into a scratch directory does not make the work dir reachable,
	// so the same shape stays allowed.
	allowed := []struct {
		name string
		cmd  string
	}{
		{"cd into a scratch dir, then rm a name", "cd " + sub + "\nrm -rf scratch"},
		{"cd into a scratch dir, then rm a subdir", "cd " + sub + "\nrm -rf sub"},
		// The cd target does not exist, so the cd FAILS and the cwd stays
		// at the work dir — where "scratch" is a harmless descendant.
		{"cd to a missing dir, then rm a name", "cd " + root + "/missing\nrm -rf scratch"},
	}
	for _, c := range allowed {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got != "" {
				t.Errorf("command %q must be allowed, got blocked with: %s", c.cmd, got)
			}
		})
	}
}

// TestLiteralPathValue pins the literal test itself.
func TestLiteralPathValue(t *testing.T) {
	cases := []struct {
		rhs  string
		want string
		ok   bool
	}{
		{"/tmp/scratch", "/tmp/scratch", true},
		{`"/tmp/scratch"`, "/tmp/scratch", true},
		{`'/tmp/scratch'`, "/tmp/scratch", true},
		{"/tmp/a b", "/tmp/a b", false}, // unquoted, the shell would split it
		{`"/tmp/a b"`, "/tmp/a b", true},
		{"", "", false},
		{"$HOME/x", "", false},
		{"$(pwd)", "", false},
		{"~/x", "", false},
		{"/tmp/*", "", false},
		{"a;b", "", false},
	}
	for _, c := range cases {
		got, ok := literalPathValue(c.rhs)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("literalPathValue(%q) = (%q, %v), want (%q, %v)", c.rhs, got, ok, c.want, c.ok)
		}
	}
}

// TestVarRef pins the variable-reference parser, including the truncated
// brace that shellTokens produces (`${VAR` — the closing brace is trimmed
// off the token edge).
func TestVarRef(t *testing.T) {
	cases := []struct {
		content string
		name    string
		suffix  string
		ok      bool
	}{
		{"$VAR", "VAR", "", true},
		{"${VAR}", "VAR", "", true},
		{"${VAR", "VAR", "", true},
		{"$VAR/sub", "VAR", "/sub", true},
		{"${VAR}/sub", "VAR", "/sub", true},
		{"$VAR/sub/dir", "VAR", "/sub/dir", true},
		{"/abs/path", "", "", false},
		{"", "", "", false},
		{"$", "", "", false},
	}
	for _, c := range cases {
		name, suffix, ok := varRef(c.content)
		if ok != c.ok || (ok && (name != c.name || suffix != c.suffix)) {
			t.Errorf("varRef(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.content, name, suffix, ok, c.name, c.suffix, c.ok)
		}
	}
}

// TestSuffixIsInert pins which suffixes may be appended to a resolved value.
func TestSuffixIsInert(t *testing.T) {
	ok := []string{"", "/sub", "/sub/dir"}
	for _, s := range ok {
		if !suffixIsInert(s) {
			t.Errorf("suffix %q must be inert", s)
		}
	}
	bad := []string{"/..", "/../..", "/sub/..", "$OTHER", "/a*b", "~/x"}
	for _, s := range bad {
		if suffixIsInert(s) {
			t.Errorf("suffix %q must not be inert", s)
		}
	}
}

// TestCommandSubstitutions pins which substitutions are analysed. A
// substitution inside single quotes is literal text and runs nothing;
// inside double quotes it does run.
func TestCommandSubstitutions(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"$(rm -rf x)", []string{"rm -rf x"}},
		{"echo $(rm -rf x)", []string{"rm -rf x"}},
		{"echo \"$(rm -rf x)\"", []string{"rm -rf x"}},
		{"`rm -rf x`", []string{"rm -rf x"}},
		{"echo '$(rm -rf x)'", nil},
		{"echo '`rm -rf x`'", nil},
		{"$(echo $(rm -rf x))", []string{"echo $(rm -rf x)"}},
	}
	for _, c := range cases {
		got := commandSubstitutions(c.cmd)
		if len(got) != len(c.want) {
			t.Errorf("commandSubstitutions(%q) = %v, want %v", c.cmd, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("commandSubstitutions(%q) = %v, want %v", c.cmd, got, c.want)
			}
		}
	}
}

// TestIsDestructiveBinary pins the absolute-path match: /bin/rm is rm, but a
// path that merely ends in "rm" as a directory name is not the binary.
func TestIsDestructiveBinary(t *testing.T) {
	for _, tok := range []string{"rm", "/bin/rm", "/usr/bin/rm"} {
		if !isDestructiveBinary(tok, "rm") {
			t.Errorf("%q must be recognised as rm", tok)
		}
	}
	for _, tok := range []string{"rmdir", "rm.txt", "./rm.txt", "confirm", "/bin/rm.exe"} {
		if isDestructiveBinary(tok, "rm") {
			t.Errorf("%q must NOT be recognised as rm", tok)
		}
	}
}

// TestFindIsTreeDeleting pins which find invocations delete a whole tree
// (dangerous) and which delete a named subset (ordinary cleanup).
func TestFindIsTreeDeleting(t *testing.T) {
	cases := []struct {
		cmd        string
		deleting   bool
		restricted bool
	}{
		{"find . -delete", true, false},
		{"find . -exec rm -rf {} +", true, false},
		{"find . -name '*.o' -delete", true, true},
		{"find . -iname '*.o' -delete", true, true},
		{"find . -path './build/*' -delete", true, true},
		{"find . -type f -delete", true, false},
		{"find . -name '*.o'", false, false},
		{"find . -print", false, false},
	}
	for _, c := range cases {
		deleting, restricted, _ := findIsTreeDeleting(strings.Fields(c.cmd)[1:])
		if deleting != c.deleting || restricted != c.restricted {
			t.Errorf("findIsTreeDeleting(%q) = (%v, %v), want (%v, %v)",
				c.cmd, deleting, restricted, c.deleting, c.restricted)
		}
	}
}
