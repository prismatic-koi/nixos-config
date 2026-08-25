package review_test

// run_profile_test.go — issue #2097 regression tests for the review
// fan-out's profile-inheritance path.
//
// The child-spawn surfaces (review fan-out, investigate) previously
// resolved their child profile via ResolveActiveProfile(pf, "") — empty
// flag — so every review and investigate session since `--abtest`
// (#1216) shipped ran on the host default regardless of the parent
// worker's `--profile` choice. PR #2093 fixed this for the worker
// layer; #2097 extends the same precedence chain to review and
// investigate by feeding the parent's spawn_inputs.profile_name through
// profile.InheritFromParent.
//
// These tests don't drive the full review.Run / RunAsync (those spin
// up tmux / sidecar / a real agent); they exercise the resolution +
// SpawnOpts construction step that closes the inheritance loop. Three
// behaviours are pinned:
//
//  1. Modern parent → child SpawnOpts.ProfileName = parent's value,
//     spawn_inputs row would record it (AC #5).
//  2. Abtest legs → each leg's children get their own profile,
//     never the sibling's or the state-file value (AC #7).
//  3. Legacy parent → child falls through to state-file > nix-default
//     (AC #8). No regression for pre-#2090 sessions.
//
// Plus a #1207-invariant pin: profile.InheritFromParent is called
// exactly once per round inside review.Run / review.RunAsync, so all
// 5 reviewers in a single round share the same resolved profile. The
// inline call-site comments in run.go assert this in code; this test
// asserts it through behaviour by resolving once and treating the
// result as the value used by every reviewer's SpawnOpts.
//
// Test-suite isolation contract (AGENTS.md, issue #1608):
//   - sidecartest.NewIsolated redirects $XDG_STATE_HOME to a t.TempDir()
//     and sets PRISM_TEST_MODE_RESTRICT_HOSTAPI so no host bus / DB /
//     tmux state is touched.
//   - Session names use the "prism-test@" prefix.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/profile"
	"github.com/prismatic-koi/prism/internal/review"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// seedParentSession inserts the minimal pair of rows (sessions,
// spawn_inputs) needed for profile.InheritFromParent to find the
// parent's spawn-time profile. Passing profileName="" exercises the
// negative path (row exists, column NULL).
func seedParentSession(t *testing.T, d *db.DB, sessionName, profileName string) string {
	t.Helper()
	iid := uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:  iid,
		SessionName: sessionName,
		Repo:        "prism-test",
		Worktree:    "/tmp/" + sessionName,
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession %q: %v", sessionName, err)
	}
	si := db.SpawnInputs{InstanceID: iid}
	if profileName != "" {
		p := profileName
		si.ProfileName = &p
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("InsertSpawnInputs %q: %v", sessionName, err)
	}
	return iid
}

// writeStateFile drops the runtime active-profile state file into the
// sidecartest-owned $XDG_STATE_HOME so the state-file rung of the
// precedence chain has something to read.
func writeStateFile(t *testing.T, profileName string) {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		t.Fatalf("writeStateFile: XDG_STATE_HOME unset")
	}
	dir := filepath.Join(stateHome, "prism")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active-profile"), []byte(profileName+"\n"), 0o644); err != nil {
		t.Fatalf("write active-profile state file: %v", err)
	}
}

// makeProfilesWithReviewSlots builds a ProfilesFile whose every named
// profile defines slots for the canonical review agents (review-goal,
// review-code, review-security, review-qa, review-context). This lets a
// downstream RequireSlot call resolve to a real
// model without surprising "missing slot" errors.
func makeProfilesWithReviewSlots(defaultName string, profileNames ...string) *config.ProfilesFile {
	pf := &config.ProfilesFile{
		Default:  defaultName,
		Profiles: make(map[string]config.ProfileEntry, len(profileNames)),
	}
	reviewSlots := []string{"review-goal", "review-code", "review-security", "review-qa", "review-context", "worker", "investigate"}
	for _, name := range profileNames {
		entry := make(config.ProfileEntry, len(reviewSlots))
		for _, slot := range reviewSlots {
			entry[slot] = config.RoleSlot{
				Provider: "anthropic",
				Model:    name + "/model-" + slot,
				Thinking: "medium",
			}
		}
		pf.Profiles[name] = entry
	}
	return pf
}

// resolvedSpawnInputsForReviewer drives the *production* builder
// review.NewReviewerSpawnOptsForTest (re-exported via export_test.go)
// to produce the same SpawnOpts that Run / RunAsync would hand to
// SpawnSession. We then map that to a db.SpawnInputs row via
// session.SpawnInputsFromOpts so the assertion is exactly the row that
// SpawnSession would write.
//
// This is the critical seam for issue #2097: by driving the real
// builder (rather than constructing a SpawnOpts literal in the test),
// a regression where the production code drops ProfileName from the
// builder output is caught here — the test cannot pass vacuously.
func resolvedSpawnInputsForReviewer(t *testing.T, activeProfile, agentName, parentSession string) db.SpawnInputs {
	t.Helper()
	h, err := harness.New("pi", "", nil, "", "")
	if err != nil {
		t.Fatalf("harness.New(pi): %v", err)
	}
	opts := review.NewReviewerSpawnOptsForTest(review.ReviewerSpawnInputForTest{
		AgentName:          agentName,
		AgentSession:       parentSession + "~review-1-" + agentName,
		Prompt:             "unused-in-this-test",
		Repo:               "prism-test",
		Worktree:           "/tmp/" + parentSession,
		PromptTemplateHash: "test-template-hash",
		IsolationMode:      "bwrap",
		HarnessName:        "pi",
		HarnessHandle:      h,
		ProfileName:        activeProfile,
	})
	return session.SpawnInputsFromOpts(opts)
}

// TestReviewFanout_ProfileInheritedFromModernParent is the AC #5
// positive: parent has spawn_inputs.profile_name=X → every reviewer's
// SpawnOpts.ProfileName = X (so the child's audit row records X and
// the child's runtime populatePIConfig resolves to X's models via the
// #2092 chain).
func TestReviewFanout_ProfileInheritedFromModernParent(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-modern-parent"
	const parentProfile = "anthropic-opus"
	seedParentSession(t, bus.DB, parent, parentProfile)

	// State-file points at a competing profile to prove parent wins.
	writeStateFile(t, "state-file-competing")
	pf := makeProfilesWithReviewSlots("nix-default", parentProfile, "state-file-competing", "nix-default")

	resolved, err := profile.InheritFromParent(bus.DB, parent, pf)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}
	if resolved != parentProfile {
		t.Fatalf("InheritFromParent = %q, want %q", resolved, parentProfile)
	}

	// Mirror the in-Run loop: build a SpawnOpts row for each canonical
	// reviewer using the resolved profile, and assert the audit row
	// would carry profile_name=parentProfile.
	for _, ag := range review.Agents() {
		si := resolvedSpawnInputsForReviewer(t, resolved, ag.Name, parent)
		if si.ProfileName == nil {
			t.Errorf("agent %s: spawn_inputs.profile_name is NULL, want %q",
				ag.Name, parentProfile)
			continue
		}
		if *si.ProfileName != parentProfile {
			t.Errorf("agent %s: spawn_inputs.profile_name = %q, want %q",
				ag.Name, *si.ProfileName, parentProfile)
		}
	}
}

// TestReviewFanout_AbtestLegsResolveIndependently is the AC #7 abtest
// shape: two parents sharing an abtest_pair_id but each carrying its
// own spawn_inputs.profile_name produce review fan-outs that record
// the leg-specific profile on every reviewer's audit row. No leg's
// reviewers bleed across to the sibling leg's profile, and neither
// leg's reviewers leak to the state-file or nix-default rung.
func TestReviewFanout_AbtestLegsResolveIndependently(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const pairID = "test-abtest-pair-2097-review"
	writeStateFile(t, "state-file-must-lose")
	pf := makeProfilesWithReviewSlots("nix-default",
		"abtest-leg-a", "abtest-leg-b", "state-file-must-lose", "nix-default")

	legs := []struct {
		sessionName, profile string
	}{
		{"prism-test@worker-abtest-leg-a-review", "abtest-leg-a"},
		{"prism-test@worker-abtest-leg-b-review", "abtest-leg-b"},
	}
	for _, leg := range legs {
		iid := uuid.New().String()
		if err := bus.DB.InsertSession(db.Session{
			InstanceID:  iid,
			SessionName: leg.sessionName,
			Repo:        "prism-test",
			Worktree:    "/tmp/" + leg.sessionName,
			Harness:     "pi",
		}); err != nil {
			t.Fatalf("InsertSession %q: %v", leg.sessionName, err)
		}
		p := leg.profile
		pair := pairID
		if err := bus.DB.InsertSpawnInputs(db.SpawnInputs{
			InstanceID:   iid,
			ProfileName:  &p,
			AbtestPairID: &pair,
		}); err != nil {
			t.Fatalf("InsertSpawnInputs %q: %v", leg.sessionName, err)
		}
	}

	for _, leg := range legs {
		t.Run(leg.profile, func(t *testing.T) {
			resolved, err := profile.InheritFromParent(bus.DB, leg.sessionName, pf)
			if err != nil {
				t.Fatalf("InheritFromParent: %v", err)
			}
			if resolved != leg.profile {
				t.Fatalf("InheritFromParent = %q, want %q (per-leg profile must survive)",
					resolved, leg.profile)
			}
			if resolved == "state-file-must-lose" || resolved == "nix-default" {
				t.Fatalf("leg %q leaked to state-file / nix-default \u2014 #2097 regression",
					leg.profile)
			}

			// Every canonical reviewer's SpawnOpts.ProfileName must
			// carry this leg's profile, not the sibling's or the
			// state-file fallback.
			for _, ag := range review.Agents() {
				si := resolvedSpawnInputsForReviewer(t, resolved, ag.Name, leg.sessionName)
				if si.ProfileName == nil || *si.ProfileName != leg.profile {
					var got string
					if si.ProfileName != nil {
						got = *si.ProfileName
					}
					t.Errorf("leg %q agent %s: spawn_inputs.profile_name = %q, want %q",
						leg.profile, ag.Name, got, leg.profile)
				}
			}
		})
	}
}

// TestReviewFanout_LegacyParentFallsThroughToStateFile is the AC #8
// negative: parent has no spawn_inputs row (pre-#2090) → resolution
// falls through to the state-file value → no regression for legacy
// sessions. The child's audit row records the resolved state-file
// value, and the child's runtime populatePIConfig resolves to that
// same value via the #2092 chain (so behaviour matches the pre-#2097
// world where legacy parents ran on whatever state-file pointed at).
func TestReviewFanout_LegacyParentFallsThroughToStateFile(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-legacy-review"
	// No InsertSession / InsertSpawnInputs \u2014 the parent is fully legacy.
	writeStateFile(t, "state-file-legacy-default")
	pf := makeProfilesWithReviewSlots("nix-default",
		"state-file-legacy-default", "nix-default")

	resolved, err := profile.InheritFromParent(bus.DB, parent, pf)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}
	if resolved != "state-file-legacy-default" {
		t.Fatalf("InheritFromParent = %q, want \"state-file-legacy-default\"", resolved)
	}

	for _, ag := range review.Agents() {
		si := resolvedSpawnInputsForReviewer(t, resolved, ag.Name, parent)
		if si.ProfileName == nil || *si.ProfileName != "state-file-legacy-default" {
			var got string
			if si.ProfileName != nil {
				got = *si.ProfileName
			}
			t.Errorf("legacy parent agent %s: spawn_inputs.profile_name = %q, want \"state-file-legacy-default\"",
				ag.Name, got)
		}
	}
}

// TestReviewFanout_LegacyParentFallsThroughToNixDefault completes the
// AC #8 chain: legacy parent + no state-file → resolution lands on
// pf.Default. This is the path every pre-#2097 review ran on for users
// who had not invoked `prism profile set`.
func TestReviewFanout_LegacyParentFallsThroughToNixDefault(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-fully-legacy-review"
	// No InsertSession / InsertSpawnInputs, and no state-file write.
	pf := makeProfilesWithReviewSlots("nix-default", "nix-default")

	resolved, err := profile.InheritFromParent(bus.DB, parent, pf)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}
	if resolved != "nix-default" {
		t.Fatalf("InheritFromParent = %q, want \"nix-default\"", resolved)
	}

	for _, ag := range review.Agents() {
		si := resolvedSpawnInputsForReviewer(t, resolved, ag.Name, parent)
		if si.ProfileName == nil || *si.ProfileName != "nix-default" {
			var got string
			if si.ProfileName != nil {
				got = *si.ProfileName
			}
			t.Errorf("agent %s: spawn_inputs.profile_name = %q, want \"nix-default\"",
				ag.Name, got)
		}
	}
}

// TestReviewFanout_RoundLevelConsistency_Issue1207 is the regression
// pin for the #1207 invariant referenced in
// internal/review/run.go (the resolution-once-per-round comment).
// Even though the parent's profile_name could change between two
// review rounds for the same parent (e.g. a `prism profile set` in
// between), within a single round every reviewer must share the same
// resolved profile.
//
// We assert this by treating InheritFromParent as a pure function of
// (parent, state, pf) and confirming every reviewer in a single
// "round snapshot" sees the same value. The production code calls
// InheritFromParent exactly once outside the agent loop in both Run
// and RunAsync \u2014 the snapshot here mirrors that pattern.
func TestReviewFanout_RoundLevelConsistency_Issue1207(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const parent = "prism-test@worker-round-consistency"
	const parentProfile = "round-consistent-profile"
	seedParentSession(t, bus.DB, parent, parentProfile)
	pf := makeProfilesWithReviewSlots("nix-default", parentProfile, "nix-default")

	// One resolution for the round (mirrors the inline call site in Run).
	resolved, err := profile.InheritFromParent(bus.DB, parent, pf)
	if err != nil {
		t.Fatalf("InheritFromParent: %v", err)
	}

	// Every reviewer in the round must see this exact value.
	seen := make(map[string]int, len(review.Agents()))
	for _, ag := range review.Agents() {
		si := resolvedSpawnInputsForReviewer(t, resolved, ag.Name, parent)
		if si.ProfileName == nil {
			t.Fatalf("agent %s: spawn_inputs.profile_name is NULL, want %q",
				ag.Name, parentProfile)
		}
		seen[*si.ProfileName]++
	}
	if len(seen) != 1 {
		t.Fatalf("round-level consistency broken: reviewers resolved to %d distinct profiles %v",
			len(seen), seen)
	}
	if _, ok := seen[parentProfile]; !ok {
		t.Errorf("round resolved to profiles %v, want a single %q entry", seen, parentProfile)
	}
}
