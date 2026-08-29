// Package tailcursor is the reusable mechanism that lets prism publish
// Prometheus counters from a database that is pruned.
//
// # The problem it exists to solve
//
// internal/db/maintenance.go Prune deletes from agent_events,
// bus_messages, and sessions at 90 days, and the sessions delete cascades to
// spawn_outcome and spawn_inputs. Prune runs on the normal event path
// (cmd/event.go), not in a maintenance window.
//
// A counter computed as a full-table aggregate — SELECT COUNT(*) ... —
// therefore DECREASES at the prune horizon. Prometheus reads a decreasing
// counter as a process restart and compensates for it, so rate() and
// increase() return wrong numbers across that boundary, silently, with no
// error anywhere. The failure is invisible in testing because it appears
// only after 90 days of real data.
//
// # The mechanism
//
//   - Read the source table forward by its monotonic id.
//   - Accumulate into in-memory counter values.
//   - Persist the cursor AND the accumulated values, together, in one
//     atomic write.
//   - On start, load that state and resume from the cursor.
//   - Never run a full-table aggregate to produce a counter value.
//
// Prune then deletes rows that sit BEHIND the cursor and have already been
// counted, so no counter moves.
//
// # Why the cursor and the values are saved together
//
// Because they are one atomic snapshot, a crash is self-healing rather than
// lossy. A resume replays every row after the saved cursor, and the saved
// values are exactly the values that cursor produced — so the replay
// rebuilds the same total. Saving them separately would let a counter be
// restored ahead of or behind its cursor, which double-counts or drops.
package tailcursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// StateVersion is the on-disk schema version of the state file. Bump it when
// the meaning of an existing field changes; a file carrying any other
// version is treated as corrupt and the daemon re-initialises at the head of
// the source (see Store.Load).
const StateVersion = 1

// ErrNoState is returned by Store.Load when the state file does not exist.
// This is the normal first-run condition, not a fault: the caller
// initialises each tailer at the current head of its source and does NOT
// backfill history.
var ErrNoState = errors.New("tailcursor: no state file")

// CorruptError reports a state file that exists but cannot be used —
// truncated, not valid JSON, carrying an unknown version, or holding a value
// that would break the counter contract.
//
// The daemon must log this and carry on, never crash: a state file is
// operational bookkeeping, and losing it costs accumulated history, not
// availability.
type CorruptError struct {
	Path string
	Err  error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("tailcursor: corrupt state file %s: %v", e.Path, e.Err)
}

func (e *CorruptError) Unwrap() error { return e.Err }

// State is the on-disk snapshot.
//
// Cursors maps a tailer name to the id of the last record it consumed.
// Counters maps a metric name to that metric's persisted values, keyed by
// the opaque label-value encoding of internal/metrics.
type State struct {
	Version  int                           `json:"version"`
	Cursors  map[string]int64              `json:"cursors"`
	Counters map[string]map[string]float64 `json:"counters"`
}

// NewState returns an empty state at the current version.
func NewState() *State {
	return &State{
		Version:  StateVersion,
		Cursors:  make(map[string]int64),
		Counters: make(map[string]map[string]float64),
	}
}

// Cursor returns the saved cursor for name and whether one was present.
func (s *State) Cursor(name string) (int64, bool) {
	if s == nil || s.Cursors == nil {
		return 0, false
	}
	v, ok := s.Cursors[name]
	return v, ok
}

// SetCursor records the cursor for name.
func (s *State) SetCursor(name string, id int64) {
	if s.Cursors == nil {
		s.Cursors = make(map[string]int64)
	}
	s.Cursors[name] = id
}

// Store reads and writes a State at a fixed path.
type Store struct {
	path string
}

// NewStore returns a Store backed by the file at path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path returns the state-file path.
func (s *Store) Path() string { return s.path }

// Load reads the state file.
//
// It returns ErrNoState when the file is absent, and a *CorruptError when
// the file is present but unusable. Callers must handle both by
// re-initialising at the head of the source — never by exiting.
func (s *Store) Load() (*State, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoState
		}
		return nil, fmt.Errorf("tailcursor: read state file %s: %w", s.path, err)
	}
	if len(raw) == 0 {
		return nil, &CorruptError{Path: s.path, Err: errors.New("file is empty")}
	}

	var st State
	// DisallowUnknownFields is deliberately NOT set: an older binary must
	// be able to read a file written by a newer one at the same version
	// without treating an added field as corruption.
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, &CorruptError{Path: s.path, Err: err}
	}
	if st.Version != StateVersion {
		return nil, &CorruptError{
			Path: s.path,
			Err:  fmt.Errorf("state version %d, want %d", st.Version, StateVersion),
		}
	}
	if st.Cursors == nil {
		st.Cursors = make(map[string]int64)
	}
	if st.Counters == nil {
		st.Counters = make(map[string]map[string]float64)
	}
	for name, id := range st.Cursors {
		if id < 0 {
			return nil, &CorruptError{
				Path: s.path,
				Err:  fmt.Errorf("cursor %q is negative (%d)", name, id),
			}
		}
	}
	for metric, values := range st.Counters {
		for key, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, &CorruptError{
					Path: s.path,
					Err:  fmt.Errorf("counter %q key %q is not finite (%v)", metric, key, v),
				}
			}
			if v < 0 {
				return nil, &CorruptError{
					Path: s.path,
					Err:  fmt.Errorf("counter %q key %q is negative (%v)", metric, key, v),
				}
			}
		}
	}
	return &st, nil
}

// Save writes st atomically.
//
// The sequence is: write a temporary file in the same directory, fsync it,
// rename it over the target, then fsync the directory. A crash at any point
// therefore leaves either the previous complete file or the new complete
// file on disk — never a half-written one. Recovery from the previous file
// is exact, because the cursor and the counter values in it are one
// snapshot (see the package comment).
func (s *Store) Save(st *State) error {
	if st == nil {
		return errors.New("tailcursor: Save(nil)")
	}
	out := *st
	out.Version = StateVersion
	if out.Cursors == nil {
		out.Cursors = make(map[string]int64)
	}
	if out.Counters == nil {
		out.Counters = make(map[string]map[string]float64)
	}

	raw, err := json.Marshal(&out)
	if err != nil {
		return fmt.Errorf("tailcursor: marshal state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("tailcursor: create state dir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".prism-exporter-state-*.tmp")
	if err != nil {
		return fmt.Errorf("tailcursor: create temp state file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup for every failure path below. A successful
	// rename makes this a no-op.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("tailcursor: write temp state file: %w", err)
	}
	// os.CreateTemp already creates at 0600. This states the mode the file
	// must end up with, so a reader does not have to know that, and so a
	// change of temp-file helper cannot widen it silently. The state file
	// records this host's activity.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("tailcursor: chmod temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("tailcursor: fsync temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("tailcursor: close temp state file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("tailcursor: rename state file into place: %w", err)
	}
	// fsync the directory so the rename itself is durable. Without this a
	// power loss can lose the directory entry even though the file
	// contents were synced.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
