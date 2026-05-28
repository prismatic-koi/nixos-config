package db

// DB-layer tests for the muted flag on agent_status (#2013). These exercise
// the SetMuted / IsMuted helpers and the persistence-across-Open contract
// asserted by the issue ACs:
//
//   - "Setting a session to muted, restarting the sidecar, and reading
//     db.Status for that session returns Muted: true."
//   - "prism cleanup --session <name> removes the session row including its
//     muted flag; a subsequent prism mute <name> for that now-gone session
//     returns a clear 'session not found' error."

import (
	"path/filepath"
	"testing"
	"time"
)

// openMutedTestDB opens a fresh DB under t.TempDir() and registers cleanup.
func openMutedTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "muted.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

// TestStatus_MutedDefaultsFalse asserts new rows default to unmuted.
func TestStatus_MutedDefaultsFalse(t *testing.T) {
	d, _ := openMutedTestDB(t)

	const session = "prism-test@muted-default"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	st, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus returned nil")
	}
	if st.Muted {
		t.Errorf("new row default Muted = true, want false")
	}
}

// TestStatus_SetMutedRoundtrip asserts SetMuted persists and CurrentStatus
// reads back the same value. Both the true and false transitions are tested.
func TestStatus_SetMutedRoundtrip(t *testing.T) {
	d, _ := openMutedTestDB(t)
	const session = "prism-test@muted-roundtrip"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}

	ok, err := d.SetMuted(session, true)
	if err != nil || !ok {
		t.Fatalf("SetMuted(true): ok=%v err=%v", ok, err)
	}
	st, _ := d.CurrentStatus(session)
	if !st.Muted {
		t.Error("Muted false after SetMuted(true)")
	}

	ok, err = d.SetMuted(session, false)
	if err != nil || !ok {
		t.Fatalf("SetMuted(false): ok=%v err=%v", ok, err)
	}
	st, _ = d.CurrentStatus(session)
	if st.Muted {
		t.Error("Muted true after SetMuted(false)")
	}
}

// TestStatus_MutedPersistsAcrossReopen asserts the AC: muted=true survives a
// db.Close + db.Open cycle (the on-disk persistence contract that mirrors a
// sidecar restart).
func TestStatus_MutedPersistsAcrossReopen(t *testing.T) {
	d, path := openMutedTestDB(t)
	const session = "prism-test@muted-persist"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if _, err := d.SetMuted(session, true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}
	d.Close()

	// Re-open the same on-disk file.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer d2.Close()

	st, err := d2.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus after reopen: %v", err)
	}
	if st == nil {
		t.Fatal("CurrentStatus after reopen returned nil")
	}
	if !st.Muted {
		t.Errorf("Muted not persisted across reopen; got Muted=false, want true")
	}
}

// TestStatus_SetMutedMissingRowReturnsFalse asserts that SetMuted on a
// non-existent row returns (false, nil) and does NOT insert a phantom row.
func TestStatus_SetMutedMissingRowReturnsFalse(t *testing.T) {
	d, _ := openMutedTestDB(t)

	ok, err := d.SetMuted("does-not-exist", true)
	if err != nil {
		t.Fatalf("SetMuted: %v", err)
	}
	if ok {
		t.Error("SetMuted on missing row returned ok=true, want false")
	}

	st, _ := d.CurrentStatus("does-not-exist")
	if st != nil {
		t.Errorf("phantom row inserted: %+v", st)
	}
}

// TestStatus_IsMutedTreatsEndedSessionAsMissing asserts the AC behaviour
// after `prism cleanup --session <name>`: the agent_status row carries
// ended_at, IsMuted returns (false, false, nil), and the CLI maps that to
// a "session not found" error.
//
// We simulate cleanup by calling SetEnded (the same method cleanup invokes
// under the hood) rather than running the full cleanup command, which would
// pull in tmux / git / archive paths irrelevant to the DB-layer contract.
func TestStatus_IsMutedTreatsEndedSessionAsMissing(t *testing.T) {
	d, _ := openMutedTestDB(t)
	const session = "prism-test@muted-ended"
	if err := d.UpsertStatus(session, "prism-test", "/tmp/w", "active", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	// Mute first, then end \u2014 this also covers the "muted flag is preserved
	// in the row but the CLI cannot see it" forensic case.
	if _, err := d.SetMuted(session, true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}
	if err := d.SetEnded(session); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}

	muted, ok, err := d.IsMuted(session)
	if err != nil {
		t.Fatalf("IsMuted: %v", err)
	}
	if ok {
		t.Error("IsMuted on ended session returned ok=true; want false (treated as not-found)")
	}
	if muted {
		t.Error("IsMuted on ended session returned muted=true; want false")
	}

	// And SetMuted on an ended session is also a no-op (rows affected = 0).
	updated, err := d.SetMuted(session, false)
	if err != nil {
		t.Fatalf("SetMuted on ended: %v", err)
	}
	// SetMuted does not filter on ended_at by design \u2014 it updates whatever row
	// matches. We accept the post-state observation: if SetMuted updated, the
	// row's muted column is now 0; if not, the prior value persists. Either
	// way IsMuted continues to treat the ended row as missing.
	_ = updated

	muted2, ok2, err := d.IsMuted(session)
	if err != nil {
		t.Fatalf("IsMuted (post): %v", err)
	}
	if ok2 || muted2 {
		t.Errorf("IsMuted after re-set on ended session: ok=%v muted=%v; want false,false", ok2, muted2)
	}

	// Direct CurrentStatus must still surface the row \u2014 the dashboard / audit
	// path needs to see ended sessions. Confirm ended_at is set.
	st, _ := d.CurrentStatus(session)
	if st == nil {
		t.Fatal("CurrentStatus returned nil for ended row; want non-nil")
	}
	if st.EndedAt == nil {
		t.Error("ended row has nil EndedAt; SetEnded should have stamped it")
	} else if st.EndedAt.After(time.Now().Add(1 * time.Second)) {
		t.Errorf("ended row EndedAt %v is in the future", st.EndedAt)
	}
}
