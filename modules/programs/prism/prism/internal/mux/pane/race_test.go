// Concurrency stress tests. The AC on #2151 calls out -race cleanliness
// explicitly; this file holds the goroutine fan-outs that exercise the
// SessionTree mutex under load. Tests are deliberately not "stress
// benchmarks" — they fan out a moderate number of goroutines and
// exercise every mutating operation, which is enough to flush any
// missing lock acquisition that the Go race detector cares about.
//
// All operations against the tree go through the public API, so the
// tests double as a smoke check that the public surface holds together
// under contention. They never assert exact final-state values
// (interleavings make that brittle); they assert structural invariants
// after the storm via Validate.
package pane

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentAddSession fans out N goroutines that each add a
// distinct top-level session. After Wait, every session must be in
// the tree and Validate must pass. The race detector catches any
// missing lock acquisition on the SessionOrder / RepoOrder writes.
func TestConcurrentAddSession(t *testing.T) {
	tree := New()
	const n = 64

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("repo-%d@main", i%4)
			_ = tree.AddSession(Session{
				ID:   fmt.Sprintf("repo-%d@worker-%d", i%4, i),
				Repo: fmt.Sprintf("repo-%d", i%4),
			})
			_ = id // keep the formatter from eliding
		}(i)
	}
	wg.Wait()

	if got := tree.Len(); got != n {
		t.Errorf("Len after %d concurrent adds = %d, want %d", n, got, n)
	}
	if err := tree.Validate(); err != nil {
		t.Errorf("Validate after concurrent adds: %v", err)
	}
}

// TestConcurrentMixedOperations exercises every mutating operation
// against a shared tree while a second pool of goroutines reads
// continuously. The shape is intentionally chaotic — no goroutine
// owns a session ID, and several goroutines deliberately race on the
// same ID via AddSession+RemoveSession+AddPane+ActivatePane. Once the
// storm subsides, Validate must still pass.
//
// Errors from individual operations are ignored (the race may legitimately
// cause AddPane on a session that was just removed) — the point is that
// no operation panics, no operation corrupts the tree, and Validate is
// happy at the end.
func TestConcurrentMixedOperations(t *testing.T) {
	tree := New()

	// Pre-populate a fixed set of sessions so AddPane / ActivatePane /
	// NextPane have something to race on from the start.
	const baseSessions = 8
	for i := 0; i < baseSessions; i++ {
		mustAddSession(t, tree, Session{
			ID:   fmt.Sprintf("base@%d", i),
			Repo: "base",
		})
	}

	const writers = 16
	const opsPerWriter = 200
	var wg sync.WaitGroup
	wg.Add(writers)

	// Counter so each writer picks a unique-ish ID range while still
	// overlapping with neighbours to exercise duplicate-add paths.
	var counter atomic.Int64

	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for op := 0; op < opsPerWriter; op++ {
				switch op % 8 {
				case 0:
					id := fmt.Sprintf("dyn@%d", counter.Add(1)%(int64(writers)*8))
					_ = tree.AddSession(Session{ID: id, Repo: "dyn"})
				case 1:
					id := fmt.Sprintf("dyn@%d", counter.Add(1)%(int64(writers)*8))
					_ = tree.RemoveSession(id)
				case 2:
					id := fmt.Sprintf("base@%d", op%baseSessions)
					_ = tree.AddPane(id, Pane{Name: fmt.Sprintf("p-%d-%d", w, op)})
				case 3:
					id := fmt.Sprintf("base@%d", op%baseSessions)
					if names := paneNames(tree, id); len(names) > 0 {
						_ = tree.RemovePane(id, names[0])
					}
				case 4:
					id := fmt.Sprintf("base@%d", op%baseSessions)
					_, _ = tree.NextPane(id)
				case 5:
					id := fmt.Sprintf("base@%d", op%baseSessions)
					_, _ = tree.PrevPane(id)
				case 6:
					id := fmt.Sprintf("base@%d", op%baseSessions)
					_ = tree.ActivateSession(id)
				case 7:
					id := fmt.Sprintf("base@%d", op%baseSessions)
					if names := paneNames(tree, id); len(names) > 0 {
						_ = tree.ActivatePane(id, names[0])
					}
				}
			}
		}(w)
	}

	// Reader pool — keeps the read side busy so the race detector sees
	// reads concurrent with writes.
	const readers = 8
	stop := make(chan struct{})
	var rwg sync.WaitGroup
	rwg.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer rwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = tree.Sessions()
				_ = tree.Repos()
				_ = tree.Len()
				_ = tree.ActiveSessionID()
			}
		}()
	}

	wg.Wait()
	close(stop)
	rwg.Wait()

	if err := tree.Validate(); err != nil {
		t.Errorf("Validate after concurrent storm: %v", err)
	}
}

// TestConcurrentJSONMarshalDuringWrites pushes the MarshalJSON path
// under contention. The marshal goroutine holds the read lock while
// json.Marshal walks the inner state — the writers must not be able to
// mutate maps mid-walk, which would otherwise trip the runtime's
// concurrent-map-iteration detector and crash with "fatal error".
func TestConcurrentJSONMarshalDuringWrites(t *testing.T) {
	tree := New()
	const writers = 8

	stop := make(chan struct{})
	var wwg sync.WaitGroup
	wwg.Add(writers)

	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wwg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("rw-%d@%d", w, i)
				_ = tree.AddSession(Session{ID: id, Repo: fmt.Sprintf("rw-%d", w)})
				_ = tree.AddPane(id, Pane{Name: "agent"})
				_ = tree.RemoveSession(id)
				i++
			}
		}(w)
	}

	// Marshal in a tight loop on the main goroutine.
	for i := 0; i < 500; i++ {
		if _, err := tree.MarshalJSON(); err != nil {
			t.Fatalf("MarshalJSON iter %d: %v", i, err)
		}
	}
	close(stop)
	wwg.Wait()

	if err := tree.Validate(); err != nil {
		t.Errorf("Validate after marshal storm: %v", err)
	}
}

// paneNames is a test helper: snapshot the pane names of a session.
// Used by the chaotic writer pool to pick a pane name to operate on.
// Lifting it into a helper keeps the writer's switch readable.
func paneNames(tree *SessionTree, sessionID string) []string {
	s, ok := tree.Session(sessionID)
	if !ok {
		return nil
	}
	out := make([]string, len(s.Panes))
	for i, p := range s.Panes {
		out[i] = p.Name
	}
	return out
}
