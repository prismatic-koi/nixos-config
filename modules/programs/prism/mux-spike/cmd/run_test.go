// Structural tests for the interactive paint loop. These are NOT a substitute
// for an interactive smoke test against a real TTY — they cannot prove the
// frame *looks* right, only that the byte stream has the structural
// properties that protect against the two bugs the initial spike shipped
// with:
//
//   1. LF-without-CR right-drift. xvt's Render() separates rows with bare
//      LF; in raw mode that walks the cursor down without resetting the
//      column. The fix is to never emit raw inter-row LF — each row gets
//      its own CSI <y>;1 H absolute position. The assertions here pin
//      that.
//
//   2. Previous-frame residue. Render() does not pad short rows, so
//      anything left from the previous paint at columns past the row's
//      end-of-content stays on screen. The fix is a trailing CSI K
//      (erase-in-line) on every row. The assertions here pin that too.
//
// Interactive verification is Ben's responsibility once the patch lands —
// only he has the host TTY the run subcommand drives.
package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/prismatic-koi/nixos-config/modules/programs/prism/mux-spike/internal/vt"
)

// feed runs raw bytes into a fresh Host of the given dimensions and returns
// the host. The bytes are interpreted by x/vt as if they came from a PTY.
func feed(t *testing.T, cols, rows int, payload string) *vt.Host {
	t.Helper()
	h := vt.New(cols, rows)
	if _, err := h.Feed([]byte(payload)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	return h
}

// TestBuildFrame_PerRowAbsolutePositioning is the load-bearing structural
// assertion. Every visible row must be preceded by a CSI <y>;1 H sequence
// — that is what prevents the LF-without-CR right-drift in raw mode.
//
// Equivalently: between consecutive row contents, the bytes that *would*
// have been a bare \n in xvt's Render() must not appear in the frame.
func TestBuildFrame_PerRowAbsolutePositioning(t *testing.T) {
	const cols, rows = 20, 5
	// Three short rows; rows 3 and 4 are blank. This is the shape that
	// breaks Render(): unfilled rows leave residue from a previous frame.
	h := feed(t, cols, rows, "ROW0\r\nROW1\r\nROW2")

	frame := buildFrame(h)

	for y := 1; y <= rows; y++ {
		want := fmt.Sprintf("\x1b[%d;1H", y)
		if !strings.Contains(frame, want) {
			t.Errorf("frame missing absolute-position prefix %q for row %d.\nframe=%q",
				want, y, frame)
		}
	}

	// Verify ordering: row k's position must come before row k+1's. If
	// they ever swapped we'd be painting rows out of order, which would
	// still pass the contains check above but break in practice.
	var lastIdx int
	for y := 1; y <= rows; y++ {
		want := fmt.Sprintf("\x1b[%d;1H", y)
		idx := strings.Index(frame, want)
		if idx < lastIdx {
			t.Errorf("row %d position %q appears at byte %d, before previous row's %d",
				y, want, idx, lastIdx)
		}
		lastIdx = idx
	}
}

// TestBuildFrame_NoBareLineFeedBetweenRows is the direct anti-regression
// against the original bug — a bare LF anywhere between row contents would
// have allowed the right-drift to come back.
//
// We allow a single trailing LF only if it sits at the very end of the
// frame (xvt sometimes emits one and there's no harm in passing it on);
// any LF that is NOT immediately followed by CSI <n>;1 H or end-of-string
// is suspect.
func TestBuildFrame_NoBareLineFeedBetweenRows(t *testing.T) {
	h := feed(t, 20, 5, "ROW0\r\nROW1\r\nROW2")
	frame := buildFrame(h)

	for i := 0; i < len(frame); i++ {
		if frame[i] != '\n' {
			continue
		}
		t.Errorf("frame contains bare LF at byte %d — that re-introduces the right-drift bug.\nframe=%q",
			i, frame)
	}
}

// TestBuildFrame_EraseToEndOfLinePerRow pins the residue fix. Each row's
// emitted content must end with CSI K (erase-in-line) so any leftover
// glyphs from a previous frame at columns past this row's end-of-content
// get blanked. We also expect a CSI 0 m reset immediately before CSI K so
// the erase paints the default background, not the last cell's bg colour.
func TestBuildFrame_EraseToEndOfLinePerRow(t *testing.T) {
	h := feed(t, 20, 3, "AAA\r\nBBB\r\nCCC")
	frame := buildFrame(h)

	// Count occurrences of the reset+EL pair. Should be exactly one per
	// row (3 in this case). If the count is lower the residue bug is
	// back; if higher, something is double-painting.
	got := strings.Count(frame, "\x1b[0m\x1b[K")
	if got != 3 {
		t.Errorf("expected 3 reset+erase-in-line sequences (one per row), got %d.\nframe=%q",
			got, frame)
	}
}

// TestBuildFrame_CursorPositionedAtEngineCursor verifies the frame ends
// with the host cursor parked where the engine thinks the cursor is —
// otherwise nvim's blinking cursor would sit at the home position or at
// wherever the last row's absolute-position sequence left it.
func TestBuildFrame_CursorPositionedAtEngineCursor(t *testing.T) {
	// Move cursor to row 2 col 5 (1-indexed in CSI, 0-indexed in the
	// Snapshot — we assert against the Snapshot's view).
	h := feed(t, 20, 5, "\x1b[2;5H")
	snap := h.Snapshot()
	if snap.CursorX != 4 || snap.CursorY != 1 {
		t.Fatalf("engine cursor not where we set it: got (%d,%d), want (4,1)",
			snap.CursorX, snap.CursorY)
	}

	frame := buildFrame(h)
	want := fmt.Sprintf("\x1b[%d;%dH", snap.CursorY+1, snap.CursorX+1)

	// The cursor-position sequence must appear AFTER every per-row
	// absolute-position sequence — otherwise the per-row paints would
	// stomp on it.
	cursorIdx := strings.LastIndex(frame, want)
	if cursorIdx < 0 {
		t.Fatalf("frame missing cursor-position sequence %q.\nframe=%q", want, frame)
	}
	rowPosRe := regexp.MustCompile(`\x1b\[(\d+);1H`)
	for _, m := range rowPosRe.FindAllStringIndex(frame, -1) {
		if m[0] > cursorIdx {
			t.Errorf("row-positioning sequence at byte %d appears after the cursor-position at byte %d — the cursor will be wrong.",
				m[0], cursorIdx)
		}
	}
}

// TestBuildFrame_CursorVisibilityMirrorsEngine checks that the frame's
// final DECTCEM state matches the engine's. Default state is visible, and
// the frame should re-show the cursor (paint hides it mid-frame so the
// dart between row positions doesn't blink across the screen).
func TestBuildFrame_CursorVisibilityMirrorsEngine(t *testing.T) {
	t.Run("visible by default", func(t *testing.T) {
		h := feed(t, 10, 3, "hi")
		if !h.CursorVisible() {
			t.Fatalf("engine should default to cursor-visible")
		}
		frame := buildFrame(h)
		// Hide must appear once at the start (mid-paint suppression),
		// show must appear at the end.
		if !strings.HasPrefix(frame, "\x1b[?25l") {
			t.Errorf("frame should start with cursor-hide (\\x1b[?25l) to suppress mid-paint blinking.\nframe=%q",
				frame)
		}
		if !strings.HasSuffix(frame, "\x1b[?25h") {
			t.Errorf("frame should end with cursor-show (\\x1b[?25h) when engine has cursor visible.\nframe=%q",
				frame)
		}
	})

	t.Run("hidden when engine hides it", func(t *testing.T) {
		// DECTCEM off — engine should track this via the CursorVisibility
		// callback we register in vt.New.
		h := feed(t, 10, 3, "\x1b[?25l")
		if h.CursorVisible() {
			t.Fatalf("engine should track DECTCEM-off as cursor-hidden")
		}
		frame := buildFrame(h)
		if strings.HasSuffix(frame, "\x1b[?25h") {
			t.Errorf("frame should NOT re-show the cursor when engine hid it.\nframe=%q",
				frame)
		}
	})
}

// TestBuildFrame_CursorShapeMirrorsEngine pins the DECSCUSR (cursor-shape)
// relay added on top of PR #2144's visibility + position mirroring. nvim
// sends "\x1b[5 q" (blinking bar) on INSERT-mode entry; if the paint loop
// does not forward that sequence to the host terminal, the user's cursor
// stays a block and INSERT mode is invisible.
//
// Anti-regression: this test was verified to FAIL against a stub of
// buildFrame that omits the `host.CursorShape()` emission — same discipline
// PR #2144 applied to its five structural tests. The "no shape emitted by
// default" sub-test also pins that we don't override the host's terminal
// default before the hosted TUI signals a shape.
func TestBuildFrame_CursorShapeMirrorsEngine(t *testing.T) {
	// DECSCUSR sequences look like "\x1b[<n> q" — CSI, decimal digits,
	// literal space, literal q. Match zero-or-more digits so the regex
	// also catches "\x1b[ q" (reset to default) variants if we ever emit
	// them by accident inside a frame.
	decscusrRe := regexp.MustCompile(`\x1b\[\d* q`)

	t.Run("no shape emitted by default", func(t *testing.T) {
		// Fresh emulator, no DECSCUSR fed. CursorShape() returns 0; the
		// frame must NOT contain any DECSCUSR or we'd be stomping on the
		// host terminal's configured default cursor.
		h := feed(t, 10, 3, "hi")
		if got := h.CursorShape(); got != 0 {
			t.Fatalf("engine cursor shape should be 0 before any DECSCUSR; got %d", got)
		}
		frame := buildFrame(h)
		if loc := decscusrRe.FindStringIndex(frame); loc != nil {
			t.Errorf("frame should not emit DECSCUSR before engine has seen one; found %q at byte %d.\nframe=%q",
				frame[loc[0]:loc[1]], loc[0], frame)
		}
	})

	t.Run("mirrors blinking bar (DECSCUSR 5)", func(t *testing.T) {
		// \x1b[5 q is the sequence nvim emits when entering INSERT mode.
		h := feed(t, 10, 3, "\x1b[5 q")
		if got := h.CursorShape(); got != 5 {
			t.Fatalf("engine should track DECSCUSR 5 (blinking bar); got %d", got)
		}

		frame := buildFrame(h)
		const want = "\x1b[5 q"
		idx := strings.Index(frame, want)
		if idx < 0 {
			t.Fatalf("frame missing DECSCUSR %q.\nframe=%q", want, frame)
		}

		// The shape sequence must land AFTER the final cursor-position
		// sequence — otherwise a row-paint or the end-of-frame position
		// move could reset the cursor shape on terminals that tie shape
		// state to the cursor. The spec is explicit: "emit ... at
		// end-of-frame alongside the cursor-position + visibility-restore".
		posRe := regexp.MustCompile(`\x1b\[\d+;\d+H`)
		positions := posRe.FindAllStringIndex(frame, -1)
		if len(positions) == 0 {
			t.Fatalf("frame missing any cursor-position sequence.\nframe=%q", frame)
		}
		lastPos := positions[len(positions)-1]
		if idx < lastPos[1] {
			t.Errorf("DECSCUSR at byte %d appears before the final cursor-position sequence ending at byte %d — the shape relay must come after the position move.\nframe=%q",
				idx, lastPos[1], frame)
		}
	})

	t.Run("mirrors steady block (DECSCUSR 2)", func(t *testing.T) {
		// Sanity-check the n-value reconstruction across a different
		// (style, steady) pair so the test doesn't pass by accident on a
		// hard-coded "5" path.
		h := feed(t, 10, 3, "\x1b[2 q")
		if got := h.CursorShape(); got != 2 {
			t.Fatalf("engine should track DECSCUSR 2 (steady block); got %d", got)
		}
		frame := buildFrame(h)
		if !strings.Contains(frame, "\x1b[2 q") {
			t.Errorf("frame missing DECSCUSR 2.\nframe=%q", frame)
		}
	})
}

// TestBuildFrame_EmitsOneRowPerEmulatorRow guarantees that even when the
// engine has rendered fewer rows than the emulator height (e.g. a fresh
// emulator with a single line of text), we still emit positioning for
// every row up to the configured height. Otherwise the bottom of a
// previous frame could remain on screen — the same residue class of bug
// from a different angle.
func TestBuildFrame_EmitsOneRowPerEmulatorRow(t *testing.T) {
	const cols, rows = 10, 7
	h := feed(t, cols, rows, "only one") // single-row content
	frame := buildFrame(h)
	for y := 1; y <= rows; y++ {
		want := fmt.Sprintf("\x1b[%d;1H", y)
		if !strings.Contains(frame, want) {
			t.Errorf("frame missing per-row position for row %d (height=%d). All %d rows must be repainted every frame to avoid residue.\nframe=%q",
				y, rows, rows, frame)
		}
	}
}
