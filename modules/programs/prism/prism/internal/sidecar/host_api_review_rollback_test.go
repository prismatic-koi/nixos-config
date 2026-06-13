package sidecar

// host_api_review_rollback_test.go — issue #2258.
//
// Regression tests for the /review handler's pre-emptive `reviewing` state
// write. Pre-#2258 the handler wrote `reviewing` to the DB and set
// reviewingInFlight=true BEFORE decoding/validating the request body, and
// did not roll back on any post-write failure (host-side child refusal,
// subprocess spawn failure, etc.). A refused /review therefore left the
// calling worker pinned in `reviewing`, suppressing both finished-debounce
// notifications (handleSessionFinished, events.go) and turn_start active
// writes (handlePipeFrame), so the worker silently wedged until a
// successful review-complete prompt arrived. When the worker could not
// re-run review (e.g. a stuck companion group), the wedge was indefinite.
//
// The fix splits the failure modes:
//
//   - Validation errors (bad JSON, bad pr_number, unknown agent) now fire
//     BEFORE the pre-emptive write, so nothing to roll back.
//   - Subprocess failures (Start error, Wait non-zero exit — i.e. host-side
//     refusal: behind-`origin/main` gate, round-already-in-progress, fetch
//     failure) trigger an in-handler rollback that restores prevStatus.State
//     in the DB, clears reviewingInFlight, and pushes reviewing_state{false}
//     to the PI extension.
//
// # Isolation contract (#1608)
//
// Every test in this file constructs its Sidecar via
// sidecartest.NewIsolated(t, ""), so:
//   - XDG_STATE_HOME points at a t.TempDir(); host paths never escape the
//     test sandbox.
//   - PRISM_TEST_MODE_RESTRICT_HOSTAPI=1 prevents promptdelivery from
//     dialling a real host socket.
//   - The DB is an isolated SQLite file under the test tempdir; the test
//     session name uses the "prism-test@" prefix so a leaked write cannot
//     collide with any live coordinator on the developer's host.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// newReviewRollbackSidecar constructs an isolated sidecar suitable for
// driving the /review handler directly via doHostAPI. The bus's isolated
// DB is reused as the sidecar's DB, and a stub prism binary is installed
// at PrismBinaryPath so cmd.Start() succeeds without touching the real
// prism executable. The stub's body is the caller's responsibility — pass
// a `#!/bin/sh\n...` script that exits 0 (success) or non-zero (refusal)
// as the test requires.
//
// The session's agent_status row is seeded with state=active (the typical
// pre-review state) so the /review handler's pre-emptive write path is
// exercised — the bug under test only manifests when an agent_status row
// exists for the calling session.
func newReviewRollbackSidecar(t *testing.T, stubBody string) (*Sidecar, *db.DB, string) {
	t.Helper()

	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	if err := os.WriteFile(stubPath, []byte(stubBody), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}

	bus := sidecartest.NewIsolated(t, "")
	sessionName := "prism-test@review-rollback-" + sanitiseTestName(t.Name())
	repo := "prism-test"
	worktree := t.TempDir()
	if err := bus.DB.UpsertStatus(sessionName, repo, worktree, string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("seed agent_status row for %q: %v", sessionName, err)
	}

	clk := newTestClock()
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        worktree,
		HarnessURL:      "http://localhost:14000",
		DB:              bus.DB,
		Clock:           clk,
		AgentRole:       "worker",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	return New(cfg), bus.DB, sessionName
}

// sanitiseTestName turns t.Name() into a string safe to embed in a session
// name: replaces "/" (subtest separator) with "-". The result is folded
// into "prism-test@review-rollback-<sanitised>" by the caller so each
// subtest uses its own session row in the isolated DB.
func sanitiseTestName(name string) string {
	return strings.ReplaceAll(name, "/", "-")
}

// stateOf reads agent_status.state for sessionName from d, failing the
// test on lookup error or missing row.
func stateOf(t *testing.T, d *db.DB, sessionName string) string {
	t.Helper()
	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		t.Fatalf("CurrentStatus(%q): %v", sessionName, err)
	}
	if st == nil {
		t.Fatalf("CurrentStatus(%q): row missing", sessionName)
	}
	return st.State
}

// reviewingInFlightOf returns the value of s.reviewingInFlight under s.mu.
func reviewingInFlightOf(s *Sidecar) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reviewingInFlight
}

// ── AC #1: validation failures preserve pre-call state and flag ─────────────

// TestReviewRollback_BadJSON_NoStateChange covers AC #1: a /review request
// with malformed JSON returns 400 and leaves both agent_status.state and
// s.reviewingInFlight exactly as they were before the call. Pre-fix this
// test FAILS because the handler wrote `reviewing` before decoding the
// body — the assertion that state remains "active" and that the flag is
// false would not hold.
func TestReviewRollback_BadJSON_NoStateChange(t *testing.T) {
	sc, d, sessionName := newReviewRollbackSidecar(t, "#!/bin/sh\nexit 0\n")

	if got := stateOf(t, d, sessionName); got != string(agent.StateActive) {
		t.Fatalf("pre-call state = %q, want %q", got, agent.StateActive)
	}
	if reviewingInFlightOf(sc) {
		t.Fatalf("pre-call reviewingInFlight = true, want false")
	}

	rr := doHostAPI(t, sc, http.MethodPost, "/review", `{not valid json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}

	if got := stateOf(t, d, sessionName); got != string(agent.StateActive) {
		t.Errorf("post-call state = %q, want %q (unchanged)", got, agent.StateActive)
	}
	if reviewingInFlightOf(sc) {
		t.Errorf("post-call reviewingInFlight = true, want false (unchanged)")
	}
}

// TestReviewRollback_BadPRNumber_NoStateChange covers AC #1 for the
// non-numeric pr_number case. The validation now runs before the
// pre-emptive write, so the rejection must leave state untouched.
func TestReviewRollback_BadPRNumber_NoStateChange(t *testing.T) {
	sc, d, sessionName := newReviewRollbackSidecar(t, "#!/bin/sh\nexit 0\n")

	rr := doHostAPI(t, sc, http.MethodPost, "/review", `{"pr_number":"--keep"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "pr_number must be a numeric string") {
		t.Errorf("body = %q, want 'pr_number must be a numeric string' error", rr.Body.String())
	}

	if got := stateOf(t, d, sessionName); got != string(agent.StateActive) {
		t.Errorf("post-call state = %q, want %q (unchanged)", got, agent.StateActive)
	}
	if reviewingInFlightOf(sc) {
		t.Errorf("post-call reviewingInFlight = true, want false (unchanged)")
	}
}

// TestReviewRollback_UnknownAgent_NoStateChange covers AC #1 for the
// unknown-agent-name case. The handler must reject the bogus agent name
// before any state mutation.
func TestReviewRollback_UnknownAgent_NoStateChange(t *testing.T) {
	sc, d, sessionName := newReviewRollbackSidecar(t, "#!/bin/sh\nexit 0\n")

	rr := doHostAPI(t, sc, http.MethodPost, "/review",
		`{"pr_number":"123","agents":["review-bogus"]}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unknown agent name") {
		t.Errorf("body = %q, want 'unknown agent name' error", rr.Body.String())
	}

	if got := stateOf(t, d, sessionName); got != string(agent.StateActive) {
		t.Errorf("post-call state = %q, want %q (unchanged)", got, agent.StateActive)
	}
	if reviewingInFlightOf(sc) {
		t.Errorf("post-call reviewingInFlight = true, want false (unchanged)")
	}
}

// TestReviewRollback_MissingPRNumber_NoStateChange covers AC #1 for the
// missing pr_number case. This path runs BEFORE the numeric-check but
// AFTER decode, so it likewise must not touch state.
func TestReviewRollback_MissingPRNumber_NoStateChange(t *testing.T) {
	sc, d, sessionName := newReviewRollbackSidecar(t, "#!/bin/sh\nexit 0\n")

	rr := doHostAPI(t, sc, http.MethodPost, "/review", `{}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}

	if got := stateOf(t, d, sessionName); got != string(agent.StateActive) {
		t.Errorf("post-call state = %q, want %q (unchanged)", got, agent.StateActive)
	}
	if reviewingInFlightOf(sc) {
		t.Errorf("post-call reviewingInFlight = true, want false (unchanged)")
	}
}

// ── AC #2: host-side child refusal rolls back state, flag, and pi push ──────

// TestReviewRollback_ChildRefusal_RollsBackStateAndFlag covers AC #2: a
// /review request that passes validation but whose subprocess (the
// host-side `prism review`) exits non-zero — modelling the behind-main
// gate refusal, round-already-in-progress refusal, fetch failure, etc. —
// must leave the calling session's state and reviewingInFlight exactly as
// they were before the request, and the pi extension must receive a
// reviewing_state{false} frame so its pendingReviewCall guard releases
// in lock-step.
//
// The stub binary prints a diagnostic line to stderr (mimicking the
// in-progress guard's error shape) and exits 1.
func TestReviewRollback_ChildRefusal_RollsBackStateAndFlag(t *testing.T) {
	const refusalStub = `#!/bin/sh
echo "prism review: round 1 is already in progress for this PR" >&2
exit 1
`
	sc, d, sessionName := newReviewRollbackSidecar(t, refusalStub)

	// Wire up an outbound pipe channel so we can observe the
	// reviewing_state frames the handler emits to the PI extension. The
	// channel is sized to comfortably hold the set-true + clear-false
	// pair plus headroom for any sequencing slop.
	pipeCh := make(chan []byte, 16)
	sc.mu.Lock()
	sc.harnessPipeOutCh = pipeCh
	sc.mu.Unlock()

	if got := stateOf(t, d, sessionName); got != string(agent.StateActive) {
		t.Fatalf("pre-call state = %q, want %q", got, agent.StateActive)
	}

	rr := doHostAPI(t, sc, http.MethodPost, "/review", `{"pr_number":"123"}`)
	// The handler returns 200 with a streamed body terminated by the
	// failure sentinel — the HTTP code does not encode the subprocess
	// exit. proxyReviewAsync (the client side) consumes the sentinel and
	// surfaces the failure to the agent.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (failure conveyed via sentinel); body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), ReviewSentinelFailed) {
		t.Errorf("body %q does not contain failure sentinel %q", rr.Body.String(), ReviewSentinelFailed)
	}

	// Rollback must restore the pre-call state (active) and clear the flag.
	if got := stateOf(t, d, sessionName); got != string(agent.StateActive) {
		t.Errorf("post-call state = %q, want %q (rolled back)", got, agent.StateActive)
	}
	if reviewingInFlightOf(sc) {
		t.Errorf("post-call reviewingInFlight = true, want false (rolled back)")
	}

	// Drain reviewing_state frames from the pipe. We expect at minimum
	// one in_flight=true (the pre-emptive write) and one in_flight=false
	// (the rollback). Frames may be interleaved with prompt/other frames
	// in principle, so we filter by type rather than asserting on a
	// fixed sequence.
	deadline := time.Now().Add(500 * time.Millisecond)
	var sawTrue, sawFalse bool
	for time.Now().Before(deadline) && (!sawTrue || !sawFalse) {
		select {
		case f := <-pipeCh:
			str := string(f)
			if !strings.Contains(str, `"type":"reviewing_state"`) {
				continue
			}
			if strings.Contains(str, `"in_flight":true`) {
				sawTrue = true
			}
			if strings.Contains(str, `"in_flight":false`) {
				sawFalse = true
			}
		case <-time.After(20 * time.Millisecond):
			// keep polling until the outer deadline
		}
	}
	if !sawTrue {
		t.Errorf("did not observe reviewing_state{in_flight:true} on the pipe before rollback")
	}
	if !sawFalse {
		t.Errorf("did not observe reviewing_state{in_flight:false} on the pipe after rollback — pi extension's pendingReviewCall guard would not release in lock-step")
	}
}

// TestReviewRollback_SubprocessSpawnFailure_RollsBackStateAndFlag covers
// the cmd.Start() failure path: the stub binary does not exist, so the
// subprocess never spawns, and the handler returns 500. Rollback must
// still fire even though the failure happened before any HTTP body bytes
// were streamed.
func TestReviewRollback_SubprocessSpawnFailure_RollsBackStateAndFlag(t *testing.T) {
	// Build a Config that points PrismBinaryPath at a non-existent path
	// so cmd.Start() fails with "no such file or directory" before any
	// subprocess output is produced.
	bus := sidecartest.NewIsolated(t, "")
	sessionName := "prism-test@review-rollback-spawn-fail"
	repo := "prism-test"
	worktree := t.TempDir()
	if err := bus.DB.UpsertStatus(sessionName, repo, worktree, string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("seed agent_status row: %v", err)
	}
	clk := newTestClock()
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        worktree,
		HarnessURL:      "http://localhost:14000",
		DB:              bus.DB,
		Clock:           clk,
		AgentRole:       "worker",
		PrismBinaryPath: filepath.Join(t.TempDir(), "does-not-exist"),
		Harness:         newSSEHarness(),
	}
	sc := New(cfg)

	rr := doHostAPI(t, sc, http.MethodPost, "/review", `{"pr_number":"123"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (subprocess start failure); body = %s", rr.Code, rr.Body.String())
	}

	if got := stateOf(t, bus.DB, sessionName); got != string(agent.StateActive) {
		t.Errorf("post-call state = %q, want %q (rolled back)", got, agent.StateActive)
	}
	if reviewingInFlightOf(sc) {
		t.Errorf("post-call reviewingInFlight = true, want false (rolled back)")
	}
}

// ── AC #3: post-rollback, finished-debounce / turn_start behave normally ────

// TestReviewRollback_AfterRefusal_FinishedDebounceFires covers AC #3:
// after a refused /review, the worker's finished-debounce path must run
// normally (no reviewing-based suppression). Both suppression guards in
// events.go (handleSessionFinished, handleSessionIdle) gate on
// s.reviewingInFlight; this test asserts the flag is clear, which is the
// precondition for those guards to release.
//
// The handlePipeFrame turn_start guard (sidecar.go ~line 2456) also gates
// on s.reviewingInFlight: with the flag cleared, an inbound turn_start
// frame produces the normal active write rather than being skipped.
//
// Verified end-to-end via the same s.reviewingInFlight invariant: every
// suppression in the codebase reads this flag, so clearing it restores
// every suppressed path simultaneously. The unit assertion here is on
// the invariant rather than on each downstream effect.
func TestReviewRollback_AfterRefusal_FinishedDebounceFires(t *testing.T) {
	const refusalStub = `#!/bin/sh
echo "prism review: round 1 is already in progress for this PR" >&2
exit 1
`
	sc, d, sessionName := newReviewRollbackSidecar(t, refusalStub)

	rr := doHostAPI(t, sc, http.MethodPost, "/review", `{"pr_number":"123"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	// AC #3 — both suppression paths (handleSessionFinished and
	// handleSessionIdle) and the turn_start guard read
	// s.reviewingInFlight before checking anything else. With the flag
	// cleared by rollback, none of those paths suppress.
	if reviewingInFlightOf(sc) {
		t.Fatalf("reviewingInFlight = true after refused /review — finished-debounce and turn_start handling would remain suppressed")
	}

	// State must also be restored — the handleSessionIdle path
	// (SSE-harness) additionally ANDs reviewingInFlight with
	// currentDBState() == StateReviewing.
	if got := stateOf(t, d, sessionName); got != string(agent.StateActive) {
		t.Errorf("post-rollback state = %q, want %q — handleSessionIdle's AND-guard would still trigger", got, agent.StateActive)
	}
}

// ── AC #4: successful /review immediately after refused behaves normally ────

// TestReviewRollback_RefusedThenSuccess covers AC #4: a successful
// /review issued immediately after a refused one must behave identically
// to a first-time /review — no double-set or stale-clear interactions.
//
// We model "host child refused" with a stub that exits 1 on the first
// invocation and 0 on subsequent ones, by checkpointing via a sentinel
// file. After the first refusal rolls back, the second call must again
// transition the session to reviewing and leave the flag set for the
// monitor to clear later via the /prompt review-complete path.
func TestReviewRollback_RefusedThenSuccess(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	sessionName := "prism-test@review-rollback-refused-then-success"
	repo := "prism-test"
	worktree := t.TempDir()
	if err := bus.DB.UpsertStatus(sessionName, repo, worktree, string(agent.StateActive), nil, nil); err != nil {
		t.Fatalf("seed agent_status row: %v", err)
	}

	// Bi-modal stub: first invocation exits 1, all subsequent exit 0.
	// The sentinel file is created on first invocation and persists for
	// the lifetime of the test tempdir.
	stubDir := t.TempDir()
	sentinelPath := filepath.Join(stubDir, "first-call-done")
	stubPath := filepath.Join(stubDir, "prism-stub")
	stubBody := `#!/bin/sh
if [ -e "` + sentinelPath + `" ]; then
  echo "second-call: success"
  exit 0
fi
touch "` + sentinelPath + `"
echo "first-call: refused" >&2
exit 1
`
	if err := os.WriteFile(stubPath, []byte(stubBody), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	clk := newTestClock()
	cfg := Config{
		SessionName:     sessionName,
		Repo:            repo,
		Worktree:        worktree,
		HarnessURL:      "http://localhost:14000",
		DB:              bus.DB,
		Clock:           clk,
		AgentRole:       "worker",
		PrismBinaryPath: stubPath,
		Harness:         newSSEHarness(),
	}
	sc := New(cfg)

	// First call — refused, must roll back.
	rr := doHostAPI(t, sc, http.MethodPost, "/review", `{"pr_number":"123"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("first call: status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), ReviewSentinelFailed) {
		t.Fatalf("first call body %q does not contain failure sentinel", rr.Body.String())
	}
	if got := stateOf(t, bus.DB, sessionName); got != string(agent.StateActive) {
		t.Fatalf("after first call: state = %q, want %q (rolled back)", got, agent.StateActive)
	}
	if reviewingInFlightOf(sc) {
		t.Fatalf("after first call: reviewingInFlight = true, want false (rolled back)")
	}

	// Second call — same handler, no special re-arming required. Must
	// behave like a first-time /review: pre-emptive write transitions
	// the session to reviewing and the flag is left set for the monitor
	// to clear later. If rollback left any stale bookkeeping (e.g. a
	// nil prevStatus that prevented the second pre-emptive write from
	// recording its own prev state, or a missed flag clear), this
	// assertion would fail.
	rr = doHostAPI(t, sc, http.MethodPost, "/review", `{"pr_number":"456"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second call: status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), ReviewSentinelPassed) {
		t.Errorf("second call body %q does not contain pass sentinel", rr.Body.String())
	}
	// On a successful /review, the monitor (subprocess) is responsible
	// for clearing reviewingInFlight via the /prompt review-complete
	// path. The handler MUST leave the flag set and the state as
	// `reviewing`. If the handler over-cleared (e.g. ran rollback on
	// success too), this assertion would fail.
	if got := stateOf(t, bus.DB, sessionName); got != string(agent.StateReviewing) {
		t.Errorf("after second call (success): state = %q, want %q (handler holds reviewing for monitor)", got, agent.StateReviewing)
	}
	if !reviewingInFlightOf(sc) {
		t.Errorf("after second call (success): reviewingInFlight = false, want true (handler holds the flag for the monitor)")
	}
}
