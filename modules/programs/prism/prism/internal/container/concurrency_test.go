package container

import (
	"path/filepath"
	"strings"
	"testing"

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
	if err := d.UpsertStatusSeedRootAgentName(sessionName, "repo", "/workspace", "idle", nil, nil, role); err != nil {
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

// ── CheckCap tests ───────────────────────────────────────────────────────────

// TestCheckCap_FiveSessionsAllows verifies the AC: 5 live sessions → spawn allowed.
func TestCheckCap_FiveSessionsAllows(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 5; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	res := CheckCap(path, DefaultConcurrencyCap, emptyPodman)
	if res.Count != 5 {
		t.Errorf("expected count=5, got %d", res.Count)
	}
	if res.Exceeded {
		t.Error("expected Exceeded=false at 5 sessions with cap=6")
	}
}

// TestCheckCap_SixSessionsRefuses verifies the AC: 6 live sessions → spawn refused.
func TestCheckCap_SixSessionsRefuses(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 6; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	res := CheckCap(path, DefaultConcurrencyCap, emptyPodman)
	if res.Count != 6 {
		t.Errorf("expected count=6, got %d", res.Count)
	}
	if !res.Exceeded {
		t.Error("expected Exceeded=true at 6 sessions with cap=6")
	}
}

// TestCheckCap_SixSessionsWithFlagProceeds verifies the AC: 6 sessions +
// --ignore-concurrency-cap → the caller gets Exceeded=true (cap is hit) but
// the warning formatter runs cleanly, and the caller may proceed.
//
// The flag decision logic lives in the command layer (cmd/concurrency.go):
// when Exceeded=true and --ignore-concurrency-cap is set, it calls
// FormatExceededWarning and returns nil (proceed). This test verifies that:
//  1. CheckCap correctly reports Exceeded=true at 6 sessions.
//  2. FormatExceededWarning produces a non-empty, correctly-formatted warning.
//  3. The warning includes in-flight session names so the caller can log them.
//
// Together these guarantee that the "ignoreCap + warning + proceed" path has
// the data it needs to behave correctly at the command layer.
func TestCheckCap_SixSessionsWithFlagProceeds(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 6; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	res := CheckCap(path, DefaultConcurrencyCap, emptyPodman)

	// 1. Cap must be exceeded so the command layer knows to inspect the flag.
	if !res.Exceeded {
		t.Errorf("expected Exceeded=true at 6 sessions with cap=%d, got false", DefaultConcurrencyCap)
	}
	if res.Count != 6 {
		t.Errorf("expected Count=6, got %d", res.Count)
	}
	if res.Cap != DefaultConcurrencyCap {
		t.Errorf("expected Cap=%d, got %d", DefaultConcurrencyCap, res.Cap)
	}

	// 2. Warning formatter must not panic and must produce a non-empty string
	// mentioning "exceeded" so the user understands the override.
	warning := FormatExceededWarning(res)
	if warning == "" {
		t.Error("FormatExceededWarning should return a non-empty warning string")
	}
	if !strings.Contains(warning, "exceeded") {
		t.Errorf("warning should mention 'exceeded', got: %s", warning)
	}

	// 3. Warning must list in-flight sessions so the caller can log them.
	if !strings.Contains(warning, "repo@branch-") {
		t.Errorf("warning should contain session names, got: %s", warning)
	}
}

// ── FormatExceededError tests ────────────────────────────────────────────────

func TestFormatExceededError_ContainsSessionNames(t *testing.T) {
	d, path := openTestDB(t)
	seedSession(t, d, "nixos-config@main", "coordinator")
	seedSession(t, d, "nixos-config@feature-x", "worker")

	res := CheckCap(path, DefaultConcurrencyCap, emptyPodman)
	msg := FormatExceededError(res)

	if !strings.Contains(msg, "nixos-config@main") {
		t.Errorf("error message should contain session name 'nixos-config@main', got:\n%s", msg)
	}
	if !strings.Contains(msg, "--ignore-concurrency-cap") {
		t.Errorf("error message should mention --ignore-concurrency-cap, got:\n%s", msg)
	}
}

func TestFormatExceededError_ContainsCount(t *testing.T) {
	d, path := openTestDB(t)
	for i := 0; i < 6; i++ {
		seedSession(t, d, "repo@branch-"+string(rune('a'+i)), "worker")
	}

	res := CheckCap(path, DefaultConcurrencyCap, emptyPodman)
	msg := FormatExceededError(res)

	if !strings.Contains(msg, "6") {
		t.Errorf("error message should contain count 6, got:\n%s", msg)
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
