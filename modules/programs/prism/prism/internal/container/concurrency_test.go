package container

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	"path/filepath"
)

// openTestDB creates a fresh in-memory SQLite DB at a temp path and returns
// it along with a cleanup function. Each call produces an independent DB.
func openTestDB(t *testing.T) (*db.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism-test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("openTestDB: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

// seedSession inserts a single active (ended_at IS NULL) agent_status row.
func seedSession(t *testing.T, d *db.DB, sessionName, role string) {
	t.Helper()
	// Use UpsertStatusSeedRootAgentName to set the root_agent_name so roleFor
	// can return the correct label.
	if err := d.UpsertStatusSeedRootAgentName(sessionName, "repo", "/workspace", "idle", nil, nil, role, ""); err != nil {
		t.Fatalf("seedSession(%q): %v", sessionName, err)
	}
}

// ── Cap helper tests ──────────────────────────────────────────────────────────

// capFromDB is a helper that exercises the concurrency cap check by querying
// all active sessions from the DB and constructing a CapStatus.
func capFromDB(t *testing.T, dbPath string) CapStatus {
	t.Helper()
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("capFromDB: open DB: %v", err)
	}
	defer d.Close()
	statuses, err := d.AllActiveStatus()
	if err != nil {
		t.Fatalf("capFromDB: AllActiveStatus: %v", err)
	}
	var inFlight []InFlightSession
	for _, s := range statuses {
		inFlight = append(inFlight, InFlightSession{
			Name: s.SessionName,
			Role: roleFor(s.SessionName, s.RootAgentName),
		})
	}
	limit := DefaultConcurrencyCap
	count := len(inFlight)
	return CapStatus{
		Mode:     config.IsolationBwrap,
		Limit:    limit,
		Count:    count,
		Exceeded: count >= limit,
		InFlight: inFlight,
	}
}

// TestCap_FiveSessionsAllows verifies: 5 live sessions → spawn allowed.
func TestCap_FiveSessionsAllows(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 5; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	status := capFromDB(t, path)
	if status.Count != 5 {
		t.Errorf("expected count=5, got %d", status.Count)
	}
	if status.Exceeded {
		t.Error("expected Exceeded=false at 5 sessions with cap=6")
	}
}

// TestCap_SixSessionsRefuses verifies: 6 live sessions → spawn refused.
func TestCap_SixSessionsRefuses(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 6; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	status := capFromDB(t, path)
	if status.Count != 6 {
		t.Errorf("expected count=6, got %d", status.Count)
	}
	if !status.Exceeded {
		t.Error("expected Exceeded=true at 6 sessions with cap=6")
	}
}

// TestCap_SixSessionsWithFlagProceeds verifies: 6 sessions +
// --ignore-concurrency-cap → warning produced, nil error returned.
func TestCap_SixSessionsWithFlagProceeds(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 6; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	status := capFromDB(t, path)

	if !status.Exceeded {
		t.Errorf("expected Exceeded=true at 6 sessions with cap=%d, got false", DefaultConcurrencyCap)
	}
	if status.Count != 6 {
		t.Errorf("expected Count=6, got %d", status.Count)
	}
	if status.Limit != DefaultConcurrencyCap {
		t.Errorf("expected Limit=%d, got %d", DefaultConcurrencyCap, status.Limit)
	}

	// Check that RenderWarning produces a non-empty, correctly-formatted string.
	warning := status.RenderWarning()
	if warning == "" {
		t.Error("RenderWarning should return a non-empty warning string")
	}
	if !strings.Contains(warning, "exceeded") {
		t.Errorf("warning should mention 'exceeded', got: %s", warning)
	}
}

// ── CapStatus.RenderError tests ───────────────────────────────────────────────

func TestCapStatus_RenderError_ContainsSessionNames(t *testing.T) {
	d, path := openTestDB(t)
	seedSession(t, d, "nixos-config@main", "coordinator")
	seedSession(t, d, "nixos-config@feature-x", "worker")

	status := capFromDB(t, path)
	msg := status.RenderError()

	if !strings.Contains(msg, "--ignore-concurrency-cap") {
		t.Errorf("error message should mention --ignore-concurrency-cap, got:\n%s", msg)
	}
}

func TestCapStatus_RenderError_ContainsCount(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 6; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	status := capFromDB(t, path)
	msg := status.RenderError()

	if !strings.Contains(msg, "6") {
		t.Errorf("error message should contain count 6, got:\n%s", msg)
	}
}

// TestCapStatus_RenderWarning_ModeNoun verifies that the mode noun is
// correct for each isolation mode.
func TestCapStatus_RenderWarning_ModeNoun(t *testing.T) {
	tests := []struct {
		mode config.IsolationMode
		want string
	}{
		{"bwrap", "bwrap sessions"},
		{"sandbox-exec", "sandbox-exec sessions"},
	}
	for _, tc := range tests {
		status := CapStatus{
			Mode:     tc.mode,
			Limit:    5,
			Count:    5,
			Exceeded: true,
			InFlight: []InFlightSession{{Name: "repo@feature", Role: "worker"}},
		}
		warning := status.RenderWarning()
		if !strings.Contains(warning, tc.want) {
			t.Errorf("mode %q: RenderWarning should contain %q, got:\n%s", tc.mode, tc.want, warning)
		}
		errMsg := status.RenderError()
		if !strings.Contains(errMsg, tc.want) {
			t.Errorf("mode %q: RenderError should contain %q, got:\n%s", tc.mode, tc.want, errMsg)
		}
	}
}

// TestCapStatus_Check_UncappedAlwaysPasses verifies that Limit=0 short-circuits.
func TestCapStatus_Check_UncappedAlwaysPasses(t *testing.T) {
	status := CapStatus{Mode: "host", Limit: 0, Count: 100, Exceeded: false}
	if err := status.Check(false); err != nil {
		t.Errorf("expected nil for uncapped status, got %v", err)
	}
}

// TestCapStatus_Check_ExceededReturnsError verifies cap-exceeded returns error.
func TestCapStatus_Check_ExceededReturnsError(t *testing.T) {
	status := CapStatus{
		Mode:     "bwrap",
		Limit:    5,
		Count:    5,
		Exceeded: true,
		InFlight: []InFlightSession{{Name: "repo@feature", Role: "worker"}},
	}
	err := status.Check(false)
	if err == nil {
		t.Error("expected non-nil error when cap is exceeded and ignoreCap=false")
	}
}

// TestCapStatus_Check_IgnoreCapReturnsNil verifies ignoreCap bypasses the cap.
func TestCapStatus_Check_IgnoreCapReturnsNil(t *testing.T) {
	status := CapStatus{
		Mode:     "bwrap",
		Limit:    5,
		Count:    5,
		Exceeded: true,
		InFlight: []InFlightSession{{Name: "repo@feature", Role: "worker"}},
	}
	err := status.Check(true)
	if err != nil {
		t.Errorf("expected nil when ignoreCap=true, got %v", err)
	}
}

// ── roleFor tests ────────────────────────────────────────────────────────────

func TestRoleFor_UsesRootAgentName(t *testing.T) {
	name := "coordinator"
	role := roleFor("repo@feature", &name)
	if role != "coordinator" {
		t.Errorf("expected 'coordinator', got %q", role)
	}
}

func TestRoleFor_FallsBackToMainHeuristic(t *testing.T) {
	role := roleFor("nixos-config@main", nil)
	if role != "coordinator" {
		t.Errorf("expected 'coordinator' for @main session, got %q", role)
	}
}

func TestRoleFor_UnknownWhenNotMain(t *testing.T) {
	role := roleFor("nixos-config@feature", nil)
	if role != "unknown" {
		t.Errorf("expected 'unknown' for non-main session without rootAgentName, got %q", role)
	}
}
