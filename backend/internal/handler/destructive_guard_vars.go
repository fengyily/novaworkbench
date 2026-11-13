package handler

import (
	"os"
	"path/filepath"
	"strings"
)

// Static resolution of the shell variables a command assigns.
//
// WHY THIS FILE EXISTS
//
// The guard used to fail closed on EVERY operand containing a variable
// expansion ("$TREE", "${DIR}/sub"), because it had no way to know what the
// variable would hold. That is safe but unusable: the scratch-directory idiom
// is everywhere, and a sub-task that builds a fake tree under /tmp and then
// deletes it gets its whole run killed:
//
//	TREE=/tmp/p1repro/tree
//	rm -rf "$TREE"          ← vetoed: "禁止删除工作目录或其父目录。"
//
// (req_0ee0bfd8de6f5da6 / st_1ade1c126af93917, "[P1-2] …" — the agent built
// a fake device tree under /tmp to reproduce a false-negative in the isolation
// check, and the guard killed the run.)
//
// The fix is NOT to whitelist variable names or shapes — the next spelling
// ("$DIR", "${SCRATCH}/sub", "export TREE=…") would be blocked again, and any
// whitelist can be dodged. The fix is to RESOLVE the variable and then judge
// the RESOLVED path by exactly the same rules as any other operand:
//
//	TREE=/tmp/p1repro/tree  →  rm -rf /tmp/p1repro/tree   → unrelated  → ALLOW
//	TREE=<the work dir>      →  rm -rf <the work dir>    → the work dir → BLOCK
//
// So the relaxation is not a hole: the resolved path is judged, not trusted.
//
// ORDERING MATTERS, and this is where the shell's own semantics decide the
// answer:
//
//   - `VAR=x` as a STANDALONE statement persists for the statements that
//     follow it — that is the case worth resolving.
//   - `VAR=x cmd` (a prefix assignment) does NOT affect how `cmd` expands its
//     own arguments, and does not persist afterwards. Verified:
//     `TREE=old; TREE=new echo "[$TREE]"` prints `[old]`. Such an assignment
//     is therefore ignored for resolution purposes.
//   - an assignment AFTER the use cannot have influenced it.
//
// The resolver walks the command in statement order and carries the bindings
// forward, so each use sees exactly the bindings that were in effect when the
// shell reached it.

// varKind classifies what we know about a variable assigned in the command.
type varKind int

const (
	// varUnknown: the value cannot be determined statically. Operands that
	// reference it keep failing closed.
	varUnknown varKind = iota
	// varLiteral: the value is a plain path literal.
	varLiteral
	// varTempDir: the value is a freshly created `mktemp -d` directory. It
	// lives under the system temp dir, so it can never be the work dir — but
	// a ".." suffix can climb out of it, which disqualifies the relaxation.
	varTempDir
)

// varBinding is what we know about one variable.
type varBinding struct {
	kind varKind
	// path is the literal value, for varLiteral.
	path string
}

// guardState carries the shell state the guard tracks while walking a
// command: the bindings of the variables assigned so far, and the effective
// working directory (which a `cd` may have changed).
type guardState struct {
	workDir string
	// tempVars is the set of variables the WHOLE command assigns from
	// `mktemp -d`, computed by mktempVars. It is the gate for trusting a
	// mktemp binding at all: that scan rejects the cases where the value
	// is not provably a scratch dir (TMPDIR set, the variable assigned
	// something else as well).
	tempVars map[string]bool
	// base is the directory relative operands resolve against. It starts as
	// the work dir (the child's cwd) and follows any `cd`.
	base string
	// baseKnown is false once a `cd` moved us somewhere we cannot pin down
	// (an unresolvable target). Relative operands then fail closed.
	baseKnown bool
	// vars holds the bindings of the variables assigned so far.
	vars map[string]varBinding
}

func newGuardState(workDir string, tempVars map[string]bool) *guardState {
	return &guardState{
		workDir:   workDir,
		tempVars:  tempVars,
		base:      workDir,
		baseKnown: true,
		vars:      map[string]varBinding{},
	}
}

// assignSegment extracts the variable assignments a statement makes and
// folds them into the state.
//
// Only STANDALONE assignments count. A prefix assignment (`VAR=x cmd`) does
// not persist and does not affect `cmd`'s own argument expansion, so it is
// ignored here — the command that carries it is judged with the bindings
// from BEFORE it, exactly as the shell expands it.
//
// A statement that assigns a variable we cannot resolve (a command
// substitution, another variable, a glob) makes that variable UNKNOWN, even
// if an earlier statement had bound it to a literal: the later assignment
// wins in the shell, so the earlier binding must not be used.
func (st *guardState) assignSegment(seg string) {
	assigns := segmentAssignments(seg)
	if len(assigns) == 0 {
		return
	}
	// The statement must consist ONLY of assignments (plus an optional
	// `export` / `readonly` / `local` keyword) for them to be standalone.
	if !segmentIsAssignmentOnly(seg, assigns) {
		return
	}
	for _, a := range assigns {
		st.vars[a.name] = st.bindingFor(a.name, a.value)
	}
}

// bindingFor classifies an assignment's right-hand side.
//
// The mktemp case is gated on the whole-command scan (st.tempVars): that
// scan is what rejects a `mktemp -d` whose result is not provably a scratch
// directory (TMPDIR pointing elsewhere, or the variable also being assigned
// something else). A binding the scan does not vouch for is left UNKNOWN, so
// the operand keeps failing closed.
func (st *guardState) bindingFor(name, rhs string) varBinding {
	if isMktempDirCall(rhs) {
		if st.tempVars[name] {
			return varBinding{kind: varTempDir}
		}
		return varBinding{kind: varUnknown}
	}
	if p, ok := literalPathValue(rhs); ok {
		return varBinding{kind: varLiteral, path: p}
	}
	return varBinding{kind: varUnknown}
}

// segmentAssignment is one `NAME=value` found in a statement.
type segmentAssignment struct {
	name  string
	value string
}

// segmentAssignments returns the assignments a statement makes.
func segmentAssignments(seg string) []segmentAssignment {
	matches := assignRe.FindAllStringSubmatchIndex(seg, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]segmentAssignment, 0, len(matches))
	for _, m := range matches {
		out = append(out, segmentAssignment{
			name:  seg[m[2]:m[3]],
			value: seg[m[4]:m[5]],
		})
	}
	return out
}

// segmentIsAssignmentOnly reports whether a statement consists only of
// assignments (optionally prefixed by `export` / `readonly` / `local`), i.e.
// whether the assignments persist for the statements that follow.
//
// The check is textual: everything the assignments do not cover must be
// whitespace or one of those keywords. A statement that also runs a command
// (`VAR=x rm -rf "$VAR"`) is a prefix assignment, which persists for nothing.
func segmentIsAssignmentOnly(seg string, assigns []segmentAssignment) bool {
	// Cover the spans each assignment occupies (from the start of its NAME
	// to the end of its value) and check what is left over.
	covered := make([]bool, len(seg))
	for _, m := range assignRe.FindAllStringSubmatchIndex(seg, -1) {
		for i := m[2]; i < m[5]; i++ {
			if i < len(covered) {
				covered[i] = true
			}
		}
	}
	rest := strings.Builder{}
	for i := range seg {
		if !covered[i] {
			rest.WriteByte(seg[i])
		}
	}
	leftover := strings.TrimSpace(rest.String())
	for _, kw := range []string{"export", "readonly", "local", "declare", "typeset"} {
		leftover = strings.TrimSpace(strings.TrimPrefix(leftover, kw))
	}
	return leftover == ""
}

// literalPathValue returns the path a plain literal assignment holds.
//
// A literal is a value the shell will not touch: no variable expansion, no
// command substitution, no tilde, no glob, no control operator. Quoted
// values may contain spaces (the shell would not have split them); unquoted
// ones may not.
func literalPathValue(rhs string) (string, bool) {
	trimmed := strings.TrimSpace(rhs)
	if trimmed == "" {
		return "", false
	}
	quoted := false
	if len(trimmed) >= 2 {
		q := trimmed[0]
		if (q == '"' || q == '\'') && trimmed[len(trimmed)-1] == q {
			trimmed = trimmed[1 : len(trimmed)-1]
			quoted = true
		}
	}
	if trimmed == "" {
		return "", false
	}
	// Anything the shell would expand disqualifies the literal. A quoted
	// value still expands $ and `, so those are checked either way.
	if strings.ContainsAny(trimmed, "$`~*?[]{};&|<>()\n") {
		return "", false
	}
	if !quoted && strings.ContainsAny(trimmed, " \t") {
		return "", false
	}
	return trimmed, true
}

// varRef parses a path operand that references a shell variable: "$VAR",
// "${VAR}", "${VAR" (the closing brace is trimmed off by shellTokens), each
// optionally followed by a suffix such as "/sub/dir".
//
// The suffix is returned verbatim; the caller decides whether it is safe to
// append to a resolved value.
func varRef(content string) (name, suffix string, ok bool) {
	if !strings.HasPrefix(content, "$") {
		return "", "", false
	}
	rest := content[1:]
	if strings.HasPrefix(rest, "{") {
		rest = rest[1:]
	}
	n := 0
	for n < len(rest) && isIdentChar(rest[n]) {
		n++
	}
	if n == 0 {
		return "", "", false
	}
	name = rest[:n]
	rest = rest[n:]
	rest = strings.TrimPrefix(rest, "}")
	return name, rest, true
}

// suffixIsInert reports whether a suffix can be appended to a resolved value
// without changing where it points.
//
// A ".." segment can climb out of the resolved directory, and any expansion
// metacharacter means the suffix is not a literal path at all. Both make the
// operand unresolvable, so the guard fails closed.
func suffixIsInert(suffix string) bool {
	if suffix == "" {
		return true
	}
	if strings.ContainsAny(suffix, "$`~*?[]{};&|<>()\n") {
		return false
	}
	for _, seg := range strings.Split(suffix, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// noteCds tracks a `cd` so relative operands resolve against the right
// directory.
//
// The child's cwd is the work dir, and `cmd.Dir = opts.WorkDir` means the
// shell starts there. A `cd` changes it — and the base is what relative
// operands resolve against, so `cd .. && rm -rf <name>` really does aim at
// the work dir even though "build/" from the work dir is a harmless
// descendant.
//
// Three cases:
//
//   - The target is a literal that EXISTS: the cd succeeds, so the base
//     becomes it.
//   - The target is a literal that does NOT exist: the cd FAILS (the shell
//     stays put) — so the base is unchanged.
//   - The target cannot be resolved at all: the base becomes unknown and
//     relative operands fail closed.
//
// A `cd` inside a subshell does not affect the parent shell, so a statement
// that opens a subshell is ignored here.
func (st *guardState) noteCds(seg string) {
	if st.leadingParen(seg) {
		return // inside a subshell: the parent's cwd is unaffected
	}
	toks := shellTokens(seg)
	if len(toks) == 0 || toks[0] != "cd" {
		return
	}
	target := ""
	for _, t := range toks[1:] {
		if isRedirectToken(t) {
			continue
		}
		target = t
		break
	}
	if target == "" {
		// `cd` with no argument goes to $HOME — not statically known.
		st.baseKnown = false
		return
	}
	if target == "-" {
		st.baseKnown = false
		return
	}
	if inner, quoted := quotedLiteral(target); quoted {
		target = inner
	}
	if target == "~" || strings.HasPrefix(target, "~/") {
		st.baseKnown = false
		return
	}
	if strings.ContainsAny(target, "$`|&;<>") {
		st.baseKnown = false
		return
	}
	resolved := target
	if !strings.HasPrefix(resolved, "/") {
		resolved = st.base + "/" + resolved
	}
	resolved = filepathClean(resolved)
	// The cd only takes effect if the directory exists. When it does not,
	// the shell stays where it was.
	if _, err := os.Stat(resolved); err != nil {
		return
	}
	st.base = resolved
}

// leadingParen reports whether a statement opens a subshell.
func (st *guardState) leadingParen(seg string) bool {
	trimmed := strings.TrimLeft(seg, " \t")
	return strings.HasPrefix(trimmed, "(")
}

// filepathClean normalises a path.
func filepathClean(p string) string {
	return filepath.Clean(p)
}

// resolveBase returns the directory relative operands resolve against, or
// "" when it is not statically known (a `cd` we could not pin down). The
// caller fails closed in that case.
func (st *guardState) resolveBase() string {
	if st == nil || !st.baseKnown {
		return ""
	}
	return st.base
}
