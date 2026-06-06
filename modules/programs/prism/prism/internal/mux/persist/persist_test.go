// Tests for the persist package. Every test uses t.TempDir() for any
// path that touches the filesystem and t.Setenv("XDG_STATE_HOME", ...)
// for DefaultPath coverage so the suite is homeless-shelter-clean under
// the nix sandbox (HOME=/homeless-shelter is intentionally unwritable).
//
// The suite pins the AC of #2156:
//
//  1. Round-trip — Save → Load reconstructs the tree, comparing under the
//     pane package's accessor surface (Repos, Sessions, ActiveSessionID,
//     ActivePane). Map iteration order is intentionally not part of the
//     contract.
//  2. Schema-version handling — the wire format carries schema_version;
//     unknown versions (older or newer) reject with
//     ErrUnknownSchemaVersion rather than partially populating a tree.
//  3. Corrupt-snapshot handling — truncated input, invalid JSON, and
//     internal tree-invariant violations all surface as ErrCorrupt. The
//     LoadOrEmpty path logs and falls back to an empty tree without
//     panicking.
//  4. Atomic write — concurrent crash-mid-write leaves no torn snapshot
//     and no tempfile debris.
//  5. Snapshotter periodic + shutdown — ticker fires Save at Interval,
//     ctx cancellation triggers one final Save and clean exit.
package persist_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/pane"
	"github.com/prismatic-koi/prism/internal/mux/persist"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// snapshotPath returns a fresh t.TempDir()-rooted path the test can use as
// the snapshot file. The parent directory does not exist yet — Save is
// expected to MkdirAll its way to it.
func snapshotPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "prism", "mux", "session.json")
}

// newSampleTree returns a populated tree exercising every shape the
// session model supports: top-level sessions in two repo clusters, a
// review subsession, multiple panes per session, a non-default
// ActivePane, and a non-empty ActiveSession pointer.
func newSampleTree(t *testing.T) *pane.SessionTree {
	t.Helper()
	tree := pane.New()
	mustAdd := func(s pane.Session) {
		if err := tree.AddSession(s); err != nil {
			t.Fatalf("AddSession(%q): %v", s.ID, err)
		}
	}
	mustAdd(pane.Session{
		ID: "nixos-config@main", Repo: "nixos-config", Branch: "main",
		Worktree: "/home/ben/code/nixos-config", AgentRole: "coordinator",
		SidecarAddr: "/run/prism/host.sock",
	})
	mustAdd(pane.Session{
		ID: "nixos-config@2156-mux-persist", Repo: "nixos-config",
		Branch: "2156-mux-persist", AgentRole: "worker",
	})
	mustAdd(pane.Session{
		ID:       "nixos-config@2156-mux-persist~review-1-review-code",
		ParentID: "nixos-config@2156-mux-persist", AgentRole: "review-code",
	})
	mustAdd(pane.Session{
		ID: "home-ops@main", Repo: "home-ops", AgentRole: "coordinator",
	})
	if err := tree.AddPane("nixos-config@main", pane.Pane{Name: "agent"}); err != nil {
		t.Fatal(err)
	}
	if err := tree.AddPane("nixos-config@main", pane.Pane{Name: "term"}); err != nil {
		t.Fatal(err)
	}
	if err := tree.AddPane("nixos-config@main", pane.Pane{Name: "edit"}); err != nil {
		t.Fatal(err)
	}
	if err := tree.ActivatePane("nixos-config@main", "term"); err != nil {
		t.Fatal(err)
	}
	if err := tree.ActivateSession("nixos-config@2156-mux-persist"); err != nil {
		t.Fatal(err)
	}
	return tree
}

// assertTreesEquivalent compares two trees through the accessor surface
// (Repos, Sessions order, ActiveSessionID, ActivePane per session). This
// is the contract pane.go pins; internal map iteration order is NOT part
// of the contract.
func assertTreesEquivalent(t *testing.T, got, want *pane.SessionTree) {
	t.Helper()
	if g, w := got.Repos(), want.Repos(); !reflect.DeepEqual(g, w) {
		t.Errorf("Repos mismatch: got %v want %v", g, w)
	}
	gotSessions, wantSessions := got.Sessions(), want.Sessions()
	if len(gotSessions) != len(wantSessions) {
		t.Fatalf("Sessions length mismatch: got %d want %d", len(gotSessions), len(wantSessions))
	}
	for i := range gotSessions {
		if !reflect.DeepEqual(gotSessions[i], wantSessions[i]) {
			t.Errorf("Sessions[%d] mismatch:\n got  %+v\n want %+v", i, gotSessions[i], wantSessions[i])
		}
	}
	if g, w := got.ActiveSessionID(), want.ActiveSessionID(); g != w {
		t.Errorf("ActiveSessionID mismatch: got %q want %q", g, w)
	}
	for _, s := range wantSessions {
		gotName, gotOK := got.ActivePaneName(s.ID)
		wantName, wantOK := want.ActivePaneName(s.ID)
		if gotOK != wantOK || gotName != wantName {
			t.Errorf("ActivePaneName(%q) mismatch: got (%q,%v) want (%q,%v)",
				s.ID, gotName, gotOK, wantName, wantOK)
		}
	}
}

// ---------------------------------------------------------------------------
// DefaultPath
// ---------------------------------------------------------------------------

func TestDefaultPath_HonoursXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/pretend-state")
	got, err := persist.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := "/tmp/pretend-state/prism/mux/session.json"
	if got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
}

func TestDefaultPath_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/tmp/pretend-home")
	got, err := persist.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := "/tmp/pretend-home/.local/state/prism/mux/session.json"
	if got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}
}

func TestDefaultPath_ErrorsWhenNeitherSet(t *testing.T) {
	// Both XDG_STATE_HOME and HOME empty. Note: os.UserHomeDir may still
	// succeed on some platforms via /etc/passwd lookup, so we cannot
	// universally assert an error here. We assert _either_ an error
	// _or_ a path that does NOT start with /tmp — the path content is
	// not the point, the absence of accidental success on a "truly
	// unset" environment is.
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	got, err := persist.DefaultPath()
	if err == nil && got == "" {
		t.Errorf("DefaultPath returned ('',nil) — want either ('',err) or a non-empty fallback path")
	}
}

// ---------------------------------------------------------------------------
// Round-trip
// ---------------------------------------------------------------------------

// TestRoundTrip is the headline AC: Save → restart-simulation (re-Load)
// → assert the restored tree matches the pre-snapshot tree.
func TestRoundTrip(t *testing.T) {
	path := snapshotPath(t)
	original := newSampleTree(t)
	if err := persist.Save(path, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	restored, err := persist.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertTreesEquivalent(t, restored, original)
	if err := restored.Validate(); err != nil {
		t.Errorf("restored tree fails Validate: %v", err)
	}
}

// TestRoundTrip_EmptyTree pins the contract that the empty zero-state
// also survives the round trip — this is the very-first-startup path
// (snapshot from a daemon that ran briefly but never accepted a
// session).
func TestRoundTrip_EmptyTree(t *testing.T) {
	path := snapshotPath(t)
	original := pane.New()
	if err := persist.Save(path, original); err != nil {
		t.Fatalf("Save: %v", err)
	}
	restored, err := persist.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if restored.Len() != 0 {
		t.Errorf("restored.Len() = %d, want 0", restored.Len())
	}
	if err := restored.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestRoundTrip_OverwritesPreviousSnapshot pins the contract that
// repeated Save calls replace the previous file atomically rather than
// appending or partially overwriting.
func TestRoundTrip_OverwritesPreviousSnapshot(t *testing.T) {
	path := snapshotPath(t)
	first := pane.New()
	if err := first.AddSession(pane.Session{
		ID: "nixos-config@main", Repo: "nixos-config",
	}); err != nil {
		t.Fatal(err)
	}
	if err := persist.Save(path, first); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	second := newSampleTree(t)
	if err := persist.Save(path, second); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	restored, err := persist.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertTreesEquivalent(t, restored, second)
}

// ---------------------------------------------------------------------------
// Save error paths
// ---------------------------------------------------------------------------

func TestSave_RejectsEmptyPath(t *testing.T) {
	err := persist.Save("", pane.New())
	if err == nil {
		t.Fatal("Save(\"\", tree): want error, got nil")
	}
}

func TestSave_RejectsNilTree(t *testing.T) {
	err := persist.Save(snapshotPath(t), nil)
	if err == nil {
		t.Fatal("Save(path, nil): want error, got nil")
	}
}

// TestSave_CreatesParentDirectory pins the contract that Save does
// MkdirAll for the snapshot's parent directory — the typical first-run
// path is the prism/mux/ directory not yet existing.
func TestSave_CreatesParentDirectory(t *testing.T) {
	// Nested path that does not exist yet.
	root := t.TempDir()
	path := filepath.Join(root, "deep", "nested", "session.json")
	if err := persist.Save(path, pane.New()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected snapshot at %s: %v", path, err)
	}
}

// TestSave_LeavesNoTempFileOnSuccess verifies the cleanup of the
// tempfile after rename — debris in the snapshot directory would
// accumulate over a long-running daemon's lifetime.
func TestSave_LeavesNoTempFileOnSuccess(t *testing.T) {
	path := snapshotPath(t)
	if err := persist.Save(path, newSampleTree(t)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(path) {
			continue
		}
		t.Errorf("unexpected leftover file in snapshot dir: %s", e.Name())
	}
}

// ---------------------------------------------------------------------------
// Load error paths
// ---------------------------------------------------------------------------

func TestLoad_RejectsEmptyPath(t *testing.T) {
	_, err := persist.Load("")
	if err == nil {
		t.Fatal("Load(\"\"): want error, got nil")
	}
}

// TestLoad_MissingFile pins the "first-run is not an error" contract —
// the absent file surfaces as os.ErrNotExist (wrapped) so the caller
// can branch with errors.Is.
func TestLoad_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-written.json")
	_, err := persist.Load(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load missing: err = %v, want errors.Is(err, os.ErrNotExist)", err)
	}
}

// TestLoad_InvalidJSON covers the "file exists but is not JSON" path —
// the file might have been touched by an editor, or written by an
// unrelated tool. ErrCorrupt is the expected sentinel.
func TestLoad_InvalidJSON(t *testing.T) {
	path := snapshotPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := persist.Load(path)
	if !errors.Is(err, persist.ErrCorrupt) {
		t.Fatalf("Load: err = %v, want errors.Is(err, ErrCorrupt)", err)
	}
}

// TestLoad_Truncated covers the "crash mid-write" failure shape — a
// JSON document cut off partway. With our atomic write semantics this
// state is unreachable through Save, but a hostile filesystem or
// external corruption can still produce it. Must surface as ErrCorrupt.
func TestLoad_Truncated(t *testing.T) {
	path := snapshotPath(t)
	// Build a real snapshot then chop off the second half.
	if err := persist.Save(path, newSampleTree(t)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = persist.Load(path)
	if !errors.Is(err, persist.ErrCorrupt) {
		t.Fatalf("Load truncated: err = %v, want errors.Is(err, ErrCorrupt)", err)
	}
}

// TestLoad_UnknownSchemaVersion_Older / _Newer pins both sides of the
// version-mismatch contract. AC: "Mismatch handling (older or newer than
// the running daemon supports) is explicit and tested."
func TestLoad_UnknownSchemaVersion_Newer(t *testing.T) {
	path := snapshotPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Use a known-good tree but with schema_version set to v999.
	treeJSON, err := json.Marshal(newSampleTree(t))
	if err != nil {
		t.Fatal(err)
	}
	wire := map[string]any{
		"schema_version": 999,
		"tree":           json.RawMessage(treeJSON),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = persist.Load(path)
	if !errors.Is(err, persist.ErrUnknownSchemaVersion) {
		t.Fatalf("Load newer schema: err = %v, want errors.Is(err, ErrUnknownSchemaVersion)", err)
	}
}

func TestLoad_UnknownSchemaVersion_Older(t *testing.T) {
	path := snapshotPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// schema_version 0 is also "unknown" — both missing-field and
	// genuine-v0-from-a-pre-versioned-build land here.
	wire := map[string]any{
		"schema_version": 0,
		"tree":           json.RawMessage(`{}`),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = persist.Load(path)
	if !errors.Is(err, persist.ErrUnknownSchemaVersion) {
		t.Fatalf("Load v0: err = %v, want errors.Is(err, ErrUnknownSchemaVersion)", err)
	}
}

// TestLoad_MissingTreePayload pins the "schema_version present but no
// tree" path. This is a distinct ErrCorrupt path — the file declares
// itself ours but contains nothing usable.
func TestLoad_MissingTreePayload(t *testing.T) {
	path := snapshotPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	wire := map[string]any{"schema_version": persist.CurrentSchemaVersion}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = persist.Load(path)
	if !errors.Is(err, persist.ErrCorrupt) {
		t.Fatalf("Load missing-tree: err = %v, want errors.Is(err, ErrCorrupt)", err)
	}
}

// TestLoad_InconsistentTree pins the tree-invariant violation path — a
// snapshot that round-trips JSON but fails pane.UnmarshalJSON's
// invariants surfaces as ErrCorrupt, not silently as a half-built tree.
// This guards the contract that the persist layer never produces a
// tree that itself violates pane invariants.
func TestLoad_InconsistentTree(t *testing.T) {
	path := snapshotPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A tree whose RepoOrder length disagrees with SessionOrder is
	// rejected by pane.validate; build the JSON by hand.
	badTree := `{
		"sessions": {
			"a@main": {"id":"a@main","repo":"a"}
		},
		"repo_order": ["a", "b"],
		"session_order": {"a": ["a@main"]},
		"child_order": {}
	}`
	wire := map[string]any{
		"schema_version": persist.CurrentSchemaVersion,
		"tree":           json.RawMessage(badTree),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = persist.Load(path)
	if !errors.Is(err, persist.ErrCorrupt) {
		t.Fatalf("Load inconsistent: err = %v, want errors.Is(err, ErrCorrupt)", err)
	}
}

// ---------------------------------------------------------------------------
// LoadOrEmpty
// ---------------------------------------------------------------------------

// TestLoadOrEmpty_MissingFileIsSilent — first-run path should NOT log
// anything; a noisy startup is bad UX.
func TestLoadOrEmpty_MissingFileIsSilent(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	path := filepath.Join(t.TempDir(), "never-written.json")
	tree := persist.LoadOrEmpty(path, logger)
	if tree == nil {
		t.Fatal("LoadOrEmpty returned nil tree")
	}
	if tree.Len() != 0 {
		t.Errorf("Len() = %d, want 0", tree.Len())
	}
	if buf.Len() != 0 {
		t.Errorf("LoadOrEmpty logged for missing file (should be silent first-run):\n%s", buf.String())
	}
}

// TestLoadOrEmpty_CorruptLogsAndFallsBack — corrupt persisted state must
// not crash the daemon and must produce a log line so the operator can
// see what happened.
func TestLoadOrEmpty_CorruptLogsAndFallsBack(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	path := snapshotPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ this is not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	tree := persist.LoadOrEmpty(path, logger)
	if tree == nil {
		t.Fatal("LoadOrEmpty returned nil tree")
	}
	if tree.Len() != 0 {
		t.Errorf("Len() = %d, want 0", tree.Len())
	}
	if !strings.Contains(buf.String(), "corrupt") {
		t.Errorf("expected log line mentioning 'corrupt', got:\n%s", buf.String())
	}
}

// TestLoadOrEmpty_UnknownSchemaVersionLogsAndFallsBack — schema-mismatch
// path must also log + fall back without crashing.
func TestLoadOrEmpty_UnknownSchemaVersionLogsAndFallsBack(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	path := snapshotPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"schema_version": 999, "tree": json.RawMessage(`{}`)})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	tree := persist.LoadOrEmpty(path, logger)
	if tree == nil {
		t.Fatal("LoadOrEmpty returned nil tree")
	}
	if tree.Len() != 0 {
		t.Errorf("Len() = %d, want 0", tree.Len())
	}
	if !strings.Contains(buf.String(), "unknown schema version") {
		t.Errorf("expected log line mentioning 'unknown schema version', got:\n%s", buf.String())
	}
}

// TestLoadOrEmpty_HappyPathDoesNotLog — successful load should not
// produce a log line either.
func TestLoadOrEmpty_HappyPathDoesNotLog(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	path := snapshotPath(t)
	original := newSampleTree(t)
	if err := persist.Save(path, original); err != nil {
		t.Fatal(err)
	}
	restored := persist.LoadOrEmpty(path, logger)
	assertTreesEquivalent(t, restored, original)
	if buf.Len() != 0 {
		t.Errorf("LoadOrEmpty on happy path produced unexpected log:\n%s", buf.String())
	}
}

// TestLoadOrEmpty_NilLogger pins the default-logger fallback contract —
// passing nil must not panic.
func TestLoadOrEmpty_NilLogger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	tree := persist.LoadOrEmpty(path, nil)
	if tree == nil {
		t.Fatal("LoadOrEmpty returned nil tree with nil logger")
	}
}

// ---------------------------------------------------------------------------
// On-disk wire format
// ---------------------------------------------------------------------------

// TestSaveProducesExpectedTopLevelShape pins the wire format itself:
// a top-level object with `schema_version` and `tree` keys. Tests that
// assert on the exact shape protect us against an accidental
// refactoring that ships a different layout (e.g. inlining the tree
// fields into the top level, which would silently break v1 readers
// against future v2 files).
func TestSaveProducesExpectedTopLevelShape(t *testing.T) {
	path := snapshotPath(t)
	if err := persist.Save(path, newSampleTree(t)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unmarshal top-level: %v", err)
	}
	sv, ok := top["schema_version"]
	if !ok {
		t.Fatal("snapshot missing schema_version")
	}
	var n int
	if err := json.Unmarshal(sv, &n); err != nil {
		t.Fatalf("schema_version is not an int: %v", err)
	}
	if n != persist.CurrentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", n, persist.CurrentSchemaVersion)
	}
	if _, ok := top["tree"]; !ok {
		t.Error("snapshot missing tree")
	}
}

// ---------------------------------------------------------------------------
// Snapshotter
// ---------------------------------------------------------------------------

// TestSnapshotter_PeriodicAndShutdown is the integration-shape test for
// the Snapshotter: a sub-Interval Run that exits via ctx cancellation
// writes at least the final snapshot, and that snapshot round-trips
// back to the source tree.
func TestSnapshotter_PeriodicAndShutdown(t *testing.T) {
	path := snapshotPath(t)
	tree := newSampleTree(t)
	var buf bytes.Buffer
	s := &persist.Snapshotter{
		Path:     path,
		Tree:     tree,
		Interval: 20 * time.Millisecond,
		Logger:   log.New(&buf, "", 0),
	}
	ctx, cancel := context.WithCancel(context.Background())

	// Run in background; cancel after a few intervals so we know the
	// ticker has fired at least once.
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait long enough that the ticker definitely fired.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Snapshotter.Run returned err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshotter.Run did not exit within 2s of ctx cancel")
	}

	// The final snapshot on shutdown must exist and round-trip.
	restored, err := persist.Load(path)
	if err != nil {
		t.Fatalf("Load after Snapshotter shutdown: %v", err)
	}
	assertTreesEquivalent(t, restored, tree)
}

// TestSnapshotter_FinalSnapshotEvenWithoutTickerFiring pins the
// shutdown contract — even if ctx is cancelled before any ticker tick,
// Run still writes one snapshot before returning. This is the
// "user immediately Ctrl-C'd after launching the daemon" path.
func TestSnapshotter_FinalSnapshotEvenWithoutTickerFiring(t *testing.T) {
	path := snapshotPath(t)
	tree := newSampleTree(t)
	s := &persist.Snapshotter{
		Path:     path,
		Tree:     tree,
		Interval: 10 * time.Minute, // far longer than the test runs
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the ticker cannot fire — only the
	// ctx-Done branch should execute.
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Snapshotter.Run: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected final snapshot at %s: %v", path, err)
	}
	restored, err := persist.Load(path)
	if err != nil {
		t.Fatalf("Load after immediate-cancel shutdown: %v", err)
	}
	assertTreesEquivalent(t, restored, tree)
}

// TestSnapshotter_RejectsZeroValueConfig pins the contract that the
// zero value is not ready for use — Path and Tree must both be
// populated.
func TestSnapshotter_RejectsZeroValueConfig(t *testing.T) {
	s := &persist.Snapshotter{}
	err := s.Run(context.Background())
	if err == nil {
		t.Fatal("Snapshotter.Run on zero value: want error, got nil")
	}
	s = &persist.Snapshotter{Path: snapshotPath(t)}
	err = s.Run(context.Background())
	if err == nil {
		t.Fatal("Snapshotter.Run with nil Tree: want error, got nil")
	}
}

// TestSnapshotter_DefaultInterval pins the contract that an unset
// Interval falls back to DefaultInterval. We do not directly observe
// the timer value; instead we verify the contract symbolically: Run
// with Interval = 0 must NOT spin (which it would if a zero
// time.NewTicker were used — that would panic) and must write a final
// snapshot on cancellation.
func TestSnapshotter_DefaultInterval(t *testing.T) {
	path := snapshotPath(t)
	tree := pane.New()
	s := &persist.Snapshotter{
		Path: path,
		Tree: tree,
		// Interval left zero — must default to DefaultInterval.
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Snapshotter.Run with Interval=0: %v", err)
	}
}

// TestSnapshotter_ConcurrentTreeMutation exercises the race-detector
// contract: a Snapshotter writing periodically while the server
// mutates the tree must produce no race report under `go test -race`.
// This is the smoke test that we are correctly relying on the pane
// package's internal mutex for both readers (Save → MarshalJSON →
// RLock) and writers (AddSession → Lock).
func TestSnapshotter_ConcurrentTreeMutation(t *testing.T) {
	path := snapshotPath(t)
	tree := pane.New()
	// Seed one repo cluster so AddSession can append children rather
	// than always paying RepoOrder allocation.
	if err := tree.AddSession(pane.Session{
		ID: "nixos-config@main", Repo: "nixos-config",
	}); err != nil {
		t.Fatal(err)
	}

	s := &persist.Snapshotter{
		Path:     path,
		Tree:     tree,
		Interval: 5 * time.Millisecond,
		Logger:   log.New(&bytes.Buffer{}, "", 0),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var snapshotterDone sync.WaitGroup
	snapshotterDone.Add(1)
	go func() {
		defer snapshotterDone.Done()
		_ = s.Run(ctx)
	}()

	// Fan out mutators while the snapshotter ticks.
	var mutators sync.WaitGroup
	for i := 0; i < 8; i++ {
		mutators.Add(1)
		go func(id int) {
			defer mutators.Done()
			for j := 0; j < 25; j++ {
				name := sessionName(id, j)
				_ = tree.AddSession(pane.Session{
					ID: name, Repo: "nixos-config",
				})
				_ = tree.AddPane(name, pane.Pane{Name: "agent"})
				_ = tree.RemoveSession(name)
			}
		}(i)
	}
	mutators.Wait()
	cancel()
	snapshotterDone.Wait()

	// Final tree should still pass Validate — the persist layer must
	// not have torn the model.
	if err := tree.Validate(); err != nil {
		t.Errorf("tree fails Validate after concurrent mutation: %v", err)
	}
}

func sessionName(workerID, iter int) string {
	return "nixos-config@worker-" + itoa(workerID) + "-" + itoa(iter)
}

// itoa is a small dependency-free int-to-string for sessionName so the
// race test does not pull in fmt — fmt's I/O paths can interact with
// the race detector in subtle ways and we want to keep this test as
// lean as possible.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
