package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// updateGolden lets a developer regenerate the testdata/*.golden files
// when an intentional rendering change ships. Run with
// `go test ./internal/mux/render -run TestGolden -update` to refresh.
var updateGolden = flag.Bool("update", false, "regenerate testdata/*.golden")

// TestMain forces lipgloss to render at TrueColor unconditionally so
// the golden tests do not depend on the host terminal's colour
// detection. Without this, lipgloss disables colour when stdout is not
// a TTY (which it isn't under `go test`), and the goldens would have
// to live as plain text — losing the ability to assert on §3.1's hex
// values.
//
// The goldens themselves are ANSI-stripped via stripANSI before being
// compared, so the colour profile choice does not bleed into the
// fixture content; it only matters for the targeted style-assertion
// tests in style_test.go.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// stripANSI removes every ANSI escape from s. Used by the golden tests
// to keep the on-disk fixture readable — the colours are asserted
// separately in style_test.go.
func stripANSI(s string) string {
	return ansi.Strip(s)
}

// trimTrailingSpaces strips trailing whitespace from each line of s.
// Golden files store the structural shape of the render output; the
// renderer pads to fixed width with spaces, but trailing spaces are
// invisible noise in an editor view of the fixture. Stripping them
// keeps the goldens diff-friendly without losing the structural
// signal.
func trimTrailingSpaces(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}

// assertGolden checks that got matches the contents of
// testdata/<name>.golden, or rewrites the golden file when -update is
// passed. Both sides are ANSI-stripped and trailing-space-trimmed
// before the compare so the fixture stays human-readable.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	normalised := trimTrailingSpaces(stripANSI(got))

	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(normalised), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run with -update to create)", path, err)
	}
	wantS := string(want)
	if normalised != wantS {
		t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s",
			name, normalised, wantS)
	}
}
