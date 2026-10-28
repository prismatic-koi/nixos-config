package git

// host_guard_test.go — a source scan for the rule in
// docs/invariants/host-git-commands.md.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
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
// It also fails if a function starts gh or glab and does not call
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
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		findings, err := checkHostGitSource(rel, src)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, f := range findings {
			t.Error(f)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("scanned %d files under %s; the walk did not find the module", scanned, root)
	}
}

// TestHostGit_GuardFindings feeds the checker small files. A gh call that
// sets cmd.Env to a value that is not from git.EnvForDir or Repo.Env must
// fail: an assignment to the Env field is not a call to Env.
func TestHostGit_GuardFindings(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"git exec", `package p
import "os/exec"
func f() { _ = exec.Command("git", "status") }
`, 1},
		{"git exec with a context", `package p
import ("context"; "os/exec")
func f(ctx context.Context) { _ = exec.CommandContext(ctx, "git", "status") }
`, 1},
		{"gh with the inherited env", `package p
import ("os"; "os/exec")
func f() {
	cmd := exec.Command("gh", "pr", "view")
	cmd.Env = os.Environ()
	_ = cmd.Run()
}
`, 1},
		{"glab with a filtered env", `package p
import ("os"; "os/exec")
func clean(e []string) []string { return e }
func f() {
	cmd := exec.Command("glab", "mr", "view")
	cmd.Env = clean(os.Environ())
	_ = cmd.Run()
}
`, 1},
		{"gh with no env", `package p
import "os/exec"
func f() { _ = exec.Command("gh", "pr", "view").Run() }
`, 1},
		{"gh with git.EnvForDir", `package p
import ("os"; "os/exec"; "github.com/prismatic-koi/prism/internal/git")
func f() error {
	env, err := git.EnvForDir("", os.Environ())
	if err != nil { return err }
	cmd := exec.Command("gh", "pr", "view")
	cmd.Env = env
	return cmd.Run()
}
`, 0},
		{"gh with Repo.Env in a closure", `package p
import ("os"; "os/exec"; "github.com/prismatic-koi/prism/internal/git")
func f(r git.Repo) {
	env := r.Env(os.Environ())
	run := func() error {
		cmd := exec.Command("gh", "api", "x")
		cmd.Env = env
		return cmd.Run()
	}
	_ = run()
}
`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := checkHostGitSource("internal/p/p.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(findings) != tc.want {
				t.Errorf("findings = %d %v, want %d", len(findings), findings, tc.want)
			}
		})
	}
}

// checkHostGitSource returns one finding for each call in src that breaks
// the rule. rel is the path of the file below the module root.
func checkHostGitSource(rel string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, err
	}
	var findings []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// Only a call counts. `cmd.Env = ...` is a field of exec.Cmd, not
		// a call to Repo.Env.
		usesHostEnv := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "EnvForDir" || sel.Sel.Name == "Env") {
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
					findings = append(findings, fmt.Sprintf("%s: %s starts git directly; use git.Repo (see docs/invariants/host-git-commands.md)",
						fset.Position(call.Pos()), fn.Name.Name))
				}
			case "gh", "glab":
				key := rel + ":" + fn.Name.Name
				if !usesHostEnv && ghWithoutEnvAllowed[key] == "" {
					findings = append(findings, fmt.Sprintf("%s: %s starts %s without a call to git.EnvForDir or Repo.Env (see docs/invariants/host-git-commands.md)",
						fset.Position(call.Pos()), fn.Name.Name, prog))
				}
			}
			return true
		})
	}
	return findings, nil
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
