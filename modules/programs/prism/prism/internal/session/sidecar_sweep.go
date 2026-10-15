package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// sidecarPIDSuffix ends the name of every sidecar PID file in run/.
const sidecarPIDSuffix = "-sidecar.pid"

// SidecarPIDFile is one sidecar PID file in $XDG_STATE_HOME/prism/run/.
type SidecarPIDFile struct {
	Session string
	// PID is 0 when the file does not hold a PID.
	PID int
	// ModTime is when StartSidecarWithOpts wrote the file, which is just
	// after the sidecar process started.
	ModTime time.Time
}

// ListSidecarPIDFiles returns every sidecar PID file in the run directory.
// A missing run directory gives no files and no error.
func ListSidecarPIDFiles() ([]SidecarPIDFile, error) {
	base, err := sidecarStateDir()
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(base, "run")
	entries, err := os.ReadDir(runDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sidecar PID files: %w", err)
	}
	var files []SidecarPIDFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, sidecarPIDSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		session := strings.TrimSuffix(name, sidecarPIDSuffix)
		files = append(files, SidecarPIDFile{
			Session: session,
			PID:     readSidecarPID(session),
			ModTime: info.ModTime(),
		})
	}
	return files, nil
}

// SweepOpts configures SweepEndedSidecars.
type SweepOpts struct {
	// StopTimeout is passed to EnsureSidecarStopped. Zero uses
	// SidecarStopTimeout.
	StopTimeout time.Duration
	// ReleaseLive is asked about an ended session whose sidecar runs and
	// whose tmux session still exists. When it returns true, the sweep
	// stops the sidecar and then calls KillTmux for the session. When it is
	// nil or returns false, the sweep leaves the session alone.
	ReleaseLive func(st *db.Status) bool
	// KillTmux closes a tmux session. It is required when ReleaseLive is
	// set.
	KillTmux func(sessionName string)
	// LockWait bounds the wait for a sweep that another process runs. Zero
	// uses sweepLockWait.
	LockWait time.Duration
}

// SweepResult reports what one sweep did.
type SweepResult struct {
	// Stopped names the sessions whose sidecar the sweep stopped.
	Stopped []string
	// Failed names the sessions whose sidecar still runs after the stop.
	Failed []string
	// StaleRemoved names the sessions whose PID file the sweep removed
	// without a signal, because its PID did not run that session's sidecar.
	StaleRemoved []string
	// Busy is true when another sweep held the lock for the whole wait.
	// The sweep then did nothing.
	Busy bool
}

// sweepLockWait is how long a sweep waits for a sweep that another process
// runs before it gives up.
const sweepLockWait = 30 * time.Second

// sweepTmuxPresence is the tmux probe of the sweep. Tests replace it.
var sweepTmuxPresence = TmuxSessionPresence

// SetSweepTmuxPresenceForTest replaces the tmux probe of SweepEndedSidecars
// until the returned restore runs. Tests in other packages use it, so that a
// sweep test never asks the tmux server of the host. Production code must
// not call it.
func SetSweepTmuxPresenceForTest(probe func(sessionName string) TmuxPresence) (restore func()) {
	prev := sweepTmuxPresence
	sweepTmuxPresence = probe
	return func() { sweepTmuxPresence = prev }
}

// SweepEndedSidecars stops the sidecar of every ended session, and removes
// every PID file whose PID does not run the sidecar of its session.
//
// It fails closed. If it cannot read the set of live sessions, it returns
// the error, sends no signal, and removes no PID file. It never touches the
// PID file or the sidecar of a session whose row is live (ended_at IS NULL).
// That covers the review agents of a round that still runs.
//
// It stops a sidecar only when all of these are true:
//
//   - the PID runs the sidecar of that session (the #3136 owner check);
//   - the row of the session exists and has ended_at set, read again just
//     before the stop;
//   - the PID file is older than ended_at. A newer file belongs to a sidecar
//     that started after the session ended: a new incarnation whose row
//     still carries the ended_at of the old one;
//   - tmux reports the session gone, or ReleaseLive allows the sweep to
//     close the tmux session too. An unknown tmux answer stops nothing.
//
// The PID files are listed before the live set is read. A session that
// spawns during the sweep writes its row before its PID file, so a PID file
// that the sweep sees always has a row that the live set can show.
func SweepEndedSidecars(d *db.DB, opts SweepOpts) (SweepResult, error) {
	var res SweepResult
	if d == nil {
		return res, fmt.Errorf("sweep ended sidecars: nil database handle")
	}
	timeout := opts.StopTimeout
	if timeout <= 0 {
		timeout = SidecarStopTimeout
	}

	files, err := ListSidecarPIDFiles()
	if err != nil {
		return res, fmt.Errorf("sweep ended sidecars: %w", err)
	}
	if len(files) == 0 {
		return res, nil
	}

	unlock, locked, err := lockSidecarSweep(opts.LockWait)
	if err != nil {
		return res, fmt.Errorf("sweep ended sidecars: %w", err)
	}
	if !locked {
		res.Busy = true
		return res, nil
	}
	defer unlock()

	live, err := d.AllActiveStatus()
	if err != nil {
		return res, fmt.Errorf("sweep ended sidecars: read live sessions: %w", err)
	}
	liveSet := make(map[string]bool, len(live))
	for _, st := range live {
		liveSet[st.SessionName] = true
	}

	for _, f := range files {
		if liveSet[f.Session] {
			continue
		}
		switch sweepPIDOwner(f) {
		case pidOwnerGone, pidOwnerOther:
			removeSidecarPIDFileIf(f.Session, f.PID)
			res.StaleRemoved = append(res.StaleRemoved, f.Session)
			continue
		case pidOwnerUnknown:
			continue
		}

		st, err := d.CurrentStatus(f.Session)
		if err != nil || st == nil || st.EndedAt == nil {
			continue
		}
		if f.ModTime.After(*st.EndedAt) {
			continue
		}
		killTmux := false
		switch sweepTmuxPresence(f.Session) {
		case TmuxGone:
		case TmuxPresent:
			if opts.ReleaseLive == nil || opts.KillTmux == nil || !opts.ReleaseLive(st) {
				continue
			}
			killTmux = true
		default:
			continue
		}
		if _, gone := stopSidecarPID(f.Session, f.PID, timeout, false); !gone {
			res.Failed = append(res.Failed, f.Session)
			continue
		}
		res.Stopped = append(res.Stopped, f.Session)
		if killTmux {
			opts.KillTmux(f.Session)
		}
	}
	return res, nil
}

// sweepPIDOwner classifies the PID of a PID file. A file with no PID, and a
// PID that another user owns, count as gone: no sidecar of this user runs
// there.
func sweepPIDOwner(f SidecarPIDFile) pidOwner {
	if f.PID <= 0 || !sidecarProcessExists(f.PID) {
		return pidOwnerGone
	}
	if syscall.Kill(f.PID, 0) == syscall.EPERM {
		return pidOwnerOther
	}
	return sidecarPIDOwner(f.PID, f.Session)
}

// lockSidecarSweep takes an exclusive lock on run/sidecar-sweep.lock, so
// that the sweeps of the five sidecars of one review round do not run the
// same stops at the same time. It waits up to wait (zero: sweepLockWait).
// locked is false when another process held the lock for the whole wait.
func lockSidecarSweep(wait time.Duration) (unlock func(), locked bool, err error) {
	if wait <= 0 {
		wait = sweepLockWait
	}
	base, err := sidecarStateDir()
	if err != nil {
		return nil, false, err
	}
	runDir := filepath.Join(base, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, false, fmt.Errorf("create run dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(runDir, "sidecar-sweep.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open sweep lock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lockErr == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, true, nil
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, false, fmt.Errorf("lock sweep: %w", lockErr)
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, false, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}
