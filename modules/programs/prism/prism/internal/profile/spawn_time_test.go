package profile

// spawn_time_test.go — unit tests for the spawn-time lookup helper.
//
// SpawnTimeForSession lives in this package so the child-spawn surfaces
// (`internal/review`, `cmd/investigate.go`) can read the same column
// without an import cycle. The tests here pin the lookup behaviour and
// the best-effort error swallowing contract.
//
// Test-suite isolation contract (AGENTS.md):
//   - sidecartest.NewIsolated redirects $XDG_STATE_HOME to a t.TempDir()
//     and sets PRISM_TEST_MODE_RESTRICT_HOSTAPI so no host bus / DB /
//     tmux state is touched.
//   - Session names use the "prism-test@" prefix.

import (
	"testing"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// seedSessionWithSpawnInputs is a small fixture helper: it inserts a
// sessions row + a spawn_inputs row for the given session name. When
// profileName is "", the spawn_inputs row is still written but with NULL
// profile_name — exercising the negative path where the row exists but
// the column is empty.
func seedSessionWithSpawnInputs(t *testing.T, d *db.DB, sessionName, profileName string) string {
	t.Helper()
	instanceID := uuid.New().String()
	if err := d.InsertSession(db.Session{
		InstanceID:  instanceID,
		SessionName: sessionName,
		Repo:        "prism-test",
		Worktree:    "/tmp/" + sessionName,
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("seedSessionWithSpawnInputs InsertSession %q: %v", sessionName, err)
	}
	si := db.SpawnInputs{InstanceID: instanceID}
	if profileName != "" {
		p := profileName
		si.ProfileName = &p
	}
	if err := d.InsertSpawnInputs(si); err != nil {
		t.Fatalf("seedSessionWithSpawnInputs InsertSpawnInputs %q: %v", sessionName, err)
	}
	return instanceID
}

// TestSpawnTimeForSession_Positive is the core lookup happy path: a
// session with spawn_inputs.profile_name=X returns X.
func TestSpawnTimeForSession_Positive(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const sessionName = "prism-test@worker-positive"
	const wantProfile = "anthropic-opus"
	seedSessionWithSpawnInputs(t, bus.DB, sessionName, wantProfile)

	got := SpawnTimeForSession(bus.DB, sessionName)
	if got != wantProfile {
		t.Errorf("SpawnTimeForSession = %q, want %q", got, wantProfile)
	}
}

// TestSpawnTimeForSession_NullProfileName is the AC #8 negative path:
// the spawn_inputs row exists but profile_name is NULL → "". Callers
// fall through to ResolveActiveProfile's state-file > nix-default chain
// unchanged (no regression for legacy / host-mode sessions that record
// the audit row without a profile).
func TestSpawnTimeForSession_NullProfileName(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const sessionName = "prism-test@worker-null-profile"
	seedSessionWithSpawnInputs(t, bus.DB, sessionName, "" /* NULL profile_name */)

	if got := SpawnTimeForSession(bus.DB, sessionName); got != "" {
		t.Errorf("SpawnTimeForSession = %q, want \"\" (NULL profile_name must collapse to empty)", got)
	}
}

// TestSpawnTimeForSession_MissingSpawnInputs covers a sessions row
// that has no spawn_inputs row (legacy). The lookup must
// short-circuit without error.
func TestSpawnTimeForSession_MissingSpawnInputs(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const sessionName = "prism-test@worker-legacy-no-spawn-inputs"
	if err := bus.DB.InsertSession(db.Session{
		InstanceID:  uuid.New().String(),
		SessionName: sessionName,
		Repo:        "prism-test",
		Worktree:    "/tmp/" + sessionName,
		Harness:     "pi",
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	// No InsertSpawnInputs call → the row is absent.

	if got := SpawnTimeForSession(bus.DB, sessionName); got != "" {
		t.Errorf("SpawnTimeForSession = %q, want \"\" (missing spawn_inputs row must collapse to empty)", got)
	}
}

// TestSpawnTimeForSession_MissingSession covers the case where the
// session name is not in the DB at all. The lookup must return ""
// rather than erroring — this is the contract that lets a transient DB
// problem or an unknown session-name not wedge the spawn path.
func TestSpawnTimeForSession_MissingSession(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	if got := SpawnTimeForSession(bus.DB, "prism-test@no-such-session"); got != "" {
		t.Errorf("SpawnTimeForSession = %q, want \"\" (unknown session must collapse to empty)", got)
	}
}

// TestSpawnTimeForSession_NilDB exercises the explicit nil-d guard:
// passing a nil *db.DB must short-circuit before any I/O and return ""
// rather than panic on nil-dereference. This is the contract callers
// rely on when they want to call the helper unconditionally and let
// the empty return drive the fallback chain.
func TestSpawnTimeForSession_NilDB(t *testing.T) {
	if got := SpawnTimeForSession(nil, "prism-test@anything"); got != "" {
		t.Errorf("SpawnTimeForSession(nil, ...) = %q, want \"\"", got)
	}
}

// TestSpawnTimeForSession_EmptySession is the empty-string-session
// short-circuit. The helper must not touch the DB at all.
func TestSpawnTimeForSession_EmptySession(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	if got := SpawnTimeForSession(bus.DB, ""); got != "" {
		t.Errorf("SpawnTimeForSession(d, \"\") = %q, want \"\"", got)
	}
}

// TestSpawnTimeForSession_AbtestPair pins the AC #7 abtest-leg shape:
// two sessions with distinct profile_name values each resolve to their
// own value. The pair_id is also set to mirror the on-disk shape
// `prism spawn --abtest` produces; the lookup keys off instance_id (via
// session name) so the pair_id is informational here, but its presence
// guards against an accidental aggregate-by-pair regression.
func TestSpawnTimeForSession_AbtestPair(t *testing.T) {
	bus := sidecartest.NewIsolated(t, "")
	const pairID = "test-abtest-pair-2097"
	legs := []struct {
		sessionName string
		profile     string
	}{
		{"prism-test@worker-abtest-leg-a", "abtest-leg-a"},
		{"prism-test@worker-abtest-leg-b", "abtest-leg-b"},
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
			if got := SpawnTimeForSession(bus.DB, leg.sessionName); got != leg.profile {
				t.Errorf("SpawnTimeForSession(%q) = %q, want %q (abtest legs must not bleed across)",
					leg.sessionName, got, leg.profile)
			}
		})
	}
}
