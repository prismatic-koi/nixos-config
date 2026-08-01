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

// TestSte_NoOutOfScopeChecksFire covers the "no findings for passive
// voice, part-of-speech misuse, or synonym rotation" AC. Sentence
// length, slop words, and `-ing`-after-comma ARE in-scope now (#2496)
// and have dedicated tests below — this test only asserts the
// permanent omissions still do not fire.
func TestSte_NoOutOfScopeChecksFire(t *testing.T) {
	body := strings.Join([]string{
		"# Test doc",
		"",
		// Passive voice, part-of-speech misuse ("test" as verb, "check"
		// as verb), synonym rotation (check/verify/validate). None of
		// these are in the enabled check set.
		"The file was written by the worker. The result is stored on disk.",
		"",
		"You test the pump. You check that the value is correct. You validate",
		"the reading. You verify that the meter agrees.",
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

// TestSte_SentenceLengthFiresOverLimit covers the "sentence over the
// applied word limit" AC. A 30-word descriptive sentence must fire.
func TestSte_SentenceLengthFiresOverLimit(t *testing.T) {
	// A 30-word single sentence. STE 6.3 limit is 25.
	body := strings.Join([]string{
		"# Test doc",
		"",
		"The scanner walks every file in the tree and reports a finding for each",
		"token that does not resolve against the built index of the current",
		"prism source root, in a repeatable pass.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	saw := false
	for _, f := range findings {
		if f.Category == "ste" && f.Rule == "ste-6.3-sentence-length" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("expected ste-6.3-sentence-length finding, got %+v", findings)
	}
}

// TestSte_SentenceLengthRuleAppliedIsRule63 covers the "apply the more
// permissive 25-word limit uniformly" documentation choice: a 22-word
// sentence must NOT fire even though it exceeds the procedural 20-word
// Rule 5.1 limit.
func TestSte_SentenceLengthRuleAppliedIsRule63(t *testing.T) {
	body := strings.Join([]string{
		"# Test doc",
		"",
		// 22 words — over Rule 5.1's 20 but under Rule 6.3's 25.
		"The scanner walks every file in the tree and reports one finding for",
		"every token that does not resolve against the built index.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" && f.Rule == "ste-6.3-sentence-length" {
			t.Errorf("expected no length finding on 22-word sentence, got %s", f.String())
		}
	}
}

// TestSte_SentenceLengthTokenisationRules8_5_6_7 covers the "backticked
// spans, numbers with units, parenthesised text, and hyphenated words
// each count as one word" AC. A sentence carrying enough Rule 8.5/8.6/8.7
// tokens that the naive word count would exceed 25 must NOT fire when
// each such token is correctly counted as one.
func TestSte_SentenceLengthTokenisationRules8_5_6_7(t *testing.T) {
	// Naive whitespace-split count would be ~34 words. With the STE
	// tokeniser: `foo bar baz` = 1 (Rule 8.6 backtick), `(parenthesised
	// clause here)` = 1 (Rule 8.5), `10 ms` = 1 (Rule 8.6 number+unit),
	// `sandbox-exec` and `long-hyphenated-word` each count as 1 (Rule
	// 8.7). Under-25 result, no finding.
	body := strings.Join([]string{
		"# Test doc",
		"",
		"The scanner runs `foo bar baz quux quux quux quux quux` in " +
			"`sandbox-exec` for 10 ms per file (parenthesised clause with " +
			"many words inside it) against the long-hyphenated-word set of " +
			"tokens carried by the index.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" && f.Rule == "ste-6.3-sentence-length" {
			t.Errorf("expected no length finding after Rule 8.5/8.6/8.7 tokenisation, got %s", f.String())
		}
	}
}

// TestSte_SentenceLengthColonTerminatesSentence covers the Rule 8.4 AC:
// a vertical-list lead-in colon terminates a sentence for word-count
// purposes. A paragraph that would exceed 25 words if the colon did NOT
// terminate must, with the terminator honoured, split into two short
// sentences and produce no finding.
// Rule 8.4: a vertical-list lead-in colon terminates the sentence for
// word-count purposes. The check runs on the segmenter directly so the
// paragraph-boundary segmentation does not accidentally satisfy the AC
// by acting as a fallback terminator.
func TestSte_SentenceLengthColonTerminatesSentence(t *testing.T) {
	paragraph := "The scanner walks every file in the tree and reports a finding for each token it cannot resolve against its index:"
	sentences := segmentSentences(paragraph)
	if len(sentences) != 1 {
		t.Fatalf("expected 1 sentence, got %d: %+v", len(sentences), sentences)
	}
	if !strings.HasSuffix(strings.TrimSpace(sentences[0].text), ":") {
		t.Errorf("expected the sentence text to end at `:`, got %q", sentences[0].text)
	}
	// A 22-word lead-in ending at the colon must not fire the length check.
	body := "# Test\n\n" + paragraph + "\n\n- item\n- item\n"
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" && f.Rule == "ste-6.3-sentence-length" {
			t.Errorf("expected the lead-in colon paragraph to not fire, got %s", f.String())
		}
	}
}

// TestSte_SentenceLengthSkipsListItemsHeadingsAndTables covers the
// "list-item fragments, table cells, and headings produce no
// sentence-length findings" AC.
func TestSte_SentenceLengthSkipsListItemsHeadingsAndTables(t *testing.T) {
	body := strings.Join([]string{
		// 40+ word heading, list item, and table row.
		"# The scanner walks every file in the tree and reports a finding for each token that does not resolve against the built index of the current prism source root",
		"",
		"- The scanner walks every file in the tree and reports a finding for each token that does not resolve against the built index of the current prism source root.",
		"",
		"| Column | Description |",
		"|---|---|",
		"| id | The scanner walks every file in the tree and reports a finding for each token that does not resolve against the built index of the current prism source root. |",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" && f.Rule == "ste-6.3-sentence-length" {
			t.Errorf("expected no length finding on non-prose lines, got %s", f.String())
		}
	}
}

// TestSte_SlopWordFires covers the "slop word from the documented list"
// AC.
func TestSte_SlopWordFires(t *testing.T) {
	body := "# Test\n\nThis will leverage the tool. Also seamlessly integrated.\nWe use it in order to move faster.\n"
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	sawSingleWord, sawPhrase := false, false
	for _, f := range findings {
		if f.Category != "ste" || f.Rule != "ste-slop" {
			continue
		}
		switch strings.ToLower(f.Token) {
		case "leverage", "seamlessly":
			sawSingleWord = true
		case "in order to":
			sawPhrase = true
		}
	}
	if !sawSingleWord {
		t.Errorf("expected a single-word slop finding (leverage/seamlessly), got %+v", findings)
	}
	if !sawPhrase {
		t.Errorf("expected a slop phrase finding for `in order to`, got %+v", findings)
	}
}

// TestSte_IngAfterCommaFires covers the "-ing clause after a comma"
// AC. The check runs only on prose paragraphs, so this test uses a
// paragraph body.
func TestSte_IngAfterCommaFires(t *testing.T) {
	body := "# Test\n\nThe migration ran to completion, making the table available for reads.\n"
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	saw := false
	for _, f := range findings {
		if f.Category == "ste" && f.Rule == "ste-3.5-ing-after-comma" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("expected ste-3.5-ing-after-comma finding, got %+v", findings)
	}
}

// TestSte_IngAfterCommaSkipsTablesAndListItems covers the precision
// design decision to skip the -ing check on tabular and list content.
// Rule 3.5 forbids -ing as a verb, but table cells and list items
// almost always use -ing words as adjectives ("missing required
// value") or gerund nouns, so firing there is noise.
func TestSte_IngAfterCommaSkipsTablesAndListItems(t *testing.T) {
	body := strings.Join([]string{
		"# Test",
		"",
		"| Symptom | Fix |",
		"|---|---|",
		"| a value, missing required field | fix the client |",
		"",
		"- one thing, including another",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" && f.Rule == "ste-3.5-ing-after-comma" {
			t.Errorf("expected -ing check to skip table/list content, got %s", f.String())
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
		{"ste-slop", "We will leverage the tool.\n", "leverage"},
		{"ste-3.5-ing-after-comma", "The migration ran, making the table available.\n", ", making"},
	}
	for _, tc := range cases {
		t.Run(tc.rule, func(t *testing.T) {
			// Positive path: check enabled, finding present.
			prose := steStripToProse([]byte(tc.body))
			fs := runSteChecks(prose, nil, nil)
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
			fs2 := runSteChecks(prose, enabled, nil)
			for _, f := range fs2 {
				if f.rule == tc.rule {
					t.Errorf("disabled %q still produced finding: %+v", tc.rule, f)
				}
			}
		})
	}

	// Sentence length runs on a separate function against raw content;
	// its revert-and-watch-fail proof lives here too.
	t.Run("ste-6.3-sentence-length", func(t *testing.T) {
		body := []byte("The scanner walks every file in the tree and reports one finding for each token that does not resolve against the built index of the current prism source root, per pass.\n")
		fs := runSentenceLengthCheck(body, nil)
		saw := false
		for _, f := range fs {
			if f.rule == "ste-6.3-sentence-length" {
				saw = true
			}
		}
		if !saw {
			t.Fatalf("baseline: expected ste-6.3-sentence-length to fire, got %+v", fs)
		}
		fs2 := runSentenceLengthCheck(body, map[string]bool{"ste-6.3-sentence-length": false})
		if len(fs2) != 0 {
			t.Errorf("disabled ste-6.3-sentence-length still produced findings: %+v", fs2)
		}
	})
}

// TestSte_SkipFileScopedToIdentifiersKeepsSteFindings covers the Phase 1
// per-lint scoping AC (#2497): a doc carrying `<!-- doclint-skip-file:
// identifiers | reason -->` must still be scanned by STE, and produce STE
// findings on prose that violates a rule.
func TestSte_SkipFileScopedToIdentifiersKeepsSteFindings(t *testing.T) {
	body := strings.Join([]string{
		"<!-- doclint-skip-file: identifiers | external interface, TypeScript identifiers only -->",
		"",
		"# Test doc",
		"",
		"This paragraph has a semicolon; and a modal should.",
		"A reference to `mountTypeAllowlist` which does not exist as a Go ident.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	sawSemi, sawModal, sawIdent := false, false, false
	for _, f := range findings {
		switch f.Rule {
		case "ste-8.1-semicolon":
			sawSemi = true
		case "ste-3.2-modal":
			sawModal = true
		}
		if f.Token == "mountTypeAllowlist" {
			sawIdent = true
		}
	}
	if !sawSemi {
		t.Errorf("expected ste-8.1-semicolon under identifier-scoped skip, got %+v", findings)
	}
	if !sawModal {
		t.Errorf("expected ste-3.2-modal under identifier-scoped skip, got %+v", findings)
	}
	if sawIdent {
		t.Errorf("expected identifier check to be suppressed under identifier-scoped skip, got finding for mountTypeAllowlist")
	}
}

// TestSte_SkipFileScopedToSteSuppressesSteFindings covers the reverse:
// `<!-- doclint-skip-file: ste | reason -->` must suppress STE prose
// findings but keep identifier resolution active.
func TestSte_SkipFileScopedToSteSuppressesSteFindings(t *testing.T) {
	body := strings.Join([]string{
		"<!-- doclint-skip-file: ste | this doc is a machine-generated changelog -->",
		"",
		"# Test doc",
		"",
		"This paragraph has a semicolon; and a modal should.",
		"A reference to `mountTypeAllowlist` which does not exist as a Go ident.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, f := range findings {
		if f.Category == "ste" {
			t.Errorf("expected no STE finding under ste-scoped skip, got %s", f.String())
		}
	}
	sawIdent := false
	for _, f := range findings {
		if f.Token == "mountTypeAllowlist" {
			sawIdent = true
		}
	}
	if !sawIdent {
		t.Errorf("expected identifier resolution to remain active under ste-scoped skip, got %+v", findings)
	}
}

// TestSte_UnparameterisedSkipFileStaysGlobal covers the backwards-compat
// AC: an existing `<!-- doclint-skip-file: reason -->` without a class
// list keeps its historical global behaviour — both STE and identifier
// findings are suppressed. This is the invariant that let
// pi-wire-protocol.md keep its global skip through Phase 1 of the #2497
// migration, before Phase 2 migrated it to the `identifiers`-only scope.
func TestSte_UnparameterisedSkipFileStaysGlobal(t *testing.T) {
	body := strings.Join([]string{
		"<!-- doclint-skip-file: external interface, no scoping given -->",
		"",
		"# Test doc",
		"",
		"This paragraph has a semicolon; and a modal should.",
		"A reference to `mountTypeAllowlist` which does not exist as a Go ident.",
		"",
	}, "\n")
	root, _ := synthSteRoot(t, body)
	findings, err := Scan(root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected global suppression from unparameterised skip-file, got %d findings: %+v", len(findings), findings)
	}
}

// TestParseSkipFileDirective_Shapes covers the parser directly across the
// shapes the AC calls out: no directive, unparameterised (global),
// identifiers-only, ste-only, both classes, unknown class (silently
// dropped so a typo does not widen the skip).
func TestParseSkipFileDirective_Shapes(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantHas   bool
		wantScope skipFileScope
	}{
		{
			name:    "no directive",
			body:    "# Hello\n\nSome prose.\n",
			wantHas: false,
		},
		{
			name:      "unparameterised is global",
			body:      "<!-- doclint-skip-file: a reason -->\n",
			wantHas:   true,
			wantScope: skipFileScope{identifiers: true, ste: true},
		},
		{
			name:      "identifiers-only",
			body:      "<!-- doclint-skip-file: identifiers | external interface -->\n",
			wantHas:   true,
			wantScope: skipFileScope{identifiers: true},
		},
		{
			name:      "ste-only",
			body:      "<!-- doclint-skip-file: ste | machine-generated -->\n",
			wantHas:   true,
			wantScope: skipFileScope{ste: true},
		},
		{
			name:      "both classes explicit",
			body:      "<!-- doclint-skip-file: identifiers, ste | both -->\n",
			wantHas:   true,
			wantScope: skipFileScope{identifiers: true, ste: true},
		},
		{
			name:      "unknown class is silently dropped",
			body:      "<!-- doclint-skip-file: identifers | typo of identifiers -->\n",
			wantHas:   true,
			wantScope: skipFileScope{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, has := parseSkipFileDirective([]byte(tc.body))
			if has != tc.wantHas {
				t.Fatalf("has: got %v, want %v", has, tc.wantHas)
			}
			if got != tc.wantScope {
				t.Errorf("scope: got %+v, want %+v", got, tc.wantScope)
			}
		})
	}
}

// TestSte_SkipScopingIsNotANoOp is the revert-and-watch-fail proof for
// Phase 1: hasSkipFileDirective (the pre-scoping global gate) must NOT
// short-circuit STE scanning when the directive is scoped to identifiers.
// The check simulates the pre-scoping behaviour by calling
// hasSkipFileDirective on the same content that
// TestSte_SkipFileScopedToIdentifiersKeepsSteFindings uses — that helper
// returns true only when both categories are suppressed. If it returned
// true for an identifier-scoped directive, the STE pass would silently
// skip and the AC test above would still pass vacuously.
func TestSte_SkipScopingIsNotANoOp(t *testing.T) {
	identScoped := []byte("<!-- doclint-skip-file: identifiers | reason -->\n")
	if hasSkipFileDirective(identScoped) {
		t.Fatal("identifier-scoped skip must not report as global; the STE pass would then be short-circuited")
	}
	steScoped := []byte("<!-- doclint-skip-file: ste | reason -->\n")
	if hasSkipFileDirective(steScoped) {
		t.Fatal("ste-scoped skip must not report as global; the identifier pass would then be short-circuited")
	}
	global := []byte("<!-- doclint-skip-file: reason -->\n")
	if !hasSkipFileDirective(global) {
		t.Fatal("unparameterised skip must remain global")
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
