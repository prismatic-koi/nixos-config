// Package tmuxstatus formats agent-session state counts for embedding in a
// tmux status-right segment.
//
// This is the rendering layer used by `prism sessions status --tmux-format`.
//
// The output uses tmux #[fg=...] colour escapes — these are interpreted by
// tmux when the string is substituted into status-right; they are *not*
// terminal escape sequences. A consumer that prints the output anywhere
// other than tmux will see literal "#[fg=#xxxxxx]" text.
//
// All formatters return "" when there is nothing to render. This is the
// graceful-degradation contract relied on by tmux's continuous status-right
// invocation: an empty string disappears from the status bar; a non-empty
// string with a trailing "| " separator integrates with the rest of the
// status segments.
package tmuxstatus

import (
	"fmt"
	"strings"
)

// Counts is the per-state session-count input to the formatters.
//
// The five fields here are the canonical state set rendered in the status
// bar. Other transient states must be folded into one of these by the
// caller before rendering — keeping the formatter agnostic of upstream
// state taxonomies means callers can evolve their state machines
// independently without dragging the formatter along.
type Counts struct {
	Active   int
	Waiting  int
	Idle     int
	Finished int
	Error    int
}

// Colors holds the four foreground colours referenced by the formatter, plus
// the trailing-separator colour used to close out the segment.
//
// The fields are tmux-format colour values (typically "#rrggbb" hex strings,
// but any tmux-accepted colour name works). They come from the prism config
// (loaded via internal/config) so a single ~/.config/prism/config.json drives
// the segment.
type Colors struct {
	Yellow  string // waiting count
	Purple  string // active count
	Green   string // finished count
	Red     string // error count
	Primary string // trailing "| " separator
}

// FormatWaiting renders the --waiting --tmux-format segment: a single
// "<n> waiting" pip in Yellow followed by the Primary-coloured "| "
// separator. Returns "" when c.Waiting <= 0 so the segment disappears
// entirely from the status bar (the AC's "no waiting → empty string" path).
//
// The trailing space after "| " is intentional — it separates this segment
// from whatever follows in status-right (the prism segment, the hostname,
// etc.).
func FormatWaiting(c Counts, col Colors) string {
	if c.Waiting <= 0 {
		return ""
	}
	return fmt.Sprintf("#[fg=%s]%d waiting #[fg=%s]| ", col.Yellow, c.Waiting, col.Primary)
}

// Format renders the full multi-state segment: a Yellow waiting pip, a
// Purple active pip, a Green finished pip, and a Red error pip — each
// included only when its count is non-zero, in that fixed order, separated
// by a single space, with a trailing Primary-coloured "| " separator.
//
// Returns "" when no pip would render (all four counts are zero), so the
// segment again vanishes from the status bar rather than emitting a
// dangling separator. Idle is intentionally omitted: idle sessions are the
// non-event baseline and don't earn a status-bar pip.
func Format(c Counts, col Colors) string {
	var parts []string
	if c.Waiting > 0 {
		parts = append(parts, fmt.Sprintf("#[fg=%s]%d waiting", col.Yellow, c.Waiting))
	}
	if c.Active > 0 {
		parts = append(parts, fmt.Sprintf("#[fg=%s]%d active", col.Purple, c.Active))
	}
	if c.Finished > 0 {
		parts = append(parts, fmt.Sprintf("#[fg=%s]%d done", col.Green, c.Finished))
	}
	if c.Error > 0 {
		parts = append(parts, fmt.Sprintf("#[fg=%s]%d error", col.Red, c.Error))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("%s #[fg=%s]| ", strings.Join(parts, " "), col.Primary)
}
