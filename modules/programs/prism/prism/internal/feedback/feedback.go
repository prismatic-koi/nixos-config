// Package feedback implements the local-first JSONL store used by
// `prism feedback`.
//
// Each entry is one JSON object on its own line; the file is append-only.
// The default location is $XDG_STATE_HOME/prism/feedback.jsonl, falling
// back to $HOME/.local/state/prism/feedback.jsonl. Callers can override
// the path entirely via the StorePath field of Store, which is what the
// test suite uses to keep the nix sandbox (HOME=/homeless-shelter) happy.
//
// The store deliberately does not depend on the prism DB: feedback is a
// local-first record by design, and avoiding the DB means
// a corrupted DB or a missing schema migration cannot prevent an operator
// from recording friction.
package feedback

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry is a single feedback record, written one-per-line in feedback.jsonl.
//
// Required fields: Timestamp, Text, Session (may be ""), PrismVersion.
// Optional fields are kept omitempty so older readers see only the fields
// they expect, and so a future schema bump can add fields without breaking
// existing files.
type Entry struct {
	Timestamp    string `json:"timestamp"`
	Text         string `json:"text"`
	Session      string `json:"session,omitempty"`
	PrismVersion string `json:"prism_version"`
	Repo         string `json:"repo,omitempty"`
	CWD          string `json:"cwd,omitempty"`
	LastCommand  string `json:"last_command,omitempty"`
}

// Store is the on-disk feedback log. The zero value is invalid; use
// NewStore to construct one.
type Store struct {
	// Path is the absolute path to feedback.jsonl. Tests pass a t.TempDir()
	// path; the production caller resolves this from $XDG_STATE_HOME via
	// DefaultPath().
	Path string
}

// NewStore constructs a Store at path. The file is not opened or created
// until the first Append/List/Prune call so the constructor is cheap and
// safe to call when the user does not yet have a writable state dir.
func NewStore(path string) *Store { return &Store{Path: path} }

// DefaultPath returns the canonical feedback.jsonl path for the current
// user, honouring $XDG_STATE_HOME first and falling back to
// $HOME/.local/state/prism/feedback.jsonl. Returns ("", error) when neither
// is discoverable — the caller (cmd/feedback.go) should surface a clear
// error rather than silently writing to an unexpected location.
//
// Order of resolution:
//
//  1. $XDG_STATE_HOME/prism/feedback.jsonl (if XDG_STATE_HOME is non-empty)
//  2. $HOME/.local/state/prism/feedback.jsonl (if HOME is non-empty)
//
// Reading $XDG_STATE_HOME first matters for the nix-build sandbox where
// HOME=/homeless-shelter is intentionally unwritable; tests override
// XDG_STATE_HOME to a writable t.TempDir() to avoid that path entirely.
func DefaultPath() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "prism", "feedback.jsonl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("feedback: cannot resolve store path — neither XDG_STATE_HOME nor HOME is set")
	}
	return filepath.Join(home, ".local", "state", "prism", "feedback.jsonl"), nil
}

// Append writes one entry as a JSONL line. The parent directory is created
// on demand. Returns the path it wrote to so the caller can include it in
// the user-facing confirmation message (matching the cleanup-style "wrote
// N entries to <path>" pattern the existing CLI uses).
func (s *Store) Append(e Entry) error {
	if s.Path == "" {
		return errors.New("feedback: Store.Path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fmt.Errorf("feedback: create parent dir: %w", err)
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("feedback: open store: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	// json.Encoder.Encode appends a newline, which is exactly the JSONL
	// framing we want.
	if err := enc.Encode(e); err != nil {
		return fmt.Errorf("feedback: encode entry: %w", err)
	}
	return nil
}

// List reads all entries from the store. Returns an empty slice (not nil
// error) when the file does not yet exist — `prism feedback list` on a
// fresh machine should print nothing without an error.
//
// Malformed lines are skipped with a non-fatal warning to stderr; one
// corrupt line shouldn't render the whole log unreadable.
func (s *Store) List() ([]Entry, error) {
	if s.Path == "" {
		return nil, errors.New("feedback: Store.Path is empty")
	}
	f, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("feedback: open store: %w", err)
	}
	defer f.Close()
	return readEntries(f)
}

// readEntries parses one Entry per line, skipping blanks and JSON-parse
// failures. Exposed as a package-level helper so Prune can re-use it
// without re-opening the file.
func readEntries(r io.Reader) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	// Allow long lines — feedback text plus context fields can exceed the
	// default 64 KiB buffer.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			fmt.Fprintf(os.Stderr, "feedback: skipping malformed line: %v\n", err)
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("feedback: read store: %w", err)
	}
	return out, nil
}

// FilterSince returns the subset of entries whose Timestamp is at or after
// cutoff. Entries with an unparseable Timestamp are kept (defence-in-depth:
// if we can't tell whether a record is too old, we don't drop it).
func FilterSince(entries []Entry, cutoff time.Time) []Entry {
	out := entries[:0:0]
	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			out = append(out, e)
			continue
		}
		if !t.Before(cutoff) {
			out = append(out, e)
		}
	}
	return out
}

// Prune removes entries with Timestamp older than cutoff. Returns the
// number of entries kept, the number removed, and any error. The on-disk
// file is rewritten atomically (tempfile + rename) so a crash mid-prune
// cannot corrupt the store.
func (s *Store) Prune(cutoff time.Time) (kept, removed int, err error) {
	if s.Path == "" {
		return 0, 0, errors.New("feedback: Store.Path is empty")
	}
	all, err := s.List()
	if err != nil {
		return 0, 0, err
	}
	if len(all) == 0 {
		return 0, 0, nil
	}
	keepers := FilterSince(all, cutoff)
	removed = len(all) - len(keepers)
	if removed == 0 {
		return len(all), 0, nil
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, 0, fmt.Errorf("feedback: create parent dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".feedback-prune-*.tmp")
	if err != nil {
		return 0, 0, fmt.Errorf("feedback: create tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	enc := json.NewEncoder(tmp)
	for _, e := range keepers {
		if err := enc.Encode(e); err != nil {
			_ = tmp.Close()
			return 0, 0, fmt.Errorf("feedback: encode kept entry: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		return 0, 0, fmt.Errorf("feedback: close tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, s.Path); err != nil {
		return 0, 0, fmt.Errorf("feedback: rename: %w", err)
	}
	cleanup = false
	return len(keepers), removed, nil
}
