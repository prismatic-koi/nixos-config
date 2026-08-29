package db_test

// Evidence that closes the leading port-recycling hypothesis.
//
// The hypothesis: the harness port allocator can hand out a port that is still
// held, and a review agent that draws a recently released port then fails its
// readiness gate.
//
// These tests measure the two ways a port can be "still held" and pin the
// result, so the hypothesis is closed by measurement rather than by argument:
//
//  1. A live listener holds the port. AllocatePort must skip it — the probe in
//     portAvailable binds 127.0.0.1:<port>, which is the exact address the
//     sidecar binds later (internal/sidecar/sidecar.go), so the probe and the
//     real bind agree.
//
//  2. A socket for the port sits in TIME_WAIT. Go's net.Listen sets
//     SO_REUSEADDR, so TIME_WAIT blocks neither the probe nor the later bind.
//     A recycled port is therefore usable, and cannot fail a readiness gate.
//
// The allocation range (14000–14999) also sits below the Linux ephemeral
// range, so an outbound connection cannot take a prism port from under the
// probe between the probe and the sidecar's bind.

import (
	"fmt"
	"net"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
)

// TestAllocatePort_RangeIsBelowEphemeralRange pins the constant relationship
// that keeps the probe-to-bind window safe: no kernel-chosen local port can
// land inside the prism range. Linux's lowest default ephemeral port is 32768.
func TestAllocatePort_RangeIsBelowEphemeralRange(t *testing.T) {
	const lowestDefaultEphemeralPort = 32768
	if db.PortRangeEnd >= lowestDefaultEphemeralPort {
		t.Errorf("PortRangeEnd = %d overlaps the default ephemeral range (from %d): "+
			"an outbound connection could take an allocated port between the probe and the sidecar bind",
			db.PortRangeEnd, lowestDefaultEphemeralPort)
	}
	if db.PortRangeStart > db.PortRangeEnd {
		t.Errorf("PortRangeStart %d > PortRangeEnd %d", db.PortRangeStart, db.PortRangeEnd)
	}
}

// TestAllocatePort_LiveListenerOnRecycledPort_IsNotHandedOut is hypothesis (1).
// A session releases its port, a listener keeps holding it, and a second
// session allocates. The second session must not receive the held port.
func TestAllocatePort_LiveListenerOnRecycledPort_IsNotHandedOut(t *testing.T) {
	d := openTestDB(t)

	const holder = "prism-test@2613-holder"
	const claimant = "prism-test@2613-claimant"
	for _, s := range []string{holder, claimant} {
		if err := d.UpsertStatus(s, "prism-test", "/code/prism-test/"+s, "idle", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%s): %v", s, err)
		}
	}

	port, err := d.AllocatePort(holder)
	if err != nil {
		t.Fatalf("AllocatePort(holder): %v", err)
	}

	// The holder's process keeps the socket open while the DB row releases
	// the port — exactly the "released shortly before by a torn-down
	// session" shape the hypothesis describes.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen on %d: %v", port, err)
	}
	defer ln.Close()
	if err := d.ReleasePort(holder); err != nil {
		t.Fatalf("ReleasePort(holder): %v", err)
	}

	got, err := d.AllocatePort(claimant)
	if err != nil {
		t.Fatalf("AllocatePort(claimant): %v", err)
	}
	if got == port {
		t.Fatalf("AllocatePort handed out port %d while a listener still holds it", port)
	}

	// The allocated port must be bindable by the address the sidecar uses.
	check, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", got))
	if err != nil {
		t.Fatalf("allocated port %d is not bindable: %v", got, err)
	}
	check.Close()
}

// TestAllocatePort_RecycledPortInTimeWait_StaysUsable is hypothesis (2), and
// the one named first. It drives a real connection on an allocated port,
// closes it from the listening side so the local end enters TIME_WAIT, then
// asserts that the allocator still offers the port AND that the address the
// sidecar binds is still bindable.
//
// If this test ever fails, port recycling IS able to break a later bind and
// the allocator needs a TIME_WAIT-aware probe.
func TestAllocatePort_RecycledPortInTimeWait_StaysUsable(t *testing.T) {
	d := openTestDB(t)

	const session = "prism-test@2613-timewait"
	if err := d.UpsertStatus(session, "prism-test", "/code/prism-test/tw", "idle", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	port, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}

	client, err := net.Dial("tcp", addr)
	if err != nil {
		ln.Close()
		t.Fatalf("dial %s: %v", addr, err)
	}
	server, err := ln.Accept()
	if err != nil {
		client.Close()
		ln.Close()
		t.Fatalf("accept on %s: %v", addr, err)
	}
	// Close the SERVER end first. The active closer holds TIME_WAIT, so the
	// local (port) side of the connection is the one that lingers.
	server.Close()
	client.Close()
	ln.Close()

	// The session re-allocates, as a sidecar restart would. Stickiness means
	// it asks for the same port back; the probe decides whether it gets it.
	got, err := d.AllocatePort(session)
	if err != nil {
		t.Fatalf("AllocatePort after TIME_WAIT: %v", err)
	}
	if got != port {
		t.Errorf("AllocatePort refused the recycled port: got %d, want %d — "+
			"a TIME_WAIT socket must not make a port look busy", got, port)
	}

	// The decisive assertion: the sidecar's own bind still succeeds. This is
	// what a readiness gate would have depended on.
	relisten, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("re-bind of recycled port %s failed: %v — port recycling CAN break the sidecar bind", addr, err)
	}
	relisten.Close()
}
