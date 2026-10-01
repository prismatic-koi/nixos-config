package cmd

// Tests for identity-based ownership in the orphan-resource sweep
// (issue #2951).
//
// The sweep used to decide what to remove by testing one name prefix
// against another. Two distinct live sessions collide under that rule —
// by FOLDING (`repo@feat/x` and `repo@feat-x` sanitise to one string)
// and by NESTING (`foo` is a strict prefix of `foo-bar`) — so one
// session's cleanup could destroy another's data volume, and the guard
// that stopped it leaked the resources it spared.
//
// These tests pin the replacement: a resource is this session's when its
// name carries the instance token of one of the session's incarnations,
// and never otherwise.
//
// # Which of these fail without the change
//
// Three of them are the revert-and-watch-fail pairs, and they are worth
// naming because the surrounding file has tests that pass either way:
//
//   - TestVolumeSweep_FoldedSiblingVolumeNotSwept — the old rule skipped
//     an EQUAL sibling prefix as "this is me", so a folded sibling's
//     volumes were swept.
//   - TestVolumeSweep_ForeignInstanceVolumeSurvivesWithoutSiblingGuard —
//     the old rule's protection came entirely from a database read, so a
//     failed read destroyed the sibling's data.
//   - TestVolumeSweep_OwnNestedNameIsSwept — the old guard suppressed a
//     legitimate sweep, which is the leak half of the same defect.
//
// The rest are positive controls and back-compat pins.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/proglog"
)

// Two canonical UUIDs standing in for two session incarnations, and a
// third for an earlier incarnation of the session being cleaned.
const (
	swIIDA = "3f2a1b0c-1234-5678-9abc-def012345678"
	swIIDB = "7d4e5f60-abcd-4321-8765-0fedcba98765"
	swIIDC = "11112222-3333-4444-5555-666677778888"
)

// ownerFor builds the owner for a session with a single incarnation and
// no legacy siblings — the shape most of these tests want.
func ownerFor(session, instanceID string) resourceOwner {
	return newResourceOwner(session, []string{instanceID}, nil)
}

// prefixFor is the name prefix the proxy injects for one incarnation.
func prefixFor(session, instanceID string) string {
	return container.ResourceNamePrefixForOwner(instanceID, session)
}

// lines joins names into the newline-terminated shape podman's
// --format output takes.
func lines(names ...string) []byte {
	return []byte(strings.Join(names, "\n") + "\n")
}

// rmArgsFrom returns the resource names passed to the removal command,
// stripping the leading verb tokens.
func rmArgsFrom(t *testing.T, calls [][]string, verbTokens int) []string {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("expected a list call and a removal call; got %d: %v", len(calls), calls)
	}
	return calls[1][verbTokens:]
}

// ── NESTING: containers ───────────────────────────────────────────────────

// TestSweep_NestedForeignInstanceContainerNotSwept is the nesting shape
// for containers. Session `foo` cleans up while live session `foo-bar`
// holds containers whose names start with `foo`'s legacy prefix.
func TestSweep_NestedForeignInstanceContainerNotSwept(t *testing.T) {
	r := installFakeRunner(t)
	mine := prefixFor("foo", swIIDA) + "aaaaaaaa"
	theirs := prefixFor("foo-bar", swIIDB) + "bbbbbbbb"
	r.script(
		scriptedResponse{stdout: lines(mine, theirs)},
		scriptedResponse{stdout: []byte("")},
	)

	got := sweepWithRunner(r, ownerFor("foo", swIIDA))
	if got != 1 {
		t.Fatalf("count: got %d, want 1", got)
	}
	rm := rmArgsFrom(t, r.calls(), 2)
	if len(rm) != 1 || rm[0] != mine {
		t.Fatalf("rm args: got %v, want [%s]", rm, mine)
	}
	for _, name := range rm {
		if name == theirs {
			t.Errorf("SECURITY: sweep removed live session foo-bar's container %q", name)
		}
	}
}

// ── NESTING: volumes ──────────────────────────────────────────────────────

// TestVolumeSweep_ForeignInstanceVolumeSurvivesWithoutSiblingGuard is a
// revert-and-watch-fail pin. The owner carries NO legacy sibling
// prefixes, which is what a failed database read produces. Under the old
// rule that degradation removed the guard entirely and the sibling's
// data volume went with it; under identity the protection does not
// depend on a database read at all.
func TestVolumeSweep_ForeignInstanceVolumeSurvivesWithoutSiblingGuard(t *testing.T) {
	r := installFakeRunner(t)
	mine := prefixFor("foo", swIIDA) + "aaaaaaaa"
	theirs := prefixFor("foo-bar", swIIDB) + "pgdata"
	r.script(
		scriptedResponse{stdout: lines(mine, theirs)},
		scriptedResponse{stdout: []byte("")},
	)

	// nil siblings: the guard contributes nothing.
	got := sweepVolumesWithRunner(r, newResourceOwner("foo", []string{swIIDA}, nil))
	if got != 1 {
		t.Fatalf("count: got %d, want 1", got)
	}
	rm := rmArgsFrom(t, r.calls(), 2)
	for _, name := range rm {
		if name == theirs {
			t.Fatalf("SECURITY: sweep destroyed live session foo-bar's data volume %q with no database guard in play", name)
		}
	}
	if len(rm) != 1 || rm[0] != mine {
		t.Errorf("volume rm args: got %v, want [%s]", rm, mine)
	}
}

// TestVolumeSweep_OwnNestedNameIsSwept is the leak half of the same
// defect, and the other revert-and-watch-fail pin. Session `foo` is
// entitled to name a volume `<its prefix>bar-data`. The old guard could
// not tell that name from live session `foo-bar`'s, so it left it in
// place and the volume leaked. Identity removes the ambiguity, so the
// sweep reaches it.
func TestVolumeSweep_OwnNestedNameIsSwept(t *testing.T) {
	r := installFakeRunner(t)
	mine := prefixFor("foo", swIIDA) + "bar-data"
	theirs := prefixFor("foo-bar", swIIDB) + "data"
	r.script(
		scriptedResponse{stdout: lines(mine, theirs)},
		scriptedResponse{stdout: []byte("")},
	)

	// A live sibling IS present in the legacy guard, and it must not
	// suppress an identity-scoped name.
	owner := newResourceOwner("foo", []string{swIIDA},
		[]string{container.ResourceNamePrefixForSession("foo-bar")})
	got := sweepVolumesWithRunner(r, owner)
	if got != 1 {
		t.Fatalf("count: got %d, want 1 (the session's own nested-looking volume must not leak)", got)
	}
	rm := rmArgsFrom(t, r.calls(), 2)
	if len(rm) != 1 || rm[0] != mine {
		t.Fatalf("volume rm args: got %v, want [%s]", rm, mine)
	}
}

// ── FOLDING ───────────────────────────────────────────────────────────────

// TestVolumeSweep_FoldedSiblingVolumeNotSwept is the folding shape from
// the issue title, and the third revert-and-watch-fail pin. The two
// session names sanitise to ONE prefix, so the old rule read the
// sibling's prefix as its own, skipped it as "this is me", and swept the
// sibling's data volume.
func TestVolumeSweep_FoldedSiblingVolumeNotSwept(t *testing.T) {
	const slashed = "repo@feat/x"
	const hyphened = "repo@feat-x"
	if container.ResourceNamePrefixForSession(slashed) != container.ResourceNamePrefixForSession(hyphened) {
		t.Fatalf("test premise broken: %q and %q no longer fold together", slashed, hyphened)
	}

	r := installFakeRunner(t)
	mine := prefixFor(slashed, swIIDA) + "pgdata"
	theirs := prefixFor(hyphened, swIIDB) + "pgdata"
	r.script(
		scriptedResponse{stdout: lines(mine, theirs)},
		scriptedResponse{stdout: []byte("")},
	)

	got := sweepVolumesWithRunner(r, ownerFor(slashed, swIIDA))
	if got != 1 {
		t.Fatalf("count: got %d, want 1", got)
	}
	rm := rmArgsFrom(t, r.calls(), 2)
	for _, name := range rm {
		if name == theirs {
			t.Fatalf("SECURITY: sweep of %q destroyed the data volume of live session %q", slashed, hyphened)
		}
	}
}

// ── Incarnations ──────────────────────────────────────────────────────────

// TestVolumeSweep_EveryIncarnationIsSwept pins that a new incarnation does
// not orphan the volumes the previous incarnation created. A session that
// is opened again after its previous incarnation ended (`prism switch`
// after a close), or that the `prism restore` fallback restarts, gets a NEW
// instance ID for the same session name. A sweep that knew only the current
// token would leave the older volumes on the host forever.
func TestVolumeSweep_EveryIncarnationIsSwept(t *testing.T) {
	r := installFakeRunner(t)
	current := prefixFor("repo@main", swIIDA) + "pgdata"
	earlier := prefixFor("repo@main", swIIDC) + "pgdata"
	foreign := prefixFor("repo@other", swIIDB) + "pgdata"
	r.script(
		scriptedResponse{stdout: lines(current, earlier, foreign)},
		scriptedResponse{stdout: []byte("")},
	)

	owner := newResourceOwner("repo@main", []string{swIIDA, swIIDC}, nil)
	got := sweepVolumesWithRunner(r, owner)
	if got != 2 {
		t.Fatalf("count: got %d, want 2 (both incarnations)", got)
	}
	rm := rmArgsFrom(t, r.calls(), 2)
	for _, want := range []string{current, earlier} {
		var saw bool
		for _, name := range rm {
			if name == want {
				saw = true
			}
		}
		if !saw {
			t.Errorf("volume %q was not swept; args=%v", want, rm)
		}
	}
	for _, name := range rm {
		if name == foreign {
			t.Errorf("SECURITY: sweep removed another session's volume %q", name)
		}
	}
}

// TestResourceOwnerForSession_UnionsIncarnationsFromDB covers the
// assembly, which the sweep tests above do not reach: they build the
// owner directly, so nothing else pins how the token set is derived from
// the database.
func TestResourceOwnerForSession_UnionsIncarnationsFromDB(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	const session = "repo@main"
	seedContainerSession(t, dbFile, session, true)

	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	// Two incarnations of this session, and one of another session that
	// must not contribute a token.
	for _, s := range []db.Session{
		{InstanceID: swIIDA, SessionName: session, Repo: "repo", Worktree: "/tmp/wt"},
		{InstanceID: swIIDC, SessionName: session, Repo: "repo", Worktree: "/tmp/wt"},
		{InstanceID: swIIDB, SessionName: "repo@other", Repo: "repo", Worktree: "/tmp/other"},
	} {
		if err := d.InsertSession(s); err != nil {
			t.Fatalf("InsertSession(%s): %v", s.InstanceID, err)
		}
	}
	if err := d.SetInstanceID(session, swIIDA); err != nil {
		t.Fatalf("SetInstanceID: %v", err)
	}
	status, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}

	owner := resourceOwnerForSession(d, session, status)

	want := map[string]bool{
		container.InstanceTokenForID(swIIDA): true,
		container.InstanceTokenForID(swIIDC): true,
	}
	if len(owner.instanceTokens) != len(want) {
		t.Fatalf("tokens: got %v, want exactly the two incarnations of %q", owner.instanceTokens, session)
	}
	for _, tok := range owner.instanceTokens {
		if !want[tok] {
			t.Errorf("unexpected token %q — another session's incarnation must not be swept", tok)
		}
	}
}

// ── Back-compat: resources created before identity naming ─────────────────

// TestVolumeSweep_LegacyNameStillSweptAlongsideIdentity is the
// back-compat AC. A volume created before this change carries the legacy
// prefix and no token. Cleanup of its owning session must still reach
// it, in the same call as the identity-scoped ones, or the upgrade
// orphans every volume already on the host.
func TestVolumeSweep_LegacyNameStillSweptAlongsideIdentity(t *testing.T) {
	r := installFakeRunner(t)
	identityScoped := prefixFor("foo", swIIDA) + "pgdata"
	legacy := container.ResourceNamePrefixForSession("foo") + "pgdata"
	r.script(
		scriptedResponse{stdout: lines(identityScoped, legacy)},
		scriptedResponse{stdout: []byte("")},
	)

	got := sweepVolumesWithRunner(r, ownerFor("foo", swIIDA))
	if got != 2 {
		t.Fatalf("count: got %d, want 2 (the pre-identity volume must not be orphaned by the upgrade)", got)
	}
	rm := rmArgsFrom(t, r.calls(), 2)
	for _, want := range []string{identityScoped, legacy} {
		var saw bool
		for _, name := range rm {
			if name == want {
				saw = true
			}
		}
		if !saw {
			t.Errorf("volume %q was not swept; args=%v", want, rm)
		}
	}
}

// TestSweep_LegacyContainerShapeStillSwept is the container half of the
// same back-compat AC. The legacy rule is the strict auto-name shape,
// unchanged: a pre-identity container matching it is still removed, and
// a pre-identity name that does not match it is still left alone.
func TestSweep_LegacyContainerShapeStillSwept(t *testing.T) {
	r := installFakeRunner(t)
	legacyAuto := container.ResourceNamePrefixForSession("foo") + "aaaaaaaa"
	legacyUserNamed := container.ResourceNamePrefixForSession("foo") + "mycontainer"
	r.script(
		scriptedResponse{stdout: lines(legacyAuto, legacyUserNamed)},
		scriptedResponse{stdout: []byte("")},
	)

	got := sweepWithRunner(r, ownerFor("foo", swIIDA))
	if got != 1 {
		t.Fatalf("count: got %d, want 1", got)
	}
	rm := rmArgsFrom(t, r.calls(), 2)
	if len(rm) != 1 || rm[0] != legacyAuto {
		t.Errorf("rm args: got %v, want [%s]", rm, legacyAuto)
	}
}

// TestVolumeSweep_LegacyNameOfFoldedSiblingNotSwept pins the one gap
// that stays open, and pins that it is guarded rather than ignored. A
// PRE-IDENTITY volume carries no token, so nothing in its name says
// which of two folded sessions created it. The sweep leaves it and warns
// instead of guessing, which is the same direction the sibling guard
// already took for the nested shape.
func TestVolumeSweep_LegacyNameOfFoldedSiblingNotSwept(t *testing.T) {
	r := installFakeRunner(t)
	const slashed = "repo@feat/x"
	legacy := container.ResourceNamePrefixForSession(slashed) + "pgdata"
	r.script(scriptedResponse{stdout: lines(legacy)})

	// legacySiblingPrefixes returns this session's own prefix when
	// another LIVE session folds onto it, which suppresses the whole
	// legacy sweep.
	owner := newResourceOwner(slashed, []string{swIIDA},
		[]string{container.ResourceNamePrefixForSession("repo@feat-x")})

	if got := sweepVolumesWithRunner(r, owner); got != 0 {
		t.Fatalf("count: got %d, want 0 (an ambiguous pre-identity name must not be destroyed)", got)
	}
	if calls := r.calls(); len(calls) != 1 {
		t.Errorf("expected the ls invocation only; got %v", calls)
	}
}

// TestLegacySiblingPrefixes_FoldingCollisionSuppressesLegacySweep covers
// the derivation of that guard from the database. The old version
// skipped a prefix EQUAL to its own as "this is me", which is exactly
// how the folded sibling's volumes were swept; self-exclusion now runs
// on the session name instead.
func TestLegacySiblingPrefixes_FoldingCollisionSuppressesLegacySweep(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	const slashed = "repo@feat/x"
	const hyphened = "repo@feat-x"
	for _, s := range []string{slashed, hyphened, "repo@unrelated"} {
		seedContainerSession(t, dbFile, s, true)
	}

	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	got := legacySiblingPrefixes(d, slashed)
	own := container.ResourceNamePrefixForSession(slashed)
	var sawOwn bool
	for _, p := range got {
		if p == own {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Errorf("got %v, want it to contain %q — a live session folds onto this prefix, so every pre-identity name under it is ambiguous", got, own)
	}
}

// TestLegacySiblingPrefixes_ExcludesSelf pins the complement: with no
// folded sibling in the database, the session's own prefix must NOT
// appear, or the legacy sweep suppresses itself and every pre-identity
// resource leaks.
func TestLegacySiblingPrefixes_ExcludesSelf(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	seedContainerSession(t, dbFile, "repo@solo", true)

	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if got := legacySiblingPrefixes(d, "repo@solo"); len(got) != 0 {
		t.Errorf("got %v, want none — the session must not suppress its own legacy sweep", got)
	}
}

// ── Ownership decision, in isolation ──────────────────────────────────────

// TestResourceOwner_OwnershipVerdicts is the table over the decision
// itself, so a failure names the verdict rather than a removal count.
func TestResourceOwner_OwnershipVerdicts(t *testing.T) {
	owner := newResourceOwner("foo", []string{swIIDA},
		[]string{container.ResourceNamePrefixForSession("foo-bar")})
	legacyPrefix := container.ResourceNamePrefixForSession("foo")

	cases := []struct {
		name       string
		wantVolume ownership
		why        string
	}{
		{prefixFor("foo", swIIDA) + "pgdata", ownershipMine, "own identity"},
		{prefixFor("foo", swIIDA) + "bar-data", ownershipMine, "own identity, nested-looking suffix"},
		{prefixFor("foo", swIIDA), ownershipMine, "the bare prefix, which the create policy admits"},
		{prefixFor("foo-bar", swIIDB) + "data", ownershipOther, "live sibling's identity"},
		{prefixFor("foo", swIIDB) + "data", ownershipOtherPrunedIncarnation, "same session name, another incarnation not in our token set — the pruned-incarnation shape (issue #2972)"},
		{legacyPrefix + "pgdata", ownershipMine, "pre-identity name under our legacy prefix"},
		{legacyPrefix + "bar-data", ownershipLegacySiblingClaim, "pre-identity name a live sibling also claims"},
		{"user-" + legacyPrefix + "pgdata", ownershipUnclaimed, "substring trap"},
		{"postgres-data", ownershipUnclaimed, "not a prism resource"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := owner.volumeOwnership(tc.name); got != tc.wantVolume {
				t.Errorf("volumeOwnership(%q) = %d, want %d — %s", tc.name, got, tc.wantVolume, tc.why)
			}
		})
	}
}

// ── Pruned-incarnation skip warning (issue #2972, Part 1) ─────────────────
//
// A resource created by an incarnation whose `sessions` row has since been
// pruned (db.Prune, 90-day window) carries a token this session's owner
// does not hold, even though the name's decorative half reads as this
// session's own name. collectSweepable must warn on that specific shape,
// and on no other, and the warning must never change the removal decision.

// captureStderrDuringFn redirects os.Stderr for the duration of fn and
// returns whatever was written, draining concurrently per the same
// kernel-pipe-buffer defence as captureStdoutDuringFn in
// cleanup_lifecycle_test.go. It also pins proglog's effective level to warn
// for the duration, since collectSweepable's diagnostics go through
// proglog.Warnf, which the package's default level (error) would otherwise
// suppress.
func captureStderrDuringFn(t *testing.T, fn func()) string {
	t.Helper()
	restore := proglog.SetLevelForTest(proglog.LevelWarn)
	defer restore()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	done := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(r)
		done <- buf
	}()

	defer func() {
		os.Stderr = orig
	}()
	fn()
	_ = w.Close()
	return string(<-done)
}

// TestVolumeSweep_PrunedIncarnationSkipIsWarned pins the positive case:
// the owning session ("foo") is cleaning up, another incarnation of the
// very same session name created a volume, and that incarnation's token
// is NOT in the owner's set — the shape a pruned `sessions` row produces.
// The volume must still be skipped (never removed under a stale identity
// guess), but the operator must be told: named, and told the owning
// incarnation is not in the database.
func TestVolumeSweep_PrunedIncarnationSkipIsWarned(t *testing.T) {
	r := installFakeRunner(t)
	mine := prefixFor("foo", swIIDA) + "pgdata"
	pruned := prefixFor("foo", swIIDC) + "pgdata"
	r.script(
		scriptedResponse{stdout: lines(mine, pruned)},
		scriptedResponse{stdout: []byte("")},
	)

	owner := ownerFor("foo", swIIDA)
	var got int
	stderr := captureStderrDuringFn(t, func() {
		got = sweepVolumesWithRunner(r, owner)
	})

	if got != 1 {
		t.Fatalf("count: got %d, want 1 (the pruned incarnation's volume must not be removed)", got)
	}
	rm := rmArgsFrom(t, r.calls(), 2)
	if len(rm) != 1 || rm[0] != mine {
		t.Fatalf("rm args: got %v, want [%s] — the warning must not change the removal decision", rm, mine)
	}
	if !strings.Contains(stderr, pruned) {
		t.Errorf("warning does not name the skipped resource %q; got %q", pruned, stderr)
	}
	if !strings.Contains(stderr, "not in the database") {
		t.Errorf("warning does not state the owning incarnation is missing from the database; got %q", stderr)
	}
}

// TestVolumeSweep_NoPrunedIncarnation_NoWarning is the negative control:
// a sweep that skips no pruned-incarnation-shaped resource must emit no
// warning at all. Ordinary ownershipOther (a live sibling's identity) and
// ownershipUnclaimed names must not trip this path.
func TestVolumeSweep_NoPrunedIncarnation_NoWarning(t *testing.T) {
	r := installFakeRunner(t)
	mine := prefixFor("foo", swIIDA) + "pgdata"
	siblings := prefixFor("bar", swIIDB) + "data"
	r.script(
		scriptedResponse{stdout: lines(mine, siblings)},
		scriptedResponse{stdout: []byte("")},
	)

	owner := ownerFor("foo", swIIDA)
	var got int
	stderr := captureStderrDuringFn(t, func() {
		got = sweepVolumesWithRunner(r, owner)
	})

	if got != 1 {
		t.Fatalf("count: got %d, want 1", got)
	}
	if stderr != "" {
		t.Errorf("expected no warning when no resource skips as a pruned incarnation; got %q", stderr)
	}
}

// TestResourceOwner_DifferentSessionSameShape_NoWarningVerdict pins the
// last edge case directly against the ownership decision: a live
// DIFFERENT session's identity-scoped name must resolve to plain
// ownershipOther, never ownershipOtherPrunedIncarnation, even when it
// happens to sit right next to this session's own resources in a listing.
// Only a decorative half that equals THIS session's sanitised name trips
// the pruned-incarnation verdict.
func TestResourceOwner_DifferentSessionSameShape_NoWarningVerdict(t *testing.T) {
	owner := ownerFor("foo", swIIDA)
	foreign := prefixFor("bar", swIIDB) + "data"
	if got := owner.volumeOwnership(foreign); got != ownershipOther {
		t.Fatalf("volumeOwnership(%q) = %d, want ownershipOther (%d) — a different session's identity is the ordinary not-mine case", foreign, got, ownershipOther)
	}
}

// TestResourceOwner_NestedSiblingIdentityNotMisattributed is the nesting
// variant of the same edge case: a live NESTED sibling ("foo-bar") whose
// suffix happens to make its full name start with this session's
// ("foo") decorative half plus a dash must still resolve to plain
// ownershipOther, not the pruned-incarnation verdict — decoratesAsSelf
// stands down whenever legacySiblingPrefixes already flags the candidate
// as indistinguishable from a live sibling.
func TestResourceOwner_NestedSiblingIdentityNotMisattributed(t *testing.T) {
	owner := newResourceOwner("foo", []string{swIIDA},
		[]string{container.ResourceNamePrefixForSession("foo-bar")})
	foreign := prefixFor("foo-bar", swIIDB) + "data"
	if got := owner.volumeOwnership(foreign); got != ownershipOther {
		t.Fatalf("volumeOwnership(%q) = %d, want ownershipOther (%d) — a live nested sibling's identity must not be misread as this session's pruned incarnation", foreign, got, ownershipOther)
	}
}
