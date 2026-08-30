package cmd

// Orphan-container sweep for sessions with containers_enabled=1.
//
// The sweep complements the per-mode container teardown that
// `removeContainerIfExists` already performs: that path removes the
// SESSION'S OWN bwrap/podman/sandbox-exec container (the agent's
// runtime), while this path sweeps any DERIVATIVE containers the
// agent created via the proxy during the session. The naming
// convention enforced by `internal/podmanproxy::applyContainerNamePolicy`
// is `prism-<sessionName>-<8 hex chars>` — the sweep filter targets
// exactly that strict shape so a sibling session whose name shares
// a prefix (e.g. session "foo" vs session "foo-bar") cannot be
// caught by accident.
//
// The sweep is best-effort: every podman invocation is bounded by a
// shared 30-second context, and any failure (podman not on PATH,
// machine off, socket missing, rm returns non-zero) is logged at
// warning level and does NOT abort cleanup. The worktree and DB
// teardown ALWAYS run regardless of sweep outcome — the
// containers_swept count is just an observability field in the
// `--json` envelope.
//
// Test seam: the runner that shells out to `podman` is interface-
// dispatched via `podmanRunnerForTest`, set by tests through
// SetTestPodmanRunner. Production wires it to execPodmanRunner.

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/proglog"
)

// podmanSweepBudget is the upper bound on the total time the orphan
// sweep may consume per session. The two podman invocations (ps + rm)
// share this single context — there is no per-invocation budget.
//
// 30 seconds matches the per-mode container teardown in
// `removeContainerIfExists` (35s for the isolator's stop+rm). The
// sweep is downstream of that and only deals with orphan derivatives
// that the agent created, so a tighter cap is acceptable.
const podmanSweepBudget = 30 * time.Second

// podmanRunner is the test-seamable wrapper around the `podman`
// binary. Tests inject a stub via SetTestPodmanRunner; production
// uses execPodmanRunner. Keeping this interface narrow (a single
// Run method) makes the test stub trivially expressive — it just
// records arguments and returns canned output / error.
type podmanRunner interface {
	// Run invokes `podman` with the supplied args. The combined
	// stdout (and only stdout — stderr is dropped, matching the
	// docker/podman convention that progress messages should not
	// confuse the parser) is returned on success. On non-zero
	// exit, err is non-nil and stdout may carry partial output.
	Run(ctx context.Context, args ...string) (stdout []byte, err error)
}

// execPodmanRunner is the production implementation backed by
// `exec.CommandContext("podman", args...)`. The binary is resolved
// from PATH at invocation time, so a host without podman installed
// returns exec.ErrNotFound and the sweep logs a single warning then
// continues.
type execPodmanRunner struct{}

func (execPodmanRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "podman", args...)
	return cmd.Output()
}

// podmanRunnerForTest is overridden by tests. nil means "use the
// production execPodmanRunner". The package-level global mirrors
// the SetTestDBPath pattern already established for cmd-package
// tests (see cmd/db.go).
var podmanRunnerForTest podmanRunner

// SetTestPodmanRunner overrides the podman runner used by the
// orphan-container sweep. Pass nil to restore the production
// execPodmanRunner. Only intended for tests; production code MUST
// NOT call this.
func SetTestPodmanRunner(r podmanRunner) { podmanRunnerForTest = r }

func currentPodmanRunner() podmanRunner {
	if podmanRunnerForTest != nil {
		return podmanRunnerForTest
	}
	return execPodmanRunner{}
}

// sweepOrphanContainersForSession runs the orphan-container sweep
// for sessionName, gated on the session's agent_status.containers_enabled
// column. Returns:
//
//	count : number of containers force-removed.
//	ran   : true if the sweep ran (containers_enabled=1 and the DB
//	        read succeeded); false if it was a no-op (containers_enabled=0,
//	        row missing, or DB error).
//
// When ran is false the JSON envelope MUST omit the
// "containers_swept" field entirely — this is the spec.
//
// Errors from podman are NEVER fatal: they're surfaced as warnings
// via proglog.Warnf and the function returns whatever count it
// managed to confirm (0 on failure).
func sweepOrphanContainersForSession(sessionName string) (count int, ran bool) {
	d, err := openDB()
	if err != nil {
		// DB unavailable. The cleanup caller logs the DB-open error
		// separately; we silently skip the sweep here. Returning
		// ran=false is structurally correct: a session whose
		// containers_enabled value we could not read is treated as
		// "do not issue podman commands". This is the safe
		// direction — we'd rather skip a sweep than spawn a
		// spurious warning for a session that didn't use the proxy.
		return 0, false
	}
	defer d.Close()
	status, err := d.CurrentStatus(sessionName)
	if err != nil || status == nil || !status.ContainersEnabled {
		return 0, false
	}
	return sweepWithRunner(currentPodmanRunner(), sessionName), true
}

// containerNamePattern returns the strict regex that matches the
// auto-prefix container name shape the proxy produces:
// `prism-<sessionName>-<8 lowercase hex chars>`.
//
// Strict-shape filtering (not just "starts with prefix") is a
// security requirement: a sibling session "foo-bar" whose containers are
// "prism-foo-bar-XXXXXXXX" must NOT be swept when cleaning session
// "foo". Simple prefix matching cannot distinguish those two cases
// because "prism-foo-bar-XXXXXXXX" starts with "prism-foo-". The
// trailing `[a-f0-9]{8}$` anchor closes that gap by relying on the
// random-suffix shape the proxy enforces.
//
// User-supplied Names that pass Half 1's prefix check but do not
// match this strict shape (e.g. "prism-foo-myname") are NOT swept
// — the user took ownership of the name and is responsible for
// teardown. This is a documented limitation of the auto-sweep
// design and is acceptable: the sweep is a safety net for orphan
// auto-named containers, not a comprehensive teardown for all
// possible naming patterns.
func containerNamePattern(sessionName string) *regexp.Regexp {
	prefix := "prism-" + sessionName + "-"
	return regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + "[a-f0-9]{8}$")
}

// sweepWithRunner is the inner sweep, called with an explicit runner
// (production uses execPodmanRunner; tests inject a stub). Returns
// the number of containers actually force-removed.
//
// Flow:
//  1. Build the strict regex pattern for this session.
//  2. Call `podman ps -a --filter name=<anchored-regex> --format {{.Names}}`.
//     The anchored regex is the FIRST line of defence: podman libpod's
//     filter is regex-matched, so an anchored regex on the podman
//     side already excludes most non-matching containers.
//  3. Re-filter the result in Go with the same regex. This is
//     defence-in-depth against any podman version that interprets
//     the filter as a substring match, which the docker compat
//     surface can do. Without this Go-side re-check, a substring
//     match on the podman side could leak a sibling session's
//     containers into the rm batch.
//  4. If any names survive both filters, invoke `podman rm -f` on
//     them in a single batch. Empty list short-circuits — no rm
//     invocation, count returned as 0.
//
// Failures at any step log a warning and return what we have so
// far. The cleanup flow continues regardless.
func sweepWithRunner(runner podmanRunner, sessionName string) int {
	ctx, cancel := context.WithTimeout(context.Background(), podmanSweepBudget)
	defer cancel()

	pattern := containerNamePattern(sessionName)
	// The libpod `name` filter is a regex on the container name.
	// We anchor at the start AND at the end (via the regex) so the
	// suffix shape excludes sibling-prefix matches. QuoteMeta is
	// belt-and-braces — session names use only `@`, alphanumerics,
	// hyphens, and dots, none of which are regex metacharacters in
	// practice, but a future relaxation of the session-name
	// character set should not silently turn into a regex injection.
	podmanFilter := "name=" + pattern.String()

	out, err := runner.Run(ctx, "ps", "-a",
		"--filter", podmanFilter,
		"--format", "{{.Names}}")
	if err != nil {
		// Common, non-fatal failure modes:
		//   - podman not installed (exec.ErrNotFound)
		//   - Darwin machine off (`Cannot connect to Podman`)
		//   - Linux user systemd not running (ECONNREFUSED)
		// In all cases the agent's session never had a container
		// running, so there is nothing to sweep. Log once and
		// continue.
		proglog.Warnf("[prism] warning: cleanup: orphan-container sweep: podman ps for %q failed (%v) — continuing cleanup\n",
			sessionName, err)
		return 0
	}

	var names []string
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		name := strings.TrimSpace(string(line))
		if name == "" {
			continue
		}
		// Defence-in-depth: enforce strict-shape match in Go even
		// if podman returned extras. This is the load-bearing check
		// for the strict-shape security requirement.
		if !pattern.MatchString(name) {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return 0
	}

	// Single batched `podman rm -f` for the matched names. xargs is
	// fine on the shell side but we avoid the shell entirely by
	// passing the names directly as arguments. `rm -f` on a name
	// that is already gone (race with another cleanup) is
	// non-fatal — podman returns a non-zero exit but the residual
	// state is "container is gone", which is the desired post-
	// condition. We log a warning but treat the count as the
	// number of names we asked to remove.
	args := append([]string{"rm", "-f"}, names...)
	if _, err := runner.Run(ctx, args...); err != nil {
		proglog.Warnf("[prism] warning: cleanup: orphan-container sweep: podman rm for %q failed (%v) — some containers may remain\n",
			sessionName, err)
		// Return 0 because we don't know how many were actually
		// removed. Reporting a guessed count would mislead the
		// operator into thinking the sweep succeeded.
		return 0
	}
	return len(names)
}

// formatPodmanArgs renders a slice of args into a single space-joined
// string for inclusion in warning messages.
//
//nolint:unused // debugging helper for warning-message construction
func formatPodmanArgs(args []string) string {
	return fmt.Sprintf("podman %s", strings.Join(args, " "))
}
