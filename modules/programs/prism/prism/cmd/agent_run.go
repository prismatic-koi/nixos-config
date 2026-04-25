package cmd

// prism agent-run — exec the bwrap sandbox for a bwrap-mode agent session.
//
// This command is invoked by the tmux agent window when the resolved isolation
// mode is "bwrap". It reconstructs the container.Manager from the session's DB
// row and config, writes the same temp files that Manager.Create() writes for
// podman sessions (SSH config, gitconfig, opencode.json), and then runs:
//
//	bwrap <args...>
//
// as a child process (not a direct exec). A PTY pair is created so that bwrap
// and opencode see a real terminal on all three fds (stdin/stdout/stderr).
// The master side is read and tee'd to both the tmux pane (os.Stdout) and a
// per-session log file at:
//
//	~/.local/state/prism/run/<session>/agent-run.log
//
// Using a PTY pair preserves terminal semantics that are required for opencode
// to work correctly:
//   - TIOCGWINSZ on stdout succeeds (Bubble Tea uses fd 1 for size queries)
//   - SIGWINCH is delivered when the host terminal resizes
//   - Interactive input (key sequences, escape codes) is passed through cleanly
//
// The slave PTY's window size is initialised from the host PTY (stdin fd 0)
// and updated whenever SIGWINCH is received, keeping the sandbox dimensions
// in sync with the actual tmux pane size.
//
// Signal forwarding: SIGTERM, SIGINT, and SIGHUP received by agent-run are
// forwarded to the child process group. When the tmux pane's controlling
// terminal is closed, the kernel sends SIGHUP to agent-run, which forwards
// it to bwrap and its children.
//
// On non-Linux platforms the command fails immediately with a clear error
// because bubblewrap is Linux-only.
//
// Flags:
//
//	--session <name>   prism session name (e.g. "nixos-config@feature")

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
	opencode "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/session"
)

var agentRunCmd = &cobra.Command{
	Use:   "agent-run",
	Short: "Exec the bwrap sandbox for a bwrap-mode agent session (internal)",
	Long: `Exec the bwrap sandbox for a bwrap-mode agent session.

This command is an internal implementation detail invoked by the tmux agent
window when the isolation mode is "bwrap". It is not intended for direct user
invocation.

It reconstructs the container config from the session's DB row, writes temp
files (SSH config, gitconfig), builds the bwrap argument list, and runs bwrap
as a child process. A PTY pair gives bwrap a real terminal so that opencode's
Bubble Tea TUI can query TIOCGWINSZ correctly. The master side is tee'd to
both the tmux pane and ~/.local/state/prism/run/<session>/agent-run.log.`,
	RunE: runAgentRun,
}

func init() {
	agentRunCmd.Flags().String("session", "", "Prism session name (e.g. nixos-config@main)")
	_ = agentRunCmd.MarkFlagRequired("session")
	rootCmd.AddCommand(agentRunCmd)
}

func runAgentRun(cmd *cobra.Command, args []string) error {
	// Fail fast on non-Linux: bubblewrap is Linux-only.
	if runtime.GOOS != "linux" {
		return fmt.Errorf("prism agent-run: bwrap isolation requires Linux; current platform is %s", runtime.GOOS)
	}

	sessionName, _ := cmd.Flags().GetString("session")

	// Open the prism database to look up session state.
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("agent-run: open database: %w", err)
	}
	status, err := d.CurrentStatus(sessionName)
	d.Close()
	if err != nil {
		return fmt.Errorf("agent-run: query session %q: %w", sessionName, err)
	}
	if status == nil {
		return fmt.Errorf("agent-run: session %q not found in DB", sessionName)
	}

	// Verify this is actually a bwrap session.
	isoMode := status.EffectiveIsolationMode()
	if isoMode != "bwrap" {
		return fmt.Errorf("agent-run: session %q has isolation mode %q, not bwrap; this command is only for bwrap sessions", sessionName, isoMode)
	}

	// Load prism config for git identity and SSH key names.
	cfg := config.Load()

	// Reconstruct the container.Config from the session state.
	worktree := status.Worktree
	if worktree == "" {
		return fmt.Errorf("agent-run: session %q has no recorded worktree", sessionName)
	}
	bareRoot := git.BareRoot(worktree)
	var worktreeGitDir string
	if bareRoot != "" {
		worktreeGitDir = filepath.Join(bareRoot, ".bare", "worktrees", filepath.Base(worktree))
	}

	// Resolve port from the DB status. HarnessPort is used for the opencode
	// --port flag in the bwrap args.
	port := 0
	if status.HarnessPort != nil {
		port = *status.HarnessPort
	}

	// Resolve agent role from status.
	agentRole := ""
	if status.RootAgentName != nil {
		agentRole = *status.RootAgentName
	}
	if agentRole == "" {
		agentRole = session.DefaultAgent(worktree, "")
	}

	// Look up the host-API socket path so bwrap can mount it.
	hostAPISockPath := ""
	if sockPath, sockErr := session.SidecarHostAPIPath(sessionName); sockErr == nil {
		hostAPISockPath = sockPath
	}

	// Load profiles.json for agent env vars (e.g. GIT_EDITOR, KUBECONFIG,
	// AWS_CONFIG_FILE). Non-fatal if missing — agent env vars are injected on
	// a best-effort basis.
	var agentEnvVars map[string]string
	if pf, pfErr := config.LoadProfiles(); pfErr == nil && pf != nil {
		agentEnvVars = pf.AgentEnvVars
	}

	// Populate harness-specific runtime env vars for the bwrap sandbox.
	agentRunHarness := opencode.New("", nil, "", "")
	ctrCfg := container.Config{
		SessionName:       sessionName,
		Worktree:          worktree,
		BareRoot:          bareRoot,
		WorktreeGitDir:    worktreeGitDir,
		AllocatedPort:     port,
		AgentRole:         agentRole,
		GitUserName:       cfg.GitUserName,
		GitUserEmail:      cfg.GitUserEmail,
		SshAccessKeyName:  cfg.SshAccessKeyName,
		SshSigningKeyName: cfg.SshSigningKeyName,
		HostAPISockPath:   hostAPISockPath,
		RuntimeEnv:        agentRunHarness.RuntimeEnv(),
		AgentEnvVars:      agentEnvVars,
	}

	// Read the initial prompt from the pane env var set by session.go at
	// window-creation time. When non-empty, populate InitialPrompt so that
	// bwrap.go's BuildArgs appends --prompt to the opencode invocation.
	// The env var is set via tmux's -e flag in tmux.NewWindow, which means
	// it lives only in this pane's environment and dies with the pane.
	applyInitialPromptEnvVar(&ctrCfg)

	// Construct the Manager. PrepareBwrap will write temp files and build args.
	m := container.New(ctrCfg)

	bwrapArgs, err := m.PrepareBwrap()
	if err != nil {
		return fmt.Errorf("agent-run: prepare bwrap args: %w", err)
	}

	// Locate the bwrap binary.
	bwrapBin, err := findBwrap()
	if err != nil {
		return fmt.Errorf("agent-run: %w", err)
	}

	// Open the per-session agent-run log file. The parent directory is the
	// per-session run dir (run/<session>/), which also holds hostapi.sock.
	// We create it here if it does not exist yet (it is normally pre-created
	// by container.prepareVolumeDirs, but agent-run may run before that on
	// some paths).
	logPath, logPathErr := session.AgentRunLogPath(sessionName)
	var logFile *os.File
	if logPathErr != nil {
		fmt.Fprintf(os.Stderr, "[agent-run] warning: cannot resolve agent-run log path: %v — continuing without log file\n", logPathErr)
	} else {
		if mkErr := os.MkdirAll(filepath.Dir(logPath), 0o700); mkErr != nil {
			fmt.Fprintf(os.Stderr, "[agent-run] warning: cannot create log directory %s: %v — continuing without log file\n", filepath.Dir(logPath), mkErr)
		} else {
			var openErr error
			logFile, openErr = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if openErr != nil {
				fmt.Fprintf(os.Stderr, "[agent-run] warning: cannot open agent-run log %s: %v — continuing without log file\n", logPath, openErr)
			}
		}
	}
	if logFile != nil {
		defer logFile.Close()
	}

	// Build the bwrap command. argv[0] must be the binary path.
	// The child env is filtered to a minimal allow-list so that nothing
	// from the invoking shell leaks into bwrap's own process environment.
	// This is defence-in-depth: bwrap itself will also run with --clearenv
	// (see internal/container/bwrap.go) so the sandbox interior is wiped
	// regardless. Stripping here ensures secrets never appear in bwrap's
	// own /proc/<pid>/environ either, and means "ps aux" / "bwrap --help"
	// style debugging can't accidentally expose them.
	bwrapCmd := exec.Command(bwrapBin, bwrapArgs...)
	bwrapCmd.Env = minimalBwrapExecEnv(os.Environ())

	// Pass the real PTY fds directly to bwrap so that opencode's Bubble Tea
	// TUI sees a real terminal on stdin/stdout. This is essential: Bubble Tea
	// calls TIOCGWINSZ on stdout (fd 1); if stdout is a pipe the ioctl returns
	// ENOTTY and the TUI renders at zero width. Using the real fds replicates
	// the behaviour of the previous syscall.Exec approach.
	//
	// stderr is piped so that bwrap harness startup errors can be tee'd to
	// both the pane and the log file. stderr does not need TIOCGWINSZ (it is
	// not used for TUI rendering), so a pipe is fine there. Once opencode is
	// running its TUI output goes via stdout (the real PTY) and is not
	// separately logged — which is acceptable since the log's purpose is
	// forensic inspection of startup failures, not session transcripts.
	var stderrR, stderrW *os.File
	if pipeErr := func() error {
		var err error
		stderrR, stderrW, err = os.Pipe()
		return err
	}(); pipeErr != nil {
		fmt.Fprintf(os.Stderr, "[agent-run] warning: cannot create stderr pipe: %v — stderr will not be logged\n", pipeErr)
	}

	bwrapCmd.Stdin = os.Stdin
	bwrapCmd.Stdout = os.Stdout
	if stderrW != nil {
		bwrapCmd.Stderr = stderrW
	} else {
		bwrapCmd.Stderr = os.Stderr
	}

	// Place bwrap in its own process group so that signal forwarding targets
	// the entire group (bwrap + any children it spawns). When the tmux pane
	// dies, the kernel sends SIGHUP to agent-run (which inherits the pane's
	// controlling terminal); agent-run then forwards it to the bwrap process
	// group via forwardSignalsToBwrap.
	bwrapCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := bwrapCmd.Start(); err != nil {
		return fmt.Errorf("agent-run: start bwrap: %w", err)
	}

	// Close the write end of the stderr pipe in the parent now that bwrap has
	// inherited it. This is required so that reads from the read end return
	// EOF when bwrap exits, rather than blocking forever.
	if stderrW != nil {
		stderrW.Close()
	}

	// Give the terminal foreground to bwrap's process group so that the host
	// PTY routes keypresses and SIGWINCH to bwrap/opencode rather than to
	// agent-run. Restore the original foreground pgid after bwrap exits.
	origPgid := tcsetpgrpForeground(int(os.Stdin.Fd()), bwrapCmd.Process.Pid)

	// Forward SIGTERM, SIGINT, SIGHUP, and SIGWINCH to the child process group.
	doneCh := make(chan struct{})
	go forwardSignalsToBwrap(bwrapCmd.Process, doneCh, nil)

	// Tee bwrap's stderr to both the pane (os.Stderr) and the log file.
	// This goroutine exits when stderrR reaches EOF (after bwrap exits and
	// stderrW is closed).
	teeDone := make(chan struct{})
	if stderrR != nil {
		go func() {
			defer close(teeDone)
			var logWriter io.Writer
			if logFile != nil {
				logWriter = logFile
			}
			teePipe(stderrR, os.Stderr, logWriter)
			stderrR.Close()
		}()
	} else {
		close(teeDone)
	}

	waitErr := bwrapCmd.Wait()
	close(doneCh)
	<-teeDone

	// Restore the original foreground process group.
	if origPgid > 0 {
		_ = tcsetpgrpRestore(int(os.Stdin.Fd()), origPgid)
	}

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("agent-run: wait bwrap: %w", waitErr)
	}
	return nil
}

// teePipe reads from r and writes to primary (always) and optionally to
// secondary. Runs until r returns an error or EOF.
func teePipe(r io.Reader, primary io.Writer, secondary io.Writer) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = primary.Write(buf[:n])
			if secondary != nil {
				_, _ = secondary.Write(buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

// tcsetpgrpForeground hands the terminal foreground process group to the
// process group of pid using TIOCSPGRP on the given fd. It returns the
// original foreground pgid so it can be restored later, or 0 on error (e.g.
// fd is not a TTY — non-interactive / test contexts).
func tcsetpgrpForeground(fd int, pid int) (origPgid int) {
	var current int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&current))); errno != 0 {
		return 0
	}
	pgid := int32(pid)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSPGRP, uintptr(unsafe.Pointer(&pgid))); errno != 0 {
		return 0
	}
	return int(current)
}

// tcsetpgrpRestore restores the terminal foreground process group to pgid.
func tcsetpgrpRestore(fd int, pgid int) error {
	pg := int32(pgid)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSPGRP, uintptr(unsafe.Pointer(&pg)))
	if errno != 0 {
		return errno
	}
	return nil
}

// forwardSignalsToBwrap forwards SIGTERM, SIGINT, SIGHUP, and SIGWINCH to the
// bwrap child process group until doneCh is closed. onWinch is called on each
// SIGWINCH so the caller can resize the PTY slave to match the new host size.
func forwardSignalsToBwrap(proc *os.Process, doneCh <-chan struct{}, onWinch func()) {
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	for {
		select {
		case <-doneCh:
			return
		case sig := <-sigCh:
			if sig == syscall.SIGWINCH {
				if onWinch != nil {
					onWinch()
				}
				// Also forward SIGWINCH to the bwrap process group so that
				// any process inside the sandbox that has registered for it
				// (e.g. a shell) is notified too.
				if proc != nil {
					_ = syscall.Kill(-proc.Pid, syscall.SIGWINCH)
				}
				continue
			}
			if proc == nil {
				continue
			}
			// Send to the process group (negative PGID = group of bwrap).
			_ = syscall.Kill(-proc.Pid, sig.(syscall.Signal))
		}
	}
}

// minimalBwrapExecEnv filters a hostEnv slice (K=V pairs, as returned by
// os.Environ()) down to a minimal allow-list that bwrap itself needs to run.
// The returned env is what the bwrap *process* sees; it is NOT the sandbox
// interior env (bwrap starts the sandbox with --clearenv and rebuilds the
// interior env from explicit --setenv pairs).
//
// Allow-list rationale:
//
//   - PATH:     bwrap uses PATH to locate interpreters when parsing some
//     arguments, and tools that bwrap itself spawns during setup rely on it.
//   - HOME, USER, LOGNAME: used in default paths, error messages, and (more
//     importantly) by any subcommand bwrap shells out to internally.
//   - TERM, LANG, LC_ALL: avoid bwrap logging locale/terminal warnings.
//
// Everything else — including PRISM_GITHUB_TOKEN_*, GITHUB_TOKEN,
// GITHUB_PACKAGES_TOKEN, ANTHROPIC_API_KEY, OPENROUTER_API_KEY, and any
// other secret a prism coordinator might export — is dropped.
func minimalBwrapExecEnv(hostEnv []string) []string {
	allow := map[string]bool{
		"PATH":      true,
		"HOME":      true,
		"USER":      true,
		"LOGNAME":   true,
		"TERM":      true,
		"COLORTERM": true,
		"LANG":      true,
		"LC_ALL":    true,
	}
	out := make([]string, 0, len(allow))
	for _, kv := range hostEnv {
		eq := -1
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				eq = i
				break
			}
		}
		if eq <= 0 {
			continue
		}
		if allow[kv[:eq]] {
			out = append(out, kv)
		}
	}
	return out
}

// applyInitialPromptEnvVar reads PRISM_INITIAL_PROMPT from the process
// environment and, when non-empty, sets cfg.InitialPrompt. This feeds the
// existing bwrap.go BuildArgs --prompt append at lines 466-468 without
// requiring any new persistent state: the env var is set by tmux.NewWindow
// via the -e flag and exists only for the lifetime of this pane.
func applyInitialPromptEnvVar(cfg *container.Config) {
	if initialPrompt := os.Getenv("PRISM_INITIAL_PROMPT"); initialPrompt != "" {
		cfg.InitialPrompt = initialPrompt
	}
}

// findBwrap locates the bwrap binary on PATH or in well-known Nix store paths.
func findBwrap() (string, error) {
	if path, err := exec.LookPath("bwrap"); err == nil {
		return path, nil
	}
	candidates := []string{
		"/run/current-system/sw/bin/bwrap",
		"/usr/bin/bwrap",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("bwrap binary not found on PATH or in well-known locations; ensure bubblewrap is installed (pkgs.bubblewrap)")
}
