package main

// sessions_status.go — `iris sessions status` implementation.
//
// Prints state counts derived from the iris daemon's sessions_snapshot.
// Without --json: one human-readable line padded across the canonical state
// set (visual stability across invocations matters for shell aliases that
// align several lines side by side).
// With --json: a single JSON object keyed by state with integer counts.
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

	"github.com/prismatic-koi/prism/internal/iris"
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
	sockPath := resolveSocketPath(cmd)

	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()

	snap, err := fetchSessionsSnapshot(ctx, sockPath)
	if err != nil {
		return err
	}

	counts := countStates(snap.Sessions)
	if jsonMode {
		return renderStatusJSON(cmd.OutOrStdout(), counts)
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
