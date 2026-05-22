package db

import (
	"errors"
	"testing"
	"time"
)

// TestIsSQLiteBusy verifies the IsSQLiteBusy helper recognises the error
// message patterns produced by the modernc.org/sqlite driver and the db
// package for SQLITE_BUSY and SQLITE_LOCKED conditions.
func TestIsSQLiteBusy(t *testing.T) {
	cases := []struct {
		name     string
		errMsg   string
		wantBusy bool
	}{
		{"SQLITE_BUSY", "db: upsert status: database is locked (5) (SQLITE_BUSY)", true},
		{"SQLITE_LOCKED", "db: upsert status: database is locked (6) (SQLITE_LOCKED)", true},
		{"database is locked only", "database is locked", true},
		{"not found", "db: upsert status: no such table: agent_status", false},
		{"generic error", "some other db error", false},
		{"nil error", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.errMsg != "" {
				err = errors.New(tc.errMsg)
			}
			got := IsSQLiteBusy(err)
			if got != tc.wantBusy {
				t.Errorf("IsSQLiteBusy(%q) = %v, want %v", tc.errMsg, got, tc.wantBusy)
			}
		})
	}
}

// TestWithBusyRetry_SuccessOnFirstAttempt verifies that WithBusyRetry returns
// nil immediately when fn succeeds on the first call.
func TestWithBusyRetry_SuccessOnFirstAttempt(t *testing.T) {
	calls := 0
	err := WithBusyRetry(3, time.Millisecond, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

// TestWithBusyRetry_SuccessAfterBusy verifies that WithBusyRetry retries on
// SQLITE_BUSY and returns nil when fn eventually succeeds.
func TestWithBusyRetry_SuccessAfterBusy(t *testing.T) {
	busyErr := errors.New("database is locked (5) (SQLITE_BUSY)")
	calls := 0
	err := WithBusyRetry(3, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return busyErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("want nil after retry, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

// TestWithBusyRetry_ExhaustsAttempts verifies that WithBusyRetry returns the
// last BUSY error when all attempts are exhausted.
func TestWithBusyRetry_ExhaustsAttempts(t *testing.T) {
	busyErr := errors.New("database is locked (SQLITE_BUSY)")
	calls := 0
	err := WithBusyRetry(3, time.Millisecond, func() error {
		calls++
		return busyErr
	})
	if err == nil {
		t.Fatal("want non-nil error, got nil")
	}
	if calls != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
	if !IsSQLiteBusy(err) {
		t.Errorf("returned error should be SQLITE_BUSY; got %v", err)
	}
}

// TestWithBusyRetry_NonBusyNoRetry verifies that non-BUSY errors short-circuit
// immediately without further retries.
func TestWithBusyRetry_NonBusyNoRetry(t *testing.T) {
	nonBusy := errors.New("some unexpected db error")
	calls := 0
	err := WithBusyRetry(3, time.Millisecond, func() error {
		calls++
		return nonBusy
	})
	if err != nonBusy {
		t.Fatalf("want original error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("want 1 call (no retry on non-BUSY), got %d", calls)
	}
}
