package render

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// keyMsg builds a tea.KeyMsg from a string the same way bubbletea
// renders it via KeyMsg.String() — keeps the tests honest about the
// key-name canonicalisation.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "ctrl+b":
		return tea.KeyMsg{Type: tea.KeyCtrlB}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	// Single-character keys (q, k, j, h, l).
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// applyKeys sequentially feeds key strings to m.Update and returns the
// resulting *Model (typecast back from tea.Model). Test scaffolding —
// keeps the per-test setup linear.
func applyKeys(t *testing.T, m *Model, keys ...string) *Model {
	t.Helper()
	var model tea.Model = m
	for _, k := range keys {
		nextModel, _ := model.Update(keyMsg(k))
		model = nextModel
	}
	out, ok := model.(*Model)
	if !ok {
		t.Fatalf("Update returned non-*Model: %T", model)
	}
	return out
}

// TestKeymap_NavUpDown asserts the §3.1 `↑` / `↓` (and `k` / `j`)
// move the cursor by exactly one row each, clamped at the ends.
func TestKeymap_NavUpDown(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))

	// Default cursor is 0; up should be a no-op.
	m = applyKeys(t, m, "up")
	if got := m.CursorRow(); got != 0 {
		t.Errorf("up at top: cursor = %d, want 0", got)
	}

	// Down by 3.
	m = applyKeys(t, m, "down", "down", "down")
	if got := m.CursorRow(); got != 3 {
		t.Errorf("down x3: cursor = %d, want 3", got)
	}

	// `k` / `j` aliases.
	m = applyKeys(t, m, "k", "j")
	if got := m.CursorRow(); got != 3 {
		t.Errorf("k then j: cursor = %d, want 3", got)
	}
}

// TestKeymap_CollapseExpand asserts §3.1's `←` / `→` collapse-expand
// semantics on a repo header.
func TestKeymap_CollapseExpand(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	// Cursor 0 is the nixos-config repo header.
	if !m.repoExpanded("nixos-config") {
		t.Fatal("nixos-config should default to expanded")
	}
	// Left collapses the repo.
	m = applyKeys(t, m, "left")
	if m.repoExpanded("nixos-config") {
		t.Errorf("left on expanded repo should collapse it")
	}
	// Right re-expands it.
	m = applyKeys(t, m, "right")
	if !m.repoExpanded("nixos-config") {
		t.Errorf("right on collapsed repo should expand it")
	}
	// Another right steps into the first child session.
	m = applyKeys(t, m, "right")
	if m.CursorRow() != 1 {
		t.Errorf("right on expanded repo should walk into first child; cursor = %d", m.CursorRow())
	}
}

// TestKeymap_ReviewsExpandedDefaultCollapsed verifies §3.1's "reviews
// collapsed by default" and the `→` semantics on a session row.
func TestKeymap_ReviewsExpandedDefaultCollapsed(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))

	// Walk down to @2141-mux-spike — row 2 (header, @main, @2141).
	m = applyKeys(t, m, "down", "down")
	row, ok := m.selectedRow()
	if !ok || row.SessionID != "nixos-config@2141-mux-spike" {
		t.Fatalf("expected to land on @2141-mux-spike, got %#v", row)
	}
	// Reviews should be collapsed by default.
	if m.reviewsExpanded(row.SessionID) {
		t.Fatal("reviews should default to collapsed (§3.1)")
	}
	// Right on a collapsed-review session expands them.
	m = applyKeys(t, m, "right")
	if !m.reviewsExpanded(row.SessionID) {
		t.Errorf("right on collapsed reviews should expand them")
	}
	// Left collapses again.
	m = applyKeys(t, m, "left")
	if m.reviewsExpanded(row.SessionID) {
		t.Errorf("left on expanded reviews should collapse them")
	}
}

// TestKeymap_Quit verifies `q` and `Ctrl-C` both emit tea.Quit per
// §3.1.
func TestKeymap_Quit(t *testing.T) {
	tree := buildFixtureTree(t)
	for _, k := range []string{"q", "ctrl+c"} {
		m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
		_, cmd := m.Update(keyMsg(k))
		if cmd == nil {
			t.Errorf("%s: expected tea.Quit command, got nil", k)
			continue
		}
		// The only way to know it's tea.Quit without reflection is to
		// invoke it and compare against tea.QuitMsg.
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Errorf("%s: expected tea.QuitMsg, got %T", k, msg)
		}
	}
}

// TestKeymap_TabCyclesPane asserts §3.1's `Tab` / `Shift-Tab` cycle
// the selected session's active pane (the inner ring).
func TestKeymap_TabCyclesPane(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	// Walk down to a top-level session.
	m = applyKeys(t, m, "down")
	id := m.SelectedSessionID()
	if id == "" {
		t.Fatal("expected a session selection after one down")
	}
	sess, _ := tree.Session(id)
	initial := sess.ActivePane
	m = applyKeys(t, m, "tab")
	sess, _ = tree.Session(id)
	if sess.ActivePane == initial {
		t.Errorf("Tab should advance the active pane; stayed at %q", initial)
	}
	m = applyKeys(t, m, "shift+tab")
	sess, _ = tree.Session(id)
	if sess.ActivePane != initial {
		t.Errorf("Shift-Tab after Tab should restore initial pane %q; got %q", initial, sess.ActivePane)
	}
}

// TestKeymap_EnterActivatesSession asserts §3.1's `Enter` commits the
// selection to the tree's ActiveSession pointer.
func TestKeymap_EnterActivatesSession(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	m = applyKeys(t, m, "down", "enter")
	got := tree.ActiveSessionID()
	want := m.SelectedSessionID()
	if got != want {
		t.Errorf("Enter should activate selected session: tree.ActiveSessionID()=%q, selected=%q", got, want)
	}
}

// TestPopover_NarrowOnly asserts §3.1's `Ctrl-B` toggles the popover
// only in narrow mode. Wide mode is a no-op.
func TestPopover_NarrowOnly(t *testing.T) {
	tree := buildFixtureTree(t)
	wide := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	wide = applyKeys(t, wide, "ctrl+b")
	if wide.PopoverOpen() {
		t.Errorf("Ctrl-B in wide mode should be a no-op")
	}

	narrow := New(tree, WithStates(fixtureStates()), WithSize(60, 24))
	narrow = applyKeys(t, narrow, "ctrl+b")
	if !narrow.PopoverOpen() {
		t.Errorf("Ctrl-B in narrow mode should open the popover")
	}
	narrow = applyKeys(t, narrow, "ctrl+b")
	if narrow.PopoverOpen() {
		t.Errorf("second Ctrl-B should close the popover")
	}
}

// TestPopover_EscDismisses asserts §3.1's `Esc` dismisses the popover
// without making a selection.
func TestPopover_EscDismisses(t *testing.T) {
	tree := buildFixtureTree(t)
	narrow := New(tree, WithStates(fixtureStates()), WithSize(60, 24))
	narrow = applyKeys(t, narrow, "ctrl+b")
	if !narrow.PopoverOpen() {
		t.Fatal("setup: popover should be open after Ctrl-B")
	}
	preActive := tree.ActiveSessionID()
	narrow = applyKeys(t, narrow, "esc")
	if narrow.PopoverOpen() {
		t.Errorf("Esc should dismiss the popover")
	}
	if tree.ActiveSessionID() != preActive {
		t.Errorf("Esc should not change ActiveSession")
	}
}

// TestPopover_EnterSelectsAndDismisses asserts §3.1's "Enter selects
// and dismisses the popover" in narrow mode.
func TestPopover_EnterSelectsAndDismisses(t *testing.T) {
	tree := buildFixtureTree(t)
	narrow := New(tree, WithStates(fixtureStates()), WithSize(60, 24))
	narrow = applyKeys(t, narrow, "ctrl+b", "down", "enter")
	if narrow.PopoverOpen() {
		t.Errorf("Enter should dismiss the popover in narrow mode")
	}
	if tree.ActiveSessionID() == "" {
		t.Errorf("Enter should activate the selected session")
	}
}

// TestPopover_ClosesOnResize asserts the popover closes if the
// terminal grows back into wide territory mid-session — leaving it
// open would be confusing once the inline sidebar is visible.
func TestPopover_ClosesOnResize(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(60, 24))
	m = applyKeys(t, m, "ctrl+b")
	if !m.PopoverOpen() {
		t.Fatal("setup: popover should be open")
	}
	model, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	m = model.(*Model)
	if m.PopoverOpen() {
		t.Errorf("resize to wide mode should close the popover")
	}
}

// TestView_NarrowShape asserts the §3.1 narrow-mode layout: 1-row
// topbar + full-width pane, no inline sidebar.
func TestView_NarrowShape(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(60, 12))
	view := stripANSI(m.View())
	lines := strings.Split(view, "\n")
	// First line is the topbar — should contain a session identity
	// and the `^B switch` hint.
	top := lines[0]
	if !strings.Contains(top, "^B switch") {
		t.Errorf("narrow topbar missing `^B switch`: %q", top)
	}
	// Header (`prism · N sessions`) MUST NOT appear in narrow mode —
	// that's the sidebar's chrome, hidden in mobile shape.
	if strings.Contains(view, "prism · ") {
		t.Errorf("narrow mode should not render the sidebar header")
	}
}

// TestView_NarrowPopoverShape asserts the §3.1 narrow-mode popover
// renders the sidebar's standard header + footer (it uses the same
// render path).
func TestView_NarrowPopoverShape(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(60, 20))
	m = applyKeys(t, m, "ctrl+b")
	view := stripANSI(m.View())
	if !strings.Contains(view, "prism · ") {
		t.Errorf("narrow popover should render the sidebar header (`prism · N sessions`)")
	}
	if !strings.Contains(view, "esc close") {
		t.Errorf("narrow topbar should advertise `esc close` while popover is open")
	}
}

// TestView_WideShape asserts the §3.1 wide-mode layout: 32-col sidebar
// + active pane to the right. Pane area shows the (no PTY) placeholder
// because no HostProvider is wired.
func TestView_WideShape(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	view := stripANSI(m.View())
	// First row should contain the sidebar header AND start of the
	// pane area on the same row (lipgloss.JoinHorizontal).
	first := strings.Split(view, "\n")[0]
	if !strings.Contains(first, "prism · ") {
		t.Errorf("wide-mode first row should contain sidebar header: %q", first)
	}
}

// TestView_UninitialisedReportsInitialising asserts the View renders a
// safe placeholder before the first tea.WindowSizeMsg arrives — the
// canonical bubbletea startup race.
func TestView_UninitialisedReportsInitialising(t *testing.T) {
	tree := pane.New()
	m := New(tree)
	if got := m.View(); got != "initialising…" {
		t.Errorf("View before WindowSizeMsg = %q, want \"initialising…\"", got)
	}
}

// TestSelectedSessionID_RepoFallsThrough asserts that landing the
// cursor on a repo header resolves SelectedSessionID() to the repo's
// first session — matches the spike's SelectedSession behaviour, which
// the active pane area depends on.
func TestSelectedSessionID_RepoFallsThrough(t *testing.T) {
	tree := buildFixtureTree(t)
	m := New(tree, WithStates(fixtureStates()), WithSize(120, 24))
	row, ok := m.selectedRow()
	if !ok || row.Kind != RowRepo {
		t.Fatalf("setup: expected to start on a repo header, got %#v", row)
	}
	want := tree.RepoSessions(row.Repo)[0]
	if got := m.SelectedSessionID(); got != want {
		t.Errorf("SelectedSessionID on repo header = %q, want %q", got, want)
	}
}

// TestSelectedSessionID_Empty asserts the empty-tree case — no session
// is resolvable so SelectedSessionID returns "".
func TestSelectedSessionID_Empty(t *testing.T) {
	m := New(pane.New(), WithSize(120, 24))
	if got := m.SelectedSessionID(); got != "" {
		t.Errorf("empty tree SelectedSessionID = %q, want \"\"", got)
	}
}

// TestReviewDisplayName covers the §3.1 review-label rewrite rule:
// `~review-N-<agent>` → `~N-<agent>`, with the redundant `review-`
// stripped from the agent component too.
func TestReviewDisplayName(t *testing.T) {
	cases := []struct {
		id, parent, want string
	}{
		{"nixos-config@2141~review-1-review-code", "nixos-config@2141", "~1-code"},
		{"nixos-config@2141~review-2-review-security", "nixos-config@2141", "~2-security"},
		{"nixos-config@2141~review-1-code", "nixos-config@2141", "~1-code"},
		// Non-review name passes through unchanged.
		{"nixos-config@2141~something-else", "nixos-config@2141", "~something-else"},
	}
	for _, c := range cases {
		got := reviewDisplayName(c.id, c.parent)
		if got != c.want {
			t.Errorf("reviewDisplayName(%q, %q) = %q, want %q", c.id, c.parent, got, c.want)
		}
	}
}
