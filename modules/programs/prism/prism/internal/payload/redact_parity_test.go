package payload_test

// Parity guard between the Go redactor and the pi extension's redactor
// (issue #2589).
//
// The two implementations are the two halves of one control: the extension
// redacts before the frame reaches the socket, the Go layer redacts before
// the INSERT. If their registries drift, a credential the extension does not
// know about is caught only by the weaker second control, and a shape the Go
// side does not know about is never caught at all.
//
// The test reads the TypeScript source and compares the four things that must
// agree: the exact env-var names, the name prefixes, the name suffixes, and
// the shape rules (name AND pattern — the pattern strings are deliberately
// written to be byte-identical in both dialects).
//
// The nix build sandbox copies in only modules/programs/prism/prism, so the
// extension source is absent there and the test skips. It runs in the
// `go-tests` CI job and on a developer checkout, which is where drift is
// introduced.

import (
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

func readExtensionSource(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(extensionSourcePath)
	if err != nil {
		t.Skipf("cannot resolve %s: %v", extensionSourcePath, err)
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
