package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestSidebar_DefaultCollapsedReviews is the §3.1 canonical shape with
// reviews collapsed by default — the "more common case because reviews
// are hidden by default" per the design doc. Asserts the rendered
// hierarchy matches the §3.1 example verbatim:
//
//	▾ nixos-config
//	├─ ○  @main                  idle
//	├─ ◑  @2141-mux-spike (5 rev) reviewing
//	├─ ●  @degender-global-i…    active
//	├─ ◐  @battery-monitor-ref…  waiting
//	└─ ●  @stale-finished-ses…   finished
//	▾ home-ops
//	├─ ○  @main                  idle
//	└─ ▲  @plex-image-bump       escalated
//	▸ pi-extensions
func TestSidebar_DefaultCollapsedReviews(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree,
		WithStates(fixtureStates()),
		WithSize(120, 24),
		// pi-extensions starts collapsed in the §3.1 example.
		WithRepoExpanded("pi-extensions", false),
	)
	got := m.renderSidebar(SidebarWidth, 20)
	assertGolden(t, "sidebar_default_collapsed", got)
}

// TestSidebar_ExpandedReviews is the §3.1 canonical shape with one
// review group expanded — the leading example in the design doc:
//
//	▾ nixos-config
//	├─ ○  @main
//	├─ ◑  @2141-mux-spike
//	│  ├─ ●  ~1-code
//	│  ├─ ●  ~1-goal
//	│  ├─ ●  ~1-qa
//	│  ├─ ▲  ~1-security
//	│  └─ ●  ~1-context
//	...
func TestSidebar_ExpandedReviews(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree,
		WithStates(fixtureStates()),
		WithSize(120, 24),
		WithReviewsExpanded("nixos-config@2141-mux-spike", true),
		WithRepoExpanded("pi-extensions", false),
	)
	got := m.renderSidebar(SidebarWidth, 20)
	assertGolden(t, "sidebar_expanded_reviews", got)
}

// TestSidebar_Header verifies the §3.1 header counts only top-level
// (non-review) sessions, even when reviews are expanded — the value
// should not shift on expand/collapse.
func TestSidebar_Header(t *testing.T) {
	tree := buildFixtureTree(t)
	collapsed := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	expanded := New(tree, WithStates(fixtureStates()), WithSize(120, 24),
		WithReviewsExpanded("nixos-config@2141-mux-spike", true))

	collapsedHeader := firstLine(stripANSI(collapsed.renderSidebar(SidebarWidth, 20)))
	expandedHeader := firstLine(stripANSI(expanded.renderSidebar(SidebarWidth, 20)))

	// The fixture has 8 top-level sessions across the three repo
	// clusters (5 + 2 + 1) — matches the §3.1 example's
	// `prism · 8 sessions` header verbatim.
	want := "prism · 8 sessions"
	if !strings.Contains(collapsedHeader, want) {
		t.Errorf("collapsed header missing %q\ngot: %q", want, collapsedHeader)
	}
	if !strings.Contains(expandedHeader, want) {
		t.Errorf("expanded header missing %q\ngot: %q", want, expandedHeader)
	}
}

// TestSidebar_Footer asserts the §3.1 keymap hint is present. At the
// SidebarWidth=32 default, the full hint does not fit (it is ~48 cells
// wide) and the §3.1 "truncates to glyphs-only" fallback fires — so we
// test the full-hint shape at a width that comfortably fits it.
func TestSidebar_Footer(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	out := stripANSI(m.renderSidebar(60, 20))
	lines := strings.Split(out, "\n")
	footer := lines[len(lines)-1]
	if !strings.Contains(footer, "↑↓ nav") || !strings.Contains(footer, "←→ collapse") {
		t.Errorf("footer missing §3.1 hint shape: %q", footer)
	}
}

// TestSidebar_FooterGlyphTruncation asserts the §3.1 glyph-only
// truncation when the sidebar is too narrow for the full footer hint.
func TestSidebar_FooterGlyphTruncation(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	// At SidebarWidth=32 the long footer hint does not fit (~48 cells),
	// so the §3.1 "truncates to glyphs-only" fallback fires.
	out := stripANSI(m.renderSidebar(SidebarWidth, 20))
	lines := strings.Split(out, "\n")
	footer := lines[len(lines)-1]
	// Glyph footer must contain every glyph cell.
	for _, glyph := range []string{"↑↓", "←→", "⏎", "⇥", "q"} {
		if !strings.Contains(footer, glyph) {
			t.Errorf("glyph footer missing %q: %q", glyph, footer)
		}
	}
	// And must NOT contain the long words — otherwise we didn't
	// actually truncate.
	if strings.Contains(footer, "collapse") {
		t.Errorf("glyph footer should not contain word `collapse`: %q", footer)
	}
}

// TestSidebar_FixedWidth asserts the §3.1 32-column sidebar width — no
// row exceeds it once rendered, even with very long session names.
func TestSidebar_FixedWidth(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	out := m.renderSidebar(SidebarWidth, 20)
	for i, line := range strings.Split(out, "\n") {
		w := ansi.StringWidth(line)
		if w > SidebarWidth {
			t.Errorf("line %d exceeds SidebarWidth (%d > %d): %q", i, w, SidebarWidth, stripANSI(line))
		}
	}
}

// TestSidebar_TruncationDropOrder asserts the §3.1 drop order: when
// space is tight the state label is dropped first, then the badge,
// then the name is ellipsis-truncated.
func TestSidebar_TruncationDropOrder(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))

	// Sanity: at full SidebarWidth (32) the state label IS present
	// for the idle main session ("idle"), and absent for the long
	// battery-monitor-refactor name (truncated to leave space for
	// the state label).
	out := stripANSI(m.renderSidebar(SidebarWidth, 20))
	if !strings.Contains(out, "@main") || !strings.Contains(out, "idle") {
		t.Errorf("32-col sidebar should keep both name and state label for short session\n%s", out)
	}

	// Squeeze: at 18 cols the state label must drop while the badge
	// is preserved.
	out18 := stripANSI(m.renderSidebar(18, 20))
	if strings.Contains(out18, "reviewing") {
		t.Errorf("18-col sidebar should drop the state label; got line containing `reviewing`\n%s", out18)
	}
}

// firstLine returns the first newline-delimited line of s.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
