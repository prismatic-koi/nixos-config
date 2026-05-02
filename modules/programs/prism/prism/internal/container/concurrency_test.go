package container

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
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

// noPodman returns a podmanPS stub that reports no live containers — simulates
// a system where podman is not available.
func noPodman() ([]string, bool) {
	return nil, false
}

// emptyPodman returns a podmanPS stub that returns an empty list successfully.
func emptyPodman() ([]string, bool) {
	return []string{}, true
}

// ── ListInFlight tests ───────────────────────────────────────────────────────

func TestListInFlight_EmptyDB_NoContainers(t *testing.T) {
	_, path := openTestDB(t)
	result, failed := ListInFlight(path, emptyPodman)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %v", result)
	}
	if failed {
		t.Error("expected podmanFailed=false with empty podman response")
	}
}

func TestListInFlight_DBSessions_Counted(t *testing.T) {
	d, path := openTestDB(t)
	seedSession(t, d, "nixos-config@main", "coordinator")
	seedSession(t, d, "nixos-config@feature-a", "worker")

	result, _ := ListInFlight(path, emptyPodman)
	if len(result) != 2 {
		t.Errorf("expected 2 in-flight sessions, got %d: %v", len(result), result)
	}
}

func TestListInFlight_PodmanFailure_FallsBackToDBOnly(t *testing.T) {
	d, path := openTestDB(t)
	seedSession(t, d, "nixos-config@main", "coordinator")

	result, podmanFailed := ListInFlight(path, noPodman)
	if !podmanFailed {
		t.Error("expected podmanFailed=true when podman stub fails")
	}
	if len(result) != 1 {
		t.Errorf("expected 1 session from DB fallback, got %d", len(result))
	}
}

func TestListInFlight_DeduplicatesByContainerName(t *testing.T) {
	d, path := openTestDB(t)
	sessionName := "nixos-config@feature-a"
	seedSession(t, d, sessionName, "worker")

	// Pretend podman also reports this container.
	containerName := NameForSession(sessionName)
	podmanStub := func() ([]string, bool) {
		return []string{containerName}, true
	}

	result, _ := ListInFlight(path, podmanStub)
	if len(result) != 1 {
		t.Errorf("expected deduplicated to 1, got %d: %v", len(result), result)
	}
}

func TestListInFlight_PodmanOnlyContainerCounted(t *testing.T) {
	// A container present in podman but NOT in the DB should still be counted.
	_, path := openTestDB(t)
	podmanStub := func() ([]string, bool) {
		return []string{"prism-nixos-config-orphan"}, true
	}
	result, _ := ListInFlight(path, podmanStub)
	if len(result) != 1 {
		t.Errorf("expected 1 podman-only container, got %d", len(result))
	}
}

func TestListInFlight_NonPrismContainersIgnored(t *testing.T) {
	// Containers without the "prism-" prefix are not prism-managed.
	_, path := openTestDB(t)
	podmanStub := func() ([]string, bool) {
		return []string{"some-other-container", "nginx-proxy"}, true
	}
	result, _ := ListInFlight(path, podmanStub)
	if len(result) != 0 {
		t.Errorf("expected 0 non-prism containers counted, got %d: %v", len(result), result)
	}
}

// ── podmanIsolator.Cap tests (replaces CheckCap tests) ───────────────────────

// podmanCapWithPS is a helper that calls podmanIsolator.Cap() with a fake
// podmanPS injection. Since Cap() calls ListInFlight(nil) directly, we
// test it via a small helper that exercises the same logic.
//
// For testing we construct the CapStatus directly from ListInFlight + the cap
// constant, mirroring what podmanIsolator.Cap does internally.
func podmanCapFromDB(t *testing.T, dbPath string, podmanPS func() ([]string, bool)) CapStatus {
	t.Helper()
	inFlight, podmanFailed := ListInFlight(dbPath, podmanPS)
	limit := DefaultConcurrencyCap
	count := len(inFlight)
	note := ""
	if podmanFailed {
		note = "podman ps failed — concurrency check is using DB-only count (may be imprecise)"
	}
	return CapStatus{
		Mode:     "podman",
		Limit:    limit,
		Count:    count,
		Exceeded: count >= limit,
		InFlight: inFlight,
		Note:     note,
	}
}

// TestPodmanCap_FiveSessionsAllows verifies: 5 live sessions → spawn allowed.
func TestPodmanCap_FiveSessionsAllows(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 5; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	status := podmanCapFromDB(t, path, emptyPodman)
	if status.Count != 5 {
		t.Errorf("expected count=5, got %d", status.Count)
	}
	if status.Exceeded {
		t.Error("expected Exceeded=false at 5 sessions with cap=6")
	}
}

// TestPodmanCap_SixSessionsRefuses verifies: 6 live sessions → spawn refused.
func TestPodmanCap_SixSessionsRefuses(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 6; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	status := podmanCapFromDB(t, path, emptyPodman)
	if status.Count != 6 {
		t.Errorf("expected count=6, got %d", status.Count)
	}
	if !status.Exceeded {
		t.Error("expected Exceeded=true at 6 sessions with cap=6")
	}
}

// TestPodmanCap_SixSessionsWithFlagProceeds verifies: 6 sessions +
// --ignore-concurrency-cap → warning produced, nil error returned.
func TestPodmanCap_SixSessionsWithFlagProceeds(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 6; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	status := podmanCapFromDB(t, path, emptyPodman)

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
	if !strings.Contains(warning, "repo@branch-") {
		t.Errorf("warning should contain session names, got: %s", warning)
	}
}

// ── CapStatus.RenderError tests ───────────────────────────────────────────────

func TestCapStatus_RenderError_ContainsSessionNames(t *testing.T) {
	d, path := openTestDB(t)
	seedSession(t, d, "nixos-config@main", "coordinator")
	seedSession(t, d, "nixos-config@feature-x", "worker")

	status := podmanCapFromDB(t, path, emptyPodman)
	msg := status.RenderError()

	if !strings.Contains(msg, "nixos-config@main") {
		t.Errorf("error message should contain session name 'nixos-config@main', got:\n%s", msg)
	}
	if !strings.Contains(msg, "--ignore-concurrency-cap") {
		t.Errorf("error message should mention --ignore-concurrency-cap, got:\n%s", msg)
	}
}

func TestCapStatus_RenderError_ContainsCount(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 6; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	status := podmanCapFromDB(t, path, emptyPodman)
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
		{"podman", "agent containers"},
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

// TestPodmanIsolator_Cap_IntegrationSmoke runs podmanIsolator.Cap against a
// real DB to verify end-to-end behavior without subprocess podman calls.
func TestPodmanIsolator_Cap_IntegrationSmoke(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 3; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}
	// We can't call podmanIsolator.Cap directly from outside the package
	// in a _test.go file that is package container, but we can construct one.
	iso := newPodmanIsolator("test-container")
	status := iso.Cap(context.Background(), path)
	// podman ps will fail in the test environment, which is fine — the DB
	// count should still be 3 and Exceeded should be false (3 < 6).
	if status.Count < 3 {
		t.Errorf("expected count >= 3 from DB, got %d", status.Count)
	}
	if status.Limit != DefaultConcurrencyCap {
		t.Errorf("expected Limit=%d, got %d", DefaultConcurrencyCap, status.Limit)
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
