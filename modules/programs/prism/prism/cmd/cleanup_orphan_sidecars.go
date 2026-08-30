package cmd

// cleanup_orphan_sidecars.go — find and SIGTERM orphan review-group and
// investigator-group sidecar processes that outlived their DB rows.
//
// Background: when a worker session is cleaned up after its PR
// is merged, `prism cleanup --yes --session <worker>` clears the worker's
// own state and runs review.CleanupReviewSessionsForParent which cleans up
// DB rows for known review children. However, sidecar processes spawned
// for review-group children (`<worker>~review-N-<role>`) and investigator
// children (`<worker>~investigate-<slug>`) are not always reachable through
// the DB by the time cleanup runs — their DB rows may have been ended /
// purged earlier, or they may have been spawned without a DB row at all.
// The OS-level sidecar processes then leak: each holds bwrap mounts, a
// port allocation, and resident memory.
//
// The traversal here is the cleanup-side safety net. It is intentionally
// process-tree based rather than DB-based so it works regardless of DB
// state, regardless of whether the worker's worktree still exists, and
// regardless of when in the cleanup sequence it runs.
//
// Linux-only: it walks /proc. On other platforms it is a no-op (NixOS is
// the supported deployment target and prism CI runs on Linux). macOS dev
// boxes still call this; it returns silently rather than erroring.

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/prismatic-koi/prism/internal/proglog"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// killOrphanReviewSidecars enumerates running prism sidecar processes whose
// `--session` argv argument has the prefix "<parentSession>~" and an infix
// of "~review-" or "~investigate-" after that prefix, then sends SIGTERM
// to each. The corresponding sidecar PID file is also removed on a best-
// effort basis so a subsequent KillSidecar call does not stumble over it.
//
// The function is tolerant of races: processes that exit between
// enumeration and signal are silently ignored (ESRCH). It is also
// idempotent — calling it twice in a row simply finds zero matching
// processes the second time.
//
// On non-Linux platforms this is a no-op (returns immediately). The
// project's deployment target is NixOS; macOS-side cleanup of leaked
// sidecars is out of scope.
//
// The parentSession argument must not be empty — callers should guard
// against that (it would otherwise match every sidecar process whose
// session name starts with "~", which is meaningless).
func killOrphanReviewSidecars(parentSession string) {
	if parentSession == "" {
		return
	}
	if runtime.GOOS != "linux" {
		return
	}

	pids, err := findOrphanReviewSidecarPIDs(parentSession, "/proc")
	if err != nil {
		// /proc is missing or unreadable — non-fatal; we just can't
		// do the traversal. Log to stderr so the caller has a
		// breadcrumb but do not return an error.
		proglog.Warnf("[prism] warning: killOrphanReviewSidecars: enumerate /proc: %v\n", err)
		return
	}

	for _, match := range pids {
		// SIGTERM first — graceful shutdown gives the sidecar a chance to
		// clean up its socket and PID file. ESRCH is benign (process
		// already exited between enumeration and signal).
		if err := syscall.Kill(match.pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			proglog.Warnf("[prism] warning: killOrphanReviewSidecars: kill pid %d (session %q): %v\n",
				match.pid, match.session, err)
			continue
		}
		// Best-effort PID file removal so a later KillSidecar() doesn't
		// trip over a stale PID file pointing at a now-dead process.
		// SidecarPIDPath is pure (just path construction) so it is safe
		// to call even after the process is gone.
		removeSidecarPIDFile(match.session)
	}
}

// sidecarMatch records the PID and session name of a sidecar that
// killOrphanReviewSidecars determined belongs to a review or
// investigator child of the parent.
type sidecarMatch struct {
	pid     int
	session string
}

// findOrphanReviewSidecarPIDs walks procRoot (typically "/proc") looking
// for prism sidecar processes whose --session argv arg matches the parent
// session's review or investigator children. It is exported as a
// lower-case helper for testing — tests can populate a fake /proc tree in
// a t.TempDir() and pass that path.
//
// Matching criteria (all must hold):
//
//  1. /proc/<pid>/cmdline is readable.
//  2. The cmdline contains both "sidecar" and "--session" tokens — the
//     same invariant session.KillSidecar uses to recognise prism
//     sidecars. We do not require the binary basename to be "prism"
//     because `go test` re-invokes the test binary as a stub sidecar
//     (see killsidecar_test.go::startStubProcess) and its argv[0] is
//     "*.test", not "prism".
//  3. The --session flag's value has the prefix "<parentSession>~".
//  4. The suffix after that prefix begins with "review-" or
//     "investigate-". This intentionally excludes the parent itself
//     (whose --session value is exactly parentSession with no "~")
//     and any unrelated sub-session shape we have not yet introduced.
func findOrphanReviewSidecarPIDs(parentSession, procRoot string) ([]sidecarMatch, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	prefix := parentSession + "~"
	var matches []sidecarMatch
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// /proc contains many non-numeric entries (self, sys, etc.).
		// Skip anything that isn't a PID.
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		cmdlineBytes, err := os.ReadFile(filepath.Join(procRoot, name, "cmdline"))
		if err != nil {
			// Process exited between ReadDir and ReadFile, or we
			// can't read it (permissions). Skip silently — this is
			// the common race when /proc churns.
			continue
		}
		// /proc/<pid>/cmdline uses NUL byte separators between argv
		// entries and ends with a trailing NUL. Splitting on "\x00"
		// recovers the argv slice; a final empty element from the
		// trailing NUL is harmless to leave in.
		argv := strings.Split(string(cmdlineBytes), "\x00")
		if !isPrismSidecarCmdline(argv) {
			continue
		}
		sessionArg := sessionArgFromCmdline(argv)
		if sessionArg == "" {
			continue
		}
		if !strings.HasPrefix(sessionArg, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(sessionArg, prefix)
		if !strings.HasPrefix(suffix, "review-") && !strings.HasPrefix(suffix, "investigate-") {
			continue
		}
		matches = append(matches, sidecarMatch{pid: pid, session: sessionArg})
	}
	return matches, nil
}

// isPrismSidecarCmdline reports whether argv looks like a prism sidecar
// invocation: it must contain both a "sidecar" token and a "--session"
// flag (in either "--session foo" or "--session=foo" form). This matches
// the invariant in session.KillSidecar so the two recognisers stay in
// lockstep.
func isPrismSidecarCmdline(argv []string) bool {
	hasSidecar := false
	hasSession := false
	for _, a := range argv {
		if a == "sidecar" {
			hasSidecar = true
		}
		if a == "--session" || strings.HasPrefix(a, "--session=") {
			hasSession = true
		}
	}
	return hasSidecar && hasSession
}

// sessionArgFromCmdline returns the value of the --session flag in argv,
// supporting both "--session foo" (two argv slots) and "--session=foo"
// (single argv slot). Returns "" when --session is absent or the
// argument is malformed (e.g. trailing --session with no value).
func sessionArgFromCmdline(argv []string) string {
	for i, a := range argv {
		if a == "--session" {
			if i+1 < len(argv) {
				return argv[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, "--session=") {
			return strings.TrimPrefix(a, "--session=")
		}
	}
	return ""
}

// removeSidecarPIDFile removes the PID file for sessionName best-effort.
// Used after we have SIGTERMed a process we discovered via /proc — the
// PID file (if any) is now stale and would only mislead a later
// KillSidecar call.
func removeSidecarPIDFile(sessionName string) {
	pidPath, err := prismSession.SidecarPIDPath(sessionName)
	if err != nil {
		return
	}
	_ = os.Remove(pidPath)
}
