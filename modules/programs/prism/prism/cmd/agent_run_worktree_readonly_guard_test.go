package cmd

// Wiring guard for #3079: both agent-run dispatch files must copy the
// read-only flag into the container.Config that builds the sandbox. If a
// handler drops the WorktreeReadOnly key, every isolator test stays green
// while review agents and investigators get a writable worktree again.
//
// The guard parses the files as AST, so it also covers the Darwin file on
// Linux CI, where that file does not compile.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAgentRunSetsWorktreeReadOnly(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: could not determine source file location")
	}
	dir := filepath.Dir(thisFile)

	for _, name := range []string{"agent_run.go", "agent_run_sandbox_exec_darwin.go"} {
		t.Run(name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("ParseFile %q: %v", name, err)
			}
			var configs, withKey int
			ast.Inspect(f, func(node ast.Node) bool {
				lit, ok := node.(*ast.CompositeLit)
				if !ok || !isQualifiedIdent(lit.Type, "container", "Config") {
					return true
				}
				configs++
				for _, elt := range lit.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok && isIdent(kv.Key, "WorktreeReadOnly") {
						withKey++
						break
					}
				}
				return true
			})
			if configs == 0 {
				t.Fatalf("%s has no container.Config literal; the guard no longer matches the code", name)
			}
			if withKey != configs {
				t.Errorf("%s: %d of %d container.Config literals set WorktreeReadOnly; each must copy the flag from agent_status.worktree_readonly", name, withKey, configs)
			}
		})
	}
}
