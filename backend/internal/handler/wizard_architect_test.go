package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/service"
)

// TestClearUnestablishedDesignSession pins the RC-2 guard semantics.
//
// Background (the regression this guards): prepareArchitectDesign pre-mints a
// design session id and persists it BEFORE spawning claude. If the run then
// fails without the CLI ever establishing that session, the stale id used to
// survive on the requirement row. The next run reads it back as
// req.DesignSessionID, which (a) skips the requirement-seeded prompt branch
// (`if skipAnalysis && sourceSID == ""`) so the architect prompt degrades to
// "基于我们刚才完成的需求分析对话…" with NO requirement content, and (b) sends
// `--resume <id>` against a conversation that does not exist — a permanent
// self-locking failure.
//
// The two early-return conditions are the whole safety story, so both are
// asserted explicitly: we must NOT clobber a real session.
func TestClearUnestablishedDesignSession(t *testing.T) {
	dir, _ := os.MkdirTemp("", "clear-design-sid-")
	defer os.RemoveAll(dir)
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()

	reqID := seedRequirementForSchedule(t, d)
	reqSvc := service.NewRequirementService(d)
	h := &WizardHandler{reqSvc: reqSvc}

	const preexisting = "sid-preexisting-real-session"

	cases := []struct {
		name        string
		newDesignID string // designRunParams.NewDesignSID
		outSessID   string // claudeStreamOutcome.sessionID (from stream system/init)
		want        string
		why         string
	}{
		{
			name:        "no pre-mint this run -> leave row untouched",
			newDesignID: "",
			outSessID:   "",
			want:        preexisting,
			why:         "NewDesignSID empty means this run did not mint an id (pure resume path), nothing to roll back",
		},
		{
			name:        "CLI reported a session_id -> session was established, keep it",
			newDesignID: "sid-freshly-minted",
			outSessID:   "sid-freshly-minted",
			want:        preexisting,
			why:         "system/init arrived, so the conversation exists and must stay resumable",
		},
		{
			name:        "pre-minted but never established -> clear the stale id",
			newDesignID: "sid-freshly-minted",
			outSessID:   "",
			want:        "",
			why:         "this is the exact RC-2 poisoning case the fix targets",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Reset the row to a known non-empty state before each case so the
			// assertion can distinguish "left alone" from "cleared".
			if err := reqSvc.UpdateDesignSession(reqID, preexisting); err != nil {
				t.Fatalf("seed design_session_id: %v", err)
			}

			h.clearUnestablishedDesignSession(
				&designRunParams{NewDesignSID: c.newDesignID},
				claudeStreamOutcome{sessionID: c.outSessID},
				reqID,
			)

			got, err := reqSvc.Get(reqID)
			if err != nil {
				t.Fatalf("reload requirement: %v", err)
			}
			if got.DesignSessionID != c.want {
				t.Fatalf("DesignSessionID = %q, want %q (%s)", got.DesignSessionID, c.want, c.why)
			}
		})
	}
}

// TestArchitectRemoteCallSitePassesTrueSourceSID is a source-level wiring
// guard for RC-1 — the actual root cause of "architect prompt has no
// requirement content".
//
// The defect was a single wrong field VALUE in a struct literal: the remote
// call site passed `sourceSID: sessionArg` instead of `sourceSID: sourceSID`.
// Since sessionArg is reassigned to the freshly-minted id on the fresh path,
// prepareRemoteAgentRun's `Resume: in.sourceSID != ""` became true even for a
// brand-new conversation, so the run sent `--resume <new-id>` (a conversation
// that does not exist) instead of `--session-id <new-id>`. Combined with the
// pre-minted id being persisted, that poisoned the requirement permanently.
//
// There is no runtime harness for this in unit tests (it needs a real Agent
// server + claude CLI), and the failure mode is invisible to the compiler
// because both fields are strings. So we assert the wiring directly against
// the AST. The dev-stage counterpart this must mirror is the runRemoteCoding
// call site in wizard_coding.go.
//
// If this test fails after a refactor: check that the remoteRunInput literal
// in execArchitectDesign still forwards the *true* source session (the value
// that is "" on a fresh/skip-analysis run) into the `sourceSID` field, and
// keeps `sessionArg` as its own separate field.
func TestArchitectRemoteCallSitePassesTrueSourceSID(t *testing.T) {
	const srcFile = "wizard_architect.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, srcFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", srcFile, err)
	}

	// Collect every `remoteRunInput{...}` composite literal and its field
	// assignments.
	type literal struct {
		pos    token.Position
		fields map[string]string // field name -> rendered value expression
	}
	var literals []literal

	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		ident, ok := cl.Type.(*ast.Ident)
		if !ok || ident.Name != "remoteRunInput" {
			return true
		}
		l := literal{pos: fset.Position(cl.Pos()), fields: map[string]string{}}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			l.fields[key.Name] = renderExpr(kv.Value)
		}
		literals = append(literals, l)
		return true
	})

	if len(literals) == 0 {
		t.Fatalf("no remoteRunInput composite literal found in %s — "+
			"if execArchitectDesign was refactored, update this guard to match", srcFile)
	}

	for _, l := range literals {
		got, present := l.fields["sourceSID"]
		if !present {
			t.Fatalf("%s: remoteRunInput literal at %s has no sourceSID field — "+
				"the resume decision depends on it", srcFile, l.pos)
		}
		if got != "sourceSID" {
			t.Fatalf("%s: remoteRunInput{sourceSID: %s} at %s — RC-1 regression.\n"+
				"sourceSID must be the TRUE source session (empty on a fresh/skip-analysis run); "+
				"passing sessionArg makes fresh runs emit `--resume <new-id>` against a "+
				"non-existent conversation. Mirror wizard_coding.go's runRemoteCoding call site.",
				srcFile, got, l.pos)
		}
		// sessionArg must remain its own field — collapsing the two is the bug.
		if sa, ok := l.fields["sessionArg"]; ok && sa != "sessionArg" {
			t.Fatalf("%s: remoteRunInput{sessionArg: %s} at %s — sessionArg must stay "+
				"the separately-threaded session id", srcFile, sa, l.pos)
		}
	}
}

// renderExpr produces a stable, comparable rendering for the small set of
// expressions used as field values in the literals we inspect (identifiers,
// selector expressions, basic literals). Returns a descriptive placeholder for
// anything else so the assertion above fails loudly rather than silently
// matching.
func renderExpr(e ast.Expr) string {
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
	case *ast.UnaryExpr:
		return v.Op.String() + renderExpr(v.X)
	case *ast.CallExpr:
		return "<call>"
	}
	return "<expr>"
}
