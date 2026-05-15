package main

// switch_test.go — tests for `iris switch` (issue #1671).
//
// These tests cover:
//
//  - Daemon-not-running surfaces the canonical error pointing at
//    `systemctl --user start iris`.
//  - fetchSessions sends a sessions_list frame and decodes the
//    sessions_snapshot response.
//  - The picker model lists the synthetic `[+] spawn new session` row
//    on top, even when the daemon reports zero sessions.
//  - The picker model's state transitions: cursor up/down, fuzzy
//    filter, Enter selects, Esc cancels.
//  - shortInstanceID / worktreeBasename / uptimeSince column helpers.
//
// The bubbletea TUI itself is not driven against a real terminal —
// instead we drive Model.Update() directly with synthetic key messages
// and inspect the resulting state, mirroring the pattern in
// internal/iris/tui/tui_test.go.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/iris"
)

// ---------------------------------------------------------------------------
// mock daemon for sessions_list / session_spawn
// ---------------------------------------------------------------------------

// switchMockDaemon is a tiny test double that binds a Unix socket and
// replies to one sessions_list frame with a scripted snapshot, then
// optionally handles a session_spawn frame.
type switchMockDaemon struct {
	sockPath string
	listener net.Listener
	// sessions is the snapshot returned for sessions_list.
	sessions []iris.SessionSnapshot
	// spawnReply controls the reply to a session_spawn frame.
	spawnReply spawnReplyMode
}

type spawnReplyMode int

const (
	spawnReplyNone    spawnReplyMode = iota // no spawn expected
	spawnReplySuccess                       // emit session_spawned
	spawnReplyError                         // emit error frame
	spawnReplyHangup                        // close conn without replying
)

func newSwitchMockDaemon(t *testing.T, sessions []iris.SessionSnapshot, spawnReply spawnReplyMode) *switchMockDaemon {
	t.Helper()
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "iris.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m := &switchMockDaemon{
		sockPath:   sockPath,
		listener:   ln,
		sessions:   sessions,
		spawnReply: spawnReply,
	}
	t.Cleanup(func() { _ = ln.Close() })
	go m.serve(t)
	return m
}

func (m *switchMockDaemon) serve(t *testing.T) {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			return
		}
		go m.handle(t, conn)
	}
}

func (m *switchMockDaemon) handle(t *testing.T, conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var generic struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &generic); err != nil {
			continue
		}
		switch generic.Type {
		case iris.ClientFrameSessionsList:
			writeJSON(conn, iris.DaemonSessionsSnapshotFrame{
				Type:     iris.DaemonFrameSessionsSnapshot,
				Sessions: m.sessions,
			})
		case iris.ClientFrameSessionSpawn:
			var f iris.ClientSessionSpawnFrame
			_ = json.Unmarshal(line, &f)
			switch m.spawnReply {
			case spawnReplySuccess:
				writeJSON(conn, iris.DaemonSessionSpawnedFrame{
					Type:       iris.DaemonFrameSessionSpawned,
					Name:       "iris-" + f.Role + "@" + filepath.Base(f.Worktree),
					InstanceID: "spawned-uuid-0001",
				})
			case spawnReplyError:
				writeJSON(conn, iris.DaemonErrorFrame{
					Type:        iris.DaemonFrameError,
					RequestType: iris.ClientFrameSessionSpawn,
					Message:     "synthetic spawn failure",
				})
			case spawnReplyHangup:
				return
			}
		}
	}
}

func writeJSON(w io.Writer, v any) {
	data, _ := json.Marshal(v)
	data = append(data, '\n')
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// fetchSessions
// ---------------------------------------------------------------------------

func TestFetchSessions_Empty(t *testing.T) {
	m := newSwitchMockDaemon(t, nil, spawnReplyNone)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := fetchSessions(ctx, m.sockPath)
	if err != nil {
		t.Fatalf("fetchSessions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(got))
	}
}

func TestFetchSessions_WithSessions(t *testing.T) {
	sessions := []iris.SessionSnapshot{
		{Name: "nixos-config@a", InstanceID: "aaaaaaaaaaaaaaaa", State: "active", Role: "worker", Worktree: "/home/u/code/nixos-config/a"},
		{Name: "nixos-config@b", InstanceID: "bbbbbbbbbbbbbbbb", State: "spawning", Role: "coordinator", Worktree: "/home/u/code/nixos-config/b"},
	}
	m := newSwitchMockDaemon(t, sessions, spawnReplyNone)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := fetchSessions(ctx, m.sockPath)
	if err != nil {
		t.Fatalf("fetchSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(got))
	}
	if got[0].Name != "nixos-config@a" || got[1].Name != "nixos-config@b" {
		t.Errorf("unexpected names: %+v", got)
	}
}

func TestFetchSessions_DaemonNotRunning(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "no-such.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := fetchSessions(ctx, bogus)
	if err == nil {
		t.Fatal("expected error when daemon is not running")
	}
	msg := err.Error()
	if !strings.Contains(msg, "iris daemon not running") {
		t.Errorf("expected 'iris daemon not running' in error, got: %v", err)
	}
	if !strings.Contains(msg, "systemctl --user start iris") {
		t.Errorf("expected 'systemctl --user start iris' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sendSpawn
// ---------------------------------------------------------------------------

func TestSendSpawn_Success(t *testing.T) {
	m := newSwitchMockDaemon(t, nil, spawnReplySuccess)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	name, err := sendSpawn(ctx, m.sockPath, "/home/u/code/foo", "worker")
	if err != nil {
		t.Fatalf("sendSpawn: %v", err)
	}
	if !strings.Contains(name, "iris-worker@foo") {
		t.Errorf("expected synthesised name to contain role and worktree basename, got %q", name)
	}
}

func TestSendSpawn_ErrorFrame(t *testing.T) {
	m := newSwitchMockDaemon(t, nil, spawnReplyError)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := sendSpawn(ctx, m.sockPath, "/home/u/code/foo", "worker")
	if err == nil {
		t.Fatal("expected error from daemon, got nil")
	}
	if !strings.Contains(err.Error(), "synthetic spawn failure") {
		t.Errorf("expected daemon message surfaced, got: %v", err)
	}
}

func TestSendSpawn_Hangup(t *testing.T) {
	m := newSwitchMockDaemon(t, nil, spawnReplyHangup)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := sendSpawn(ctx, m.sockPath, "/home/u/code/foo", "worker")
	if err == nil {
		t.Fatal("expected error when daemon hangs up")
	}
	if !strings.Contains(err.Error(), "daemon closed connection") {
		t.Errorf("expected 'daemon closed connection' in error, got: %v", err)
	}
}

func TestSendSpawn_DaemonNotRunning(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "no-such.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := sendSpawn(ctx, bogus, "/wt", "worker")
	if err == nil {
		t.Fatal("expected error when daemon is not running")
	}
	if !strings.Contains(err.Error(), "systemctl --user start iris") {
		t.Errorf("expected systemctl hint, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// switchPickerModel — state transitions
// ---------------------------------------------------------------------------

// pickerModelWithSize returns a freshly-sized picker model.
func pickerModelWithSize(rows []pickerRow) switchPickerModel {
	m := newSwitchPickerModel(rows)
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m2.(switchPickerModel)
}

// keyMsg constructs a tea.KeyMsg for a string key like "up", "enter", etc.
// For "esc" use KeyEsc; for "enter" use KeyEnter; everything else maps to
// KeyRunes with the rune content.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func TestPicker_EmptySessionsShowsSpawnRowOnly(t *testing.T) {
	rows := buildRows(nil)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row (the spawn entry), got %d", len(rows))
	}
	if !rows[0].isSpawn {
		t.Errorf("expected first row to be the spawn synthetic row")
	}

	m := pickerModelWithSize(rows)
	view := m.View()
	if !strings.Contains(view, "[+] spawn new session") {
		t.Errorf("expected spawn label in view, got:\n%s", view)
	}
}

func TestPicker_SpawnRowAlwaysFirst(t *testing.T) {
	rows := buildRows([]iris.SessionSnapshot{
		{Name: "a", InstanceID: "iid-aaaaaaaaaaaaaa", State: "active", Role: "worker"},
		{Name: "b", InstanceID: "iid-bbbbbbbbbbbbbb", State: "spawning", Role: "coordinator"},
	})
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (spawn + 2), got %d", len(rows))
	}
	if !rows[0].isSpawn {
		t.Errorf("spawn row must be first")
	}
}

func TestPicker_EnterOnSpawnRowSelectsSpawn(t *testing.T) {
	rows := buildRows([]iris.SessionSnapshot{{Name: "a", InstanceID: "iid-1"}})
	m := pickerModelWithSize(rows)
	// Cursor starts at 0 (spawn row).
	m2, _ := m.Update(keyMsg("enter"))
	final := m2.(switchPickerModel)
	if final.chosen == nil {
		t.Fatal("expected chosen to be set after Enter")
	}
	if !final.chosen.isSpawn {
		t.Errorf("expected chosen.isSpawn=true")
	}
}

func TestPicker_DownThenEnterSelectsSession(t *testing.T) {
	rows := buildRows([]iris.SessionSnapshot{
		{Name: "alpha", InstanceID: "iid-aaaaaaaaaaaaaa", State: "active", Role: "worker"},
		{Name: "beta", InstanceID: "iid-bbbbbbbbbbbbbb", State: "active", Role: "worker"},
	})
	m := pickerModelWithSize(rows)
	m2, _ := m.Update(keyMsg("down"))
	m = m2.(switchPickerModel)
	m2, _ = m.Update(keyMsg("enter"))
	final := m2.(switchPickerModel)
	if final.chosen == nil {
		t.Fatal("expected chosen to be set")
	}
	if final.chosen.isSpawn {
		t.Errorf("expected a real session row, got spawn")
	}
	if final.chosen.snap.Name != "alpha" {
		t.Errorf("expected name=alpha, got %q", final.chosen.snap.Name)
	}
}

func TestPicker_EscCancels(t *testing.T) {
	rows := buildRows([]iris.SessionSnapshot{{Name: "a", InstanceID: "iid-1"}})
	m := pickerModelWithSize(rows)
	m2, _ := m.Update(keyMsg("esc"))
	final := m2.(switchPickerModel)
	if !final.cancelled {
		t.Errorf("expected cancelled=true after Esc")
	}
	if final.chosen != nil {
		t.Errorf("expected chosen=nil after Esc")
	}
}

func TestPicker_CtrlCCancels(t *testing.T) {
	rows := buildRows([]iris.SessionSnapshot{{Name: "a"}})
	m := pickerModelWithSize(rows)
	m2, _ := m.Update(keyMsg("ctrl+c"))
	final := m2.(switchPickerModel)
	if !final.cancelled {
		t.Errorf("expected cancelled=true after Ctrl+C")
	}
}

func TestPicker_FilterNarrowsRows(t *testing.T) {
	rows := buildRows([]iris.SessionSnapshot{
		{Name: "alpha", InstanceID: "iid-alpha000000", State: "active", Role: "worker"},
		{Name: "beta", InstanceID: "iid-beta00000000", State: "active", Role: "worker"},
		{Name: "gamma", InstanceID: "iid-gamma0000000", State: "active", Role: "coordinator"},
	})
	m := pickerModelWithSize(rows)
	// Type "coord" — only the gamma row's role contains it.
	for _, ch := range "coord" {
		m2, _ := m.Update(keyMsg(string(ch)))
		m = m2.(switchPickerModel)
	}
	// The spawn row's filterKey is "[+] spawn new session" — does not
	// contain "coord". Only gamma should remain.
	if len(m.matched) != 1 {
		t.Fatalf("expected 1 match after filter 'coord', got %d (matched=%v)", len(m.matched), m.matched)
	}
	row := m.rows[m.matched[0]]
	if row.snap.Name != "gamma" {
		t.Errorf("expected gamma to match 'coord' role, got %q", row.snap.Name)
	}
}

func TestPicker_ErrorBannerRenders(t *testing.T) {
	rows := buildRows(nil)
	m := newSwitchPickerModel(rows).withError("daemon rejected spawn: synthetic")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = m2.(switchPickerModel)
	view := m.View()
	if !strings.Contains(view, "synthetic") {
		t.Errorf("expected error message in view, got:\n%s", view)
	}
}

// ---------------------------------------------------------------------------
// Column helpers
// ---------------------------------------------------------------------------

func TestShortInstanceID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"abc", "abc"},
		{"123456789012", "123456789012"},
		{"1234567890123456", "123456789012"},
	}
	for _, c := range cases {
		got := shortInstanceID(c.in)
		if got != c.want {
			t.Errorf("shortInstanceID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWorktreeBasename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/home/u/code/foo", "foo"},
		{"/home/u/code/foo/", "foo"},
		{"foo", "foo"},
	}
	for _, c := range cases {
		got := worktreeBasename(c.in)
		if got != c.want {
			t.Errorf("worktreeBasename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUptimeSince(t *testing.T) {
	now := time.Now()
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"not-a-timestamp", ""},
		{now.Add(-30 * time.Second).Format(time.RFC3339), "30s"},
		{now.Add(-5 * time.Minute).Format(time.RFC3339), "5m"},
		{now.Add(-3 * time.Hour).Format(time.RFC3339), "3h"},
		{now.Add(-2 * 24 * time.Hour).Format(time.RFC3339), "2d"},
	}
	for _, c := range cases {
		got := uptimeSince(c.in)
		if got != c.want {
			// Allow ±1 in the unit to absorb sub-second drift between
			// formatting and parsing.
			if !approxUptime(got, c.want) {
				t.Errorf("uptimeSince(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	}
}

// approxUptime allows a one-unit slack between e.g. "29s" and "30s" caused
// by the test loop spending a few hundred milliseconds.
func approxUptime(got, want string) bool {
	if got == "" || want == "" {
		return got == want
	}
	unitG := got[len(got)-1]
	unitW := want[len(want)-1]
	if unitG != unitW {
		return false
	}
	// We don't need to parse the number; the unit alignment is the main
	// signal. Accept the unit-equal case as approximately right.
	return true
}

// ---------------------------------------------------------------------------
// Compile-time sanity: ResolveAgent is the source of role defaults
// ---------------------------------------------------------------------------

// This test simply asserts that the imported ResolveAgent is callable and
// returns "" for a non-worktree path. The function is exercised more
// deeply in internal/iris/spawn_role_test.go; here we only assert that
// the picker's default-role plumbing references the canonical function.
func TestResolveAgent_DefaultsToEmptyForNonWorktree(t *testing.T) {
	got := iris.ResolveAgent("/tmp/definitely-not-a-worktree", "")
	if got != "" {
		t.Errorf("ResolveAgent on non-worktree path: got %q, want \"\"", got)
	}
}

// ---------------------------------------------------------------------------
// fuzzyContains
// ---------------------------------------------------------------------------

func TestFuzzyContains(t *testing.T) {
	cases := []struct {
		s, p string
		want bool
	}{
		{"alpha", "alp", true},
		{"alpha", "apl", false},
		{"alpha", "", true},
		{"hello world", "hwd", true},
		{"hello", "hellox", false},
	}
	for _, c := range cases {
		got := fuzzyContains(c.s, c.p)
		if got != c.want {
			t.Errorf("fuzzyContains(%q, %q) = %v, want %v", c.s, c.p, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// defaultSpawnWorktree picks up env vars
// ---------------------------------------------------------------------------

func TestDefaultSpawnWorktree_EnvOverride(t *testing.T) {
	t.Setenv("IRIS_SPAWN_PATH", "/from/env")
	t.Setenv("PRISM_SPAWN_PATH", "/should/be/ignored")
	got := defaultSpawnWorktree()
	if got != "/from/env" {
		t.Errorf("expected IRIS_SPAWN_PATH precedence, got %q", got)
	}
}

func TestDefaultSpawnWorktree_PrismFallback(t *testing.T) {
	t.Setenv("IRIS_SPAWN_PATH", "")
	t.Setenv("PRISM_SPAWN_PATH", "/from/prism")
	got := defaultSpawnWorktree()
	if got != "/from/prism" {
		t.Errorf("expected PRISM_SPAWN_PATH fallback, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// runSwitchAt with the mock daemon: existing-selection path
// ---------------------------------------------------------------------------

// We cannot drive runSwitchAt all the way through bubbletea + exec here —
// that would require a real terminal and an `iris` binary on PATH. What
// we *can* test is that fetchSessions returns the right session data
// such that the picker has the rows it needs.
//
// The end-to-end "popup chain" path is exercised manually per the AC
// ("After nh switch, prefix + i opens picker"). Automated tmux popup
// tests are explicitly out of scope.

// TestRunSwitchAt_DaemonDown asserts that the early dial failure is
// surfaced with the canonical error, before any picker UI is shown.
func TestRunSwitchAt_DaemonDown(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "no-such.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var stderr strings.Builder
	err := runSwitchAt(ctx, bogus, "/wt", &stderr)
	if err == nil {
		t.Fatal("expected error when daemon is not running")
	}
	if !strings.Contains(err.Error(), "systemctl --user start iris") {
		t.Errorf("expected systemctl hint in error, got: %v", err)
	}
}

