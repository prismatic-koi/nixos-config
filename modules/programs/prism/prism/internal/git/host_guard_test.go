package git

// host_guard_test.go — a source scan for the rule in
// docs/invariants/host-git-commands.md.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hostGitExecAllowed names the files that can start git themselves.
var hostGitExecAllowed = map[string]bool{
	"internal/git/host.go": true,
}

// ghWithoutEnvAllowed names the functions that start gh without the env of
// git.EnvForDir or Repo.Env, with the reason. Each one names the repo on the
// command line, so gh runs no git.
var ghWithoutEnvAllowed = map[string]string{
	"internal/dashboard/poll.go:FetchGitHubStats": "gh api graphql with a literal query",
	"internal/mergequeue/watcher.go:execGH":       "every caller passes --repo or a repos/<owner>/<repo> path",
}

// TestHostGit_NoDirectGitExec fails if a non-test file outside
// hostGitExecAllowed calls exec.Command or exec.CommandContext with "git".
// Use git.Repo, git.OpenDir, or git.CloneCommand.
//
// It also fails if a function starts gh or glab and does not get the env from
// git.EnvForDir or Repo.Env, unless ghWithoutEnvAllowed names it.
func TestHostGit_NoDirectGitExec(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		scanned++
		checkHostGitFile(t, path, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("scanned %d files under %s; the walk did not find the module", scanned, root)
	}
}

func checkHostGitFile(t *testing.T, path, rel string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		usesHostEnv := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "EnvForDir" || sel.Sel.Name == "Env") {
				usesHostEnv = true
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			prog := execProgram(call)
			switch prog {
			case "git":
				if !hostGitExecAllowed[rel] {
					t.Errorf("%s: %s starts git directly; use git.Repo (see docs/invariants/host-git-commands.md)",
						fset.Position(call.Pos()), fn.Name.Name)
				}
			case "gh", "glab":
				key := rel + ":" + fn.Name.Name
				if !usesHostEnv && ghWithoutEnvAllowed[key] == "" {
					t.Errorf("%s: %s starts %s without git.EnvForDir or Repo.Env (see docs/invariants/host-git-commands.md)",
						fset.Position(call.Pos()), fn.Name.Name, prog)
				}
			}
			return true
		})
	}
}

// execProgram returns the program name of an exec.Command or
// exec.CommandContext call when it is a string literal, or "".
func execProgram(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return ""
	}
	idx := -1
	switch sel.Sel.Name {
	case "Command":
		idx = 0
	case "CommandContext":
		idx = 1
	}
	if idx < 0 || len(call.Args) <= idx {
		return ""
	}
	lit, ok := call.Args[idx].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return name
}
