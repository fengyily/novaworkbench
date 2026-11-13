package handler

import "testing"

// The mktemp carve-out: a `mktemp -d` scratch directory lives under the
// system temp dir and can never be the work dir, so an operand that points
// at one must not be vetoed.
//
// This is not hypothetical. job_0984023e (requirement req_0ee0bfd8de6f5da6,
// sub-task st_01590a11d1a73b79 "[P1-1] strip_comments 不可移植…") built a
// scratch CMake tree to reproduce a portability bug and was killed by the
// guard:
//
//	cd <workdir>
//	TMP=$(mktemp -d)
//	mkdir -p "$TMP/tree/src" …
//	cat > "$TMP/tree/CMakeLists.txt" <<'EOF' … EOF
//	…
//	rm -rf "$TMP"        ← vetoed: "禁止删除工作目录或其父目录。"
//
// The guard fails closed on ANY unresolvable operand, and "$TMP" is
// unresolvable — but the variable is provably a scratch dir, so the veto is
// a false positive that cost a real sub-task its entire run.
func TestDestructiveCommandReason_AllowsMktempScratchDir(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	allowed := []string{
		"TMP=$(mktemp -d)\nrm -rf \"$TMP\"",
		"TMP=$(mktemp -d)\nrm -rf \"$TMP/tree\"",
		"TMP=$(mktemp -d)\nrm -rf ${TMP}",
		"TMP=$(mktemp -d)\nrm -rf \"${TMP}/tree\"",
		"TMP=$(mktemp -d)\nmkdir -p \"$TMP/tree\"\ncd \"$TMP\" && rm -rf \"$TMP/tree\"",
		"TMP=$(mktemp -d -t nova)\nrm -rf \"$TMP\"",
		"TMP=$(mktemp --directory)\nrm -rf \"$TMP\"",
		"TMP=\"$(mktemp -d)\"\nrm -rf \"$TMP\"",
		"cd " + wd + "\nTMP=$(mktemp -d)\ncat > \"$TMP/CMakeLists.txt\" <<'EOF'\ncmake_minimum_required(VERSION 3.20.0)\nEOF\nrm -rf \"$TMP\"",
	}
	for _, cmd := range allowed {
		t.Run(cmd, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", cmd, wd); got != "" {
				t.Errorf("command %q must be allowed, got blocked with: %s", cmd, got)
			}
		})
	}
}

// The carve-out must not become a hole. Everything the guard used to veto
// still has to be vetoed.
//
// The two rules that keep it narrow:
//
//   - a variable qualifies ONLY when its sole assignment is the mktemp call
//     (a later `TMP="$WORKDIR"` wins in shell semantics, so the variable is
//     dropped);
//   - TMPDIR is honoured by mktemp -d, so a command that sets it is not
//     trusted.
func TestDestructiveCommandReason_StillBlocksAroundMktemp(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	blocked := []struct{ name, cmd string }{
		{"PWD is not a temp var", "rm -rf $PWD"},
		{"HOME is not a temp var", "rm -rf $HOME"},
		{"unnamed variable", "rm -rf \"$SCRATCH\""},
		{"variable reassigned to the workdir", "TMP=$(mktemp -d)\nTMP=\"" + wd + "\"\nrm -rf \"$TMP\""},
		{"TMPDIR may point anywhere", "TMPDIR=" + wd + "\nTMP=$(mktemp -d)\nrm -rf \"$TMP\""},
		{"dot-dot escapes the scratch dir", "TMP=$(mktemp -d)\nrm -rf \"$TMP/../..\""},
		{"workdir through a temp var", "TMP=$(mktemp -d)\nrm -rf \"$TMP/../../../../..\""},
		{"workdir is still the workdir", "TMP=$(mktemp -d)\nrm -rf " + wd},
		{"workdir parent is still vetoed", "TMP=$(mktemp -d)\nrm -rf " + "/Users/dev/.novaworkbench/worktrees/proj"},
		{"implicit cwd is still vetoed", "TMP=$(mktemp -d)\nrm -rf ."},
		{"git reset still vetoed", "TMP=$(mktemp -d)\ngit reset --hard"},
		{"git clean still vetoed", "TMP=$(mktemp -d)\ngit clean -fdx"},
		{"mv of the workdir still vetoed", "TMP=$(mktemp -d)\nmv " + wd + " /tmp/x"},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}
}

// The literal-prefix rule: an operand whose tail cannot be resolved is
// judged by the part in front of the expansion.
//
// The case that motivated it is the same shape as the mktemp one, but with a
// literal path: job_0222f9c6 (requirement req_0ee0bfd8de6f5da6, sub-task
// st_5f234dbf6304a1b8 "[P1-3] _Static_assert 的 MTU 下界…") built the same
// host test three times under a temp path, one variant per MTU length:
//
//	for m in 21 244 255; do rm -rf /tmp/asanok$m; cmake -S test/host -B /tmp/asanok$m …; done
//
// "/tmp/asanok$m" was vetoed as "禁止删除工作目录或其父目录。" — but its literal
// prefix is an absolute path outside the work dir, and the tail cannot reach
// the work dir by accident.
func TestDestructiveCommandReason_AllowsLiteralPrefixOutsideWorkDir(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	allowed := []string{
		"for m in 21 244 255; do rm -rf /tmp/asanok$m; cmake -S test/host -B /tmp/asanok$m; done",
		"rm -rf /tmp/asanok$m",
		"rm -rf /tmp/build-$BRANCH",
		"rm -rf /var/tmp/x$ID",
		"rm -rf /tmp/$SCRATCH",
	}
	for _, cmd := range allowed {
		t.Run(cmd, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", cmd, wd); got != "" {
				t.Errorf("command %q must be allowed, got blocked with: %s", cmd, got)
			}
		})
	}
}

// The prefix rule must not become a hole: a prefix that lands inside the work
// dir (or has no literal prefix at all) keeps failing closed.
func TestDestructiveCommandReason_StillBlocksAroundLiteralPrefix(t *testing.T) {
	const wd = "/Users/dev/.novaworkbench/worktrees/proj/req_abc"
	blocked := []struct{ name, cmd string }{
		{"no literal prefix", "rm -rf $FOO"},
		{"no literal prefix with slash", "rm -rf $FOO/bar"},
		{"prefix is the workdir", "rm -rf " + wd + "/$FOO"},
		{"prefix is an ancestor of the workdir", "rm -rf /Users/dev/.novaworkbench/worktrees/$PROJ"},
		{"prefix is inside the workdir", "rm -rf build/$FOO"},
		{"prefix is the workdir plus a dot", "rm -rf ./$FOO"},
		{"home shortcut has no prefix", "rm -rf ~/$FOO"},
		{"bare variable is still blocked", "rm -rf $PWD"},
		{"workdir literal is still blocked", "rm -rf " + wd},
		{"relative cwd is still blocked", "rm -rf ."},
		{"git reset is still blocked", "git reset --hard"},
	}
	for _, c := range blocked {
		t.Run(c.name, func(t *testing.T) {
			if got := destructiveCommandReason("Bash", c.cmd, wd); got == "" {
				t.Errorf("command %q must be blocked inside %s", c.cmd, wd)
			}
		})
	}
}

// mktempVars must only trust a variable that is assigned from mktemp and
// from nothing else.
func TestMktempVars(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{"plain", "TMP=$(mktemp -d)\nrm -rf \"$TMP\"", []string{"TMP"}},
		{"quoted", "TMP=\"$(mktemp -d)\"\nrm -rf \"$TMP\"", []string{"TMP"}},
		{"two vars", "A=$(mktemp -d)\nB=$(mktemp -d)\nrm -rf \"$A\" \"$B\"", []string{"A", "B"}},
		{"reassigned", "TMP=$(mktemp -d)\nTMP=/tmp/other\nrm -rf \"$TMP\"", nil},
		{"not mktemp", "TMP=/tmp/other\nrm -rf \"$TMP\"", nil},
		{"TMPDIR set", "TMPDIR=/x\nTMP=$(mktemp -d)\nrm -rf \"$TMP\"", nil},
		{"no -d", "TMP=$(mktemp)\nrm -rf \"$TMP\"", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mktempVars(c.cmd)
			if len(got) != len(c.want) {
				t.Fatalf("mktempVars(%q) = %v, want %v", c.cmd, got, c.want)
			}
			for _, name := range c.want {
				if !got[name] {
					t.Errorf("mktempVars(%q) missing %q, got %v", c.cmd, name, got)
				}
			}
		})
	}
}
