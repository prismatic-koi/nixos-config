package tmuxstatus

import (
	"strings"
	"testing"
)

// testColors returns a fixed palette so output expectations are stable across
// CI hosts (defaults from internal/config could drift; the tests don't care
// about specific shades, only about which colour-slot is wired to which pip).
func testColors() Colors {
	return Colors{
		Yellow:  "#yellow",
		Purple:  "#purple",
		Green:   "#green",
		Red:     "#red",
		Primary: "#primary",
	}
}

// TestFormatWaiting_ZeroIsEmpty asserts the AC: zero-waiting → empty string.
// The status bar must not render a stray separator when there's nothing to say.
func TestFormatWaiting_ZeroIsEmpty(t *testing.T) {
	if got := FormatWaiting(Counts{}, testColors()); got != "" {
		t.Errorf("FormatWaiting(0) = %q, want empty string", got)
	}
	// A non-zero non-Waiting field must not flip the result to non-empty:
	// --waiting is strict, only the Waiting bucket counts.
	if got := FormatWaiting(Counts{Active: 5, Finished: 3, Error: 1}, testColors()); got != "" {
		t.Errorf("FormatWaiting(active+finished+error, no waiting) = %q, want empty", got)
	}
}

// TestFormatWaiting_NonZero asserts the byte-for-byte segment shape so the
// iris and prism status bar segments stay visually identical.
func TestFormatWaiting_NonZero(t *testing.T) {
	got := FormatWaiting(Counts{Waiting: 2}, testColors())
	want := "#[fg=#yellow]2 waiting #[fg=#primary]| "
	if got != want {
		t.Errorf("FormatWaiting:\n got: %q\nwant: %q", got, want)
	}
}

// TestFormat_AllZeroIsEmpty asserts the all-zero (or only-idle) case yields
// an empty string. Idle is the non-event baseline and never earns a pip.
func TestFormat_AllZeroIsEmpty(t *testing.T) {
	if got := Format(Counts{}, testColors()); got != "" {
		t.Errorf("Format(zero) = %q, want empty", got)
	}
	if got := Format(Counts{Idle: 7}, testColors()); got != "" {
		t.Errorf("Format(only idle) = %q, want empty", got)
	}
}

// TestFormat_OrderAndColours asserts the canonical ordering — waiting,
// active, finished, error — and the colour slot each pip occupies. The
// trailing "| " separator must be Primary-coloured and present when at least
// one pip rendered.
func TestFormat_OrderAndColours(t *testing.T) {
	got := Format(Counts{Active: 1, Waiting: 2, Finished: 3, Error: 4}, testColors())

	// Order matters: waiting must come before active before finished before error.
	wantSubsInOrder := []string{
		"#[fg=#yellow]2 waiting",
		"#[fg=#purple]1 active",
		"#[fg=#green]3 done",
		"#[fg=#red]4 error",
		"#[fg=#primary]| ",
	}
	prev := -1
	for _, s := range wantSubsInOrder {
		idx := strings.Index(got, s)
		if idx < 0 {
			t.Errorf("Format output missing %q:\n  full: %q", s, got)
			continue
		}
		if idx <= prev {
			t.Errorf("Format output has %q out of order (idx %d <= prev %d):\n  full: %q",
				s, idx, prev, got)
		}
		prev = idx
	}
}

// TestFormat_OmitsZeroPips asserts pips with zero count are dropped, not
// rendered as "0 waiting". Otherwise the status bar would always show every
// label regardless of whether anything is happening.
func TestFormat_OmitsZeroPips(t *testing.T) {
	got := Format(Counts{Waiting: 1}, testColors())
	if !strings.Contains(got, "1 waiting") {
		t.Errorf("Format must include the present pip: %q", got)
	}
	for _, banned := range []string{"0 active", "0 done", "0 error", "0 waiting"} {
		if strings.Contains(got, banned) {
			t.Errorf("Format must not render zero-count pip %q: %q", banned, got)
		}
	}
}
