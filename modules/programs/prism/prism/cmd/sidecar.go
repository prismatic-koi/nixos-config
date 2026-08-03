package cmd

// prism sidecar — long-running SSE consumer that drives agent state
// transitions.
//
// Flags:
//
//	--session <name>           prism session name (e.g. "nixos-config@main")
//	--harness-url <url>        base URL of the harness HTTP endpoint
//	                           (e.g. "http://localhost:14000")
//	--isolation-mode <mode>    isolation mode: "bwrap", "sandbox-exec", or
//	                           "host" (default: "host" when absent); see
//	                           config.ValidIsolationModes
//	--agent-role <role>        "worker" or "coordinator" (used to select the
//	                           correct GitHub token; when empty, the role is
//	                           inferred from events at runtime)
//	--port <n>                 allocated host port (required in bwrap/
//	                           sandbox-exec mode)
//
// The sidecar connects to <harness-url>/event and maps harness events to
// agent state transitions, writing them to prism.db. It handles idle debounce,
// permission tracking, and dashboard sentinel updates.
//
// In bwrap and sandbox-exec mode, the sandbox is launched and owned by the
// tmux pane via "prism agent-run". The sidecar sets up the host-API Unix
// socket, selects the container-mode harness adapter, and runs the full SSE
// loop.
//
// In host mode (--isolation-mode=host or no flag), the sidecar connects to an
// already-running agent process. No sandbox management is performed.
//
// Clean shutdown: SIGINT and SIGTERM write "interrupted" state before exiting.

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/proglog"
	prismSession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar"
)

var sidecarCmd = &cobra.Command{
	Use:   "sidecar",
	Short: "Harness event sidecar for agent session management",
	Long: `Connects to the harness event stream and maps events to agent
state transitions in prism.db. Designed to be started alongside the agent harness
by prism spawn.

The sidecar handles: state machine transitions, idle debounce (2s),
permission tracking, event logging, and dashboard sentinel updates.

In bwrap and sandbox-exec mode, the sidecar sets up the host-API Unix socket
and runs the full SSE loop. The sandbox itself is owned by the tmux pane via
"prism agent-run". In host mode the sidecar connects to an already-running
agent process.`,
	RunE: runSidecar,
}

func init() {
	sidecarCmd.Flags().String("session", "", "Prism session name (e.g. nixos-config@main)")
	sidecarCmd.Flags().String("harness-url", "", "Base URL of the harness HTTP server (used by HTTP-transport harnesses)")
	sidecarCmd.Flags().String("isolation-mode", "", "Isolation mode: bwrap, sandbox-exec, or host (default: host)")
	sidecarCmd.Flags().String("agent-role", "", "Agent role: worker or coordinator (inferred from SSE events when empty)")
	sidecarCmd.Flags().Int("port", 0, "Allocated host port (required in bwrap/sandbox-exec mode)")
	sidecarCmd.Flags().String("plugin-path", "", "Host path to prism-hooks.ts plugin (unused; retained for back-compat)")
	sidecarCmd.Flags().String("initial-prompt", "", "Initial prompt to deliver to the agent after readiness")
	sidecarCmd.Flags().String("config-content", "", "JSON blob for the harness config (unused; retained for back-compat)")
	sidecarCmd.Flags().String("instance-id", "", "UUID instance identifier for this session incarnation (for bus message scoping)")
	sidecarCmd.Flags().Bool("worktree-readonly", false, "Mount the worktree read-only inside the container (used for review agents)")
	sidecarCmd.Flags().String("harness", "pi", "Agent harness to use")
	sidecarCmd.Flags().String("harness-binary", "", "Path to the harness binary (required for stdio-pipe harnesses; ignored for http-port harnesses)")
	sidecarCmd.Flags().String("bwrap-path", "", "Override path to the bwrap binary used to sandbox stdio-pipe harnesses (default: resolved via PATH)")
	sidecarCmd.Flags().StringArray("model-override", nil, "Per-role model override in role=model format (repeatable; C.2)")
	_ = sidecarCmd.MarkFlagRequired("session")
	_ = sidecarCmd.MarkFlagRequired("harness-url")
	rootCmd.AddCommand(sidecarCmd)
}

func runSidecar(cmd *cobra.Command, args []string) error {
	sessionName, _ := cmd.Flags().GetString("session")
	harnessURL, _ := cmd.Flags().GetString("harness-url")
	isolationModeFlag, _ := cmd.Flags().GetString("isolation-mode")
	agentRole, _ := cmd.Flags().GetString("agent-role")
	port, _ := cmd.Flags().GetInt("port")
	pluginPath, _ := cmd.Flags().GetString("plugin-path")
	_ = pluginPath // retained for back-compat; no longer consumed
	initialPrompt, _ := cmd.Flags().GetString("initial-prompt")
	configContent, _ := cmd.Flags().GetString("config-content")
	_ = configContent
	instanceID, _ := cmd.Flags().GetString("instance-id")
	worktreeReadOnly, _ := cmd.Flags().GetBool("worktree-readonly")
	_ = worktreeReadOnly
	harnessName, _ := cmd.Flags().GetString("harness")
	harnessBinaryPath, _ := cmd.Flags().GetString("harness-binary")
	bwrapPath, _ := cmd.Flags().GetString("bwrap-path")
	modelOverrideRaw, _ := cmd.Flags().GetStringArray("model-override")
	if harnessName == "" {
		harnessName = "pi"
	}
	if _, ok := harness.Lookup(harnessName); !ok {
		return fmt.Errorf("sidecar: unknown harness %q: valid harnesses: %s", harnessName, strings.Join(harness.Names(), ", "))
	}

	// Parse --model-override flags into a role→model map (C.2).
	// Each entry is expected to be "role=model"; malformed entries are logged
	// and skipped rather than causing a hard failure at sidecar startup.
	modelsByRole := make(map[string]string, len(modelOverrideRaw))
	for _, entry := range modelOverrideRaw {
		role, model, ok := strings.Cut(entry, "=")
		if !ok || role == "" || model == "" {
			proglog.Warnf("[prism sidecar] warning: ignoring malformed --model-override %q (expected role=model)\n", entry)
			continue
		}
		modelsByRole[role] = model
	}
	if len(modelsByRole) == 0 {
		modelsByRole = nil
	}

	// Resolve the effective isolation mode. Valid values are defined by
	// config.ValidIsolationModes; anything else falls back to host.
	var isolationMode config.IsolationMode
	if isolationModeFlag != "" && config.IsValidIsolationMode(isolationModeFlag) {
		isolationMode = config.IsolationMode(isolationModeFlag)
	} else {
		isolationMode = config.IsolationHost
	}

	// Look up the isolation capabilities for this mode. All per-mode branching
	// below reads from isoCaps rather than comparing against raw mode constants.
	isoCaps := container.CapabilitiesFor(isolationMode)

	// needsHostAPI is true for any mode where the agent runs in a sandbox
	// without direct access to host tmux — bwrap and sandbox-exec
	// (NeedsHostAPISocket) both require the host-API Unix socket.
	needsHostAPI := isoCaps.NeedsHostAPISocket || isoCaps.IsContainer

	// useContainerHarness is true for modes where the agent is pre-created with
	// --prompt at launch (bwrap, sandbox-exec), so the harness uses
	// GET /session to retrieve the existing session ID rather than POST /session.
	useContainerHarness := isoCaps.NeedsHostAPISocket || isoCaps.IsContainer

	// Derive repo and worktree from session name and environment.
	// The session name format is "repo@branch". The worktree is expected
	// to be passed via environment (PRISM_WORKTREE) or derived from CWD.
	repo := sessionName
	if idx := strings.Index(sessionName, "@"); idx >= 0 {
		repo = sessionName[:idx]
	}

	worktree := os.Getenv("PRISM_WORKTREE")
	if worktree == "" {
		worktree, _ = os.Getwd()
	}
	// Resolve worktree to absolute path before passing it to the sidecar config
	// and the initial UpsertStatus call.
	if abs, err := filepath.Abs(worktree); err == nil {
		worktree = abs
	}

	// Open the prism database.
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("sidecar: open database: %w", err)
	}
	defer d.Close()

	// Ensure there is an initial agent_status row so the session is visible on
	// the dashboard immediately. Use UpsertStatus with state "idle" and write
	// repo/worktree — mirroring what tmux-session-start does.
	if err := d.UpsertStatus(sessionName, repo, worktree, "idle", nil, nil); err != nil {
		proglog.Errorf("[prism sidecar] initial upsert: %v\n", err)
	}

	// Load prism runtime config.
	prismCfg := config.Load()
	_ = prismCfg

	// Sanitise the sidecar's own environment so that any subprocess it later
	// spawns (e.g. `prism review` via the /review host-API handler, which
	// inherits os.Environ()) sees valid GitHub token values, regardless of
	// how prism itself was launched.  Fixes the sidecar half of issue #2348:
	// under the boot-restore path the tmux server is started from a systemd
	// user unit, so `$(cat /run/secrets/…)` env-var values propagate
	// verbatim through the process tree — without this call, gh 401's on
	// every subprocess-issued API call until a `prism restart` clears the
	// broken tmux server env.  The account for GITHUB_TOKEN itself is
	// derived from the worktree's bare repo, which is resolved lower down;
	// resolve it once here so we can populate GITHUB_TOKEN too.
	sidecarBareRoot := git.BareRoot(worktree)
	sidecarAccount := container.GitHubAccountFromBareRoot(sidecarBareRoot)
	container.SanitizeGitHubTokenEnv(prismCfg.GitHubTokenPaths, sidecarAccount, agentRole)

	// Resolve the agent model once via the harness adapter. The adapter is
	// constructed transiently here for the EffectiveModel call; a fresh
	// adapter with the resolved model is constructed below for the sidecar.
	// When modelsByRole contains an entry for agentRole, that takes precedence
	// over the profile harness-config lookup (C.2 §6.3).
	var agentModel string
	if m, ok := modelsByRole[agentRole]; ok && m != "" {
		agentModel = m
	} else {
		modelProbe, _ := harness.New(harnessName, "", nil, agentRole, "")
		agentModel = modelProbe.EffectiveModel(agentRole)
	}

	// ctrCfg is always nil — no current isolation mode runs the agent in a
	// sidecar-managed container. The downstream nil checks gate all
	// container-specific code paths.
	var ctrCfg *container.Config

	// Build the host-API socket path for bwrap and sandbox-exec modes.
	// The agent runs in a sandbox without direct access to host tmux in both
	// cases, so the host-API Unix socket is required for proxying prism
	// CLI calls.
	var hostAPISockPath string
	if needsHostAPI {
		if port == 0 && isoCaps.NeedsHostAPISocket {
			return fmt.Errorf("sidecar: --port is required in bwrap/sandbox-exec mode")
		}
		sockPath, err := prismSession.SidecarHostAPIPath(sessionName)
		if err != nil {
			return fmt.Errorf("sidecar: resolve host-API socket path: %w", err)
		}
		hostAPISockPath = sockPath
		// Wire the socket path into the container config so the container
		// gets the socket mounted and PRISM_HOST_API injected (A-2).
		// In bwrap and sandbox-exec modes ctrCfg is nil — the sandbox args
		// are built by the isolator at prism agent-run time, not here.
		if ctrCfg != nil {
			ctrCfg.HostAPISockPath = sockPath
		}
	}

	// Build the harness pipe socket path for socket-pipe harnesses (P2.SIDECAR).
	// The socket co-locates with the host-API socket in the same per-session
	// directory, so the existing bind-mount for that directory covers it too.
	//
	// Transport selection is gated on isolation mode, not GOOS (issue #2078):
	// sandbox-exec cannot reliably reach Unix sockets, so it uses a TCP port on
	// 127.0.0.1. Every other mode (host on Linux/Darwin, bwrap on Linux) uses a
	// Unix socket co-located with the host-API socket. Gating on GOOS broke
	// Darwin host-mode pi sessions, because agentPaneEnvVars always injects a
	// unix:// URL for host mode while the sidecar was binding TCP.
	var harnessPipeSockPath string
	var harnessPipeTCPPort int
	if isSocketPipe, useTCP := selectHarnessPipeTransport(harnessName, isolationMode); isSocketPipe {
		if useTCP {
			// sandbox-exec: bind the TCP port already recorded in
			// agent_status.harness_port (written by the spawn/restore path
			// before the agent pane was created). Never re-allocate when a
			// port is recorded — agent-run does a one-shot read of the same
			// column and bakes PRISM_HARNESS_PIPE into PI's immutable env, so
			// a second allocation here would race that read (issue #2357).
			tcpPort, portErr := resolveHarnessPipeTCPPort(d, sessionName, port)
			if portErr != nil {
				return fmt.Errorf("sidecar: resolve harness pipe TCP port: %w", portErr)
			}
			harnessPipeTCPPort = tcpPort
			if ctrCfg != nil {
				ctrCfg.HarnessPipeTCPPort = tcpPort
			}
		} else {
			pipePath, pipeErr := prismSession.SidecarHarnessPipePath(sessionName)
			if pipeErr != nil {
				return fmt.Errorf("sidecar: resolve harness pipe path: %w", pipeErr)
			}
			harnessPipeSockPath = pipePath
			if ctrCfg != nil {
				ctrCfg.HarnessPipeSockPath = pipePath
			}
		}
	}

	// Build the OnReady callback — only for container-owning modes (AC-18,
	// AC-19). No current isolation mode sets IsContainer, so onReady stays nil.
	// In bwrap and sandbox-exec modes the sidecar does NOT write the readiness
	// signal: "prism agent-run" in the tmux pane starts immediately without
	// waiting. In host mode there is no readiness file at all.
	var onReady func()
	if isoCaps.IsContainer {
		onReady = func() {
			readyPath, pathErr := prismSession.SidecarReadyPath(sessionName)
			if pathErr != nil {
				proglog.Errorf("[prism sidecar] ready path: %v\n", pathErr)
				return
			}
			if err := os.MkdirAll(filepath.Dir(readyPath), 0o755); err != nil {
				proglog.Errorf("[prism sidecar] ready dir: %v\n", err)
				return
			}
			f, err := os.Create(readyPath)
			if err != nil {
				proglog.Errorf("[prism sidecar] write ready signal: %v\n", err)
				return
			}
			_ = f.Close()
			proglog.Infof("[prism sidecar] ready signal written: %s\n", readyPath)
		}
	}

	// Construct the harness adapter and inject it via Config.Harness.
	// The adapter encapsulates all runtime-specific behaviour: session
	// creation, prompt delivery, SSE subscription, event type extraction, and
	// event mapping. The sidecar calls through the harness.Harness interface
	// and has no direct dependency on the adapter package (#710).
	//
	// In bwrap and sandbox-exec mode, use NewContainer so that:
	//   - CreateSession uses GET /session to retrieve the existing session ID
	//     (the agent already created a session when the TUI started)
	//   - DeliverInitialPrompt is a no-op (prompt was sent via --prompt CLI flag)
	//
	// When modelsByRole is non-nil (C.2 --model-override), pass the full map
	// so DeliverInitialPrompt and EffectiveModel use per-role models.
	var h harness.Harness
	if len(modelsByRole) > 0 {
		if useContainerHarness {
			h, err = harness.NewContainerWithModelOverrides(harnessName, harnessURL, nil, agentRole, modelsByRole)
		} else {
			h, err = harness.NewWithModelOverrides(harnessName, harnessURL, nil, agentRole, modelsByRole)
		}
	} else if useContainerHarness {
		h, err = harness.NewContainer(harnessName, harnessURL, nil, agentRole, agentModel)
	} else {
		h, err = harness.New(harnessName, harnessURL, nil, agentRole, agentModel)
	}
	if err != nil {
		return fmt.Errorf("sidecar: construct harness adapter: %w", err)
	}

	// Inject harness-specific runtime env vars into the container config.
	// This replaces previously hard-coded harness-specific env vars
	// in container.go and bwrap.go with values from the harness adapter.
	if ctrCfg != nil {
		ctrCfg.RuntimeEnv = h.RuntimeEnv()
	}

	// Resolve the per-session podman proxy listener path (#2317 / #2320).
	// The path is set unconditionally so the sidecar wiring sees it; the
	// gate on actually starting the proxy is the agent_status.containers_enabled
	// column, read inside runPodmanProxyIfEnabled. Resolution failure is
	// non-fatal — the sidecar still starts, just without the proxy surface.
	podmanProxyListenerPath, podmanProxyErr := prismSession.SidecarPodmanProxyPath(sessionName)
	if podmanProxyErr != nil {
		proglog.Warnf("[prism sidecar] resolve podman proxy socket path: %v (containers_enabled sessions will have no proxy)\n", podmanProxyErr)
		podmanProxyListenerPath = ""
	}

	// bareRoot is the worktree's bare repo root, used by the podman proxy's
	// allowed bind sources so the agent can mount the bare repo into a
	// container alongside the worktree. git.BareRoot returns "" when the
	// path is not inside a bare+worktree layout; that is fine — the
	// allowlist just gains one fewer entry in that case.
	bareRoot := git.BareRoot(worktree)

	// Tier-3 /checkin troubleshooting privilege (#2587). The list is read
	// host-side, once, at sidecar start: the rendered file lives under
	// ~/.config/prism/ and is deliberately not bound into any sandbox, so no
	// agent can read or edit its own privilege.
	//
	// A missing file returns an empty list with no error, which grants the
	// privilege to nobody. A malformed file fails closed the same way: the
	// sidecar warns and starts with an empty list rather than refusing to
	// start or, worse, widening the grant.
	checkinPrivilegedRepos, checkinPrivErr := config.LoadCheckinPrivilegedRepos()
	if checkinPrivErr != nil {
		proglog.Warnf("[prism sidecar] read %s: %v (no repo carries the checkin troubleshooting privilege)\n",
			config.CheckinPrivilegedReposFileName, checkinPrivErr)
		checkinPrivilegedRepos = nil
	}

	cfg := sidecar.Config{
		SessionName:             sessionName,
		Repo:                    repo,
		Worktree:                worktree,
		BareRoot:                bareRoot,
		HarnessURL:              harnessURL,
		DB:                      d,
		Clock:                   sidecar.RealClock(),
		AgentRole:               agentRole,
		AgentModel:              agentModel,
		ModelsByRole:            modelsByRole,
		HarnessName:             harnessName,
		HarnessBinaryPath:       harnessBinaryPath,
		BwrapPath:               bwrapPath,
		InstanceID:              instanceID,
		IsolationMode:           isolationMode,
		Container:               ctrCfg,
		HostAPISockPath:         hostAPISockPath,
		HarnessPipeSockPath:     harnessPipeSockPath,
		HarnessPipeTCPPort:      harnessPipeTCPPort,
		PodmanProxyListenerPath: podmanProxyListenerPath,
		CheckinPrivilegedRepos:  checkinPrivilegedRepos,
		OnReady:                 onReady,
		InitialPrompt:           initialPrompt,
		Harness:                 h,
	}
	sc := sidecar.New(cfg)

	// Set up signal handling for clean shutdown.
	//
	// Two-signal contract:
	//   - First SIGINT/SIGTERM: invoke sc.Shutdown() synchronously, then
	//     cancel() the run context.  This preserves the DB-ordering invariant:
	//     Shutdown() completes its writes before cancel() fires and d.Close()
	//     is reached.
	//   - Second SIGINT/SIGTERM while Shutdown is still running: reset the
	//     signal to the Go runtime's default handler and re-raise it, causing
	//     an immediate process exit.  The process terminates before d.Close()
	//     runs, so the ordering invariant is moot on this path.  This gives the
	//     operator a reliable force-exit path (two Ctrl-C presses) without
	//     requiring a kill -9.
	//
	// signal.Stop(sigCh) is deferred to release the subscription on the happy
	// path (no signal received) so OS resources are not held for the process
	// lifetime after Run() returns.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	runSignalHandler(sigCh, sc.Shutdown, cancel)

	// Run clipboard staging dir cleanup in the background at sidecar startup.
	// This implements the TTL sweep that prevents ~/.cache/prism/clipboard/ from
	// growing unboundedly across multiple paste operations. The sweep is
	// fire-and-forget; errors are non-fatal (logged by runClipboardClean itself).
	// Runs for bwrap and sandbox-exec modes (both run the agent in a sandbox
	// where the clipboard staging dir is mounted).
	if needsHostAPI {
		go func() {
			_ = runClipboardClean(nil, nil)
		}()
	}

	proglog.Infof("[prism sidecar] starting: session=%s url=%s isolation=%s\n",
		sessionName, harnessURL, isolationMode)

	if err := sc.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("sidecar: %w", err)
	}

	proglog.Infof("[prism sidecar] shutting down\n")
	return nil
}

// sidecarForceExit is the function called on the second signal to immediately
// terminate the process.  It is a package-level variable so tests can replace
// it with a stub that does not actually kill the test process.
var sidecarForceExit = func(sig syscall.Signal) {
	signal.Reset(syscall.SIGINT, syscall.SIGTERM)
	_ = syscall.Kill(os.Getpid(), sig)
}

// runSignalHandler starts the two-signal shutdown goroutine and returns
// immediately.  The caller must have already called signal.Notify(sigCh, ...) and
// deferred signal.Stop(sigCh).
//
// Two-signal contract:
//
//  1. First SIGINT/SIGTERM: invoke shutdownFn() synchronously, then call
//     cancelFn() to unblock the Run loop.  This preserves the critical ordering
//     invariant: Shutdown() must complete its DB writes before cancel() fires
//     and allows d.Close() to proceed.  Shutdown may take several hundred
//     milliseconds to seconds (DB writes, container teardown, listener draining).
//
//  2. Second SIGINT/SIGTERM arriving while Shutdown is still running: reset the
//     SIGINT/SIGTERM handlers to the Go runtime defaults and re-raise the
//     signal.  This causes an immediate process exit, bypassing the d.Close()
//     defer entirely, and gives the operator a reliable force-exit path (two
//     Ctrl-C presses) without requiring kill -9.  The DB ordering invariant is
//     not a concern here — on force-exit the process terminates immediately and
//     no deferred cleanup runs.
func runSignalHandler(sigCh <-chan os.Signal, shutdownFn func(), cancelFn func()) {
	go func() {
		// First signal: arm the second-signal watcher, then run Shutdown
		// synchronously so that all DB writes complete before cancel() fires.
		_, ok := <-sigCh
		if !ok {
			return // channel closed on normal exit
		}

		// Concurrently watch for a second signal while Shutdown runs.
		// If it arrives, force-exit immediately — no cleanup needed because
		// the process is being killed and defer d.Close() will not run.
		go func() {
			sig2, ok2 := <-sigCh
			if !ok2 {
				return // channel closed — normal exit already in progress
			}
			sidecarForceExit(sig2.(syscall.Signal))
		}()

		// Shutdown() runs synchronously before cancel() so that its DB writes
		// (AbandonWatchingMerges, UpdateSessionEnded, upsertState,
		// writeStateChange) complete while the database connection is still open.
		// defer d.Close() fires only after runSidecar returns, which happens
		// after sc.Run() exits, which happens after cancel() fires here.
		shutdownFn()
		cancelFn()
	}()
}

// selectHarnessPipeTransport decides whether a named harness needs a harness
// pipe and, if so, whether the sidecar should bind a TCP port or a Unix
// socket for it.
//
// Returns isSocketPipe=false for harnesses whose shape is not
// TransportSocketPipe (e.g. SSE-only harnesses) — those need no harness pipe.
// Returns useTCP=true only under sandbox-exec isolation, where the sandboxed
// agent-run process cannot reliably reach a Unix socket on the host. All
// other isolation modes (host on Linux/Darwin, bwrap on Linux) use a Unix
// socket co-located with the host-API socket.
//
// This function is the single source of truth for the transport decision —
// agentPaneEnvVars (host mode) and agent-run (sandbox-exec, bwrap) must inject
// PRISM_HARNESS_PIPE values consistent with whatever this function picks. See
// issue #2078 for the regression that motivated extracting this gate.
func selectHarnessPipeTransport(harnessName string, isolationMode config.IsolationMode) (isSocketPipe bool, useTCP bool) {
	shape, ok := harness.ShapeOf(harnessName)
	if !ok || shape != harness.TransportSocketPipe {
		return false, false
	}
	return true, isolationMode == config.IsolationSandboxExec
}

// resolveHarnessPipeTCPPort returns the TCP port the sidecar must bind for
// the harness pipe listener under sandbox-exec isolation.
//
// The authoritative value is the one already recorded in
// agent_status.harness_port — written synchronously by the spawn/restore
// path (db.AllocatePort) BEFORE the agent pane was created. `prism
// agent-run` does a one-shot read of the same column and bakes
// PRISM_HARNESS_PIPE=tcp://127.0.0.1:<port> into PI's immutable process
// env. Re-allocating here (the pre-#2357 behaviour) raced that read: when
// agent-run read first, PI was left pointed at a port nobody binds, forever
// — the #1554 first-connect retry recovers from a timing race, not a wrong
// port value — and the session ran headless.
//
// portFlag (the --port flag value, i.e. what the spawner allocated) is used
// only for a divergence warning: agent-run reads the DB, not the flag, so
// the DB value wins when they disagree.
//
// When no port is recorded at all (cleared row, direct sidecar invocation),
// fall back to db.AllocatePort. Post-#2357 AllocatePort excludes the
// session's own row from the used-port set and prefers the previously
// recorded port, so even the fallback is idempotent while the port stays
// free.
func resolveHarnessPipeTCPPort(d *db.DB, sessionName string, portFlag int) (int, error) {
	st, err := d.CurrentStatus(sessionName)
	if err != nil {
		return 0, fmt.Errorf("read agent_status: %w", err)
	}
	if st != nil && st.HarnessPort != nil && *st.HarnessPort != 0 {
		if portFlag != 0 && portFlag != *st.HarnessPort {
			proglog.Warnf("[prism sidecar] --port %d diverges from agent_status.harness_port %d for %q — using the DB value (agent-run reads the DB)\n",
				portFlag, *st.HarnessPort, sessionName)
		}
		return *st.HarnessPort, nil
	}
	return d.AllocatePort(sessionName)
}
