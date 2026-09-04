package db

// Tests for the active-profile write path and its mtime-cached resolver
// Internal-package tests so they can reach
// newProfileResolverForTest and the unexported ProfileResolver.reads counter.

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// writeStateFile writes the active-profile state file with content and sets
// its mtime so a switch is deterministically newer than the previous read.
func writeStateFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes state file: %v", err)
	}
}

// eventProfileName reads the profile_name column for one agent_events row.
// The returned bool is false when the column is SQL NULL.
func eventProfileName(t *testing.T, d *DB, id string) (string, bool) {
	t.Helper()
	var name sql.NullString
	err := d.QueryRow(`SELECT profile_name FROM agent_events WHERE id = ?`, id).Scan(&name)
	if err != nil {
		t.Fatalf("select profile_name for %s: %v", id, err)
	}
	return name.String, name.Valid
}

// AC [functional]: an event written while the state file holds "heavy" records
// profile_name='heavy'.
func TestWriteEvent_RecordsActiveProfile_StateFile(t *testing.T) {
	d := openAccountTestDB(t)
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile")
	writeStateFile(t, statePath, "heavy\n", time.Now())
	d.SetProfileResolver(newProfileResolverForTest(statePath, "standard"))

	id := writeTestEvent(t, d, "repo@main")

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "heavy" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"heavy\", true)", got, ok)
	}
}

// AC [functional]: with no state file, an event records the nix default — the
// coordinator case that would otherwise fold to "default".
func TestWriteEvent_RecordsNixDefault_NoStateFile(t *testing.T) {
	d := openAccountTestDB(t)
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile") // never created
	d.SetProfileResolver(newProfileResolverForTest(statePath, "standard"))

	id := writeTestEvent(t, d, "repo@main")

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "standard" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"standard\", true)", got, ok)
	}
}

// AC [edge]: with neither a state file nor a nix default, an event records the
// explicit "unknown" placeholder, never SQL NULL or an empty string.
func TestWriteEvent_UnknownWhenNoStateAndNoDefault(t *testing.T) {
	d := openAccountTestDB(t)
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile") // never created
	d.SetProfileResolver(newProfileResolverForTest(statePath, ""))

	id := writeTestEvent(t, d, "repo@main")

	got, ok := eventProfileName(t, d, id)
	if !ok || got != unknownProfile {
		t.Fatalf("profile_name = (%q, valid=%v), want (%q, true)", got, ok, unknownProfile)
	}
}

// AC [functional]: a whitespace-only state file falls through to the nix
// default, mirroring config.ActiveProfile's empty-means-default contract.
func TestWriteEvent_WhitespaceStateFile_FallsToDefault(t *testing.T) {
	d := openAccountTestDB(t)
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile")
	writeStateFile(t, statePath, "   \n", time.Now())
	d.SetProfileResolver(newProfileResolverForTest(statePath, "light"))

	id := writeTestEvent(t, d, "repo@main")

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "light" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"light\", true)", got, ok)
	}
}

// The resolver reads the state-file CONTENT at most twice across N events that
// span one profile switch: once for the initial value, once for the value
// after the switch. This mirrors the account resolver's mtime-cache guarantee.
func TestProfileResolver_ReadsAtMostTwicePerSwitch(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile")
	writeStateFile(t, statePath, "standard\n", time.Now().Add(-time.Hour))
	r := newProfileResolverForTest(statePath, "light")

	for i := 0; i < 10; i++ {
		if got := r.Name(); got != "standard" {
			t.Fatalf("before switch: Name() = %q, want standard", got)
		}
	}
	// Switch to "heavy" with a strictly newer mtime.
	writeStateFile(t, statePath, "heavy\n", time.Now())
	for i := 0; i < 10; i++ {
		if got := r.Name(); got != "heavy" {
			t.Fatalf("after switch: Name() = %q, want heavy", got)
		}
	}

	if r.reads > 2 {
		t.Errorf("resolver read the state file %d times across 20 resolutions and one switch, want at most 2", r.reads)
	}
}

// seedSessionRow inserts a sessions row and returns its instance_id.
// agent_events.instance_id and spawn_inputs.instance_id both reference it.
func seedSessionRow(t *testing.T, d *DB, session string) string {
	t.Helper()
	inst := uuid.New().String()
	if _, err := d.conn.Exec(
		`INSERT INTO sessions (instance_id, session_name, repo, worktree, harness, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		inst, session, "repo", "/code/repo/main", "pi", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert session %q: %v", session, err)
	}
	return inst
}

// seedSpawnProfile inserts the spawn_inputs row for instanceID. A nil profile
// writes SQL NULL, which is what a spawn with no --profile flag records.
func seedSpawnProfile(t *testing.T, d *DB, instanceID string, profile *string) {
	t.Helper()
	if err := d.InsertSpawnInputs(SpawnInputs{
		InstanceID:  instanceID,
		ProfileName: profile,
		CreatedAt:   time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertSpawnInputs for %s: %v", instanceID, err)
	}
}

// spawnedSession seeds a session and its spawn_inputs row in one step and
// returns the instance_id.
func spawnedSession(t *testing.T, d *DB, session string, profile *string) string {
	t.Helper()
	inst := seedSessionRow(t, d, session)
	seedSpawnProfile(t, d, inst, profile)
	return inst
}

// writeTestEventForInstance writes an event that carries an instance_id, as
// every sidecar-written event does. writeTestEvent leaves it NULL.
func writeTestEventForInstance(t *testing.T, d *DB, session, instanceID string) string {
	t.Helper()
	id := uuid.New().String()
	err := d.WriteEvent(Event{
		ID:          id,
		SessionName: session,
		Repo:        "repo",
		Worktree:    "/code/repo/main",
		InstanceID:  &instanceID,
		Type:        "msg_assistant",
		Payload:     `{"cost":0.01}`,
		CreatedAt:   time.Now(),
	})
	if err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	return id
}

// bindMachineProfile points the handle's machine-active resolution at a temp
// state file holding active, with nixDefault behind it. An empty active leaves
// the state file uncreated, so the nix default is what resolves.
func bindMachineProfile(t *testing.T, d *DB, active, nixDefault string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "prism", "active-profile")
	if active != "" {
		writeStateFile(t, statePath, active+"\n", time.Now())
	}
	d.SetProfileResolver(newProfileResolverForTest(statePath, nixDefault))
}

// expireSessionProfileMisses ages every cached miss past
// negativeSpawnProfileTTL, so the next lookup re-queries.
func expireSessionProfileMisses(d *DB) {
	d.sessionProfiles.mu.Lock()
	defer d.sessionProfiles.mu.Unlock()
	for k, e := range d.sessionProfiles.entries {
		if e.name == "" {
			e.at = e.at.Add(-2 * negativeSpawnProfileTTL)
			d.sessionProfiles.entries[k] = e
		}
	}
}

func ptr(s string) *string { return &s }

// AC [functional]: a cost event written by a spawned session records that
// session's own spawn_inputs.profile_name, on a host whose active profile is a
// different tier.
func TestWriteEvent_SpawnProfileBeatsMachineActive(t *testing.T) {
	d := openAccountTestDB(t)
	bindMachineProfile(t, d, "standard", "standard")
	inst := spawnedSession(t, d, "repo@feature", ptr("max"))

	id := writeTestEventForInstance(t, d, "repo@feature", inst)

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "max" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"max\", true)", got, ok)
	}
}

// AC [functional]: a session with no spawn_inputs row — a coordinator — keeps
// the machine-active resolution.
func TestWriteEvent_NoSpawnRow_RecordsMachineActive(t *testing.T) {
	d := openAccountTestDB(t)
	bindMachineProfile(t, d, "heavy", "standard")
	inst := seedSessionRow(t, d, "repo@main")

	id := writeTestEventForInstance(t, d, "repo@main", inst)

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "heavy" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"heavy\", true)", got, ok)
	}
}

// AC [functional]: a review agent records the profile it inherited from its
// parent worker's spawn. internal/profile.InheritFromParent copies the parent's
// tier onto the child's own spawn_inputs row, so the child reads it the same
// way any spawned session does.
func TestWriteEvent_ReviewAgentRecordsInheritedProfile(t *testing.T) {
	d := openAccountTestDB(t)
	bindMachineProfile(t, d, "standard", "standard")
	child := spawnedSession(t, d, "repo@feature~review-1-review-code", ptr("max"))

	id := writeTestEventForInstance(t, d, "repo@feature~review-1-review-code", child)

	got, ok := eventProfileName(t, d, id)
	if !ok || got != "max" {
		t.Fatalf("profile_name = (%q, valid=%v), want (\"max\", true)", got, ok)
	}
}

// AC [edge-case]: a spawn_inputs row with a NULL or blank profile_name falls
// back to the machine-active profile and never records an empty label.
func TestWriteEvent_UnusableSpawnProfile_FallsBackToMachineActive(t *testing.T) {
	cases := map[string]*string{
		"null":       nil,
		"empty":      ptr(""),
		"whitespace": ptr("  \n"),
	}
	for name, profile := range cases {
		t.Run(name, func(t *testing.T) {
			d := openAccountTestDB(t)
			bindMachineProfile(t, d, "light", "standard")
			inst := spawnedSession(t, d, "repo@feature", profile)

			id := writeTestEventForInstance(t, d, "repo@feature", inst)

			got, ok := eventProfileName(t, d, id)
			if !ok || got != "light" {
				t.Fatalf("profile_name = (%q, valid=%v), want (\"light\", true)", got, ok)
			}
		})
	}
}

// AC [edge-case]: with no spawn profile, no state file, and no nix default,
// the recorded value is the literal "unknown".
func TestWriteEvent_NoSpawnProfileNoMachineProfile_Unknown(t *testing.T) {
	d := openAccountTestDB(t)
	bindMachineProfile(t, d, "", "")
	inst := spawnedSession(t, d, "repo@feature", nil)

	id := writeTestEventForInstance(t, d, "repo@feature", inst)

	got, ok := eventProfileName(t, d, id)
	if !ok || got != unknownProfile {
		t.Fatalf("profile_name = (%q, valid=%v), want (%q, true)", got, ok, unknownProfile)
	}
}

// AC [functional]: two sessions running concurrently on one host under
// different profiles produce two distinct profile values, so the cost counter
// splits into two series.
func TestWriteEvent_ConcurrentSessionsRecordOwnProfiles(t *testing.T) {
	d := openAccountTestDB(t)
	bindMachineProfile(t, d, "standard", "standard")
	maxInst := spawnedSession(t, d, "repo@big", ptr("max"))
	lightInst := spawnedSession(t, d, "repo@small", ptr("light"))

	// Interleaved, as two sidecars sharing one host would write them.
	firstMax := writeTestEventForInstance(t, d, "repo@big", maxInst)
	firstLight := writeTestEventForInstance(t, d, "repo@small", lightInst)
	secondMax := writeTestEventForInstance(t, d, "repo@big", maxInst)

	for _, tc := range []struct{ id, want string }{
		{firstMax, "max"},
		{secondMax, "max"},
		{firstLight, "light"},
	} {
		if got, ok := eventProfileName(t, d, tc.id); !ok || got != tc.want {
			t.Errorf("profile_name = (%q, valid=%v), want (%q, true)", got, ok, tc.want)
		}
	}
}

// AC [functional]: rows written before a change are never rewritten. The stamp
// is the stamp — an event written under the machine profile keeps it after the
// session's spawn row appears.
func TestWriteEvent_EarlierRowsAreNotRewritten(t *testing.T) {
	d := openAccountTestDB(t)
	bindMachineProfile(t, d, "standard", "standard")
	inst := seedSessionRow(t, d, "repo@feature")

	earlier := writeTestEventForInstance(t, d, "repo@feature", inst)

	seedSpawnProfile(t, d, inst, ptr("max"))
	expireSessionProfileMisses(d)
	later := writeTestEventForInstance(t, d, "repo@feature", inst)

	if got, ok := eventProfileName(t, d, earlier); !ok || got != "standard" {
		t.Errorf("earlier row profile_name = (%q, valid=%v), want (\"standard\", true)", got, ok)
	}
	if got, ok := eventProfileName(t, d, later); !ok || got != "max" {
		t.Errorf("later row profile_name = (%q, valid=%v), want (\"max\", true)", got, ok)
	}
}

// AC [performance]: the event-write path issues no per-event query for the
// spawn row. N events across two sessions cost one query per session.
func TestSessionProfileCache_OneQueryPerSession(t *testing.T) {
	d := openAccountTestDB(t)
	bindMachineProfile(t, d, "standard", "standard")
	maxInst := spawnedSession(t, d, "repo@big", ptr("max"))
	coordInst := seedSessionRow(t, d, "repo@main")

	for i := 0; i < 10; i++ {
		writeTestEventForInstance(t, d, "repo@big", maxInst)
		writeTestEventForInstance(t, d, "repo@main", coordInst)
	}

	d.sessionProfiles.mu.Lock()
	queries := d.sessionProfiles.queries
	d.sessionProfiles.mu.Unlock()
	if queries != 2 {
		t.Errorf("spawn-row queries across 20 events for 2 sessions = %d, want 2", queries)
	}
}

// A miss is re-queried once its TTL expires: internal/session/spawn.go writes
// the spawn-intent event before it inserts the spawn_inputs row, so a session's
// first lookup can miss a row that lands moments later.
func TestSessionProfileCache_RefreshesMissAfterTTL(t *testing.T) {
	d := openAccountTestDB(t)
	bindMachineProfile(t, d, "standard", "standard")
	inst := seedSessionRow(t, d, "repo@feature")

	if got := d.spawnProfileName(&inst); got != "" {
		t.Fatalf("spawn profile before the row exists = %q, want empty", got)
	}
	seedSpawnProfile(t, d, inst, ptr("max"))
	if got := d.spawnProfileName(&inst); got != "" {
		t.Fatalf("spawn profile within the miss TTL = %q, want the cached empty answer", got)
	}

	expireSessionProfileMisses(d)
	if got := d.spawnProfileName(&inst); got != "max" {
		t.Fatalf("spawn profile after the miss TTL = %q, want max", got)
	}
}

// An event with no instance_id has nothing to key the spawn lookup on, so it
// takes the machine-active profile and costs no query.
func TestWriteEvent_NoInstanceID_RecordsMachineActiveWithoutQuery(t *testing.T) {
	d := openAccountTestDB(t)
	bindMachineProfile(t, d, "heavy", "standard")

	id := writeTestEvent(t, d, "repo@main")

	if got, ok := eventProfileName(t, d, id); !ok || got != "heavy" {
		t.Errorf("profile_name = (%q, valid=%v), want (\"heavy\", true)", got, ok)
	}
	d.sessionProfiles.mu.Lock()
	queries := d.sessionProfiles.queries
	d.sessionProfiles.mu.Unlock()
	if queries != 0 {
		t.Errorf("spawn-row queries for an event with no instance_id = %d, want 0", queries)
	}
}

// TestMigrateV41ToV42_PopulatedDBNoDataLoss migrates a populated v41 prism.db
// without data loss, is idempotent across two runs, and leaves pre-migration
// rows reading back as SQL NULL profile_name (the no-backfill policy).
func TestMigrateV41ToV42_PopulatedDBNoDataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prism.db")
	seedV41PopulatedDB(t, path)

	// First Open migrates v41 → v42 and adds the nullable column.
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}

	// Pre-migration rows survive and read back as SQL NULL profile_name.
	if got, ok := eventProfileName(t, d, "old-event-1"); ok {
		t.Errorf("pre-migration agent_events row profile_name = %q, want SQL NULL (no backfill)", got)
	}
	var eventCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM agent_events`).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 2 {
		t.Errorf("agent_events count after migration = %d, want 2 (no data loss)", eventCount)
	}
	d.Close()

	// Second Open is idempotent: version stays at currentSchemaVersion.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (idempotent): %v", err)
	}
	defer d2.Close()
	var version int
	if err := d2.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", version, currentSchemaVersion)
	}
	if err := d2.QueryRow(`SELECT COUNT(*) FROM agent_events`).Scan(&eventCount); err != nil {
		t.Fatalf("count events after 2nd open: %v", err)
	}
	if eventCount != 2 {
		t.Errorf("agent_events count after 2nd open = %d, want 2", eventCount)
	}
}

// seedV41PopulatedDB writes a raw SQLite database in its v41 shape
// (agent_events has account_name but no profile_name), populated with a couple
// of rows, and stamps schema_version = 41.
func seedV41PopulatedDB(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()

	stmts := []string{
		`CREATE TABLE agent_events (
			id TEXT PRIMARY KEY, session_name TEXT NOT NULL, repo TEXT NOT NULL,
			worktree TEXT NOT NULL, harness_session_id TEXT, type TEXT NOT NULL,
			payload TEXT NOT NULL, created_at INTEGER NOT NULL, instance_id TEXT,
			account_name TEXT
		)`,
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version) VALUES (41)`,
		`INSERT INTO agent_events (id, session_name, repo, worktree, type, payload, created_at)
		   VALUES ('old-event-1', 'repo@main', 'repo', '/wt', 'msg_assistant', '{}', 1),
		          ('old-event-2', 'repo@main', 'repo', '/wt', 'msg_assistant', '{}', 2)`,
	}
	for _, s := range stmts {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("seed exec: %v\nstmt: %s", err, s)
		}
	}
}
