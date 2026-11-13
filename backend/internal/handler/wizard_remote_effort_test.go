package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestRemoteRunParamsCarryEffort pins the reasoning-effort tier on EVERY
// remote parameter-struct literal.
//
// Why a source-scanning test: remoteRunInput / remoteCodingInput are the
// parameter structs that carry a run's configuration into the remote
// dispatchers (prepareRemoteAgentRun / runRemoteCoding), which turn them
// into the llm.StreamOpts the worker POST body is built from. A literal
// that forgets the `Effort` field therefore compiles fine and fails
// SILENTLY — the remote run falls back to the CLI's own default while the
// local path used the configured stage tier.
//
// That is exactly the divergence this guard exists to prevent. The design
// stage is the clearest case: the requirement names `high` as the design
// default, and a remote design run that silently dropped the tier would
// reason at a different depth than the same run on the local machine —
// with nothing in the logs to say so.
//
// The test scans EVERY non-test source file in the package for these
// literals and requires each one to set Effort. A NEW call site in a file
// that is not in the expectation table fails the test on purpose: whoever
// adds it has to decide which tier that path uses, rather than inheriting
// "no tier" by omission.
//
// The expected value per (file, struct) is also pinned, so a path cannot
// silently start using ANOTHER stage's tier (a design run picking up the
// coding tier would be just as wrong as picking up none).
func TestRemoteRunParamsCarryEffort(t *testing.T) {
	// (file, struct) → the rendered expression(s) the Effort field may use.
	//
	//   p.Effort  — a params struct that already resolved the tier
	//               (designRunParams / wikiRunParams / codingRunParams).
	//   effort    — a local variable the handler resolved before the run.
	//   resolveEffort(...) — resolved inline at the call site.
	expected := map[string]map[string][]string{
		// The five remoteRunInput sites: the ones that used to drop the tier.
		"wizard_architect.go": {"remoteRunInput": {"p.Effort"}},
		"wizard_analyst.go":   {"remoteRunInput": {"effort"}},
		"wizard_wiki.go":      {"remoteRunInput": {"p.Effort"}},
		"wizard_docs.go":      {"remoteRunInput": {"effort"}},
		// The coding path already carried the tier; keep it that way.
		"wizard_coding.go":        {"remoteCodingInput": {"p.Effort", "effort"}},
		"wizard_orchestration.go": {"remoteCodingInput": {"resolveEffort(...)"}},
		// The sub-task runner dispatches its remote child with the tier
		// the runner resolved for that child (st.Effort).
		"sub_task_runner.go": {"remoteCodingInput": {"st.Effort"}},
	}

	// The two structs that carry a remote run's configuration.
	guarded := map[string]bool{"remoteRunInput": true, "remoteCodingInput": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	seenFiles := 0
	totalLiterals := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(src), "remoteRunInput{") &&
			!strings.Contains(string(src), "remoteCodingInput{") {
			continue
		}
		seenFiles++

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		byStruct, known := expected[name]
		if !known {
			t.Errorf("%s constructs a remote run parameter literal but is not in the "+
				"expectation table of TestRemoteRunParamsCarryEffort — add it with the "+
				"tier that path is supposed to use", name)
			continue
		}

		ast.Inspect(file, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := cl.Type.(*ast.Ident)
			if !ok || !guarded[ident.Name] {
				return true
			}
			totalLiterals++

			pos := fset.Position(cl.Pos())
			var got string
			var present bool
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Effort" {
					continue
				}
				present = true
				got = renderEffortExpr(kv.Value)
			}

			allowed := byStruct[ident.Name]
			if len(allowed) == 0 {
				t.Errorf("%s: %s literal at %s is not in the expectation table — add the "+
					"tier that path is supposed to use", name, ident.Name, pos)
				return true
			}
			if !present {
				t.Errorf("%s: %s literal at %s has no Effort field — the remote run would "+
					"fall back to the CLI's own default while the local path used the "+
					"configured stage tier. Pass Effort: %s.", name, ident.Name, pos, allowed[0])
				return true
			}
			if got == `""` {
				t.Errorf("%s: %s{Effort: \"\"} at %s — the field is present but always "+
					"empty, which drops the tier just as silently as omitting it",
					name, ident.Name, pos)
				return true
			}
			for _, a := range allowed {
				if got == a {
					return true
				}
			}
			t.Errorf("%s: %s{Effort: %s} at %s — want one of %v. A run must not pick up "+
				"another stage's tier.", name, ident.Name, got, pos, allowed)
			return true
		})
	}

	if seenFiles == 0 {
		t.Fatal("no file constructs a remote run parameter literal — if the remote run " +
			"path was refactored, update this guard to match")
	}
	if totalLiterals == 0 {
		t.Fatal("no remote run parameter literal found — if the structs were renamed, " +
			"update this guard to match")
	}
}

// TestRemoteRunInputFeedsStreamOpts pins the last hop: the tier carried on
// remoteRunInput has to reach the llm.StreamOpts that the worker body is
// built from.
//
// This is the same "scan the source" style as the guard above, applied to
// the hop that turns the parameter struct into the CLI invocation. Without
// it, a literal could correctly set Effort and the tier would still be
// dropped one step later — the exact shape of the bug this file exists to
// catch.
func TestRemoteRunInputFeedsStreamOpts(t *testing.T) {
	const srcFile = "wizard_remote.go"
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read %s: %v", srcFile, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, srcFile, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcFile, err)
	}

	// Find the llm.StreamOpts literal inside prepareRemoteAgentRun and
	// check that it copies in.Effort.
	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "prepareRemoteAgentRun" {
			return true
		}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			cl, ok := m.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "llm" || sel.Sel.Name != "StreamOpts" {
				return true
			}
			found = true
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Effort" {
					continue
				}
				if got := renderEffortExpr(kv.Value); got != "in.Effort" {
					t.Errorf("%s: llm.StreamOpts{Effort: %s} in prepareRemoteAgentRun — "+
						"want in.Effort (the tier carried on remoteRunInput)", srcFile, got)
				}
				return true
			}
			t.Errorf("%s: the llm.StreamOpts built in prepareRemoteAgentRun has no Effort "+
				"field — the tier carried on remoteRunInput would be dropped here", srcFile)
			return true
		})
		return true
	})

	if !found {
		t.Fatalf("no llm.StreamOpts literal found inside prepareRemoteAgentRun in %s — "+
			"if the function was refactored, update this guard to match", srcFile)
	}
}

// renderEffortExpr renders the small set of expressions used as the Effort
// field's value, so the expectations in the table above stay readable.
//
// It differs from the package's renderExpr in one way: a call expression
// keeps its callee name ("resolveEffort(...)") instead of collapsing to
// "<call>". That distinction matters here — a blanket "<call>" would let
// any call expression pass the check.
func renderEffortExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
		return "<selector>"
	case *ast.BasicLit:
		return v.Value
	case *ast.CallExpr:
		name := renderEffortExpr(v.Fun)
		return name + "(...)"
	case *ast.UnaryExpr:
		return v.Op.String() + renderEffortExpr(v.X)
	}
	return "<expr>"
}
