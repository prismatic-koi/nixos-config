package session

import (
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

func TestIsMetaSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		// Known meta-sessions — must return true.
		{"scratchpad", true},
		{"prism-dashboard", true},

		// Non-meta sessions — must return false.
		{"nixos-config@main", false},
		{"nixos-config@feature-branch", false},
		{"worker", false},
		{"coordinator", false},
		{"obsidian", false},
		{"", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsMetaSession(tt.name)
			if got != tt.want {
				t.Errorf("IsMetaSession(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// openTestDB opens a fresh temp SQLite DB for session tests and registers cleanup.
func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// ── IsCoordinatorSession tests ───────────────────────────────────────────────

// TestIsCoordinatorSession_DBBackedCoordinator verifies the primary DB-backed
// path: a session with root_agent_name == "coordinator" returns true.
func TestIsCoordinatorSession_DBBackedCoordinator(t *testing.T) {
	d := openTestDB(t)
	const sess = "nixos-config@main"
	if err := d.UpsertStatusSeedRootAgentName(sess, "nixos-config", "/worktree/main", "idle", nil, nil, "coordinator"); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if !IsCoordinatorSession(sess, d) {
		t.Errorf("IsCoordinatorSession(%q) = false, want true (coordinator in DB)", sess)
	}
}

// TestIsCoordinatorSession_DBBackedWorker verifies that a session with
// root_agent_name == "worker" returns false when it does NOT end with @main.
// NOTE: if the session name ends with @main, the heuristic wins and returns
// true (see TestIsCoordinatorSession_StaleRootAgentName_MainHeuristicWins).
func TestIsCoordinatorSession_DBBackedWorker(t *testing.T) {
	d := openTestDB(t)
	// A worker session on a non-@main branch — DB value must cause false.
	const sess = "nixos-config@feature-worker"
	if err := d.UpsertStatusSeedRootAgentName(sess, "nixos-config", "/worktree/feature-worker", "idle", nil, nil, "worker"); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if IsCoordinatorSession(sess, d) {
		t.Errorf("IsCoordinatorSession(%q) = true, want false (DB says worker, non-@main)", sess)
	}
}

// TestIsCoordinatorSession_DBBackedWorkerBranch verifies a normal worker
// session (non-@main branch) with root_agent_name == "worker" returns false.
func TestIsCoordinatorSession_DBBackedWorkerBranch(t *testing.T) {
	d := openTestDB(t)
	const sess = "nixos-config@feature-branch"
	if err := d.UpsertStatusSeedRootAgentName(sess, "nixos-config", "/worktree/feature-branch", "idle", nil, nil, "worker"); err != nil {
		t.Fatalf("UpsertStatusSeedRootAgentName: %v", err)
	}
	if IsCoordinatorSession(sess, d) {
		t.Errorf("IsCoordinatorSession(%q) = true, want false (worker on feature branch)", sess)
	}
}

// TestIsCoordinatorSession_NullRootAgentName_FallsBackToHeuristic verifies
// that a pre-migration row with NULL root_agent_name falls back to the
// name-suffix heuristic rather than returning a hard error.
// A @main-suffixed session should return true; a non-@main session false.
func TestIsCoordinatorSession_NullRootAgentName_FallsBackToHeuristic(t *testing.T) {
	d := openTestDB(t)
	// UpsertStatus leaves root_agent_name NULL (pre-migration path).
	const coordSess = "nixos-config@main"
	const workerSess = "nixos-config@feature-branch"
	if err := d.UpsertStatus(coordSess, "nixos-config", "/worktree/main", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (coordinator): %v", err)
	}
	if err := d.UpsertStatus(workerSess, "nixos-config", "/worktree/feature", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus (worker): %v", err)
	}

	if !IsCoordinatorSession(coordSess, d) {
		t.Errorf("IsCoordinatorSession(%q) = false, want true (NULL root_agent_name, @main heuristic)", coordSess)
	}
	if IsCoordinatorSession(workerSess, d) {
		t.Errorf("IsCoordinatorSession(%q) = true, want false (NULL root_agent_name, non-@main)", workerSess)
	}
}

// TestIsCoordinatorSession_NoRow_FallsBackToHeuristic verifies that when there
// is no DB row at all, the name-suffix heuristic is used.
func TestIsCoordinatorSession_NoRow_FallsBackToHeuristic(t *testing.T) {
	d := openTestDB(t) // empty DB — no rows seeded

	if !IsCoordinatorSession("nixos-config@main", d) {
		t.Error("IsCoordinatorSession(nixos-config@main, emptyDB) = false, want true (no row → heuristic)")
	}
	if IsCoordinatorSession("nixos-config@feature", d) {
		t.Error("IsCoordinatorSession(nixos-config@feature, emptyDB) = true, want false (no row → heuristic)")
	}
}

// TestIsCoordinatorSession_NilDB_FallsBackToHeuristic verifies that when d is
// nil (DB unavailable), the name-suffix heuristic is used unconditionally.
func TestIsCoordinatorSession_NilDB_FallsBackToHeuristic(t *testing.T) {
	if !IsCoordinatorSession("nixos-config@main", nil) {
		t.Error("IsCoordinatorSession(nixos-config@main, nil) = false, want true (nil DB → heuristic)")
	}
	if IsCoordinatorSession("nixos-config@feature", nil) {
		t.Error("IsCoordinatorSession(nixos-config@feature, nil) = true, want false (nil DB → heuristic)")
	}
	// Edge case: empty session name should not be considered coordinator.
	if IsCoordinatorSession("", nil) {
		t.Error("IsCoordinatorSession(\"\", nil) = true, want false")
	}
}

// TestIsCoordinatorSession_StaleRootAgentName_MainHeuristicWins verifies the
// fix for the bug where a coordinator session with root_agent_name="worker"
// (stale/incorrect DB value) would incorrectly return false, allowing
// self-notification to proceed.
func TestIsCoordinatorSession_StaleRootAgentName_MainHeuristicWins(t *testing.T) {
	d := openTestDB(t)

	sess := "nixos-config@main"
	// Seed the session with a stale root_agent_name of "worker".
	if err := d.UpsertStatusSeedRootAgentName(sess, "nixos-config", "/code/main", "active", nil, nil, "worker"); err != nil {
		t.Fatalf("seed stale worker: %v", err)
	}

	// Despite DB saying "worker", @main heuristic must win → true.
	if !IsCoordinatorSession(sess, d) {
		t.Errorf("IsCoordinatorSession(%q) = false, want true (@main heuristic must win over stale DB value)", sess)
	}
}

// TestIsCoordinatorSession_StaleRootAgentName_NonMain verifies that the fix
// does not accidentally promote a genuine worker session with a non-@main name.
func TestIsCoordinatorSession_StaleRootAgentName_NonMain(t *testing.T) {
	d := openTestDB(t)

	sess := "nixos-config@feature"
	if err := d.UpsertStatusSeedRootAgentName(sess, "nixos-config", "/code/feature", "active", nil, nil, "worker"); err != nil {
		t.Fatalf("seed worker: %v", err)
	}

	// DB says worker, name does not end in @main → must return false.
	if IsCoordinatorSession(sess, d) {
		t.Errorf("IsCoordinatorSession(%q) = true, want false (non-@main worker must not be promoted)", sess)
	}
}
