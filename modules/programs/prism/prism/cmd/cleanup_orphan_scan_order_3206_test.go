package cmd

// Tests for #3206: the orphan-sidecar scan runs after the stop of the review
// children, and it skips each child that the stop kept. Otherwise the scan
// removes the PID file of a review child first, and EnsureSidecarStopped then
// returns "stopped" for that child with no proof of exit.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

// fakeOrphanScan is a fake /proc tree for the orphan-sidecar scan, and a
// recorder for the signals of the scan. A SIGTERM removes the /proc entry of
// its PID, as if the sidecar exits.
type fakeOrphanScan struct {
	t         *testing.T
	root      string
	signalled []int
}

func withFakeOrphanScan(t *testing.T) *fakeOrphanScan {
	t.Helper()
	f := &fakeOrphanScan{t: t, root: t.TempDir()}
	prevRoot, prevSignal := orphanScanProcRoot, orphanScanSignal
	orphanScanProcRoot = f.root
	orphanScanSignal = func(pid int, sig syscall.Signal) error {
		f.signalled = append(f.signalled, pid)
		if sig == syscall.SIGTERM {
			f.exit(pid)
		}
		return nil
	}
	t.Cleanup(func() { orphanScanProcRoot, orphanScanSignal = prevRoot, prevSignal })
	return f
}

// start adds a sidecar process for session to the fake /proc tree.
func (f *fakeOrphanScan) start(pid int, session string) {
	writeFakeProcEntry(f.t, f.root, pid, []string{"prism", "sidecar", "--session", session})
}

func (f *fakeOrphanScan) exit(pid int) {
	if err := os.RemoveAll(filepath.Join(f.root, strconv.Itoa(pid))); err != nil {
		f.t.Errorf("remove fake /proc entry %d: %v", pid, err)
	}
}

func writeSidecarPIDFile(t *testing.T, session string, pid int) {
	t.Helper()
	pidPath, err := prismSession.SidecarPIDPath(session)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write PID file: %v", err)
	}
}

func sidecarPIDFileExists(t *testing.T, session string) bool {
	t.Helper()
	pidPath, err := prismSession.SidecarPIDPath(session)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	_, err = os.Stat(pidPath)
	return err == nil
}

// orphanScanSites are the four cleanup sites that run the scan for the
// children of parent.
var orphanScanSites = []struct {
	name string
	run  func(t *testing.T, parent string) error
}{
	{"doCleanup", func(t *testing.T, parent string) error {
		m := cleanupModel{session: parent, worktreeName: "branch"}
		done, ok := m.doCleanup()().(cleanupDoneMsg)
		if !ok {
			t.Fatal("doCleanup did not return a cleanupDoneMsg")
		}
		return done.err
	}},
	{"headlessCleanupWithJSONTo", func(_ *testing.T, parent string) error {
		return headlessCleanupWithJSONTo(parent, "branch", "", "", false, io.Discard)
	}},
	{"headlessCloseSessionWithJSONTo", func(_ *testing.T, parent string) error {
		return headlessCloseSessionWithJSONTo(parent, false, io.Discard)
	}},
	{"cleanupChildrenOfKeptSession", func(_ *testing.T, parent string) error {
		return sidecarsNotStoppedError(cleanupChildrenOfKeptSession(parent))
	}},
}

// AC (#3206): in each site, the stop of the review children runs before the
// scan, and it finds the PID file of each child. The scan then stops an
// investigator child and a review child with no PID file. It does not
// signal a kept child and does not remove its PID file, also on a second
// cleanup run.
func TestCleanupSites_OrphanScanRunsAfterChildStop_3206(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the orphan-sidecar scan walks /proc on Linux only")
	}
	for _, site := range orphanScanSites {
		t.Run(site.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			// A descendant name makes doCleanup skip git, so that the path
			// needs no repository.
			const parent = "myrepo@scan-order~investigate-p"
			stopped := parent + "~review-1-review-code"
			kept := parent + "~review-1-review-goal"
			noPIDFile := parent + "~review-1-review-qa"
			investigator := parent + "~investigate-x"
			// PIDs above the largest pid_max of Linux, so that no real
			// process has one of them.
			pids := map[string]int{stopped: 5000001, kept: 5000002, noPIDFile: 5000003, investigator: 5000004}

			dbFile := seedHookRows(t, parent, stopped, kept, noPIDFile)
			withSpyTmux(t)
			installFakeRunner(t)
			prevStop := cleanupStopSidecar
			cleanupStopSidecar = func(string) error { return nil }
			t.Cleanup(func() { cleanupStopSidecar = prevStop })

			scan := withFakeOrphanScan(t)
			for name, pid := range pids {
				scan.start(pid, name)
			}
			hasPIDFile := map[string]bool{stopped: true, kept: true}
			for name := range hasPIDFile {
				writeSidecarPIDFile(t, name, pids[name])
			}

			// The fake child stop acts as CleanupReviewSessionsForParent with
			// the real EnsureSidecarStopped: a child with a PID file stops
			// with proof of exit or is kept, and a child with no PID file
			// counts as stopped.
			var childStops int
			prevChildren := cleanupReviewChildren
			cleanupReviewChildren = func(d *db.DB, p, _ string) []string {
				childStops++
				if p != parent {
					t.Errorf("child stop for %q, want %q", p, parent)
				}
				if len(scan.signalled) != 0 {
					t.Errorf("stop %d: the scan signalled PIDs %v before the child stop", childStops, scan.signalled)
				}
				for name, has := range hasPIDFile {
					if has && !sidecarPIDFileExists(t, name) {
						t.Errorf("stop %d: PID file of %q gone before the child stop", childStops, name)
					}
				}
				if hasPIDFile[stopped] {
					scan.exit(pids[stopped])
					removeSidecarPIDFile(stopped)
					hasPIDFile[stopped] = false
				}
				for _, name := range []string{stopped, noPIDFile} {
					_ = d.ReleasePort(name)
					_ = d.SetEnded(name)
				}
				return []string{kept}
			}
			t.Cleanup(func() { cleanupReviewChildren = prevChildren })

			for run := 1; run <= 2; run++ {
				scan.signalled = nil
				var err error
				captureStderrDuringFn(t, func() { err = site.run(t, parent) })

				if childStops != run {
					t.Fatalf("run %d: %d child stops, want %d", run, childStops, run)
				}
				if !errors.Is(err, errSidecarNotStopped) || !strings.Contains(err.Error(), strconv.Quote(kept)) {
					t.Errorf("run %d: error = %v, want an errSidecarNotStopped error that names %q", run, err, kept)
				}
				var want []int
				if run == 1 {
					want = []int{pids[noPIDFile], pids[investigator]}
				}
				got := slices.Clone(scan.signalled)
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Errorf("run %d: the scan signalled PIDs %v, want %v", run, got, want)
				}
				if !sidecarPIDFileExists(t, kept) {
					t.Errorf("run %d: PID file of the kept child removed", run)
				}
				if st := readStatus(t, dbFile, kept); st.EndedAt != nil || st.HarnessPort == nil {
					t.Errorf("run %d: kept child: ended_at=%v port=%v, want an open row with its port", run, st.EndedAt, st.HarnessPort)
				}
			}
		})
	}
}

// The keep argument of the scan skips only the named children.
func TestKillOrphanReviewSidecars_SkipsKeptChildren_3206(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the orphan-sidecar scan walks /proc on Linux only")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const parent = "myrepo@scan-keep"
	kept, other := parent+"~review-2-review-goal", parent+"~review-2-review-code"
	scan := withFakeOrphanScan(t)
	scan.start(5000011, kept)
	scan.start(5000012, other)
	writeSidecarPIDFile(t, kept, 5000011)
	writeSidecarPIDFile(t, other, 5000012)

	killOrphanReviewSidecars(parent, []string{kept})

	if !slices.Equal(scan.signalled, []int{5000012}) {
		t.Errorf("signalled PIDs %v, want [5000012]", scan.signalled)
	}
	if !sidecarPIDFileExists(t, kept) {
		t.Error("PID file of the kept child removed")
	}
	if sidecarPIDFileExists(t, other) {
		t.Error("PID file of the signalled child kept, want removed")
	}
}
