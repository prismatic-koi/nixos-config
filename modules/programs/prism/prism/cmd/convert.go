package cmd

// prism convert — convert a regular git repo to the prism bare+worktree layout.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/sandboxenv"
	"github.com/prismatic-koi/prism/internal/session"
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
			return errors.Join(fmt.Errorf("conversion failed: %w", err), restartAfterConvert(res, path, false))
		}

		fmt.Printf("done — worktree at %s\n", res.worktreePath)
		return restartAfterConvert(res, path, true)
	},
}

func init() {
	rootCmd.AddCommand(convertCmd)
}

// convertResult is the result of convertPlainClone.
type convertResult struct {
	// worktreePath is the default-branch worktree after a conversion.
	worktreePath string
	// sessionName is the name of the root session of the clone.
	sessionName string
	// ran is true when the tmux session of sessionName ran before the stop.
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
	name, err := session.NameFor(path, "")
	if err != nil {
		return convertResult{}, err
	}
	res := convertResult{sessionName: name}
	if !git.IsRegularRepo(path) {
		return res, fmt.Errorf("convert %s: the dir is not a plain git clone", path)
	}
	// The stop kills each pane of the session, so it also kills this
	// process if it runs in one of them.
	if convertCallerSession() == name {
		return res, fmt.Errorf("convert %s: this command runs in a pane of session %q, and the conversion stops that session. Run the command in a shell outside that session, or use the C-f switcher", path, name)
	}

	lock, err := session.LockForConvert(path, convertLockWait)
	if err != nil {
		return res, err
	}
	defer lock.Release()

	res.ran = session.HasExactSession(name)
	if res.ran {
		progress("  stopping session " + name + "...")
	}
	if err := stopSessionForConvert(name); err != nil {
		return res, fmt.Errorf("convert %s: %w", path, err)
	}

	res.worktreePath, err = convertToBare(path, progress)
	return res, err
}

// restartAfterConvert starts a session again after a prism convert, if the
// root session ran before the stop. After a conversion, the session starts
// in the new worktree. After a failed conversion, the session starts in
// the clone. If the stop failed and the session still runs,
// ensureAndSwitch leaves it as it is.
func restartAfterConvert(res convertResult, path string, converted bool) error {
	if !res.ran {
		return nil
	}
	if converted {
		return convertStartSession(res.worktreePath, path)
	}
	if err := refuseStartBesideBare(path); err != nil {
		return err
	}
	return convertStartSession(path, "")
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
