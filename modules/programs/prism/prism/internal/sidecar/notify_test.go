// Tests in this file exercise notifyCoordinator's audit-row behaviour:
//   - On delivery failure, WriteBusMessageFailed must be called with a fully
//     populated db.BusMessage (issue #1856).
//   - On delivery success, WriteBusMessageFailed must NOT be called.
//
// # Isolation contract
//
// Every test in this file constructs a sidecar.Sidecar via
// sidecartest.NewIsolated and uses session names with the "prism-test@"
// prefix so a running `go test ./internal/sidecar/...` can never deliver to
// or write through a live coordinator on the host. See sidecartest for the
// full isolation guarantees.
package sidecar

import (
	"errors"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// seedTestWorker inserts an active worker session row into d so
// notifyCoordinator's self-status / escalated guards see a real row when the
// worker calls them.
func seedTestWorker(t *testing.T, d *db.DB, sessionName, repo string) {
	t.Helper()
	if err := d.UpsertStatus(sessionName, repo, "/tmp/test-worktree-"+sessionName, "finished", nil, nil); err != nil {
		t.Fatalf("seed worker %q: %v", sessionName, err)
	}
}

// seedTestCoordinator marks the coordinator row created by
// sidecartest.NewIsolated with root_agent_name='coordinator' so
// CoordinatorForRepo discovers it via the DB-backed path.
func seedTestCoordinator(t *testing.T, d *db.DB, coordSession string) {
	t.Helper()
	if err := d.QueryRow(
		"UPDATE agent_status SET root_agent_name = 'coordinator' WHERE session_name = ? RETURNING session_name",
		coordSession,
	).Scan(new(string)); err != nil {
		t.Fatalf("seed coordinator root_agent_name: %v", err)
	}
}

// readSingleFailedBusMessage selects exactly one bus_messages row with
// failed_at IS NOT NULL and returns it as a db.BusMessage. The Repo and
// ToInstanceID fields on the returned message are set from the corresponding
// columns; SentAt/FailedAt/DeliveredAt are populated from the int64 columns.
// Fails the test if zero or more than one such row exists.
func readSingleFailedBusMessage(t *testing.T, d *db.DB) db.BusMessage {
	t.Helper()
	var count int
	if err := d.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE failed_at IS NOT NULL").Scan(&count); err != nil {
		t.Fatalf("count failed bus_messages: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 failed bus_message, got %d", count)
	}
	row := d.QueryRow(`
SELECT id, from_session, to_session, repo, text, urgency, sent_at, delivered_at, failed_at
FROM bus_messages
WHERE failed_at IS NOT NULL
LIMIT 1`)
	var m db.BusMessage
	var sentAtMs int64
	var deliveredAtMs, failedAtMs *int64
	if err := row.Scan(&m.ID, &m.FromSession, &m.ToSession, &m.Repo, &m.Text, &m.Urgency, &sentAtMs, &deliveredAtMs, &failedAtMs); err != nil {
		t.Fatalf("scan failed bus_message: %v", err)
	}
	m.SentAt = time.UnixMilli(sentAtMs)
	if deliveredAtMs != nil {
		t := time.UnixMilli(*deliveredAtMs)
		m.DeliveredAt = &t
	}
	if failedAtMs != nil {
		t := time.UnixMilli(*failedAtMs)
		m.FailedAt = &t
	}
	return m
}

// TestNotifyCoordinator_WriteBusMessageFailed_OnDeliveryError verifies the
// failure-audit path (issue #1856 AC #1): when promptdelivery returns an
// error (mimicking a SIGTERMed coordinator vanishing between
// CoordinatorForRepo and DeliverToSession), the sidecar must write a
// bus_messages row via WriteBusMessageFailed with the full BusMessage shape.
//
// The seam used here is the narrow notifyCoordinatorDeliverFn function
// pointer on the Sidecar struct; see the field declaration for the rationale.
func TestNotifyCoordinator_WriteBusMessageFailed_OnDeliveryError(t *testing.T) {
	workerSession := "prism-test@worker-failaudit"
	coordSession := "prism-test@coordinator-failaudit"
	repo := "prism-test"

	// NewIsolated seeds the coordinator session row pointing at the
	// in-process HTTP/socket bus. We promote it to a real coordinator with
	// root_agent_name='coordinator' so CoordinatorForRepo discovers it.
	bus := sidecartest.NewIsolated(t, coordSession)
	seedTestCoordinator(t, bus.DB, coordSession)
	seedTestWorker(t, bus.DB, workerSession, repo)

	clk := newTestClock()
	cfg := Config{
		SessionName: workerSession,
		Repo:        repo,
		Worktree:    "/tmp/test-worker-failaudit",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
		AgentRole:   "worker",
	}
	s := New(cfg)

	// Install the failure-injection seam: force DeliverToSession to return
	// an error so the WriteBusMessageFailed branch in notifyCoordinator runs.
	injectedErr := errors.New("simulated: coordinator vanished mid-delivery")
	var capturedCoordinator string
	var capturedText string
	s.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		capturedCoordinator = sessionName
		capturedText = text
		return injectedErr
	}

	// Record the boundary times so we can assert sent_at/failed_at are in
	// a sane range. Allow a small slack on both sides for the millisecond
	// rounding inside the DB write.
	before := time.Now().Add(-1 * time.Second)
	s.notifyCoordinator("")
	after := time.Now().Add(1 * time.Second)

	// The seam closure must have run with the coordinator's session name.
	if capturedCoordinator != coordSession {
		t.Errorf("deliver fn called with sessionName=%q, want %q", capturedCoordinator, coordSession)
	}
	wantText := "Agent " + workerSession + " has finished its current task"
	if capturedText != wantText {
		t.Errorf("deliver fn called with text=%q, want %q", capturedText, wantText)
	}

	// Now assert the audit row.
	m := readSingleFailedBusMessage(t, bus.DB)

	if m.ID == "" {
		t.Error("audit row: ID is empty, want a UUID")
	}
	if m.FromSession != workerSession {
		t.Errorf("audit row: FromSession = %q, want %q", m.FromSession, workerSession)
	}
	if m.ToSession != coordSession {
		t.Errorf("audit row: ToSession = %q, want %q", m.ToSession, coordSession)
	}
	if m.Repo != repo {
		t.Errorf("audit row: Repo = %q, want %q", m.Repo, repo)
	}
	if m.Text != wantText {
		t.Errorf("audit row: Text = %q, want %q", m.Text, wantText)
	}
	if m.Urgency != "normal" {
		t.Errorf("audit row: Urgency = %q, want %q", m.Urgency, "normal")
	}
	if m.SentAt.IsZero() {
		t.Error("audit row: SentAt is zero, want non-zero")
	}
	if m.SentAt.Before(before) || m.SentAt.After(after) {
		t.Errorf("audit row: SentAt %v outside expected window [%v, %v]", m.SentAt, before, after)
	}
	if m.FailedAt == nil {
		t.Error("audit row: FailedAt is nil, want non-nil")
	} else if m.FailedAt.Before(before) || m.FailedAt.After(after) {
		t.Errorf("audit row: FailedAt %v outside expected window [%v, %v]", *m.FailedAt, before, after)
	}
	if m.DeliveredAt != nil {
		t.Errorf("audit row: DeliveredAt = %v, want nil (delivery failed)", *m.DeliveredAt)
	}
}

// TestNotifyCoordinator_NoFailedAudit_OnDeliverySuccess verifies the
// complementary happy-path assertion (issue #1856 AC #2): when delivery
// succeeds, no bus_messages row with failed_at IS NOT NULL is written.
//
// This test installs the same seam but returns nil; the delivered-audit row
// is still written, but no failed-audit row may exist.
func TestNotifyCoordinator_NoFailedAudit_OnDeliverySuccess(t *testing.T) {
	workerSession := "prism-test@worker-okaudit"
	coordSession := "prism-test@coordinator-okaudit"
	repo := "prism-test"

	bus := sidecartest.NewIsolated(t, coordSession)
	seedTestCoordinator(t, bus.DB, coordSession)
	seedTestWorker(t, bus.DB, workerSession, repo)

	clk := newTestClock()
	cfg := Config{
		SessionName: workerSession,
		Repo:        repo,
		Worktree:    "/tmp/test-worker-okaudit",
		DB:          bus.DB,
		Clock:       clk,
		HTTPClient:  bus.HTTPServer.Client(),
		Harness:     newSSEHarness(),
		AgentRole:   "worker",
	}
	s := New(cfg)

	// Force a successful delivery — the seam returns nil, so the
	// WriteBusMessageFailed branch must not run.
	var delivered bool
	s.notifyCoordinatorDeliverFn = func(sessionName string, status *db.Status, text string, buildHTTPBody func(string, *db.Status) map[string]any, source string, deliverAs string) error {
		delivered = true
		return nil
	}

	s.notifyCoordinator("")

	if !delivered {
		t.Fatal("delivery seam was not invoked; the test cannot assert success path")
	}

	// No failed audit row may exist.
	var failedCount int
	if err := bus.DB.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE failed_at IS NOT NULL").Scan(&failedCount); err != nil {
		t.Fatalf("count failed bus_messages: %v", err)
	}
	if failedCount != 0 {
		t.Errorf("delivery succeeded but %d failed-audit row(s) were written; want 0", failedCount)
	}

	// And exactly one delivered audit row must exist for the coordinator.
	var deliveredCount int
	if err := bus.DB.QueryRow("SELECT COUNT(*) FROM bus_messages WHERE delivered_at IS NOT NULL AND to_session = ?", coordSession).Scan(&deliveredCount); err != nil {
		t.Fatalf("count delivered bus_messages: %v", err)
	}
	if deliveredCount != 1 {
		t.Errorf("delivered audit row count = %d, want 1", deliveredCount)
	}
}
