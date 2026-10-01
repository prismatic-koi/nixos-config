package cmd

// Orphan-resource sweep for sessions with containers_enabled=1.
//
// Two resource classes are swept, in this order:
//
//  1. Containers the session owns.
//  2. Volumes the session owns.
//
// Both are gated on the SAME agent_status.containers_enabled read, so a
// session that never enabled containers issues no podman command at
// all.
//
// # Ownership is identity, not a shared prefix
//
// A resource created through the proxy carries the owning incarnation's
// INSTANCE ID in its name, left-anchored:
// `prism-<instance token>-<sanitised session>-<suffix>`.
// `resourceOwner` decides what to remove by parsing that token back out
// and comparing it, and never by testing one name prefix against
// another. Prefix equality is a heuristic for identity and two distinct
// live sessions collide under it — by folding and by nesting — so it
// could destroy a live sibling's data volume. See
// internal/container/resource_identity.go and issue #2951.
//
// A session that restarts gets a NEW instance ID, so the owner carries
// the token of EVERY incarnation of the session name, read from the
// `sessions` table. Cleaning a session therefore reaches the volumes its
// earlier incarnations created, which a single-token sweep would leak.
//
// # The legacy half
//
// A resource created before the identity prefix landed carries no token.
// It is still swept, by the pre-identity rule it was created under: the
// strict `prism-<sanitised session>-<8 hex chars>` shape for a container,
// the plain `prism-<sanitised session>-` prefix for a volume, and the
// sibling guard over both. That rule is ambiguous — that is why it was
// replaced — so the guard errs toward leaving a resource in place. See
// `legacySiblingPrefixes`.
//
// Images the session pulled are NOT swept. See docs/podman-proxy.md
// section 8.3 for the two conditions that work needs first.
//
// The container sweep complements the per-mode container teardown that
// `removeContainerIfExists` already performs: that path removes the
// SESSION'S OWN bwrap/sandbox-exec container (the agent's runtime),
// while this path sweeps any DERIVATIVE containers the agent created
// via the proxy during the session.
//
// The sweep is best-effort: each resource class gets its own
// 30-second context (60 seconds worst case across both classes),
// and any failure (podman not on PATH, machine off, socket missing,
// rm returns non-zero) is logged at warning level and does NOT abort
// cleanup. The worktree and DB teardown ALWAYS run regardless of
// sweep outcome — the containers_swept count is just an
// observability field in the `--json` envelope.
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

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
)

// podmanSweepBudget is the upper bound on the time a single resource
// class's sweep may consume per session. Each class (containers,
// volumes) builds its own context from this budget, so the podman
// invocations within a class (ps + rm) share one context, but the
// two classes do not share a context with each other — worst case
// across both classes is 60 seconds.
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
}

// sweepScope selects which resource classes a cleanup path sweeps. The
// two values are NOT interchangeable, and the call site must name the
// one that matches its teardown contract.
//
// The distinction is a container is stateless and a volume is not. A
// soft close preserves the worktree, the branch, and the transcript so
// the session can be reopened and resumed, so it must not destroy the
// session's data either. A hard cleanup destroys the worktree and the
// branch, so the volumes go with them.
//
// A soft-closed session still reaches hard cleanup eventually, and the
// volume sweep runs then. So the cost of the narrow scope is a
// temporarily leaked volume, not a permanently leaked one. That is the
// same trade siblingVolumePrefixes already makes: a leaked volume is
// cheaper than data the operator expected to survive.
type sweepScope int

const (
	// sweepContainersOnly is the SOFT-close scope: `prism close`,
	// coordinator and non-worktree sessions, and `--keep-worktree`.
	// Sweeps containers, never volumes.
	sweepContainersOnly sweepScope = iota

	// sweepContainersAndVolumes is the HARD-cleanup scope: the paths
	// that also remove the worktree and delete the branch.
	sweepContainersAndVolumes
)

// sweepsVolumes reports whether this scope includes the volume sweep.
func (s sweepScope) sweepsVolumes() bool { return s == sweepContainersAndVolumes }

// sweepSessionResourcesForSession runs the container and volume sweeps
// for sessionName, gated on the session's
// agent_status.containers_enabled column. Returns:
//
//	counts : per-class number of resources removed.
//	ran    : true if the sweep ran (containers_enabled=1 and the DB
//	         read succeeded); false if it was a no-op (containers_enabled=0,
//	         row missing, or DB error).
//
// When ran is false the JSON envelope MUST omit the containers_swept
// and volumes_swept fields entirely — this is the spec, and it is what
// makes "a session that never enabled containers issues no podman
// command" observable from the outside.
//
// scope decides whether the volume sweep runs at all. Under
// sweepContainersOnly no volume podman command is issued and
// counts.volumes stays 0, which the caller must render as an ABSENT
// volumes_swept key rather than a zero one.
//
// Errors from podman are NEVER fatal: they're surfaced as warnings
// via proglog.Warnf and the function returns whatever counts it
// managed to confirm (0 per failed class).
func sweepSessionResourcesForSession(sessionName string, scope sweepScope) (counts sweepCounts, ran bool) {
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

	owner := resourceOwnerForSession(d, sessionName, status)
	runner := currentPodmanRunner()
	counts.containers = sweepWithRunner(runner, owner)
	if scope.sweepsVolumes() {
		counts.volumes = sweepVolumesWithRunner(runner, owner)
	}
	return counts, true
}

// podmanResourceNameFilter is the `--filter name=` value both sweeps
// send to podman. It narrows the listing to names that could be prism
// resources and nothing more.
//
// It is an OPTIMISATION, not a control. The sweep used to anchor this
// filter on the per-session prefix and treat that as its first line of
// defence, because the Go-side check behind it was a prefix heuristic
// too, and a podman version that read the filter as a substring match
// could widen what that heuristic saw. The Go-side decision is now an
// exact ownership parse (`resourceOwner`), which is total: it answers
// for every name podman can return, including names of other sessions
// and names that are not prism resources at all. So the filter's only
// job is to keep the listing small.
//
// One listing per resource class also serves both halves of the
// decision. The identity half and the legacy half need different names
// out of the same population, and issuing two listings per class would
// double the podman invocations on every teardown path.
var podmanResourceNameFilter = "name=^" + regexp.QuoteMeta(container.ResourceNamePrefixRoot)

// ownership is the sweep's verdict on one resource name.
type ownership int

const (
	// ownershipMine — the name carries this session's identity, or it
	// is a legacy name only this session can claim. Remove it.
	ownershipMine ownership = iota

	// ownershipOther — the name carries an instance token that is not
	// this session's. It belongs to a different incarnation, which may
	// be a live session. Leave it, silently: this is the common case on
	// a host running several sessions, and warning per name would drown
	// the teardown output.
	ownershipOther

	// ownershipOtherPrunedIncarnation — the name carries an instance
	// token that is not this session's, AND its decorative half equals
	// this session's sanitised name. That combination is the shape a
	// PRUNED incarnation of THIS SAME session produces: the resource was
	// created by an earlier incarnation whose `sessions` row aged out of
	// the 90-day retention window (db.Prune), so its token no longer
	// appears in this session's token set even though the name reads as
	// this session's. See resourceOwnerForSession and issue #2972.
	//
	// Leave it, exactly as ownershipOther would — the decision to skip
	// does not change, only the visibility of the skip does. Warn,
	// because unlike the ordinary ownershipOther case (another live
	// session, expected and silent) this shape means the resource is
	// now UNREACHABLE to every future sweep: no session can ever own
	// that token again.
	ownershipOtherPrunedIncarnation

	// ownershipUnclaimed — the name carries no identity and does not
	// match this session's legacy rule. Leave it.
	ownershipUnclaimed

	// ownershipLegacySiblingClaim — the name carries no identity, it
	// matches this session's legacy rule, and it matches a live
	// sibling's legacy rule too. The name space cannot tell the two
	// apart, so leave it and say so: the operator is the only one who
	// can resolve it.
	ownershipLegacySiblingClaim
)

// resourceOwner carries the identity a sweep decides ownership by, plus
// the fallback rule for resources that carry no identity.
//
// It is built once per cleanup and used for both resource classes, so
// the container sweep and the volume sweep cannot disagree about who
// owns what.
type resourceOwner struct {
	// sessionName is used in warning text only. It is never an input to
	// an ownership decision on an identity-scoped name.
	sessionName string

	// instanceTokens holds the resource-name token of every incarnation
	// of sessionName, most-recent-first. A session that restarted N
	// times has N tokens and owns the resources of all of them.
	instanceTokens []string

	// legacyPrefix is the pre-identity name prefix,
	// `prism-<sanitised session>-`. It applies ONLY to names that carry
	// no instance token.
	legacyPrefix string

	// decorativeName is the sanitised session name alone, with neither
	// the `prism-` root nor the trailing `-` that legacyPrefix carries.
	// It is the DECORATIVE half of an identity-scoped name
	// (`prism-<token>-<decorativeName>-<suffix>`) — see
	// resource_identity.go for why that half carries no ownership
	// meaning on its own. It exists here only to detect the pruned-
	// incarnation shape in identityOwnership: a name whose token is not
	// ours but whose decorative half IS ours.
	decorativeName string

	// legacyContainerShape is the strict auto-name shape the proxy used
	// to inject, `^<legacyPrefix>[a-f0-9]{8}$`. Compiled once here
	// rather than per name.
	legacyContainerShape *regexp.Regexp

	// legacySiblingPrefixes are the legacy prefixes of OTHER live
	// sessions that legacyPrefix cannot be told apart from. See
	// legacySiblingPrefixes.
	legacySiblingPrefixes []string
}

// newResourceOwner builds a resourceOwner from a session name, the
// instance IDs of its incarnations, and the legacy prefixes of the live
// sessions its legacy prefix collides with.
//
// An instance ID that yields no token is dropped rather than rejected:
// such a session's resources carry the legacy prefix, which the legacy
// half of the decision already covers.
func newResourceOwner(sessionName string, instanceIDs, legacySiblings []string) resourceOwner {
	legacyPrefix := container.ResourceNamePrefixForSession(sessionName)
	decorativeName := strings.TrimSuffix(strings.TrimPrefix(legacyPrefix, container.ResourceNamePrefixRoot), "-")
	o := resourceOwner{
		sessionName:    sessionName,
		legacyPrefix:   legacyPrefix,
		decorativeName: decorativeName,
		// QuoteMeta because a future relaxation of the session-name
		// character set must not become a regex injection. Today's set
		// carries no metacharacter that survives sanitisation.
		legacyContainerShape:  regexp.MustCompile("^" + regexp.QuoteMeta(legacyPrefix) + "[a-f0-9]{8}$"),
		legacySiblingPrefixes: legacySiblings,
	}
	seen := make(map[string]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		token := container.InstanceTokenForID(id)
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		o.instanceTokens = append(o.instanceTokens, token)
	}
	return o
}

// resourceOwnerForSession assembles the owner for the session being
// cleaned.
//
// The token set is the union of two reads, because neither alone is
// complete. `agent_status.instance_id` is the CURRENT incarnation and is
// the one a live session is creating resources under right now. The
// `sessions` rows are the incarnations of the session name that the
// database STILL HOLDS, which is what reaches a volume an earlier
// incarnation created before a reopen minted a new instance ID.
//
// # The union is not every incarnation that ever existed
//
// `db.Prune` runs `DELETE FROM sessions WHERE ended_at IS NOT NULL AND
// ended_at < ?` (internal/db/maintenance.go), and both callers pass a
// ninety-day window (cmd/event.go, cmd/restore.go). Every close stamps
// `sessions.ended_at`, and a restore clears it again because a restore
// continues the incarnation. So the row of an incarnation that ended more
// than ninety days ago is gone.
//
// A resource created by a pruned incarnation therefore carries a token
// this set does not hold. identityOwnership resolves it to
// ownershipOther, and collectSweepable skips it. The legacy rule does
// not catch it either, because identityOwnership already answered for
// the name. The resource leaks, and the skip is silent.
//
// A long-lived session name that is closed and opened again often, and
// reaches hard cleanup rarely, is the reachable case. Do NOT close this by falling back to the
// name when a token is unknown — that is the collision issue #2951
// closed. Issue #2972 carries the follow-up: a diagnostic warning for
// the silent skip, and the prune-versus-ownership retention question
// underneath it. docs/podman-proxy.md §8.3 records the residual.
//
// A failed `sessions` read degrades to the current incarnation plus the
// legacy rule, with a warning. That leaks an older incarnation's volumes
// rather than risking another session's, which is the direction every
// other degradation in this file takes.
func resourceOwnerForSession(d *db.DB, sessionName string, status *db.Status) resourceOwner {
	var instanceIDs []string
	if status != nil && status.InstanceID != nil {
		instanceIDs = append(instanceIDs, *status.InstanceID)
	}
	if d != nil {
		sessions, err := d.SessionsByName(sessionName)
		if err != nil {
			proglog.Warnf("[prism] warning: cleanup: sweep: list incarnations of %q failed (%v) — sweeping the current incarnation only\n", sessionName, err)
		} else {
			for _, s := range sessions {
				instanceIDs = append(instanceIDs, s.InstanceID)
			}
		}
	}
	return newResourceOwner(sessionName, instanceIDs, legacySiblingPrefixes(d, sessionName))
}

// ownsToken reports whether token belongs to one of this session's
// incarnations.
func (o resourceOwner) ownsToken(token string) bool {
	for _, t := range o.instanceTokens {
		if t == token {
			return true
		}
	}
	return false
}

// identityOwnership answers for a name that carries an instance token,
// and reports ok=false for one that does not so the caller can apply its
// class's legacy rule.
//
// This is the load-bearing check. It is an exact comparison of one
// instance token against a set of instance tokens, so no name a session
// can choose puts it on the wrong side: the token sits at a fixed
// left-anchored position, it is 32 hex characters, and it carries no `-`
// for the parse to stop at early.
func (o resourceOwner) identityOwnership(name string) (ownership, bool) {
	token, ok := container.ResourceOwnerToken(name)
	if !ok {
		return ownershipUnclaimed, false
	}
	if o.ownsToken(token) {
		return ownershipMine, true
	}
	if o.decoratesAsSelf(name, token) {
		return ownershipOtherPrunedIncarnation, true
	}
	return ownershipOther, true
}

// decoratesAsSelf reports whether name's decorative half — the segment
// between the instance token and the free-form suffix — equals this
// session's own sanitised name. token must already be the value
// identityOwnership parsed out of name via container.ResourceOwnerToken.
//
// The decorative half is free-form after the sanitised name (the volume
// policy admits any user-chosen suffix), so this cannot be a fixed-width
// segment parse the way the token is. It is instead an exact-or-prefix
// test against the ONE string this session would have written there:
// either the decorative half IS the sanitised name with nothing after it
// (the bare prefix is a valid volume name), or it continues with the
// `-` that separates it from a suffix.
//
// This shares the NESTING ambiguity that legacySiblingPrefixes guards
// against for the legacy rule: a free-form suffix that itself starts
// with a session-name-shaped fragment ("bar-data" appended after decor
// "foo") reads identically to a longer decorative half ("foo-bar" with
// suffix "data"). Unlike the legacy rule, this function cannot just err
// toward "leave it" on ambiguity — the resource is already left, since
// identityOwnership has resolved to ownershipOther by the time this runs.
// What it must not do is MISATTRIBUTE the skip to this session when a
// live sibling's identity is the more likely explanation. So it reuses
// legacySiblingPrefixes — the same DB-backed set that already answers
// "which other live session's name is this one indistinguishable from" —
// and stands down whenever the candidate falls inside a sibling's prefix.
func (o resourceOwner) decoratesAsSelf(name, token string) bool {
	rest, ok := strings.CutPrefix(name, container.ResourceNamePrefixRoot+token+"-")
	if !ok {
		return false
	}
	if !(rest == o.decorativeName || strings.HasPrefix(rest, o.decorativeName+"-")) {
		return false
	}
	candidate := container.ResourceNamePrefixRoot + rest
	for _, sibPrefix := range o.legacySiblingPrefixes {
		if strings.HasPrefix(candidate, sibPrefix) {
			return false
		}
	}
	return true
}

// legacyOwnership answers for a name that carries no instance token,
// given whether the name matched the caller's class-specific legacy
// rule.
func (o resourceOwner) legacyOwnership(name string, matchesLegacyRule bool) ownership {
	if !matchesLegacyRule {
		return ownershipUnclaimed
	}
	if claimedBySibling(name, o.legacySiblingPrefixes) {
		return ownershipLegacySiblingClaim
	}
	return ownershipMine
}

// containerOwnership decides whether the container called name is this
// session's to remove.
//
// The legacy rule is the strict auto-name shape rather than the plain
// prefix, unchanged from before identity existed. A user-supplied name
// that passed the create-time prefix check but does not match the shape
// is NOT swept on this path: the user took ownership of the name, and
// there is no identity in it to prove otherwise.
func (o resourceOwner) containerOwnership(name string) ownership {
	if own, ok := o.identityOwnership(name); ok {
		return own
	}
	return o.legacyOwnership(name, o.legacyContainerShape.MatchString(name))
}

// volumeOwnership decides whether the volume called name is this
// session's to remove.
//
// The legacy rule is a plain prefix match with no further shape
// condition, unchanged from before identity existed. The volume policy
// admitted any user-chosen suffix inside the prefix, and a user-named
// volume holds data, so every name that policy admitted must be
// reachable here — including the name that is exactly the prefix.
func (o resourceOwner) volumeOwnership(name string) ownership {
	if own, ok := o.identityOwnership(name); ok {
		return own
	}
	return o.legacyOwnership(name, strings.HasPrefix(name, o.legacyPrefix))
}

// collectSweepable partitions a podman listing into the names to remove,
// applying decide to each. Names left behind for a live sibling's legacy
// claim are warned about individually, because that outcome is a leak
// the operator may need to resolve by hand. The warning names the
// session doing the cleanup, so the operator can tell which of two
// colliding sessions produced it.
func collectSweepable(out []byte, class, sessionName string, decide func(string) ownership) []string {
	var names []string
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		name := strings.TrimSpace(string(line))
		if name == "" {
			continue
		}
		switch decide(name) {
		case ownershipMine:
			names = append(names, name)
		case ownershipLegacySiblingClaim:
			proglog.Warnf("[prism] warning: cleanup: %s sweep for %q: leaving %q — it predates instance-ID naming and a live sibling session claims the same name prefix\n", class, sessionName, name)
		case ownershipOtherPrunedIncarnation:
			// Same disposition as ownershipOther — this is still a skip,
			// never a removal — but this shape means the owning
			// incarnation's `sessions` row is gone (pruned), so the
			// resource can never be reached by identity again. Warn so
			// the operator can find it; see issue #2972.
			proglog.Warnf("[prism] warning: cleanup: %s sweep for %q: skipping %q — it looks like an earlier incarnation of this session, but that incarnation is not in the database (likely pruned) and its resources can no longer be reached by identity\n", class, sessionName, name)
		case ownershipOther, ownershipUnclaimed:
			// Not ours. Nothing to say.
		}
	}
	return names
}

// sweepWithRunner is the inner container sweep, called with an explicit
// runner (production uses execPodmanRunner; tests inject a stub).
// Returns the number of containers actually force-removed.
//
// Flow:
//  1. Call `podman ps -a --filter name=^prism- --format {{.Names}}`.
//     The filter narrows the listing; it does not decide anything.
//  2. Decide ownership of each returned name in Go, with
//     resourceOwner.containerOwnership. This is the load-bearing step,
//     and it is total — it answers for every name podman can return,
//     including a substring trap like "user-prism-foo-aaaaaaaa" and a
//     name belonging to another live session.
//  3. If any names are ours, invoke `podman rm -f` on them in a single
//     batch. Empty list short-circuits — no rm invocation, count
//     returned as 0.
//
// Failures at any step log a warning and return what we have so
// far. The cleanup flow continues regardless.
func sweepWithRunner(runner podmanRunner, owner resourceOwner) int {
	ctx, cancel := context.WithTimeout(context.Background(), podmanSweepBudget)
	defer cancel()

	out, err := runner.Run(ctx, "ps", "-a",
		"--filter", podmanResourceNameFilter,
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
			owner.sessionName, err)
		return 0
	}

	names := collectSweepable(out, "orphan-container", owner.sessionName, owner.containerOwnership)
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
			owner.sessionName, err)
		// Return 0 because we don't know how many were actually
		// removed. Reporting a guessed count would mislead the
		// operator into thinking the sweep succeeded.
		return 0
	}
	return len(names)
}

// legacySiblingPrefixes returns the LEGACY name prefixes of every OTHER
// live session that this session's legacy prefix cannot be told apart
// from. It guards resources created before instance-ID naming, and it
// guards nothing else — an identity-scoped name never reaches it.
//
// Two collision shapes put a prefix in this list, and both are decided
// in SANITISED space, on the prefixes rather than on the raw session
// names. Sanitisation folds `@`, `/`, `.`, and `~` all to `-`, and it is
// the prefix, not the session name, that a resource name carries.
//
//  1. NESTING: the other session's prefix strictly EXTENDS this one's.
//     Session "foo" has "prism-foo-" and session "foo-bar" has
//     "prism-foo-bar-", so "prism-foo-bar-data" starts with both.
//  2. FOLDING: the other session's prefix EQUALS this one's, reached
//     from a different raw name — `repo@feat/x` and `repo@feat-x` both
//     sanitise to "prism-repo-feat-x-". Every legacy name under the
//     prefix is then ambiguous, so the returned list contains this
//     session's own prefix and the legacy sweep removes nothing.
//
// The name alone cannot resolve either shape: session "foo" is allowed
// to create a volume explicitly named "prism-foo-bar-data" too. So the
// guard errs toward NOT deleting. The cost is a leaked resource when the
// name really did belong to this session; the benefit is that no cleanup
// destroys another running session's data. Resources created under
// instance-ID naming need no such trade, because their ownership is
// exact.
//
// The session being cleaned is excluded by NAME, not by prefix
// equality. Excluding by prefix would also drop a folding collision,
// which is the second shape above.
//
// A DB read failure returns nil, which degrades to the plain legacy
// rule. That is the documented sweep behaviour, not a silent weakening:
// the guard is defence in depth over a rule that only applies to legacy
// names.
func legacySiblingPrefixes(d *db.DB, sessionName string) []string {
	if d == nil {
		return nil
	}
	statuses, err := d.AllActiveStatus()
	if err != nil {
		proglog.Warnf("[prism] warning: cleanup: sweep: list active sessions failed (%v) — sweeping pre-identity names on the name prefix alone\n", err)
		return nil
	}
	ownPrefix := container.ResourceNamePrefixForSession(sessionName)
	var prefixes []string
	seen := make(map[string]bool, len(statuses))
	for _, st := range statuses {
		if st.SessionName == sessionName {
			continue
		}
		otherPrefix := container.ResourceNamePrefixForSession(st.SessionName)
		if !strings.HasPrefix(otherPrefix, ownPrefix) {
			continue
		}
		if seen[otherPrefix] {
			continue
		}
		seen[otherPrefix] = true
		prefixes = append(prefixes, otherPrefix)
	}
	return prefixes
}

// sweepVolumesWithRunner removes every volume the session owns. Returns
// the number of volumes removed.
//
// Flow mirrors sweepWithRunner:
//
//  1. `podman volume ls --filter name=^prism- --format {{.Name}}`. The
//     filter narrows the listing; it does not decide anything.
//  2. Decide ownership of each returned name in Go, with
//     resourceOwner.volumeOwnership. This is the load-bearing step. It
//     is total, so a substring trap like "user-prism-foo-data" and a
//     live sibling's volume both fall out here rather than into the
//     removal batch.
//  3. `podman volume rm` the survivors in one batch. An empty list
//     short-circuits with no rm invocation.
//
// Failures at any step log a warning and return what we have so far.
// The cleanup flow continues regardless.
func sweepVolumesWithRunner(runner podmanRunner, owner resourceOwner) int {
	// A budget of its own, not a share of the container sweep's: a
	// container sweep that burned its full 30 s must not leave the
	// volume sweep with no time to run. This makes the worst-case
	// total across both classes 60 s, not 30 s.
	ctx, cancel := context.WithTimeout(context.Background(), podmanSweepBudget)
	defer cancel()

	out, err := runner.Run(ctx, "volume", "ls",
		"--filter", podmanResourceNameFilter,
		"--format", "{{.Name}}")
	if err != nil {
		proglog.Warnf("[prism] warning: cleanup: volume sweep: podman volume ls for %q failed (%v) — continuing cleanup\n",
			owner.sessionName, err)
		return 0
	}

	names := collectSweepable(out, "volume", owner.sessionName, owner.volumeOwnership)
	if len(names) == 0 {
		return 0
	}

	args := append([]string{"volume", "rm"}, names...)
	if _, err := runner.Run(ctx, args...); err != nil {
		proglog.Warnf("[prism] warning: cleanup: volume sweep: podman volume rm for %q failed (%v) — some volumes may remain\n",
			owner.sessionName, err)
		// Same reasoning as the container sweep: we do not know how
		// many of the batch were removed, so we report none rather
		// than mislead the operator.
		return 0
	}
	return len(names)
}

// claimedBySibling reports whether name falls inside one of the
// sibling-session legacy prefixes. See legacySiblingPrefixes for why a
// match means "leave it alone".
func claimedBySibling(name string, siblingPrefixes []string) bool {
	for _, sp := range siblingPrefixes {
		if strings.HasPrefix(name, sp) {
			return true
		}
	}
	return false
}

// formatPodmanArgs renders a slice of args into a single space-joined
// string for inclusion in warning messages.
//
//nolint:unused // debugging helper for warning-message construction
func formatPodmanArgs(args []string) string {
	return fmt.Sprintf("podman %s", strings.Join(args, " "))
}
