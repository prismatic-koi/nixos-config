package doclint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// synthPrismRoot builds a tiny fake prism source tree at t.TempDir(): a
// go.mod, a docs/ dir, and a couple of .go files with a few known
// identifiers. Returns the tempdir path.
func synthPrismRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	must := func(name, body string) {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	must("go.mod", "module fake\n\ngo 1.22\n")
	must("docs/keep.md", "")
	must("internal/podmanproxy/policy.go", `package podmanproxy

func checkHostConfig() {}

var hostConfig = struct{}{}

// The reason format string for host_bind: is emitted as a literal.
const _ = "host_bind:"

// agent_status.instance_id lives in a CREATE TABLE literal below;
// the loose ident extractor picks up "agent_status" and "instance_id"
// as bare words so SQL table.column references resolve.
const schemaTables = `+"`"+`
CREATE TABLE agent_status (session_name TEXT, instance_id TEXT);
`+"`"+`
`)
	must("internal/config/config.go", `package config

const (
	// ExampleEnvVar is referenced by docs as an env var name.
	ExampleEnvVar = "XDG_STATE_HOME"
)
`)
	return root
}

// TestScan_DetectsStaleIdent is the load-bearing "the lint is not a no-op"
// test: a synthetic doc with a KNOWN-stale identifier must produce a
// finding. Paired with TestScan_ResolvesRealIdent which asserts the same
// harness passes when the identifier is real.
func TestScan_DetectsStaleIdent(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	if err := os.WriteFile(docPath, []byte("A reference to `mountTypeAllowlist` which does not exist.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Token != "mountTypeAllowlist" {
		t.Errorf("finding token: got %q, want %q", f.Token, "mountTypeAllowlist")
	}
	if f.Rule != "go_ident" {
		t.Errorf("finding rule: got %q, want %q", f.Rule, "go_ident")
	}
	if !strings.HasSuffix(f.File, "example.md") {
		t.Errorf("finding file: got %q, want to end with example.md", f.File)
	}
	if f.Line != 1 {
		t.Errorf("finding line: got %d, want 1", f.Line)
	}
}

func TestScan_ResolvesRealIdent(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	// `checkHostConfig` is a real ident in the synth root; `hostConfig`
	// too. Neither should produce a finding.
	if err := os.WriteFile(docPath, []byte("References `checkHostConfig` and `hostConfig`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected zero findings, got %+v", findings)
	}
}

func TestScan_IgnoreDirectiveSuppressesFinding(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	body := "<!-- doclint-ignore: mountTypeAllowlist -->\nReference `mountTypeAllowlist`.\n"
	if err := os.WriteFile(docPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected doclint-ignore to suppress finding, got %+v", findings)
	}
}

func TestScan_SkipFileDirectiveSuppressesAllFindings(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	body := "<!-- doclint-skip-file: describes an external interface -->\n\n" +
		"Refs to `mountTypeAllowlist`, `anotherStaleIdent`, `nopeNotHere`.\n"
	if err := os.WriteFile(docPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected doclint-skip-file to suppress all findings, got %+v", findings)
	}
}

// TestScan_SkipFileDirectiveInsideFencedBlockDoesNotSkip is the regression
// for the review-context finding on PR #2344: a doclint-skip-file directive
// that appears INSIDE a fenced code block is a prose example, not an active
// directive, and must NOT opt the file out. Before the fix, this test's
// stale `mountTypeAllowlist` reference would have been silently masked.
func TestScan_SkipFileDirectiveInsideFencedBlockDoesNotSkip(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	body := "Stale reference `mountTypeAllowlist`.\n\n" +
		"Directive syntax example (must NOT activate):\n\n" +
		"```markdown\n" +
		"<!-- doclint-skip-file: example reason -->\n" +
		"```\n"
	if err := os.WriteFile(docPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected fenced-block skip-file example to NOT opt the file out (want 1 finding for `mountTypeAllowlist`), got %d: %+v", len(findings), findings)
	}
	if findings[0].Token != "mountTypeAllowlist" {
		t.Errorf("expected finding on `mountTypeAllowlist`, got %+v", findings[0])
	}
}

// TestScan_IgnoreDirectiveInsideFencedBlockDoesNotSuppress is the sibling
// regression for the same class of leak on the per-token annotation. A
// `<!-- doclint-ignore: token -->` directive that appears inside a fenced
// code block is a prose example of the directive syntax and must NOT
// contribute tokens to the ignore set.
func TestScan_IgnoreDirectiveInsideFencedBlockDoesNotSuppress(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	body := "Stale reference `mountTypeAllowlist`.\n\n" +
		"```markdown\n" +
		"<!-- doclint-ignore: mountTypeAllowlist -->\n" +
		"```\n"
	if err := os.WriteFile(docPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected fenced-block ignore example to NOT suppress (want 1 finding), got %d: %+v", len(findings), findings)
	}
	if findings[0].Token != "mountTypeAllowlist" {
		t.Errorf("expected finding on `mountTypeAllowlist`, got %+v", findings[0])
	}
}

// TestScan_InlineBacktickedDirectiveIsProseNotActive is the sibling of the
// fenced-block tests for the second half of the same review finding: a
// directive that appears INLINE inside a backticked prose phrase (e.g. a
// section heading like “ ### Per-file: `<!-- doclint-skip-file: reason -->` “)
// is a prose example, not an active directive, because the directive text
// is not at the start of a logical line.
func TestScan_InlineBacktickedDirectiveIsProseNotActive(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	body := "Stale reference `mountTypeAllowlist`.\n\n" +
		"### Per-file: `<!-- doclint-skip-file: example -->`\n\n" +
		"### Per-token: `<!-- doclint-ignore: mountTypeAllowlist -->`\n"
	if err := os.WriteFile(docPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected inline-backticked prose directives to be inert (want 1 finding), got %d: %+v", len(findings), findings)
	}
	if findings[0].Token != "mountTypeAllowlist" {
		t.Errorf("expected finding on `mountTypeAllowlist`, got %+v", findings[0])
	}
}

func TestScan_SkipsAbsentRepoRootAGENTS(t *testing.T) {
	// Passing repoRoot=\"\" simulates the nix sandbox build where the
	// repo-root AGENTS.md is not present. The lint must not fail and
	// must not error.
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	if err := os.WriteFile(docPath, []byte("nothing to flag here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected zero findings, got %+v", findings)
	}
}

func TestScan_FencedCodeBlocksNotScanned(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	body := "Prose reference `checkHostConfig` (real).\n\n" +
		"```go\n" +
		"// fenced block — `bogusIdent` must NOT be flagged\n" +
		"foo := bogusIdent()\n" +
		"```\n"
	if err := os.WriteFile(docPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected fenced-block tokens to be skipped, got %+v", findings)
	}
}

func TestScan_ColonTokenPrefixLiteral(t *testing.T) {
	// `host_bind:` is stored in the synth root as a Go string literal
	// (const _ = "host_bind:"). It also appears as a bare Go
	// identifier `host_bind` via the identRe extraction. Either match
	// should satisfy the colon-token resolver.
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	if err := os.WriteFile(docPath, []byte("Reason format `host_bind:<path>`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected host_bind:<path> to resolve, got %+v", findings)
	}
}

func TestScan_SqlTableColumnResolvesFromStringLiteralIdents(t *testing.T) {
	// `agent_status.instance_id` — both segments live inside a Go
	// string literal in the synth root but the loose identRe
	// extractor picks them up as bare words. Dotted resolver checks
	// each segment as an ident.
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	if err := os.WriteFile(docPath, []byte("Column `agent_status.instance_id`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected SQL table.column to resolve, got %+v", findings)
	}
}

// TestScan_FileWithMember_BareBasename asserts that a doc reference like
// `policy.go::checkHostConfig` (no directory) resolves via the basename
// index, without requiring the doc to name the full path.
func TestScan_FileWithMember_BareBasename(t *testing.T) {
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	if err := os.WriteFile(docPath, []byte("See `policy.go::checkHostConfig`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected bare basename to resolve, got %+v", findings)
	}
}

func TestScan_FindingIncludesResolutionRule(t *testing.T) {
	// AC #2: every finding must name the file, line, offending token,
	// AND the resolution rule that was attempted.
	root := synthPrismRoot(t)
	docPath := filepath.Join(root, "docs", "example.md")
	if err := os.WriteFile(docPath, []byte("Path `internal/no/such/file.go`.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.File == "" {
		t.Errorf("finding.File empty")
	}
	if f.Line == 0 {
		t.Errorf("finding.Line 0")
	}
	if f.Token != "internal/no/such/file.go" {
		t.Errorf("finding.Token: got %q", f.Token)
	}
	if f.Rule != "file_path" {
		t.Errorf("finding.Rule: got %q, want file_path", f.Rule)
	}
	// String output must incorporate all four fields.
	s := f.String()
	for _, want := range []string{"example.md", "1:", "internal/no/such/file.go", "rule=file_path"} {
		if !strings.Contains(s, want) {
			t.Errorf("finding.String() missing %q: %s", want, s)
		}
	}
}
