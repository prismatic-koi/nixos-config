package db

import (
	"path/filepath"
	"testing"
	"time"
)

// TestPendingReplayDeliveries_InsertLoadDelete exercises the full happy path
// of the pending-replay durable buffer (issue #2359 Gap B): insert three
// rows in one session's queue, load them back in FIFO order, delete one,
// and verify the remaining set. This is the persistence half of the
// contract the sidecar's bufferPendingReplay/flushPendingReplay pair
// relies on across a restart.
func TestPendingReplayDeliveries_InsertLoadDelete(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const session = "prism-test@invoker-pending-replay-happy"
	rows := []PendingReplayRow{
		{SessionName: session, DeliveryID: "id-a", Text: "first", DeliverAs: "steer", Source: "escalate", QueuedAt: time.UnixMilli(1000)},
		{SessionName: session, DeliveryID: "id-b", Text: "second", DeliverAs: "followUp", Source: "", QueuedAt: time.UnixMilli(2000)},
		{SessionName: session, DeliveryID: "id-c", Text: "third", DeliverAs: "nextTurn", Source: "review-complete", QueuedAt: time.UnixMilli(3000)},
	}
	for _, r := range rows {
		if _, err := d.InsertPendingReplayDelivery(r); err != nil {
			t.Fatalf("InsertPendingReplayDelivery(%q): %v", r.DeliveryID, err)
		}
	}

	got, err := d.LoadPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("LoadPendingReplayDeliveries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Load: want 3 rows, got %d", len(got))
	}
	if got[0].DeliveryID != "id-a" || got[1].DeliveryID != "id-b" || got[2].DeliveryID != "id-c" {
		t.Errorf("Load: wrong FIFO order: %+v", got)
	}
	if got[0].Text != "first" || got[2].Source != "review-complete" {
		t.Errorf("Load: field round-trip broken: %+v", got)
	}

	if err := d.DeletePendingReplayDelivery(session, "id-b"); err != nil {
		t.Fatalf("DeletePendingReplayDelivery: %v", err)
	}
	after, err := d.LoadPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("LoadPendingReplayDeliveries after delete: %v", err)
	}
	if len(after) != 2 || after[0].DeliveryID != "id-a" || after[1].DeliveryID != "id-c" {
		t.Errorf("After delete: expected [id-a, id-c], got %+v", after)
	}

	n, err := d.CountPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("CountPendingReplayDeliveries: %v", err)
	}
	if n != 2 {
		t.Errorf("CountPendingReplayDeliveries: want 2, got %d", n)
	}
}

// TestPendingReplayDeliveries_DedupOnRepeatInsert verifies that inserting
// the same (session_name, delivery_id) twice is a no-op on the second
// insert — the existing row is preserved. This mirrors the in-memory dedup
// semantics from #1685 that the durable buffer must preserve across a
// sidecar restart (issue #2359 AC: exactly-once delivery is preserved).
func TestPendingReplayDeliveries_DedupOnRepeatInsert(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const session = "prism-test@invoker-pending-replay-dedup"
	original := PendingReplayRow{
		SessionName: session, DeliveryID: "dedup-key", Text: "original",
		DeliverAs: "steer", Source: "escalate", QueuedAt: time.UnixMilli(1000),
	}
	dup := original
	dup.Text = "REPLACED"
	dup.QueuedAt = time.UnixMilli(2000)

	if _, err := d.InsertPendingReplayDelivery(original); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := d.InsertPendingReplayDelivery(dup); err != nil {
		t.Fatalf("dup insert: %v", err)
	}
	got, err := d.LoadPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row after dedup insert, got %d: %+v", len(got), got)
	}
	if got[0].Text != "original" {
		t.Errorf("dedup should preserve original row; got Text=%q, want %q", got[0].Text, "original")
	}
	if got[0].QueuedAt.UnixMilli() != 1000 {
		t.Errorf("dedup should preserve original queued_at; got %d, want 1000", got[0].QueuedAt.UnixMilli())
	}
}

// TestPendingReplayDeliveries_PerSessionScope confirms rows are scoped to
// their session_name — a load for one session must not return rows from
// another. This matters for the shared prism.db that runs many sidecars.
func TestPendingReplayDeliveries_PerSessionScope(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if _, err := d.InsertPendingReplayDelivery(PendingReplayRow{
		SessionName: "prism-test@invoker-scope-a", DeliveryID: "id-a",
		Text: "for-a", DeliverAs: "steer", Source: "",
		QueuedAt: time.UnixMilli(1000),
	}); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if _, err := d.InsertPendingReplayDelivery(PendingReplayRow{
		SessionName: "prism-test@invoker-scope-b", DeliveryID: "id-b",
		Text: "for-b", DeliverAs: "steer", Source: "",
		QueuedAt: time.UnixMilli(1000),
	}); err != nil {
		t.Fatalf("insert b: %v", err)
	}

	rowsA, err := d.LoadPendingReplayDeliveries("prism-test@invoker-scope-a")
	if err != nil {
		t.Fatalf("Load a: %v", err)
	}
	if len(rowsA) != 1 || rowsA[0].Text != "for-a" {
		t.Errorf("session-a load returned wrong rows: %+v", rowsA)
	}
	rowsB, err := d.LoadPendingReplayDeliveries("prism-test@invoker-scope-b")
	if err != nil {
		t.Fatalf("Load b: %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].Text != "for-b" {
		t.Errorf("session-b load returned wrong rows: %+v", rowsB)
	}
}

// TestPendingReplayDeliveries_NoIDGetsSyntheticKey verifies that a caller
// passing an empty delivery_id (legacy /prompt callers that omit the field
// entirely) still gets a durable row. Two no-ID inserts in a row must both
// land — no-ID entries cannot dedup on the receive side either, so the
// durable buffer's behaviour must match the in-memory buffer's (which
// keeps every no-ID entry).
func TestPendingReplayDeliveries_NoIDGetsSyntheticKey(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "prism.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	const session = "prism-test@invoker-no-id"
	// InsertPendingReplayDelivery generates a synthetic key from time.Now
	// nanoseconds. Two immediate inserts should still produce two rows
	// (the nanosecond stamp differs by at least a few ns even on fast
	// hardware), and the caller can look them up via LoadPendingReplayDeliveries.
	var keys []string
	for i := 0; i < 3; i++ {
		key, err := d.InsertPendingReplayDelivery(PendingReplayRow{
			SessionName: session, DeliveryID: "", Text: "no-id",
			DeliverAs: "steer", Source: "",
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		if key == "" {
			t.Fatalf("insert %d: expected non-empty synthetic key", i)
		}
		keys = append(keys, key)
		// Sleep a hair to guarantee distinct nanosecond stamps even on
		// systems with coarse clock granularity.
		time.Sleep(time.Millisecond)
	}
	// Verify the synthetic keys are all distinct so no-ID entries never
	// collide with each other.
	uniq := make(map[string]bool)
	for _, k := range keys {
		uniq[k] = true
	}
	if len(uniq) != len(keys) {
		t.Errorf("synthetic keys are not unique: %v", keys)
	}
	got, err := d.LoadPendingReplayDeliveries(session)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("want 3 no-ID rows, got %d: %+v", len(got), got)
	}
	for _, row := range got {
		if row.DeliveryID == "" {
			t.Errorf("stored row has empty delivery_id; expected synthetic key: %+v", row)
		}
	}
}
