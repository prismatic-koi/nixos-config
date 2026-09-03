package usage

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The constants in refresh.go are a hand-maintained mirror of model-config.ts.
// PR #2919 bumped the TypeScript and left the Go behind, so `prism account
// usage` would have presented a different CLI version and a different beta set
// than every pi session on the same account.
//
// These tests read the TypeScript and compare. They are the mechanical half of
// the "change this file in the same commit" instruction at the top of
// refresh.go.
//
// The nix build copies only the Go module (`src = ../modules/programs/prism/
// prism` in pkgs/prism.nix), so the TypeScript is absent there and these skip.
// The `go-tests` CI job runs against a full checkout and does compare.

const modelConfigTS = "../../../pi/extensions/anthropic-oauth/model-config.ts"

func readModelConfigTS(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(modelConfigTS)
	if err != nil {
		t.Fatalf("resolving %s: %v", modelConfigTS, err)
	}
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("model-config.ts not present at %s (expected in the nix sandbox)", path)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}

// quotedStrings pulls every double-quoted literal out of a captured block.
func quotedStrings(block string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(block, -1) {
		out = append(out, m[1])
	}
	return out
}

// captureOrFail keeps the tests below non-vacuous: if the TypeScript is
// reshaped so a pattern stops matching, that is a failure, not a silent pass
// with nothing compared.
func captureOrFail(t *testing.T, src, pattern, what string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not find %s in model-config.ts — the mirror check is "+
			"not comparing anything; fix this pattern: %s", what, pattern)
	}
	return m[1]
}

func TestMirror_CCVersionMatchesModelConfigTS(t *testing.T) {
	src := readModelConfigTS(t)
	want := captureOrFail(t, src, `ccVersion:\s*"([^"]+)"`, "ccVersion")
	if ccVersion != want {
		t.Errorf("ccVersion = %q, model-config.ts has %q — update refresh.go in "+
			"the same commit as the extension", ccVersion, want)
	}
}

func TestMirror_BaseBetasMatchModelConfigTS(t *testing.T) {
	src := readModelConfigTS(t)
	want := quotedStrings(captureOrFail(t, src, `baseBetas:\s*\[([^\]]*)\]`, "baseBetas"))
	if len(want) == 0 {
		t.Fatal("parsed an empty baseBetas from model-config.ts")
	}
	if len(baseBetas) != len(want) {
		t.Fatalf("baseBetas = %v (%d entries), model-config.ts has %v (%d entries)",
			baseBetas, len(baseBetas), want, len(want))
	}
	// Order is part of the wire form: the header is a comma-joined list.
	for i := range want {
		if baseBetas[i] != want[i] {
			t.Errorf("baseBetas[%d] = %q, model-config.ts has %q", i, baseBetas[i], want[i])
		}
	}
}

func TestMirror_HaikuExcludeMatchesModelConfigTS(t *testing.T) {
	src := readModelConfigTS(t)
	want := quotedStrings(captureOrFail(t, src,
		`haiku:\s*\{\s*exclude:\s*\[([^\]]*)\]`, "the haiku exclude list"))
	if len(want) == 0 {
		t.Fatal("parsed an empty haiku exclude list from model-config.ts")
	}
	if len(haikuExcludedBetas) != len(want) {
		t.Fatalf("haikuExcludedBetas = %v, model-config.ts has %v", haikuExcludedBetas, want)
	}
	for i := range want {
		if haikuExcludedBetas[i] != want[i] {
			t.Errorf("haikuExcludedBetas[%d] = %q, model-config.ts has %q",
				i, haikuExcludedBetas[i], want[i])
		}
	}
}
