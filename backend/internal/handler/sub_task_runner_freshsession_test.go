package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestSubTaskRunnerFreshSessionClearsSourceSIDUnconditionally pins the
// RC for "选「新会话」建子任务时仍报 ❌ 源会话已失效（session 文件不存在）".
//
// Background (the regression this guards): SubTaskRunner.Run has an
//
//	if freshSession {
//	    if ctx := buildParentContext(...); ctx != "" {
//	        ...
//	        sourceSID = ""    // ← was nested here
//	    }
//	}
//
// block. The sourceSID clear was nested INSIDE the `if ctx != ""` guard.
// When the parent requirement has no injectable context (empty
// description, no design docs, no JSONL on disk, no prior sub-task rows,
// no usage snapshot) buildParentContext returns "" and the clear is
// skipped. runLocalSubTaskAttempt then forwards the stale sourceSID as
// StreamOpts.SessionID; llm/gateway.go's `else` branch emits
// `--session-id <stale-sid>`, and the CLI bails with "session 文件不存在"
// — exactly the wording the user reported.
//
// There is no runtime harness for this in unit tests (it would need
// SubTaskService + JobStore + Claude CLI + the full sub-task row), and
// the failure mode is invisible to the compiler because `sourceSID` is
// just a `string`. So we assert the wiring directly against the AST,
// matching TestArchitectRemoteCallSitePassesTrueSourceSID's pattern.
//
// The invariant: the assignment `sourceSID = ""` reachable from the
// `if freshSession { ... }` block in SubTaskRunner.Run must NOT be
// wrapped in any `if ctx != ""` / `if len(...) == 0` / `if buildParentContext(...) == ""`
// guard. The block must drop the stale SID on EVERY freshSession path,
// including the empty-buildParentContext one.
func TestSubTaskRunnerFreshSessionClearsSourceSIDUnconditionally(t *testing.T) {
	const srcFile = "sub_task_runner.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, srcFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcFile, err)
	}

	// Walk every `if freshSession { ... }` block in the file. There
	// should be exactly one in Run() — the prompt-building fresh-session
	// branch. If a refactor splits it or adds a second site, this test
	// will flag it so the guard can be updated (and the new site
	// audited).
	var freshBlocks []*ast.IfStmt
	ast.Inspect(file, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		// Match `if freshSession { ... }` — the bare identifier form.
		ident, ok := ifStmt.Cond.(*ast.Ident)
		if !ok || ident.Name != "freshSession" {
			return true
		}
		freshBlocks = append(freshBlocks, ifStmt)
		return true
	})

	if len(freshBlocks) == 0 {
		t.Fatalf("no `if freshSession { ... }` block found in %s — "+
			"if SubTaskRunner.Run's fresh-session branch was refactored, update this guard", srcFile)
	}
	if len(freshBlocks) > 1 {
		t.Fatalf("%d `if freshSession { ... }` blocks found in %s — "+
			"this guard only audits one; split / add a sibling guard", len(freshBlocks), srcFile)
	}

	// Build a parent map so we can walk ancestors of any node in O(1).
	parents := buildParentMap(file)

	body := freshBlocks[0].Body
	if body == nil {
		t.Fatalf("`if freshSession { ... }` has empty body in %s", srcFile)
	}

	clears := collectSourceSIDCLEars(body)
	if len(clears) == 0 {
		t.Fatalf("no `sourceSID = \"\"` assignment inside the `if freshSession { ... }` block in %s — "+
			"the stale-SID clear has been removed; the bug regresses", srcFile)
	}

	for _, assign := range clears {
		if isNestedUnderEmptyCtxGuard(assign, parents) {
			t.Fatalf("`sourceSID = \"\"` at %s is nested inside an empty-ctx guard (e.g. `if ctx != \"\"`) — "+
				"this is exactly the RC: when buildParentContext returns empty, the stale parent SID "+
				"is forwarded as StreamOpts.SessionID and the CLI emits `--session-id <stale-sid>`, "+
				"surfacing as `❌ 源会话已失效（session 文件不存在）`. Move the assignment OUT of "+
				"the guard so it runs unconditionally for freshSession=true.",
				fset.Position(assign.Pos()))
		}
	}

	// Belt-and-suspenders guard: the fork=true candidates loop must
	// also short-circuit to a single empty candidate when freshSession /
	// bare is set. Otherwise staleSourceCandidates would still walk up
	// to parentSourceSID / req.CodingSessionID, and the loop would
	// emit `--session-id <stale-sid>` (from req.CodingSessionID) — the
	// same symptom as above, just surfaced through a different code
	// path. We don't pin the exact placement of the guard (it lives in
	// the fork=true branch, not the freshSession block), we only
	// require the assignment `candidates = []string{""}` to appear
	// inside an `if freshSession || bare { ... }` somewhere in the
	// file.
	if !hasFreshOrBareCandidatesShortCircuit(file) {
		t.Fatalf("`candidates = []string{\"\"}` inside `if freshSession || bare { ... }` not found in %s — "+
			"the fork=true stale-fallback loop would otherwise walk up to req.CodingSessionID and emit "+
			"`--session-id <stale-sid>`, surfacing as `❌ 源会话已失效（session 文件不存在）` even after "+
			"the prompt-building clear above. Add the guard so fresh-session / bare paths opt out of the "+
			"parent-chain retry.", srcFile)
	}
}

// hasFreshOrBareCandidatesShortCircuit reports whether the file contains
// an `if freshSession || bare { ... candidates = []string{""} ... }`
// (anywhere — exact placement is irrelevant, only the existence and
// correct predicate matter).
func hasFreshOrBareCandidatesShortCircuit(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if !isFreshOrBareCond(ifStmt.Cond) {
			return true
		}
		ast.Inspect(ifStmt.Body, func(m ast.Node) bool {
			as, ok := m.(*ast.AssignStmt)
			if !ok {
				return true
			}
			if len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			lhs, ok := as.Lhs[0].(*ast.Ident)
			if !ok || lhs.Name != "candidates" {
				return true
			}
			composite, ok := as.Rhs[0].(*ast.CompositeLit)
			if !ok {
				return true
			}
			if len(composite.Elts) != 1 {
				return true
			}
			lit, ok := composite.Elts[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || lit.Value != `""` {
				return true
			}
			found = true
			return false
		})
		return !found
	})
	return found
}

// isFreshOrBareCond reports whether the expression looks like
// `freshSession || bare` (either operand order).
func isFreshOrBareCond(e ast.Expr) bool {
	be, ok := e.(*ast.BinaryExpr)
	if !ok || be.Op != token.LOR {
		return false
	}
	lhs, lok := be.X.(*ast.Ident)
	rhs, rok := be.Y.(*ast.Ident)
	if !lok || !rok {
		return false
	}
	return (lhs.Name == "freshSession" && rhs.Name == "bare") ||
		(lhs.Name == "bare" && rhs.Name == "freshSession")
}

// collectSourceSIDCLEars walks the AST rooted at root and returns every
// `*ast.AssignStmt` whose LHS is the bare identifier `sourceSID` and
// whose RHS is the string literal `""`.
func collectSourceSIDCLEars(root ast.Node) []*ast.AssignStmt {
	var out []*ast.AssignStmt
	ast.Inspect(root, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		if len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "sourceSID" {
			return true
		}
		// Match any "" literal on the RHS — renderExpr gives a stable
		// comparison and tolerates refactors that add a typed conversion.
		if renderExpr(as.Rhs[0]) == `""` {
			out = append(out, as)
		}
		return true
	})
	return out
}

// buildParentMap walks the AST and records each node's parent. The
// returned map lets us walk ancestors of an arbitrary node in O(depth)
// without re-traversing the tree.
func buildParentMap(root ast.Node) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		for _, child := range childrenOf(n) {
			parents[child] = n
			walk(child)
		}
	}
	walk(root)
	return parents
}

// childrenOf returns the immediate child nodes of n in declaration
// order. We only need to recognize the node types that appear in
// sub_task_runner.go's freshSession block, but the list is small and
// the function is purely structural.
func childrenOf(n ast.Node) []ast.Node {
	var out []ast.Node
	switch v := n.(type) {
	case *ast.File:
		for _, d := range v.Decls {
			out = append(out, d)
		}
	case *ast.FuncDecl:
		if v.Recv != nil {
			out = append(out, v.Recv)
		}
		out = append(out, v.Type)
		if v.Body != nil {
			out = append(out, v.Body)
		}
	case *ast.BlockStmt:
		for _, s := range v.List {
			out = append(out, s)
		}
	case *ast.IfStmt:
		if v.Init != nil {
			out = append(out, v.Init)
		}
		out = append(out, v.Cond)
		if v.Body != nil {
			out = append(out, v.Body)
		}
		if v.Else != nil {
			out = append(out, v.Else)
		}
	case *ast.AssignStmt:
		for _, e := range v.Lhs {
			out = append(out, e)
		}
		for _, e := range v.Rhs {
			out = append(out, e)
		}
	case *ast.ExprStmt:
		out = append(out, v.X)
	case *ast.CallExpr:
		out = append(out, v.Fun)
		for _, a := range v.Args {
			out = append(out, a)
		}
	case *ast.BinaryExpr:
		out = append(out, v.X, v.Y)
	case *ast.UnaryExpr:
		out = append(out, v.X)
	case *ast.SelectorExpr:
		out = append(out, v.X, v.Sel)
	}
	return out
}

// isNestedUnderEmptyCtxGuard reports whether the given assignment sits
// inside an IfStmt whose condition looks like `ctx != ""` / `ctx == ""` /
// `len(ctx) == 0` / `buildParentContext(...) != ""` etc. — the family of
// guards that protect an "empty parent context" code path.
func isNestedUnderEmptyCtxGuard(target ast.Node, parents map[ast.Node]ast.Node) bool {
	for cur := parents[target]; cur != nil; cur = parents[cur] {
		ifStmt, ok := cur.(*ast.IfStmt)
		if !ok {
			continue
		}
		rendered := renderExpr(ifStmt.Cond)
		switch rendered {
		case `ctx != ""`, `ctx == ""`,
			`len(ctx) == 0`, `len(ctx) != 0`:
			return true
		}
		// Catch the `if ctx := buildParentContext(...); ctx != ""` form —
		// its Cond is a *ast.BinaryExpr with Y="" and X="ctx".
		if be, ok := ifStmt.Cond.(*ast.BinaryExpr); ok && be.Op == token.NEQ {
			if renderExpr(be.Y) == `""` {
				// Two cases we care about:
				//   1. `ctx != ""` (Init declaration form)
				//   2. `buildParentContext(...) != ""` (any future shape)
				if renderExpr(be.X) == "ctx" {
					return true
				}
				if _, isCall := be.X.(*ast.CallExpr); isCall {
					return true
				}
			}
		}
	}
	return false
}
