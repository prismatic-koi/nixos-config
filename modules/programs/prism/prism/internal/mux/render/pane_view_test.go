package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/prismatic-koi/prism/internal/mux/vt"
)

// TestActivePane_NoHostProvider asserts the §3.1 "active pane area"
// renders the placeholder when no HostProvider is wired — the
// pre-#2153 wiring state.
func TestActivePane_NoHostProvider(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	got := stripANSI(m.renderActivePane(80, 20))
	if !strings.Contains(got, "(no PTY") {
		t.Errorf("expected `(no PTY ...)` placeholder, got:\n%s", got)
	}
}

// TestActivePane_HostRenderedContent asserts that when a HostProvider
// returns a real vt.Host that has been fed PTY output, the rendered
// pane area shows the host's content. This is the load-bearing
// integration between #2150 (vt) and the renderer.
func TestActivePane_HostRenderedContent(t *testing.T) {
	tree := buildFixtureTree(t)

	// Build a 40-col, 5-row vt.Host and feed it a short string.
	host := vt.New(40, 5)
	const banner = "hello from PTY"
	if _, err := host.Feed([]byte(banner)); err != nil {
		t.Fatalf("Feed: %v", err)
	}

	// Wire the HostProvider so the selected session's first pane
	// resolves to our host.
	id := "nixos-config@main"
	hosts := HostFunc(func(sessionID, paneName string) *vt.Host {
		if sessionID == id && paneName == "agent" {
			return host
		}
		return nil
	})
	m := New(tree, WithStates(fixtureStates()), WithHosts(hosts), WithSize(120, 24))
	// Walk to @main (row 1; row 0 is the repo header).
	m = applyKeys(t, m, "down")
	if got := m.SelectedSessionID(); got != id {
		t.Fatalf("setup: SelectedSessionID = %q, want %q", got, id)
	}

	got := stripANSI(m.renderActivePane(60, 10))
	if !strings.Contains(got, banner) {
		t.Errorf("active pane should show PTY content %q; got:\n%s", banner, got)
	}
}

// TestActivePane_HostFrameClippedToBox asserts that the active pane
// area produces exactly (width, height) cells regardless of the
// host's own dimensions — the layout invariant lipgloss.JoinHorizontal
// relies on.
func TestActivePane_HostFrameClippedToBox(t *testing.T) {
	tree := buildFixtureTree(t)
	host := vt.New(80, 20)
	// Feed a long string spanning multiple rows.
	_, _ = host.Feed([]byte(strings.Repeat("ab", 200)))
	hosts := HostFunc(func(sessionID, paneName string) *vt.Host { return host })
	m := New(tree, WithStates(fixtureStates()), WithHosts(hosts), WithSize(120, 24))
	m = applyKeys(t, m, "down")

	const width, height = 40, 8
	got := m.renderActivePane(width, height)
	lines := strings.Split(got, "\n")
	if len(lines) != height {
		t.Errorf("renderActivePane height = %d, want %d", len(lines), height)
	}
	for i, line := range lines {
		w := ansi.StringWidth(line)
		if w != width {
			t.Errorf("line %d width = %d, want %d (line: %q)", i, w, width, stripANSI(line))
		}
	}
}
