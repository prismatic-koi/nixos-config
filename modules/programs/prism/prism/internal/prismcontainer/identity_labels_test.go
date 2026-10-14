package prismcontainer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// identityLabelConsts names the constants of the labels that identityLabels
// sets. TestLabelReads_OnlyIdentityLabels holds each label read in this
// package to this set.
var identityLabelConsts = map[string]string{
	"LabelInstanceID": LabelInstanceID,
	"LabelService":    LabelService,
	"LabelBuild":      LabelBuild,
}

// labelOptions returns the --label options of a podman argument vector as
// a map.
func labelOptions(t *testing.T, args []string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--label" {
			continue
		}
		k, v, _ := strings.Cut(args[i+1], "=")
		if _, dup := got[k]; dup {
			t.Errorf("label %s is set twice: %q", k, args)
		}
		got[k] = v
	}
	return got
}

// TestContainerArgs_SetEveryIdentityLabel: each podman run argument vector
// of prism sets each identity label, so that no image label reaches the
// container under that key. A label that does not apply is empty.
func TestContainerArgs_SetEveryIdentityLabel(t *testing.T) {
	const id = "0f0e0d0c-0b0a-4908-8706-050403020100"
	c := Caller{SessionName: "prism-test@labels", InstanceID: id}
	v := validRun{image: "docker.io/library/alpine", mount: MountNone, timeout: time.Minute}
	unit := buildUnitPrefix(id) + "00000001"
	for name, tc := range map[string]struct {
		args []string
		want map[string]string
	}{
		"run":    {runArgs(c, v, "n", "/cid"), map[string]string{LabelInstanceID: id, LabelService: "", LabelBuild: ""}},
		"start":  {startArgs(c, validStart{validRun: v}, "n", "01020304", "/cid"), map[string]string{LabelInstanceID: id, LabelService: "01020304", LabelBuild: ""}},
		"anchor": {anchorRunArgs(id, unit, time.Minute), map[string]string{LabelInstanceID: id, LabelService: "", LabelBuild: unit}},
	} {
		t.Run(name, func(t *testing.T) {
			if len(tc.want) != len(identityLabelConsts) {
				t.Fatalf("the test case names %d labels, want each of the %d identity labels", len(tc.want), len(identityLabelConsts))
			}
			if got := labelOptions(t, tc.args); !maps.Equal(got, tc.want) {
				t.Errorf("--label options = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLabelReads_OnlyIdentityLabels: every read of a label in this package
// names an identity label. If code reads another label to decide the owner
// or the kind of a container, an image LABEL can set it. Add the label to
// identityLabels first, then to identityLabelConsts.
func TestLabelReads_OnlyIdentityLabels(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var reads int
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ix, ok := n.(*ast.IndexExpr)
			if !ok {
				return true
			}
			if sel, ok := ix.X.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Labels" {
				return true
			}
			reads++
			if key, ok := ix.Index.(*ast.Ident); !ok || identityLabelConsts[key.Name] == "" {
				t.Errorf("%s reads a label that is not an identity label", fset.Position(ix.Pos()))
			}
			return true
		})
	}
	if reads == 0 {
		t.Fatal("found no label read, so the test proves nothing")
	}
}
