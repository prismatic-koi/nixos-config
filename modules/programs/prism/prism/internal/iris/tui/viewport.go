package tui

import "github.com/prismatic-koi/prism/internal/iris/narrative"

// viewport.go — the conversation pane's buffered scrollback abstraction
// (issue #1770 child 5 of the iris-tui design).
//
// `lineViewport` is the functionally-equivalent in-house alternative to
// github.com/charmbracelet/bubbles/viewport. It tracks scroll position
// over an externally-owned []NarrativeLine slice; the Model continues to
// own the underlying line buffer so that the tool-card splice logic
// (#1769 rebuildCardLines) can mutate the slice in place without going
// through this viewport.
//
// Design notes on auto-tail (acceptance criteria for #1770):
//
//   - `following` is the boolean "stick to bottom" flag. When true,
//     the viewport's visible window is the last `height` lines regardless
//     of the offset field.
//
//   - When a NEW line arrives at the tail of the buffer:
//
//   - following == true  → stay at bottom (the new line becomes visible)
//
//   - following == false → offset is unchanged; the operator's reading
//     position is preserved. The pendingNewCount field on the Model
//     (not on the viewport) increments to drive the "↓ N new" indicator.
//
//   - When OLDER lines are PREPENDED (lazy-load):
//
//   - following stays whatever it was.
//
//   - offset shifts by len(prepended) so the same top line remains the
//     top line on screen.
//
//   - When the operator scrolls down past the bottom edge, following
//     is re-enabled. This is the auto-tail "resume" path.
//
// All offset arithmetic clamps to [0, max(0, len(lines)-height)] so that
// the visible window is always a valid slice of the line buffer.
type lineViewport struct {
	// following is the auto-tail flag. true → snap to bottom each frame.
	following bool
	// offset is the index of the topmost visible line into the externally
	// owned line buffer. Meaningful when following is false; recomputed
	// on every call to Update() so that it always points at a valid row.
	offset int
	// height is the most recent content height (visible rows). Stored
	// here so methods that don't take the height parameter (Visible,
	// AtBottom, AtTop) can still answer correctly. Updated by Update().
	height int
}

// newLineViewport constructs an empty viewport in the "following" state
// (auto-tail to bottom). This matches the AC: at session start there
// are no events, and the next event to arrive should be visible.
func newLineViewport() lineViewport {
	return lineViewport{following: true}
}

// Update clamps the viewport's offset against the current line count and
// height. Call this at the start of each frame (before Visible) so the
// offset always points at a valid window. When `following` is true the
// offset is dragged to the bottom on every Update — that is the auto-tail
// mechanism.
//
// height is the number of visible rows the pane can render. Must be > 0
// for a useful Visible() result; callers pass 0 when the pane is too
// small to draw, and Visible() then returns an empty slice.
func (v *lineViewport) Update(lineCount, height int) {
	if height < 0 {
		height = 0
	}
	v.height = height
	if v.following {
		// Auto-tail: offset = bottom.
		v.offset = maxOffset(lineCount, height)
		return
	}
	// Manual offset; clamp.
	if v.offset < 0 {
		v.offset = 0
	}
	if max := maxOffset(lineCount, height); v.offset > max {
		v.offset = max
	}
}

// Visible returns the slice of `lines` currently visible in the pane.
// Returns up to `height` lines starting at `offset`. An empty slice is
// returned when height == 0 or lines is empty.
func (v *lineViewport) Visible(lines []narrative.NarrativeLine) []narrative.NarrativeLine {
	if v.height <= 0 || len(lines) == 0 {
		return nil
	}
	start := v.offset
	if start < 0 {
		start = 0
	}
	end := start + v.height
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		start = end
	}
	return lines[start:end]
}

// VisibleRange returns the [start, end) window into the externally
// owned line buffer. Equivalent to Visible() but without needing the
// caller to pass the buffer in — used when the buffer is held on a
// struct that is not directly accessible to the viewport.
func (v *lineViewport) VisibleRange(lineCount int) (start, end int) {
	if v.height <= 0 || lineCount == 0 {
		return 0, 0
	}
	start = v.offset
	if start < 0 {
		start = 0
	}
	end = start + v.height
	if end > lineCount {
		end = lineCount
	}
	if start > end {
		start = end
	}
	return start, end
}

// AtBottom reports whether the viewport is currently showing the
// last `height` lines of the buffer. true when `following` is set OR
// when the manual offset happens to coincide with the bottom of the
// buffer. The auto-tail decision (in handleDaemonFrame) consults this
// before appending a new line.
func (v *lineViewport) AtBottom(lineCount int) bool {
	if v.following {
		return true
	}
	return v.offset >= maxOffset(lineCount, v.height)
}

// AtTop reports whether the viewport's first visible line is the
// first line in the buffer. Used to gate the lazy-load request: when
// the operator scrolls past the top, the TUI fires a request for
// older events.
func (v *lineViewport) AtTop() bool {
	return v.offset <= 0
}

// ScrollUp moves the visible window up by `n` lines, capped at 0.
// Disables auto-tail because scrolling up explicitly is the operator
// reading historic content. Returns true when the offset actually
// moved (false when already at the top).
//
// When the viewport is transitioning out of auto-tail mode (following
// == true on entry), the offset is first snapped to the current
// bottom-of-buffer position (lineCount-height) before the scroll-up
// is applied. This keeps the visible content stable across the
// transition: the row at the bottom of the screen immediately before
// the PgUp is the same row visible immediately after (modulo the
// requested scroll).
func (v *lineViewport) ScrollUp(n, lineCount int) bool {
	if n <= 0 {
		return false
	}
	if v.following {
		v.offset = maxOffset(lineCount, v.height)
	}
	v.following = false
	newOffset := v.offset - n
	if newOffset < 0 {
		newOffset = 0
	}
	if newOffset == v.offset {
		return false
	}
	v.offset = newOffset
	return true
}

// ScrollDown moves the visible window down by `n` lines. When the
// resulting offset lands at the bottom edge, re-enables auto-tail so
// future appends snap to bottom (the AC: "Pressing End ... jumps to
// the bottom and resumes auto-tail" — ScrollDown to the bottom does
// the same).
func (v *lineViewport) ScrollDown(n, lineCount int) bool {
	if n <= 0 {
		return false
	}
	max := maxOffset(lineCount, v.height)
	// When entering ScrollDown from auto-tail mode the offset may be
	// stale (the renderer is what normally drags it forward each
	// frame). Snap to bottom so the no-op return short-circuits
	// correctly when already at the bottom edge.
	if v.following {
		v.offset = max
	}
	newOffset := v.offset + n
	if newOffset >= max {
		newOffset = max
		v.following = true
	}
	if newOffset == v.offset && v.following {
		// Already at bottom; nothing moved but auto-tail is on.
		return false
	}
	if newOffset == v.offset {
		return false
	}
	v.offset = newOffset
	return true
}

// GotoTop jumps to the head of the buffer. Disables auto-tail.
// Equivalent to the `home` / `g` keybinding (AC #1770).
func (v *lineViewport) GotoTop() {
	v.following = false
	v.offset = 0
}

// GotoBottom jumps to the tail of the buffer and re-enables
// auto-tail. Equivalent to the `end` / `G` keybinding (AC #1770).
func (v *lineViewport) GotoBottom(lineCount int) {
	v.following = true
	v.offset = maxOffset(lineCount, v.height)
}

// OnAppend is the hook called when one or more lines are appended to
// the tail of the buffer. When auto-tail is on, the offset is dragged
// forward by Update() on the next frame — nothing to do here. When
// auto-tail is OFF, we leave the offset alone so the operator's
// reading position is unchanged.
//
// This is structured as an explicit method (even though the body is
// currently empty) so that future changes to auto-tail semantics
// (e.g. sticky-to-line behaviour rather than sticky-to-bottom) have
// an obvious seam to hook into without re-touching every appender.
func (v *lineViewport) OnAppend(added int) { _ = added }

// OnPrepend shifts the offset forward by `added` so the same line
// that was the top of the visible window before the prepend stays at
// the top after. Auto-tail is preserved: when following == true, the
// new lines at the head are invisible (we're still showing the tail);
// when following == false, the offset shift keeps the reading
// position pinned to the same source row.
func (v *lineViewport) OnPrepend(added int) {
	if added <= 0 {
		return
	}
	v.offset += added
}

// Reset returns the viewport to its initial following-bottom state
// with offset 0. Called by Model.resetEventPane on session switch
// (AC: "Switching focus to a different session resets the viewport
// to that session's tail").
func (v *lineViewport) Reset() {
	v.following = true
	v.offset = 0
}

// Following reports whether the viewport is currently in auto-tail
// mode. Exposed so the Model can decide whether to increment the
// pendingNewCount indicator when a new event arrives.
func (v *lineViewport) Following() bool { return v.following }

// Offset returns the current top-line offset. Test-only accessor;
// production code should not depend on the absolute value.
func (v *lineViewport) Offset() int { return v.offset }

// maxOffset returns the largest valid offset for a buffer of
// `lineCount` lines and a viewport of `height` rows. Equal to
// max(0, lineCount-height) so that the last `height` lines fit
// exactly into the visible window.
func maxOffset(lineCount, height int) int {
	if lineCount <= height {
		return 0
	}
	return lineCount - height
}
