package cmd

// Tests for the orphan-resource sweep (containers, volumes, images).
//
// The sweep is wired into the four cleanup paths via
// applySessionResourceSweep; this file exercises each class in
// isolation through its test-injectable inner function
// (sweepWithRunner / sweepVolumesWithRunner / sweepImagesWithRunner)
// and end-to-end through headlessCleanupWithJSON with a stub
// podmanRunner.
//
// End-to-end invocation counts in this file are exact on purpose. The
// "a session that never enabled containers issues no podman command"
// property is only observable as a count, so a loose assertion would
// stop testing it.
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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
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
// not a plain substring. This is the security requirement's first line of
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
	wantFilter := "name=^" + regexp.QuoteMeta(container.ResourceNamePrefixForSession(session)) + "[a-f0-9]{8}$"
	if !containsArgValue(args, "--filter", wantFilter) {
		t.Errorf("--filter not anchored to the strict per-session shape; args=%v", args)
	}
	if !containsArg(args, "--format") {
		t.Errorf("--format flag missing; args=%v", args)
	}
}

// ── sweepWithRunner: empty result ─────────────────────────────────────────

// TestSweep_ZeroMatches_NoRmInvocation checks that when the prefix-filter
// sweep returns zero matches, cleanup does not invoke podman rm and reports
// containers_swept=0.
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

// TestSweep_MatchedContainersRemoved verifies that with
// containers_enabled=1 and ps returning matching names, the sweep
// invokes podman rm -f on each name and reports the count.
func TestSweep_MatchedContainersRemoved(t *testing.T) {
	session := "prism-test@happy"
	r := installFakeRunner(t)
	// ps returns three valid names matching the strict shape.
	psOut := strings.Join([]string{
		"prism-prism-test-happy-aaaaaaaa",
		"prism-prism-test-happy-bbbbbbbb",
		"prism-prism-test-happy-cccccccc",
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
		"prism-prism-test-happy-aaaaaaaa",
		"prism-prism-test-happy-bbbbbbbb",
		"prism-prism-test-happy-cccccccc",
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

// ── sweepWithRunner: security — sibling session not swept ─────────────────

// TestSweep_SiblingPrefixSessionNotSwept is the load-bearing security
// test: when two sessions exist whose names are in a prefix
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

// TestSweep_PsFailureIsNonFatal checks that when podman ps fails (machine
// off, socket down), cleanup logs a warning and completes successfully. The
// sweep returns 0 and does NOT subsequently invoke rm.
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

// TestHeadlessCleanup_ContainersDisabled_NoPodmanInvocation checks the
// critical property: when the session has containers_enabled=0, cleanup
// issues NO podman commands (no new warnings about podman being unavailable
// on sessions that did not enable containers).
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

	// NO podman commands issued.
	if calls := r.calls(); len(calls) != 0 {
		t.Errorf("AC VIOLATION: containers_enabled=0 must not issue podman commands; got: %v", calls)
	}

	// containers_swept key is OMITTED from the envelope.
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	if _, present := env["containers_swept"]; present {
		t.Errorf("containers_swept key should be omitted when containers_enabled=0; envelope=%v", env)
	}
}

// TestHeadlessCleanup_ContainersEnabled_EmptySweepReportsZero checks the
// edge case: when the prefix-filter sweep returns zero matches, cleanup does
// not invoke podman rm and reports containers_swept=0.
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

	// All three sweep counts present and zero in the envelope. The
	// three fields are written from one containers_enabled read, so an
	// envelope carries all three or none.
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	for _, key := range []string{"containers_swept", "volumes_swept", "images_swept"} {
		got, present := env[key]
		if !present {
			t.Fatalf("%s key must be present (containers_enabled=1) in envelope=%v", key, env)
		}
		// JSON numbers decode as float64 in map[string]any.
		f, isNum := got.(float64)
		if !isNum {
			t.Fatalf("%s must be a number; got %v (%T)", key, got, got)
		}
		if int(f) != 0 {
			t.Errorf("%s: got %v, want 0", key, f)
		}
	}

	// Two list invocations (container ps + volume ls), no removals. The
	// image sweep issues nothing: the seeded row carries no instance_id,
	// so there is no ledger to replay.
	calls := r.calls()
	if len(calls) != 2 {
		t.Errorf("expected two podman invocations (ps + volume ls); got %d: %v", len(calls), calls)
	}
	for _, args := range calls {
		if len(args) >= 1 && (args[0] == "rm" || args[0] == "rmi") {
			t.Errorf("zero matches must not trigger a removal; got: %v", args)
		}
		if len(args) >= 2 && args[0] == "volume" && args[1] == "rm" {
			t.Errorf("zero matches must not trigger podman volume rm; got: %v", args)
		}
	}
}

// TestHeadlessCleanup_ContainersEnabled_SweepReportsCount checks that
// `prism cleanup --yes --session <name> --json` for a containers-enabled
// session includes "containers_swept": <n> in the JSON envelope.
func TestHeadlessCleanup_ContainersEnabled_SweepReportsCount(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	withNoopTmux(t)
	r := installFakeRunner(t)
	psOut := strings.Join([]string{
		"prism-prism-test-swept-11111111",
		"prism-prism-test-swept-22222222",
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

	// ps + rm for the containers, then volume ls (which the stub
	// answers empty, so no volume rm follows).
	calls := r.calls()
	if len(calls) != 3 {
		t.Fatalf("expected ps + rm + volume ls invocations; got %d: %v", len(calls), calls)
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

// ── sweepVolumesWithRunner ────────────────────────────────────────────────

// TestVolumeSweep_FilterAnchoredAtPrefix pins the podman-side filter
// shape. The volume filter is anchored at the START of the name but not
// at the end, because the volume policy admits user-chosen suffixes as
// well as the 8-hex auto-name.
func TestVolumeSweep_FilterAnchoredAtPrefix(t *testing.T) {
	r := installFakeRunner(t)
	r.script(scriptedResponse{stdout: []byte("")})

	session := "prism-test@vol-filter"
	_ = sweepVolumesWithRunner(r, session, nil)

	calls := r.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one podman invocation (volume ls); got %d: %v", len(calls), calls)
	}
	args := calls[0]
	if len(args) < 2 || args[0] != "volume" || args[1] != "ls" {
		t.Fatalf("first invocation must be \"volume ls\"; got %v", args)
	}
	wantFilter := "name=^" + regexp.QuoteMeta(container.ResourceNamePrefixForSession(session))
	if !containsArgValue(args, "--filter", wantFilter) {
		t.Errorf("--filter not anchored to the per-session prefix; args=%v", args)
	}
	if !containsArgValue(args, "--format", "{{.Name}}") {
		t.Errorf("--format missing or wrong; args=%v", args)
	}
}

// TestVolumeSweep_PrefixedVolumesRemoved is the AC: cleanup removes
// every volume whose name starts with the session's prefix. Both the
// auto-injected 8-hex shape and a user-chosen suffix must go — a
// user-named volume holds data that outlives the session otherwise.
func TestVolumeSweep_PrefixedVolumesRemoved(t *testing.T) {
	r := installFakeRunner(t)
	lsOut := strings.Join([]string{
		"prism-foo-aaaaaaaa",    // auto-injected shape
		"prism-foo-my-postgres", // user-chosen suffix
		"prism-foo-cache.v2",    // dots are legal in volume names
	}, "\n") + "\n"
	r.script(
		scriptedResponse{stdout: []byte(lsOut)},
		scriptedResponse{stdout: []byte("")},
	)

	got := sweepVolumesWithRunner(r, "foo", nil)
	if got != 3 {
		t.Errorf("count: got %d, want 3", got)
	}
	calls := r.calls()
	if len(calls) != 2 {
		t.Fatalf("expected volume ls + volume rm; got %d: %v", len(calls), calls)
	}
	rm := calls[1]
	if len(rm) < 2 || rm[0] != "volume" || rm[1] != "rm" {
		t.Fatalf("second invocation must be \"volume rm\"; got %v", rm)
	}
	want := []string{"prism-foo-aaaaaaaa", "prism-foo-my-postgres", "prism-foo-cache.v2"}
	gotNames := rm[2:]
	if len(gotNames) != len(want) {
		t.Fatalf("volume rm args: got %v, want %v", gotNames, want)
	}
	for i, w := range want {
		if gotNames[i] != w {
			t.Errorf("volume rm arg %d: got %q, want %q", i, gotNames[i], w)
		}
	}
}

// TestVolumeSweep_SubstringTrapNotSwept is the security counterpart of
// TestSweep_SubstringTrapNotSwept: a volume whose name CONTAINS the
// prefix but does not START with it must survive. The stub returns it
// regardless of the filter, which simulates a podman version that reads
// the filter as a substring match.
func TestVolumeSweep_SubstringTrapNotSwept(t *testing.T) {
	r := installFakeRunner(t)
	lsOut := strings.Join([]string{
		"prism-foo-aaaaaaaa",      // legit
		"user-prism-foo-bbbbbbbb", // substring trap — must NOT be swept
	}, "\n") + "\n"
	r.script(
		scriptedResponse{stdout: []byte(lsOut)},
		scriptedResponse{stdout: []byte("")},
	)

	got := sweepVolumesWithRunner(r, "foo", nil)
	if got != 1 {
		t.Errorf("count: got %d, want 1", got)
	}
	rmArgs := r.calls()[1][2:]
	for _, name := range rmArgs {
		if !strings.HasPrefix(name, "prism-foo-") {
			t.Errorf("SECURITY: volume sweep removed %q, which the session cannot own", name)
		}
	}
}

// TestVolumeSweep_BarePrefixNameIsSwept pins the agreement between the
// create-side policy and the sweep.
//
// `applyVolumeNamePolicy` admits an explicit Name that equals the prefix
// exactly — `strings.HasPrefix(prefix, prefix)` is true — and podman
// accepts the resulting trailing dash, so `podman volume create
// prism-<session>-` succeeds. The sweep must therefore reach it. An
// earlier version excluded it, on the false premise that the policy
// could not produce that name, and the volume leaked permanently.
//
// The general invariant this defends: every name the volume policy
// admits must be reachable by the sweep.
func TestVolumeSweep_BarePrefixNameIsSwept(t *testing.T) {
	r := installFakeRunner(t)
	lsOut := strings.Join([]string{
		"prism-foo-",         // exactly the prefix — create admits it
		"prism-foo-aaaaaaaa", // auto-injected shape
	}, "\n") + "\n"
	r.script(
		scriptedResponse{stdout: []byte(lsOut)},
		scriptedResponse{stdout: []byte("")},
	)

	got := sweepVolumesWithRunner(r, "foo", nil)
	if got != 2 {
		t.Fatalf("count: got %d, want 2 (the bare-prefix volume must be swept, not leaked)", got)
	}
	rmArgs := r.calls()[1][2:]
	var sawBare bool
	for _, name := range rmArgs {
		if name == "prism-foo-" {
			sawBare = true
		}
	}
	if !sawBare {
		t.Errorf("the bare-prefix volume was not passed to podman volume rm; args=%v", rmArgs)
	}
}

// TestVolumeSweep_SiblingSessionVolumeNotSwept is the load-bearing
// sibling-prefix test. Session "foo" and session "foo-bar" have prefixes
// where one contains the other, so a plain prefix match on "prism-foo-"
// would destroy the live sibling's data volume.
//
// Volume names cannot resolve the ambiguity on their own — session
// "foo" is allowed to name a volume "prism-foo-bar-data" too — so the
// guard errs toward leaving it in place. This test fails if the guard is
// removed.
func TestVolumeSweep_SiblingSessionVolumeNotSwept(t *testing.T) {
	r := installFakeRunner(t)
	lsOut := strings.Join([]string{
		"prism-foo-aaaaaaaa",     // session "foo" — sweep it
		"prism-foo-bar-bbbbbbbb", // live session "foo-bar" — leave it
		"prism-foo-bar-pgdata",   // ditto
	}, "\n") + "\n"
	r.script(
		scriptedResponse{stdout: []byte(lsOut)},
		scriptedResponse{stdout: []byte("")},
	)

	siblings := []string{"prism-foo-bar-"}
	got := sweepVolumesWithRunner(r, "foo", siblings)
	if got != 1 {
		t.Fatalf("count: got %d, want 1 (only foo's own volume should be swept)", got)
	}
	rmArgs := r.calls()[1][2:]
	if len(rmArgs) != 1 || rmArgs[0] != "prism-foo-aaaaaaaa" {
		t.Fatalf("volume rm args: got %v, want [prism-foo-aaaaaaaa]", rmArgs)
	}
	for _, name := range rmArgs {
		if strings.HasPrefix(name, "prism-foo-bar-") {
			t.Errorf("SECURITY: sweep removed live sibling session's volume %q", name)
		}
	}
}

// TestVolumeSweep_LsFailureIsNonFatal pins the best-effort contract:
// podman being unavailable logs a warning and returns 0 without
// attempting a removal.
func TestVolumeSweep_LsFailureIsNonFatal(t *testing.T) {
	r := installFakeRunner(t)
	r.script(scriptedResponse{stdout: []byte(""), err: errors.New("exit status 125")})

	if got := sweepVolumesWithRunner(r, "foo", nil); got != 0 {
		t.Errorf("count on ls failure: got %d, want 0", got)
	}
	if len(r.calls()) != 1 {
		t.Errorf("expected only the ls invocation on failure; got %v", r.calls())
	}
}

// TestVolumeSweep_ZeroMatchesIssuesNoRemoval covers the case where the
// session enabled containers but created no volume.
func TestVolumeSweep_ZeroMatchesIssuesNoRemoval(t *testing.T) {
	r := installFakeRunner(t)
	r.script(scriptedResponse{stdout: []byte("\n\n")})

	if got := sweepVolumesWithRunner(r, "foo", nil); got != 0 {
		t.Errorf("count: got %d, want 0", got)
	}
	if len(r.calls()) != 1 {
		t.Errorf("expected only the ls invocation; got %v", r.calls())
	}
}

// TestSiblingVolumePrefixes_SelectsOnlyNestedLiveSessions covers the
// guard's CALL SITE, which the sweepVolumesWithRunner tests do not
// reach: they pass the sibling list in directly, so nothing pins how
// that list is actually derived from the database.
//
// The comparison must run in SANITISED space. Sanitisation folds `@`,
// `/`, `.`, and `~` all to `-`, so a raw-session-name comparison finds
// different neighbours than the volume names actually carry.
func TestSiblingVolumePrefixes_SelectsOnlyNestedLiveSessions(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	for _, s := range []string{
		"repo@foo",         // the session being cleaned
		"repo@foo-bar",     // nested: its prefix extends foo's
		"repo@foo-bar-baz", // nested deeper, also a sibling
		"repo@foobar",      // NOT nested: prefix is prism-repo-foobar-
		"repo@other",       // unrelated
	} {
		seedContainerSession(t, dbFile, s, true)
	}

	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	got := siblingVolumePrefixes(d, "repo@foo")
	want := map[string]bool{
		"prism-repo-foo-bar-":     true,
		"prism-repo-foo-bar-baz-": true,
	}
	if len(got) != len(want) {
		t.Fatalf("sibling prefixes: got %v, want the two nested ones %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected sibling prefix %q: a non-nested session must not suppress the sweep", p)
		}
		// The session being cleaned must never appear in its own
		// sibling list, or the guard suppresses the entire sweep.
		if p == "prism-repo-foo-" {
			t.Error("the cleaned session's own prefix is in the sibling list; the sweep would remove nothing")
		}
	}
}

// TestSiblingVolumePrefixes_NilDBIsSafe pins the documented
// degradation: without a database the guard contributes nothing and the
// sweep falls back to the plain prefix match.
func TestSiblingVolumePrefixes_NilDBIsSafe(t *testing.T) {
	if got := siblingVolumePrefixes(nil, "repo@foo"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// ── sweepImagesWithRunner ─────────────────────────────────────────────────

// writeImageLedger writes a ledger file holding one JSON line per
// supplied reference and returns its path.
func writeImageLedger(t *testing.T, refs ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "podman-images.log")
	var b strings.Builder
	for _, ref := range refs {
		line, err := json.Marshal(map[string]string{"image": ref})
		if err != nil {
			t.Fatalf("marshal ledger line: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	return path
}

// TestImageSweep_LedgerImagesRemoved is the AC: cleanup removes every
// image the session pulled through its proxy. One `podman rmi` per
// reference, never a host-wide prune — the image store is shared with
// the user and with every other session.
func TestImageSweep_LedgerImagesRemoved(t *testing.T) {
	r := installFakeRunner(t)
	r.script(
		scriptedResponse{stdout: []byte("")},
		scriptedResponse{stdout: []byte("")},
	)
	ledger := writeImageLedger(t, "alpine:3.19", "docker.io/library/busybox:latest")

	got := sweepImagesWithRunner(r, "foo", ledger)
	if got != 2 {
		t.Errorf("count: got %d, want 2", got)
	}
	calls := r.calls()
	if len(calls) != 2 {
		t.Fatalf("expected one rmi per image; got %d: %v", len(calls), calls)
	}
	want := [][]string{
		{"rmi", "alpine:3.19"},
		{"rmi", "docker.io/library/busybox:latest"},
	}
	for i, w := range want {
		if len(calls[i]) != 2 || calls[i][0] != w[0] || calls[i][1] != w[1] {
			t.Errorf("invocation %d: got %v, want %v", i, calls[i], w)
		}
	}
	for _, args := range calls {
		if containsArg(args, "prune") {
			t.Errorf("the image sweep must never prune the shared image store; got %v", args)
		}
	}
}

// TestImageSweep_InUseImageDoesNotStrandTheRest is the AC: when podman
// refuses to remove one image because a container still uses it, cleanup
// removes the rest and does not fail.
//
// This is why the sweep issues one rmi per image rather than one batched
// call: a batch would report a single non-zero exit and the sweep could
// not tell which images survived.
func TestImageSweep_InUseImageDoesNotStrandTheRest(t *testing.T) {
	r := installFakeRunner(t)
	r.script(
		scriptedResponse{stdout: []byte("")},
		scriptedResponse{stdout: []byte("image is in use by a container"), err: errors.New("exit status 2")},
		scriptedResponse{stdout: []byte("")},
	)
	ledger := writeImageLedger(t, "alpine:3.19", "postgres:16", "busybox:latest")

	got := sweepImagesWithRunner(r, "foo", ledger)
	if got != 2 {
		t.Errorf("count: got %d, want 2 (the in-use image is skipped, the rest are removed)", got)
	}
	calls := r.calls()
	if len(calls) != 3 {
		t.Fatalf("a failed rmi must not stop the sweep; got %d invocations: %v", len(calls), calls)
	}
	if calls[2][1] != "busybox:latest" {
		t.Errorf("the sweep did not continue past the failure; third invocation was %v", calls[2])
	}
}

// TestImageSweep_NoLedgerIssuesNoPodmanCommand covers the session that
// enabled containers but never pulled an image, and the session whose
// row carries no instance_id (so no ledger path can be derived).
func TestImageSweep_NoLedgerIssuesNoPodmanCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"no instance id", ""},
		{"ledger file absent", filepath.Join(t.TempDir(), "absent.log")},
		{"ledger file empty", writeImageLedger(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := installFakeRunner(t)
			if got := sweepImagesWithRunner(r, "foo", tc.path); got != 0 {
				t.Errorf("count: got %d, want 0", got)
			}
			if calls := r.calls(); len(calls) != 0 {
				t.Errorf("expected no podman invocation; got %v", calls)
			}
		})
	}
}

// TestImageSweep_HostileLedgerEntriesNotPassedToPodman is the sweep-side
// half of the argument-injection defence. A ledger is a file on disk;
// the sweep must not hand its contents to podman unchecked.
func TestImageSweep_HostileLedgerEntriesNotPassedToPodman(t *testing.T) {
	r := installFakeRunner(t)
	r.script(scriptedResponse{stdout: []byte("")})
	ledger := writeImageLedger(t, "--all", "-f", "alpine:3.19")

	got := sweepImagesWithRunner(r, "foo", ledger)
	if got != 1 {
		t.Errorf("count: got %d, want 1 (only the well-formed reference)", got)
	}
	for _, args := range r.calls() {
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				t.Errorf("SECURITY: flag-shaped ledger entry %q reached the podman command line: %v", a, args)
			}
		}
	}
}

// ── end-to-end: image sweep through headlessCleanup ───────────────────────

// TestHeadlessCleanup_ImagesSweptFromLedger walks the whole path: a
// containers-enabled session with an instance_id and a ledger on disk
// gets its pulled image removed, and the count surfaces in the JSON
// envelope.
//
// XDG_STATE_HOME is redirected so container.SessionWorkDirPath resolves
// inside the test tempdir rather than the developer's real home (and so
// the test survives the homeless-shelter nix sandbox).
func TestHeadlessCleanup_ImagesSweptFromLedger(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	withNoopTmux(t)
	r := installFakeRunner(t)
	r.script(
		scriptedResponse{stdout: []byte("")}, // container ps: nothing
		scriptedResponse{stdout: []byte("")}, // volume ls: nothing
		scriptedResponse{stdout: []byte("")}, // rmi alpine:3.19
	)

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	session := "prism-test@image-sweep"
	instanceID := "instance-image-sweep"
	seedContainerSession(t, dbFile, session, true)
	setSessionInstanceID(t, dbFile, session, instanceID)

	sessionDir := filepath.Join(stateHome, "prism", "sessions", instanceID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	ledger := filepath.Join(sessionDir, "podman-images.log")
	if err := os.WriteFile(ledger, []byte(`{"image":"alpine:3.19"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	out := captureStdoutDuringFn(t, func() {
		if err := headlessCleanupWithJSON(session, "image-sweep", "", "", true); err != nil {
			t.Fatalf("headlessCleanupWithJSON: %v", err)
		}
	})

	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %q", err, out)
	}
	got, present := env["images_swept"]
	if !present {
		t.Fatalf("images_swept key must be present in envelope=%v", env)
	}
	if f := got.(float64); int(f) != 1 {
		t.Errorf("images_swept: got %v, want 1", f)
	}

	var sawRmi bool
	for _, args := range r.calls() {
		if len(args) == 2 && args[0] == "rmi" && args[1] == "alpine:3.19" {
			sawRmi = true
		}
	}
	if !sawRmi {
		t.Errorf("expected `podman rmi alpine:3.19`; got %v", r.calls())
	}
}

// setSessionInstanceID stamps agent_status.instance_id so the cleanup
// path can derive the per-session work dir (and therefore the image
// ledger path).
func setSessionInstanceID(t *testing.T, dbFile, session, instanceID string) {
	t.Helper()
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := d.SetInstanceID(session, instanceID); err != nil {
		t.Fatalf("SetInstanceID(%q, %q): %v", session, instanceID, err)
	}
}

// Compile-time guard: keep fmt imported for ad-hoc debugging.
var _ = fmt.Sprintf
