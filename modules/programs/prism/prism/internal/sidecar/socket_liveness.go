package sidecar

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// duplicateStartDialTimeout is how long the duplicate-start check waits for a
// dial against an existing Unix socket. Kept short (250ms) so a hung path —
// for example, a socket whose owning process is paused in a debugger — does
// not stall sidecar startup. The accept loop on the remote side responds
// immediately when a sidecar is genuinely alive. ECONNREFUSED from a
// tombstone returns near-instantly without consuming the timeout budget.
const duplicateStartDialTimeout = 250 * time.Millisecond

// duplicateStartError is returned when another sidecar process is detected
// owning one of this session's Unix socket paths. The caller is expected to
// surface it as a fatal startup error and exit non-zero without touching the
// socket file or writing to the database.
type duplicateStartError struct {
	SockPath    string
	SessionName string
	SockKind    string // "hostapi.sock" or "pipe.sock"
}

func (e *duplicateStartError) Error() string {
	return fmt.Sprintf(
		"sidecar: another instance is already running for session %q (%s at %s is responsive); refusing to start",
		e.SessionName, e.SockKind, e.SockPath,
	)
}

// socketLiveness describes the disposition of a Unix socket path at sidecar
// startup. Tri-state: live (owned by another process), tombstone (file exists
// but no listener), or absent.
type socketLiveness int

const (
	socketAbsent socketLiveness = iota
	socketTombstone
	socketLive
)

// checkSocketLiveness probes sockPath to determine whether another sidecar
// process is currently listening on it.
//
//   - If the path does not exist, returns socketAbsent. Safe to bind.
//   - If the dial fails with ECONNREFUSED (or equivalent) and the file still
//     exists, returns socketTombstone. Safe to os.Remove and bind.
//   - If the dial succeeds, the remote end is live. The connection is closed
//     immediately and socketLive is returned. The caller MUST refuse to start
//     and MUST NOT touch the socket file.
//   - Any other dial error (for example, permission denied or timeout) is
//     returned as-is. The caller must treat this as a fatal startup error
//     rather than blindly removing the socket — a remove can clobber a live
//     sidecar whose accept loop merely happened to be slow.
//
// sockPath is the absolute filesystem path of the Unix socket to probe.
func checkSocketLiveness(sockPath string) (socketLiveness, error) {
	if _, statErr := os.Stat(sockPath); statErr != nil {
		// Any stat failure (ENOENT, ENOTDIR on a malformed parent, EACCES) is
		// treated as "no live sidecar to refuse against" — the downstream
		// net.Listen path will surface its own clear error if the path is
		// genuinely unusable. This intentionally keeps the duplicate-start
		// guard narrow: it refuses only on positive proof of a remote
		// listener, never when probing was inconclusive. The cost of a false
		// negative here is a normal bind failure one step later. The cost of a
		// false positive is a refusal to start when no other sidecar exists.
		return socketAbsent, nil
	}

	// File exists. Dial it with a short timeout to determine whether a
	// listener is alive on the other end.
	conn, dialErr := net.DialTimeout("unix", sockPath, duplicateStartDialTimeout)
	if dialErr == nil {
		_ = conn.Close()
		return socketLive, nil
	}

	// ECONNREFUSED with the file present is the tombstone signature — same
	// shape isStaleTombstoneSocket uses on the client side
	// (internal/promptdelivery/promptdelivery.go).
	if errors.Is(dialErr, syscall.ECONNREFUSED) {
		return socketTombstone, nil
	}

	// Any other dial error (timeout, permission denied, and others) is also
	// treated as inconclusive — same rationale as the stat case above.
	// The bind path will fail loudly if the path is truly unusable.
	return socketAbsent, fmt.Errorf("dial %q: %w", sockPath, dialErr)
}

// checkNoLiveSidecar verifies that no other sidecar process is currently
// owning sockPath for sessionName. On socketLive it returns a
// *duplicateStartError; on socketTombstone or socketAbsent it returns nil.
//
// Inconclusive probe failures (stat or dial errors that are neither
// ENOENT-style nor ECONNREFUSED) are deliberately treated as "no live
// sidecar found": the guard refuses only on positive proof. Such errors
// are logged via the optional logf callback so they are visible in the
// sidecar log, but startup continues so the downstream bind path can
// surface its own clear failure if the path is genuinely unusable.
//
// sockKind is a short label ("hostapi.sock" or "pipe.sock") used in the
// error message so logs identify which path was contested.
func checkNoLiveSidecar(sockPath, sessionName, sockKind string, logf func(format string, args ...any)) error {
	if sockPath == "" {
		return nil
	}
	state, err := checkSocketLiveness(sockPath)
	if err != nil && logf != nil {
		logf("sidecar: duplicate-start probe for %s at %q inconclusive (continuing to bind): %v", sockKind, sockPath, err)
	}
	if state == socketLive {
		return &duplicateStartError{
			SockPath:    sockPath,
			SessionName: sessionName,
			SockKind:    sockKind,
		}
	}
	return nil
}
