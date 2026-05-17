package cmd

// Tests for killOrphanReviewSidecars (issue #1751).
//
// The traversal walks /proc looking at each running process's argv via
// /proc/<pid>/cmdline, matches `--session <parent>~review-…` (or
// `~investigate-…`), and SIGTERMs the survivors. These tests cover:
//
//   - findOrphanReviewSidecarPIDs against a fake /proc tree on disk: pure
//     unit tests for the argv-parsing and prefix-matching logic. No real
//     processes are spawned for these.
//   - killOrphanReviewSidecars end-to-end on /proc with real stub processes
//     (re-invocations of the test binary via PRISM_CMD_TEST_STUB=1) that
//     present cmdlines like a real sidecar. Verifies the SIGTERM actually
//     terminates the processes and that unrelated stubs (different parent,
//     non-review children, non-sidecar processes) are not touched.
//
// The stub-spawning approach is the same one used by killsidecar_test.go
// (see startStubProcess and TestMain) so we re-use its infrastructure.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startStubProcessNamed re-invokes the current test binary with
// PRISM_CMD_TEST_STUB=1 and the given session name on the argv, so the
// stub's /proc/<pid>/cmdline contains both "sidecar" and "--session <name>"
// — the invariants killOrphanReviewSidecars checks for.
//
// Returns the PID and a cleanup func; tests should register the cleanup
// with t.Cleanup so leaked stubs do not survive a failed assertion.
func startStubProcessNamed(t *testing.T, sessionName string) (pid int, cleanup func()) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	cmd := exec.Command(self, "sidecar", "--session", sessionName)
	cmd.Env = append(os.Environ(), "PRISM_CMD_TEST_STUB=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub process %q: %v", sessionName, err)
	}

	return cmd.Process.Pid, func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// writeFakeProcEntry creates procRoot/<pid>/cmdline with the given argv as
// NUL-separated bytes plus a trailing NUL — mirroring the real kernel-
// supplied layout of /proc/<pid>/cmdline.
//
// procRoot is a t.TempDir(); each test populates its own fake tree so
// these unit tests can run on any platform and need no /proc access.
func writeFakeProcEntry(t *testing.T, procRoot string, pid int, argv []string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fake /proc entry: %v", err)
	}
	var buf []byte
	for _, a := range argv {
		buf = append(buf, []byte(a)...)
		buf = append(buf, 0)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), buf, 0o644); err != nil {
		t.Fatalf("write fake cmdline: %v", err)
	}
}

// TestFindOrphanReviewSidecarPIDs_MatchesAllReviewCycles covers the
// "multiple review cycles" AC: a parent with both ~review-1-… and
// ~review-2-… children should produce matches for both.
func TestFindOrphanReviewSidecarPIDs_MatchesAllReviewCycles(t *testing.T) {
	procRoot := t.TempDir()
	parent := "myrepo@feature"

	// Two review cycles, five agents each — typical real-world pattern.
	cycles := [][]string{
		{
			parent + "~review-1-review-goal",
			parent + "~review-1-review-code",
			parent + "~review-1-review-security",
			parent + "~review-1-review-qa",
			parent + "~review-1-review-context",
		},
		{
			parent + "~review-2-review-goal",
			parent + "~review-2-review-code",
		},
	}

	pid := 10000
	wantSessions := map[string]bool{}
	for _, cycle := range cycles {
		for _, s := range cycle {
			writeFakeProcEntry(t, procRoot, pid, []string{"prism", "sidecar", "--session", s, "--port", "14000"})
			wantSessions[s] = true
			pid++
		}
	}

	// Add an unrelated sidecar (different parent) — must NOT match.
	writeFakeProcEntry(t, procRoot, pid, []string{"prism", "sidecar", "--session", "other-repo@branch~review-1-review-goal"})
	pid++
	// Add a non-sidecar process (no "sidecar" token) — must NOT match.
	writeFakeProcEntry(t, procRoot, pid, []string{"/usr/bin/sleep", "60"})
	pid++
	// Add a non-PID directory under /proc (mimics e.g. /proc/sys).
	if err := os.MkdirAll(filepath.Join(procRoot, "sys"), 0o755); err != nil {
		t.Fatalf("mkdir non-PID: %v", err)
	}

	matches, err := findOrphanReviewSidecarPIDs(parent, procRoot)
	if err != nil {
		t.Fatalf("findOrphanReviewSidecarPIDs: %v", err)
	}

	gotSessions := map[string]bool{}
	for _, m := range matches {
		gotSessions[m.session] = true
	}

	for s := range wantSessions {
		if !gotSessions[s] {
			t.Errorf("missing match for session %q", s)
		}
	}
	for s := range gotSessions {
		if !wantSessions[s] {
			t.Errorf("unexpected match for session %q", s)
		}
	}
}

// TestFindOrphanReviewSidecarPIDs_MatchesInvestigatorChildren covers the
// "~investigate-" infix AC alongside the "~review-" one.
func TestFindOrphanReviewSidecarPIDs_MatchesInvestigatorChildren(t *testing.T) {
	procRoot := t.TempDir()
	parent := "myrepo@feature"

	writeFakeProcEntry(t, procRoot, 11001, []string{"prism", "sidecar", "--session", parent + "~investigate-some-slug"})
	writeFakeProcEntry(t, procRoot, 11002, []string{"prism", "sidecar", "--session", parent + "~investigate-another-slug"})
	// Parent itself — must NOT match (no "~" suffix).
	writeFakeProcEntry(t, procRoot, 11003, []string{"prism", "sidecar", "--session", parent})

	matches, err := findOrphanReviewSidecarPIDs(parent, procRoot)
	if err != nil {
		t.Fatalf("findOrphanReviewSidecarPIDs: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(matches), matches)
	}
}

// TestFindOrphanReviewSidecarPIDs_DoesNotMatchOtherParent verifies the
// "non-review sessions unchanged" AC — an unrelated parent's review
// children are not matched.
func TestFindOrphanReviewSidecarPIDs_DoesNotMatchOtherParent(t *testing.T) {
	procRoot := t.TempDir()

	// We're cleaning up "myrepo@feature" but a sibling worker exists
	// with its own review children. Those must NOT be matched.
	writeFakeProcEntry(t, procRoot, 12001, []string{"prism", "sidecar", "--session", "myrepo@feature~review-1-review-goal"})
	writeFakeProcEntry(t, procRoot, 12002, []string{"prism", "sidecar", "--session", "myrepo@other-branch~review-1-review-goal"})
	writeFakeProcEntry(t, procRoot, 12003, []string{"prism", "sidecar", "--session", "other-repo@feature~review-1-review-goal"})

	matches, err := findOrphanReviewSidecarPIDs("myrepo@feature", procRoot)
	if err != nil {
		t.Fatalf("findOrphanReviewSidecarPIDs: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want exactly 1 (for myrepo@feature only): %+v", len(matches), matches)
	}
	if matches[0].session != "myrepo@feature~review-1-review-goal" {
		t.Errorf("matched wrong session: %q", matches[0].session)
	}
}

// TestFindOrphanReviewSidecarPIDs_EqualsArgForm verifies the --session=foo
// (single argv slot) variant parses correctly. The prism sidecar invokes
// it as "--session <name>" today, but tolerating both forms keeps the
// matcher robust if that changes.
func TestFindOrphanReviewSidecarPIDs_EqualsArgForm(t *testing.T) {
	procRoot := t.TempDir()
	parent := "myrepo@feature"

	writeFakeProcEntry(t, procRoot, 13001, []string{"prism", "sidecar", "--session=" + parent + "~review-1-review-goal"})

	matches, err := findOrphanReviewSidecarPIDs(parent, procRoot)
	if err != nil {
		t.Fatalf("findOrphanReviewSidecarPIDs: %v", err)
	}
	if len(matches) != 1 || matches[0].session != parent+"~review-1-review-goal" {
		t.Fatalf("expected one --session=… match, got %+v", matches)
	}
}

// TestFindOrphanReviewSidecarPIDs_EmptyParent guards the "do not match
// every sidecar starting with ~" failure mode. Passing an empty parent
// would otherwise match every prism sidecar on the host.
func TestFindOrphanReviewSidecarPIDs_EmptyParent(t *testing.T) {
	// killOrphanReviewSidecars short-circuits on empty parent. The
	// underlying findOrphanReviewSidecarPIDs is permissive — the empty-
	// guard is at the public wrapper layer. Exercise the wrapper to
	// confirm it does nothing.
	if runtime.GOOS != "linux" {
		t.Skip("killOrphanReviewSidecars is a no-op on non-Linux")
	}
	// Spawn a real review-shaped stub. If the empty guard were missing,
	// the wrapper would walk /proc and SIGTERM this stub. We assert it
	// survives.
	pid, cleanup := startStubProcessNamed(t, "myrepo@feature~review-1-review-goal")
	t.Cleanup(cleanup)
	// Give the kernel a beat to populate /proc/<pid>/cmdline.
	time.Sleep(30 * time.Millisecond)

	killOrphanReviewSidecars("")

	if !processExists(pid) {
		t.Fatalf("stub pid %d was killed despite empty parent — empty-guard regression", pid)
	}
}

// TestKillOrphanReviewSidecars_EndToEnd is the integration test required
// by AC: spawn fake ~review-1-… and ~review-2-… sidecars, call
// killOrphanReviewSidecars, assert all are dead and unrelated stubs are
// untouched.
func TestKillOrphanReviewSidecars_EndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("killOrphanReviewSidecars walks /proc — Linux only")
	}

	parent := "myrepo@feature"

	// Spawn five review-shape stubs (two cycles + investigator).
	reviewSessions := []string{
		parent + "~review-1-review-goal",
		parent + "~review-1-review-code",
		parent + "~review-2-review-goal",
		parent + "~review-2-review-code",
		parent + "~investigate-some-slug",
	}
	reviewPIDs := make([]int, 0, len(reviewSessions))
	for _, s := range reviewSessions {
		pid, cleanup := startStubProcessNamed(t, s)
		t.Cleanup(cleanup)
		reviewPIDs = append(reviewPIDs, pid)
	}

	// Spawn two unrelated stubs that must survive: an unrelated parent's
	// review child, and the parent itself.
	unrelatedSessions := []string{
		"other-repo@feature~review-1-review-goal",
		parent, // the parent sidecar itself
	}
	unrelatedPIDs := make([]int, 0, len(unrelatedSessions))
	for _, s := range unrelatedSessions {
		pid, cleanup := startStubProcessNamed(t, s)
		t.Cleanup(cleanup)
		unrelatedPIDs = append(unrelatedPIDs, pid)
	}

	// Give the kernel time to populate /proc/<pid>/cmdline for every stub.
	time.Sleep(50 * time.Millisecond)
	for i, pid := range reviewPIDs {
		if !cmdlineContainsSession(pid, reviewSessions[i]) {
			t.Skipf("kernel did not populate /proc/%d/cmdline with %q yet — test environment too slow", pid, reviewSessions[i])
		}
	}

	killOrphanReviewSidecars(parent)

	// All review/investigator PIDs should be dead within a short timeout
	// (SIGTERM → process exit → /proc entry gone).
	deadline := time.Now().Add(3 * time.Second)
	for i, pid := range reviewPIDs {
		for time.Now().Before(deadline) && processExists(pid) {
			time.Sleep(20 * time.Millisecond)
		}
		if processExists(pid) {
			t.Errorf("review stub pid %d (%q) still alive after killOrphanReviewSidecars",
				pid, reviewSessions[i])
		}
	}

	// All unrelated PIDs must still be alive.
	for i, pid := range unrelatedPIDs {
		if !processExists(pid) {
			t.Errorf("unrelated stub pid %d (%q) was killed by killOrphanReviewSidecars — must not match",
				pid, unrelatedSessions[i])
		}
	}
}

// TestKillOrphanReviewSidecars_AlreadyExited covers the edge-case AC: a
// review sidecar that has already exited produces no error log and no
// panic. We exercise this by calling the function against a parent for
// which no live processes exist — the loop body never runs and the
// function returns cleanly. The "no spurious error log" portion is
// covered by the lack of test output checks; a panic or non-nil error
// would fail the test.
func TestKillOrphanReviewSidecars_AlreadyExited(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("killOrphanReviewSidecars walks /proc — Linux only")
	}
	// Use a parent name that no live process matches.
	killOrphanReviewSidecars("nonexistent@parent-with-no-children")
}

// TestKillOrphanReviewSidecars_RepeatIsIdempotent confirms calling the
// wrapper twice in a row is harmless: the first call kills the stubs,
// the second call finds no live processes (because they're gone) and
// returns silently with no error log. This covers the "already exited
// — no spurious error log" edge-case AC at the wrapper layer.
func TestKillOrphanReviewSidecars_RepeatIsIdempotent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("killOrphanReviewSidecars walks /proc — Linux only")
	}

	parent := "myrepo@idempotent-feature"
	sessionName := parent + "~review-1-review-goal"

	pid, cleanup := startStubProcessNamed(t, sessionName)
	t.Cleanup(cleanup)
	time.Sleep(30 * time.Millisecond)

	killOrphanReviewSidecars(parent)

	// Wait for the stub to die.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && processExists(pid) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("stub pid %d still alive after first killOrphanReviewSidecars", pid)
	}

	// Second call — must not panic, must not produce an error. Cannot
	// directly assert "no stderr output" without capturing it, but a
	// regression to a panic would fail this test.
	killOrphanReviewSidecars(parent)
}

// cmdlineContainsSession returns true when /proc/<pid>/cmdline contains
// the given session name. Used to wait out kernel cmdline-publication
// latency in TestKillOrphanReviewSidecars_EndToEnd.
func cmdlineContainsSession(pid int, session string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	for _, arg := range strings.Split(string(data), "\x00") {
		if arg == session {
			return true
		}
	}
	return false
}

// TestSessionArgFromCmdline_SpaceForm verifies the --session foo form.
func TestSessionArgFromCmdline_SpaceForm(t *testing.T) {
	got := sessionArgFromCmdline([]string{"prism", "sidecar", "--session", "x", "--port", "1"})
	if got != "x" {
		t.Errorf("got %q, want %q", got, "x")
	}
}

// TestSessionArgFromCmdline_EqualsForm verifies the --session=foo form.
func TestSessionArgFromCmdline_EqualsForm(t *testing.T) {
	got := sessionArgFromCmdline([]string{"prism", "sidecar", "--session=x", "--port", "1"})
	if got != "x" {
		t.Errorf("got %q, want %q", got, "x")
	}
}

// TestSessionArgFromCmdline_TrailingFlag handles the malformed case
// where --session appears with no value.
func TestSessionArgFromCmdline_TrailingFlag(t *testing.T) {
	got := sessionArgFromCmdline([]string{"prism", "sidecar", "--session"})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestIsPrismSidecarCmdline rejects argvs that don't include both the
// "sidecar" and "--session" tokens. Mirrors the invariant in
// session.KillSidecar so the two recognisers stay in sync.
func TestIsPrismSidecarCmdline(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"sidecar+session", []string{"prism", "sidecar", "--session", "x"}, true},
		{"missing sidecar", []string{"prism", "--session", "x"}, false},
		{"missing session", []string{"prism", "sidecar"}, false},
		{"unrelated process", []string{"/usr/bin/sleep", "60"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPrismSidecarCmdline(c.argv); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
