package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestStateVisualMapping asserts the §3.1 state-to-glyph-and-colour
// table verbatim. The hex values are the binding ones — failure here
// means the visual vocabulary has drifted from the spec.
func TestStateVisualMapping(t *testing.T) {
	cases := []struct {
		state      State
		wantGlyph  string
		wantHex    string
		wantStrike bool
	}{
		{StateActive, "●", "#4ade80", false},
		{StateIdle, "○", "#71717a", false},
		{StateWaiting, "◐", "#facc15", false},
		{StateReviewing, "◑", "#60a5fa", false},
		{StateEscalated, "▲", "#f87171", false},
		{StateFinished, "●", "#52525b", true},
	}
	for _, c := range cases {
		v := visualFor(c.state)
		if v.glyph != c.wantGlyph {
			t.Errorf("%s: glyph = %q, want %q", c.state, v.glyph, c.wantGlyph)
		}
		if string(v.colour) != c.wantHex {
			t.Errorf("%s: colour = %q, want %q", c.state, v.colour, c.wantHex)
		}
		if v.strikethrough != c.wantStrike {
			t.Errorf("%s: strikethrough = %v, want %v", c.state, v.strikethrough, c.wantStrike)
		}
	}
}

// TestSelectionHighlight asserts the §3.1 selected-row style: zinc-700
// background, zinc-50 foreground, bold. Exercises the style by
// rendering a short string and inspecting the emitted ANSI for the hex
// values lipgloss writes.
func TestSelectionHighlight(t *testing.T) {
	// Force TrueColor so the hex makes it into the ANSI output.
	lipgloss.SetColorProfile(lipgloss.ColorProfile())
	rendered := nameStyle(StateActive, true).Render("@main")
	// Convert RGB hex to the channel decimals lipgloss writes.
	for _, hex := range []string{"#fafafa", "#3f3f46"} {
		dec := hexToANSIChannels(hex)
		if !strings.Contains(rendered, dec) {
			t.Errorf("rendered selection style missing channels for %s (%s)\nrendered: %q",
				hex, dec, rendered)
		}
	}
}

// TestSidebarWidth asserts §3.1's "Fixed 32 columns in MVP" constant.
// A regression here would mean the sidebar width drifted from spec.
func TestSidebarWidth(t *testing.T) {
	if SidebarWidth != 32 {
		t.Errorf("SidebarWidth = %d, want 32 (§3.1)", SidebarWidth)
	}
	if NarrowWidthThreshold != 80 {
		t.Errorf("NarrowWidthThreshold = %d, want 80 (§3.1)", NarrowWidthThreshold)
	}
}

// hexToANSIChannels converts a `#rrggbb` string to the decimal channel
// triplet lipgloss writes in its TrueColor ANSI escapes (the form
// "248;248;250"). Used by the colour-assertion tests to check the
// rendered output without depending on lipgloss internals.
func hexToANSIChannels(hex string) string {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return hex
	}
	rs, gs, bs := hex[0:2], hex[2:4], hex[4:6]
	return parseHex(rs) + ";" + parseHex(gs) + ";" + parseHex(bs)
}

// parseHex parses a two-character hex string to its decimal value
// string. Falls back to the input on parse error so a malformed test
// input shows up as a mismatch rather than a silent zero.
func parseHex(s string) string {
	if len(s) != 2 {
		return s
	}
	var n int
	for _, c := range s {
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			v = int(c - 'A' + 10)
		default:
			return s
		}
		n = n*16 + v
	}
	return intToString(n)
}

// intToString avoids strconv — keeps the colour-test helper free of
// stdlib imports that would otherwise drift outside the visible style
// assertion.
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
