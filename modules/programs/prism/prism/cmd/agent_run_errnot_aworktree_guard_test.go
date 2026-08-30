package cmd

// agent_run_errnot_aworktree_guard_test.go — wiring guard for the
// ErrNotAWorktree handling at both agent-run call sites.
//
// git.ErrNotAWorktree is checked with errors.Is at two call sites:
//
//   - cmd/agent_run.go
//   - cmd/agent_run_sandbox_exec_darwin.go
//
// The internal/git tests exercise the sentinel itself, not the wiring.
// Deleting the errors.Is branch from either call site leaves every other test
// green while the regression returns (dead pane for a normal clone).
//
// The Darwin call site is the more exposed of the two because it is not built
// or exercised on the Linux CI path.
//
// This guard parses both dispatch files as AST and asserts they handle
// git.ErrNotAWorktree by:
// 1. Checking for the error with errors.Is as a non-error condition
// 2. Including a recovery path that sets a git-dir variable to empty string
//
// AST parsing rather than string matching makes the guard insensitive to
// cosmetic changes: variable renames, reformatting across lines, and comment
// edits all pass the guard. Reading the Darwin file as text to the parser
// (build constraints are not evaluated) keeps the guard effective on Linux CI.
// This follows the precedent set by internal/db/schema-version-guard_test.go
// and agent_env_roles_guard_test.go.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAgentRunHandlesErrNotAWorktree asserts that both the bwrap dispatch
// (agent_run.go) and the sandbox-exec dispatch (agent_run_sandbox_exec_darwin.go)
// properly handle git.ErrNotAWorktree via AST inspection, verifying:
//
// 1. An errors.Is(err, git.ErrNotAWorktree) call exists in a boolean context
// 2. A recovery path that assigns an empty string to a git-directory-related variable exists
//
// The guard is insensitive to cosmetic changes (variable renames, reformatting,
// comment edits) and runs on Linux CI by parsing the Darwin source directly
// without evaluating build constraints.
func TestAgentRunHandlesErrNotAWorktree(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: could not determine source file location")
	}
	dir := filepath.Dir(thisFile)

	for _, name := range []string{"agent_run.go", "agent_run_sandbox_exec_darwin.go"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("ParseFile %q: %v", name, err)
			}

			// Walk the AST looking for:
			// 1. errors.Is(x, git.ErrNotAWorktree) in a negated boolean context
			// 2. An assignment to a git-dir variable in a recovery block
			var foundErrIsCheck bool
			var foundRecoveryAssign bool

			ast.Inspect(f, func(node ast.Node) bool {
				// Look for the negated errors.Is call:
				// if err != nil && !errors.Is(err, git.ErrNotAWorktree)
				if ifStmt, ok := node.(*ast.IfStmt); ok {
					if isErrCheckCondition(ifStmt.Cond) {
						foundErrIsCheck = true
					}

					// Look for recovery path:
					// if errors.Is(err, git.ErrNotAWorktree) { worktreeGitDir = "" }
					if isErrIsCallCondition(ifStmt.Cond) && hasEmptyStringAssignmentToGitDir(ifStmt.Body) {
						foundRecoveryAssign = true
					}
				}
				return true
			})

			if !foundErrIsCheck {
				t.Errorf("%s must check git.ErrNotAWorktree with errors.Is as a non-error condition to handle normal clones (issue #2549, #2550)", name)
			}
			if !foundRecoveryAssign {
				t.Errorf("%s must have a recovery path that sets a git-dir variable to empty string when git.ErrNotAWorktree is encountered (issue #2549, #2550)", name)
			}
		})
	}
}

// isErrCheckCondition returns true if the condition represents
// (x != nil && !errors.Is(..., git.ErrNotAWorktree)).
// The exact variable names and formatting do not matter.
func isErrCheckCondition(expr ast.Expr) bool {
	if binExpr, ok := expr.(*ast.BinaryExpr); ok {
		if binExpr.Op.String() != "&&" {
			return false
		}

		// Left side: x != nil (but the variable name can be anything)
		leftIsNotNil := isBinaryOpWithNil(binExpr.X, "!=")

		// Right side: !errors.Is(..., git.ErrNotAWorktree)
		rightIsNotErrIs := isNotErrIsCall(binExpr.Y)

		return leftIsNotNil && rightIsNotErrIs
	}
	return false
}

// isErrIsCallCondition returns true if the condition is a direct call to
// errors.Is(..., git.ErrNotAWorktree) (not negated).
func isErrIsCallCondition(expr ast.Expr) bool {
	return isErrIsCall(expr)
}

// isErrIsCall returns true if expr is errors.Is(x, git.ErrNotAWorktree)
// for any x (the first argument can be any variable).
func isErrIsCall(expr ast.Expr) bool {
	if callExpr, ok := expr.(*ast.CallExpr); ok {
		// Function must be errors.Is
		if !isQualifiedIdent(callExpr.Fun, "errors", "Is") {
			return false
		}
		// Arguments: (anything, git.ErrNotAWorktree)
		if len(callExpr.Args) != 2 {
			return false
		}
		// Second argument must be git.ErrNotAWorktree
		return isQualifiedIdent(callExpr.Args[1], "git", "ErrNotAWorktree")
	}
	return false
}

// isNotErrIsCall returns true if expr is !errors.Is(..., git.ErrNotAWorktree).
func isNotErrIsCall(expr ast.Expr) bool {
	if unaryExpr, ok := expr.(*ast.UnaryExpr); ok {
		if unaryExpr.Op.String() == "!" {
			return isErrIsCall(unaryExpr.X)
		}
	}
	return false
}

// isBinaryOpWithNil returns true if expr is (x op nil) for any identifier x.
func isBinaryOpWithNil(expr ast.Expr, op string) bool {
	if binExpr, ok := expr.(*ast.BinaryExpr); ok {
		if binExpr.Op.String() != op {
			return false
		}
		// Either side can be nil (x != nil or nil != x)
		isLeftNil := isIdent(binExpr.X, "nil")
		isRightNil := isIdent(binExpr.Y, "nil")
		hasIdent := !isLeftNil || !isRightNil
		return (isLeftNil || isRightNil) && hasIdent
	}
	return false
}

// isQualifiedIdent returns true if expr is pkg.name (a selector expression).
func isQualifiedIdent(expr ast.Expr, pkg string, name string) bool {
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		if !isIdent(sel.X, pkg) {
			return false
		}
		return sel.Sel.Name == name
	}
	return false
}

// isIdent returns true if expr is an identifier with the given name.
func isIdent(expr ast.Expr, name string) bool {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name == name
	}
	return false
}

// hasEmptyStringAssignmentToGitDir returns true if the block contains
// an assignment of an empty string to a variable with "gitdir" or
// "worktreegitdir" in its name (case-insensitive).
func hasEmptyStringAssignmentToGitDir(block *ast.BlockStmt) bool {
	if block == nil {
		return false
	}

	for _, stmt := range block.List {
		if assignStmt, ok := stmt.(*ast.AssignStmt); ok {
			if len(assignStmt.Lhs) > 0 && len(assignStmt.Rhs) > 0 {
				// Check if LHS is an identifier related to git directory
				if lhsIdent, ok := assignStmt.Lhs[0].(*ast.Ident); ok {
					if isGitDirVariable(lhsIdent.Name) {
						// Check if RHS is an empty string literal
						if isEmptyString(assignStmt.Rhs[0]) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// isGitDirVariable returns true if the variable name suggests it holds
// a git directory path (case-insensitive check for "gitdir" or "worktreegit").
func isGitDirVariable(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "gitdir") || strings.Contains(lower, "worktreegit")
}

// isEmptyString returns true if expr is a string literal "".
func isEmptyString(expr ast.Expr) bool {
	if lit, ok := expr.(*ast.BasicLit); ok {
		return lit.Kind == token.STRING && lit.Value == `""`
	}
	return false
}
