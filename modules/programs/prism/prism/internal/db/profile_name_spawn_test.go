package db

// Tests for the spawn_inputs half of the write-time profile stamp: a spawned
// session records its own profile ahead of the machine-active one.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// unspawnedSession inserts a sessions row with no spawn_inputs row (the
// coordinator shape) and returns the instance_id.
func unspawnedSession(t *testing.T, d *DB, session string) string {
	t.Helper()
	inst := uuid.New().String()
	if _, err := d.conn.Exec(
		`INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		inst, session, "repo", "/code/repo/"+session, "pi", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return inst
}

// spawnedSession inserts a sessions row and a spawn_inputs row for a fresh
// instance and returns the instance_id. profile nil leaves
// spawn_inputs.profile_name NULL.
func spawnedSession(t *testing.T, d *DB, session string, profile *string) string {
	t.Helper()
	inst := unspawnedSession(t, d, session)
	if err := d.InsertSpawnInputs(SpawnInputs{InstanceID: inst, ProfileName: profile, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}
	return inst
}

// writeTestEventFor writes one event carrying instanceID and returns its id.
func writeTestEventFor(t *testing.T, d *DB, session string, instanceID *string) string {
	t.Helper()
	id := uuid.New().String()
	err := d.WriteEvent(Event{
		ID:          id,
		SessionName: session,
		Repo:        "repo",
		Worktree:    "/code/repo/" + session,
		InstanceID:  instanceID,
		Type:        "msg_assistant",
		Payload:     `{"cost":0.01}`,
		CreatedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	return id
}

// machineActive binds d to a machine-active resolver whose state file holds
// active (absent when active is "") and whose nix default is nixDefault.
func machineActive(t *testing.T, d *DB, active, nixDefault string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile")
	if active != "" {
		writeStateFile(t, statePath, active+"\n", time.Now())
	}
	d.SetProfileResolver(newProfileResolverForTest(statePath, nixDefault))
}

// AC [functional]: a spawned session records spawn_inputs.profile_name, and
// a `--profile max` spawn records "max" on a host whose active profile is
// "standard".
func TestWriteEvent_SpawnedSession_RecordsSpawnProfile(t *testing.T) {
	d := openAccountTestDB(t)
	machineActive(t, d, "standard", "standard")
	inst := spawnedSession(t, d, "repo@fix-thing", profilePtr("max"))

	id := writeTestEventFor(t, d, "repo@fix-thing", &inst)

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "max" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"max\", true)", got, ok)
	}
}

// AC [functional]: a session with no spawn_inputs row (a coordinator) records
// the machine-active profile.
func TestWriteEvent_NoSpawnRow_RecordsMachineActive(t *testing.T) {
	d := openAccountTestDB(t)
	machineActive(t, d, "heavy", "standard")
	inst := unspawnedSession(t, d, "repo@main")

	id := writeTestEventFor(t, d, "repo@main", &inst)

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "heavy" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"heavy\", true)", got, ok)
	}
}

// AC [edge-case]: a spawn_inputs row whose profile_name is NULL or empty
// falls back to the machine-active resolution, never an empty label.
func TestWriteEvent_EmptySpawnProfile_FallsBackToMachineActive(t *testing.T) {
	for name, profile := range map[string]*string{"null": nil, "empty": profilePtr(""), "blank": profilePtr("  ")} {
		t.Run(name, func(t *testing.T) {
			d := openAccountTestDB(t)
			machineActive(t, d, "", "light")
			inst := spawnedSession(t, d, "repo@old", profile)

			id := writeTestEventFor(t, d, "repo@old", &inst)

			got, ok := eventProfileName(t, d, id)
			if !ok || got != "light" {
				t.Fatalf("profile_name = (%q, valid=%v), want (\"light\", true)", got, ok)
			}
		})
	}
}

// AC [edge-case]: with an empty spawn profile and no machine-active source,
// the recorded value is the literal "unknown".
func TestWriteEvent_EmptySpawnProfile_NoMachineActive_Unknown(t *testing.T) {
	d := openAccountTestDB(t)
	machineActive(t, d, "", "")
	inst := spawnedSession(t, d, "repo@old", nil)

	id := writeTestEventFor(t, d, "repo@old", &inst)

	got, ok := eventProfileName(t, d, id)
	if !ok || got != unknownProfile {
		t.Fatalf("profile_name = (%q, valid=%v), want (%q, true)", got, ok, unknownProfile)
	}
}

// AC [functional]: two sessions on one host under different profiles record
// two distinct names, whatever the machine-active profile is.
func TestWriteEvent_ConcurrentSessions_DistinctProfiles(t *testing.T) {
	d := openAccountTestDB(t)
	machineActive(t, d, "standard", "standard")
	instA := spawnedSession(t, d, "repo@a", profilePtr("max"))
	instB := spawnedSession(t, d, "repo@b", profilePtr("light"))

	idA := writeTestEventFor(t, d, "repo@a", &instA)
	idB := writeTestEventFor(t, d, "repo@b", &instB)
	idA2 := writeTestEventFor(t, d, "repo@a", &instA)

	for id, want := range map[string]string{idA: "max", idB: "light", idA2: "max"} {
		got, ok := eventProfileName(t, d, id)
		if !ok || got != want {
			t.Fatalf("profile_name for %s = (%q, valid=%v), want (%q, true)", id, got, ok, want)
		}
	}
}

// AC [performance]: N events for one spawned session reach SQLite for the
// spawn row once, and N events for a session with no row reach it once
// within the miss TTL.
func TestWriteEvent_SpawnProfileLookup_CachedPerSession(t *testing.T) {
	d := openAccountTestDB(t)
	machineActive(t, d, "standard", "standard")
	inst := spawnedSession(t, d, "repo@a", profilePtr("max"))
	coord := unspawnedSession(t, d, "repo@main")

	for i := 0; i < 20; i++ {
		writeTestEventFor(t, d, "repo@a", &inst)
		writeTestEventFor(t, d, "repo@main", &coord)
	}

	d.spawnProfileMu.Lock()
	queries := d.spawnProfileQueries
	d.spawnProfileMu.Unlock()
	if queries > 2 {
		t.Errorf("spawn_inputs queried %d times across 40 events on 2 sessions, want at most 2", queries)
	}
}

// A miss is re-queried once its TTL expires, so a session whose first event
// precedes its spawn_inputs row picks the row up afterwards.
func TestWriteEvent_SpawnProfileMiss_RequeriedAfterTTL(t *testing.T) {
	d := openAccountTestDB(t)
	machineActive(t, d, "standard", "standard")
	inst := unspawnedSession(t, d, "repo@late")

	if got := d.spawnProfileFor(inst); got != "" {
		t.Fatalf("before spawn row: spawnProfileFor = %q, want empty", got)
	}
	if err := d.InsertSpawnInputs(SpawnInputs{InstanceID: inst, ProfileName: profilePtr("max"), CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("InsertSpawnInputs: %v", err)
	}

	if got := d.spawnProfileFor(inst); got != "" {
		t.Fatalf("within TTL: spawnProfileFor = %q, want cached miss", got)
	}
	d.spawnProfileMu.Lock()
	ent := d.spawnProfiles[inst]
	ent.expiresAt = time.Now().Add(-time.Second)
	d.spawnProfiles[inst] = ent
	d.spawnProfileMu.Unlock()

	if got := d.spawnProfileFor(inst); got != "max" {
		t.Fatalf("after TTL: spawnProfileFor = %q, want max", got)
	}
}

func profilePtr(s string) *string { return &s }
