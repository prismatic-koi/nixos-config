package payload_test

// Parity guard between the Go redactor and the pi extension's redactor.
//
// The two implementations are the two halves of one control: the extension
// redacts before the frame reaches the socket, the Go layer redacts before
// the INSERT. If their registries drift, a credential the extension does not
// know about is caught only by the weaker second control, and a shape the Go
// side does not know about is never caught at all.
//
// The test reads the TypeScript source and compares the seven things that
// must agree: the exact env-var names, the name prefixes, the name suffixes,
// the credential file registry, the shape rules of the combined pass (name AND
// pattern — the pattern strings are deliberately written to be
// byte-identical in both dialects), the private-key shape (name, BEGIN
// and END patterns, and triggers), and the context shapes (name, pattern,
// triggers, and fold flag, in order).
//
// The private-key pass is code, not one pattern, so equal strings do not
// prove equal output. testdata/private-key-cases.json holds the output both
// sides must give. This file runs the cases against Go, and prism.test.ts
// runs the same file against the extension. testdata/context-shape-cases.json
// does the same for the context shapes: the two regexp engines must give the
// same match for the same pattern.
//
// The nix build sandbox copies in only modules/programs/prism/prism, so the
// extension source is absent there and the source tests skip. They run in the
// `go-tests` CI job and on a developer checkout, which is where drift is
// introduced.

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/payload"
)

// extensionSourcePath is the pi extension, relative to this package
// directory: internal/payload -> prism -> pi/extensions/prism.ts.
const extensionSourcePath = "../../../pi/extensions/prism.ts"

// extensionTestSourcePath is the test suite of the pi extension.
const extensionTestSourcePath = "../../../pi/extensions/prism.test.ts"

// privateKeyCasesPath is the private-key case file that both suites run.
const privateKeyCasesPath = "testdata/private-key-cases.json"

// contextShapeCasesPath is the context shape case file that both suites run.
const contextShapeCasesPath = "testdata/context-shape-cases.json"

func readExtensionSource(t *testing.T) string {
	t.Helper()
	return readExtensionFile(t, extensionSourcePath)
}

func readExtensionFile(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Skipf("cannot resolve %s: %v", rel, err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("extension source not present at %s (expected inside the nix build sandbox)", abs)
		}
		t.Fatalf("read %s: %v", abs, err)
	}
	return string(b)
}

// stringArrayLiteral extracts the quoted entries of a `const NAME ... = [ … ]`
// array literal from the TypeScript source.
func stringArrayLiteral(t *testing.T, src, name string) []string {
	t.Helper()
	// Match from the declaration to the first closing bracket at the start
	// of a line, which is how prettier formats these arrays.
	re := regexp.MustCompile(`(?s)export const ` + regexp.QuoteMeta(name) + `[^=]*=\s*\[(.*?)\n\]`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not find array literal %s in the extension source", name)
	}
	entryRe := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	var out []string
	for _, e := range entryRe.FindAllStringSubmatch(m[1], -1) {
		unquoted, err := strconv.Unquote(`"` + e[1] + `"`)
		if err != nil {
			t.Fatalf("%s: cannot unquote entry %q: %v", name, e[1], err)
		}
		out = append(out, unquoted)
	}
	if len(out) == 0 {
		t.Fatalf("array literal %s parsed to zero entries", name)
	}
	return out
}

func assertSameSet(t *testing.T, what string, goSide, tsSide []string) {
	t.Helper()
	g := slices.Clone(goSide)
	s := slices.Clone(tsSide)
	slices.Sort(g)
	slices.Sort(s)
	if !slices.Equal(g, s) {
		t.Errorf("%s drifted between Go and the pi extension:\n  Go: %v\n  TS: %v", what, g, s)
	}
}

func TestRedactorParityWithExtension_EnvNameRegistry(t *testing.T) {
	src := readExtensionSource(t)

	assertSameSet(t, "CREDENTIAL_ENV_NAMES",
		payload.CredentialEnvNames(),
		stringArrayLiteral(t, src, "CREDENTIAL_ENV_NAMES"))

	assertSameSet(t, "CREDENTIAL_ENV_PREFIXES",
		payload.CredentialEnvPrefixes(),
		stringArrayLiteral(t, src, "CREDENTIAL_ENV_PREFIXES"))

	assertSameSet(t, "CREDENTIAL_ENV_NAME_SUFFIXES",
		payload.CredentialEnvNameSuffixes(),
		stringArrayLiteral(t, src, "CREDENTIAL_ENV_NAME_SUFFIXES"))
}

// TestRedactorParityWithExtension_CredentialFileRegistry compares the path
// variables whose file the value layer reads, and the marker name of each
// (issue #3472). If one side drops an entry, that side leaves the credential
// in the output.
func TestRedactorParityWithExtension_CredentialFileRegistry(t *testing.T) {
	src := readExtensionSource(t)
	block := regexp.MustCompile(`(?s)export const CREDENTIAL_FILE_ENV_NAMES[^=]*=\s*\{(.*?)\n\}`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("could not find CREDENTIAL_FILE_ENV_NAMES in the extension source")
	}
	ts := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*([A-Z0-9_]+):\s*"([^"]+)",?\s*$`).FindAllStringSubmatch(block[1], -1) {
		ts[m[1]] = m[2]
	}
	if len(ts) == 0 {
		t.Fatal("CREDENTIAL_FILE_ENV_NAMES parsed to zero entries")
	}
	if goSide := payload.CredentialFileEnvNames(); !maps.Equal(goSide, ts) {
		t.Errorf("credential file registry drifted between Go and the pi extension:\n  Go: %v\n  TS: %v", goSide, ts)
	}
}

func TestRedactorParityWithExtension_ShapeRules(t *testing.T) {
	src := readExtensionSource(t)

	// Each shape is `{ name: "…", pattern: String.raw`…` }`, in either the
	// one-line or the prettier-wrapped form.
	block := regexp.MustCompile(`(?s)export const CREDENTIAL_SHAPES[^=]*=\s*\[(.*?)\n\]`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("could not find CREDENTIAL_SHAPES in the extension source")
	}
	ruleRe := regexp.MustCompile(`(?s)name:\s*"([^"]+)",\s*pattern:\s*String\.raw` + "`" + `([^` + "`" + `]*)` + "`" + `,\s*triggers:\s*\[([^\]]*)\]`)
	matches := ruleRe.FindAllStringSubmatch(block[1], -1)
	if len(matches) == 0 {
		t.Fatal("CREDENTIAL_SHAPES parsed to zero rules")
	}

	entryRe := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	tsNames := make([]string, 0, len(matches))
	tsPatterns := make(map[string]string, len(matches))
	tsTriggers := make(map[string][]string, len(matches))
	for _, m := range matches {
		tsNames = append(tsNames, m[1])
		tsPatterns[m[1]] = m[2]
		for _, e := range entryRe.FindAllStringSubmatch(m[3], -1) {
			unquoted, err := strconv.Unquote(`"` + e[1] + `"`)
			if err != nil {
				t.Fatalf("shape %q: cannot unquote trigger %q: %v", m[1], e[1], err)
			}
			tsTriggers[m[1]] = append(tsTriggers[m[1]], unquoted)
		}
	}

	goNames := payload.CredentialShapeNames()

	// Order matters: alternation is leftmost-first in both engines, so the
	// rule order decides which name an overlapping match is attributed to.
	if !slices.Equal(goNames, tsNames) {
		t.Fatalf("shape rule order/name drifted:\n  Go: %v\n  TS: %v", goNames, tsNames)
	}

	goPatterns := payload.CredentialShapePatterns()
	goTriggers := payload.CredentialShapeTriggers()
	for _, name := range goNames {
		if goPatterns[name] != tsPatterns[name] {
			t.Errorf("shape %q pattern drifted:\n  Go: %s\n  TS: %s", name, goPatterns[name], tsPatterns[name])
		}
		// The prefilter is only sound while every trigger is a necessary
		// substring of the pattern. Both sides must use the same set, or
		// one of them silently skips a shape the other applies.
		if !slices.Equal(goTriggers[name], tsTriggers[name]) {
			t.Errorf("shape %q triggers drifted:\n  Go: %v\n  TS: %v", name, goTriggers[name], tsTriggers[name])
		}
	}
}

// TestRedactorParityWithExtension_ContextShapes compares the context shapes
// (issue #3476). If one side drops a rule or a trigger, that side leaves the
// UniFi session token in the output.
func TestRedactorParityWithExtension_ContextShapes(t *testing.T) {
	src := readExtensionSource(t)
	block := regexp.MustCompile(`(?s)export const CONTEXT_SHAPES[^=]*=\s*\[(.*?)\n\]`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("could not find CONTEXT_SHAPES in the extension source")
	}
	ruleRe := regexp.MustCompile(`(?s)name:\s*"([^"]+)",\s*pattern:\s*String\.raw` + "`" + `([^` + "`" + `]*)` + "`" + `,\s*triggers:\s*\[([^\]]*)\],\s*foldTriggers:\s*(true|false)`)
	entryRe := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	var ts []payload.ContextShape
	for _, m := range ruleRe.FindAllStringSubmatch(block[1], -1) {
		rule := payload.ContextShape{Name: m[1], Pattern: m[2], FoldTriggers: m[4] == "true"}
		for _, e := range entryRe.FindAllStringSubmatch(m[3], -1) {
			unquoted, err := strconv.Unquote(`"` + e[1] + `"`)
			if err != nil {
				t.Fatalf("context shape %q: cannot unquote trigger %q: %v", m[1], e[1], err)
			}
			rule.Triggers = append(rule.Triggers, unquoted)
		}
		ts = append(ts, rule)
	}
	if len(ts) == 0 {
		t.Fatal("CONTEXT_SHAPES parsed to zero rules")
	}

	goSide := payload.ContextShapes()
	if len(goSide) != len(ts) {
		t.Fatalf("context shape count drifted: Go %d, TS %d", len(goSide), len(ts))
	}
	for i := range goSide {
		g, s := goSide[i], ts[i]
		if g.Name != s.Name {
			t.Errorf("context shape %d name/order drifted: Go %q, TS %q", i, g.Name, s.Name)
		}
		if g.Pattern != s.Pattern {
			t.Errorf("context shape %q pattern drifted:\n  Go: %s\n  TS: %s", g.Name, g.Pattern, s.Pattern)
		}
		if !slices.Equal(g.Triggers, s.Triggers) || g.FoldTriggers != s.FoldTriggers {
			t.Errorf("context shape %q triggers drifted:\n  Go: %q fold=%v\n  TS: %q fold=%v", g.Name, g.Triggers, g.FoldTriggers, s.Triggers, s.FoldTriggers)
		}
	}
}

func TestRedactorParityWithExtension_PrivateKeyShape(t *testing.T) {
	src := readExtensionSource(t)

	block := regexp.MustCompile(`(?s)export const PRIVATE_KEY_SHAPE[^=]*=\s*\{(.*?)\n\}`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("could not find PRIVATE_KEY_SHAPE in the extension source")
	}
	field := func(re string) string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^\s*` + re).FindStringSubmatch(block[1])
		if m == nil {
			t.Fatalf("PRIVATE_KEY_SHAPE: no field matches %s", re)
		}
		return m[1]
	}
	rawField := func(key string) string {
		t.Helper()
		return field(key + `:\s*String\.raw` + "`" + "([^`]*)" + "`")
	}

	name, begin, end, triggers := payload.PrivateKeyShapeForTest()
	if ts := field(`name:\s*"([^"]+)"`); ts != name {
		t.Errorf("private-key name drifted: Go %q, TS %q", name, ts)
	}
	if ts := rawField("begin"); ts != begin {
		t.Errorf("private-key BEGIN pattern drifted:\n  Go: %s\n  TS: %s", begin, ts)
	}
	if ts := rawField("end"); ts != end {
		t.Errorf("private-key END pattern drifted:\n  Go: %s\n  TS: %s", end, ts)
	}
	var tsTriggers []string
	for _, e := range regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(field(`triggers:\s*\[([^\]]*)\]`), -1) {
		unquoted, err := strconv.Unquote(`"` + e[1] + `"`)
		if err != nil {
			t.Fatalf("private-key trigger %q: cannot unquote: %v", e[1], err)
		}
		tsTriggers = append(tsTriggers, unquoted)
	}
	if !slices.Equal(triggers, tsTriggers) {
		t.Errorf("private-key triggers drifted:\n  Go: %v\n  TS: %v", triggers, tsTriggers)
	}
}

// segment is one part of an input or a want in the private-key case file:
// a string, or {"repeat": s, "count": n} for n copies of s.
type segment string

func (s *segment) UnmarshalJSON(b []byte) error {
	var lit string
	if err := json.Unmarshal(b, &lit); err == nil {
		*s = segment(lit)
		return nil
	}
	var rep struct {
		Repeat string `json:"repeat"`
		Count  int    `json:"count"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		return err
	}
	if rep.Repeat == "" || rep.Count < 1 {
		return fmt.Errorf("repeat segment needs a non-empty repeat and a count of 1 or more: %s", b)
	}
	*s = segment(strings.Repeat(rep.Repeat, rep.Count))
	return nil
}

type redactCase struct {
	Name  string
	Input string
	Want  string
}

func loadPrivateKeyCases(tb testing.TB) []redactCase {
	tb.Helper()
	return loadRedactCases(tb, privateKeyCasesPath)
}

func loadRedactCases(tb testing.TB, path string) []redactCase {
	tb.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read %s: %v", path, err)
	}
	var file struct {
		Cases []struct {
			Name  string    `json:"name"`
			Input []segment `json:"input"`
			Want  []segment `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		tb.Fatalf("parse %s: %v", path, err)
	}
	if len(file.Cases) == 0 {
		tb.Fatalf("%s holds no cases", path)
	}
	join := func(segs []segment) string {
		var sb strings.Builder
		for _, s := range segs {
			sb.WriteString(string(s))
		}
		return sb.String()
	}
	out := make([]redactCase, 0, len(file.Cases))
	for _, c := range file.Cases {
		out = append(out, redactCase{Name: c.Name, Input: join(c.Input), Want: join(c.Want)})
	}
	return out
}

// mismatch describes where got and want first differ, without a dump of an
// input that can be 1 MiB long.
func mismatch(got, want string) string {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	window := func(s string) string {
		return s[max(0, i-40):min(len(s), i+40)]
	}
	return fmt.Sprintf("first difference at byte %d (got %d bytes, want %d bytes):\n  got:  %q\n  want: %q",
		i, len(got), len(want), window(got), window(want))
}

// TestRedactorParityWithExtension_PrivateKeyCases runs the shared case file
// against Go. prism.test.ts runs the same file against the extension, so a
// pass on both sides means the two outputs are identical.
func TestRedactorParityWithExtension_PrivateKeyCases(t *testing.T) {
	r := payload.NewShapeOnlyRedactor()
	for _, tc := range loadPrivateKeyCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			if got := r.Redact(tc.Input); got != tc.Want {
				t.Error(mismatch(got, tc.Want))
			}
		})
	}
}

// TestRedactorParityWithExtension_ContextShapeCases runs the shared context
// shape case file against Go. prism.test.ts runs the same file against the
// extension.
func TestRedactorParityWithExtension_ContextShapeCases(t *testing.T) {
	r := payload.NewShapeOnlyRedactor()
	for _, tc := range loadRedactCases(t, contextShapeCasesPath) {
		t.Run(tc.Name, func(t *testing.T) {
			if got := r.Redact(tc.Input); got != tc.Want {
				t.Error(mismatch(got, tc.Want))
			}
			if got := r.Redact(tc.Want); got != tc.Want {
				t.Errorf("a second pass changed the output: %s", mismatch(got, tc.Want))
			}
		})
	}
}

// TestRedactorParityWithExtension_ExtensionRunsTheCaseFiles is a source
// guard. A case file proves parity only while both suites run it.
func TestRedactorParityWithExtension_ExtensionRunsTheCaseFiles(t *testing.T) {
	src := readExtensionFile(t, extensionTestSourcePath)
	for _, p := range []string{privateKeyCasesPath, contextShapeCasesPath} {
		if want := filepath.Base(p); !strings.Contains(src, want) {
			t.Errorf("prism.test.ts no longer reads %s — the parity check of that file is gone from the extension side", want)
		}
	}
}

func TestRedactorParityWithExtension_Constants(t *testing.T) {
	src := readExtensionSource(t)

	minRe := regexp.MustCompile(`export const REDACTION_MIN_VALUE_LENGTH\s*=\s*(\d+)`)
	m := minRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("could not find REDACTION_MIN_VALUE_LENGTH in the extension source")
	}
	tsMin, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("REDACTION_MIN_VALUE_LENGTH is not a number: %v", err)
	}
	if tsMin != payload.MinCredentialValueLen {
		t.Errorf("minimum value length drifted: Go %d, TS %d", payload.MinCredentialValueLen, tsMin)
	}

	maxRe := regexp.MustCompile(`export const CREDENTIAL_FILE_MAX_BYTES\s*=\s*(\d+)`)
	if mm := maxRe.FindStringSubmatch(src); mm == nil {
		t.Error("could not find CREDENTIAL_FILE_MAX_BYTES in the extension source")
	} else if tsMax, _ := strconv.Atoi(mm[1]); tsMax != payload.MaxCredentialFileSize {
		t.Errorf("credential file size limit drifted: Go %d, TS %d", payload.MaxCredentialFileSize, tsMax)
	}

	for _, tc := range []struct{ constName, goValue string }{
		{"REDACTION_MARKER_PREFIX", payload.RedactionMarkerPrefix},
		{"REDACTION_MARKER_SUFFIX", payload.RedactionMarkerSuffix},
	} {
		re := regexp.MustCompile(`export const ` + tc.constName + `\s*=\s*"((?:[^"\\]|\\.)*)"`)
		mm := re.FindStringSubmatch(src)
		if mm == nil {
			t.Errorf("could not find %s in the extension source", tc.constName)
			continue
		}
		unquoted, uErr := strconv.Unquote(`"` + mm[1] + `"`)
		if uErr != nil {
			t.Errorf("%s: cannot unquote %q: %v", tc.constName, mm[1], uErr)
			continue
		}
		if unquoted != tc.goValue {
			t.Errorf("%s drifted: Go %q, TS %q", tc.constName, tc.goValue, unquoted)
		}
	}
}

// TestRedactorParityWithExtension_ExtensionRedactsBeforeTheSocket is a source
// guard, not a behavioural one: it pins the fact that the extension's single
// outbound choke point applies the redactor. A future edit that removes the
// call fails here even though the Go side still passes its own tests.
func TestRedactorParityWithExtension_ExtensionRedactsBeforeTheSocket(t *testing.T) {
	src := readExtensionSource(t)
	for _, want := range []string{
		"redactor.redactFrame(frame)",
		"defaultSecretRedactor().redact(s)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("extension source no longer contains %q — the pre-socket redaction may have been removed", want)
		}
	}
}
