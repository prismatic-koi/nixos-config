package cmd

// prism agent-run — exec the sandbox for a bwrap or sandbox-exec mode agent session.
//
// This command is invoked by the tmux agent window when the resolved isolation
// mode is "bwrap" or "sandbox-exec". It reconstructs the container.Manager
// from the session's DB row and config, writes the per-session temp files
// (SSH config, gitconfig,
// opencode.json), and then runs:
//
//	bwrap <args...>        (bwrap mode — supervised child with PTY, Linux-only)
//	sandbox-exec <args...> (sandbox-exec mode — supervised child with kqueue lifecycle, Darwin-only; see #1018)
//
// as a child process (not a direct exec). A PTY pair is created so that bwrap
// and the agent sees a real terminal on all three fds (stdin/stdout/stderr).
// The master side is read and tee'd to both the tmux pane (os.Stdout) and a
// per-session log file at:
//
//	~/.local/state/prism/run/<session>/agent-run.log
//
// Using a PTY pair preserves terminal semantics that are required for the agent
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
// On non-Linux platforms the bwrap path fails immediately with a clear error
// because bubblewrap is Linux-only. The sandbox-exec path requires macOS;
// the platform guard in spawn.go intercepts sandbox-exec requests on non-Darwin
// before agent-run is reached.
//
// Flags:
//
//	--session <name>   prism session name (e.g. "nixos-config@feature")
//	--model <id>       model identifier override (overrides profile slot's model;
//	                    sourced from `prism spawn --model`, issue #2086)
//	--variant <name>   variant/thinking-level override (overrides profile slot's
//	                    thinking; sourced from `prism spawn --variant`, issue #2086)

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
)

// errAgentRunNoStatus is the error a registered AgentRun handler returns
// when the dispatcher has not stashed a DB status in the per-session cache.
// In practice this should not happen — runAgentRun stashes the status
// before calling iso.AgentRun — but the handler surfaces a clear error
// rather than panicking on a nil pointer.
func errAgentRunNoStatus(sessionName string) error {
	return fmt.Errorf("agent-run: session %q: status cache is empty (programming error)", sessionName)
}

var agentRunCmd = &cobra.Command{
	Use:   "agent-run",
	Short: "Exec the sandbox for a bwrap or sandbox-exec mode agent session (internal)",
	Long: `Exec the sandbox for a bwrap or sandbox-exec mode agent session.

This command is an internal implementation detail invoked by the tmux agent
window when the isolation mode is "bwrap" or "sandbox-exec". It is not intended
for direct user invocation.

It reconstructs the container config from the session's DB row, writes temp
files (SSH config, gitconfig), builds the sandbox argument list, and runs the
sandbox as a child process. A PTY pair gives the sandbox a real terminal so
that the agent TUI can query TIOCGWINSZ correctly. The master side
is tee'd to both the tmux pane and ~/.local/state/prism/run/<session>/agent-run.log.`,
	RunE: runAgentRun,
}

func init() {
	agentRunCmd.Flags().String("session", "", "Prism session name (e.g. nixos-config@main)")
	_ = agentRunCmd.MarkFlagRequired("session")
	// --model / --variant: CLI overrides forwarded from `prism spawn` via the
	// tmux pane command (issue #2086). When set, populatePIConfig uses these
	// values in place of the active profile slot's Model / Thinking. Empty
	// values fall back to the slot, matching the pre-#2086 behaviour.
	agentRunCmd.Flags().String("model", "", "Model identifier override (overrides profile slot's model; sourced from prism spawn --model)")
	agentRunCmd.Flags().String("variant", "", "Variant/thinking-level override (overrides profile slot's thinking; sourced from prism spawn --variant)")
	// --provider: CLI override forwarded from `prism spawn --provider` via the
	// tmux pane command (issue #2852). When set, populatePIConfig uses this
	// value in place of the active profile slot's Provider. Empty value falls
	// back to the slot, matching the pre-#2852 behaviour.
	agentRunCmd.Flags().String("provider", "", "Provider override (overrides profile slot's provider; sourced from prism spawn --provider)")
	rootCmd.AddCommand(agentRunCmd)

	// Register the per-mode AgentRun handlers with the container package
	// (issue #1140 A1.L6). The dispatch in cmd/agent_run.go's runAgentRun
	// resolves the persisted isolation mode from the DB, looks up the
	// registered isolator via container.For(mode, ...), and calls
	// iso.AgentRun(ctx, opts) — which routes back here via the registered
	// handlers below.
	//
	// Keeping the handler bodies in the cmd package (rather than moving
	// them into internal/container) is a deliberate scope decision: moving
	// them would create a circular dependency with internal/session,
	// internal/git, internal/harness, and internal/db, none of which the
	// container package imports today. Registration at init() time keeps
	// the dispatch shape (`iso.AgentRun(ctx, opts)`) without re-plumbing
	// the dependency graph.
	container.RegisterAgentRunHandler(config.IsolationBwrap, runAgentRunBwrapHandler)
	container.RegisterAgentRunHandler(config.IsolationSandboxExec, runAgentRunSandboxExecHandler)
}

func runAgentRun(cmd *cobra.Command, args []string) error {
	// Capture wall-clock entry time so that the bwrap and sandbox-exec
	// dispatch paths can emit `[timing]` markers symmetric to the
	// sidecar-side instrumentation (see internal/sidecar/sidecar.go
	// "from start" lines). All `[timing]` durations from agent-run are
	// relative to this point, which is the closest analogue to the sidecar's
	// `sessionStart` marker (taken when sidecar Run() begins).
	agentRunStart := time.Now()

	sessionName, _ := cmd.Flags().GetString("session")
	modelOverride, _ := cmd.Flags().GetString("model")
	variantOverride, _ := cmd.Flags().GetString("variant")
	providerOverride, _ := cmd.Flags().GetString("provider")

	// Open the agent-run log file as early as possible so that pre-exec
	// `[timing]` markers can be written to it and the failure point is
	// locatable even when later setup fails (e.g. bwrap exec returns ENOENT).
	// The log file is also tee-target for bwrap stderr further down; opening
	// here means there is no second open call.
	logFile := openAgentRunLog(sessionName)
	if logFile != nil {
		defer logFile.Close()
	}

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

	// Dispatch based on the session's isolation mode.
	//
	// Post A1.L6 (issue #1140): the per-mode switch collapses to a single
	// container.For(mode).AgentRun(ctx, opts) call. host is not a
	// valid mode for `prism agent-run`; the registered isolator returns
	// the original "this command is only for bwrap and sandbox-exec
	// sessions" error verbatim, replacing the manual `else` arm.
	isoMode := config.IsolationMode(status.IsolationMode)

	// Belt-and-braces platform guard. The container.For-resolved isolator's
	// AgentRun body would also fail eventually (bubblewrap is Linux-only,
	// sandbox-exec is macOS-only) but the platform check here surfaces a
	// clearer error before the binary lookup. Mirrors the pre-refactor
	// switch's runtime.GOOS gates.
	switch isoMode {
	case config.IsolationBwrap:
		if runtime.GOOS != "linux" {
			return fmt.Errorf("prism agent-run: bwrap isolation requires Linux; current platform is %s", runtime.GOOS)
		}
	case config.IsolationSandboxExec:
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("prism agent-run: sandbox-exec isolation requires macOS (Darwin); current platform is %s", runtime.GOOS)
		}
	}

	iso, err := container.For(isoMode, container.ConstructorOpts{Name: sessionName})
	if err != nil {
		return fmt.Errorf("agent-run: %w", err)
	}

	// The handler stashes the looked-up status on the package-level cache
	// so the registered handlers (which only get AgentRunOpts) can read it
	// without re-querying the DB. The cache is per-session-name and is
	// cleared by the handler when it is done.
	storeAgentRunStatus(sessionName, status)
	defer clearAgentRunStatus(sessionName)

	// Stash CLI overrides (--model / --variant) so per-mode handlers can
	// read them without re-parsing argv. Cleared by clearAgentRunStatus.
	// Empty fields mean "no override" — the active profile slot's value
	// is used unchanged (issue #2086).
	storeAgentRunOverrides(sessionName, piOverrides{Model: modelOverride, Variant: variantOverride, Provider: providerOverride})

	return iso.AgentRun(cmd.Context(), container.AgentRunOpts{
		SessionName: sessionName,
		StartTime:   agentRunStart,
		LogFile:     logFile,
	})
}

// runAgentRunBwrapHandler is the registered AgentRun handler for the bwrap
// isolation mode (issue #1140 A1.L6). It is the body that previously lived
// in the bwrap arm of runAgentRun's switch statement. The handler reads the
// pre-looked-up DB status from the package-level cache populated by
// runAgentRun.
func runAgentRunBwrapHandler(ctx context.Context, opts container.AgentRunOpts) error {
	sessionName := opts.SessionName
	agentRunStart := opts.StartTime
	logFile := opts.LogFile
	status := loadAgentRunStatus(sessionName)
	if status == nil {
		return errAgentRunNoStatus(sessionName)
	}

	// Load prism config for git identity and SSH key names.
	cfg := config.Load()

	// Reconstruct the container.Config from the session state.
	worktree := status.Worktree
	if worktree == "" {
		return fmt.Errorf("agent-run: session %q has no recorded worktree", sessionName)
	}
	bareRoot := git.BareRoot(worktree)
	worktreeGitDir, err := git.ResolveWorktreeGitDir(worktree)
	if err != nil && !errors.Is(err, git.ErrNotAWorktree) {
		return fmt.Errorf("agent-run: session %q: resolve worktree git dir: %w", sessionName, err)
	}
	if errors.Is(err, git.ErrNotAWorktree) {
		// Not a prism bare+worktree layout (e.g. a normal git clone) —
		// leave worktreeGitDir empty. The bwrap bind logic already guards
		// on BareRoot/WorktreeGitDir being non-empty (internal/container/
		// bwrap.go), so this is a no-op there rather than a broken bind.
		worktreeGitDir = ""
	}

	// Resolve port from the DB status. HarnessPort is used for the harness
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
	// AWS_CONFIG_FILE), filtered for the session role. Non-fatal if missing —
	// agent env vars are injected on a best-effort basis.
	//
	// The role filter runs here, upstream of the isolator (issue #2533): the
	// map handed to container.Config already has the keys this role must not
	// receive removed, so the isolator stays role-agnostic and keeps emitting
	// every key it is given (the #2235 invariant).
	agentEnvVars := config.AgentEnvVarsForRole(agentRole)

	// Resolve the harness name from the DB status. Fall back to "pi"
	// for pre-registry rows that have a NULL harness column.
	harnessName := "pi"
	if status.Harness != nil && *status.Harness != "" {
		harnessName = *status.Harness
	}

	// Look up the harness pipe socket path for socket-pipe harnesses (e.g. "pi").
	// This must be set on ctrCfg so that bwrap.go's BuildArgs emits
	// --setenv PRISM_HARNESS_PIPE unix://<path>. Without it the PI binary starts
	// inside the sandbox without knowing where the sidecar pipe socket is.
	harnessPipeSockPath := ""
	if shape, ok := harness.ShapeOf(harnessName); ok && shape == harness.TransportSocketPipe {
		pipePath, pipeErr := session.SidecarHarnessPipePath(sessionName)
		if pipeErr != nil {
			return fmt.Errorf("agent-run: resolve harness pipe path for %q: %w", harnessName, pipeErr)
		}
		harnessPipeSockPath = pipePath
	}

	// Resolve the host-side agent-run log path so bwrap can expose it as
	// PRISM_AGENT_RUN_LOG — the durable target for the PI extension's
	// first-connect give-up diagnostic (issue #2357). Non-fatal on error:
	// the extension falls back to pane-scrollback-only logging.
	agentRunLogPath := ""
	if p, logPathErr := session.AgentRunLogPath(sessionName); logPathErr == nil {
		agentRunLogPath = p
	}

	// Populate harness-specific runtime env vars for the bwrap sandbox.
	// harnessName comes from the DB; if it is not registered, fall back to
	// a zero-env map rather than failing the entire agent-run.
	agentRunHarness, _ := harness.New(harnessName, "", nil, "", "")
	var runtimeEnv map[string]string
	if agentRunHarness != nil {
		runtimeEnv = agentRunHarness.RuntimeEnv()
	}
	// Carry the persisted harness session UUID through to ctrCfg so that
	// PIInvocation can append --session <id> for conversation resume on
	// restore (issue #1838). Empty when the harness never started or this is
	// a spawn/switch path — PIInvocation treats empty as a silent no-op.
	harnessSessionID := ""
	if status.HarnessSessionID != nil {
		harnessSessionID = *status.HarnessSessionID
	}

	// Resolve InstanceID from DB status. Required so the per-session work dir
	// is namespaced by the same instance_id that prism cleanup uses (#2317 /
	// #2321: bwrap with containers_enabled needs the work dir for the
	// container-scratch bind mount). Sandbox-exec already populates this on
	// the same path (cmd/agent_run_sandbox_exec_darwin.go).
	instanceID := ""
	if status.InstanceID != nil {
		instanceID = *status.InstanceID
	}

	// Resolve the filtered podman API socket path so bwrap can emit
	// CONTAINER_HOST / DOCKER_HOST when containers_enabled is set. Resolution
	// failure is non-fatal at this layer — PrepareBwrap re-validates the
	// containers_enabled invariants and produces the surfaced error.
	podmanProxySockPath := ""
	if p, podmanErr := session.SidecarPodmanProxyPath(sessionName); podmanErr == nil {
		podmanProxySockPath = p
	}

	ctrCfg := container.Config{
		SessionName:         sessionName,
		Worktree:            worktree,
		BareRoot:            bareRoot,
		WorktreeGitDir:      worktreeGitDir,
		AllocatedPort:       port,
		AgentRole:           agentRole,
		GitUserName:         cfg.GitUserName,
		GitUserEmail:        cfg.GitUserEmail,
		SshAccessKeyName:    cfg.SshAccessKeyName,
		SshSigningKeyName:   cfg.SshSigningKeyName,
		GitHubTokenPath:     cfg.GitHubTokenPath,
		GitHubTokenPaths:    cfg.GitHubTokenPaths,
		GitLabTokenPath:     cfg.GitLabTokenPath,
		HostAPISockPath:     hostAPISockPath,
		HarnessPipeSockPath: harnessPipeSockPath,
		AgentRunLogPath:     agentRunLogPath,
		InstanceID:          instanceID,
		ContainersEnabled:   status.ContainersEnabled,
		PodmanProxySockPath: podmanProxySockPath,
		RuntimeEnv:          runtimeEnv,
		AgentEnvVars:        agentEnvVars,
		Harness:             harnessName,
		HarnessSessionID:    harnessSessionID,
	}

	// PI-harness: populate PI-specific config fields from the active profile slot.
	if harnessName == "pi" {
		if piErr := populatePIConfig(&ctrCfg, sessionName, agentRole, cfg, loadAgentRunOverrides(sessionName)); piErr != nil {
			return fmt.Errorf("agent-run: %w", piErr)
		}
	}

	// Read the initial prompt from the pane env var set by session.go at
	// window-creation time. When non-empty, populate InitialPrompt so that
	// bwrap.go's BuildArgs appends --prompt to the agent invocation.
	// The env var is set via tmux's -e flag in tmux.NewWindow, which means
	// it lives only in this pane's environment and dies with the pane.
	applyInitialPromptEnvVar(&ctrCfg)

	// Construct the Manager. PrepareBwrap will write temp files and build args.
	m := container.New(ctrCfg)

	// `[timing] pre-exec`: covers everything between agent-run entry and the
	// start of bwrap-args assembly — DB lookup, config load, profile load,
	// Manager construction. Symmetric to the sidecar's `[timing] pre-Create`
	// in internal/sidecar/sidecar.go.
	logTimingTo(logFile, "pre-exec", time.Since(agentRunStart))

	argsBuildStart := time.Now()
	bwrapArgs, err := m.PrepareBwrap()
	if err != nil {
		return fmt.Errorf("agent-run: prepare bwrap args: %w", err)
	}
	// `[timing] bwrap-args build`: time spent assembling the bwrap argv plus
	// writing the per-session SSH config / gitconfig / opencode.json temp files
	// (PrepareBwrap does both). Emitted before the binary lookup and exec so
	// that an exec-stage failure still leaves this marker in the agent-run log.
	logTimingTo(logFile, "bwrap-args build", time.Since(argsBuildStart))

	// Locate the bwrap binary.
	bwrapBin, err := findBwrap()
	if err != nil {
		return fmt.Errorf("agent-run: %w", err)
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

	// Pass the real PTY fds directly to bwrap so that the agent's Bubble Tea
	// TUI sees a real terminal on stdin/stdout. This is essential: Bubble Tea
	// calls TIOCGWINSZ on stdout (fd 1); if stdout is a pipe the ioctl returns
	// ENOTTY and the TUI renders at zero width. Using the real fds replicates
	// the behaviour of the previous syscall.Exec approach.
	//
	// stderr is piped so that bwrap harness startup errors can be tee'd to
	// both the pane and the log file. stderr does not need TIOCGWINSZ (it is
	// not used for TUI rendering), so a pipe is fine there. Once the agent is
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
	// group via SuperviseChild (see cmd/supervise.go).
	bwrapCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// `[timing] bwrap exec`: total wall time from agent-run entry to the
	// moment bwrap is started (fork+exec under exec.Cmd.Start). The phase
	// name follows the AC text — we use exec.Cmd.Start rather than
	// syscall.Exec because agent-run supervises the child for PTY/signal
	// handling, but the wall-time semantic is identical: this is the point
	// at which control passes to the bwrap binary. Sidecar-side markers
	// (`[timing] agent listening: <d> from start`) measure from sidecar
	// spawn, not from this point, so the two timelines are independent —
	// stitch them via wall-clock from each log to bound "exec → agent up".
	logTimingTo(logFile, "bwrap exec", time.Since(agentRunStart))

	// Layer 1 FD isolation (#2190): cap the sandbox child's RLIMIT_NOFILE so
	// a misbehaving agent cannot exhaust the host-wide FD pool. The caps are
	// applied to this process immediately before Start() and restored
	// immediately after — the child inherits them at fork time, while the
	// parent's own FD bookkeeping (PTY, stderr pipe, log file) is unaffected.
	// Warnings (host-hard clamping, setrlimit failures) go to the agent-run
	// log; failures never abort the spawn.
	restoreRlimit := container.ApplyAgentRlimitNofile(
		cfg.AgentMaxOpenFilesSoft, cfg.AgentMaxOpenFilesHard,
		func(format string, args ...any) { logAgentRunWarning(logFile, format, args...) })

	if err := bwrapCmd.Start(); err != nil {
		restoreRlimit()
		return fmt.Errorf("agent-run: start bwrap: %w", err)
	}
	restoreRlimit()

	// Close the write end of the stderr pipe in the parent now that bwrap has
	// inherited it. This is required so that reads from the read end return
	// EOF when bwrap exits, rather than blocking forever.
	if stderrW != nil {
		stderrW.Close()
	}

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

	// Supervise the child: foreground the pgid, forward signals (including
	// SIGWINCH for the bwrap path so resizes propagate to the sandbox), and
	// wait. The shared SuperviseChild helper (supervise.go, A2.SUP) replaces
	// the previous open-coded tcsetpgrpForeground / signal-forwarder /
	// cmd.Wait / tcsetpgrpRestore sequence — same behaviour, single
	// implementation across the bwrap and sandbox-exec dispatch paths.
	waitErr := SuperviseChild(bwrapCmd, int(os.Stdin.Fd()), SuperviseOpts{
		ForwardWinch: true,
		OnWinch:      nil,
	})
	<-teeDone

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
//
// The implementation is a thin alias over container.MinimalIsolatedExecEnv
// (the same logic is reused by the sandbox-exec path). Both call sites share
// a single helper because the filter is identical across modes.
func minimalBwrapExecEnv(hostEnv []string) []string {
	return container.MinimalIsolatedExecEnv(hostEnv)
}

// applyInitialPromptEnvVar populates cfg.InitialPrompt from the agent pane's
// environment. Two delivery shapes are supported:
//
//  1. PRISM_INITIAL_PROMPT_FILE (post-#1092): the env var holds a filesystem
//     path. agent-run reads the file and uses its contents as the prompt.
//     This is the shape SpawnSession uses for bwrap/sandbox-exec sessions
//     so the prompt body never has to fit on tmux's `new-window -e` argv —
//     the file lives in the per-session run directory next to
//     agent-startup.log / agent-run.log / hostapi.sock.
//
//  2. PRISM_INITIAL_PROMPT (pre-#1092 fallback): the env var carries the
//     prompt body inline. Honoured for back-compat with any pane env that
//     was set up before #1092 (and for direct callers of agent-run that
//     have not migrated yet).
//
// Precedence — `PRISM_INITIAL_PROMPT_FILE` wins outright when set:
//
//   - If both env vars are set, the file path is used and the inline
//     value is ignored. A stale inline value from a re-attached pane
//     must not override the fresh path the spawner just wrote.
//
//   - If `PRISM_INITIAL_PROMPT_FILE` is set but the file read fails, the
//     inline value is NOT consulted as a fallback. The contract is "FILE
//     takes precedence absolutely" — silently substituting an inline
//     value would mask a real failure (e.g. the spawner wrote the file
//     but it was deleted, or perms were tampered with). agent-run logs
//     the read error to stderr and proceeds with no initial prompt.
//
// File-read failures are not fatal to agent-run itself. The pane is
// already alive; failing here would leave the operator with a dead review
// window and no way to recover. The empty-prompt outcome is no worse than
// the pre-#1042 behaviour where review agents started without a prompt at
// all.
func applyInitialPromptEnvVar(cfg *container.Config) {
	if path := os.Getenv("PRISM_INITIAL_PROMPT_FILE"); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"[agent-run] warning: read PRISM_INITIAL_PROMPT_FILE %q: %v — starting agent without an initial prompt\n",
				path, err)
			return
		}
		cfg.InitialPrompt = string(body)
		return
	}
	if initialPrompt := os.Getenv("PRISM_INITIAL_PROMPT"); initialPrompt != "" {
		cfg.InitialPrompt = initialPrompt
	}
}

// validateSandboxExecArgs checks that the args returned by
// Manager.PrepareSandboxExec have the expected shape and that the profile
// path on disk is readable. It exists as a separate function so the
// edge-case AC ("missing/unreadable profile-temp path returns a clear error
// and does not exec") can be unit-tested without needing a Darwin host or
// real /usr/bin/sandbox-exec binary.
//
// Returned errors are wrapped with the `agent-run:` prefix used by the
// surrounding command so they appear coherently in the agent-run log.
func validateSandboxExecArgs(args []string) error {
	if len(args) < 4 || args[0] != "sandbox-exec" || args[1] != "-f" {
		return fmt.Errorf("agent-run: sandbox-exec args have unexpected shape (len=%d): %v", len(args), args)
	}
	profilePath := args[2]
	if profilePath == "" {
		return fmt.Errorf("agent-run: sandbox-exec profile path is empty")
	}
	if _, statErr := os.Stat(profilePath); statErr != nil {
		return fmt.Errorf("agent-run: sandbox-exec profile %s is missing or unreadable: %w", profilePath, statErr)
	}
	return nil
}

// runAgentRunSandboxExec is defined in agent_run_sandbox_exec_darwin.go on
// Darwin (where sandbox-exec is available) and has a non-Darwin stub in
// agent_run_sandbox_exec_other.go. The dispatch in runAgentRun already guards
// against non-Darwin via a runtime.GOOS check, so the stub is unreachable in
// practice — it exists only to satisfy the compiler on non-Darwin platforms.

// openAgentRunLog opens the per-session agent-run log file in append mode for
// the bwrap and sandbox-exec dispatch paths. It encapsulates the resolve →
// mkdir → open dance previously inlined in runAgentRun, and is reused by both
// dispatch paths so they share a single log destination for `[timing]` markers
// and bwrap stderr tee output.
//
// Returns nil on any failure (path-resolve, mkdir, open). All failures are
// reported via stderr warnings — this function never returns an error so that
// agent-run can continue running an isolated sandbox even when the host log
// dir is unwritable. Callers must handle a nil return from this function
// (logTimingTo also tolerates nil).
//
// The destination is normally `~/.local/state/prism/run/<session>/agent-run.log`
// (see internal/session.AgentRunLogPath). The file is opened with O_APPEND so
// repeated agent-run invocations across a single session preserve history.
func openAgentRunLog(sessionName string) *os.File {
	logPath, logPathErr := session.AgentRunLogPath(sessionName)
	if logPathErr != nil {
		fmt.Fprintf(os.Stderr, "[agent-run] warning: cannot resolve agent-run log path: %v — continuing without log file\n", logPathErr)
		return nil
	}
	if mkErr := os.MkdirAll(filepath.Dir(logPath), 0o700); mkErr != nil {
		fmt.Fprintf(os.Stderr, "[agent-run] warning: cannot create log directory %s: %v — continuing without log file\n", filepath.Dir(logPath), mkErr)
		return nil
	}
	logFile, openErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if openErr != nil {
		fmt.Fprintf(os.Stderr, "[agent-run] warning: cannot open agent-run log %s: %v — continuing without log file\n", logPath, openErr)
		return nil
	}
	return logFile
}

// logTimingTo writes a single `[timing] <phase>: <duration>` line to both the
// per-session agent-run log file (when non-nil) and stderr. The format matches
// the sidecar-side `[timing]` markers (see internal/sidecar/sidecar.go and
// internal/container/container.go) so the two timelines grep coherently:
//
//	grep '\[timing\]' ~/.local/state/prism/run/<session>/agent-run.log
//	grep '\[timing\]' ~/.local/state/prism/logs/<session>-sidecar.log
//
// Durations are rounded to milliseconds for readability — sub-millisecond
// noise on these phases is rarely actionable. Errors writing to the log file
// are silently ignored; the stderr write is best-effort too. The intent is
// instrumentation that never blocks or fails the launch path.
func logTimingTo(logFile *os.File, phase string, d time.Duration) {
	line := fmt.Sprintf("[timing] %s: %s\n", phase, d.Round(time.Millisecond))
	// Stderr is gated through proglog so the line is visible only when
	// PRISM_LOG_LEVEL=debug; the file write below is unconditional so the
	// per-session agent-run.log keeps every [timing] marker for post-mortem
	// `grep '\[timing\]'` diagnostics (see issue #1818).
	proglog.Debugf("%s", line)
	if logFile != nil {
		_, _ = logFile.WriteString(line)
	}
}

// logAgentRunWarning writes a single "[agent-run] warning: ..." line to both
// stderr (the tmux pane) and the per-session agent-run log file (when
// non-nil). Mirrors logTimingTo's dual-destination shape so warnings are
// greppable post-mortem in agent-run.log alongside the [timing] markers.
// Used by the #2190 RLIMIT_NOFILE clamp/apply path on both the bwrap and
// sandbox-exec dispatch paths.
func logAgentRunWarning(logFile *os.File, format string, args ...any) {
	line := fmt.Sprintf("[agent-run] warning: "+format+"\n", args...)
	fmt.Fprint(os.Stderr, line)
	if logFile != nil {
		_, _ = logFile.WriteString(line)
	}
}

// piOverrides bundles the CLI override values that `prism agent-run` accepts
// for the pi harness (issue #2086). Empty fields mean "no override" and the
// active profile slot's value is used unchanged. A non-empty Model wins over
// slot.Model; a non-empty Variant wins over slot.Thinking (pi consumes both
// via --model and --thinking on its argv in PIInvocation).
type piOverrides struct {
	Model   string
	Variant string

	// Provider, when non-empty, wins over slot.Provider before PIInvocation
	// emits --provider on pi's argv (issue #2852).
	Provider string
}

// populatePIConfig fills the PI-specific fields on ctrCfg for harness=pi sessions.
//
// It:
//  1. Loads profiles.json.
//  2. Resolves the profile for this session using the precedence
//     spawn_inputs.profile_name (#2090) > runtime state file > nix-default.
//     The DB lookup is the post-#2092 fix for the silent-drop of
//     `prism spawn --profile <X>` / `--abtest <A> <B>`: spawn-time profile
//     choice is now read back from the audit row that #2090 writes for every
//     session, so `--abtest` legs run on their own profile slot instead of
//     the active profile's slot. When the DB has no row (legacy sessions,
//     paths that legitimately don't write spawn_inputs, or transient lookup
//     errors), the function falls through to the existing
//     state-file / nix-default resolution unchanged.
//  3. Looks up the slot for the session's agent role — returns a clear error if
//     the profile does not define a slot for this role.
//  4. Calls EnsurePIAgentConfigDir to resolve the shared host ~/.pi/agent path
//     (creating it if absent on a fresh install) and records the host/sandbox
//     paths on ctrCfg so bwrap can bind-mount the directory and set
//     PI_CODING_AGENT_DIR. The shared mount carries settings.json, themes/,
//     AGENTS.md, skills/, and auth.json — all identical across sessions —
//     since design #2031 PR3 (#2034) collapsed the per-session staging dir.
//     The role system-prompt is injected at runtime by the prism PI extension,
//     not staged here.
//  5. Populates PIExtensionHostDir from cfg.PIExtensionDir (set by Nix).
//  6. Copies PIProvider, PIModel, PIThinking from the profile slot, then
//     applies any CLI overrides from `prism agent-run --model` /
//     `--variant` / `--provider` (issues #2086 and #2852). A non-empty Model
//     override wins over slot.Model; a non-empty Variant override wins over
//     slot.Thinking; a non-empty Provider override wins over slot.Provider.
//     Empty override fields leave the slot values unchanged so the pre-#2086
//     default path is preserved.
func populatePIConfig(ctrCfg *container.Config, sessionName, agentRole string, cfg config.Config, overrides piOverrides) error {
	// Load profiles.json.
	pf, pfErr := config.LoadProfiles()
	if pfErr != nil {
		return fmt.Errorf("pi: load profiles: %w", pfErr)
	}

	// Resolve the profile for this session.
	//
	// Precedence (post-#2092): spawn_inputs.profile_name > state file > nix-default.
	//
	// Before #2092 we passed "" as the flag value, so `prism spawn --profile <X>`
	// and both legs of `prism spawn --abtest <A> <B>` were silently substituted
	// for the active profile. The #2090 audit row now carries the spawn-time
	// profile per instance_id, so reading it back here makes the spawn-time
	// choice authoritative without re-plumbing a CLI flag on `agent-run`.
	//
	// When the lookup returns "" (no spawn_inputs row, NULL profile_name, or a
	// transient DB error) we fall through to the existing state-file /
	// nix-default path. This preserves restart / restore semantics for legacy
	// sessions that pre-date #2090 and the host-mode path that does not call
	// this function at all.
	spawnProfile := spawnTimeProfileForSession(sessionName)
	profileName, _, err := config.ResolveActiveProfile(pf, spawnProfile)
	if err != nil {
		return fmt.Errorf("pi: resolve active profile: %w", err)
	}
	if profileName == "" {
		return fmt.Errorf("pi: no active profile found — set a profile with `prism profile set <name>` or configure a default in profiles.json")
	}

	// Require that the active profile defines a slot for this role.
	if slotErr := config.RequireSlot(pf, profileName, agentRole); slotErr != nil {
		return fmt.Errorf("pi: %w", slotErr)
	}
	slot, _ := config.SlotForRole(pf, profileName, agentRole)

	// Resolve the shared PI agent config directory (~/.pi/agent on the host)
	// and the canonical in-sandbox path (/run/prism/pi-agent). Bwrap will
	// bind-mount the host dir READ-WRITE at the sandbox path; writes to
	// auth.json reach the host file via that same parent bind. RW (not RO)
	// is load-bearing for OAuth refresh — proper-lockfile mkdir's
	// auth.json.lock on the parent dir, which would EPERM under an RO mount
	// (see pi_invocation.go top-of-file for the full rationale). A redundant
	// host-path RW bind of auth.json is also retained so $HOME-resolving
	// call paths inside the sandbox keep working. On sandbox-exec, the
	// in-sandbox path is overridden in the dispatcher to equal the host
	// path because sandbox-exec shares the host filesystem. Design #2031,
	// PR3 (#2034).
	//
	// EnsurePIAgentConfigDir creates the host dir if absent so a fresh install
	// does not fail the spawn. sessionName is intentionally unused here — the
	// shared mount is identical for every session.
	_ = sessionName
	hostDir, sandboxDir, err := container.EnsurePIAgentConfigDir()
	if err != nil {
		return fmt.Errorf("pi: resolve agent config dir: %w", err)
	}
	ctrCfg.PIAgentConfigHostDir = hostDir
	ctrCfg.PIAgentConfigSandboxDir = sandboxDir

	// Extension host directory from Nix-written config.
	if cfg.PIExtensionDir == "" {
		return fmt.Errorf("pi: PIExtensionDir is not set in prism config — ensure the prism PI extension is configured in Nix (piExtensionDir in config.json)")
	}
	ctrCfg.PIExtensionHostDir = cfg.PIExtensionDir

	// Model/provider/thinking from the profile slot, then CLI overrides win.
	//
	// Issue #2086: `prism spawn --model` / `--variant` flow through to
	// `prism agent-run` as explicit flags (threaded via AgentPaneOpts in the
	// tmux pane command). When non-empty they replace the slot's value here,
	// before PIInvocation reads PIModel / PIThinking. Empty override fields
	// fall through to the slot value unchanged.
	ctrCfg.PIProvider = slot.Provider
	ctrCfg.PIModel = slot.Model
	ctrCfg.PIThinking = slot.Thinking
	if overrides.Model != "" {
		ctrCfg.PIModel = overrides.Model
	}
	if overrides.Variant != "" {
		ctrCfg.PIThinking = overrides.Variant
	}
	if overrides.Provider != "" {
		ctrCfg.PIProvider = overrides.Provider
	}

	// Resolve the pi binary path. This must be the absolute store
	// path (or profile path) so that bwrap can bind-mount it into the sandbox.
	// A missing binary is a hard error: a silent fallback to a bare name would
	// result in ENOENT inside bwrap and the session exiting in milliseconds
	// with no useful error message.
	piBin, lookErr := exec.LookPath("pi")
	if lookErr != nil {
		return fmt.Errorf("pi: resolve pi binary: %w — ensure pi is installed and on PATH", lookErr)
	}
	// Resolve symlinks so bwrap gets the real nix store path, not a profile
	// symlink that does not exist inside the sandbox namespace.
	if resolved, evalErr := filepath.EvalSymlinks(piBin); evalErr == nil {
		piBin = resolved
	}
	ctrCfg.PIBinaryPath = piBin

	return nil
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
