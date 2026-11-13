package handler

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Destructive-command guard for sub-task runs.
//
// WHY THIS FILE EXISTS
//
// The 2026-10-24 incident: a sub-task lost its working directory mid-run and
// then tried to "repair" it with `rm -rf <cwd>` plus a git clone loop, which
// only made things worse. A guard vetoes Bash commands that would delete /
// reset / move the working directory.
//
// The first implementation decided "is this command dangerous?" by looking for
// the work-dir path *anywhere in the command string* (plus keyword matches for
// `git worktree remove`). That proxy is wrong for path-explicit commands: what
// actually gets deleted is the OPERAND, not the mere mention of the work dir.
// A harmless `cd <workDir> && git worktree remove /tmp/basecheck` — the agent
// cleaning up a scratch worktree it created itself — was vetoed, which killed
// the whole sub-task run (req_10fdcd929761f813).
//
// The guard therefore classifies the operator:
//
//   - cwd-implicit (`git reset --hard`, `git clean -fdx`, `git worktree
//     remove` without a path): the child's cwd IS the work dir
//     (`cmd.Dir = opts.WorkDir`), so these are always vetoed.
//   - path-explicit (`git worktree remove <path>`, `rm -rf <path>`,
//     `mv <src> <dst>`): vetoed only when an operand resolves to the work dir
//     itself or to one of its ancestors.
//
// Operands that cannot be resolved statically (variable expansion, command
// substitution, `~`, unbalanced quotes, redirections) fail closed: they are
// treated as targeting the work dir.

const (
	msgResetHard    = "git reset --hard 会丢弃工作目录中的全部未提交改动。"
	msgCleanFdx     = "git clean -fdx 会删除工作目录中所有未跟踪文件。"
	msgWorktreeRm   = "git worktree remove 会移除工作目录本身。"
	msgRmWorkDir    = "禁止删除工作目录或其父目录。"
	msgMvWorkDir    = "禁止移动/重命名工作目录。"
	msgShellWrapper = "禁止在 shell 子命令中删除/重置工作目录。"
	msgFindDelete   = "禁止用 find -delete 清空工作目录。"
)

// destructiveCommandReason inspects a Bash command string and reports why it
// must not run inside workDir ("" = allowed).
//
// Scope note: this is a targeted guard against the failure mode that actually
// happened — an agent deciding to delete its own working tree — not a general
// shell sandbox. Ordinary cleanup like `rm -rf build/` or `git checkout -- .`
// keeps working, and so does removing a worktree the agent created elsewhere.
func destructiveCommandReason(toolName, cmd, workDir string) string {
	if toolName != "Bash" || cmd == "" || workDir == "" {
		return ""
	}
	return destructiveInCommandString(cmd, workDir)
}

// destructiveInCommandString splits a command into segments at shell control
// operators and checks every segment. A composite command may mix a safe and
// a dangerous step ("cd <wd> && rm -rf <wd>"), so segments are judged
// individually.
//
// The segments are walked in ORDER, because shell semantics are sequential:
// a variable assigned in an earlier statement holds that value in a later
// one, and a `cd` earlier in the command changes what a relative path later
// in it refers to. The state accumulated along the way is what lets the
// guard resolve `TREE=/tmp/scratch` + `rm -rf "$TREE"` without guessing.
func destructiveInCommandString(cmd, workDir string) string {
	// The temp-var set is computed once for the whole command: an assignment
	// (`TMP=$(mktemp -d)`) usually sits in an EARLIER segment than the `rm`
	// that consumes it, so per-segment analysis would never see it.
	tempVars := mktempVars(cmd)
	// A command substitution runs a nested command, exactly like `sh -c`:
	// `$(rm -rf <workdir>)` deletes the work dir just as surely as the
	// spelled-out form does. The segment splitter keeps the substitution
	// inside one segment, so the guard only ever saw the OUTER command and
	// the substitution was a blind spot — the same class of hole the
	// `sh -c` / `eval` branches already cover.
	//
	// The body is a whole command string, so it is analysed recursively
	// under the same fail-closed rules.
	for _, inner := range commandSubstitutions(cmd) {
		if reason := destructiveInCommandString(inner, workDir); reason != "" {
			return msgShellWrapper
		}
	}
	st := newGuardState(workDir, tempVars)
	for _, seg := range splitShellSegments(cmd) {
		// The state reflects the shell BEFORE this statement runs, which is
		// what decides how this statement's own arguments expand. Only
		// afterwards are this statement's assignments folded in.
		if reason := destructiveInTokensState(shellTokens(seg), workDir, tempVars, st); reason != "" {
			return reason
		}
		st.assignSegment(seg)
		st.noteCds(seg)
	}
	return ""
}

// splitShellSegments splits a shell command at unquoted control operators
// (&&, ||, ;, | and newlines). Quoted text is left intact so that
// `git commit -m "fix && stuff"` stays a single segment.
//
// Splitting is deliberately simple: when it cannot tell where a segment ends
// the consequence is merely that more text is judged as one segment, which
// can only make the verdict stricter, never laxer.
func splitShellSegments(cmd string) []string {
	var segs []string
	var cur strings.Builder
	inSingle, inDouble := false, false

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segs = append(segs, s)
		}
		cur.Reset()
	}

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(cmd):
			cur.WriteByte(c)
			i++
			cur.WriteByte(cmd[i])
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
			continue
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
			continue
		}
		if !inSingle && !inDouble {
			switch c {
			case '\n', ';':
				flush()
				continue
			case '|':
				if i+1 < len(cmd) && cmd[i+1] == '|' {
					i++
				}
				flush()
				continue
			case '&':
				if i+1 < len(cmd) && cmd[i+1] == '&' {
					i++
				}
				flush()
				continue
			}
		}
		cur.WriteByte(c)
	}
	flush()
	return segs
}

// shellTokens splits one segment into whitespace-separated tokens. Quotes are
// kept on the token so that the operand resolver can tell a quoted literal
// from an unquoted path.
//
// Redirection / grouping punctuation is trimmed off token edges: a segment
// like "(git reset --hard)" must still be recognized as `git reset --hard`.
func shellTokens(seg string) []string {
	var toks []string
	var cur strings.Builder
	inSingle, inDouble := false, false

	flush := func() {
		t := strings.Trim(cur.String(), "(){};|&<>")
		if t != "" {
			toks = append(toks, t)
		}
		cur.Reset()
	}

	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c == '\\' && !inSingle && i+1 < len(seg):
			cur.WriteByte(seg[i+1])
			i++
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
			continue
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
			continue
		}
		if !inSingle && !inDouble && (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
			flush()
			continue
		}
		cur.WriteByte(c)
	}
	flush()
	return toks
}

// quotedLiteral returns the content of a fully quoted token ("..." or '...').
// The second result is false when the token is not a balanced quoted literal.
func quotedLiteral(tok string) (string, bool) {
	if len(tok) >= 2 {
		q := tok[0]
		if (q == '"' || q == '\'') && tok[len(tok)-1] == q {
			return tok[1 : len(tok)-1], true
		}
	}
	return "", false
}

// hasUnresolvableMeta reports whether a path operand cannot be resolved
// statically: variable expansion ($), command substitution (`), home
// shortcut (~), history expansion (!), redirection / pipes, sub shells, or a
// quote that does not close. Such operands fail closed.
func hasUnresolvableMeta(tok string) bool {
	return strings.ContainsAny(tok, "$`~!<>&;(){}'\"")
}

// resolveOperand resolves a path operand to an absolute path. Relative paths
// are resolved against base — the child's cwd is the work dir, so that is the
// only correct base.
//
// The second result reports whether the operand contained a glob. For a
// globbed operand only the literal prefix before the first wildcard is kept.
func resolveOperand(op, base string) (string, bool) {
	globbed := strings.ContainsAny(op, "*?[")
	lit := op
	if globbed {
		lit = lit[:strings.IndexAny(lit, "*?[")]
	}
	var resolved string
	if filepath.IsAbs(lit) {
		resolved = filepath.Clean(lit)
	} else {
		resolved = filepath.Join(base, lit)
	}
	return resolved, globbed
}

// pathRelation describes how a resolved path relates to the work dir.
type pathRelation int

const (
	relUnrelated pathRelation = iota
	relSelf
	relDescendant
	relAncestor
)

// classifyPath reports how target relates to workDir.
//
// Deleting an ANCESTOR of the work dir destroys it just as thoroughly as
// deleting the work dir itself (`rm -rf ~/.novaworkbench/worktrees/proj`
// takes the requirement with it), so ancestors count as dangerous.
// Descendants are safe: `rm -rf build/` has always been allowed.
//
// Both sides go through symlink resolution so that macOS' /tmp → /private/tmp
// redirect cannot hide a match.
func classifyPath(target, workDir string) pathRelation {
	t := canonicalizePath(target)
	w := canonicalizePath(workDir)
	if t == "" || w == "" {
		return relUnrelated
	}
	if t == w {
		return relSelf
	}
	if pathIsUnder(t, w) {
		return relDescendant
	}
	if pathIsUnder(w, t) {
		return relAncestor
	}
	return relUnrelated
}

// pathIsUnder reports whether child is strictly inside parent.
func pathIsUnder(child, parent string) bool {
	if child == "" || parent == "" || child == parent {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonicalizePath returns the symlink-resolved absolute path of p.
//
// Unlike canonicalPath (worktree.go) it copes with paths that do not exist
// yet: the longest existing ancestor is resolved and the remaining segments
// are appended. That keeps `/tmp/x` and `/private/tmp/x` comparable on macOS
// even when the leaf has not been created yet.
func canonicalizePath(p string) string {
	if p == "" {
		return ""
	}
	abs := p
	if !filepath.IsAbs(abs) {
		if a, err := filepath.Abs(abs); err == nil {
			abs = a
		}
	}
	abs = filepath.Clean(abs)

	rest := ""
	cur := abs
	for {
		if _, err := os.Stat(cur); err == nil {
			if resolved, err := filepath.EvalSymlinks(cur); err == nil {
				if rest == "" {
					return resolved
				}
				return filepath.Join(resolved, rest)
			}
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// operandTargetsWorkDirState is the shared verdict for every path-explicit
// operator: does this operand point at the work dir itself or at one of its
// ancestors?
//
// It fails closed — an operand that cannot be resolved statically counts as
// targeting the work dir.
//
// The ONE exception is an operand that provably points at a `mktemp -d`
// scratch directory (see mktempVars): the shell created that directory
// itself, under the system temp dir, so it cannot be the work dir. Without
// this carve-out the guard vetoes the extremely common
// `TMP=$(mktemp -d) … rm -rf "$TMP"` idiom, which cost a real sub-task its
// run (job_0984023e, req_0ee0bfd8de6f5da6).
//
// A SECOND exception resolves a plain literal assignment (see
// destructive_guard_vars.go): `TREE=/tmp/scratch` makes `rm -rf "$TREE"` a
// deletable path we can judge exactly like any other operand. The resolved
// path is JUDGED, not trusted — a variable bound to the work dir is still
// vetoed.
//
// st is nil only for callers that have no accumulated state; then the
// whole-command mktemp scan is used as a fallback.
func operandTargetsWorkDirState(op, workDir string, tempVars map[string]bool, st *guardState) bool {
	if op == "" {
		// No operand at all: the operator acts on the cwd, which is the work dir.
		return true
	}
	content := op
	if inner, ok := quotedLiteral(op); ok {
		content = inner
	}
	// The base that relative operands resolve against. It is the work dir
	// until a `cd` moves it; when a `cd` moved it somewhere we cannot pin
	// down, relative operands fail closed (base is empty).
	base := workDir
	if st != nil {
		base = st.resolveBase()
	}
	// A variable reference is resolved through the ORDERED state: the binding
	// in effect at this point in the command is the one the shell would use.
	// A variable assigned LATER in the command has no binding yet, so the
	// operand stays unresolvable and fails closed.
	//
	// This is why the mktemp carve-out cannot rely on a whole-command scan
	// alone: `rm -rf "$TMP"` BEFORE `TMP=$(mktemp -d)` deletes the OLD
	// value of $TMP, not a scratch dir. The scan vouches that TMP is a
	// mktemp variable SOMEWHERE in the command; the order decides whether it
	// is in effect yet.
	if name, suffix, ok := varRef(content); ok {
		if b, known := st.vars[name]; known {
			switch b.kind {
			case varTempDir:
				if suffixIsInert(suffix) {
					// A mktemp scratch dir (or a path inside one) —
					// never the work dir.
					return false
				}
			case varLiteral:
				if suffixIsInert(suffix) {
					resolved, globbed := resolveOperand(b.path+suffix, base)
					if !globbed {
						switch classifyPath(resolved, workDir) {
						case relSelf, relAncestor:
							return true
						}
						return false
					}
					return canonicalizePath(resolved) == canonicalizePath(workDir)
				}
			}
		}
	}
	if hasUnresolvableMeta(content) {
		// The operand has a tail that cannot be resolved statically. Judge it
		// by its LITERAL PREFIX — the same reasoning the glob branch uses
		// ("a glob with a literal prefix stays inside the work dir and is
		// harmless").
		//
		// "/tmp/asanok$m" (a build under a temp path, one variant per loop
		// iteration) has the literal prefix "/tmp/asanok": an absolute path
		// that is neither the work dir nor inside it. The tail cannot turn
		// that into the work dir by accident — that would take a deliberate
		// ".." climb plus a path to the work dir spelled out in the
		// variable.
		//
		// A prefix that resolves INSIDE the work dir (or to the work dir
		// itself / one of its ancestors) stays blocked: "$FOO" could be
		// "..", and a bare "$FOO" has no prefix at all, so it fails closed.
		if prefix := literalPrefix(content); prefix != "" && base != "" {
			resolved, globbed := resolveOperand(prefix, base)
			if !globbed {
				switch classifyPath(resolved, workDir) {
				case relSelf, relAncestor, relDescendant:
					return true
				}
				return false
			}
		}
		return true
	}
	if base == "" {
		// The cwd moved somewhere we cannot pin down, so a relative operand
		// could be anything. Fail closed.
		return true
	}
	resolved, globbed := resolveOperand(content, base)
	if globbed {
		// A bare glob ("*", "*.log") may match the work dir's own entries.
		// A glob with a literal prefix ("build/*") stays inside the work dir
		// and is harmless.
		return canonicalizePath(resolved) == canonicalizePath(workDir)
	}
	switch classifyPath(resolved, workDir) {
	case relSelf, relAncestor:
		return true
	}
	return false
}

// mktempVars returns the shell variables the command assigns from
// `mktemp -d` — a freshly created scratch directory under the system temp
// dir (or $TMPDIR).
//
// A variable qualifies ONLY when its SOLE assignment in the whole command is
// that mktemp call. If the same name is also assigned something else
// (`TMP=$(mktemp -d); TMP="$WORKDIR"`), the later assignment wins in shell
// semantics and the relaxation must not apply — so the variable is dropped.
//
// The set is also empty when the command sets TMPDIR: `mktemp -d` then
// creates the directory inside that directory, and the caller may have
// pointed TMPDIR at the work dir.
//
// Everything else keeps failing closed. `$PWD`, `$HOME` and any other
// variable stay blocked, because their value is not knowable statically.
func mktempVars(cmd string) map[string]bool {
	if strings.Contains(cmd, "TMPDIR=") {
		return nil
	}
	// Every NAME= assignment in the command, with the right-hand side.
	assigns := assignRe.FindAllStringSubmatch(cmd, -1)
	counts := map[string]int{}
	mktemp := map[string]bool{}
	for _, a := range assigns {
		name, rhs := a[1], a[2]
		counts[name]++
		if isMktempDirCall(rhs) {
			mktemp[name] = true
		}
	}
	out := map[string]bool{}
	for name := range mktemp {
		if counts[name] == 1 {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// assignRe matches a shell variable assignment at the start of a word: the
// name, then the right-hand side up to the next shell metacharacter.
//
// The command-substitution alternative comes first so that "$(mktemp -d)" is
// captured whole — the plain-word alternative would stop at the space.
var assignRe = regexp.MustCompile(`(?:^|[\s;&|(\n])([A-Za-z_][A-Za-z0-9_]*)=(\$\([^)]*\)|"[^"]*"|'[^']*'|[^\s;&|)\n]*)`)

// isMktempDirCall reports whether an assignment's right-hand side is a
// `mktemp -d` (or `mktemp --directory`) invocation.
func isMktempDirCall(rhs string) bool {
	rhs = strings.TrimSpace(rhs)
	if len(rhs) >= 2 {
		q := rhs[0]
		if (q == '"' || q == '\'') && rhs[len(rhs)-1] == q {
			rhs = rhs[1 : len(rhs)-1]
		}
	}
	if !strings.HasPrefix(rhs, "$(") || !strings.Contains(rhs, "mktemp") {
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(rhs, "$("), ")")
	for _, f := range strings.Fields(inner) {
		if f == "-d" || f == "--directory" {
			return true
		}
	}
	return false
}


// isIdentChar reports whether c may appear in a shell variable name.
func isIdentChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// literalPrefix returns the leading part of a path operand that can be
// resolved without evaluating any shell expansion — everything before the
// first character that could carry one.
//
// "/tmp/asanok$m"      → "/tmp/asanok"
// "build/$FOO/bar"     → "build/"
// "$FOO"               → ""   (nothing resolvable — the caller fails closed)
// "build/*"            → ""   (a glob is not a literal prefix)
func literalPrefix(op string) string {
	for i := 0; i < len(op); i++ {
		c := op[i]
		if c == '$' || c == '`' || c == '~' || c == '!' || c == '<' || c == '>' ||
			c == '&' || c == ';' || c == '(' || c == ')' || c == '{' || c == '}' ||
			c == '\'' || c == '"' || c == '*' || c == '?' || c == ' ' || c == '\t' {
			return op[:i]
		}
	}
	return op
}

// destructiveInTokensState scans the tokens of one command segment, with the
// accumulated shell state, so operands that reference a resolved variable
// are judged against the value they will actually hold.
func destructiveInTokensState(toks []string, workDir string, tempVars map[string]bool, st *guardState) string {
	// A shell invoked with -c runs a nested command; analyze it too, but only
	// there — scanning every quoted string would veto harmless commit
	// messages that merely mention "rm -rf .".
	for i := 0; i+1 < len(toks); i++ {
		switch toks[i] {
		case "sh", "bash", "zsh", "dash", "ksh":
			if toks[i+1] == "-c" && i+2 < len(toks) {
				if inner, ok := quotedLiteral(toks[i+2]); ok {
					if reason := destructiveInCommandString(inner, workDir); reason != "" {
						return msgShellWrapper
					}
				}
			}
		case "eval":
			if inner, ok := quotedLiteral(toks[i+1]); ok {
				if reason := destructiveInCommandString(inner, workDir); reason != "" {
					return msgShellWrapper
				}
			}
		}
	}

	for i := 0; i < len(toks); i++ {
		switch {
		case toks[i] == "git":
			sub := gitSubcommandIndex(toks, i)
			if sub >= len(toks) {
				continue
			}
			switch toks[sub] {
			case "reset":
				// cwd-implicit: always dangerous.
				for k := sub + 1; k < len(toks); k++ {
					if strings.HasPrefix(toks[k], "--hard") {
						return msgResetHard
					}
				}
			case "clean":
				hasF, hasX := gitCleanFlags(toks, sub+1)
				if hasF && hasX {
					return msgCleanFdx
				}
			case "worktree":
				if sub+1 < len(toks) && toks[sub+1] == "remove" {
					op := firstPathOperand(toks, sub+2)
					if op == "" || operandTargetsWorkDirState(op, workDir, tempVars, st) {
						return msgWorktreeRm
					}
				}
			}
		// The operator is matched by its BASE NAME, so `/bin/rm` is `rm`.
		// Without that, an absolute path to the binary walked straight past
		// the guard — the same deletion, one spelling away.
		case isDestructiveBinary(toks[i], "rm"):
			opts, operands := collectRmOptions(toks, i+1)
			if !opts.destructive() {
				continue
			}
			for _, op := range operands {
				if operandTargetsWorkDirState(op, workDir, tempVars, st) {
					return msgRmWorkDir
				}
			}
		case isDestructiveBinary(toks[i], "mv"):
			for _, op := range collectPathOperands(toks, i+1) {
				if operandTargetsWorkDirState(op, workDir, tempVars, st) {
					return msgMvWorkDir
				}
			}
		case isDestructiveBinary(toks[i], "find"):
			// `find . -delete` empties the current directory — the same
			// effect as the `rm -rf .` this guard already vetoes. A find
			// that selects by name or path deletes only the matches, which
			// is ordinary cleanup and stays allowed.
			deleting, restricted, root := findIsTreeDeleting(toks[i+1:])
			if !deleting || restricted {
				continue
			}
			if root == "" {
				// No root: find starts at the cwd, which is the work dir.
				return msgFindDelete
			}
			if operandTargetsWorkDirState(root, workDir, tempVars, st) {
				return msgFindDelete
			}
		}
	}
	return ""
}

// gitSubcommandIndex returns the index of the git sub-command, skipping git's
// own global options (`-C <dir>`, `-c <key=value>`, …). Returns len(toks)
// when the command has no sub-command.
func gitSubcommandIndex(toks []string, i int) int {
	j := i + 1
	for j < len(toks) {
		t := toks[j]
		if !strings.HasPrefix(t, "-") || t == "-" {
			return j
		}
		switch t {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--exec-path":
			j += 2
			continue
		}
		j++
	}
	return len(toks)
}

// gitCleanFlags reports whether a `git clean` invocation carries both -f and
// -x. `-fd` (without -x) only removes untracked files but keeps ignored ones,
// which is the ordinary cleanup the guard has always allowed.
func gitCleanFlags(toks []string, start int) (hasF, hasX bool) {
	for _, t := range toks[start:] {
		if t == "--" {
			break
		}
		if len(t) < 2 || !strings.HasPrefix(t, "-") {
			break
		}
		if strings.HasPrefix(t, "--") {
			if t == "--force" {
				hasF = true
			}
			continue
		}
		for i := 1; i < len(t); i++ {
			switch t[i] {
			case 'f':
				hasF = true
			case 'x':
				hasX = true
			}
		}
	}
	return
}

// rmOptions holds the parsed options of an `rm` invocation.
type rmOptions struct {
	recursive     bool
	force         bool
	longRecursive bool
}

// destructive reports whether the rm invocation can delete a directory tree.
//
// RECURSION is the criterion, not the force flag. `rm -r <dir>` deletes the
// directory tree just as surely as `rm -rf <dir>` does: in a non-interactive
// shell there is no prompt to answer, so the force flag changes only whether
// read-only files inside are skipped, not whether the tree goes away.
//
// The old test required a force flag, which left `rm -r <workdir>` — and
// `rm -r .` — unguarded while `rm --recursive <workdir>` was vetoed. That
// asymmetry was an artefact of the flag parsing, not a policy.
//
// Whether the OPERAND is the work dir is decided separately (by
// operandTargetsWorkDir), so ordinary cleanup keeps working: `rm -r build/`
// stays allowed because a descendant of the work dir is harmless.
func (o rmOptions) destructive() bool {
	return o.recursive
}

// collectRmOptions parses the options and operands of an `rm` invocation.
func collectRmOptions(toks []string, start int) (rmOptions, []string) {
	var o rmOptions
	var operands []string
	afterDoubleDash := false
	for i := start; i < len(toks); i++ {
		t := toks[i]
		if !afterDoubleDash {
			if t == "--" {
				afterDoubleDash = true
				continue
			}
			if isRedirectToken(t) {
				if redirectConsumesNext(t) {
					i++
				}
				continue
			}
			if len(t) > 1 && strings.HasPrefix(t, "-") {
				switch t {
				case "--recursive":
					o.recursive = true
					o.longRecursive = true
					continue
				case "--force":
					o.force = true
					continue
				}
				if strings.HasPrefix(t, "--") {
					continue
				}
				for k := 1; k < len(t); k++ {
					switch t[k] {
					case 'r', 'R':
						o.recursive = true
					case 'f':
						o.force = true
					}
				}
				continue
			}
		}
		operands = append(operands, t)
	}
	return o, operands
}

// collectPathOperands returns the non-option tokens of an invocation (used
// for `mv`, where both source and destination are operands).
func collectPathOperands(toks []string, start int) []string {
	var operands []string
	afterDoubleDash := false
	for i := start; i < len(toks); i++ {
		t := toks[i]
		if !afterDoubleDash {
			if t == "--" {
				afterDoubleDash = true
				continue
			}
			if isRedirectToken(t) {
				if redirectConsumesNext(t) {
					i++
				}
				continue
			}
			if len(t) > 1 && strings.HasPrefix(t, "-") {
				continue
			}
		}
		operands = append(operands, t)
	}
	return operands
}

// isRedirectToken reports whether a token is a shell redirection rather than
// a path operand (`>`, `2>`, `2>&1`, `<`…). `rm -rf build/ 2>&1` must not be
// read as "delete build/ and 2>&1".
func isRedirectToken(tok string) bool {
	if _, quoted := quotedLiteral(tok); quoted {
		return false
	}
	return strings.ContainsAny(tok, "<>&")
}

// redirectConsumesNext reports whether a redirection token is followed by a
// separate target token (`> out.txt` yes, `2>&1` no).
func redirectConsumesNext(tok string) bool {
	if strings.Contains(tok, "&") {
		return false
	}
	return strings.HasSuffix(tok, ">") || strings.HasSuffix(tok, "<")
}

// firstPathOperand returns the first path operand of an invocation, skipping
// options such as `git worktree remove --force <path>`.
func firstPathOperand(toks []string, start int) string {
	operands := collectPathOperands(toks, start)
	if len(operands) == 0 {
		return ""
	}
	return operands[0]
}
