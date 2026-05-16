package main

// main_reservation_test.go — unit tests for daemonState.reserveSupervisorName.
//
// The reservation slot exists because the daemon's spawn path is
// non-atomic: GenerateSessionName / addSupervisor are separated by
// SpawnSession which can take seconds. Two concurrent spawns targeting
// the same session name would, without reservation, both pass the
// "is name free?" check and the second would silently overwrite the
// supervisor-map entry of the first — the exact failure mode reported
// in issue #1738.
//
// These tests pin down the contract:
//
//   - reserveSupervisorName returns true exactly once for a given name
//     while either a reservation OR a live supervisor for that name exists.
//   - addSupervisor consumes the reservation (so the same name is then
//     blocked by the live entry, not the placeholder).
//   - removeSupervisor releases both the live entry and the reservation
//     so the name can be retried after a failed spawn.

import (
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/iris"
)

func newTestDaemonState() *daemonState {
	return &daemonState{
		supervisors:   make(map[string]*iris.Supervisor),
		reservedNames: make(map[string]struct{}),
	}
}

// TestReserveSupervisorName_RejectsDuplicateReservation is the core
// race-free check the spawn path relies on: a second concurrent spawn
// for the same name (where neither has called addSupervisor yet) must
// fail the reservation check rather than silently passing.
func TestReserveSupervisorName_RejectsDuplicateReservation(t *testing.T) {
	ds := newTestDaemonState()
	if !ds.reserveSupervisorName("repo/main") {
		t.Fatal("first reservation: expected true, got false")
	}
	if ds.reserveSupervisorName("repo/main") {
		t.Error("second reservation for same name: expected false, got true")
	}
	// A different name is unaffected.
	if !ds.reserveSupervisorName("repo/other") {
		t.Error("reservation for different name: expected true, got false")
	}
}

// TestReserveSupervisorName_RejectsLiveSupervisor verifies that once a
// supervisor is registered for a name, reserveSupervisorName for that
// name returns false — i.e. the live entry blocks new reservations the
// same way an in-flight one does.
func TestReserveSupervisorName_RejectsLiveSupervisor(t *testing.T) {
	ds := newTestDaemonState()
	// Live supervisor is registered directly (skipping the reservation
	// step) — equivalent to the test having seeded a fake live session.
	ds.supervisors["repo/main"] = nil
	if ds.reserveSupervisorName("repo/main") {
		t.Error("reservation for already-live name: expected false, got true")
	}
}

// TestAddSupervisor_ClearsReservation asserts the lifecycle: a successful
// spawn (reserve → addSupervisor) drops the placeholder. Subsequent
// reservation requests for the same name are blocked by the live entry,
// not by a stale reservation row.
func TestAddSupervisor_ClearsReservation(t *testing.T) {
	ds := newTestDaemonState()
	if !ds.reserveSupervisorName("repo/main") {
		t.Fatal("reserve: expected true")
	}
	// addSupervisor with a nil supervisor — we don't need a real one
	// for this test; the map mechanics are what we're asserting.
	ds.addSupervisor("repo/main", nil)
	if _, stillReserved := ds.reservedNames["repo/main"]; stillReserved {
		t.Error("addSupervisor did not drop the reservation row")
	}
	if _, live := ds.supervisors["repo/main"]; !live {
		t.Error("addSupervisor did not register the live entry")
	}
}

// TestRemoveSupervisor_ReleasesReservation asserts the failure-rollback
// path: when SpawnSession returns an error, spawnFn calls
// removeSupervisor to free the name so the user can retry.
func TestRemoveSupervisor_ReleasesReservation(t *testing.T) {
	ds := newTestDaemonState()
	if !ds.reserveSupervisorName("repo/main") {
		t.Fatal("reserve: expected true")
	}
	// Spawn failed before addSupervisor — only the reservation exists.
	ds.removeSupervisor("repo/main")
	if _, stillReserved := ds.reservedNames["repo/main"]; stillReserved {
		t.Error("removeSupervisor did not drop the reservation row")
	}
	// And the name is reservable again.
	if !ds.reserveSupervisorName("repo/main") {
		t.Error("name still blocked after removeSupervisor; expected to be free")
	}
}

// TestReserveSupervisorName_Concurrent stresses the lock: with N
// goroutines all trying to reserve the same name, exactly one must
// succeed. This is the property that makes the daemon spawn path
// race-free for issue #1738.
func TestReserveSupervisorName_Concurrent(t *testing.T) {
	ds := newTestDaemonState()
	const N = 64
	var wg sync.WaitGroup
	wg.Add(N)
	var mu sync.Mutex
	successCount := 0
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if ds.reserveSupervisorName("repo/main") {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successCount != 1 {
		t.Errorf("expected exactly 1 successful reservation under contention, got %d", successCount)
	}
}
