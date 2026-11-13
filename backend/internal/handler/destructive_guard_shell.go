package handler

import (
	"strings"
)

// The shell forms that reach the same destructive effect as the plain
// spellings the guard already recognises.
//
// The guard's job is to stop the agent from deleting its own working tree.
// A form that reaches the same effect by a different spelling is the same
// hazard — and the guard is the LAST line of defence, so a hole here is a
// hole in the product, not in one spelling.
//
// Three families, all confirmed by probing the guard:
//
//   - command substitution: `$(rm -rf <workdir>)` — a nested command the
//     segment splitter keeps inside one segment, so the outer analysis never
//     saw the `rm`.
//   - an absolute path to the binary: `/bin/rm -rf <workdir>` — the operator
//     is not spelled `rm`, so the token match missed it.
//   - `rm -r` without `-f`: the operator deletes a directory tree just the
//     same, but the "destructive" test required a force flag.

// commandSubstitutions extracts the bodies of the command substitutions in a
// command string: `$(...)` and the legacy backtick form.
//
// Only substitutions that are actually EXECUTED are returned:
//
//   - Inside SINGLE quotes nothing is substituted, so `echo '$(rm -rf x)'`
//     never runs anything and must not be analysed.
//   - Inside DOUBLE quotes a substitution still runs, so
//     `echo "$(rm -rf x)"` does execute the rm and must be analysed.
func commandSubstitutions(cmd string) []string {
	var out []string
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch c {
		case '\'':
			// Single quotes: literal until the closing quote.
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				return out
			}
			i += j + 1
		case '`':
			j := strings.IndexByte(cmd[i+1:], '`')
			if j < 0 {
				return out
			}
			out = append(out, cmd[i+1:i+1+j])
			i += j + 1
		case '$':
			if i+1 < len(cmd) && cmd[i+1] == '(' {
				body, end, ok := extractParenGroup(cmd, i+1)
				if ok {
					out = append(out, body)
					i = end
					continue
				}
			}
		}
	}
	return out
}

// extractParenGroup extracts the body of a parenthesised group that starts
// at the `(` at index open, and returns the index just past its `)`.
//
// Nested parentheses are tracked so that `$(echo $(rm -rf x))` yields the
// outer body. A group that is never closed yields ok=false — an unterminated
// substitution is a syntax error the shell will reject anyway, and the
// outer command is judged on its own.
func extractParenGroup(cmd string, open int) (body string, end int, ok bool) {
	depth := 0
	for j := open; j < len(cmd); j++ {
		switch cmd[j] {
		case '\\':
			j++
		case '\'':
			k := strings.IndexByte(cmd[j+1:], '\'')
			if k < 0 {
				return "", 0, false
			}
			j += k + 1
		case '"':
			j = skipDoubleQuoted(cmd, j)
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return cmd[open+1 : j], j, true
			}
		}
	}
	return "", 0, false
}

// skipDoubleQuoted returns the index of the closing double quote of the
// span starting at i.
func skipDoubleQuoted(cmd string, i int) int {
	for j := i + 1; j < len(cmd); j++ {
		switch cmd[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return len(cmd)
}

// isDestructiveBinary reports whether a token names one of the operators the
// guard treats as tree-deleting, looking through an absolute path.
//
// `/bin/rm` is `rm`. The path is stripped and the BASE name matched, so
// `/usr/bin/rm`, `/bin/rm` and `rm` are one operator — and a directory that
// merely happens to be called "rm" (`./rm`) is not, because the token is
// checked for a path separator first.
func isDestructiveBinary(tok, name string) bool {
	if tok == name {
		return true
	}
	if !strings.Contains(tok, "/") {
		return false
	}
	slash := strings.LastIndexByte(tok, '/')
	return tok[slash+1:] == name
}

// findIsTreeDeleting reports whether a `find` invocation deletes what it
// finds, and whether the deletion is restricted to a SUBSET of the tree.
//
// `find . -delete` empties the current directory — the same effect as the
// `rm -rf .` the guard already vetoes. `find . -exec rm -rf {} +` does too.
//
// A find that selects by NAME or PATH (`-name`, `-iname`, `-path`, `-ipath`,
// `-regex`, `-iregex`) deletes only the matching entries, which is ordinary
// cleanup — `find . -name '*.o' -delete` is something the agent does
// routinely and must keep working. The other predicates (-type, -size,
// -mtime, …) select by attribute and can still match the whole tree, so
// they do NOT make the deletion safe.
//
// The second result is the search ROOT — the path find starts from, which is
// what the deletion is aimed at. An empty root means the current directory.
func findIsTreeDeleting(toks []string) (deleting bool, restricted bool, root string) {
	for i := 0; i < len(toks); i++ {
		switch toks[i] {
		case "-delete":
			deleting = true
		case "-exec", "-execdir":
			// The next token is the program to run for each match.
			if i+1 < len(toks) && isDestructiveBinary(toks[i+1], "rm") {
				deleting = true
			}
		case "-name", "-iname", "-path", "-ipath", "-regex", "-iregex":
			restricted = true
		}
	}
	if !deleting {
		return false, false, ""
	}
	// The root is the first argument that is neither an option nor the
	// argument of one.
	skipNext := false
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(tok, "-") {
			// Options that take a separate argument.
			switch tok {
			case "-name", "-iname", "-path", "-ipath", "-regex", "-iregex",
				"-type", "-size", "-mtime", "-mmin", "-atime", "-amin",
				"-ctime", "-cmin", "-newer", "-user", "-group", "-links",
				"-inum", "-maxdepth", "-mindepth", "-printf", "-fprintf",
				"-fprint", "-delete":
				skipNext = true
			}
			continue
		}
		return true, restricted, tok
	}
	return true, restricted, ""
}
