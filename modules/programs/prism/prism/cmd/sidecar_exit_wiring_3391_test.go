package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestRunSidecar_PIDFileGoesOnlyAfterTheHostAPIClose_3391 pins the wiring
// of the exit path of `prism sidecar`, which no test runs in process: it
// binds real sockets, starts host sweeps, and handles signals.
// TestSidecarCleanExit_QueuedSessionShutdownLeavesNoContainer_3391 tests
// the behaviour of removeOwnPIDFileAfterHostAPI and CloseHostAPI.
//
// If a non-test file of cmd calls RemoveOwnSidecarPIDFile directly, or
// runSidecar does not give the CloseHostAPI method of its sidecar to the
// deferred removeOwnPIDFileAfterHostAPI, the sidecar can remove its PID file
// while a host-API handler runs (#3391), and this test fails.
func TestRunSidecar_PIDFileGoesOnlyAfterTheHostAPIClose_3391(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var runSidecarDecl *ast.FuncDecl
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name == "runSidecar" {
				runSidecarDecl = fn
			}
			if fn.Name.Name == "removeOwnPIDFileAfterHostAPI" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "RemoveOwnSidecarPIDFile" {
					t.Errorf("%s: %s calls RemoveOwnSidecarPIDFile; call removeOwnPIDFileAfterHostAPI, which waits for the host-API handlers first",
						fset.Position(sel.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
	if runSidecarDecl == nil {
		t.Fatal("runSidecar not found in the package")
	}

	// The deferred call names the variable that holds the close function.
	closeVar := ""
	ast.Inspect(runSidecarDecl.Body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		ast.Inspect(d.Call, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "removeOwnPIDFileAfterHostAPI" && len(call.Args) == 2 {
				if v, ok := call.Args[1].(*ast.Ident); ok {
					closeVar = v.Name
				}
			}
			return true
		})
		return true
	})
	if closeVar == "" {
		t.Fatal("runSidecar does not defer removeOwnPIDFileAfterHostAPI with a close function variable")
	}

	setToCloseHostAPI := false
	ast.Inspect(runSidecarDecl.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		sel, ok2 := as.Rhs[0].(*ast.SelectorExpr)
		if ok && ok2 && lhs.Name == closeVar && sel.Sel.Name == "CloseHostAPI" {
			setToCloseHostAPI = true
		}
		return true
	})
	if !setToCloseHostAPI {
		t.Errorf("runSidecar does not set %s to the CloseHostAPI method of its sidecar", closeVar)
	}
}
