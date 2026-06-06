// Tests for the vt.Host API surface — the public guarantees the
// production renderer (PR #2152 / internal/mux/render) and any future
// consumer relies on. These are lifted from the spike's
// cmd/run_test.go (which targeted the spike's buildFrame helper) and
// refocused on the host's exported methods. The functional behaviour
// under test is identical; the test scaffolding is just one layer
// lower.
//
// The bugs these pin against:
//
//  1. DECTCEM (cursor visibility) tracking — the engine must report the
//     latest DECTCEM state at all times, and the seed value must be
//     "visible" because that is the power-on state of every real
//     terminal. PR #2144 introduced the mirror.
//
//  2. DECSCUSR (cursor shape) tracking — the engine must report the
//     n-parameter from the most recent DECSCUSR, and must report 0
//     before any DECSCUSR has been seen so callers do not override the
//     host terminal's default shape. PR #2146 introduced the mirror.
//
//  3. RenderRows row-count guarantee — the renderer relies on
//     RenderRows always returning exactly Size().rows entries so it can
//     emit one absolute-position sequence per visible row, sidestepping
//     both the LF-without-CR right-drift and the previous-frame residue
//     bugs that the spike's first paint loop shipped with.
//
//  4. DrainResponses — the goroutine pump for DSR / OSC / mode-query
//     replies. Well-behaved TUIs deadlock without it; the spike found
//     this is non-optional.
//
// All tests use t.TempDir() / in-memory pipes — no $HOME or
// $XDG_STATE_HOME writes, so the suite passes under the
// homeless-shelter Nix sandbox.
package vt

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// feed runs raw bytes into a fresh Host of the given dimensions and
// returns the host. The bytes are interpreted by x/vt as if they came
// from a PTY.
func feed(t *testing.T, cols, rows int, payload string) *Host {
	t.Helper()
	h := New(cols, rows)
	if _, err := h.Feed([]byte(payload)); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	return h
}

// TestCursorVisibleDefaultsTrue pins the seed value. Every real
// terminal powers on with the cursor visible; the zero value of an
// atomic.Bool is false, so New must explicitly seed true. A renderer
// that consults CursorVisible() on a fresh host must see "visible".
func TestCursorVisibleDefaultsTrue(t *testing.T) {
	h := New(10, 3)
	if !h.CursorVisible() {
		t.Fatalf("fresh Host should report cursor visible (DECTCEM on); got false")
	}
}

// TestCursorVisibilityMirrorsDECTCEM pins the visibility-tracking
// callback wired up in New. \x1b[?25l hides, \x1b[?25h shows. The
// renderer reads CursorVisible() at end-of-frame and emits the matching
// host-terminal sequence; if this tracker drifts, the host cursor
// stops mirroring the hosted TUI.
func TestCursorVisibilityMirrorsDECTCEM(t *testing.T) {
	h := feed(t, 10, 3, "\x1b[?25l")
	if h.CursorVisible() {
		t.Errorf("after \\x1b[?25l (DECTCEM off) CursorVisible should be false")
	}
	if _, err := h.Feed([]byte("\x1b[?25h")); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if !h.CursorVisible() {
		t.Errorf("after \\x1b[?25h (DECTCEM on) CursorVisible should be true")
	}
}

// TestCursorShapeDefaultsZero pins the "no opinion yet" seed. A fresh
// emulator has not seen a DECSCUSR; CursorShape() returning 0 is the
// signal to the renderer that it must NOT emit any DECSCUSR sequence
// to the host terminal, lest it stomp on the user's configured default
// cursor shape.
func TestCursorShapeDefaultsZero(t *testing.T) {
	h := New(10, 3)
	if got := h.CursorShape(); got != 0 {
		t.Fatalf("fresh Host should report CursorShape 0 (no DECSCUSR seen); got %d", got)
	}
}

// TestCursorShapeMirrorsDECSCUSR pins the n-value reconstruction in the
// CursorStyle callback registered in New. The DECSCUSR spec encodes
// (style, steady) as n = 2*style + (steady ? 2 : 1) — the table below
// drives every shape x/vt actually surfaces so the test does not pass
// by accident on a hard-coded "5" path.
//
// n = 0 and n = 1 are intentionally absent. x/vt treats both as
// "reset to terminal default" and does NOT fire the CursorStyle
// callback for them — Host.CursorShape() consequently stays at 0, the
// sentinel that tells the renderer "do not override the host
// terminal's configured default". This is the correct behaviour for
// the renderer's contract; pinning n = 1 here would assert a
// behaviour x/vt does not provide.
func TestCursorShapeMirrorsDECSCUSR(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		want uint32
	}{
		{"steady block", "\x1b[2 q", 2},
		{"blinking underline", "\x1b[3 q", 3},
		{"steady underline", "\x1b[4 q", 4},
		{"blinking bar (nvim INSERT)", "\x1b[5 q", 5},
		{"steady bar", "\x1b[6 q", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := feed(t, 10, 3, tc.seq)
			if got := h.CursorShape(); got != tc.want {
				t.Errorf("after %q: CursorShape = %d, want %d", tc.seq, got, tc.want)
			}
		})
	}
}

// TestCursorShapeIgnoresDefaultDECSCUSR pins the "x/vt treats n=0/n=1
// as default and does not fire the callback" behaviour the table-driven
// test above relies on. If a future x/vt upgrade starts firing the
// callback for these values, this test fails loudly and the renderer's
// "shape == 0 means do not override" contract needs to be revisited.
func TestCursorShapeIgnoresDefaultDECSCUSR(t *testing.T) {
	for _, seq := range []string{"\x1b[0 q", "\x1b[1 q", "\x1b[ q"} {
		t.Run(seq, func(t *testing.T) {
			h := feed(t, 10, 3, seq)
			if got := h.CursorShape(); got != 0 {
				t.Errorf("x/vt should treat %q as default (no callback); CursorShape = %d, want 0 — renderer contract assumes 0 means \"no override\"", seq, got)
			}
		})
	}
}

// TestRenderRowsReturnsExactlyEmulatorHeight is the load-bearing
// row-count guarantee. The renderer walks RenderRows and emits one
// absolute-position sequence per index — short content must yield
// empty strings for the unfilled rows, never a slice short of the
// configured height. Without this guarantee the previous-frame
// residue bug (visible at the bottom of the screen) returns.
func TestRenderRowsReturnsExactlyEmulatorHeight(t *testing.T) {
	const cols, rows = 20, 7
	// Single short row of content; rows 1..6 must come back as
	// empty strings, not be elided.
	h := feed(t, cols, rows, "only one")
	got := h.RenderRows()
	if len(got) != rows {
		t.Fatalf("RenderRows() returned %d rows, want %d", len(got), rows)
	}
	// Row 0 should contain the content we wrote.
	if !strings.Contains(got[0], "only one") {
		t.Errorf("row 0 should contain %q; got %q", "only one", got[0])
	}
}

// TestRenderRowsAfterMultilineContent pins per-row content alignment.
// CRLF-separated input must land one row per index up to the line
// count, with the remainder padded as empty strings.
func TestRenderRowsAfterMultilineContent(t *testing.T) {
	const cols, rows = 20, 5
	h := feed(t, cols, rows, "ROW0\r\nROW1\r\nROW2")
	got := h.RenderRows()
	if len(got) != rows {
		t.Fatalf("RenderRows() returned %d rows, want %d", len(got), rows)
	}
	for i, want := range []string{"ROW0", "ROW1", "ROW2"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("row %d should contain %q; got %q", i, want, got[i])
		}
	}
}

// TestSnapshotCursorPosition pins the engine cursor read-back. After
// CSI 2;5 H the engine's cursor sits at (col=5, row=2) one-indexed,
// which Snapshot reports as (X=4, Y=1) zero-indexed. The renderer
// relies on these coordinates to park the host cursor at end-of-frame.
func TestSnapshotCursorPosition(t *testing.T) {
	h := feed(t, 20, 5, "\x1b[2;5H")
	snap := h.Snapshot()
	if snap.CursorX != 4 || snap.CursorY != 1 {
		t.Errorf("Snapshot cursor = (%d,%d), want (4,1)", snap.CursorX, snap.CursorY)
	}
	if snap.Cols != 20 || snap.Rows != 5 {
		t.Errorf("Snapshot size = (%d,%d), want (20,5)", snap.Cols, snap.Rows)
	}
}

// TestResizeUpdatesDimensions pins the Resize forward to the
// underlying emulator. SIGWINCH-driven resize in cmd/run.go calls
// Host.Resize; Snapshot must reflect the new dimensions afterwards.
func TestResizeUpdatesDimensions(t *testing.T) {
	h := New(10, 3)
	h.Resize(40, 12)
	cols, rows := h.Size()
	if cols != 40 || rows != 12 {
		t.Errorf("after Resize(40,12) Size() = (%d,%d), want (40,12)", cols, rows)
	}
	snap := h.Snapshot()
	if snap.Cols != 40 || snap.Rows != 12 {
		t.Errorf("after Resize(40,12) Snapshot = (%d,%d), want (40,12)", snap.Cols, snap.Rows)
	}
	if got := len(snap.Cells); got != 12 {
		t.Errorf("after Resize(40,12) Snapshot.Cells len = %d, want 12", got)
	}
	if got := h.RenderRows(); len(got) != 12 {
		t.Errorf("after Resize(40,12) RenderRows() len = %d, want 12", len(got))
	}
}

// TestDrainResponsesEmitsDSRReply is the load-bearing pump assertion.
// CSI 6 n is "report cursor position"; the emulator replies via the
// internal pipe writer. Without DrainResponses running, that pipe
// fills and the next Feed eventually blocks — the spike found this
// is non-optional for well-behaved TUIs.
//
// Test shape: spawn DrainResponses in a goroutine writing to a pipe,
// feed CSI 6 n, read and assert the DSR reply from the pipe.
//
// Goroutine lifecycle: DrainResponses returns only when the underlying
// emulator hits EOF on its internal response pipe. The x/vt emulator
// exposes a Close() method that triggers that EOF, but the spike's
// vt.Host does not — by deliberate AC: the production port is verbatim
// and adds no new API. Production callers (cmd/run.go in the spike,
// internal/mux/render in the prism programme) simply let the goroutine
// die at process exit. Tests follow the same pattern: the goroutine
// leaks for the duration of the test process. This is intentional and
// documented; do not "fix" it by adding a Close on Host without
// updating the wider programme design.
func TestDrainResponsesEmitsDSRReply(t *testing.T) {
	h := New(20, 5)

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })

	go func() {
		// Goroutine intentionally leaks until process exit; see
		// function comment. Error is ignored — pipe close on the
		// reader side will surface as a write error here, which is
		// the desired shutdown signal from the test's perspective.
		_ = h.DrainResponses(pw)
	}()

	// CSI 6 n triggers DSR. Position the cursor first so the reply is
	// deterministic (row=2, col=3 one-indexed).
	if _, err := h.Feed([]byte("\x1b[2;3H\x1b[6n")); err != nil {
		t.Fatalf("Feed: %v", err)
	}

	// Read the response. Use a deadline — if DrainResponses never
	// delivered the reply, this would otherwise hang until the test
	// timeout.
	type readResult struct {
		buf []byte
		err error
	}
	rrCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := pr.Read(buf)
		rrCh <- readResult{buf[:n], err}
	}()

	var got []byte
	select {
	case rr := <-rrCh:
		if rr.err != nil && rr.err != io.EOF {
			t.Fatalf("read DSR reply: %v", rr.err)
		}
		got = rr.buf
	case <-time.After(2 * time.Second):
		t.Fatalf("DrainResponses did not deliver a DSR reply within 2s — the pump is broken")
	}

	// Standard DSR reply: ESC [ <row> ; <col> R. Cursor was at
	// (row=2, col=3) one-indexed.
	want := "\x1b[2;3R"
	if !bytes.Contains(got, []byte(want)) {
		t.Errorf("DSR reply = %q, want substring %q", string(got), want)
	}
}

// TestFeedReaderConsumesUntilEOF pins the byte-pump driver used by the
// PTY-→-emulator goroutine in cmd/run.go. EOF must be reported as nil
// — the cmd/run.go pump treats nil as "child exited cleanly" and the
// goroutine returns; a non-nil EOF would log spurious errors on every
// normal shutdown.
func TestFeedReaderConsumesUntilEOF(t *testing.T) {
	const payload = "abc\r\ndef"
	h := New(10, 3)
	r := strings.NewReader(payload)
	if err := h.FeedReader(r); err != nil {
		t.Fatalf("FeedReader returned %v on EOF, want nil", err)
	}
	rows := h.RenderRows()
	if len(rows) != 3 {
		t.Fatalf("RenderRows() len = %d, want 3", len(rows))
	}
	if !strings.Contains(rows[0], "abc") {
		t.Errorf("row 0 should contain %q; got %q", "abc", rows[0])
	}
	if !strings.Contains(rows[1], "def") {
		t.Errorf("row 1 should contain %q; got %q", "def", rows[1])
	}
}

// TestRenderUsesNoBareLFBetweenRowsForRenderRows is a guard on the
// row-splitter inside RenderRows. The renderer caller relies on each
// returned row string being free of embedded LFs (otherwise emitting
// the row in raw mode walks the cursor down without resetting the
// column — the original right-drift bug). Verify directly.
func TestRenderRowsNoEmbeddedNewlines(t *testing.T) {
	h := feed(t, 20, 5, "ROW0\r\nROW1\r\nROW2")
	for i, row := range h.RenderRows() {
		if strings.ContainsRune(row, '\n') {
			t.Errorf("row %d contains embedded LF — RenderRows must split on LF: %q", i, row)
		}
	}
}

// fmtFrame is unused outside of debugging — keep imported to allow
// quick `fmt.Printf` instrumentation without an unused-import error
// when iterating on these tests.
var _ = fmt.Sprintf
