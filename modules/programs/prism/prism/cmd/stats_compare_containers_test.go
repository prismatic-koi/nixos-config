package cmd

// stats_compare_containers_test.go — `prism stats compare` Spawn Inputs
// renderer coverage for the containers_flag axis.
//
// The renderer surface mirrors the existing audit-field axes
// (profile_name, harness, isolation_mode, …). A row with a populated
// spawn_inputs row renders "true" / "false". A row with no spawn_inputs
// renders "—", so sessions with no spawn_inputs row display cleanly.

import (
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
)

// TestInputsValue_ContainersFlagTrue verifies that a spawn_inputs row with
// ContainersFlag=true surfaces as "true" via inputsValue("containers_flag").
// This is the renderer side — the `prism stats compare` Spawn Inputs block
// carries the audit column.
func TestInputsValue_ContainersFlagTrue(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionName = "repo@inputs-containers-true"
	inputs := &db.SpawnInputs{
		ProfileName:    strPtr("anthropic"),
		HarnessFlag:    strPtr("pi"),
		IsolationFlag:  strPtr("bwrap"),
		IsolationMode:  strPtr("bwrap"),
		ContainersFlag: true,
	}
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, inputs)

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})

	if got := inputsValue("containers_flag", runs[0]); got != "true" {
		t.Errorf("inputsValue(containers_flag) = %q, want %q", got, "true")
	}
}

// TestInputsValue_ContainersFlagFalse verifies that an explicitly-false
// containers_flag surfaces as "false" (distinct from "—" / missing). This
// is the discrimination AC: a row with a spawn_inputs entry that recorded
// containers_flag=0 must look different from a row with no spawn_inputs
// at all.
func TestInputsValue_ContainersFlagFalse(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionName = "repo@inputs-containers-false"
	inputs := &db.SpawnInputs{
		ProfileName:    strPtr("anthropic"),
		HarnessFlag:    strPtr("pi"),
		IsolationMode:  strPtr("bwrap"),
		ContainersFlag: false,
	}
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, inputs)

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})

	if got := inputsValue("containers_flag", runs[0]); got != "false" {
		t.Errorf("inputsValue(containers_flag) = %q, want %q (explicit false is not missing)", got, "false")
	}
}

// TestInputsValue_ContainersFlagAbsentRendersDash verifies that a session
// with no spawn_inputs row surfaces containers_flag as "—" (the missing
// glyph). Mirrors TestInputsValue_IsolationModeAbsentRendersDash for the
// no-row case so the renderer never crashes on a nil
// CompareInputs pointer.
func TestInputsValue_ContainersFlagAbsentRendersDash(t *testing.T) {
	d := openStatsTestDB(t)
	startedAt := time.Now().Add(-time.Minute)

	const sessionName = "repo@inputs-containers-absent"
	// No spawn_inputs row at all.
	iid := seedCompareSession(t, d, sessionName, startedAt, agent.StateFinished, nil)

	sess, _ := d.SessionByInstanceID(iid)
	runs := loadCompareRuns(d, []*db.Session{sess})

	if got := inputsValue("containers_flag", runs[0]); got != "—" {
		t.Errorf("inputsValue(containers_flag) on row with no spawn_inputs = %q, want %q",
			got, "—")
	}
}

// TestInputsAxes_IncludesContainersFlag verifies that the canonical axis
// list surfaced by `prism stats compare` includes containers_flag. The
// renderer iterates inputsAxes — adding the new axis without updating the
// list would silently hide it from the table and JSON output.
func TestInputsAxes_IncludesContainersFlag(t *testing.T) {
	for _, axis := range inputsAxes {
		if axis == "containers_flag" {
			return
		}
	}
	t.Errorf("inputsAxes = %v; missing %q", inputsAxes, "containers_flag")
}
