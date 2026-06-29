package cmd

// Tests for the #2324 Step-7 orphan-container sweep.
//
// The sweep is wired into the four cleanup paths via
// applyOrphanContainerSweep; this file exercises the sweep in
// isolation through sweepWithRunner (the test-injectable inner
// function) and end-to-end through headlessCleanupWithJSON with a
// stub podmanRunner.
//
// The discipline mirrors the existing cmd/cleanup_lifecycle_test.go:
// SetTestDBPath to redirect openDB at a temp DB, withNoopTmux to
// neutralise tmux side effects, and captureStdoutDuringFn for the
// JSON envelope assertion. The new piece is the stub podmanRunner
// in fakePodmanRunner: it records every Run invocation and returns
// scripted outputs so the test can assert exactly which podman args
// were sent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// fakePodmanRunner is the test stub for podmanRunner. Every Run call
// appends a copy of args to invocations and returns the next
// scripted (stdout, err) pair from responses; if responses is empty,
// Run returns ([]byte{}, nil).
//
// Concurrent calls are guarded by mu; the sweep is sequential in
// production but defensive locking keeps the stub honest if a future
// caller parallelises.
type fakePodmanRunner struct {
	mu          sync.Mutex
	invocations [][]string
	// scripted is a FIFO of (stdout, err) pairs returned by Run, in
	// invocation order. Run pops the front of the slice per call.
	scripted []scriptedResponse
}

type scriptedResponse struct {
	stdout []byte
	err    error
}

func (f *fakePodmanRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	captured := make([]string, len(args))
	copy(captured, args)
	f.invocations = append(f.invocations, captured)
	if len(f.scripted) == 0 {
		return []byte{}, nil
	}
	resp := f.scripted[0]
	f.scripted = f.scripted[1:]
	return resp.stdout, resp.err
}

func (f *fakePodmanRunner) script(responses ...scriptedResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripted = append(f.scripted, responses...)
}

func (f *fakePodmanRunner) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.invocations))
	for i, args := range f.invocations {
		out[i] = append([]string(nil), args...)
	}
	return out
}

// seedContainerSession is a small variant of seedRowWithLifecycleFields
// (from cleanup_lifecycle_test.go) that flips containers_enabled to
// the supplied value at seed time so the sweep gate fires.
func seedContainerSession(t *testing.T, dbFile, session string, containersEnabled bool) {
	t.Helper()
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.UpsertStatus(session, "prism-test", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus(%q): %v", session, err)
	}
	if _, err := d.AllocatePort(session); err != nil {
		t.Fatalf("AllocatePort(%q): %v", session, err)
	}
	if err := d.SetContainersEnabled(session, containersEnabled); err != nil {
		t.Fatalf("SetContainersEnabled(%q, %v): %v", session, containersEnabled, err)
	}
}

// installFakeRunner sets podmanRunnerForTest for the duration of the
// test and returns the runner so tests can script responses and read
// invocations.
func installFakeRunner(t *testing.T) *fakePodmanRunner {
	t.Helper()
	r := &fakePodmanRunner{}
	SetTestPodmanRunner(r)
	t.Cleanup(func() { SetTestPodmanRunner(nil) })
	return r
}

// ── sweepWithRunner: filter shape ─────────────────────────────────────────

// TestSweep_FilterUsesAnchoredRegex verifies that the podman ps
// invocation passes an anchored regex through --filter name=<regex>,
// not a plain substring. This is the security AC's first line of
// defence: podman libpod's filter is regex-matched, so anchoring on
// the podman side already excludes most non-matching containers
// before the Go-side re-filter sees them.
func TestSweep_FilterUsesAnchoredRegex(t *testing.T) {
	r := installFakeRunner(t)
	r.script(scriptedResponse{stdout: []byte("")}) // ps returns nothing

	session := "prism-test@filter-shape"
	_ = sweepWithRunner(r, session)

	calls := r.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one podman invocation (ps); got %d: %v", len(calls), calls)
	}
	args := calls[0]
	if len(args) < 1 || args[0] != "ps" {
		t.Fatalf("first arg should be \"ps\"; got %v", args)
	}
	if !containsArgValue(args, "--filter", "name=^prism-prism-test@filter-shape-[a-f0-9]{8}$") {
		t.Errorf("--filter not anchored to the strict per-session shape; args=%v", args)
	}
	if !containsArg(args, "--format") {
		t.Errorf("--format flag missing; args=%v", args)
	}
}

// ── sweepWithRunner: empty result ─────────────────────────────────────────

// TestSweep_ZeroMatches_NoRmInvocation verifies the AC: "When the
// prefix-filter sweep returns zero matches, cleanup does not invoke
// podman rm and reports containers_swept=0."
func TestSweep_ZeroMatches_NoRmInvocation(t *testing.T) {
	r := installFakeRunner(t)
	r.script(scriptedResponse{stdout: []byte("\n")}) // ps returns just newlines

	got := sweepWithRunner(r, "prism-test@empty")

	if got != 0 {
		t.Errorf("count: got %d, want 0", got)
	}
	calls := r.calls()
	if len(calls) != 1 {
		t.Errorf("expected one podman invocation (ps only); got %d: %v", len(calls), calls)
	}
	for _, args := range calls {
		if len(args) >= 1 && args[0] == "rm" {
			t.Errorf("unexpected podman rm invocation: %v", args)
		}
	}
}

// ── sweepWithRunner: happy path ───────────────────────────────────────────

// TestSweep_MatchedContainersRemoved verifies the AC: with
// containers_enabled=1 and ps returning matching names, the sweep
// invokes podman rm -f on each name and reports the count.
func TestSweep_MatchedContainersRemoved(t *testing.T) {
	session := "prism-test@happy"
	r := installFakeRunner(t)
	// ps returns three valid names matching the strict shape.
	psOut := strings.Join([]string{
		"prism-prism-test@happy-aaaaaaaa",
		"prism-prism-test@happy-bbbbbbbb",
		"prism-prism-test@happy-cccccccc",
	}, "\n") + "\n"
	r.script(
		scriptedResponse{stdout: []byte(psOut)},
		scriptedResponse{stdout: []byte("")}, // rm returns empty
	)

	got := sweepWithRunner(r, session)
	if got != 3 {
		t.Errorf("count: got %d, want 3", got)
	}
	calls := r.calls()
	if len(calls) != 2 {
		t.Fatalf("expected two podman invocations (ps + rm); got %d: %v", len(calls), calls)
	}
	rm := calls[1]
	if len(rm) < 2 || rm[0] != "rm" || rm[1] != "-f" {
		t.Fatalf("second invocation must start with \"rm -f\"; got %v", rm)
	}
	wantNames := []string{
		"prism-prism-test@happy-aaaaaaaa",
		"prism-prism-test@happy-bbbbbbbb",
		"prism-prism-test@happy-cccccccc",
	}
	got2 := rm[2:]
	if len(got2) != len(wantNames) {
		t.Fatalf("rm args: got %v, want %v", got2, wantNames)
	}
	for i, want := range wantNames {
		if got2[i] != want {
			t.Errorf("rm arg %d: got %q, want %q", i, got2[i], want)
		}
	}
}

// ── sweepWithRunner: security AC — sibling session not swept ──────────────

// TestSweep_SiblingPrefixSessionNotSwept is the load-bearing security
// AC test: when two sessions exist whose names are in a prefix
// relationship (session "foo" vs session "foo-bar"), the sweep for
// "foo" must NOT touch "foo-bar"'s containers.
//
// The stub returns BOTH containers regardless of the filter — this
// simulates a worst-case scenario where podman's filter is a
// substring match (the docker compat surface historically behaved
// this way). The production code's Go-side re-filter using the
// strict regex `^prism-<session>-[a-f0-9]{8}$` is what closes the
// gap; this test fails if the regex is loosened to a plain prefix.
func TestSweep_SiblingPrefixSessionNotSwept(t *testing.T) {
	r := installFakeRunner(t)
	// Both containers appear in podman's output. The session being
	// cleaned is "foo"; "foo-with-suffix" is the sibling session.
	// A naive prefix match on "prism-foo-" matches BOTH names; the
	// strict regex `^prism-foo-[a-f0-9]{8}$` only matches the
	// 8-hex-suffix one.
	psOut := strings.Join([]string{
		"prism-foo-aaaaaaaa",             // belongs to session "foo" — should be swept
		"prism-foo-with-suffix-bbbbbbbb", // belongs to session "foo-with-suffix" — must NOT be swept
		"prism-foo-with-suffix-cccccccc", // ditto
	}, "\n") + "\n"
	r.script(
		scriptedResponse{stdout: []byte(psOut)},
		scriptedResponse{stdout: []byte("")},
	)

	got := sweepWithRunner(r, "foo")
	if got != 1 {
		t.Errorf("count: got %d, want 1 (only foo's container should be swept)", got)
	}
	calls := r.calls()
	if len(calls) != 2 {
		t.Fatalf("expected two podman invocations (ps + rm); got %d: %v", len(calls), calls)
	}
	rmArgs := calls[1][2:] // strip "rm" "-f"
	if len(rmArgs) != 1 || rmArgs[0] != "prism-foo-aaaaaaaa" {
		t.Errorf("rm args: got %v, want [prism-foo-aaaaaaaa]", rmArgs)
	}
	for _, name := range rmArgs {
		if strings.HasPrefix(name, "prism-foo-with-suffix-") {
			t.Errorf("SECURITY AC VIOLATION: sweep removed sibling-session container %q", name)
		}
	}
}

// TestSweep_SubstringTrapNotSwept covers the related "substring trap"
// case: a container whose name CONTAINS the session prefix but does
// not START with it (e.g. "user-prism-foo-aaaaaaaa") must NOT be
// swept. This complements the sibling-prefix case above.
func TestSweep_SubstringTrapNotSwept(t *testing.T) {
	r := installFakeRunner(t)
	psOut := strings.Join([]string{
		"prism-foo-aaaaaaaa",      // legit
		"user-prism-foo-bbbbbbbb", // substring trap — must NOT be swept
	}, "\n") + "\n"
	r.script(
		scriptedResponse{stdout: []byte(psOut)},
		scriptedResponse{stdout: []byte("")},
	)

	got := sweepWithRunner(r, "foo")
	if got != 1 {
		t.Errorf("count: got %d, want 1", got)
	}
	rmArgs := r.calls()[1][2:]
	for _, name := range rmArgs {
		if strings.HasPrefix(name, "user-") {
			t.Errorf("SECURITY AC VIOLATION: sweep removed substring-trap container %q", name)
		}
	}
}

// ── sweepWithRunner: ps failure is non-fatal ──────────────────────────────

// TestSweep_PsFailureIsNonFatal verifies the AC: "When podman ps
// fails (machine off, socket down), cleanup logs a warning and
// completes successfully." The sweep returns 0 and does NOT
// subsequently invoke rm.
func TestSweep_PsFailureIsNonFatal(t *testing.T) {
	r := installFakeRunner(t)
	r.script(scriptedResponse{
		stdout: []byte("Cannot connect to Podman\n"),
		err:    errors.New("exit status 125"),
	})

	got := sweepWithRunner(r, "prism-test@ps-fails")
	if got != 0 {
		t.Errorf("count on ps failure: got %d, want 0", got)
	}
	calls := r.calls()
	if len(calls) != 1 {
		t.Errorf("expected only ps invocation on failure; got %d calls: %v", len(calls), calls)
	}
}

// TestSweep_RmFailureIsNonFatal verifies that when podman rm fails
// after a successful ps, cleanup logs a warning and returns 0
// (count of "definitely removed") rather than aborting cleanup.
func TestSweep_RmFailureIsNonFatal(t *testing.T) {
	r := installFakeRunner(t)
	r.script(
		scriptedResponse{stdout: []byte("prism-foo-aaaaaaaa\n")},
		scriptedResponse{stdout: []byte(""), err: errors.New("exit status 1")},
	)

	got := sweepWithRunner(r, "foo")
	if got != 0 {
		t.Errorf("count on rm failure: got %d, want 0 (we don't know how many were removed)", got)
	}
	if len(r.calls()) != 2 {
		t.Errorf("expected ps + rm invocations; got %d", len(r.calls()))
	}
}

// ── end-to-end: containers_enabled gating through headlessCleanup ─────────

// TestHeadlessCleanup_ContainersDisabled_NoPodmanInvocation verifies
// the CRITICAL AC: "When the session has containers_enabled=0,
// cleanup issues NO podman commands. (No new warnings about podman
// being unavailable on sessions that did not enable containers.)"
//
// Seeds a session with containers_enabled=0 (the default) and
// asserts the stub runner was NEVER invoked, AND the JSON envelope
// omits the containers_swept key.
func TestHeadlessCleanup_ContainersDisabled_NoPodmanInvocation(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)
	r := installFakeRunner(t)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	session := "prism-test@no-containers"
	seedContainerSession(t, dbFile, session, false)

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCleanupWithJSON(session, "no-containers", "", "", true); err != nil {
			t.Fatalf("headlessCleanupWithJSON: %v", err)
		}
	})

	// AC: NO podman commands issued.
	if calls := r.calls(); len(calls) != 0 {
		t.Errorf("AC VIOLATION: containers_enabled=0 must not issue podman commands; got: %v", calls)
	}

	// AC: containers_swept key is OMITTED from the envelope.
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	if _, present := env["containers_swept"]; present {
		t.Errorf("containers_swept key should be omitted when containers_enabled=0; envelope=%v", env)
	}
}

// TestHeadlessCleanup_ContainersEnabled_EmptySweepReportsZero verifies
// the AC: "[edge-case] When the prefix-filter sweep returns zero
// matches, cleanup does not invoke podman rm and reports
// containers_swept=0."
func TestHeadlessCleanup_ContainersEnabled_EmptySweepReportsZero(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)
	r := installFakeRunner(t)
	r.script(scriptedResponse{stdout: []byte("")})

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	session := "prism-test@empty-sweep"
	seedContainerSession(t, dbFile, session, true)

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCleanupWithJSON(session, "empty-sweep", "", "", true); err != nil {
			t.Fatalf("headlessCleanupWithJSON: %v", err)
		}
	})

	// AC: containers_swept=0 present in envelope.
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	got, present := env["containers_swept"]
	if !present {
		t.Fatalf("containers_swept key must be present (containers_enabled=1) in envelope=%v", env)
	}
	// JSON numbers decode as float64 in map[string]any.
	f, isNum := got.(float64)
	if !isNum {
		t.Fatalf("containers_swept must be a number; got %v (%T)", got, got)
	}
	if int(f) != 0 {
		t.Errorf("containers_swept: got %v, want 0", f)
	}

	// AC: only ps invoked, no rm.
	calls := r.calls()
	if len(calls) != 1 {
		t.Errorf("expected one podman invocation (ps); got %d: %v", len(calls), calls)
	}
	for _, args := range calls {
		if len(args) >= 1 && args[0] == "rm" {
			t.Errorf("zero matches must not trigger podman rm; got: %v", args)
		}
	}
}

// TestHeadlessCleanup_ContainersEnabled_SweepReportsCount verifies the
// AC: "prism cleanup --yes --session <name> --json for a
// containers-enabled session includes \"containers_swept\": <n> in the
// JSON envelope."
func TestHeadlessCleanup_ContainersEnabled_SweepReportsCount(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)
	r := installFakeRunner(t)
	psOut := strings.Join([]string{
		"prism-prism-test@swept-11111111",
		"prism-prism-test@swept-22222222",
	}, "\n") + "\n"
	r.script(
		scriptedResponse{stdout: []byte(psOut)},
		scriptedResponse{stdout: []byte("")},
	)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	session := "prism-test@swept"
	seedContainerSession(t, dbFile, session, true)

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCleanupWithJSON(session, "swept", "", "", true); err != nil {
			t.Fatalf("headlessCleanupWithJSON: %v", err)
		}
	})

	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	got, present := env["containers_swept"]
	if !present {
		t.Fatalf("containers_swept key must be present in envelope=%v", env)
	}
	f := got.(float64)
	if int(f) != 2 {
		t.Errorf("containers_swept: got %v, want 2", f)
	}

	calls := r.calls()
	if len(calls) != 2 {
		t.Fatalf("expected ps + rm invocations; got %d: %v", len(calls), calls)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

// containsArgValue returns true if args contains a `flag` token
// immediately followed by `value`. Used to assert on
// --filter / --format pairs without depending on flag ordering.
func containsArgValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// Compile-time guard: keep fmt imported for ad-hoc debugging.
var _ = fmt.Sprintf
