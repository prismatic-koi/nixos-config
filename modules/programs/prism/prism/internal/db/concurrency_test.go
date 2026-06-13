package db_test

// concurrency_test.go — concurrent-access tests for the db package (#1865).
//
// These tests fire N goroutines against a shared in-memory (temp file) DB and
// assert correctness properties that should hold under concurrent access.  All
// tests are run under the race detector (`go test -race`).

import (
	"fmt"
	"sync"
	"testing"
)

// TestAllocatePort_Concurrent fires N goroutines each calling AllocatePort for
// a distinct session_name and asserts that all returned ports are distinct
// (F4, #1865).
func TestAllocatePort_Concurrent(t *testing.T) {
	const N = 50
	d := openTestDB(t)

	// Pre-create all N sessions sequentially so that concurrent AllocatePort
	// calls can proceed without racing on the INSERT path.
	for i := range N {
		name := fmt.Sprintf("concurrent@s%d", i)
		if err := d.UpsertStatus(name, "repo", "/code/repo/"+name, "idle", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %s: %v", name, err)
		}
	}

	type result struct {
		port int
		err  error
	}

	results := make([]result, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("concurrent@s%d", idx)
			port, err := d.AllocatePort(name)
			results[idx] = result{port: port, err: err}
		}(i)
	}
	wg.Wait()

	// All calls must succeed.
	for i, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d: AllocatePort error: %v", i, r.err)
		}
	}

	// All returned ports must be distinct — the core correctness guarantee.
	seen := make(map[int]int) // port → first goroutine index
	for i, r := range results {
		if r.err != nil {
			continue // already reported above
		}
		if prev, dup := seen[r.port]; dup {
			t.Errorf("port collision: goroutines %d and %d both got port %d", prev, i, r.port)
		}
		seen[r.port] = i
	}
}

// TestUpsertStatusWithRootAgent_Concurrent fires N goroutines all upserting
// the same session_name with overlapping (but distinct) field sets, then
// asserts row count = 1 and all expected fields are present per COALESCE
// semantics (F19, #1865).
//
// Because COALESCE prefers the first non-NULL value, the test sets fields that
// are written on INSERT (the first writer) and fields that are present on every
// write (state, last_seen). The critical assertion is that exactly one row
// exists and its non-NULL fields are consistent.
func TestUpsertStatusWithRootAgent_Concurrent(t *testing.T) {
	const N = 50
	const session = "concurrent-upsert-root@main"
	d := openTestDB(t)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func(idx int) {
			defer wg.Done()
			// Each goroutine writes a distinct agentName so we can verify
			// that at least one value survived (COALESCE keeps the first
			// non-NULL). We always write state="active" so the final row
			// is deterministic on the state field.
			agentName := fmt.Sprintf("agent-%d", idx)
			modelID := fmt.Sprintf("model-%d", idx)
			err := d.UpsertStatusWithRootAgent(
				session, "repo", "/code/repo/main", "active",
				strPtr("concurrent title"),
				nil, // harnessSessionID
				strPtr(agentName),
				strPtr(modelID),
			)
			if err != nil {
				t.Errorf("goroutine %d: UpsertStatusWithRootAgent: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	// Row count must be exactly 1 — no phantom duplicates.
	var count int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM agent_status WHERE session_name = ?", session,
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("row count: got %d, want 1", count)
	}

	// The surviving row must have non-NULL agent_name, model_id, and state.
	s, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.AgentName == nil {
		t.Error("agent_name is NULL after concurrent upserts; expected non-NULL (COALESCE semantics)")
	}
	if s.ModelID == nil {
		t.Error("model_id is NULL after concurrent upserts; expected non-NULL (COALESCE semantics)")
	}
	if s.State != "active" {
		t.Errorf("state: got %q, want %q", s.State, "active")
	}
}

// TestUpsertStatus_Concurrent does the same as TestUpsertStatusWithRootAgent_Concurrent
// for the lower-level UpsertStatus method (F19, #1865).
func TestUpsertStatus_Concurrent(t *testing.T) {
	const N = 50
	const session = "concurrent-upsert@main"
	d := openTestDB(t)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func(idx int) {
			defer wg.Done()
			title := fmt.Sprintf("title-%d", idx)
			err := d.UpsertStatus(
				session, "repo", "/code/repo/main", "active",
				strPtr(title),
				nil, // harnessSessionID
			)
			if err != nil {
				t.Errorf("goroutine %d: UpsertStatus: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	// Row count must be exactly 1.
	var count int
	if err := d.QueryRow(
		"SELECT COUNT(*) FROM agent_status WHERE session_name = ?", session,
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("row count: got %d, want 1", count)
	}

	// The surviving row must have a non-NULL title (COALESCE keeps first writer's value).
	s, err := d.CurrentStatus(session)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if s.Title == nil {
		t.Error("title is NULL after concurrent upserts; expected non-NULL (COALESCE semantics)")
	}
	if s.State != "active" {
		t.Errorf("state: got %q, want %q", s.State, "active")
	}
}
