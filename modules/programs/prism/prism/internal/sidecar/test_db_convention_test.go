package sidecar

// test_db_convention_test.go — pins the test-database convention (#2598).
//
// db.Open applies the schema, seeds schema_version, and runs all 38
// migrations against a WAL with synchronous=FULL. Before #2612 each statement
// committed in autocommit mode and one open cost 73 fsyncs. Since #2612 the
// sequence runs in one transaction and one open costs 7. This package opens a
// database per test, so a direct db.Open call in one test file adds 7 fsyncs
// to every run of the package, where sidecartest.OpenDB adds none.
//
// That cost is invisible on a developer host, where the test tempdir is a
// tmpfs and fsync is a no-op. It is not invisible on a CI runner, where the
// pre-#2612 figure is what pushed this package past the 10-minute go test
// timeout (#2598).
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
	"strconv"
	"strings"
	"testing"
)

// dbImportPath is the package whose Open function the convention governs.
const dbImportPath = "github.com/prismatic-koi/prism/internal/db"

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

// directDBOpens returns the position of every call to the Open function of
// dbImportPath in file.
//
// It resolves the import path rather than matching the identifier text, so an
// aliased import (prismdb "…/internal/db"; prismdb.Open) and a dot import
// (Open) are both detected. A file that does not import the package at all
// yields nothing, so a same-named Open on any other package is not a hit.
// db.OpenReadOnly is not a hit either: it does no writes and costs no fsync.
//
// Known limit, stated rather than implied: the matcher works on one file at a
// time and does not resolve types, so a local variable that shadows the
// package name would produce a false hit. Nothing in this package does that,
// and a false hit is a visible failure rather than a silent hole.
//
// Every caller in this file uses this one function. That is deliberate: a
// meta-test that carried its own copy of the matcher could not detect a break
// in the matcher the guard actually runs.
func directDBOpens(fset *token.FileSet, file *ast.File) []token.Position {
	// Resolve the local name the file uses for the package.
	var (
		localName string
		dotImport bool
		imported  bool
	)
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != dbImportPath {
			continue
		}
		imported = true
		switch {
		case imp.Name == nil:
			localName = filepath.Base(path) // "db"
		case imp.Name.Name == ".":
			dotImport = true
		case imp.Name.Name == "_":
			return nil // imported for side effects only; cannot be called
		default:
			localName = imp.Name.Name
		}
		break
	}
	if !imported {
		return nil
	}

	var out []token.Position
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		var hit bool
		if dotImport {
			ident, ok := call.Fun.(*ast.Ident)
			hit = ok && ident.Name == "Open"
		} else {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "Open" {
				base, ok := sel.X.(*ast.Ident)
				hit = ok && base.Name == localName
			}
		}

		if hit {
			out = append(out, fset.Position(call.Pos()))
		}
		return true
	})
	return out
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
				rel := filepath.Base(path)
				if dir != "." {
					rel = filepath.Join(dir, filepath.Base(path))
				}
				if reason, exempt := openDBExemptFiles[rel]; exempt {
					t.Logf("exempt: %s (%s)", rel, reason)
					continue
				}

				for _, pos := range directDBOpens(fset, file) {
					found++
					t.Errorf(
						"%s:%d calls db.Open directly. Use sidecartest.OpenDB(t, path) — a direct "+
							"open costs 7 fsyncs and this package opens one database per test (#2598). "+
							"See docs/test-database-fsync.md. If this test drives the migrations or "+
							"asserts on db.Open itself, add it to openDBExemptFiles with a reason.",
						rel, pos.Line,
					)
				}
			}
		}
	}

	if found == 0 {
		t.Log("no direct db.Open calls outside the exempt list")
	}
}

// TestDirectDBOpens_Matcher proves the matcher the guard runs is not blind.
// It calls directDBOpens — the same function TestSidecarTests_UseSidecartestOpenDB
// uses — so a break in the matcher fails this test too. A meta-test with its
// own copy of the matcher would report a clean package forever after a break,
// which is the exact vacuity this test exists to prevent.
func TestDirectDBOpens_Matcher(t *testing.T) {
	const dbImport = `"github.com/prismatic-koi/prism/internal/db"`

	cases := []struct {
		name string
		src  string
		want int
	}{
		{
			name: "plain import",
			src:  "package s\nimport " + dbImport + "\nfunc f(p string) { d, _ := db.Open(p); _ = d }\n",
			want: 1,
		},
		{
			name: "aliased import",
			src:  "package s\nimport prismdb " + dbImport + "\nfunc f(p string) { d, _ := prismdb.Open(p); _ = d }\n",
			want: 1,
		},
		{
			name: "dot import",
			src:  "package s\nimport . " + dbImport + "\nfunc f(p string) { d, _ := Open(p); _ = d }\n",
			want: 1,
		},
		{
			name: "two calls in one file",
			src:  "package s\nimport " + dbImport + "\nfunc f(p string) { db.Open(p); db.Open(p + \"2\") }\n",
			want: 2,
		},
		{
			name: "OpenReadOnly is not a hit",
			src:  "package s\nimport " + dbImport + "\nfunc f(p string) { db.OpenReadOnly(p) }\n",
			want: 0,
		},
		{
			name: "same-named Open on another package",
			src:  "package s\nimport \"database/sql\"\nfunc f(p string) { sql.Open(\"sqlite\", p) }\n",
			want: 0,
		},
		{
			name: "package not imported",
			src:  "package s\nfunc f(p string) { _ = p }\n",
			want: 0,
		},
		{
			name: "blank import cannot be called",
			src:  "package s\nimport _ " + dbImport + "\nfunc f(p string) { _ = p }\n",
			want: 0,
		},
		{
			name: "sidecartest.OpenDB is the compliant form",
			src: "package s\nimport \"github.com/prismatic-koi/prism/internal/sidecar/sidecartest\"\n" +
				"func f(p string) { sidecartest.OpenDB(nil, p) }\n",
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "sample.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse sample: %v", err)
			}
			got := directDBOpens(fset, file)
			if len(got) != tc.want {
				t.Errorf("directDBOpens found %d call(s), want %d — the guard matcher is broken", len(got), tc.want)
			}
		})
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

		if len(directDBOpens(fset, file)) == 0 {
			t.Errorf("exempt entry %s no longer calls db.Open — remove the exemption", rel)
		}
	}
}
