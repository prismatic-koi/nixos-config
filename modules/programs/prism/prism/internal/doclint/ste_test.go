package doclint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// synthSteRoot builds a fake prism source tree with one of the STE-in-scope
// basenames placed at <root>/docs/, so tests can drive the STE pass against
// realistic input without touching the real docs.
func synthSteRoot(t *testing.T, docBody string) (root, docPath string) {
	t.Helper()
	root = synthPrismRoot(t)
	// Overwrite the fake docs/keep.md placeholder with an in-scope
	// basename so the STE pass activates.
	docPath = filepath.Join(root, "docs", "doclint.md")
	if err := os.WriteFile(docPath, []byte(docBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// Remove the stray keep.md so the two docs don't both scan.
	_ = os.Remove(filepath.Join(root, "docs", "keep.md"))
	return root, docPath
}

// steRules returns the set of STE rule tags in each finding.
func steRules(fs []Finding) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		if f.Category == "ste" {
			out[f.Rule] = true
		}
	}
	return out
}

// TestSte_DetectsAllFiveClasses is the load-bearing "one positive case per
// rule" test. AC: the lint reports a finding for each of the five checks.
func TestSte_DetectsAllFiveClasses(t *testing.T) {
	body := strings.Join([]string{
		"# Test doc",
		"",
		"A semicolon; here.",                    // 8.1
		"The worker will fail if it can't run.", // 4.2
		"For example, e.g. this one.",           // GR-6
		"You should not do this.",               // 3.2
		"The change has been shipped.",          // 3.4
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got := steRules(findings)
	want := []string{
		"ste-8.1-semicolon",
		"ste-4.2-contraction",
		"ste-gr6-latin",
		"ste-3.2-modal",
		"ste-3.4-perfect",
	}
	for _, r := range want {
		if !got[r] {
			t.Errorf("expected rule %q in findings, got %+v", r, findings)
		}
	}
}

// TestSte_FencedAndInlineCodeProduceNoFindings covers the "text inside
// fenced code blocks and inline code spans produces no findings" AC.
func TestSte_FencedAndInlineCodeProduceNoFindings(t *testing.T) {
	body := strings.Join([]string{
		"# Test doc",
		"",
		"Prose that mentions `should` inline — the backticks strip it.",
		"",
		"```",
		"You should not do this; e.g. this line.",
		"The worker can't run and it has been broken.",
		"```",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" {
			t.Errorf("expected no STE finding, got %s", f.String())
		}
	}
}

// TestSte_PossessiveApostropheDoesNotFire covers the "possessive `'s`
// produces no contraction finding" AC. This is the highest-risk false
// positive in the whole check set.
func TestSte_PossessiveApostropheDoesNotFire(t *testing.T) {
	body := strings.Join([]string{
		"# Test doc",
		"",
		"Ben's review of the worker's branch on prism's docs.",
		"The system's owner is Anna's team.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" {
			t.Errorf("expected no STE finding on possessive apostrophes, got %s", f.String())
		}
	}
}

// TestSte_SkipFileDirectiveSuppressesSteFindings covers the "a file
// carrying `doclint-skip-file` produces no STE findings" AC.
func TestSte_SkipFileDirectiveSuppressesSteFindings(t *testing.T) {
	body := strings.Join([]string{
		"<!-- doclint-skip-file: external interface -->",
		"",
		"This has a semicolon; and a modal should.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" {
			t.Errorf("expected no STE finding when doclint-skip-file present, got %s", f.String())
		}
	}
}

// TestSte_IgnoreDirectiveSuppressesNamedToken covers the "`doclint-ignore`
// suppresses the named token" AC.
func TestSte_IgnoreDirectiveSuppressesNamedToken(t *testing.T) {
	body := strings.Join([]string{
		"# Test doc",
		"<!-- doclint-ignore: should -->",
		"",
		"A modal that should not fire.",
		"But a semicolon; still fires.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	sawSemi, sawShould := false, false
	for _, f := range findings {
		if f.Category != "ste" {
			continue
		}
		if f.Rule == "ste-3.2-modal" && f.Token == "should" {
			sawShould = true
		}
		if f.Rule == "ste-8.1-semicolon" {
			sawSemi = true
		}
	}
	if sawShould {
		t.Errorf("expected `should` to be suppressed by doclint-ignore")
	}
	if !sawSemi {
		t.Errorf("expected `;` finding to remain unsuppressed")
	}
}

// TestSte_FindingShape covers the "each finding names the file, line,
// offending text, and a rule identifier" AC.
func TestSte_FindingShape(t *testing.T) {
	body := "# Test\n\nA semicolon; here.\n"
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %+v", findings)
	}
	f := findings[0]
	if f.Category != "ste" {
		t.Errorf("Category: got %q, want ste", f.Category)
	}
	if f.Token != ";" {
		t.Errorf("Token: got %q, want ;", f.Token)
	}
	if f.Rule != "ste-8.1-semicolon" {
		t.Errorf("Rule: got %q", f.Rule)
	}
	if f.Line != 3 {
		t.Errorf("Line: got %d, want 3", f.Line)
	}
	if !strings.HasSuffix(f.File, "doclint.md") {
		t.Errorf("File: got %q", f.File)
	}
	s := f.String()
	for _, want := range []string{"doclint.md", "3:", "rule=ste-8.1-semicolon"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q: %s", want, s)
		}
	}
}

// TestSte_OnlyScansInScopeBasenames covers "the lint scans only the four
// named docs, and produces no findings for others". A doc at
// docs/invariants/other.md must not be STE-scanned even if its basename
// matches (parent-dir guard), and an unrelated basename must not scan.
func TestSte_OnlyScansInScopeBasenames(t *testing.T) {
	root := synthPrismRoot(t)
	_ = os.Remove(filepath.Join(root, "docs", "keep.md"))

	// A doc with an unrelated basename — must not STE-scan.
	otherPath := filepath.Join(root, "docs", "other.md")
	if err := os.WriteFile(otherPath, []byte("A semicolon; and should modal.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A doc whose basename matches, but under a nested subdir — must not
	// STE-scan either.
	nestedPath := filepath.Join(root, "docs", "invariants", "doclint.md")
	if err := os.MkdirAll(filepath.Dir(nestedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedPath, []byte("A semicolon; and should modal.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" {
			t.Errorf("expected no STE finding for out-of-scope docs, got %s", f.String())
		}
	}
}

// TestSte_NoOutOfScopeChecksFire covers the "no findings for sentence
// length, slop words, -ing clauses, passive voice, part-of-speech misuse,
// or synonym rotation" AC. We assert this by feeding text that would
// tickle each of those (out-of-scope) checks and confirming no STE finding
// fires for content free of the five in-scope patterns.
func TestSte_NoOutOfScopeChecksFire(t *testing.T) {
	body := strings.Join([]string{
		"# Test doc",
		"",
		// Long sentence (~40 words) with slop words, -ing clauses,
		// passive voice, synonym rotation. None of these are in the
		// enabled check set.
		"The robust and comprehensive system leverages a plethora of powerful",
		"features, seamlessly enabling users to effortlessly validate their",
		"configuration by verifying the settings and checking that the config",
		"is confirmed to be correct.",
		"",
		// Passive voice, no modal / no semicolon / no perfect / no Latin.
		"The file was written by the worker. The result is stored on disk.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" {
			t.Errorf("expected no STE finding for out-of-scope check content, got %s", f.String())
		}
	}
}

// TestSte_EachCheckIsNotANoOp is the revert-and-watch-fail proof: for
// each of the five checks, we assert that disabling it causes its
// positive test to STOP reporting a finding. That guarantees the check
// contributed the finding, i.e. the regex is not vacuous. AC.
func TestSte_EachCheckIsNotANoOp(t *testing.T) {
	cases := []struct {
		rule  string
		body  string
		token string
	}{
		{"ste-8.1-semicolon", "A semicolon; here.\n", ";"},
		{"ste-4.2-contraction", "It can't work.\n", "can't"},
		{"ste-gr6-latin", "Some things e.g. that.\n", "e.g."},
		{"ste-3.2-modal", "You should try.\n", "should"},
		{"ste-3.4-perfect", "It has been broken.\n", "has been"},
	}
	for _, tc := range cases {
		t.Run(tc.rule, func(t *testing.T) {
			// Positive path: check enabled, finding present.
			prose := steStripToProse([]byte(tc.body))
			fs := runSteChecks(prose, nil)
			found := false
			for _, f := range fs {
				if f.rule == tc.rule && strings.EqualFold(f.text, tc.token) {
					found = true
				}
			}
			if !found {
				t.Fatalf("baseline: expected rule %q to fire on %q, got %+v", tc.rule, tc.body, fs)
			}
			// Revert path: same check disabled, finding absent.
			enabled := map[string]bool{}
			for _, c := range steChecks {
				enabled[c.rule] = true
			}
			enabled[tc.rule] = false
			fs2 := runSteChecks(prose, enabled)
			for _, f := range fs2 {
				if f.rule == tc.rule {
					t.Errorf("disabled %q still produced finding: %+v", tc.rule, f)
				}
			}
		})
	}
}

// TestSte_SandboxRootAbsentDoesNotFail exercises the "runs in the nix
// sandbox where the repo root is absent" AC. Passing repoRoot="" is the
// canonical simulation.
func TestSte_SandboxRootAbsentDoesNotFail(t *testing.T) {
	body := "# Sandbox\n\nClean prose.\n"
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected zero findings, got %+v", findings)
	}
}
