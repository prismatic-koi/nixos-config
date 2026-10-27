package projecttrust

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestPRBranchRecord_OnlyTheSpawnFrontDoorsWriteIt: the PR-branch record
// gives --no-approve only while no sandbox can set or clear it (issue
// #3287). prism.db is not in any sandbox, so a sandbox reaches the table only
// through a host-side writer. This test lists every writer. A new caller (a
// host-API handler or a `prism event` kind, for example) fails it: decide
// first whether a sandbox can reach the new caller, then add it here.
func TestPRBranchRecord_OnlyTheSpawnFrontDoorsWriteIt(t *testing.T) {
	root := filepath.Join("..", "..")
	sqlWrite := regexp.MustCompile(`(?i)(INTO|UPDATE|DELETE\s+FROM|DROP\s+TABLE|ALTER\s+TABLE)\s+pr_branch_sessions\b`)
	want := map[string][]string{
		"SQL write":                    {"internal/db/pr_branch.go"},
		"call of MarkPRBranchSession":  {"internal/session/pr_branch.go"},
		"call of ClearPRBranchSession": {"internal/session/pr_branch.go"},
		"call of RecordPRBranch":       {"cmd/pr.go", "internal/session/spawn.go"},
	}
	got := map[string][]string{}
	add := func(kind, file string) {
		if !slices.Contains(got[kind], file) {
			got[kind] = append(got[kind], file)
		}
	}

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING && sqlWrite.MatchString(n.Value) {
					add("SQL write", rel)
				}
			case *ast.CallExpr:
				var name string
				switch fn := n.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				switch name {
				case "MarkPRBranchSession", "ClearPRBranchSession", "RecordPRBranch":
					add("call of "+name, rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for kind, files := range want {
		slices.Sort(got[kind])
		if !slices.Equal(got[kind], files) {
			t.Errorf("%s of pr_branch_sessions in %v, want only in %v", kind, got[kind], files)
		}
	}
}
