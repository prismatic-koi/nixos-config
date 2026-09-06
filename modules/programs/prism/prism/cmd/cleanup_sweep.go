package cmd

// Orphan-resource sweep for sessions with containers_enabled=1.
//
// Three resource classes are swept, in this order:
//
//  1. Containers matching the strict per-session auto-name shape.
//  2. Volumes matching the per-session name prefix.
//  3. Images the session pulled through its own proxy, replayed from
//     the per-session image ledger.
//
// All three are gated on the SAME agent_status.containers_enabled read,
// so a session that never enabled containers issues no podman command
// at all.
//
// The container sweep complements the per-mode container teardown that
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
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/podmanproxy"
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

// sweepCounts carries the per-class outcome of one resource sweep.
// Each field is the number of resources the sweep confirmed removed;
// a failed podman invocation contributes 0 rather than a guess.
type sweepCounts struct {
	containers int
	volumes    int
	images     int
}

// sweepSessionResourcesForSession runs the container, volume, and
// image sweeps for sessionName, gated on the session's
// agent_status.containers_enabled column. Returns:
//
//	counts : per-class number of resources removed.
//	ran    : true if the sweep ran (containers_enabled=1 and the DB
//	         read succeeded); false if it was a no-op (containers_enabled=0,
//	         row missing, or DB error).
//
// When ran is false the JSON envelope MUST omit the containers_swept,
// volumes_swept, and images_swept fields entirely — this is the spec,
// and it is what makes "a session that never enabled containers issues
// no podman command" observable from the outside.
//
// Errors from podman are NEVER fatal: they're surfaced as warnings
// via proglog.Warnf and the function returns whatever counts it
// managed to confirm (0 per failed class).
func sweepSessionResourcesForSession(sessionName string) (counts sweepCounts, ran bool) {
	d, err := openDB()
	if err != nil {
		// DB unavailable. The cleanup caller logs the DB-open error
		// separately; we silently skip the sweep here. Returning
		// ran=false is structurally correct: a session whose
		// containers_enabled value we could not read is treated as
		// "do not issue podman commands". This is the safe
		// direction — we'd rather skip a sweep than spawn a
		// spurious warning for a session that didn't use the proxy.
		return sweepCounts{}, false
	}
	defer d.Close()
	status, err := d.CurrentStatus(sessionName)
	if err != nil || status == nil || !status.ContainersEnabled {
		return sweepCounts{}, false
	}

	runner := currentPodmanRunner()
	counts.containers = sweepWithRunner(runner, sessionName)
	counts.volumes = sweepVolumesWithRunner(runner, sessionName, siblingVolumePrefixes(d, sessionName))
	counts.images = sweepImagesWithRunner(runner, sessionName, imageLedgerPathForStatus(status))
	return counts, true
}

// imageLedgerPathForStatus resolves the per-session image ledger path
// from an agent_status row, or "" when the row carries no instance_id
// (a session that never reached the point of having a work dir). An
// empty path makes the image sweep a no-op with no podman invocation.
func imageLedgerPathForStatus(status *db.Status) string {
	if status == nil || status.InstanceID == nil || *status.InstanceID == "" {
		return ""
	}
	sessionDir, err := container.SessionWorkDirPath(*status.InstanceID)
	if err != nil {
		return ""
	}
	return filepath.Join(sessionDir, podmanproxy.ImageLedgerFileName)
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

// volumeNamePrefix returns the per-session volume name prefix the
// proxy enforces on POST /volumes/create:
// `prism-<sessionName>-`.
//
// Unlike containerNamePattern, the volume sweep matches on the PREFIX,
// not on the strict `<prefix><8 hex>` auto-name shape. The two differ
// because the volume policy admits a user-supplied Name as long as it
// carries the prefix, and a user-named volume holds data that outlives
// the session unless the sweep removes it. Prefix matching is what
// makes "every volume this session could have created" reachable.
//
// The looser match brings back the sibling-prefix ambiguity that
// containerNamePattern's `[a-f0-9]{8}` anchor closes: session "foo"
// and session "foo-bar" produce prefixes where one is a prefix of the
// other. siblingVolumePrefixes supplies the guard for that case.
func volumeNamePrefix(sessionName string) string {
	return "prism-" + sessionName + "-"
}

// siblingVolumePrefixes returns the volume-name prefixes of every OTHER
// live session whose name extends sessionName — that is, every session
// whose own prefix is a strict extension of this one's.
//
// Why this is needed: session "foo" has prefix "prism-foo-" and
// session "foo-bar" has prefix "prism-foo-bar-". A volume named
// "prism-foo-bar-data" starts with BOTH. Cleaning up "foo" with a
// plain prefix match would destroy a live sibling session's data
// volume.
//
// The name alone cannot resolve the ambiguity: session "foo" is
// allowed to create a volume explicitly named "prism-foo-bar-data"
// too. So the guard errs toward NOT deleting. A volume claimed by a
// live sibling is left alone; the cost is a leaked volume when the
// name really did belong to this session, and the benefit is that no
// cleanup destroys another running session's data.
//
// A DB read failure returns nil, which degrades to the plain prefix
// match. That is the documented sweep behaviour, not a silent
// weakening: the guard is defence in depth over the prefix rule, not
// the rule itself.
func siblingVolumePrefixes(d *db.DB, sessionName string) []string {
	if d == nil {
		return nil
	}
	statuses, err := d.AllActiveStatus()
	if err != nil {
		proglog.Warnf("[prism] warning: cleanup: volume sweep: list active sessions failed (%v) — sweeping on the name prefix alone\n", err)
		return nil
	}
	var prefixes []string
	for _, st := range statuses {
		if st.SessionName == sessionName {
			continue
		}
		if !strings.HasPrefix(st.SessionName, sessionName+"-") {
			continue
		}
		prefixes = append(prefixes, volumeNamePrefix(st.SessionName))
	}
	return prefixes
}

// sweepVolumesWithRunner removes every volume whose name starts with
// the session's `prism-<sessionName>-` prefix, minus any name claimed
// by one of siblingPrefixes. Returns the number of volumes removed.
//
// Flow mirrors sweepWithRunner:
//
//  1. `podman volume ls --filter name=^prism-<session>- --format
//     {{.Name}}`. The anchored regex is the first line of defence —
//     podman's libpod volume filter is regex-matched.
//  2. Re-check the prefix in Go. This is the load-bearing check:
//     the docker-compat surface can treat the same filter as a
//     substring match, which would leak names like
//     "user-prism-foo-data" into the removal batch.
//  3. Drop any name claimed by a live sibling session.
//  4. `podman volume rm` the survivors in one batch. An empty list
//     short-circuits with no rm invocation.
//
// Failures at any step log a warning and return what we have so far.
// The cleanup flow continues regardless.
func sweepVolumesWithRunner(runner podmanRunner, sessionName string, siblingPrefixes []string) int {
	// A budget of its own, not a share of the container sweep's: a
	// container sweep that burned its full 30 s must not leave the
	// volume sweep with no time to run.
	ctx, cancel := context.WithTimeout(context.Background(), podmanSweepBudget)
	defer cancel()

	prefix := volumeNamePrefix(sessionName)
	// QuoteMeta for the same reason as the container filter: session
	// names use only `@`, alphanumerics, hyphens, and dots today, but a
	// future relaxation of the character set must not become a regex
	// injection into podman's filter.
	podmanFilter := "name=^" + regexp.QuoteMeta(prefix)

	out, err := runner.Run(ctx, "volume", "ls",
		"--filter", podmanFilter,
		"--format", "{{.Name}}")
	if err != nil {
		proglog.Warnf("[prism] warning: cleanup: volume sweep: podman volume ls for %q failed (%v) — continuing cleanup\n",
			sessionName, err)
		return 0
	}

	var names []string
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		name := strings.TrimSpace(string(line))
		if name == "" {
			continue
		}
		// Strict prefix, and something after it: the bare prefix is
		// not a name this session's policy can produce.
		if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
			continue
		}
		if claimedBySibling(name, siblingPrefixes) {
			proglog.Warnf("[prism] warning: cleanup: volume sweep: leaving %q — the name is also claimed by a live sibling session\n", name)
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return 0
	}

	args := append([]string{"volume", "rm"}, names...)
	if _, err := runner.Run(ctx, args...); err != nil {
		proglog.Warnf("[prism] warning: cleanup: volume sweep: podman volume rm for %q failed (%v) — some volumes may remain\n",
			sessionName, err)
		// Same reasoning as the container sweep: we do not know how
		// many of the batch were removed, so we report none rather
		// than mislead the operator.
		return 0
	}
	return len(names)
}

// claimedBySibling reports whether name falls inside one of the
// sibling-session prefixes. See siblingVolumePrefixes for why a match
// means "leave it alone".
func claimedBySibling(name string, siblingPrefixes []string) bool {
	for _, sp := range siblingPrefixes {
		if strings.HasPrefix(name, sp) {
			return true
		}
	}
	return false
}

// sweepImagesWithRunner removes the images the session pulled through
// its own proxy, replayed from the per-session image ledger at
// ledgerPath. Returns the number of images podman confirmed removed.
//
// Two properties this function must hold, both of them ACs:
//
//   - ONE podman invocation per image, not one batch. An image another
//     container still uses makes `podman rmi` exit non-zero. Batched,
//     that one failure would take the whole batch's outcome with it;
//     per-image, the sweep removes the rest and reports the true count.
//   - No podman invocation at all when the ledger is absent or empty.
//     A session that enabled containers but never pulled an image
//     leaves no ledger lines, and a session that never enabled
//     containers never reaches this function.
//
// The sweep is deliberately NOT a `podman image prune`. The image
// store is shared with the user and with every other session, so only
// the references this session asked for may be removed.
func sweepImagesWithRunner(runner podmanRunner, sessionName, ledgerPath string) int {
	if ledgerPath == "" {
		return 0
	}
	refs, err := podmanproxy.ReadImageLedger(ledgerPath)
	if err != nil {
		proglog.Warnf("[prism] warning: cleanup: image sweep: read ledger for %q failed (%v) — continuing cleanup\n",
			sessionName, err)
		return 0
	}
	if len(refs) == 0 {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), podmanSweepBudget)
	defer cancel()

	removed := 0
	for _, ref := range refs {
		if _, err := runner.Run(ctx, "rmi", ref); err != nil {
			// Expected and non-fatal: the image is still in use by
			// another container, was never pulled successfully, or
			// has already been removed. Best effort by design — keep
			// going so one stuck image does not strand the rest.
			proglog.Warnf("[prism] warning: cleanup: image sweep: podman rmi %q for %q failed (%v) — leaving it in place\n",
				ref, sessionName, err)
			continue
		}
		removed++
	}
	return removed
}

// formatPodmanArgs renders a slice of args into a single space-joined
// string for inclusion in warning messages.
//
//nolint:unused // debugging helper for warning-message construction
func formatPodmanArgs(args []string) string {
	return fmt.Sprintf("podman %s", strings.Join(args, " "))
}
