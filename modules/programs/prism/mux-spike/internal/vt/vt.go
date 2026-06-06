// Package vt hosts a charmbracelet/x/vt Emulator and exposes cell-grid
// extraction helpers. This is the VT engine under test in the spike.
package vt

import (
	"image/color"
	"io"
	"sync"
	"sync/atomic"

	xvt "github.com/charmbracelet/x/vt"

	uv "github.com/charmbracelet/ultraviolet"
)

// Host wraps an x/vt Emulator with synchronisation around concurrent writes
// (from the PTY) and reads (from the renderer / corpus driver).
//
// x/vt's Emulator is not documented as safe for concurrent reads alongside
// writes; the spike serialises both behind a Mutex to keep the surface
// honest. If x/vt later grows a SafeEmulator that fits this shape, swap it
// in here.
type Host struct {
	mu   sync.Mutex
	emul *xvt.Emulator
	w, h int

	// cursorVisible tracks DECTCEM (the cursor show/hide mode) so the
	// renderer in cmd/run.go can mirror it to the host terminal each
	// frame. x/vt does not expose a getter for the visibility flag, but
	// it fires a Callbacks.CursorVisibility callback whenever the hosted
	// TUI toggles it. Terminals power on with the cursor visible, so the
	// zero value (false) is wrong — we seed true in New.
	cursorVisible atomic.Bool
}

// New constructs a Host backed by an Emulator of the given dimensions.
func New(cols, rows int) *Host {
	h := &Host{
		emul: xvt.NewEmulator(cols, rows),
		w:    cols,
		h:    rows,
	}
	h.cursorVisible.Store(true)
	h.emul.SetCallbacks(xvt.Callbacks{
		CursorVisibility: func(v bool) { h.cursorVisible.Store(v) },
	})
	return h
}

// CursorVisible reports the emulator's current DECTCEM state. Safe to call
// concurrently with Feed — the value is tracked via an atomic updated from
// the x/vt callback that fires inside Write.
func (h *Host) CursorVisible() bool {
	return h.cursorVisible.Load()
}

// Feed writes raw PTY bytes into the VT engine. Safe to call concurrently
// with Snapshot.
func (h *Host) Feed(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.emul.Write(p)
}

// FeedReader pumps bytes from r until EOF or error. Returns the underlying
// error (io.EOF is reported as nil — callers care about non-EOF failures).
func (h *Host) FeedReader(r io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := h.Feed(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// DrainResponses pumps the emulator's response stream (DSR replies, OSC
// answers, mode queries) into w. The TUI under test will deadlock if its
// query responses pile up unread — x/vt writes them into an internal
// io.PipeWriter that blocks once the pipe buffer fills. Spawn this in a
// goroutine alongside the PTY-→-emulator pump.
func (h *Host) DrainResponses(w io.Writer) error {
	buf := make([]byte, 4096)
	for {
		n, err := h.emul.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// Size returns the emulator dimensions.
func (h *Host) Size() (cols, rows int) {
	return h.w, h.h
}

// Resize forwards a resize to the underlying emulator.
func (h *Host) Resize(cols, rows int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.emul.Resize(cols, rows)
	h.w, h.h = cols, rows
}

// Cell is the spike's denormalised view of a single VT cell. Mirrors the
// fields the report.md grading process cares about.
type Cell struct {
	Glyph     string `json:"glyph"`
	Width     int    `json:"width"`
	FgKind    string `json:"fg_kind"`     // "default" | "rgb" | "indexed" | "ansi"
	FgValue   uint32 `json:"fg_value"`    // packed RGB or index
	BgKind    string `json:"bg_kind"`
	BgValue   uint32 `json:"bg_value"`
	Attrs     uint8  `json:"attrs"`       // ultraviolet.Attr* bitfield
	Underline int    `json:"underline"`   // ansi.Underline value (0 = none)
	ULColor   uint32 `json:"ul_color"`    // packed RGB if non-default
	Link      string `json:"link,omitempty"`
}

// Snapshot is a structural copy of the visible VT screen at one instant.
type Snapshot struct {
	Cols      int      `json:"cols"`
	Rows      int      `json:"rows"`
	CursorX   int      `json:"cursor_x"`
	CursorY   int      `json:"cursor_y"`
	AltScreen bool     `json:"alt_screen"`
	Cells     [][]Cell `json:"cells"` // Cells[row][col]
}

// Snapshot returns a structural copy of the emulator state.
//
// The caller MUST ensure no Feed is in flight when Snapshot is called — the
// host mutex is unfair under Go's runtime, and a streaming TUI (nvim, htop)
// will starve Snapshot's acquisition for the lifetime of the process. The
// corpus driver pauses its read pump before calling Snapshot. Asserting
// that contract here would require a TryLock loop, which is its own
// portability headache; for the spike, the contract is documented and
// upheld by the single caller.
func (h *Host) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	pos := h.emul.CursorPosition()
	snap := Snapshot{
		Cols:      h.w,
		Rows:      h.h,
		CursorX:   pos.X,
		CursorY:   pos.Y,
		AltScreen: h.emul.IsAltScreen(),
		Cells:     make([][]Cell, h.h),
	}
	for y := 0; y < h.h; y++ {
		row := make([]Cell, h.w)
		for x := 0; x < h.w; x++ {
			c := h.emul.CellAt(x, y)
			row[x] = convertCell(c)
		}
		snap.Cells[y] = row
	}
	return snap
}

// Render returns the emulator's own ANSI rendering of the visible screen.
// This is the engine's view of what the cells should look like when emitted
// back out, useful for visual diff against the tmux capture-pane output.
//
// Caveat for live replay: x/vt's Render() emits rows separated by bare LF
// (no CR) and does NOT pad short rows to the emulator width. Driving a host
// terminal directly with this output in raw mode produces right-drift
// (cursor walks across the screen at every LF) and previous-frame residue
// (unfilled cells leave whatever was there before). The corpus path is
// fine — it captures Render() as a textual artefact — but live-replay
// callers (see cmd/run.go) should consume the per-row data and emit their
// own absolute-positioned frame.
func (h *Host) Render() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.emul.Render()
}

// RenderRows splits the emulator's Render() output into one string per
// visible row, padded out to the configured row count. Rows beyond what
// Render() emits come back as empty strings. This is the live-replay
// primitive used by cmd/run.go::paint — the caller wraps each row in an
// absolute-position sequence and an erase-to-end-of-line, sidestepping the
// LF-without-CR and no-padding hazards described on Render().
func (h *Host) RenderRows() []string {
	rendered := h.Render()
	h.mu.Lock()
	rows := h.h
	h.mu.Unlock()

	out := make([]string, rows)
	if rendered == "" {
		return out
	}
	// Split on bare LF — that is what x/vt emits between rows. A trailing
	// LF would produce an extra empty element; clip the result to row
	// count so callers always see exactly rows entries.
	line := 0
	start := 0
	for i := 0; i < len(rendered) && line < rows; i++ {
		if rendered[i] == '\n' {
			out[line] = rendered[start:i]
			line++
			start = i + 1
		}
	}
	if line < rows && start < len(rendered) {
		out[line] = rendered[start:]
	}
	return out
}

func convertCell(c *uv.Cell) Cell {
	if c == nil || c.IsZero() {
		return Cell{Glyph: " ", Width: 1, FgKind: "default", BgKind: "default"}
	}
	out := Cell{
		Glyph:     c.Content,
		Width:     c.Width,
		Attrs:     c.Style.Attrs,
		Underline: int(c.Style.Underline),
	}
	if out.Glyph == "" {
		out.Glyph = " "
	}
	out.FgKind, out.FgValue = packColor(c.Style.Fg)
	out.BgKind, out.BgValue = packColor(c.Style.Bg)
	_, out.ULColor = packColor(c.Style.UnderlineColor)
	if c.Link.URL != "" {
		out.Link = c.Link.URL
	}
	return out
}

// packColor reduces a Go color.Color to (kind, value). The spike does not
// attempt to round-trip every colour space — for the diffing target, the
// 32-bit RGBA packing is enough fidelity to spot disagreement.
func packColor(c color.Color) (string, uint32) {
	if c == nil {
		return "default", 0
	}
	r, g, b, a := c.RGBA()
	if a == 0 {
		return "default", 0
	}
	// 16-bit channels → 8-bit packed RGB.
	packed := uint32(r>>8)<<16 | uint32(g>>8)<<8 | uint32(b>>8)
	return "rgb", packed
}
