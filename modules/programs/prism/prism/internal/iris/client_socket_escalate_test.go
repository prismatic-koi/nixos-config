package iris_test

// client_socket_escalate_test.go — tests for the daemon's
// handleEscalationDeliver entry point (issue #1693). Each test stands up an
// in-process ClientSocket on a tempdir socket, dials it, and sends an
// escalation_deliver frame. The escalateSession / resumeSession / deliverPrompt
// hooks are stubs the test inspects to assert correctness.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/iris"
)

// escalateTestFixture wires a ClientSocket with the four hooks
// handleEscalationDeliver exercises (escalate, resume, deliverPrompt, roleOf)
// and a fixed in-memory sessions list returned by GetActiveSessions.
type escalateTestFixture struct {
	t        *testing.T
	sockPath string

	mu             sync.Mutex
	sessions       []iris.SessionSnapshot
	escalateCalls  []string
	resumeCalls    []string
	deliverCalls   []deliverInvocation
	roleByName     map[string]string
	escalateErr    error
	deliverErr     error
	deliverCounter int32
}

type deliverInvocation struct {
	Name      string
	Text      string
	DeliverAs string
}

func newEscalateFixture(t *testing.T, sessions []iris.SessionSnapshot, roles map[string]string) *escalateTestFixture {
	t.Helper()

	shortPrefix, err := os.MkdirTemp("", "iris-esc-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortPrefix) })

	dbPath := filepath.Join(shortPrefix, "iris.db")
	database, err := iris.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	fx := &escalateTestFixture{
		t:          t,
		sockPath:   filepath.Join(shortPrefix, "iris.sock"),
		sessions:   sessions,
		roleByName: roles,
	}

	cs := iris.NewClientSocket(iris.ClientSocketConfig{
		SockPath: fx.sockPath,
		Database: database,
		GetActiveSessions: func() []iris.SessionSnapshot {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			out := make([]iris.SessionSnapshot, len(fx.sessions))
			copy(out, fx.sessions)
			return out
		},
		DeliverPrompt: func(_ context.Context, name, text, deliverAs string, _ []string) error {
			fx.mu.Lock()
			fx.deliverCalls = append(fx.deliverCalls, deliverInvocation{Name: name, Text: text, DeliverAs: deliverAs})
			err := fx.deliverErr
			fx.mu.Unlock()
			atomic.AddInt32(&fx.deliverCounter, 1)
			return err
		},
		EscalateSession: func(name string) error {
			fx.mu.Lock()
			fx.escalateCalls = append(fx.escalateCalls, name)
			err := fx.escalateErr
			fx.mu.Unlock()
			return err
		},
		ResumeSession: func(name string) {
			fx.mu.Lock()
			fx.resumeCalls = append(fx.resumeCalls, name)
			fx.mu.Unlock()
		},
		RoleOf: func(name string) string {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			return fx.roleByName[name]
		},
	})

	if err := cs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(cs.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cs.Serve(ctx)

	return fx
}

func (fx *escalateTestFixture) dial() (net.Conn, *bufio.Reader) {
	fx.t.Helper()
	var conn net.Conn
	var err error
	for i := 0; i < 30; i++ {
		conn, err = net.DialTimeout("unix", fx.sockPath, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		fx.t.Fatalf("dial %q: %v", fx.sockPath, err)
	}
	fx.t.Cleanup(func() { _ = conn.Close() })
	return conn, bufio.NewReaderSize(conn, 1<<20)
}

func (fx *escalateTestFixture) sendEscalate(conn net.Conn, from, to, prompt string) {
	fx.t.Helper()
	frame := iris.ClientEscalationDeliverFrame{
		Type:   iris.ClientFrameEscalationDeliver,
		From:   from,
		To:     to,
		Prompt: prompt,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		fx.t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		fx.t.Fatalf("write escalation_deliver: %v", err)
	}
}

func (fx *escalateTestFixture) readFrame(conn net.Conn, r *bufio.Reader) map[string]any {
	fx.t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetReadDeadline(time.Time{}) //nolint:errcheck
	line, err := r.ReadBytes('\n')
	if err != nil {
		fx.t.Fatalf("read frame: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		fx.t.Fatalf("parse frame %q: %v", line, err)
	}
	return m
}

// TestEscalate_AutoDiscover_SingleCoordinator covers the happy path: one
// worker + one coordinator in the in-memory list, no --to. The daemon
// auto-discovers the coordinator, transitions the worker, and delivers.
func TestEscalate_AutoDiscover_SingleCoordinator(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat":  "worker",
		"iris-coord@main":   "coordinator",
	}
	fx := newEscalateFixture(t, sessions, roles)

	conn, r := fx.dial()
	fx.sendEscalate(conn, "iris-worker@feat", "", "should I merge PR 1234?")

	frame := fx.readFrame(conn, r)
	if frame["type"] != iris.DaemonFrameEscalationDelivered {
		t.Fatalf("expected escalation_delivered, got %q (full=%+v)", frame["type"], frame)
	}
	if frame["from"] != "iris-worker@feat" {
		t.Errorf("from = %v, want iris-worker@feat", frame["from"])
	}
	if frame["to"] != "iris-coord@main" {
		t.Errorf("to = %v, want iris-coord@main", frame["to"])
	}
	if d, ok := frame["delivered"].(bool); !ok || !d {
		t.Errorf("delivered = %v, want true", frame["delivered"])
	}
	if id, _ := frame["delivery_id"].(string); id == "" {
		t.Errorf("delivery_id missing in ack")
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.escalateCalls) != 1 || fx.escalateCalls[0] != "iris-worker@feat" {
		t.Errorf("escalateCalls = %+v", fx.escalateCalls)
	}
	if len(fx.deliverCalls) != 1 {
		t.Fatalf("deliverCalls = %+v (want 1)", fx.deliverCalls)
	}
	if fx.deliverCalls[0].Name != "iris-coord@main" {
		t.Errorf("delivered to %q, want iris-coord@main", fx.deliverCalls[0].Name)
	}
	if fx.deliverCalls[0].Text != "should I merge PR 1234?" {
		t.Errorf("delivered text = %q", fx.deliverCalls[0].Text)
	}
}

// TestEscalate_AutoDiscover_ZeroCoordinators covers the zero-coordinator
// branch: no coordinator in the in-memory list. The worker still transitions
// to escalated; the daemon emits an ack with delivered=false; deliverPrompt
// is NEVER invoked.
func TestEscalate_AutoDiscover_ZeroCoordinators(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@solo", State: "active", Role: "worker"},
	}
	roles := map[string]string{"iris-worker@solo": "worker"}
	fx := newEscalateFixture(t, sessions, roles)

	conn, r := fx.dial()
	fx.sendEscalate(conn, "iris-worker@solo", "", "lone wolf")

	frame := fx.readFrame(conn, r)
	if frame["type"] != iris.DaemonFrameEscalationDelivered {
		t.Fatalf("expected escalation_delivered, got %q (full=%+v)", frame["type"], frame)
	}
	if d, ok := frame["delivered"].(bool); !ok || d {
		t.Errorf("delivered = %v, want false", frame["delivered"])
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.escalateCalls) != 1 {
		t.Errorf("expected exactly one escalate call, got %+v", fx.escalateCalls)
	}
	if len(fx.deliverCalls) != 0 {
		t.Errorf("expected zero deliver calls, got %+v", fx.deliverCalls)
	}
}

// TestEscalate_AutoDiscover_MultipleCoordinators covers the
// multiple-candidates branch: two coordinators in the list, no --to. The
// daemon REJECTS with an error frame; the worker stays in active state (no
// escalate call, no deliver call).
func TestEscalate_AutoDiscover_MultipleCoordinators(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
		{Name: "iris-coord@alt", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat": "worker",
		"iris-coord@main":  "coordinator",
		"iris-coord@alt":   "coordinator",
	}
	fx := newEscalateFixture(t, sessions, roles)

	conn, r := fx.dial()
	fx.sendEscalate(conn, "iris-worker@feat", "", "ambiguous")

	frame := fx.readFrame(conn, r)
	if frame["type"] != iris.DaemonFrameError {
		t.Fatalf("expected error frame, got %q (full=%+v)", frame["type"], frame)
	}
	msg, _ := frame["message"].(string)
	if msg == "" || !containsEsc(msg, "multiple coordinator candidates") {
		t.Errorf("message does not mention multiple candidates: %q", msg)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.escalateCalls) != 0 {
		t.Errorf("expected zero escalate calls on multiple-candidate reject, got %+v", fx.escalateCalls)
	}
	if len(fx.deliverCalls) != 0 {
		t.Errorf("expected zero deliver calls on multiple-candidate reject, got %+v", fx.deliverCalls)
	}
}

// TestEscalate_ExplicitTo covers --to: the worker chooses one of several
// coordinators by name. Auto-discovery is bypassed.
func TestEscalate_ExplicitTo(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
		{Name: "iris-coord@alt", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat": "worker",
		"iris-coord@main":  "coordinator",
		"iris-coord@alt":   "coordinator",
	}
	fx := newEscalateFixture(t, sessions, roles)

	conn, r := fx.dial()
	fx.sendEscalate(conn, "iris-worker@feat", "iris-coord@alt", "targeted")

	frame := fx.readFrame(conn, r)
	if frame["type"] != iris.DaemonFrameEscalationDelivered {
		t.Fatalf("expected escalation_delivered, got %q (full=%+v)", frame["type"], frame)
	}
	if frame["to"] != "iris-coord@alt" {
		t.Errorf("to = %v, want iris-coord@alt", frame["to"])
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.deliverCalls) != 1 || fx.deliverCalls[0].Name != "iris-coord@alt" {
		t.Errorf("deliverCalls = %+v (want exactly one to iris-coord@alt)", fx.deliverCalls)
	}
}

// TestEscalate_ToNotCoordinator covers --to naming a worker (not a
// coordinator). Daemon rejects; no state change, no delivery.
func TestEscalate_ToNotCoordinator(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@a", State: "active", Role: "worker"},
		{Name: "iris-worker@b", State: "active", Role: "worker"},
	}
	roles := map[string]string{
		"iris-worker@a": "worker",
		"iris-worker@b": "worker",
	}
	fx := newEscalateFixture(t, sessions, roles)

	conn, r := fx.dial()
	fx.sendEscalate(conn, "iris-worker@a", "iris-worker@b", "wrong target")

	frame := fx.readFrame(conn, r)
	if frame["type"] != iris.DaemonFrameError {
		t.Fatalf("expected error frame, got %+v", frame)
	}
	msg, _ := frame["message"].(string)
	if !containsEsc(msg, "not a coordinator") {
		t.Errorf("message missing 'not a coordinator': %q", msg)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.escalateCalls) != 0 {
		t.Errorf("expected zero escalate calls; got %+v", fx.escalateCalls)
	}
	if len(fx.deliverCalls) != 0 {
		t.Errorf("expected zero deliver calls; got %+v", fx.deliverCalls)
	}
}

// TestEscalate_UnregisteredCaller covers the "not a registered iris session"
// branch: the From field names a session not in the active list.
func TestEscalate_UnregisteredCaller(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{"iris-coord@main": "coordinator"}
	fx := newEscalateFixture(t, sessions, roles)

	conn, r := fx.dial()
	fx.sendEscalate(conn, "iris-worker@ghost", "", "from nowhere")

	frame := fx.readFrame(conn, r)
	if frame["type"] != iris.DaemonFrameError {
		t.Fatalf("expected error frame, got %+v", frame)
	}
	msg, _ := frame["message"].(string)
	if !containsEsc(msg, "not a registered iris session") {
		t.Errorf("message missing canonical wording: %q", msg)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.escalateCalls) != 0 {
		t.Errorf("expected zero escalate calls; got %+v", fx.escalateCalls)
	}
}

// TestEscalate_ExactlyOnceDelivery sends the SAME escalation frame twice on
// the same connection. The handler must dispatch two deliverPrompt calls
// (each call mints its own delivery_id), but each call carries a distinct
// delivery_id so the receiving harness can dedup. The contract from issue
// #1695 is that the path is uniform with `iris prompt`: one invocation →
// one delivery at the dispatch layer. Two invocations → two dispatches,
// each idempotent at the harness.
//
// This test pins the wire shape rather than the harness-level dedup (which
// is downstream of this code).
func TestEscalate_ExactlyOnceDelivery(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat": "worker",
		"iris-coord@main":  "coordinator",
	}
	fx := newEscalateFixture(t, sessions, roles)

	conn, r := fx.dial()
	fx.sendEscalate(conn, "iris-worker@feat", "", "first")
	frame1 := fx.readFrame(conn, r)
	id1, _ := frame1["delivery_id"].(string)
	fx.sendEscalate(conn, "iris-worker@feat", "", "second")
	frame2 := fx.readFrame(conn, r)
	id2, _ := frame2["delivery_id"].(string)

	if id1 == "" || id2 == "" {
		t.Fatalf("expected delivery_id in both acks; got %q %q", id1, id2)
	}
	if id1 == id2 {
		t.Errorf("delivery_id should differ between two invocations; got identical %q", id1)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if got := len(fx.deliverCalls); got != 2 {
		t.Errorf("deliverCalls = %d, want 2 (one per invocation)", got)
	}
}

// TestEscalate_DeliveryFailure asserts that a delivery error from the
// underlying deliverPrompt is surfaced verbatim. The worker is still in
// escalated state at the daemon (state transition happens before delivery).
func TestEscalate_DeliveryFailure(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "active", Role: "worker"},
		{Name: "iris-coord@main", State: "active", Role: "coordinator"},
	}
	roles := map[string]string{
		"iris-worker@feat": "worker",
		"iris-coord@main":  "coordinator",
	}
	fx := newEscalateFixture(t, sessions, roles)
	fx.mu.Lock()
	fx.deliverErr = fmt.Errorf("synthetic-deliver-error")
	fx.mu.Unlock()

	conn, r := fx.dial()
	fx.sendEscalate(conn, "iris-worker@feat", "", "boom")

	frame := fx.readFrame(conn, r)
	if frame["type"] != iris.DaemonFrameError {
		t.Fatalf("expected error frame, got %+v", frame)
	}
	msg, _ := frame["message"].(string)
	if !containsEsc(msg, "synthetic-deliver-error") {
		t.Errorf("error message missing underlying cause: %q", msg)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	// The state transition fires BEFORE the delivery attempt so the worker
	// is correctly escalated even on a downstream failure.
	if len(fx.escalateCalls) != 1 {
		t.Errorf("expected one escalate call (state transition is pre-delivery); got %+v", fx.escalateCalls)
	}
}

// TestPromptDeliver_ResumesEscalated asserts that a prompt_deliver to a
// worker that is in escalated state triggers the resumeSession hook so the
// worker transitions back to active. This is the escalated→active half of
// the state machine.
func TestPromptDeliver_ResumesEscalated(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "iris-worker@feat", State: "escalated", Role: "worker"},
	}
	fx := newEscalateFixture(t, sessions, map[string]string{"iris-worker@feat": "worker"})

	conn, r := fx.dial()
	// Send a prompt_deliver frame directly.
	pd := iris.ClientPromptDeliverFrame{
		Type: iris.ClientFramePromptDeliver,
		Name: "iris-worker@feat",
		Text: "back to work",
	}
	data, _ := json.Marshal(pd)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write prompt_deliver: %v", err)
	}
	// prompt_deliver has no success ack — wait briefly for the resume hook
	// to fire. Use a short polling loop bounded by a deadline.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fx.mu.Lock()
		n := len(fx.resumeCalls)
		fx.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.resumeCalls) != 1 || fx.resumeCalls[0] != "iris-worker@feat" {
		t.Errorf("resumeCalls = %+v, want [iris-worker@feat]", fx.resumeCalls)
	}
	if len(fx.deliverCalls) != 1 {
		t.Errorf("deliverCalls = %+v (want 1)", fx.deliverCalls)
	}
	// Drain the connection — we don't expect any error frame, but make
	// sure we don't leak a buffered read deadline.
	_ = r
}

func containsEsc(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
