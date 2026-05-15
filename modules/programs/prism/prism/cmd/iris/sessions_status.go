package main

// sessions_status.go — `iris sessions status` implementation.
//
// Prints state counts derived from the iris daemon's sessions_snapshot.
// Without --json: one human-readable line padded across the canonical state
// set (visual stability across invocations matters for shell aliases that
// align several lines side by side).
// With --json: a single JSON object keyed by state with integer counts.
// With --tmux-format: a tmux #[fg=...] colour-formatted segment (mirrors
// `prism sessions status --tmux-format` byte-for-byte via the shared
// internal/tmuxstatus package). Adding --waiting restricts the segment to
// just the waiting-count pip — this is the form embedded in the tmux
// status-right alongside the prism segment during the iris coexistence
// window (#1672).
//
// Tmux status-right runs the command continuously. A non-empty error string
// would therefore be permanently visible in the status bar — so when
// --tmux-format is set and the daemon is unreachable, the command exits 0
// with an empty string. The error path is preserved for the non-tmux modes
// (operators running the command interactively still want to know the
// daemon is down).
//
// The canonical state set we report on is:
//
//	active, waiting, idle, finished, error
//
// Sessions whose `state` field doesn't match any canonical name are bucketed
// into `idle` (defensive — should not happen in normal operation; matches
// prism's behaviour). The `spawning` iris-internal state (pre-handshake)
// is also folded into `active` for the human form because from an operator's
// perspective "the session is starting up" is closer to active than to idle —
// the JSON form preserves it as its own key only if a session is actually in
// that state at the time of the snapshot.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/tmuxstatus"
)

// sessionStateCounts holds the per-state count totals. Keys correspond to the
// canonical state names. New states added to iris will surface here without
// code changes: any state we don't explicitly recognise lands in `other`,
// which is rendered only when non-zero. The five canonical buckets are
// always emitted so the JSON shape is stable.
type sessionStateCounts struct {
	Active   int `json:"active"`
	Waiting  int `json:"waiting"`
	Idle     int `json:"idle"`
	Finished int `json:"finished"`
	Error    int `json:"error"`
	// Spawning is iris-specific (pre-handshake). Emitted only when >0 so
	// scripts that key off the canonical five aren't surprised by an
	// extra always-zero field; bucketed into Active in the human form.
	Spawning int `json:"spawning,omitempty"`
}

// runSessionsStatus is the cobra RunE for `iris sessions status`.
func runSessionsStatus(cmd *cobra.Command, _ []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	tmuxFormat, _ := cmd.Flags().GetBool("tmux-format")
	waitingOnly, _ := cmd.Flags().GetBool("waiting")

	// Mutual exclusion is checked before any I/O so the error message is
	// the same whether or not the daemon is reachable. Mirrors the
	// equivalent guard in prism's status command.
	if jsonMode && tmuxFormat {
		return fmt.Errorf("iris sessions status: --json and --tmux-format are mutually exclusive")
	}

	sockPath := resolveSocketPath(cmd)
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()

	snap, err := fetchSessionsSnapshot(ctx, sockPath)
	if err != nil {
		// Graceful-degradation path for tmux status-right: an error string
		// would otherwise be permanently visible in the status bar. Exit 0
		// with no output and let the segment vanish until the daemon
		// returns. Operators running `iris sessions status` interactively
		// (no --tmux-format) still get the actionable error.
		if tmuxFormat {
			return nil
		}
		return err
	}

	counts := countStates(snap.Sessions)

	if tmuxFormat {
		return renderStatusTmux(cmd.OutOrStdout(), counts, waitingOnly)
	}
	if jsonMode {
		return renderStatusJSON(cmd.OutOrStdout(), counts)
	}
	if waitingOnly {
		return renderStatusWaitingPlain(cmd.OutOrStdout(), counts)
	}
	return renderStatusLine(cmd.OutOrStdout(), counts)
}

// countStates buckets each session into one of the canonical state counters.
// Unknown states fall into Idle so the line form remains useful — operators
// can spot the anomaly via the JSON shape if curious.
func countStates(sessions []iris.SessionSnapshot) sessionStateCounts {
	var c sessionStateCounts
	for _, s := range sessions {
		switch s.State {
		case "active":
			c.Active++
		case "waiting":
			c.Waiting++
		case "idle":
			c.Idle++
		case "finished":
			c.Finished++
		case "error":
			c.Error++
		case "spawning":
			c.Spawning++
		default:
			// Unknown — fold into idle for the human line so the total
			// session count is still visible. The JSON form preserves
			// the canonical keys and silently drops this session from
			// the per-state view; this is the same defensive bucket
			// prism uses.
			c.Idle++
		}
	}
	return c
}

// renderStatusJSON writes a single JSON object with integer state counts.
func renderStatusJSON(w io.Writer, c sessionStateCounts) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("iris sessions status --json: marshal: %w", err)
	}
	if _, err := fmt.Fprintln(w, string(data)); err != nil {
		return err
	}
	return nil
}

// renderStatusLine writes a single padded line of `state: count` pairs in the
// canonical order. The canonical set always appears even when zero so the
// columnar alignment is stable across invocations.
func renderStatusLine(w io.Writer, c sessionStateCounts) error {
	// Spawning is folded into the active bucket for the human line —
	// operators care about "is a session in flight" more than the
	// micro-distinction between spawning and active.
	active := c.Active + c.Spawning
	line := fmt.Sprintf("active: %d  waiting: %d  idle: %d  finished: %d  error: %d",
		active, c.Waiting, c.Idle, c.Finished, c.Error)
	if _, err := fmt.Fprintln(w, line); err != nil {
		return err
	}
	return nil
}

// renderStatusWaitingPlain writes just the waiting count (no formatting),
// matching `prism sessions status --waiting`. One line, integer, newline.
func renderStatusWaitingPlain(w io.Writer, c sessionStateCounts) error {
	if _, err := fmt.Fprintln(w, c.Waiting); err != nil {
		return err
	}
	return nil
}

// renderStatusTmux writes the tmux-format segment via the shared
// internal/tmuxstatus package. waitingOnly switches between the
// --waiting --tmux-format and --tmux-format renderers; both return an
// empty string when there's nothing to display, which is the desired
// status-right behaviour.
//
// Spawning is folded into Active here to match the human-line semantics:
// the status bar's "active" pip should include sessions still coming up,
// because the operator-visible distinction is "is something in flight" vs
// "is something waiting on me".
func renderStatusTmux(w io.Writer, c sessionStateCounts, waitingOnly bool) error {
	tc := tmuxstatus.Counts{
		Active:   c.Active + c.Spawning,
		Waiting:  c.Waiting,
		Idle:     c.Idle,
		Finished: c.Finished,
		Error:    c.Error,
	}
	cols := tmuxStatusColors()
	var s string
	if waitingOnly {
		s = tmuxstatus.FormatWaiting(tc, cols)
	} else {
		s = tmuxstatus.Format(tc, cols)
	}
	if s == "" {
		return nil
	}
	if _, err := fmt.Fprint(w, s); err != nil {
		return err
	}
	return nil
}

// tmuxStatusColors loads the shared prism colour palette via internal/config
// so the iris status-right segment matches the prism segment exactly. The
// config loader returns gruvbox-dark defaults when no config file is
// present, so this works in `go test`, `go build`, and Nix-built binaries
// alike. Loaded per-call (rather than at init time) so the iris CLI does
// not pay the JSON-decode cost on commands that don't need colours; the
// tmux-format command is the only caller.
func tmuxStatusColors() tmuxstatus.Colors {
	cfg := config.Load()
	return tmuxstatus.Colors{
		Yellow:  cfg.ColorYellow,
		Purple:  cfg.ColorPurple,
		Green:   cfg.ColorGreen,
		Red:     cfg.ColorRed,
		Primary: cfg.ColorPrimary,
	}
}
