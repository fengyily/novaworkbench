package handler

import (
	"os"
	"path/filepath"
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
func destructiveInCommandString(cmd, workDir string) string {
	for _, seg := range splitShellSegments(cmd) {
		if reason := destructiveInTokens(shellTokens(seg), workDir); reason != "" {
			return reason
		}
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

// operandTargetsWorkDir is the shared verdict for every path-explicit
// operator: does this operand point at the work dir itself or at one of its
// ancestors?
//
// It fails closed — an operand that cannot be resolved statically counts as
// targeting the work dir.
func operandTargetsWorkDir(op, workDir string) bool {
	if op == "" {
		// No operand at all: the operator acts on the cwd, which is the work dir.
		return true
	}
	content := op
	if inner, ok := quotedLiteral(op); ok {
		content = inner
	}
	if hasUnresolvableMeta(content) {
		return true
	}
	resolved, globbed := resolveOperand(content, workDir)
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

// destructiveInTokens scans the tokens of one command segment.
func destructiveInTokens(toks []string, workDir string) string {
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
		switch toks[i] {
		case "git":
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
					if op == "" || operandTargetsWorkDir(op, workDir) {
						return msgWorktreeRm
					}
				}
			}
		case "rm":
			opts, operands := collectRmOptions(toks, i+1)
			if !opts.destructive() {
				continue
			}
			for _, op := range operands {
				if operandTargetsWorkDir(op, workDir) {
					return msgRmWorkDir
				}
			}
		case "mv":
			for _, op := range collectPathOperands(toks, i+1) {
				if operandTargetsWorkDir(op, workDir) {
					return msgMvWorkDir
				}
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

// destructive reports whether the rm invocation can delete a directory tree
// without asking for confirmation.
//
// `rm -r` alone stays allowed (it has always been allowed); `--recursive`
// counts as destructive on its own because the previous implementation
// vetoed `rm --recursive` outright and that coverage must not be lost.
func (o rmOptions) destructive() bool {
	return o.recursive && (o.force || o.longRecursive)
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
