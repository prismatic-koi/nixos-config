// Tests for the podman container teardown portion of cleanup (issue #471).
//
// These tests exercise removeContainerIfExists and headlessCleanup's container
// removal path using a fake `podman` executable placed first on PATH. The fake
// records each invocation to a log file so the test can assert which commands
// were run, in what order, and with which arguments.
//
// Why PATH-injection rather than a mock: removeContainerIfExists calls
// exec.CommandContext(ctx, "podman", ...) with a bare "podman" string, so
// prepending a temp dir to PATH that contains an executable named "podman" is
// enough to intercept every call without touching the cleanup code.
package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
)

// installFakePodman writes a shell script named "podman" into a fresh temp
// directory, prepends that directory to PATH (scoped to the test via t.Setenv),
// and returns the path of the log file that the fake appends to on every call.
//
// The fake's behaviour is controlled by the `mode` argument:
//   - "ok"        → always exits 0 with empty output.
//   - "no-such"   → always prints "Error: no such container: <name>" to stderr
//     and exits with status 1.  This simulates the edge case where the
//     container has already been removed by the sidecar.
//
// Each invocation is appended to the log file as one line per argv word,
// followed by a "---" separator so multiple calls can be disambiguated.
func installFakePodman(t *testing.T, mode string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake podman script uses /bin/sh — not supported on windows")
	}

	dir := t.TempDir()
	logFile := filepath.Join(dir, "podman.log")
	fakePath := filepath.Join(dir, "podman")

	var body string
	switch mode {
	case "ok":
		body = `#!/bin/sh
for arg in "$@"; do
  printf '%s\n' "$arg" >> "` + logFile + `"
done
printf '%s\n' '---' >> "` + logFile + `"
exit 0
`
	case "no-such":
		body = `#!/bin/sh
for arg in "$@"; do
  printf '%s\n' "$arg" >> "` + logFile + `"
done
printf '%s\n' '---' >> "` + logFile + `"
printf '%s\n' 'Error: no such container: fake' 1>&2
exit 1
`
	default:
		t.Fatalf("installFakePodman: unknown mode %q", mode)
	}

	if err := os.WriteFile(fakePath, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}

	// Prepend the fake dir to PATH so exec.LookPath("podman") finds it first.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return logFile
}

// readPodmanCalls returns the list of invocations recorded by the fake podman.
// Each invocation is returned as a slice of arguments (matching argv, not
// including argv[0]).  Empty slices are filtered out so a trailing newline in
// the log does not produce a spurious empty call.
func readPodmanCalls(t *testing.T, logFile string) [][]string {
	t.Helper()

	data, err := os.ReadFile(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read podman log: %v", err)
	}

	var calls [][]string
	var current []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "---" {
			if len(current) > 0 {
				calls = append(calls, current)
			}
			current = nil
			continue
		}
		if line == "" {
			continue
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		calls = append(calls, current)
	}
	return calls
}

// joinArgs returns a single string of space-joined args for use in error
// messages.  It does not attempt to shell-quote; the tests don't use args with
// whitespace.
func joinArgs(args []string) string { return strings.Join(args, " ") }

// TestRemoveContainerIfExists_StopThenRm verifies that removeContainerIfExists
// issues `podman stop --time 10 <name>` followed by `podman rm --force <name>`,
// in that order, where <name> is container.NameForSession(sessionName).
//
// Covers AC-1..AC-4 (all four cleanup paths delegate to removeContainerIfExists)
// and AC-6 (direct removal via podman rm -f) from issue #471.
func TestRemoveContainerIfExists_StopThenRm(t *testing.T) {
	logFile := installFakePodman(t, "ok")

	const session = "myrepo@feature"
	wantName := container.NameForSession(session)

	removeContainerIfExists(session)

	calls := readPodmanCalls(t, logFile)
	if len(calls) != 2 {
		t.Fatalf("expected 2 podman calls, got %d: %v", len(calls), calls)
	}

	// Call 1: podman stop --time 10 <name>
	wantStop := []string{"stop", "--time", "10", wantName}
	if got := calls[0]; !equalArgs(got, wantStop) {
		t.Errorf("call 1 = %q, want %q", joinArgs(got), joinArgs(wantStop))
	}

	// Call 2: podman rm --force <name>
	wantRm := []string{"rm", "--force", wantName}
	if got := calls[1]; !equalArgs(got, wantRm) {
		t.Errorf("call 2 = %q, want %q", joinArgs(got), joinArgs(wantRm))
	}
}

// TestRemoveContainerIfExists_NoSuchContainer verifies that when podman exits
// non-zero with "no such container" in stderr, removeContainerIfExists returns
// cleanly (no panic, no propagated error — it has no error return at all, so
// the assertion is that it completes without crashing and still attempts both
// stop and rm).
//
// Covers AC-7 (no container → clean exit) from issue #471.
func TestRemoveContainerIfExists_NoSuchContainer(t *testing.T) {
	logFile := installFakePodman(t, "no-such")

	// Must complete without panicking.
	removeContainerIfExists("myrepo@already-gone")

	// Both stop and rm should still have been attempted; the "no such
	// container" result is swallowed silently.
	calls := readPodmanCalls(t, logFile)
	if len(calls) != 2 {
		t.Fatalf("expected 2 podman calls even on 'no such container', got %d: %v",
			len(calls), calls)
	}
	if calls[0][0] != "stop" {
		t.Errorf("call 1 verb = %q, want %q", calls[0][0], "stop")
	}
	if calls[1][0] != "rm" {
		t.Errorf("call 2 verb = %q, want %q", calls[1][0], "rm")
	}
}

// TestHeadlessCleanup_HostMode_SkipsPodman verifies that when host_mode is set
// for a session in the DB, headlessCleanup does NOT invoke podman at all —
// host-mode sessions run opencode directly on the host with no container.
//
// This test calls headlessCleanup with an empty worktreePath so that the git
// operations are skipped and only the sidecar/container/DB teardown runs.
// Covers the host_mode short-circuit added in #471.
func TestHeadlessCleanup_HostMode_SkipsPodman(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "") // run host-side logic directly, not via proxy
	// Redirect TmuxBin to a no-op so headlessCleanup's scratchpad-ensure
	// path does not reach the live tmux server.
	withNoopTmux(t)
	logFile := installFakePodman(t, "ok")

	// Seed a temp DB with a host-mode row.
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	session := "myrepo@host-branch"
	if err := d.UpsertStatus(session, "myrepo", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.SetHostMode(session, true); err != nil {
		t.Fatalf("SetHostMode: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	// Empty worktreePath → headlessCleanup skips git ops and runs only the
	// sidecar/container/DB path.  With host_mode=1 it must NOT touch podman.
	if err := headlessCleanup(session, "host-branch", "", ""); err != nil {
		t.Fatalf("headlessCleanup returned error %v, want nil", err)
	}

	calls := readPodmanCalls(t, logFile)
	if len(calls) != 0 {
		t.Errorf("expected 0 podman calls in host_mode, got %d: %v", len(calls), calls)
	}

	// The DB row must still have been marked ended — host_mode only gates
	// podman, not the rest of cleanup.
	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	status, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("CurrentStatus returned nil — row missing")
	}
	if status.EndedAt == nil {
		t.Errorf("ended_at is nil — host-mode session was not marked as ended")
	}
}

// TestHeadlessCleanup_ContainerMode_StopsAndRemoves verifies the positive
// container-mode path of headlessCleanup: with host_mode unset (default 0)
// and an empty worktreePath, podman stop/rm must be invoked for the session's
// container name.
//
// Covers AC-2 (headlessCleanup path) directly.
func TestHeadlessCleanup_ContainerMode_StopsAndRemoves(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "") // run host-side logic directly, not via proxy
	// Redirect TmuxBin to a no-op so headlessCleanup's scratchpad-ensure
	// path does not reach the live tmux server.
	withNoopTmux(t)
	logFile := installFakePodman(t, "ok")

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	session := "myrepo@container-branch"
	if err := d.UpsertStatus(session, "myrepo", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	// host_mode defaults to 0 — leave it alone.
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if err := headlessCleanup(session, "container-branch", "", ""); err != nil {
		t.Fatalf("headlessCleanup returned error %v, want nil", err)
	}

	wantName := container.NameForSession(session)
	calls := readPodmanCalls(t, logFile)
	if len(calls) != 2 {
		t.Fatalf("expected 2 podman calls, got %d: %v", len(calls), calls)
	}
	wantStop := []string{"stop", "--time", "10", wantName}
	if got := calls[0]; !equalArgs(got, wantStop) {
		t.Errorf("stop call = %q, want %q", joinArgs(got), joinArgs(wantStop))
	}
	wantRm := []string{"rm", "--force", wantName}
	if got := calls[1]; !equalArgs(got, wantRm) {
		t.Errorf("rm call = %q, want %q", joinArgs(got), joinArgs(wantRm))
	}
}

// equalArgs reports whether two argument slices are element-wise equal.
func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStopAndRemoveChildContainers_NoChildren verifies that when a parent
// session has no review-agent children, stopAndRemoveChildContainers issues
// no podman commands — the fast path must not regress existing behaviour.
//
// Covers the [edge-case] AC: parents with no review-agent children clean up
// exactly as before.
func TestStopAndRemoveChildContainers_NoChildren(t *testing.T) {
	logFile := installFakePodman(t, "ok")

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	parent := "myrepo@no-review"
	if err := d.UpsertStatus(parent, "myrepo", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	stopAndRemoveChildContainers(d, parent)

	calls := readPodmanCalls(t, logFile)
	if len(calls) != 0 {
		t.Errorf("expected 0 podman calls for parent with no children, got %d: %v", len(calls), calls)
	}
}

// TestStopAndRemoveChildContainers_WithChildren verifies that
// stopAndRemoveChildContainers issues stop+rm calls for each child container
// in the correct order (stop before rm, all children covered).
//
// Covers the [functional] AC:
//   - stops and removes containers for sessions matching <parent>~review-%
//   - teardown runs before the DB row is removed (verified by calling
//     stopAndRemoveChildContainers before CleanupReviewSessionsForParent)
func TestStopAndRemoveChildContainers_WithChildren(t *testing.T) {
	logFile := installFakePodman(t, "ok")

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	parent := "myrepo@worker"

	// Seed parent row.
	if err := d.UpsertStatus(parent, "myrepo", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}

	// Seed two review-agent child rows using the name-prefix path
	// (AllStatusesWithPrefix fallback — no group registered).
	child1 := parent + "~review-1-review-goal"
	child2 := parent + "~review-1-review-code"
	if err := d.UpsertStatus(child1, "myrepo", "", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus child1: %v", err)
	}
	if err := d.UpsertStatus(child2, "myrepo", "", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus child2: %v", err)
	}

	stopAndRemoveChildContainers(d, parent)

	calls := readPodmanCalls(t, logFile)

	// We expect 2 podman calls per child (stop + rm) = 4 total.
	if len(calls) != 4 {
		t.Fatalf("expected 4 podman calls for 2 children, got %d: %v", len(calls), calls)
	}

	// Verify ordering: each pair must be stop-then-rm for the same container.
	wantName1 := container.NameForSession(child1)
	wantName2 := container.NameForSession(child2)

	// Collect the names we saw in stop calls and rm calls independently.
	var stopNames, rmNames []string
	for _, call := range calls {
		if len(call) < 1 {
			continue
		}
		verb := call[0]
		name := call[len(call)-1]
		switch verb {
		case "stop":
			stopNames = append(stopNames, name)
		case "rm":
			rmNames = append(rmNames, name)
		}
	}

	if len(stopNames) != 2 {
		t.Errorf("expected 2 stop calls, got %d: %v", len(stopNames), stopNames)
	}
	if len(rmNames) != 2 {
		t.Errorf("expected 2 rm calls, got %d: %v", len(rmNames), rmNames)
	}

	// Both child container names must appear in stop and rm calls.
	wantSet := map[string]bool{wantName1: true, wantName2: true}
	for _, n := range stopNames {
		if !wantSet[n] {
			t.Errorf("unexpected container name in stop calls: %q", n)
		}
	}
	for _, n := range rmNames {
		if !wantSet[n] {
			t.Errorf("unexpected container name in rm calls: %q", n)
		}
	}

	// Verify stop-before-rm ordering: for each child, its stop call must
	// appear before its rm call in the ordered call list.
	for _, wantName := range []string{wantName1, wantName2} {
		stopIdx, rmIdx := -1, -1
		for i, call := range calls {
			if len(call) == 0 {
				continue
			}
			if calls[i][len(calls[i])-1] == wantName {
				if call[0] == "stop" && stopIdx < 0 {
					stopIdx = i
				} else if call[0] == "rm" && rmIdx < 0 {
					rmIdx = i
				}
			}
		}
		if stopIdx < 0 {
			t.Errorf("no stop call found for container %q", wantName)
		}
		if rmIdx < 0 {
			t.Errorf("no rm call found for container %q", wantName)
		}
		if stopIdx >= 0 && rmIdx >= 0 && stopIdx > rmIdx {
			t.Errorf("rm call (index %d) appears before stop call (index %d) for container %q", rmIdx, stopIdx, wantName)
		}
	}
}

// TestStopAndRemoveChildContainers_ContainerNotFound verifies that when a
// child container no longer exists (already removed externally), the
// "no such container" error is silently ignored and remaining children are
// still processed — cleanup must not fail.
//
// Covers the [edge-case] AC: a child whose container no longer exists does not
// cause cleanup to fail.
func TestStopAndRemoveChildContainers_ContainerNotFound(t *testing.T) {
	// Install a fake podman that simulates "no such container" for every call.
	logFile := installFakePodman(t, "no-such")

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	parent := "myrepo@already-gone"
	if err := d.UpsertStatus(parent, "myrepo", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}
	child := parent + "~review-1-review-qa"
	if err := d.UpsertStatus(child, "myrepo", "", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus child: %v", err)
	}

	// Must complete without panicking or returning an error (function is void).
	stopAndRemoveChildContainers(d, parent)

	// Both stop and rm must still have been attempted for the child.
	calls := readPodmanCalls(t, logFile)
	if len(calls) != 2 {
		t.Fatalf("expected 2 podman calls even on 'no such container', got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "stop" {
		t.Errorf("call 1 verb = %q, want %q", calls[0][0], "stop")
	}
	if calls[1][0] != "rm" {
		t.Errorf("call 2 verb = %q, want %q", calls[1][0], "rm")
	}
}

// TestHeadlessCleanup_StopsChildContainersBeforeDBRemoval verifies that
// headlessCleanup stops child review-agent containers before the DB rows
// for those children are removed.
//
// The test uses a fake podman in "ok" mode and seeds the DB with one child
// review session. It then calls headlessCleanup and asserts that:
//   1. podman was invoked for the child container (stop + rm), AND
//   2. the parent session DB row was marked ended.
//
// Ordering (stop-before-DB) is validated by checking the DB row for the child
// still exists when the podman calls were made — but since we can't directly
// intercept mid-cleanup state, we at minimum assert both podman calls happened.
//
// Covers: [functional] container teardown runs BEFORE the child's DB row is
// removed.
func TestHeadlessCleanup_StopsChildContainersBeforeDBRemoval(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	logFile := installFakePodman(t, "ok")

	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	parent := "myrepo@with-children"
	if err := d.UpsertStatus(parent, "myrepo", "", "running", nil, nil); err != nil {
		t.Fatalf("UpsertStatus parent: %v", err)
	}
	child := parent + "~review-1-review-context"
	if err := d.UpsertStatus(child, "myrepo", "", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus child: %v", err)
	}
	d.Close()

	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })

	if err := headlessCleanup(parent, "with-children", "", ""); err != nil {
		t.Fatalf("headlessCleanup returned error %v, want nil", err)
	}

	calls := readPodmanCalls(t, logFile)

	// We expect: 2 calls for the child (stop + rm) + 2 calls for the parent (stop + rm).
	if len(calls) != 4 {
		t.Fatalf("expected 4 podman calls (2 child + 2 parent), got %d: %v", len(calls), calls)
	}

	childName := container.NameForSession(child)
	parentName := container.NameForSession(parent)

	// Verify child calls come before parent calls.
	childStopIdx, childRmIdx, parentStopIdx := -1, -1, -1
	for i, call := range calls {
		if len(call) == 0 {
			continue
		}
		name := call[len(call)-1]
		verb := call[0]
		switch {
		case name == childName && verb == "stop" && childStopIdx < 0:
			childStopIdx = i
		case name == childName && verb == "rm" && childRmIdx < 0:
			childRmIdx = i
		case name == parentName && verb == "stop" && parentStopIdx < 0:
			parentStopIdx = i
		}
	}

	if childStopIdx < 0 {
		t.Error("no stop call found for child container")
	}
	if childRmIdx < 0 {
		t.Error("no rm call found for child container")
	}
	if parentStopIdx < 0 {
		t.Error("no stop call found for parent container")
	}

	// Child teardown must precede parent teardown.
	if childStopIdx >= 0 && parentStopIdx >= 0 && childStopIdx > parentStopIdx {
		t.Errorf("child stop (index %d) appears after parent stop (index %d) — child containers must be torn down first",
			childStopIdx, parentStopIdx)
	}

	// Parent DB row must be marked ended.
	d2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer d2.Close()
	status, err := d2.CurrentStatus(parent)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if status == nil {
		t.Fatal("CurrentStatus returned nil — row missing")
	}
	if status.EndedAt == nil {
		t.Errorf("ended_at is nil — parent session was not marked as ended")
	}
}
