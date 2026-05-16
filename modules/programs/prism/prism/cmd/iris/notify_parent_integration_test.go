package main

// notify_parent_integration_test.go — end-to-end integration test for the
// iris notifyParentWorker analogue (issue #1700). It exercises the full
// daemon-side path:
//
//   spawn parent → spawn child with ParentSession=parent → child reaches
//   terminal state → makeNotifyParent callback fires → deliverFn invoked
//   against the parent → session.parent_notified audit row written.
//
// The test does NOT exercise the wire frame (`session_spawn` Parent field
// going across the iris.sock) because that is covered by spawn_integration_test.go
// after the column / forwarding wiring lands. Here we drive the daemon
// internals directly to keep the integration small and deterministic.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/iris"
	"github.com/prismatic-koi/prism/internal/iris/iristest"
)

// notifyDeliverCall captures one call to the daemon's deliverFn closure so
// the test can assert wording, target, and delivery mode. Named with a
// `notify` prefix to avoid colliding with deliverCall in pr_integration_test.go.
type notifyDeliverCall struct {
	Name      string
	Text      string
	DeliverAs string
}

// recordingDeliverFn wraps a recorder around a deliverFn. The wrapped
// function returns success — we are testing the callback wiring, not the
// delivery layer.
func recordingDeliverFn(t *testing.T) (deliverPromptFn, func() []notifyDeliverCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []notifyDeliverCall
	)
	fn := func(_ context.Context, name, text, deliverAs string, _ []string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, notifyDeliverCall{Name: name, Text: text, DeliverAs: deliverAs})
		return nil
	}
	snapshot := func() []notifyDeliverCall {
		mu.Lock()
		defer mu.Unlock()
		out := make([]notifyDeliverCall, len(calls))
		copy(out, calls)
		return out
	}
	return fn, snapshot
}

// TestNotifyParent_FinishedDeliversToParent asserts that when the
// supervisor's NotifyParent callback (constructed via makeNotifyParent) is
// invoked for a clean terminal, the parent receives exactly one followUp
// prompt with the prism-verbatim wording.
func TestNotifyParent_FinishedDeliversToParent(t *testing.T) {
	iso := iristest.NewIsolated(t)

	state := &daemonState{
		supervisors: make(map[string]*iris.Supervisor),
	}
	// Seed a fake parent supervisor in the map. The makeNotifyParent
	// callback only consults state.supervisors[parent] to decide whether
	// the parent is live; it does not call any methods on the supervisor.
	parentName := iristest.SessionName("parent")
	childName := iristest.SessionName("child")
	state.addSupervisor(parentName, iris.NewFakeSupervisorForTest(parentName, iso.Root, "coordinator"))

	deliver, snapshot := recordingDeliverFn(t)
	notify := makeNotifyParent(context.Background(), state, deliver, iso.DB)

	notify(childName, parentName, iris.StateFinished, "delivery-uuid-1")

	// makeNotifyParent runs synchronously (its caller, setState, dispatches
	// it in a goroutine — but here we are calling notify directly).
	calls := snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one deliver call, got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != parentName {
		t.Errorf("delivered to %q, want %q", calls[0].Name, parentName)
	}
	wantBody := "Agent " + childName + " has finished its current task"
	if calls[0].Text != wantBody {
		t.Errorf("body = %q, want %q", calls[0].Text, wantBody)
	}
	if calls[0].DeliverAs != "followUp" {
		t.Errorf("deliver_as = %q, want followUp", calls[0].DeliverAs)
	}

	// session.parent_notified audit row should be present.
	assertParentNotifiedEvent(t, iso.DB, childName, parentName, wantBody, "delivery-uuid-1", "finished", true)
}

// TestNotifyParent_ErroredDeliversToParent asserts the StateError wording.
func TestNotifyParent_ErroredDeliversToParent(t *testing.T) {
	iso := iristest.NewIsolated(t)

	state := &daemonState{
		supervisors: make(map[string]*iris.Supervisor),
	}
	parentName := iristest.SessionName("parent-err")
	childName := iristest.SessionName("child-err")
	state.addSupervisor(parentName, iris.NewFakeSupervisorForTest(parentName, iso.Root, "coordinator"))

	deliver, snapshot := recordingDeliverFn(t)
	notify := makeNotifyParent(context.Background(), state, deliver, iso.DB)

	notify(childName, parentName, iris.StateError, "delivery-uuid-err")

	calls := snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one deliver call, got %d", len(calls))
	}
	wantBody := "Agent " + childName + " has errored its current task"
	if calls[0].Text != wantBody {
		t.Errorf("body = %q, want %q", calls[0].Text, wantBody)
	}

	assertParentNotifiedEvent(t, iso.DB, childName, parentName, wantBody, "delivery-uuid-err", "error", true)
}

// TestNotifyParent_ParentGoneSkipsDelivery asserts that when the parent
// supervisor has already been removed from the daemon's live map, no
// deliver call is made, no error is propagated, and an audit row is still
// written with delivered=false.
func TestNotifyParent_ParentGoneSkipsDelivery(t *testing.T) {
	iso := iristest.NewIsolated(t)

	state := &daemonState{
		supervisors: make(map[string]*iris.Supervisor),
	}
	parentName := iristest.SessionName("parent-gone")
	childName := iristest.SessionName("child-orphan")
	// NOTE: parent is NOT added to the map.

	deliver, snapshot := recordingDeliverFn(t)
	notify := makeNotifyParent(context.Background(), state, deliver, iso.DB)

	notify(childName, parentName, iris.StateFinished, "delivery-uuid-orphan")

	if calls := snapshot(); len(calls) != 0 {
		t.Fatalf("expected zero deliver calls (parent already cleaned up), got %d: %+v", len(calls), calls)
	}

	// Audit row still records the attempt, marked delivered=false so the
	// operator can see that the parent had already gone when the child
	// finished.
	wantBody := "Agent " + childName + " has finished its current task"
	assertParentNotifiedEvent(t, iso.DB, childName, parentName, wantBody, "delivery-uuid-orphan", "finished", false)
}

// TestNotifyParent_EmptyParentIsNoOp asserts that calling notify with an
// empty parent (defence-in-depth: the supervisor already guards this) is
// a no-op — no delivery, no audit row.
func TestNotifyParent_EmptyParentIsNoOp(t *testing.T) {
	iso := iristest.NewIsolated(t)

	state := &daemonState{
		supervisors: make(map[string]*iris.Supervisor),
	}
	childName := iristest.SessionName("child-top-level")

	deliver, snapshot := recordingDeliverFn(t)
	notify := makeNotifyParent(context.Background(), state, deliver, iso.DB)

	notify(childName, "", iris.StateFinished, "delivery-uuid-empty")

	if calls := snapshot(); len(calls) != 0 {
		t.Fatalf("expected zero deliver calls (empty parent), got %d", len(calls))
	}

	// No audit row either — empty parent is the top-level-spawn case.
	rows, err := iso.DB.AllSessionEvents(childName)
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}
	for _, r := range rows {
		if r.Type == "session.parent_notified" {
			t.Errorf("found session.parent_notified event for empty-parent case: %+v", r)
		}
	}
}

// TestNotifyParent_AutoMintsDeliveryIDWhenEmpty asserts that an empty
// delivery_id parameter triggers a UUID mint inside the callback so the
// audit row always carries a non-empty identifier.
func TestNotifyParent_AutoMintsDeliveryIDWhenEmpty(t *testing.T) {
	iso := iristest.NewIsolated(t)

	state := &daemonState{
		supervisors: make(map[string]*iris.Supervisor),
	}
	parentName := iristest.SessionName("parent-empty-id")
	childName := iristest.SessionName("child-empty-id")
	state.addSupervisor(parentName, iris.NewFakeSupervisorForTest(parentName, iso.Root, "coordinator"))

	deliver, _ := recordingDeliverFn(t)
	notify := makeNotifyParent(context.Background(), state, deliver, iso.DB)

	notify(childName, parentName, iris.StateFinished, "" /* empty delivery_id */)

	rows, err := iso.DB.AllSessionEvents(childName)
	if err != nil {
		t.Fatalf("AllSessionEvents: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Type != "session.parent_notified" {
			continue
		}
		var payload struct {
			DeliveryID string `json:"delivery_id"`
		}
		if err := json.Unmarshal([]byte(r.Payload), &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload.DeliveryID == "" {
			t.Errorf("delivery_id was empty in audit row; the callback must mint one when the caller passes empty")
		}
		found = true
	}
	if !found {
		t.Errorf("no session.parent_notified audit row found for %s", childName)
	}
}

// TestParentSessionRoundTripsThroughDB asserts that a Session row inserted
// with ParentSession set returns the same value on SessionByInstanceID.
// This is the lowest-level guard that the v29→v30 migration's column is
// being read back correctly.
func TestParentSessionRoundTripsThroughDB(t *testing.T) {
	iso := iristest.NewIsolated(t)

	parent := iristest.SessionName("parent-db")
	parentPtr := parent
	role := "worker"
	sess := db.Session{
		InstanceID:    "11111111-1111-1111-1111-111111111111",
		SessionName:   iristest.SessionName("child-db"),
		AgentRole:     &role,
		Repo:          "iris-test",
		Worktree:      iso.Root,
		Harness:       "pi",
		ParentSession: &parentPtr,
	}
	if err := iso.DB.InsertSession(sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	got, err := iso.DB.SessionByInstanceID(sess.InstanceID)
	if err != nil {
		t.Fatalf("SessionByInstanceID: %v", err)
	}
	if got == nil {
		t.Fatal("session not found after insert")
	}
	if got.ParentSession == nil {
		t.Fatal("ParentSession returned NULL; want non-nil")
	}
	if *got.ParentSession != parent {
		t.Errorf("ParentSession = %q, want %q", *got.ParentSession, parent)
	}
}

// assertParentNotifiedEvent reads the most recent agent_events row of type
// session.parent_notified for childName and asserts the payload matches the
// expected wording, delivery_id, state, and delivered flag.
func assertParentNotifiedEvent(t *testing.T, database *db.DB, child, parent, body, deliveryID, terminal string, delivered bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := database.AllSessionEvents(child)
		if err != nil {
			t.Fatalf("AllSessionEvents: %v", err)
		}
		for _, r := range rows {
			if r.Type != "session.parent_notified" {
				continue
			}
			var payload struct {
				Child      string `json:"child"`
				Parent     string `json:"parent"`
				State      string `json:"state"`
				Body       string `json:"body"`
				DeliveryID string `json:"delivery_id"`
				Delivered  bool   `json:"delivered"`
			}
			if err := json.Unmarshal([]byte(r.Payload), &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if payload.DeliveryID != deliveryID {
				continue // not the row we are looking for
			}
			if payload.Child != child {
				t.Errorf("payload.child = %q, want %q", payload.Child, child)
			}
			if payload.Parent != parent {
				t.Errorf("payload.parent = %q, want %q", payload.Parent, parent)
			}
			if payload.State != terminal {
				t.Errorf("payload.state = %q, want %q", payload.State, terminal)
			}
			if !strings.Contains(payload.Body, body) {
				t.Errorf("payload.body = %q, want substring %q", payload.Body, body)
			}
			if payload.Delivered != delivered {
				t.Errorf("payload.delivered = %v, want %v", payload.Delivered, delivered)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session.parent_notified audit row not found for child=%s delivery_id=%s", child, deliveryID)
}
