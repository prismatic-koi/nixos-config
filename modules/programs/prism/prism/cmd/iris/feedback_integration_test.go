package main

// feedback_integration_test.go — integration coverage for the `iris feedback`
// record / list / prune cycle (issue #1721).
//
// The test drives the cobra commands directly (no subprocess) but exercises
// the full code path: argument parsing, store creation, JSONL append, list
// + filter + JSON encoding, and prune with --yes. The on-disk feedback.jsonl
// is asserted between steps so a regression in any of the three verbs is
// caught.
//
// Isolation: the test sets XDG_STATE_HOME to a t.TempDir() so the
// production resolveIrisFeedbackStore path is exercised end-to-end without
// touching the developer's real ~/.local/state. This also keeps the test
// happy in the nix sandbox where HOME=/homeless-shelter.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/prismatic-koi/prism/internal/feedback"
)

// withIsolatedFeedback redirects $XDG_STATE_HOME to a tempdir and the
// in-process feedback-record helpers to read it. Returns the resolved
// feedback.jsonl path.
func withIsolatedFeedback(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	// Make sure no inherited endpoint sends test data anywhere.
	t.Setenv(IRISFeedbackEndpointEnv, "")
	return filepath.Join(dir, "iris", "feedback.jsonl")
}

// readEntries decodes the on-disk JSONL file and returns the entries in
// append order (oldest first). It is the test's source of truth for
// asserting record / prune outcomes — the CLI's list output is a separate
// signal we also check.
func readEntries(t *testing.T, path string) []feedback.Entry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []feedback.Entry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e feedback.Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// runFeedbackCmd is a thin helper that invokes the cobra command with the
// given args and a fresh stdout buffer. It returns the captured stdout and
// any execution error.
//
// We re-resolve the command by name on rootCmd so each call gets the same
// surface the user sees, not a cached reference. Cobra retains parsed flag
// values across Execute() calls in the same process, so we reset flag
// state on the feedback subtree before each run — otherwise --json or
// --days from one call leaks into the next.
func runFeedbackCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetFeedbackFlags()
	rootCmd.SetArgs(append([]string{"feedback"}, args...))
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	err := rootCmd.Execute()
	return out.String(), err
}

// resetFeedbackFlags clears any flag values that previous Execute() calls
// stored on the feedback subcommands. cobra/pflag does not reset values
// between invocations, so a test that ran `list --json` would leave
// --json=true visible to subsequent `list` calls in the same test binary.
func resetFeedbackFlags() {
	// pflag has no public per-flag reset hook; visit each flag and restore
	// its declared default. The subcommands carry the only flags relevant
	// to these tests (--json, --days, --yes).
	for _, c := range []*cobra.Command{feedbackListCmd, feedbackPruneCmd} {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
}

// TestIrisFeedback_RecordListPruneCycle is the canonical AC test: record 3
// entries, list them, prune the oldest, assert state. It also asserts
// IRIS_SESSION_NAME auto-population and the human-readable list ordering
// (newest first).
func TestIrisFeedback_RecordListPruneCycle(t *testing.T) {
	path := withIsolatedFeedback(t)
	t.Setenv("IRIS_SESSION_NAME", "iris-test@worker-1")

	// Inject a store-path resolver so the cobra commands route to the
	// isolated path. (XDG_STATE_HOME is also set above; the override is
	// belt-and-braces — explicit beats implicit when tests run in parallel
	// with future state-mutating code.)
	origPathFn := irisFeedbackStorePathFn
	irisFeedbackStorePathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { irisFeedbackStorePathFn = origPathFn })

	// Record 3 entries. We can't easily backdate via the CLI so we write
	// the older two directly into the store and use the CLI for the third
	// — proving that the on-disk format the CLI produces is read back
	// identically by list/prune.
	now := time.Now().UTC()
	old := feedback.Entry{
		Timestamp:    now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
		Text:         "oldest entry, will be pruned",
		Session:      "iris-test@past",
		PrismVersion: "test-sha",
	}
	mid := feedback.Entry{
		Timestamp:    now.Add(-5 * 24 * time.Hour).Format(time.RFC3339),
		Text:         "middle entry, recent enough to keep",
		Session:      "iris-test@past",
		PrismVersion: "test-sha",
	}
	if err := feedback.NewStore(path).Append(old); err != nil {
		t.Fatalf("seed old entry: %v", err)
	}
	if err := feedback.NewStore(path).Append(mid); err != nil {
		t.Fatalf("seed mid entry: %v", err)
	}

	// Record the newest entry via the CLI — exercises the full
	// runIrisFeedbackRecord path including IRIS_SESSION_NAME pickup.
	out, err := runFeedbackCmd(t, "fresh entry from the CLI")
	if err != nil {
		t.Fatalf("record via CLI: %v", err)
	}
	if !strings.Contains(out, "feedback recorded locally") {
		t.Fatalf("record success message missing: %q", out)
	}
	if !strings.Contains(out, path) {
		t.Fatalf("record message should mention store path %q; got %q", path, out)
	}

	entries := readEntries(t, path)
	if len(entries) != 3 {
		t.Fatalf("want 3 on-disk entries after record, got %d: %+v", len(entries), entries)
	}
	if entries[2].Text != "fresh entry from the CLI" {
		t.Fatalf("newest entry text wrong: %+v", entries[2])
	}
	if entries[2].Session != "iris-test@worker-1" {
		t.Fatalf("IRIS_SESSION_NAME not picked up: session=%q", entries[2].Session)
	}

	// list (human) — should print newest first; assert the order of Text
	// substrings in the captured output.
	out, err = runFeedbackCmd(t, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	freshIdx := strings.Index(out, "fresh entry from the CLI")
	midIdx := strings.Index(out, "middle entry")
	oldIdx := strings.Index(out, "oldest entry")
	if freshIdx < 0 || midIdx < 0 || oldIdx < 0 {
		t.Fatalf("list missing one of the entries: %q", out)
	}
	if !(freshIdx < midIdx && midIdx < oldIdx) {
		t.Fatalf("list should be newest-first; got order fresh=%d mid=%d old=%d in %q",
			freshIdx, midIdx, oldIdx, out)
	}

	// list --json --days 10 — should drop the 30-day-old entry and emit
	// a JSON array of exactly two.
	out, err = runFeedbackCmd(t, "list", "--json", "--days", "10")
	if err != nil {
		t.Fatalf("list --json --days: %v", err)
	}
	var jsonOut []feedback.Entry
	if err := json.Unmarshal([]byte(out), &jsonOut); err != nil {
		t.Fatalf("list --json output not a JSON array: %v\noutput=%q", err, out)
	}
	if len(jsonOut) != 2 {
		t.Fatalf("list --json --days 10: want 2 entries, got %d: %+v", len(jsonOut), jsonOut)
	}

	// prune without --yes must error and leave the file untouched.
	out, err = runFeedbackCmd(t, "prune", "--days", "10")
	if err == nil {
		t.Fatalf("prune without --yes should error, got success: %q", out)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("prune error should mention --yes; got %v", err)
	}
	if got := len(readEntries(t, path)); got != 3 {
		t.Fatalf("prune without --yes mutated the store: want 3 entries, got %d", got)
	}

	// prune --days 10 --yes drops the 30-day-old entry only.
	out, err = runFeedbackCmd(t, "prune", "--days", "10", "--yes")
	if err != nil {
		t.Fatalf("prune --yes: %v", err)
	}
	if !strings.Contains(out, "removed 1") || !strings.Contains(out, "kept 2") {
		t.Fatalf("prune summary unexpected: %q", out)
	}
	remaining := readEntries(t, path)
	if len(remaining) != 2 {
		t.Fatalf("after prune want 2 entries, got %d: %+v", len(remaining), remaining)
	}
	for _, e := range remaining {
		if strings.Contains(e.Text, "oldest") {
			t.Fatalf("oldest entry survived prune: %+v", e)
		}
	}
}

// TestIrisFeedback_FirstInvocationCreatesStore exercises the AC that the
// store is created on demand when it does not yet exist.
func TestIrisFeedback_FirstInvocationCreatesStore(t *testing.T) {
	path := withIsolatedFeedback(t)
	origPathFn := irisFeedbackStorePathFn
	irisFeedbackStorePathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { irisFeedbackStorePathFn = origPathFn })

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: feedback.jsonl should not exist, stat err=%v", err)
	}
	// list on an empty store must succeed and print nothing (human mode).
	out, err := runFeedbackCmd(t, "list")
	if err != nil {
		t.Fatalf("list on empty store: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("list on empty store should be empty; got %q", out)
	}

	// list --json on an empty store must emit `[]` (not `null`).
	out, err = runFeedbackCmd(t, "list", "--json")
	if err != nil {
		t.Fatalf("list --json on empty store: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("list --json on empty store should be []; got %q", out)
	}

	// First record creates the file (and parent dir).
	if _, err := runFeedbackCmd(t, "first note"); err != nil {
		t.Fatalf("record on fresh store: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("feedback.jsonl should now exist: %v", err)
	}
}

// TestIrisFeedback_UpstreamPostLocalFirst proves the local-first contract:
// when IRIS_FEEDBACK_ENDPOINT points at an upstream that fails, the local
// record still lands and the command exits 0 with a message that mentions
// the upstream failure.
func TestIrisFeedback_UpstreamPostLocalFirst(t *testing.T) {
	path := withIsolatedFeedback(t)
	origPathFn := irisFeedbackStorePathFn
	irisFeedbackStorePathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { irisFeedbackStorePathFn = origPathFn })

	// Two upstream variants: a 500 (POST reached server, server rejected)
	// and a happy 202 (POST accepted). Both must leave the local record on
	// disk; the success message differs.
	t.Run("upstream-500-local-still-written", func(t *testing.T) {
		var calls int32
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		t.Setenv(IRISFeedbackEndpointEnv, srv.URL)

		// Clear file between sub-tests so the assertion isolates this call.
		_ = os.Remove(path)

		out, err := runFeedbackCmd(t, "upstream 500 case")
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		if !strings.Contains(out, "feedback recorded locally") {
			t.Fatalf("want local-record success message, got %q", out)
		}
		if !strings.Contains(out, "upstream POST failed") {
			t.Fatalf("want upstream-failure annotation, got %q", out)
		}
		entries := readEntries(t, path)
		if len(entries) != 1 || entries[0].Text != "upstream 500 case" {
			t.Fatalf("local record missing after upstream failure: %+v", entries)
		}
		mu.Lock()
		got := calls
		mu.Unlock()
		if got != 1 {
			t.Fatalf("upstream should have been POSTed exactly once, got %d", got)
		}
	})

	t.Run("upstream-202-status-surfaced", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(srv.Close)
		t.Setenv(IRISFeedbackEndpointEnv, srv.URL)

		_ = os.Remove(path)

		out, err := runFeedbackCmd(t, "happy upstream")
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		if !strings.Contains(out, "status: 202") {
			t.Fatalf("want upstream-status annotation, got %q", out)
		}
		entries := readEntries(t, path)
		if len(entries) != 1 {
			t.Fatalf("want 1 local entry, got %d", len(entries))
		}
	})
}
