package db_test

// Regression tests for the harness_port double-allocation race.
//
// Without the own-row exclusion, allocatePortOnce's used-port query would not
// exclude the requesting session's own agent_status row, so a second
// AllocatePort call for the same session (the sidecar's startup allocation, or
// a sidecar restart) would pick a DIFFERENT port. For sandbox-exec pi sessions
// that drift is fatal: `prism agent-run` does a one-shot read of harness_port
// and bakes PRISM_HARNESS_PIPE into PI's immutable env, so a later overwrite
// leaves PI pointed at a port nobody binds.
//
// AllocatePort is idempotent per session: the session's own row
// is excluded from the used-port set AND the previously-recorded port is
// preferred over lower free ports, so repeated calls return the same port
// while it stays free.

import (
	"fmt"
	"net"
	"testing"
)

// TestAllocatePort_Idempotent_SameSessionReacquiresPort verifies that a
// repeated AllocatePort call for the same session returns the session's
// already-recorded port instead of drifting to a new one:
// "a sidecar restart for a live session re-acquires the session's previous
// port when that port is otherwise free").
func TestAllocatePort_Idempotent_SameSessionReacquiresPort(t *testing.T) {
	d := openTestDB(t)

	const session = "prism-test@2357-idempotent"
	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/x", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	p1, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort (first): %v", err)
	}

	// Second allocation for the same session. Without the own-row exclusion
	// this returns a different port because the session's own row is counted
	// as "in use".
	p2, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort (second): %v", err)
	}
	if p2 != p1 {
		t.Errorf("repeated AllocatePort drifted: first %d, second %d — want same port", p1, p2)
	}

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.HarnessPort == nil || *st.HarnessPort != p1 {
		t.Errorf("harness_port after repeated allocation: got %v, want %d", st.HarnessPort, p1)
	}
}

// TestAllocatePort_Sticky_PrefersRecordedPortOverLowerFreePort verifies that
// re-allocation prefers the session's previously-recorded port even when a
// LOWER port has been freed in the meantime. Plain self-exclusion without
// stickiness would return the lowest free port, changing the value that
// agent-run may already have baked into PI's env.
func TestAllocatePort_Sticky_PrefersRecordedPortOverLowerFreePort(t *testing.T) {
	d := openTestDB(t)

	for _, s := range []string{"prism-test@2357-low", "prism-test@2357-high"} {
		if err := d.UpsertStatus(s, "prism-test", "/code/prism-test/"+s, "idle", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %s: %v", s, err)
		}
	}

	pLow, err := d.AllocatePort("prism-test@2357-low")
	if err != nil {
		t.Fatalf("AllocatePort low: %v", err)
	}
	pHigh, err := d.AllocatePort("prism-test@2357-high")
	if err != nil {
		t.Fatalf("AllocatePort high: %v", err)
	}
	if pLow == pHigh {
		t.Fatalf("setup broken: both sessions got port %d", pLow)
	}

	// End the low session so its (lower) port re-enters the pool.
	if err := d.SetEnded("prism-test@2357-low"); err != nil {
		t.Fatalf("SetEnded low: %v", err)
	}

	// Re-allocation for the high session must keep its own port, not grab
	// the newly-freed lower one.
	got, err := d.AllocatePort("prism-test@2357-high")
	if err != nil {
		t.Fatalf("AllocatePort high (again): %v", err)
	}
	if got != pHigh {
		t.Errorf("re-allocation moved the port: got %d, want previously-recorded %d (freed lower port was %d)", got, pHigh, pLow)
	}
}

// TestAllocatePort_RecordedPortHeldByOS_AllocatesDifferentPort verifies that
// the OS-level availability probe still applies to the sticky own-port
// candidate: when a non-prism OS process holds the session's recorded port,
// re-allocation must skip it and move to a free port (edge-case: existing
// OS-level probe behaviour preserved).
func TestAllocatePort_RecordedPortHeldByOS_AllocatesDifferentPort(t *testing.T) {
	d := openTestDB(t)

	const session = "prism-test@2357-osheld"
	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/osheld", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	p1, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort (first): %v", err)
	}

	// Simulate a non-prism OS process squatting on the recorded port.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p1))
	if err != nil {
		t.Fatalf("listen on %d: %v", p1, err)
	}
	defer ln.Close()

	got, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort (while OS holds %d): %v", p1, err)
	}
	if got == p1 {
		t.Errorf("AllocatePort returned OS-held port %d — the availability probe must skip it", p1)
	}

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st.HarnessPort == nil || *st.HarnessPort != got {
		t.Errorf("harness_port: got %v, want %d", st.HarnessPort, got)
	}
}
