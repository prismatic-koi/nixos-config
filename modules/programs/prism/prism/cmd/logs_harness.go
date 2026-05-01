package cmd

// prism logs --harness-events <session> — raw PI JSONL frame archive viewer
// (P5.LOGS / #1218).
//
// The PI sidecar persists every inbound and outbound JSONL frame on a
// socket-pipe session into the harness_frames DB table (see
// internal/sidecar/frame_archive.go). This subcommand replays those frames
// in chronological order — one JSON object per line — and is the primary
// debugging surface for diagnosing PI wire-protocol issues without grepping
// the sidecar log.
//
// Usage:
//
//   prism logs --harness-events <session>
//   prism logs --harness-events <session> --follow
//   prism logs --harness-events <session> --direction in|out
//   prism logs --harness-events <session> --types tool_call,tool_result
//
// Output is intentionally raw (no formatting, no annotation) so it is safe
// to pipe into jq, grep, etc.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// runHarnessEvents implements the `prism logs --harness-events` subcommand.
//
// On a non-PI session (no harness_frames rows recorded) it prints a clear
// hint to stderr and returns a non-zero error so the caller's shell sees an
// exit code distinguishable from "session has zero frames yet" — this matches
// the edge-case AC.
func runHarnessEvents(sessionName, direction, typesCSV string, follow bool, w io.Writer) error {
	if direction != "" && direction != directionIn && direction != directionOut {
		return fmt.Errorf("--direction must be %q or %q (got %q)", directionIn, directionOut, direction)
	}

	types := parseTypesCSV(typesCSV)

	d, err := openDB()
	if err != nil {
		return fmt.Errorf("open DB: %w", err)
	}
	defer d.Close()

	// AC edge-case: a session with zero archived frames is treated as
	// non-PI and we exit non-zero with a clear hint. The CountHarnessFrames
	// short-circuit is necessary because an empty filter (e.g. an unknown
	// --direction) would otherwise yield zero rows and look identical to
	// the non-PI case.
	count, err := d.CountHarnessFrames(sessionName)
	if err != nil {
		return fmt.Errorf("count harness frames: %w", err)
	}
	if count == 0 {
		fmt.Fprintf(os.Stderr,
			"no harness frames recorded for session %q; this is a PI-harness-only feature\n",
			sessionName)
		return errNoHarnessFrames
	}

	frames, err := d.QueryHarnessFrames(sessionName, direction, types, "")
	if err != nil {
		return fmt.Errorf("query harness frames: %w", err)
	}

	// Print the existing frames (one JSONL object per line).
	var lastID string
	for _, f := range frames {
		if _, err := fmt.Fprintln(w, f.Payload); err != nil {
			return fmt.Errorf("write harness frame: %w", err)
		}
		lastID = f.ID
	}

	if !follow {
		return nil
	}

	// --follow: poll every 500ms for new frames after lastID. Exits cleanly
	// on Ctrl-C. We intentionally do not exit on session termination here:
	// frames are persisted and the operator can still tail interesting
	// post-mortem traffic (e.g. the trailing session_shutdown / error frame
	// that arrived after they hit follow).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			next, err := d.QueryHarnessFrames(sessionName, direction, types, lastID)
			if err != nil {
				// A "cursor frame not found" error means the cursor row
				// was pruned out from under us (very unlikely on a 7d
				// retention window during interactive use, but possible).
				// Fall back to a blanket query to keep the stream alive.
				if strings.Contains(err.Error(), "cursor frame") {
					lastID = ""
					continue
				}
				return fmt.Errorf("follow harness frames: %w", err)
			}
			for _, f := range next {
				if _, err := fmt.Fprintln(w, f.Payload); err != nil {
					return fmt.Errorf("write harness frame: %w", err)
				}
				lastID = f.ID
			}
		}
	}
}

// parseTypesCSV splits a comma-separated list, trims whitespace from each
// element, and drops empty entries. Returns nil when the input has no usable
// entries so the DB query treats it as "no type filter".
func parseTypesCSV(csv string) []string {
	if csv == "" {
		return nil
	}
	raw := strings.Split(csv, ",")
	out := raw[:0]
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// errNoHarnessFrames is returned by runHarnessEvents when the requested
// session has zero harness_frames rows. The CLI surface treats this as a
// non-zero exit code while keeping the user-facing message on stderr.
//
// Defined as a package-level sentinel so test code can assert on it via
// errors.Is.
var errNoHarnessFrames = noHarnessFramesError{}

type noHarnessFramesError struct{}

func (noHarnessFramesError) Error() string {
	return "no harness frames recorded for this session"
}

// directionIn / directionOut mirror the constants in internal/db so the
// CLI doesn't need to import the db package's literals into help text.
const (
	directionIn  = "in"
	directionOut = "out"
)
