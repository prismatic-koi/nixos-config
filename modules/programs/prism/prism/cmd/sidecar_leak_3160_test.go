package cmd

// Tests for #3160 in package cmd: the tmux-session-end hook stops the
// sidecar before it ends the row, cleanup keeps a child whose sidecar did
// not stop, the sidecar exits when its tmux session is gone, and the cleanup
// paths run the ended-session sweep.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// seedHookRows writes open rows with a port for each name and points openDB
// at the database.
func seedHookRows(t *testing.T, names ...string) string {
	t.Helper()
	resetRootCmdFlags(t)
	t.Setenv("PRISM_HOST_API", "")
	dbFile := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, n := range names {
		if err := d.UpsertStatus(n, "myrepo", "/code/myrepo/branch", "finished", nil, nil); err != nil {
			t.Fatalf("UpsertStatus(%q): %v", n, err)
		}
		if _, err := d.AllocatePort(n); err != nil {
			t.Fatalf("AllocatePort(%q): %v", n, err)
		}
	}
	d.Close()
	SetTestDBPath(dbFile)
	t.Cleanup(func() { SetTestDBPath("") })
	return dbFile
}

func readStatus(t *testing.T, dbFile, name string) *db.Status {
	t.Helper()
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	st, err := d.CurrentStatus(name)
	if err != nil || st == nil {
		t.Fatalf("CurrentStatus(%q) = %v, %v", name, st, err)
	}
	return st
}

func withHookStops(t *testing.T, self func(string) bool, children func(*db.DB, string) []string) {
	t.Helper()
	prevSelf, prevChildren := hookStopSidecar, hookStopReviewChildren
	hookStopSidecar, hookStopReviewChildren = self, children
	t.Cleanup(func() { hookStopSidecar, hookStopReviewChildren = prevSelf, prevChildren })
}

func runTmuxSessionEnd(t *testing.T, session string) {
	t.Helper()
	rootCmd.SetArgs([]string{"event", "tmux-session-end", "--session", session})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("event tmux-session-end: %v", err)
	}
}

// AC: when a sidecar stop fails, the session row and its port stay reserved.
func TestEventTmuxSessionEnd_FailedSidecarStopKeepsRowAndPort_3160(t *testing.T) {
	const session = "myrepo@hook-stop-fails"
	dbFile := seedHookRows(t, session)
	withHookStops(t, func(string) bool { return false }, func(*db.DB, string) []string {
		t.Error("children stopped after the session's own stop failed")
		return nil
	})

	runTmuxSessionEnd(t, session)

	st := readStatus(t, dbFile, session)
	if st.EndedAt != nil {
		t.Error("ended_at set after a failed sidecar stop")
	}
	if st.HarnessPort == nil {
		t.Error("harness port released after a failed sidecar stop")
	}
}

// The hook stops the sidecar of the closed session, then the sidecars of the
// review children that the SetEnded cascade reaches. A child whose sidecar
// did not stop keeps its row and port.
func TestEventTmuxSessionEnd_StopsSidecarsAndKeepsFailedChild_3160(t *testing.T) {
	const parent = "myrepo@hook-parent"
	stuck := parent + "~review-1-review-goal"
	stopped := parent + "~review-1-review-code"
	dbFile := seedHookRows(t, parent, stuck, stopped)
	var selfStops []string
	withHookStops(t,
		func(s string) bool { selfStops = append(selfStops, s); return true },
		func(_ *db.DB, p string) []string {
			if p != parent {
				t.Errorf("children of %q stopped, want %q", p, parent)
			}
			return []string{stuck}
		})

	runTmuxSessionEnd(t, parent)

	if !slices.Equal(selfStops, []string{parent}) {
		t.Errorf("own sidecar stops = %v, want [%s]", selfStops, parent)
	}
	if readStatus(t, dbFile, parent).EndedAt == nil {
		t.Error("parent row open, want ended")
	}
	if readStatus(t, dbFile, stopped).EndedAt == nil {
		t.Error("child with a stopped sidecar open, want ended by the cascade")
	}
	st := readStatus(t, dbFile, stuck)
	if st.EndedAt != nil || st.HarnessPort == nil {
		t.Errorf("child whose sidecar did not stop: ended_at=%v port=%v, want open with its port", st.EndedAt, st.HarnessPort)
	}
}

func TestApplyDBLifecycleClears_KeepsFailedChildren_3160(t *testing.T) {
	const parent = "myrepo@clears-parent"
	stuck := parent + "~review-2-review-qa"
	other := parent + "~review-2-review-security"
	dbFile := seedHookRows(t, parent, stuck, other)
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	var result cleanupResult
	applyDBLifecycleClears(d, parent, &result, stuck)
	d.Close()

	if result.EndedAtStamped != true {
		t.Errorf("EndedAtStamped = %v, want true", result.EndedAtStamped)
	}
	if readStatus(t, dbFile, other).EndedAt == nil {
		t.Error("child open, want ended by the cascade")
	}
	if readStatus(t, dbFile, stuck).EndedAt != nil {
		t.Error("child whose sidecar did not stop was ended by the cascade")
	}
}

// probeSequence returns the presences in order, then repeats the last one.
func probeSequence(seq ...prismSession.TmuxPresence) func() prismSession.TmuxPresence {
	var mu sync.Mutex
	i := 0
	return func() prismSession.TmuxPresence {
		mu.Lock()
		defer mu.Unlock()
		p := seq[min(i, len(seq)-1)]
		i++
		return p
	}
}

func TestWatchTmuxSession_3160(t *testing.T) {
	const (
		grace    = 30 * time.Millisecond
		interval = 2 * time.Millisecond
		window   = 300 * time.Millisecond
	)
	cases := []struct {
		desc     string
		probe    func() prismSession.TmuxPresence
		wantGone bool
	}{
		{"session closes", probeSequence(prismSession.TmuxPresent, prismSession.TmuxGone), true},
		{"session never seen", probeSequence(prismSession.TmuxGone), false},
		{"session stays", probeSequence(prismSession.TmuxPresent), false},
		{"unknown answers only", probeSequence(prismSession.TmuxPresent, prismSession.TmuxPresenceUnknown), false},
		{"absence not continuous", func() func() prismSession.TmuxPresence {
			var mu sync.Mutex
			n := 0
			return func() prismSession.TmuxPresence {
				mu.Lock()
				defer mu.Unlock()
				n++
				switch {
				case n == 1:
					return prismSession.TmuxPresent
				case n%5 == 0:
					return prismSession.TmuxPresenceUnknown
				default:
					return prismSession.TmuxGone
				}
			}
		}(), false},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), window)
			defer cancel()
			gone := make(chan time.Time, 1)
			start := time.Now()
			done := make(chan struct{})
			go func() {
				defer close(done)
				watchTmuxSession(ctx, c.probe, grace, interval, func() { gone <- time.Now() })
			}()
			<-done
			select {
			case at := <-gone:
				if !c.wantGone {
					t.Fatal("onGone called, want no call")
				}
				if at.Sub(start) < grace {
					t.Errorf("onGone after %v, want at least the grace %v", at.Sub(start), grace)
				}
			default:
				if c.wantGone {
					t.Fatal("onGone not called, want a call after the grace period")
				}
			}
		})
	}
}

// AC: the next `prism cleanup` stops the sidecar of an ended session that an
// earlier path left behind.
func TestSweepEndedSidecarsAfterCleanup_StopsSidecarOfEndedSession_3160(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Cleanup(prismSession.SetSweepTmuxPresenceForTest(func(string) prismSession.TmuxPresence {
		return prismSession.TmuxGone
	}))
	const name = "myrepo@cleanup-sweep~review-1-review-goal"
	dbFile := seedHookRows(t, name)

	done := startNamedSidecarStub(t, name)
	time.Sleep(100 * time.Millisecond)
	d, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.SetEnded(name); err != nil {
		t.Fatalf("SetEnded: %v", err)
	}
	d.Close()

	t.Run("no sweep inside a sandbox", func(t *testing.T) {
		t.Setenv("PRISM_HOST_API", "unix:///nonexistent/hostapi.sock")
		sweepEndedSidecarsAfterCleanup()
		select {
		case <-done:
			t.Fatal("the sweep ran with PRISM_HOST_API set")
		case <-time.After(200 * time.Millisecond):
		}
	})

	sweepEndedSidecarsAfterCleanup()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sidecar of the ended session still runs after the cleanup sweep")
	}
	pidPath, _ := prismSession.SidecarPIDPath(name)
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file still present: %v", err)
	}
}

// startNamedSidecarStub starts this test binary as `sidecar --session name`
// with the long stub env and writes its PID file. The channel closes when
// the stub exits.
func startNamedSidecarStub(t *testing.T, name string) <-chan struct{} {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(self, "sidecar", "--session", name)
	cmd.Env = append(os.Environ(), "PRISM_CMD_TEST_STUB=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stub: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	pidPath, err := prismSession.SidecarPIDPath(name)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return done
}
