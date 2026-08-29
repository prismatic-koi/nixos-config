package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ── Counting driver ──────────────────────────────────────────────────────────
//
// countingDriver wraps modernc.org/sqlite to count every QueryContext call
// observed at the database/sql ↔ driver boundary. It is used by the
// query-count invariant tests: it lets us assert that
// GroupResults and ReviewGroupsList issue a bounded number of SQL
// queries regardless of input size, regardless of whether database/sql
// dispatches via the QueryerContext fast path or via PrepareContext +
// Stmt.QueryContext.
//
// Exec / ExecContext calls are also counted (separately) so that test
// failure messages can distinguish "missed a SELECT" from "added an
// UPDATE somewhere".

type queryCounter struct {
	queries atomic.Int64
	execs   atomic.Int64
}

func (c *queryCounter) reset() {
	c.queries.Store(0)
	c.execs.Store(0)
}

// activeCounter is the counter the current registered counting driver
// reports into. It is settable per-test via withCounter so that tests
// do not see each other's traffic, but the driver itself is process-wide
// (database/sql.Register cannot be undone).
var activeCounter atomic.Pointer[queryCounter]

func currentCounter() *queryCounter {
	c := activeCounter.Load()
	if c == nil {
		return &queryCounter{} // null sink during teardown / cross-test windows
	}
	return c
}

const countingDriverName = "sqlite-counting"

var registerCountingDriverOnce sync.Once

func ensureCountingDriverRegistered() {
	registerCountingDriverOnce.Do(func() {
		// Resolve the real driver by opening a throwaway connection.
		realDB, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			panic(fmt.Sprintf("ensureCountingDriverRegistered: sql.Open(sqlite): %v", err))
		}
		realDriver := realDB.Driver()
		_ = realDB.Close()
		sql.Register(countingDriverName, &cDriver{real: realDriver})
	})
}

// withCounter installs c as the active counter for the duration of the
// test, restoring the previous counter on cleanup.
func withCounter(t *testing.T, c *queryCounter) {
	t.Helper()
	prev := activeCounter.Swap(c)
	t.Cleanup(func() { activeCounter.Store(prev) })
}

type cDriver struct{ real driver.Driver }

func (d *cDriver) Open(name string) (driver.Conn, error) {
	c, err := d.real.Open(name)
	if err != nil {
		return nil, err
	}
	return &cConn{real: c}, nil
}

// cConn wraps a sqlite driver.Conn. We implement the rich interface set
// (QueryerContext, ExecerContext, ConnPrepareContext, ConnBeginTx) so
// database/sql can use its fast paths AND so the prepared-statement path
// also flows through our wrappers.

type cConn struct{ real driver.Conn }

func (c *cConn) Prepare(query string) (driver.Stmt, error) {
	s, err := c.real.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &cStmt{real: s, query: query}, nil
}

func (c *cConn) Close() error              { return c.real.Close() }
func (c *cConn) Begin() (driver.Tx, error) { return c.real.Begin() } //nolint:staticcheck

func (c *cConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.real.(driver.ConnPrepareContext); ok {
		s, err := p.PrepareContext(ctx, query)
		if err != nil {
			return nil, err
		}
		return &cStmt{real: s, query: query}, nil
	}
	return c.Prepare(query)
}

func (c *cConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.real.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.real.Begin() //nolint:staticcheck
}

func (c *cConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	currentCounter().queries.Add(1)
	if q, ok := c.real.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, query, args)
	}
	// Fall back to prepare + query.
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	if qc, ok := stmt.(driver.StmtQueryContext); ok {
		return qc.QueryContext(ctx, args)
	}
	return nil, driver.ErrSkip
}

func (c *cConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	currentCounter().execs.Add(1)
	if e, ok := c.real.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, query, args)
	}
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	if ec, ok := stmt.(driver.StmtExecContext); ok {
		return ec.ExecContext(ctx, args)
	}
	return nil, driver.ErrSkip
}

// Ping forwards to the underlying driver when supported. modernc.org/sqlite
// implements driver.Pinger; without forwarding, database/sql falls back to a
// trivial impl that doesn't actually round-trip — fine for our purposes, but
// implementing it correctly avoids surprises.
func (c *cConn) Ping(ctx context.Context) error {
	if p, ok := c.real.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

// cStmt wraps a driver.Stmt and counts QueryContext / ExecContext invocations
// from the database/sql prepared-statement path.

type cStmt struct {
	real  driver.Stmt
	query string
}

func (s *cStmt) Close() error  { return s.real.Close() }
func (s *cStmt) NumInput() int { return s.real.NumInput() }

func (s *cStmt) Exec(args []driver.Value) (driver.Result, error) { //nolint:staticcheck
	currentCounter().execs.Add(1)
	return s.real.Exec(args) //nolint:staticcheck
}

func (s *cStmt) Query(args []driver.Value) (driver.Rows, error) { //nolint:staticcheck
	currentCounter().queries.Add(1)
	return s.real.Query(args) //nolint:staticcheck
}

func (s *cStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	currentCounter().execs.Add(1)
	if ec, ok := s.real.(driver.StmtExecContext); ok {
		return ec.ExecContext(ctx, args)
	}
	// database/sql will retry via the value-based path on ErrSkip.
	return nil, driver.ErrSkip
}

func (s *cStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	currentCounter().queries.Add(1)
	if qc, ok := s.real.(driver.StmtQueryContext); ok {
		return qc.QueryContext(ctx, args)
	}
	return nil, driver.ErrSkip
}

// ── Test helpers ─────────────────────────────────────────────────────────────

// openCountedDB opens a fresh DB using the counting driver and returns a
// *DB plus the counter. Schema / migrations are applied via openAndConfigure
// before the counter is returned, so setup traffic is not measured by the
// caller's assertions; the caller should call counter.reset() immediately
// before the operation under test if other test-setup traffic intervenes.
func openCountedDB(t *testing.T) (*DB, *queryCounter) {
	t.Helper()
	ensureCountingDriverRegistered()

	c := &queryCounter{}
	withCounter(t, c)

	path := filepath.Join(t.TempDir(), "qcount.db")
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"
	conn, err := sql.Open(countingDriverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open(counting): %v", err)
	}
	conn, err = openAndConfigure(conn)
	if err != nil {
		t.Fatalf("openAndConfigure: %v", err)
	}
	d := &DB{conn: conn, path: path}
	t.Cleanup(func() { d.Close() })
	return d, c
}

// seedGroupWithMembers registers a group, inserts `n` member sessions,
// associates each with the group, and writes one msg_assistant + one
// startup_error event per member so the batched event-fetch has data
// to reduce.
func seedGroupWithMembers(t *testing.T, d *DB, n int) (groupID string, sessions []string) {
	t.Helper()
	groupID, err := d.RegisterGroup("nixos-config@feature")
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	sessions = make([]string, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("nixos-config@feature~review-1-agent-%d", i)
		sessions[i] = name
		if err := d.UpsertStatus(name, "nixos-config", "/wt", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus %s: %v", name, err)
		}
		if err := d.QueryRow(
			"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
			groupID, name,
		).Scan(new(int)); err != nil {
			t.Fatalf("set group_id for %s: %v", name, err)
		}
		now := time.Now()
		if err := d.WriteEvent(Event{
			ID:          fmt.Sprintf("evt-msg-%d-%s", i, name),
			SessionName: name,
			Repo:        "nixos-config",
			Worktree:    "/wt",
			Type:        "msg_assistant",
			Payload:     fmt.Sprintf(`{"content":"<verdict>PASS</verdict> #%d"}`, i),
			CreatedAt:   now,
		}); err != nil {
			t.Fatalf("WriteEvent msg_assistant: %v", err)
		}
		if err := d.WriteEvent(Event{
			ID:          fmt.Sprintf("evt-startup-%d-%s", i, name),
			SessionName: name,
			Repo:        "nixos-config",
			Worktree:    "/wt",
			Type:        "startup_error",
			Payload:     fmt.Sprintf(`{"reason":"boom %d"}`, i),
			CreatedAt:   now,
		}); err != nil {
			t.Fatalf("WriteEvent startup_error: %v", err)
		}
	}
	return groupID, sessions
}

// ── F7: GroupResults query-count invariant ───────────────────────────────────

// TestGroupResults_QueryCountBounded asserts that GroupResults issues at
// most 2 SQL queries regardless of member count (F7). A per-member
// implementation would issue 2 QueryRow calls per member after the initial
// status fetch, growing linearly with the group size.
func TestGroupResults_QueryCountBounded(t *testing.T) {
	const memberCount = 8
	d, counter := openCountedDB(t)
	groupID, _ := seedGroupWithMembers(t, d, memberCount)

	counter.reset()
	results, err := d.GroupResults(groupID)
	if err != nil {
		t.Fatalf("GroupResults: %v", err)
	}
	if got := len(results); got != memberCount {
		t.Fatalf("GroupResults: got %d results, want %d", got, memberCount)
	}
	queries := counter.queries.Load()
	if queries > 2 {
		t.Fatalf("GroupResults issued %d queries for %d members; want ≤ 2 (#1868 F7)", queries, memberCount)
	}
	// Sanity: assert per-member fields are populated so we know the
	// batched fetch actually wired data through.
	for name, r := range results {
		if r.LastMessage == "" {
			t.Errorf("LastMessage for %q is empty; batched event reduction missed msg_assistant", name)
		}
		if r.StartupError == "" {
			t.Errorf("StartupError for %q is empty; batched event reduction missed startup_error", name)
		}
	}
}

// TestGroupResults_QueryCountConstantInN verifies the bound holds across two
// very different member counts — proves the cost is constant in N, not just
// "small enough at N=8".
func TestGroupResults_QueryCountConstantInN(t *testing.T) {
	for _, n := range []int{1, 16} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			d, counter := openCountedDB(t)
			groupID, _ := seedGroupWithMembers(t, d, n)
			counter.reset()
			if _, err := d.GroupResults(groupID); err != nil {
				t.Fatalf("GroupResults: %v", err)
			}
			if got := counter.queries.Load(); got > 2 {
				t.Fatalf("GroupResults issued %d queries for n=%d; want ≤ 2", got, n)
			}
		})
	}
}

// ── F8: ReviewGroupsList query-count invariant ───────────────────────────────

// TestReviewGroupsList_QueryCountBounded asserts that ReviewGroupsList
// issues at most 2 SQL queries regardless of group count (F8).
// The previous implementation called GroupMembersForGroup once per group.
func TestReviewGroupsList_QueryCountBounded(t *testing.T) {
	const groupCount = 6
	const membersPerGroup = 3
	d, counter := openCountedDB(t)

	allGroupIDs := make([]string, 0, groupCount)
	for g := 0; g < groupCount; g++ {
		parent := fmt.Sprintf("nixos-config@feat-%d", g)
		gid, err := d.RegisterGroup(parent)
		if err != nil {
			t.Fatalf("RegisterGroup: %v", err)
		}
		allGroupIDs = append(allGroupIDs, gid)
		for m := 0; m < membersPerGroup; m++ {
			name := fmt.Sprintf("%s~review-1-agent-%d", parent, m)
			if err := d.UpsertStatus(name, "nixos-config", "/wt", "finished", nil, nil); err != nil {
				t.Fatalf("UpsertStatus %s: %v", name, err)
			}
			if err := d.QueryRow(
				"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
				gid, name,
			).Scan(new(int)); err != nil {
				t.Fatalf("set group_id for %s: %v", name, err)
			}
		}
	}

	counter.reset()
	got, err := d.ReviewGroupsList(0)
	if err != nil {
		t.Fatalf("ReviewGroupsList: %v", err)
	}
	if len(got) != groupCount {
		t.Fatalf("ReviewGroupsList: got %d groups, want %d", len(got), groupCount)
	}
	for _, s := range got {
		if len(s.Members) != membersPerGroup {
			t.Errorf("group %s: got %d members, want %d", s.GroupID, len(s.Members), membersPerGroup)
		}
		if s.GroupState != "completed" {
			t.Errorf("group %s: GroupState = %q, want \"completed\"", s.GroupID, s.GroupState)
		}
	}
	if q := counter.queries.Load(); q > 2 {
		t.Fatalf("ReviewGroupsList issued %d queries for %d groups; want ≤ 2 (#1868 F8)", q, groupCount)
	}
}

// TestReviewGroupsList_QueryCountConstantInN proves the bound is constant
// in the number of groups, not "small enough at N=6".
func TestReviewGroupsList_QueryCountConstantInN(t *testing.T) {
	for _, n := range []int{1, 12} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			d, counter := openCountedDB(t)
			for g := 0; g < n; g++ {
				parent := fmt.Sprintf("nixos-config@feat-%d", g)
				gid, err := d.RegisterGroup(parent)
				if err != nil {
					t.Fatalf("RegisterGroup: %v", err)
				}
				name := fmt.Sprintf("%s~review-1-agent", parent)
				if err := d.UpsertStatus(name, "nixos-config", "/wt", "finished", nil, nil); err != nil {
					t.Fatalf("UpsertStatus: %v", err)
				}
				if err := d.QueryRow(
					"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
					gid, name,
				).Scan(new(int)); err != nil {
					t.Fatalf("set group_id: %v", err)
				}
			}
			counter.reset()
			if _, err := d.ReviewGroupsList(0); err != nil {
				t.Fatalf("ReviewGroupsList: %v", err)
			}
			if q := counter.queries.Load(); q > 2 {
				t.Fatalf("ReviewGroupsList issued %d queries for n=%d; want ≤ 2", q, n)
			}
		})
	}
}

// TestReviewGroupsList_EmptyGroupsBatched verifies the batched path produces
// the same shape as the pre-batch loop for the edge case where some groups
// have no members and some do. GroupState rolls up to "empty" only when the
// group has zero members.
func TestReviewGroupsList_EmptyGroupsBatched(t *testing.T) {
	d, _ := openCountedDB(t)

	// Group 1: empty.
	emptyParent := "nixos-config@empty"
	emptyGID, err := d.RegisterGroup(emptyParent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}

	// Group 2: one finished member.
	fullParent := "nixos-config@full"
	fullGID, err := d.RegisterGroup(fullParent)
	if err != nil {
		t.Fatalf("RegisterGroup: %v", err)
	}
	const fullSession = "nixos-config@full~review-1-agent"
	if err := d.UpsertStatus(fullSession, "nixos-config", "/wt", "finished", nil, nil); err != nil {
		t.Fatalf("UpsertStatus: %v", err)
	}
	if err := d.QueryRow(
		"UPDATE agent_status SET group_id = ? WHERE session_name = ? RETURNING 1",
		fullGID, fullSession,
	).Scan(new(int)); err != nil {
		t.Fatalf("set group_id: %v", err)
	}

	got, err := d.ReviewGroupsList(0)
	if err != nil {
		t.Fatalf("ReviewGroupsList: %v", err)
	}
	byID := map[string]ReviewGroupSummary{}
	for _, s := range got {
		byID[s.GroupID] = s
	}
	if got, want := byID[emptyGID].GroupState, "empty"; got != want {
		t.Errorf("empty group: GroupState = %q, want %q", got, want)
	}
	if got := byID[emptyGID].Members; got == nil {
		t.Errorf("empty group: Members is nil; want non-nil empty slice (shape compat)")
	} else if len(got) != 0 {
		t.Errorf("empty group: Members = %v, want empty", got)
	}
	if got, want := byID[fullGID].GroupState, "completed"; got != want {
		t.Errorf("full group: GroupState = %q, want %q", got, want)
	}
	if got, want := byID[fullGID].Members, []string{fullSession}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("full group: Members = %v, want %v", got, want)
	}
}
