package cmd

// prism convert — convert a regular git repo to the prism bare+worktree layout.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sessionname"
	"github.com/prismatic-koi/prism/internal/tmux"
)

var convertCmd = &cobra.Command{
	Use:   "convert [path]",
	Short: "Convert a regular git repo to the prism bare+worktree layout",
	Long: `Convert a regular git clone into the prism bare+worktree layout.

The repo at PATH (defaults to the current directory) is converted in-place:
  - A bare clone is created at <path>/.bare
  - A .git file pointing to .bare is written at <path>/.git
  - The working tree is moved into <path>/<branch>/
  - The worktree index is populated so git status is clean immediately

Before the conversion, prism stops the session of the clone. No session
can start in the clone until the conversion ends. If the session ran
before the conversion, prism starts the session of the new worktree.

This is the same operation performed by the C-f context switcher when you
select "[convert to bare+worktree layout]" on a regular repo.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var path string
		if len(args) == 1 {
			path = args[0]
		} else {
			var err error
			path, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("could not determine current directory: %w", err)
			}
		}

		path, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("could not resolve path: %w", err)
		}

		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("path does not exist: %s", path)
		}

		res, err := convertPlainClone(path, func(msg string) {
			fmt.Println(msg)
		})
		if err != nil {
			return errors.Join(fmt.Errorf("conversion failed: %w", err), restartAfterConvert(res, false))
		}

		fmt.Printf("done — worktree at %s\n", res.worktreePath)
		return restartAfterConvert(res, true)
	},
}

func init() {
	rootCmd.AddCommand(convertCmd)
}

// convertResult is the result of convertPlainClone.
type convertResult struct {
	// dir is the real path of the clone (canonicalDir).
	dir string
	// worktreePath is the default-branch worktree after a conversion.
	worktreePath string
	// sessionName is the name of the root session of dir.
	sessionName string
	// ran is true when a tmux session that convertPlainClone stopped ran
	// before the stop.
	ran bool
}

// Seams of convertPlainClone. Tests replace them.
var (
	convertLockWait      = 30 * time.Second
	convertPaneWait      = 10 * time.Second
	convertToBare        = git.ConvertToBare
	convertCallerSession = callerPaneSession
	convertPanePIDs      = tmux.PanePIDs
)

// convertPlainClone converts the plain clone at path to the bare+worktree
// layout (#3361). The root session of the clone can write the clone, so it
// can write the .bare that the conversion makes, and host git runs in that
// .bare. So convertPlainClone stops that session before the conversion. It
// holds the conversion lock of path from before the stop to the end of the
// conversion, and session.Create refuses each start in path while the lock
// is held.
//
// It refuses the conversion when it cannot prove that the session stopped.
func convertPlainClone(path string, progress func(string)) (convertResult, error) {
	if sandboxenv.IsInsideSandbox() {
		return convertResult{}, fmt.Errorf("convert %s: %w", path, git.ErrBareInSandbox)
	}
	// The session of the clone has the name of the real dir entry, and the
	// conversion accepts the clone through a symlink or another letter case.
	dir, err := canonicalDir(path)
	if err != nil {
		return convertResult{}, fmt.Errorf("convert %s: %w", path, err)
	}
	name, err := session.NameFor(dir, "")
	if err != nil {
		return convertResult{}, err
	}
	res := convertResult{dir: dir, sessionName: name}

	lock, err := session.LockForConvert(dir, convertLockWait)
	if err != nil {
		return res, err
	}
	defer lock.Release()

	// Check the layout with the lock held. A conversion that ended while
	// this one waited for the lock leaves a .bare, and the rollback of a
	// second ConvertToBare removes that .bare.
	if !git.IsRegularRepo(dir) {
		return res, fmt.Errorf("convert %s: the dir is not a plain git clone", dir)
	}
	sessions := sessionsInDir(dir, name)
	// The stop kills each pane of the session, so it also kills this
	// process if it runs in one of them.
	if caller := convertCallerSession(); slices.Contains(sessions, caller) {
		return res, fmt.Errorf("convert %s: this command runs in a pane of session %q, and the conversion stops that session. Run the command in a shell outside that session, or use the C-f switcher", dir, caller)
	}

	for _, s := range sessions {
		if session.HasExactSession(s) {
			res.ran = true
			progress("  stopping session " + s + "...")
		}
		if err := stopSessionForConvert(s); err != nil {
			return res, fmt.Errorf("convert %s: %w", dir, err)
		}
	}

	res.worktreePath, err = convertToBare(dir, progress)
	return res, err
}

// canonicalDir returns the real path of dir: the symlinks resolved, and the
// last element spelled as its entry in the parent dir. On a case-insensitive
// volume, a path with another letter case names the same dir.
func canonicalDir(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(resolved)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		p := filepath.Join(parent, e.Name())
		if ei, err := os.Lstat(p); err == nil && ei.IsDir() && os.SameFile(ei, info) {
			return p, nil
		}
	}
	return resolved, nil
}

// sessionsInDir returns name and each other prism session that runs in dir:
// a session with an open row or a tmux session whose dir is dir by file
// identity. Such a session started through a symlink or another letter
// case, so its name is not name.
//
// A tmux session counts only when it has a prism row or a sidecar PID file.
// tmux gives a session with no start dir the cwd of the command that made
// it, so a host shell session can have dir as its path. The meta sessions
// (scratchpad, the dashboard) are host shells too, and the stop moves the
// clients to scratchpad. A descendant has a read-only worktree. None of
// these is in the list.
func sessionsInDir(dir, name string) []string {
	names := []string{name}
	add := func(n string) {
		if n != "" && !sessionname.IsMeta(n) && !sessionname.IsDescendant(n) && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	d, err := openDB()
	if err != nil {
		d = nil
	} else {
		defer d.Close()
		if rows, err := d.AllActiveStatus(); err == nil {
			for _, r := range rows {
				if r.Worktree != "" && config.SameDir(r.Worktree, dir) {
					add(r.SessionName)
				}
			}
		}
	}
	isPrismSession := func(n string) bool {
		if d != nil {
			if st, err := d.CurrentStatus(n); err == nil && st != nil {
				return true
			}
		}
		pidPath, err := session.SidecarPIDPath(n)
		return err == nil && fileExistsAt(pidPath)
	}
	if out, err := tmux.Run("list-sessions", "-F", "#{session_name}\t#{session_path}"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if n, p, ok := strings.Cut(line, "\t"); ok && p != "" && config.SameDir(p, dir) && isPrismSession(n) {
				add(n)
			}
		}
	}
	return names
}

func fileExistsAt(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// restartAfterConvert starts a session again after a prism convert, if the
// root session ran before the stop. After a conversion, the session starts
// in the new worktree. After a failed conversion, the session starts in
// the clone. If the stop failed and the session still runs,
// ensureAndSwitch leaves it as it is.
func restartAfterConvert(res convertResult, converted bool) error {
	if !res.ran {
		return nil
	}
	if converted {
		return convertStartSession(res.worktreePath, res.dir)
	}
	if err := refuseStartBesideBare(res.dir); err != nil {
		return err
	}
	return convertStartSession(res.dir, "")
}

// convertStartSession starts the session of dir with no client, as prism
// switch starts it. Tests replace it.
var convertStartSession = func(dir, projectRoot string) error {
	s, err := loadSwitchSetup(false)
	if err != nil {
		return fmt.Errorf("start a session after the conversion: %w", err)
	}
	opts := s.opts
	opts.Headless = true
	applyPathIsolationOverride(dir, s.cfg, &opts, s.isoCaps, s.pf)
	return ensureAndSwitch(dir, projectRoot, opts)
}

// refuseStartBesideBare returns an error when a .bare is in the clone at
// path after a failed conversion. An incomplete rollback can keep the .bare,
// and the root session can write it.
func refuseStartBesideBare(path string) error {
	if _, err := os.Lstat(filepath.Join(path, ".bare")); err == nil {
		return fmt.Errorf("%s/.bare remains after the failed conversion, so prism does not start a session in %s. Remove %s/.bare, then start the session", path, path, path)
	}
	return nil
}

// switchProjectStopSidecar is the sidecar stop of stopSessionForConvert.
// Tests replace it, so that a test signals no process.
var switchProjectStopSidecar = func(sessionName string) bool {
	return session.EnsureSidecarStopped(sessionName, session.SidecarStopTimeout)
}

// stopSessionForConvert ends the root session name of a plain clone before
// the conversion. It returns an error when it cannot prove that the session
// stopped, and the caller must then not convert:
//
//   - The sidecar did not stop. The tmux session, the row, and the port
//     stay, because the sidecar can still use them.
//   - A pane process still runs after the tmux kill. The agent sandbox
//     stops after the tmux kill, not at the kill.
//   - The sweep of the `prism container` containers failed. A container
//     with a read-write mount can write the clone.
//   - The row did not end. `prism restore` starts the session of an open
//     row in the clone.
//
// A session with no sidecar, no tmux session, or no row is not an error.
func stopSessionForConvert(name string) error {
	moveClientsToScratchpad(name)
	panes, _ := convertPanePIDs(name)
	if !switchProjectStopSidecar(name) {
		fmt.Fprintf(os.Stderr, "warning: sidecar of session %q did not stop — keeping its tmux session, row, and port\n", name)
		return sidecarsNotStoppedError([]string{name})
	}
	_ = tmux.KillSession(name)
	if err := waitProcessesGone(panes, convertPaneWait); err != nil {
		return fmt.Errorf("stop session %q: %w", name, err)
	}
	if err := sweepPrismContainersForSession(name); err != nil {
		return fmt.Errorf("stop session %q: %w", name, err)
	}
	d, err := openDB()
	if err != nil {
		removeContainerIfExists(name)
		return fmt.Errorf("stop session %q: open the DB: %w", name, err)
	}
	defer d.Close()
	if isolationModeFromDB(d, name) != "host" {
		removeContainerIfExists(name)
	}
	if err := d.ReleasePort(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: release port for session %q: %v\n", name, err)
	}
	if err := setEndedStoppingReviewChildren(d, name); err != nil {
		return fmt.Errorf("stop session %q: end the row: %w", name, err)
	}
	_ = d.PurgeBusMessages(name)
	return nil
}

// moveClientsToScratchpad switches each client that views the session name
// to the scratchpad session. A client of a killed session detaches, and a
// popup of that client, prism switch for example, stops with it.
func moveClientsToScratchpad(name string) {
	clients, err := tmux.ListClients()
	if err != nil {
		return
	}
	var viewers []string
	for _, c := range clients {
		if s, err := tmux.ClientSession(c); err == nil && s == name {
			viewers = append(viewers, c)
		}
	}
	if len(viewers) == 0 {
		return
	}
	if !tmux.HasSession("scratchpad") {
		home, _ := os.UserHomeDir()
		_ = tmux.NewSessionDetached("scratchpad", home)
		_ = tmux.RenameWindow("scratchpad:0", "term")
	}
	for _, c := range viewers {
		_ = tmux.SwitchClient(c, "scratchpad")
	}
}

// callerPaneSession returns the tmux session of the pane that runs this
// process, or "" outside a pane.
func callerPaneSession() string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" || os.Getenv("TMUX") == "" {
		return ""
	}
	name, err := tmux.SessionNameOf(pane)
	if err != nil {
		return ""
	}
	return name
}

// waitProcessesGone waits until no process in pids runs. It returns an
// error that names each process that still runs after timeout.
func waitProcessesGone(pids []int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var running []int
		for _, pid := range pids {
			if pid > 0 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				running = append(running, pid)
			}
		}
		if len(running) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("pane process(es) %v still run %s after the tmux kill", running, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
