// Package promptdelivery provides a harness-aware prompt delivery helper used
// by all out-of-process callers that need to send a follow-up prompt to a
// running agent session.
//
// # Test-mode isolation guard
//
// When the environment variable PRISM_TEST_MODE_RESTRICT_HOSTAPI is set to any
// non-empty value, deliverViaSidecarSocket refuses to dial any socket path that
// does not reside under the process's $XDG_STATE_HOME directory. This prevents
// test code from accidentally dialling a live coordinator's host-API socket
// when a test's XDG_STATE_HOME is redirected to a tempdir but an incorrect
// socket path is computed. The guard is set automatically by
// sidecartest.NewIsolated and must never be set in production.
//
// # Delivery
//
// DeliverToSession POSTs {"session":…,"prompt":…} to the host-API Unix
// socket of the target session at /prompt. The /prompt handler of the
// sidecar forwards the text to the pi process via DeliverPrompt. Only a
// session whose harness has the socket-pipe shape (pi) has that path. For
// each other session, DeliverToSession returns an error and sends nothing.
//
// All callers must pass the result of db.CurrentStatus for the target session.
// The caller is responsible for pre-flight checks (session found, not ended, etc).
package promptdelivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/hostapiauth"
	"github.com/prismatic-koi/prism/internal/session"
)

// DeliveryOutcome is the summary result of a /prompt round trip. It
// surfaces the sidecar's response envelope to the caller so the CLI can
// distinguish a synchronous delivery from a buffered one and act
// accordingly.
//
// A zero-value DeliveryOutcome (Buffered=false, Replayed=false) means
// synchronous success.
type DeliveryOutcome struct {
	// Buffered is true when the sidecar's /prompt handler returned
	// {"buffered": true} — the delivery was accepted but the PI extension
	// was disconnected at the time, so the frame will be flushed on the
	// next successful handshake (marked replay=true on the wire). Callers
	// MUST surface this so operators can tell the difference between "on
	// the wire" and "parked awaiting reconnect".
	Buffered bool

	// Replayed is true when the sidecar's /prompt handler returned
	// {"replayed": true} — the delivery was dropped as a repeat of a
	// previously-seen delivery_id in the sidecar's dedup ledger.
	// Existing sender-side dedup UX (e.g. `prism escalate`'s
	// "OK already delivered" line) does its own bookkeeping; this
	// field surfaces the receiving-sidecar decision for the direct
	// `prism prompt` path.
	Replayed bool

	// DeliveryID is the delivery_id sent in the request. Callers may want
	// to include it in their user-facing output for operator debugging.
	DeliveryID string
}

// DeliverToSession delivers text to the agent session described by status.
//
// It dials the host-API Unix socket of the session and POSTs /prompt. The
// socket path is derived from sessionName via session.SidecarHostAPIPath. It
// returns an error, and sends nothing, when the harness of the session does
// not have the socket-pipe shape.
//
// status must be non-nil. The caller is expected to have already verified that
// the session is active (EndedAt == nil) before calling this function.
//
// source identifies the logical origin of the delivery. When non-empty it is
// forwarded to the sidecar's /prompt handler via the "source" JSON field. The
// sidecar uses this to gate reviewingInFlight clearing: only
// source="review-complete" (the monitor's delivery) clears the flag; all other
// deliveries leave it unchanged. Pass "" for non-monitor callers.
//
// deliverAs controls the prompt delivery mode. It is forwarded as the
// "deliver_as" JSON field to the sidecar's /prompt handler, which validates
// and passes it through to DeliverPrompt. Accepted values: "steer",
// "followUp", "nextTurn". Empty string defaults to "nextTurn".
//
// The returned DeliveryOutcome is populated when the transport surfaces
// a distinguishing signal (buffered / replayed); callers that only need the
// error can discard it.
func DeliverToSession(sessionName string, status *db.Status, text string, source string, deliverAs string) error {
	_, err := DeliverToSessionEx(sessionName, status, text, source, deliverAs)
	return err
}

// DeliverToSessionEx is the DeliveryOutcome-returning variant of
// DeliverToSession. Callers who need to surface "buffered" vs
// "synchronous" or dedup-replay to their own users (e.g. the
// `prism prompt` CLI) should use this form. Mints a fresh delivery_id
// so the receiving sidecar can dedup repeats at the bus boundary.
func DeliverToSessionEx(sessionName string, status *db.Status, text string, source string, deliverAs string) (DeliveryOutcome, error) {
	return DeliverToSessionExWithID(sessionName, status, text, source, deliverAs, uuid.NewString())
}

// DeliverToSessionWithID is the explicit-ID variant of DeliverToSession.
// The deliveryID is forwarded as the `delivery_id` JSON field on /prompt
// requests so the receiving sidecar can dedup retries. Pass "" to skip
// dedup (legacy behaviour) — the receiving sidecar will then deliver the
// frame unconditionally.
func DeliverToSessionWithID(sessionName string, status *db.Status, text string, source string, deliverAs string, deliveryID string) error {
	_, err := DeliverToSessionExWithID(sessionName, status, text, source, deliverAs, deliveryID)
	return err
}

// DeliverToSessionExWithID is the DeliveryOutcome-returning + explicit-ID
// variant of DeliverToSession. This is the single point where the receiving
// sidecar's response envelope (buffered / replayed) is parsed and surfaced
// to callers.
func DeliverToSessionExWithID(sessionName string, status *db.Status, text string, source string, deliverAs string, deliveryID string) (DeliveryOutcome, error) {
	name := ""
	if status.Harness != nil {
		name = *status.Harness
	}
	if shape, ok := harness.ShapeOf(name); !ok || shape != harness.TransportSocketPipe {
		return DeliveryOutcome{DeliveryID: deliveryID}, fmt.Errorf("deliver to session %q: harness %q has no socket pipe, so prism cannot deliver a prompt to it", sessionName, name)
	}
	return deliverViaSidecarSocket(sessionName, text, source, deliverAs, deliveryID)
}

// deliverViaSidecarSocket dials the target session's host-API Unix socket and
// POSTs /prompt with {"session": sessionName, "prompt": text, "source": source,
// "deliver_as": deliverAs}.
//
// The sidecar's /prompt handler checks that req.Session == s.cfg.SessionName
// and that the harness shape is TransportSocketPipe, then calls s.DeliverPrompt
// to write the text to the pi process's stdin pipe.
//
// source is forwarded as the "source" JSON field. The sidecar uses it to gate
// reviewingInFlight clearing: only source="review-complete" clears the flag.
// Pass "" for non-monitor callers (the field is omitted when empty).
//
// deliverAs is forwarded as the "deliver_as" JSON field. Pass "followUp" for
// post-turn notifications (coordinator notify, merge-queue outcome, startup
// failure). Pass "" to default to "nextTurn" (existing caller behaviour).
// The sidecar validates the value and rejects unknown strings with HTTP 400.
//
// Returns an error if the socket does not exist (session ended / socket cleaned
// up) or the HTTP request fails.
// EnvRestrictHostAPI is the env-var name of the test-mode isolation guard.
// When set to a non-empty value, deliverViaSidecarSocket refuses to dial
// socket paths outside the current XDG_STATE_HOME — see package-level comment.
const EnvRestrictHostAPI = "PRISM_TEST_MODE_RESTRICT_HOSTAPI"

// sidecarHostAPIPathFn resolves a session name to its host-API socket path.
// Defaults to session.SidecarHostAPIPath; tests may override this to inject a
// path outside $XDG_STATE_HOME so they can exercise the test-mode isolation
// guard. Keep this seam minimal — it exists only to make the guard branch
// testable via the public API.
var sidecarHostAPIPathFn = session.SidecarHostAPIPath

func deliverViaSidecarSocket(sessionName, text, source, deliverAs, deliveryID string) (DeliveryOutcome, error) {
	outcome := DeliveryOutcome{DeliveryID: deliveryID}
	sockPath, err := sidecarHostAPIPathFn(sessionName)
	if err != nil {
		return outcome, fmt.Errorf("resolve host-API socket path for %q: %w", sessionName, err)
	}

	// Test-mode isolation guard: when PRISM_TEST_MODE_RESTRICT_HOSTAPI is set,
	// refuse to dial any socket that does not live under $XDG_STATE_HOME. This
	// prevents a test from accidentally reaching a live coordinator socket when
	// XDG_STATE_HOME is redirected to a tempdir (the hash of the session name
	// is identical between test and production, so the path collision is real).
	// The guard is set automatically by sidecartest.NewIsolated.
	if os.Getenv(EnvRestrictHostAPI) != "" {
		stateHome := os.Getenv("XDG_STATE_HOME")
		if stateHome != "" && !hasPathPrefix(sockPath, stateHome) {
			return outcome, fmt.Errorf(
				"test-mode isolation guard: refusing to dial host-API socket at %s — path is outside XDG_STATE_HOME=%s (set PRISM_TEST_MODE_RESTRICT_HOSTAPI only in tests)",
				sockPath, stateHome,
			)
		}
	}

	if _, statErr := os.Stat(sockPath); statErr != nil {
		return outcome, fmt.Errorf("host-API socket not found at %s (session may have ended): %w", sockPath, statErr)
	}

	client := newUnixClient(sockPath)

	bodyMap := map[string]string{
		"session": sessionName,
		"prompt":  text,
	}
	if source != "" {
		bodyMap["source"] = source
	}
	if deliverAs != "" {
		bodyMap["deliver_as"] = deliverAs
	}
	if deliveryID != "" {
		bodyMap["delivery_id"] = deliveryID
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		return outcome, fmt.Errorf("marshal prompt body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, "http://prism-hostapi/prompt", bytes.NewReader(body))
	if err != nil {
		return outcome, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return outcome, fmt.Errorf("dial host-API socket at %s: %w", sockPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return outcome, fmt.Errorf("host-API /prompt returned HTTP %d for session %q: %s",
			resp.StatusCode, sessionName, errorBodySnippet(resp.Body))
	}

	// Parse the response envelope to surface {"buffered": true} /
	// {"replayed": true} to the caller. An
	// empty or unparseable body is not an error — the sidecar
	// replies {} on synchronous success. Cap the read to a small
	// upper-bound because the sidecar's response envelope is at most a
	// few dozen bytes; a runaway body indicates a client-side bug rather
	// than a legitimate response.
	env := struct {
		Buffered bool `json:"buffered"`
		Replayed bool `json:"replayed"`
	}{}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&env)
	outcome.Buffered = env.Buffered
	outcome.Replayed = env.Replayed
	return outcome, nil
}

// hasPathPrefix reports whether path p is equal to or a sub-path of prefix.
// Both paths are cleaned before comparison so trailing slashes are normalised.
func hasPathPrefix(p, prefix string) bool {
	cleanP := filepath.Clean(p)
	cleanPfx := filepath.Clean(prefix)
	if cleanP == cleanPfx {
		return true
	}
	// cleanP must start with cleanPfx + "/" to be a sub-path.
	return len(cleanP) > len(cleanPfx) && cleanP[len(cleanPfx)] == '/' &&
		cleanP[:len(cleanPfx)] == cleanPfx
}

// newUnixClient returns an *http.Client that dials sockPath over a Unix socket.
// Each request carries the host-API secret of the target session, which the
// host reads from hostapi.secret next to sockPath (hostapiauth, #3379). The
// dial refuses a sockPath that is a symlink.
//
// Tombstone hygiene: when the dial returns ECONNREFUSED and the socket file
// exists on disk, the sidecar has exited abnormally without removing the
// socket. A clearer diagnostic is surfaced in that case.
func newUnixClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: hostapiauth.UnixTransport(sockPath, func(ctx context.Context) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			conn, err := hostapiauth.DialUnix(ctx, d, sockPath)
			if err != nil {
				if isStaleTombstoneSocket(sockPath, err) {
					return nil, fmt.Errorf("host-API socket at %s is a stale tombstone — sidecar has exited without cleanup (ECONNREFUSED on existing socket file): %w", sockPath, err)
				}
				return nil, fmt.Errorf("host-API socket not available at %s: %w", sockPath, err)
			}
			return conn, nil
		}),
		Timeout: 10 * time.Second,
	}
}

// isStaleTombstoneSocket returns true when dialErr is ECONNREFUSED and the
// socket file at sockPath still exists on disk — the "tombstone" case where
// the sidecar exited abnormally without removing the socket file.
func isStaleTombstoneSocket(sockPath string, dialErr error) bool {
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return false
	}
	_, statErr := os.Stat(sockPath)
	return statErr == nil
}

// errorBodyLimit caps how many bytes of a non-2xx response body an error
// carries.
const errorBodyLimit = 512

// errorBodySnippet reads at most errorBodyLimit bytes of r and returns them
// as one line: whitespace runs collapse to one space, and a body that is cut
// short ends with "...". An empty body returns "(empty body)".
func errorBodySnippet(r io.Reader) string {
	buf, _ := io.ReadAll(io.LimitReader(r, errorBodyLimit+1))
	truncated := len(buf) > errorBodyLimit
	if truncated {
		// buf[errorBodyLimit] is the first dropped byte. If it continues a
		// multi-byte rune, back up so the cut does not split that rune.
		cut := errorBodyLimit
		for cut > 0 && !utf8.RuneStart(buf[cut]) {
			cut--
		}
		buf = buf[:cut]
	}
	s := strings.ToValidUTF8(strings.Join(strings.Fields(string(buf)), " "), "?")
	if s == "" {
		return "(empty body)"
	}
	if truncated {
		s += "..."
	}
	return s
}
