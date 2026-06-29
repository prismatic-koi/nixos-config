package cmd

// incarnation_stats_containers_test.go — per-incarnation detail view
// coverage for the new containers_flag audit column (#2317 / #2323).
//
// AC #7 of #2323 requires both surfaces to carry containers_flag:
//
//   - `prism stats <instance-id>` (human-readable) shows a containers_flag
//     row in the Spawn Inputs block.
//   - `prism stats <instance-id> --json` includes "containers_flag": true
//     in the spawn_inputs object.
//
// AC #8 (security) requires that a session row created BEFORE this PR
// landed sees no behavioural change. For the per-incarnation detail view
// that translates to: a session with NO spawn_inputs row (the pre-#2087
// shape) renders identically to its pre-#2323 baseline — no Spawn Inputs
// header line, no containers_flag row, no spawn_inputs key in the JSON
// output.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestRunStatsDetail_ContainersFlag_HumanReadable verifies that the per-
// incarnation detail human-readable output surfaces containers_flag in the
// Spawn Inputs block when --containers was recorded on the spawn_inputs
// row (AC #7 of #2323).
func TestRunStatsDetail_ContainersFlag_HumanReadable(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "pi",
		base.Add(-1*time.Hour), base, "finished", "")

	// Insert a spawn_inputs row with containers_flag=true.
	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:     iid,
		ProfileName:    strPtr("anthropic"),
		HarnessFlag:    strPtr("pi"),
		IsolationFlag:  strPtr("bwrap"),
		IsolationMode:  strPtr("bwrap"),
		ContainersFlag: true,
		CreatedAt:      base.UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runStatsDetail(iid, false, false); err != nil {
			t.Errorf("runStatsDetail: %v", err)
		}
	})

	if !strings.Contains(out, "Spawn Inputs") {
		t.Errorf("expected 'Spawn Inputs' header in output\ngot:\n%s", out)
	}
	if !strings.Contains(out, "containers_flag:") {
		t.Errorf("expected 'containers_flag:' row in Spawn Inputs block\ngot:\n%s", out)
	}
	if !strings.Contains(out, "true") {
		t.Errorf("expected containers_flag value 'true' in output\ngot:\n%s", out)
	}
}

// TestRunStatsDetail_ContainersFlag_JSON verifies that --json emits a
// spawn_inputs object that includes "containers_flag": true when the
// spawn_inputs row recorded the flag (AC #7).
func TestRunStatsDetail_ContainersFlag_JSON(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "pi",
		base.Add(-1*time.Hour), base, "finished", "")

	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:     iid,
		ProfileName:    strPtr("anthropic"),
		HarnessFlag:    strPtr("pi"),
		IsolationMode:  strPtr("bwrap"),
		ContainersFlag: true,
		CreatedAt:      base.UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runStatsDetail(iid, false, true); err != nil {
			t.Errorf("runStatsDetail --json: %v", err)
		}
	})

	var parsed struct {
		Session     *db.Session    `json:"session"`
		SpawnInputs map[string]any `json:"spawn_inputs"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &parsed); err != nil {
		t.Fatalf("unmarshal JSON output: %v\nraw: %s", err, out)
	}
	if parsed.SpawnInputs == nil {
		t.Fatalf("spawn_inputs object missing from --json output; raw: %s", out)
	}
	v, ok := parsed.SpawnInputs["containers_flag"]
	if !ok {
		t.Fatalf("spawn_inputs.containers_flag missing from --json output; raw: %s", out)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("spawn_inputs.containers_flag = %T %v, want bool; raw: %s", v, v, out)
	}
	if !b {
		t.Errorf("spawn_inputs.containers_flag = false, want true")
	}
}

// TestRunStatsDetail_ContainersFlag_DefaultFalse verifies that a row whose
// spawn_inputs.containers_flag=false (the default) surfaces as "false" in
// the human-readable output and false in the JSON spawn_inputs object.
// This is AC #3's render-side guarantee: an unset --containers spawn shows
// up as "false" everywhere, never as missing.
func TestRunStatsDetail_ContainersFlag_DefaultFalse(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "pi",
		base.Add(-1*time.Hour), base, "finished", "")

	if err := d.InsertSpawnInputs(db.SpawnInputs{
		InstanceID:     iid,
		ProfileName:    strPtr("anthropic"),
		HarnessFlag:    strPtr("pi"),
		IsolationMode:  strPtr("bwrap"),
		ContainersFlag: false,
		CreatedAt:      base.UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	// Human-readable: containers_flag: false row present.
	textOut := captureStdout(t, func() {
		if err := runStatsDetail(iid, false, false); err != nil {
			t.Errorf("runStatsDetail: %v", err)
		}
	})
	if !strings.Contains(textOut, "containers_flag:") {
		t.Errorf("expected 'containers_flag:' row in Spawn Inputs block\ngot:\n%s", textOut)
	}
	if !strings.Contains(textOut, "false") {
		t.Errorf("expected containers_flag value 'false' in output\ngot:\n%s", textOut)
	}

	// JSON: spawn_inputs.containers_flag = false (present, explicitly false).
	jsonOut := captureStdout(t, func() {
		if err := runStatsDetail(iid, false, true); err != nil {
			t.Errorf("runStatsDetail --json: %v", err)
		}
	})
	var parsed struct {
		SpawnInputs map[string]any `json:"spawn_inputs"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonOut)), &parsed); err != nil {
		t.Fatalf("unmarshal JSON output: %v\nraw: %s", err, jsonOut)
	}
	if parsed.SpawnInputs == nil {
		t.Fatalf("spawn_inputs object missing from --json output; raw: %s", jsonOut)
	}
	v, ok := parsed.SpawnInputs["containers_flag"]
	if !ok {
		t.Fatalf("spawn_inputs.containers_flag missing from --json output; raw: %s", jsonOut)
	}
	if b, ok := v.(bool); !ok || b {
		t.Errorf("spawn_inputs.containers_flag = %v, want false", v)
	}
}

// TestRunStatsDetail_PreExistingRowNoSpawnInputs verifies AC #8 of #2323:
// a session created BEFORE this PR landed (no spawn_inputs row) sees no
// behavioural change in the detail view — no Spawn Inputs header, no
// containers_flag row, no spawn_inputs key in the JSON output.
//
// This is the "old data does not regress" guarantee. The renderer must
// gracefully degrade for pre-#2087 sessions rather than printing a sea of
// "—" lines or crashing on nil.
func TestRunStatsDetail_PreExistingRowNoSpawnInputs(t *testing.T) {
	d := openIncarnationTestDB(t)
	base := time.Now().Truncate(time.Second)

	iid := uuid.New().String()
	insertTestSession(t, d, iid, "testrepo@main", "testrepo", "/code", "pi",
		base.Add(-1*time.Hour), base, "finished", "")
	// Deliberately NO InsertSpawnInputs call — simulating a pre-#2087 row.

	// Human-readable: no Spawn Inputs block.
	textOut := captureStdout(t, func() {
		if err := runStatsDetail(iid, false, false); err != nil {
			t.Errorf("runStatsDetail: %v", err)
		}
	})
	if strings.Contains(textOut, "Spawn Inputs") {
		t.Errorf("pre-#2087 row should not render Spawn Inputs header\ngot:\n%s", textOut)
	}
	if strings.Contains(textOut, "containers_flag") {
		t.Errorf("pre-#2087 row should not render containers_flag\ngot:\n%s", textOut)
	}

	// JSON: no spawn_inputs key.
	jsonOut := captureStdout(t, func() {
		if err := runStatsDetail(iid, false, true); err != nil {
			t.Errorf("runStatsDetail --json: %v", err)
		}
	})
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonOut)), &raw); err != nil {
		t.Fatalf("unmarshal JSON output: %v\nraw: %s", err, jsonOut)
	}
	if _, present := raw["spawn_inputs"]; present {
		t.Errorf("pre-#2087 row should omit spawn_inputs key from JSON output; raw: %s", jsonOut)
	}
}
