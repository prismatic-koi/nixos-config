package sidecar

// test_db_convention_test.go — pins the test-database convention (#2598).
//
// db.Open costs 73 fsyncs: it applies the schema, seeds schema_version, and
// runs all 39 migrations, each committing in autocommit mode against a WAL
// with synchronous=FULL. This package opens a database per test, so a direct
// db.Open call in one test file adds 73 fsyncs to every run of the package.
//
// That cost is invisible on a developer host, where the test tempdir is a
// tmpfs and fsync is a no-op. It is not invisible on a CI runner, where it is
// what pushed this package past the 10-minute go test timeout (#2598).
//
// The guard tests in sidecartest/templatedb_test.go pin that the template is
// healthy. This test pins the other half: that call sites reach for it. Both
// are needed. Without this one, a new test that calls db.Open directly
// reintroduces the full cost and nothing fails.
//
// The convention is documented in
// modules/programs/prism/prism/docs/test-database-fsync.md.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// openDBExemptFiles lists the files that may call db.Open directly, by path
// relative to this package's directory.
//
// Add an entry only for a test that drives the migrations or that asserts on
// db.Open itself — see "The two exceptions" in the convention document. Do
// not add one to skip the helper for convenience.
var openDBExemptFiles = map[string]string{
	filepath.Join("sidecartest", "templatedb.go"):      "the helper itself: it builds the template and opens the seeded copy",
	filepath.Join("sidecartest", "templatedb_test.go"): "asserts on db.Open itself: it compares a seeded database against one db.Open built from nothing",
}

// TestSidecarTests_UseSidecartestOpenDB fails when a file in this package or
// in sidecartest calls db.Open directly instead of sidecartest.OpenDB.
func TestSidecarTests_UseSidecartestOpenDB(t *testing.T) {
	dirs := []string{".", "sidecartest"}

	var found int
	for _, dir := range dirs {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}

		for _, pkg := range pkgs {
			for path, file := range pkg.Files {
				rel := path
				if dir != "." {
					rel = filepath.Join(dir, filepath.Base(path))
				} else {
					rel = filepath.Base(path)
				}
				if reason, exempt := openDBExemptFiles[rel]; exempt {
					t.Logf("exempt: %s (%s)", rel, reason)
					continue
				}

				ast.Inspect(file, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Open" {
						return true
					}
					ident, ok := sel.X.(*ast.Ident)
					if !ok || ident.Name != "db" {
						return true
					}
					found++
					pos := fset.Position(call.Pos())
					t.Errorf(
						"%s:%d calls db.Open directly. Use sidecartest.OpenDB(t, path) — a direct "+
							"open costs 73 fsyncs and this package opens one database per test (#2598). "+
							"See docs/test-database-fsync.md. If this test drives the migrations or "+
							"asserts on db.Open itself, add it to openDBExemptFiles with a reason.",
						rel, pos.Line,
					)
					return true
				})
			}
		}
	}

	if found == 0 {
		t.Log("no direct db.Open calls outside the exempt list")
	}
}

// TestOpenDBConventionGuard_FindsADirectCall proves the guard above is not
// vacuous. It runs the same detection over a source file that does call
// db.Open, and requires a hit. Without this, a broken matcher (a renamed
// helper, an import alias, a parser change) would report a clean package
// forever.
func TestOpenDBConventionGuard_FindsADirectCall(t *testing.T) {
	const src = `package sample

import "github.com/prismatic-koi/prism/internal/db"

func openIt(path string) {
	d, _ := db.Open(path)
	_ = d
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", src, 0)
	if err != nil {
		t.Fatalf("parse sample: %v", err)
	}

	var hits int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Open" {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "db" {
			return true
		}
		hits++
		return true
	})

	if hits != 1 {
		t.Errorf("detection found %d db.Open calls in the sample, want 1 — the guard matcher is broken", hits)
	}
}

// TestOpenDBConventionGuard_ExemptListIsAccurate fails when an exempt entry
// names a file that no longer calls db.Open. A stale exemption is a hole: the
// next file at that path inherits it silently.
func TestOpenDBConventionGuard_ExemptListIsAccurate(t *testing.T) {
	for rel, reason := range openDBExemptFiles {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("exempt entry %s has no reason", rel)
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, rel, nil, 0)
		if err != nil {
			t.Errorf("exempt entry %s does not parse: %v", rel, err)
			continue
		}

		var hits int
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Open" {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "db" {
				return true
			}
			hits++
			return true
		})

		if hits == 0 {
			t.Errorf("exempt entry %s no longer calls db.Open — remove the exemption", rel)
		}
	}
}
