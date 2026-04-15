package integration_test

// TestConcurrentContainerSQLite validates that two (and four) concurrent
// container sessions sharing the same host opencode state directory
// do not produce SQLite lock errors.
//
// Background: every container mounts an opencode state directory read-write
// into /root/.local/share/opencode inside the container (container.go:319–320).
// opencode uses SQLite for its state DB inside that directory. If opencode uses
// the default journal mode (exclusive write locks), two concurrent writers
// produce SQLITE_BUSY errors immediately. This test validates the scenario.
//
// A temporary directory (t.TempDir()) is used as the shared state directory
// rather than ~/.local/share/opencode. This keeps the test hermetic and
// prevents the :Z SELinux relabel from stealing the SELinux context of live
// prism session containers that also mount ~/.local/share/opencode.
//
// # Scope and rationale
//
// The full AC-1 spec calls for delivering a prompt to each session and waiting
// for the `finished` state in the prism DB. That path requires valid API keys,
// outbound network access, and a fully wired prism session (tmux, sidecar, agent
// plugin) — dependencies that are not available in automated test environments.
//
// This test exercises the narrower but critical sub-scenario: what happens to
// the opencode SQLite DB when two (or four) containers open it concurrently at
// startup, before any prompts are delivered. This is the point of maximum DB
// contention — multiple writers racing to initialise schema, set PRAGMAs, or
// write session startup rows — and is the scenario most likely to surface
// SQLITE_BUSY errors. If opencode uses WAL mode (which it likely does as a
// modern application), concurrent connections coexist without error. If it uses
// the default journal mode, SQLITE_BUSY will appear immediately at startup.
//
// Manual validation of the full prompt→finished path (AC-1 items 2–3) should
// be done as a runbook against a live environment with real API credentials.
//
// # Test approach
//
//  1. Start N containers (2 or 4) running "opencode serve", each mounting the
//     same host opencode state directory.
//  2. Allow a start-up pause so all containers initialise and open their DBs.
//  3. Let them run concurrently for a 15-second observation window.
//  4. Collect combined logs (stdout+stderr) from each container.
//  5. Assert that no SQLITE_BUSY / "database is locked" / "disk I/O error"
//     appears in any container's log output.
//
// The test is skipped automatically when:
//   - podman is not in PATH (AC-5)
//   - the prism-agent image is not present locally (container mode not set up)
//
// All containers are removed in t.Cleanup even on failure (AC-6).

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// observationWindow is how long we let the containers run while collecting
	// logs. opencode serve starts and opens its SQLite DB within a few seconds;
	// any immediate lock contention will surface during this 15-second window.
	observationWindow = 15 * time.Second

	// containerStartWait is the pause between starting all containers and
	// beginning the observation window. Gives all N containers time to start
	// their podman processes before we check for errors.
	containerStartWait = 3 * time.Second

	// sqliteLockPatterns are the strings we scan for in container logs.
	// Any match fails the test.
	sqliteBusyPattern   = "SQLITE_BUSY"
	sqliteLockedPattern = "database is locked"
	sqliteIOPattern     = "disk I/O error"
)

// requirePodman skips the test if podman is not available in PATH.
// Returns the path to the podman binary.
func requirePodman(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman not found in PATH — skipping container concurrency test")
	}
	return bin
}

// requirePrismAgentImage skips the test if the localhost/prism-agent:latest image is not
// present locally. The image must be built before container mode can be used;
// this guard prevents the test from failing with a confusing "pull" error.
func requirePrismAgentImage(t *testing.T, podmanBin string) {
	t.Helper()
	out, err := exec.Command(podmanBin, "image", "inspect", "localhost/prism-agent:latest",
		"--format", "{{.ID}}").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skip("localhost/prism-agent:latest image not found — build the image first to run container concurrency tests")
	}
}

// containerResult holds the collected output and any error from a single
// concurrent container run.
type containerResult struct {
	id  int
	log string
	err error
}

// runConcurrentContainers starts n containers, each mounting a shared
// temporary opencode state dir, lets them run for observationWindow, then
// collects logs. Returns the collected results.
func runConcurrentContainers(t *testing.T, podmanBin string, n int) []containerResult {
	t.Helper()

	// Use an isolated temp directory as the shared opencode state dir.
	// t.TempDir() is created automatically and cleaned up by the test framework.
	// This keeps the test hermetic and avoids the :Z SELinux relabel from
	// affecting ~/.local/share/opencode or any live prism session containers
	// that mount that path.
	stateDir := t.TempDir()

	// Use a stable, test-unique prefix so cleanup is reliable.
	prefix := fmt.Sprintf("prism-sqlite-concurrency-test-%d", time.Now().UnixNano())

	// Track container names for cleanup.
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("%s-%d", prefix, i)
	}

	// Register cleanup before any containers are created so they are removed
	// even if the test fails mid-way through startup. podman rm --ignore is a
	// no-op for containers that were never created, so this is always safe.
	t.Cleanup(func() {
		for _, name := range names {
			out, err := exec.Command(podmanBin, "rm", "--force", "--ignore", name).CombinedOutput()
			if err != nil {
				t.Logf("cleanup: podman rm %q: %v — %s", name, err, strings.TrimSpace(string(out)))
			}
		}
	})

	// Start all N containers concurrently. Each mounts the shared opencode
	// state dir and runs opencode serve, exactly as the prism sidecar does.
	var startWG sync.WaitGroup
	startErrors := make([]error, n)

	for i := 0; i < n; i++ {
		i := i
		startWG.Add(1)
		go func() {
			defer startWG.Done()
			args := []string{
				"run",
				"--detach",
				"--name", names[i],
				// Only the opencode state mount matters for this test — this is
				// the shared read-write SQLite directory under scrutiny.
				"--volume", stateDir + ":/root/.local/share/opencode:Z",
				// No worktree, no git, no credentials — opencode serve will
				// start, open its DB, and sit idle. That is sufficient to
				// exercise startup SQLite contention (see file-level doc comment
				// for the rationale on why idle startup is the key scenario).
				"localhost/prism-agent:latest",
				"opencode", "serve",
				"--port", "4096",
				"--hostname", "0.0.0.0",
			}
			out, err := exec.Command(podmanBin, args...).CombinedOutput()
			if err != nil {
				startErrors[i] = fmt.Errorf("start container %d (%q): %w\n%s",
					i, names[i], err, strings.TrimSpace(string(out)))
			}
		}()
	}
	startWG.Wait()

	for i, err := range startErrors {
		if err != nil {
			t.Fatalf("container %d failed to start: %v", i, err)
		}
	}

	// Brief pause to let all containers initialise and open their DBs.
	time.Sleep(containerStartWait)

	// Observation window: let the containers run and accumulate log output.
	time.Sleep(observationWindow)

	// Collect logs from all containers concurrently.
	results := make([]containerResult, n)
	var logWG sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		logWG.Add(1)
		go func() {
			defer logWG.Done()
			var buf bytes.Buffer
			cmd := exec.Command(podmanBin, "logs", "--timestamps", names[i])
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			if err := cmd.Run(); err != nil {
				results[i] = containerResult{
					id:  i,
					log: buf.String(),
					err: fmt.Errorf("podman logs %q: %w", names[i], err),
				}
				return
			}
			results[i] = containerResult{id: i, log: buf.String()}
		}()
	}
	logWG.Wait()

	return results
}

// assertNoSQLiteLockErrors scans all collected container logs for SQLite lock
// error patterns and fails the test if any are found.
func assertNoSQLiteLockErrors(t *testing.T, results []containerResult) {
	t.Helper()
	lockPatterns := []string{
		sqliteBusyPattern,
		sqliteLockedPattern,
		sqliteIOPattern,
	}
	for _, r := range results {
		if r.err != nil {
			t.Errorf("container %d: log collection error: %v", r.id, r.err)
			continue
		}
		foundError := false
		for _, pat := range lockPatterns {
			if strings.Contains(r.log, pat) {
				foundError = true
				t.Errorf(
					"container %d: SQLite lock error %q detected in logs.\n"+
						"This indicates the shared opencode state directory "+
						"causes lock contention when accessed from "+
						"multiple concurrent containers.\n\n"+
						"Container log excerpt:\n%s",
					r.id, pat, truncateLog(r.log, 2000),
				)
			}
		}
		if !foundError {
			t.Logf("container %d: %d log bytes, no SQLite lock errors", r.id, len(r.log))
		}
	}
}

// truncateLog returns the last n bytes of a log string for compact error output.
func truncateLog(log string, n int) string {
	if len(log) <= n {
		return log
	}
	return "...(truncated)...\n" + log[len(log)-n:]
}

// TestConcurrentContainerSQLite_TwoSessions spawns two concurrent container
// sessions sharing a temporary opencode state directory and verifies no SQLite
// lock errors occur. This is the minimum concurrent load scenario (AC-1).
//
// Skipped automatically when podman is not available or the prism-agent image
// is not present (AC-5).
func TestConcurrentContainerSQLite_TwoSessions(t *testing.T) {
	podmanBin := requirePodman(t)
	requirePrismAgentImage(t, podmanBin)

	t.Log("starting 2 concurrent containers sharing a temporary opencode state directory")
	results := runConcurrentContainers(t, podmanBin, 2)
	assertNoSQLiteLockErrors(t, results)
}

// TestConcurrentContainerSQLite_FourSessions extends the two-session test to
// four concurrent sessions (AC-4). Heavier concurrent load surfaces lock
// contention that may not appear with just two writers.
//
// Skipped automatically when podman is not available or the prism-agent image
// is not present (AC-5).
func TestConcurrentContainerSQLite_FourSessions(t *testing.T) {
	podmanBin := requirePodman(t)
	requirePrismAgentImage(t, podmanBin)

	t.Log("starting 4 concurrent containers sharing a temporary opencode state directory")
	results := runConcurrentContainers(t, podmanBin, 4)
	assertNoSQLiteLockErrors(t, results)
}
