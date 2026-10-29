package cmd

// prism switch — context switcher.
//
// Project layout is read at runtime from ~/.config/prism/config.json
// (or $PRISM_CONFIG_FILE) via the internal/config package.

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/proglog"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sessionname"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// killSidecarTimeout is the wait for an old sidecar to exit after SIGTERM,
// and then after SIGKILL, in the stale-zombie restart path. 2 seconds is
// enough for a well-behaved sidecar to handle SIGTERM while still providing
// a fast failure signal on a loaded system.
const killSidecarTimeout = 2 * time.Second

// ── session management ────────────────────────────────────────────────────────

// applyPathIsolationOverride checks cfg.ProjectIsolationOverrides for path and,
// if a valid override is found, updates opts.IsolationMode in place. If no
// override applies, opts is left unchanged.
//
// When the override changes the sandbox/host boundary (e.g. sandbox-exec →
// host), AgentEnvVars are injected from pf when the effective mode is host.
// This handles the case where the machine default is a sandboxed mode
// (AgentEnvVars skipped at opts construction time) but the path override
// switches to host mode, which requires env var injection.
//
// pf may be nil; the AgentEnvVars injection is a no-op when pf is nil.
//
// The caller is responsible for passing a non-nil opts pointer. Logging is
// done to stderr at info level when an override fires.
func applyPathIsolationOverride(path string, cfg config.Config, opts *session.Opts, isoCaps container.Capabilities, pf *config.ProfilesFile) {
	override := cfg.IsolationOverrideForPath(path)
	if override == "" {
		return
	}
	proglog.Infof("[prism switch] using isolation override %q for path %q\n", override, path)
	opts.IsolationMode = string(override)
	effCaps := container.CapabilitiesFor(override)
	// When the override crosses the sandbox/host boundary (sandboxed → host),
	// inject AgentEnvVars that would normally be set at opts construction time.
	// The pre-override isoCaps was sandboxed, so AgentEnvVars was not
	// populated; now that the effective mode is host, we must inject them here.
	if !effCaps.RequiresProfilesFile && isoCaps.RequiresProfilesFile && pf != nil {
		opts.AgentEnvVars = pf.AgentEnvVars
	}
}

// ensureAndSwitch creates the session if it doesn't exist (with the appropriate
// layout) and then switches the current client to it, unless opts.Headless is set.
// For full-layout sessions, a port is allocated from the 14000–14999 range and
// passed through to the agent via BuildAgentCmd.
func ensureAndSwitch(path string, projectRoot string, opts session.Opts) error {
	var sessionName string
	var directory string

	if path == "[scratchpad]" {
		sessionName = "scratchpad"
		home, _ := os.UserHomeDir()
		directory = home
		opts.Layout = session.LayoutScratchpad
		// Set SessionName for consistency with the full-layout branch.
		// LayoutScratchpad does not call BuildOpencodeCmd today, so this has
		// no runtime effect, but keeps the struct complete in case the
		// scratchpad layout ever gains an agent window.
		opts.SessionName = sessionName
	} else {
		directory = expandHome(path)
		name, err := session.NameFor(directory, projectRoot)
		if err != nil {
			return err
		}
		sessionName = name
		opts.SessionName = sessionName
		opts.Layout = session.LayoutFull
	}

	opts.Agent = session.DefaultAgent(directory, opts.Agent)

	// Open the DB for the startup guard, instance ID generation, and port
	// allocation. The DB handle is passed into opts.DB so that session.Create
	// can check whether an existing tmux session with the same name is a live
	// instance (last_seen within 60s).
	d, dbErr := openDB()
	if dbErr != nil {
		// Non-fatal: log and continue without a DB. The startup guard will fall
		// back to the simple HasSession check.
		fmt.Fprintf(os.Stderr, "warning: could not open DB for startup guard: %v\n", dbErr)
	} else {
		defer d.Close()
		opts.DB = d
	}

	// staleQueue is the merge queue of a stale session whose old sidecar
	// this switch stops. The sidecar that session.Create starts watches it.
	var staleQueue []db.PendingMerge

	// Liveness pre-check for the switch/launch path (ForceFresh=false).
	//
	// When a session already exists in tmux, we check its DB liveness here —
	// before any DB writes — so we can take the right action without risking
	// TOCTOU contamination from the UpsertStatus/SetInstanceID writes below
	// (which reset last_seen to NOW and would make a stale zombie appear live
	// to startupGuardKillOld inside session.Create).
	//
	// Three outcomes:
	//  1. Session is live (last_seen < 60s) → attach immediately, skip all
	//     DB mutations so the DB row is left intact. Returns here.
	//  2. Session is stale/zombie (last_seen ≥ 60s, or no DB row, with DB
	//     available) → set ForceFresh=true so session.Create kills it
	//     unconditionally without re-querying the DB. Falls through.
	//  3. Session does not exist, or no DB available → no change to ForceFresh.
	//     Falls through to normal create path (or legacy no-op if d==nil).
	if !opts.ForceFresh && session.HasExactSession(sessionName) {
		if d == nil {
			// No DB — can't determine liveness. Treat as live:
			// attach without touching anything.
			if opts.Headless {
				return nil
			}
			return session.Attach(sessionName)
		}
		st, _ := d.CurrentStatus(sessionName)
		isLive := st != nil && time.Since(st.LastSeen) < 60*time.Second
		if !isLive && session.SidecarAlive(sessionName) {
			// The DB row is quiet but the sidecar is responsive on its socket.
			// Paused-by-design states (escalated awaiting coordinator guidance,
			// reviewing awaiting verdicts) freeze last_seen for arbitrarily
			// long while the session is perfectly healthy — killing the
			// sidecar here would sever prompt delivery, finish notifications,
			// and event recording for the session. Treat a
			// responsive sidecar as live and attach instead.
			lastSeenAge := "no DB row"
			if st != nil {
				lastSeenAge = "last_seen " + time.Since(st.LastSeen).Round(time.Second).String() + " ago"
			}
			proglog.Infof("[prism switch] session %q has a stale DB row (%s) but a responsive sidecar — treating as live\n",
				sessionName, lastSeenAge)
			isLive = true
		}
		if isLive {
			// Live session — attach without touching DB state.
			if opts.Headless {
				return nil
			}
			return session.Attach(sessionName)
		}
		// Stale or zombie (no DB row, or last_seen ≥ 60s). Upgrade to
		// ForceFresh=true so session.Create kills unconditionally without a
		// second DB query that would see the freshly-written UpsertStatus row.
		//
		// Before falling through to session.Create (which kills the tmux
		// session via tmux.KillSession), we must also stop the old sidecar
		// process and wait for it to exit. Without this, the old sidecar
		// keeps owning its host-API Unix socket and the new sidecar's
		// duplicate-start guard (checkNoLiveSidecar) fires, causing the new
		// sidecar to refuse to start and the PI extension to exhaust its
		// connect-retry budget.
		//
		// If the stop cannot prove that the old sidecar is gone, switch
		// returns an error and does not start a new sidecar (#3273). On this
		// path no socket of the session answers, so the duplicate-start guard
		// cannot refuse, and a second merge-queue watcher would poll the
		// rows that the old sidecar can still watch.
		//
		// Note: this is NOT a session teardown. SetEnded and PurgeBusMessages
		// are intentionally omitted — the agent_status row (repo, worktree,
		// harness_port) is reused across the restart. The instance_id is not
		// always reused: a live old sidecar ends its sessions row when it
		// stops, and the tmux-session-start seed then mints a new instance
		// ID.
		proglog.Infof("[prism switch] stale-zombie session %q: killing old sidecar before restart\n", sessionName)
		var stopErr error
		if staleQueue, stopErr = stopStaleSidecar(d, sessionName, st); stopErr != nil {
			return stopErr
		}
		opts.ForceFresh = true
	}

	// For socket-pipe harnesses (e.g. "pi") in host isolation mode,
	// pre-compute the Unix socket path so agentPaneEnvVars can inject
	// PRISM_HARNESS_PIPE into the tmux pane. bwrap and sandbox-exec set
	// PRISM_HARNESS_PIPE via their own paths; only inject here for host mode.
	// This mirrors the same block in spawn.go.
	if hShape, hShapeOK := harness.ShapeOf(opts.HarnessName); hShapeOK && hShape == harness.TransportSocketPipe && opts.IsolationMode == "host" {
		if pipePath, pipeErr := session.SidecarHarnessPipePath(sessionName); pipeErr == nil {
			opts.HarnessPipeSockPath = pipePath
		} else {
			proglog.Warnf("[prism switch] warning: could not resolve harness pipe path for %q: %v\n", sessionName, pipeErr)
		}
	}

	// Allocate a port for full-layout sessions. The agent_status row must
	// exist before we can write harness_port to it, so we allocate after
	// session.Create (which seeds agent_status via `prism event
	// tmux-session-start`). However, BuildOpencodeCmd needs the port at
	// session creation time (it's called inside setupFullLayout). To break
	// this ordering dependency, we pre-allocate: seed the DB row first, then
	// allocate the port, then create the tmux session.
	if opts.Layout == session.LayoutFull {
		port, err := allocatePortForSession(sessionName, directory, opts.HarnessName)
		if err != nil {
			// Non-fatal: log and continue without a port. the agent will still
			// work, just without the serve API.
			fmt.Fprintf(os.Stderr, "warning: port allocation failed for %q: %v\n", sessionName, err)
		} else {
			opts.Port = port
		}
	}

	if err := session.Create(sessionName, directory, opts); err != nil {
		if len(staleQueue) > 0 {
			fmt.Fprintf(os.Stderr, "warning: prism switch %q: the session did not start, so the merge queue is not resumed: %s\n",
				sessionName, formatMergePRs(staleQueue))
		}
		return err
	}
	resumeStaleMergeQueue(d, sessionName, staleQueue)

	if opts.Headless {
		fmt.Printf("session %q created\n", sessionName)
		return nil
	}

	return session.Attach(sessionName)
}

// switchSidecarStopTimeout is the SIGTERM wait, and then the SIGKILL wait,
// that the stale-sidecar path gives the old sidecar. Tests shorten it.
var switchSidecarStopTimeout = killSidecarTimeout

// switchStopSidecar stops the old sidecar of a stale session, and reports
// whether no process that can be that sidecar runs. Tests replace it.
var switchStopSidecar = session.EnsureSidecarStopped

// stopStaleSidecar stops the old sidecar of the stale session sessionName,
// and returns the merge queue that the new sidecar must watch (#3175). st
// is the agent_status row of the session, or nil.
//
// The stop sends SIGKILL to a sidecar that does not exit inside the
// SIGTERM wait. Thus no write of a slow Shutdown comes after the resume
// (#3273). The Shutdown of the old sidecar abandons the queue, so read the
// queue first.
//
// It returns an error when the stop cannot prove that the old sidecar is
// gone. The caller must then not start a new sidecar. The PID file stays,
// so the next switch stops the same process.
func stopStaleSidecar(d *db.DB, sessionName string, st *db.Status) ([]db.PendingMerge, error) {
	queued, err := d.WatchingMerges(sessionName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: prism switch %q: read merge queue: %v\n", sessionName, err)
	}
	if !switchStopSidecar(sessionName, switchSidecarStopTimeout) {
		return nil, staleSidecarRunsError(sessionName, queued)
	}
	if st != nil && st.EndedAt == nil && st.InstanceID != nil {
		if queued, err = withMergesAbandonedByStop(d, sessionName, *st.InstanceID, queued); err != nil {
			fmt.Fprintf(os.Stderr, "warning: prism switch %q: read merge queue of the last stop: %v\n", sessionName, err)
		}
	}
	return queued, nil
}

// staleSidecarRunsError is the error of prism switch when the stop of the
// old sidecar of sessionName cannot prove that it is gone. The stop already
// wrote a warning that names the PID. queued is the merge queue that the
// switch read before the stop.
func staleSidecarRunsError(sessionName string, queued []db.PendingMerge) error {
	var b strings.Builder
	fmt.Fprintf(&b, "prism switch %q: the session is not restarted, because a process that can be the old sidecar still runs (see the warning above). "+
		"A new sidecar next to that process can start a second merge-queue watcher.", sessionName)
	if len(queued) > 0 {
		fmt.Fprintf(&b, " The merge queue is not resumed: %s.", formatMergePRs(queued))
	}
	b.WriteString(" Stop that process, then run prism switch again.")
	if pidPath, err := session.SidecarPIDPath(sessionName); err == nil {
		fmt.Fprintf(&b, " If the process is not a prism sidecar, remove its PID file first: %s", pidPath)
	}
	return errors.New(b.String())
}

// resumeStaleMergeQueue gives queued to the sidecar that session.Create
// started for sessionName, as resumeMergeQueue does. The
// tmux-session-start seed picks the instance ID of that sidecar, so read
// it after session.Create. The caller calls it only after the stop proved
// that the old sidecar is gone.
func resumeStaleMergeQueue(d *db.DB, sessionName string, queued []db.PendingMerge) {
	if len(queued) == 0 {
		return
	}
	instanceID := ""
	if st, err := d.CurrentStatus(sessionName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: prism switch %q: read instance ID: %v\n", sessionName, err)
	} else if st != nil && st.InstanceID != nil {
		instanceID = *st.InstanceID
	}
	resumeMergeQueue(d, "warning: prism switch", sessionName, instanceID, queued)
}

// allocatePortForSession ensures the agent_status row exists for sessionName
// and then allocates a port from the DB. If the session already has a port
// allocated (e.g. on restore), it returns the existing port.
//
// harnessName is the resolved harness for the new session (e.g. "pi"
// or "pi"). It is written to the DB row so that prism restore can replay the
// correct harness. When empty, defaults to "pi" (handled by
// UpsertStatusSeedRootAgentName's own default logic).
func allocatePortForSession(sessionName, directory, harnessName string) (int, error) {
	d, err := openDB()
	if err != nil {
		return 0, fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	// Check if a status row already exists and already has a port.
	existing, err := d.CurrentStatus(sessionName)
	if err != nil {
		return 0, fmt.Errorf("current status: %w", err)
	}
	if existing != nil && existing.HarnessPort != nil && existing.EndedAt == nil {
		return *existing.HarnessPort, nil
	}

	// Ensure the agent_status row exists (idempotent upsert). Use
	// UpsertStatusSeedRootAgentName so the harness name is written to the
	// DB row from the first moment — prism restore reads it from here.
	repo := repoFromWorktreePath(directory)
	if repo == "" {
		// Not inside a project worktree — derive from the session name via
		// sessionname.Repo. A local copy that split on "@" alone would attribute
		// a descendant of a non-worktree parent (`obsidian~investigate-v2`) to a
		// repo of its own rather than to `obsidian`.
		repo = sessionname.Repo(sessionName)
	}
	if err := d.UpsertStatusSeedRootAgentName(sessionName, repo, directory, "idle", nil, nil, "", harnessName, ""); err != nil {
		return 0, fmt.Errorf("upsert status: %w", err)
	}

	return d.AllocatePort(sessionName)
}

// switchSetup is the start input that each session start of prism switch
// shares: the config, the profiles, the isolation capabilities of the
// machine default, and the base session options.
type switchSetup struct {
	cfg     config.Config
	pf      *config.ProfilesFile
	isoCaps container.Capabilities
	opts    session.Opts
}

// loadSwitchSetup reads the switchSetup of prism switch. prism convert uses
// it to start the session of the new worktree.
func loadSwitchSetup(fresh bool) (switchSetup, error) {
	cfg := config.Load()

	// Derive the effective isolation mode from config via registry.Resolve.
	// switch has no --isolation flag — the machine default is used.
	isoMode, isoErr := container.Resolve(container.ResolveInput{
		ConfigDefault: cfg.DefaultIsolationMode,
	})
	if isoErr != nil {
		return switchSetup{}, isoErr
	}

	// Look up the isolation capabilities for this mode. All per-mode branching
	// below reads from isoCaps rather than comparing against raw mode constants.
	isoCaps := container.CapabilitiesFor(isoMode)

	// Load profiles.json for the per-role slot (model / provider / thinking,
	// resolved at agent-run time) and for agent env var injection (host
	// mode). Always attempt to load; treat a missing file as fatal when
	// sandboxed (bwrap or sandbox-exec), since those paths require a slot.
	var pf *config.ProfilesFile
	{
		var pfErr error
		pf, pfErr = config.LoadProfiles()
		if pfErr != nil {
			if isoCaps.RequiresProfilesFile {
				return switchSetup{}, pfErr
			}
			proglog.Warnf("[prism switch] warning: could not load profiles.json (agent env vars will not be injected): %v\n", pfErr)
			pf = nil
		}
	}

	// Pi is the sole harness. Use harness.Lookup("pi") as the single
	// source of truth rather than a hardcoded string.
	switchHarnessName := "pi"

	// Validate the harness name. Pi is hardwired but guard against future
	// misconfigurations via Lookup.
	if _, ok := harness.Lookup(switchHarnessName); !ok {
		return switchSetup{}, fmt.Errorf("prism switch: unknown harness %q: valid harnesses: %s",
			switchHarnessName, strings.Join(harness.Names(), ", "))
	}

	// Validate the active profile before any session state is created.
	//
	// Both checks read only the profiles file and the active-profile state
	// file, so neither depends on the path: they run once here.
	//
	// Without this gate a corrupt active-profile state file — or one
	// naming a profile that profiles.json no longer defines, e.g. a stale
	// "ox-alpha" — surfaces inside the agent pane at `prism agent-run`
	// (populatePIConfig → RequireSlot), after the DB row, the harness port,
	// the tmux session, and the sidecar all exist.
	//
	// This gate keys off the command-level mode, not the effective
	// capabilities after the per-path isolation override. The override
	// changes only whether the check runs, never its result, so any
	// difference is confined to a path that overrides across the
	// host/sandbox boundary. Slot presence is deliberately NOT checked
	// here: the role is not known until the path is resolved.
	if isoCaps.RequiresProfilesFile && pf != nil {
		switchProfile, _, switchProfErr := config.ResolveActiveProfile(pf, "")
		if switchProfErr != nil {
			return switchSetup{}, switchProfErr
		}
		if switchProfile != "" {
			if err := config.RequireProfile(pf, switchProfile); err != nil {
				return switchSetup{}, err
			}
		}
	}

	// Populate harness-specific env var names from the adapter so that
	// no harness-specific string literals appear in session.go.
	// harnessFlag was validated above so the error is unreachable.
	switchHarness, _ := harness.New(switchHarnessName, "", nil, "", "")
	opts := session.Opts{
		Fresh:          fresh,
		IsolationMode:  string(isoMode),
		PluginHostPath: cfg.SidecarPluginPath,
		RuntimeEnvVars: switchHarness.RuntimeEnv(),
		HarnessName:    switchHarnessName,
		// PIExtensionDir for host-mode pi launches. The container
		// path appends --extension via container.PIInvocation, so this is
		// load-bearing only for isolation=host — buildDirectAgentCmd skips
		// the flag when PIExtensionDir is empty.
		PIExtensionDir: cfg.PIExtensionDir,
	}
	// AgentEnvVars only applies to host-mode sessions; sandboxed sessions
	// receive env vars via their own injection paths (bwrap --setenv,
	// sandbox-exec profile).
	if pf != nil && !isoCaps.RequiresProfilesFile {
		opts.AgentEnvVars = pf.AgentEnvVars
	}
	return switchSetup{cfg: cfg, pf: pf, isoCaps: isoCaps, opts: opts}, nil
}

// ── cobra command ─────────────────────────────────────────────────────────────

var switchCmd = &cobra.Command{
	Use:   "switch",
	Short: "Context switcher — open or create a project session",
	RunE: func(cmd *cobra.Command, args []string) error {
		pathArg, _ := cmd.Flags().GetString("path")

		// Inside a container: proxy the switch to the host sidecar.
		// --path is the only flag that makes sense in this context.
		if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
			return proxyToHostAPI(apiURL, "/switch", map[string]any{
				"session": pathArg, // host resolves path → session name
			}, nil)
		}

		fresh, _ := cmd.Flags().GetBool("fresh")
		setup, err := loadSwitchSetup(fresh)
		if err != nil {
			return err
		}
		cfg, pf, isoCaps, opts := setup.cfg, setup.pf, setup.isoCaps, setup.opts

		// --path: open a specific path directly.
		if pathArg != "" {
			p := expandHome(pathArg)
			info, err := os.Stat(p)
			if err != nil || !info.IsDir() {
				return fmt.Errorf("not a directory: %s", pathArg)
			}
			if git.IsBareRepo(p) {
				worktrees := git.Worktrees(p)
				if len(worktrees) == 0 {
					return fmt.Errorf("no worktrees found in %s", p)
				}
				o := opts
				// Apply per-path isolation override for the first worktree.
				applyPathIsolationOverride(worktrees[0], cfg, &o, isoCaps, pf)
				return ensureAndSwitch(worktrees[0], p, o)
			}
			if bareRoot := git.BareRoot(p); bareRoot != "" {
				o := opts
				// Apply per-path isolation override for the worktree path.
				applyPathIsolationOverride(p, cfg, &o, isoCaps, pf)
				return ensureAndSwitch(p, bareRoot, o)
			}
			o := opts
			// Apply per-path isolation override for plain directory paths.
			applyPathIsolationOverride(p, cfg, &o, isoCaps, pf)
			return ensureAndSwitch(p, "", o)
		}

		// Ensure dashboard exists in background.
		ensureSwitchDashSession()

		// Top-level picker.
		entries := projectEntries()
		chosen := pick("project> ", entries)
		if chosen == nil {
			return nil
		}

		switch chosen.special {
		case "[dashboard]":
			ensureSwitchDashSession()
			client, _ := tmux.CurrentClient()
			if client == "" {
				client = tmux.CallerClient()
			}
			if client != "" {
				return tmux.SwitchClient(client, dashSession)
			}
			_, err := tmux.SwitchClientCurrent(dashSession)
			return err

		case "[scratchpad]":
			return ensureAndSwitch("[scratchpad]", "", opts)

		case "[+ clone repo]":
			return handleCloneRepo(pf, opts, isoCaps, cfg)

		default:
			p := chosen.path
			switch {
			case git.IsBareRepo(p):
				return handleBareRepo(p, pf, opts, isoCaps, cfg)
			case git.IsRegularRepo(p):
				return handleRegularRepo(p, pf, opts, isoCaps, cfg)
			default:
				o := opts
				// Apply per-path isolation override for plain directory paths
				// selected from the picker (e.g. ~/documents/obsidian).
				applyPathIsolationOverride(p, cfg, &o, isoCaps, pf)
				return ensureAndSwitch(p, "", o)
			}
		}
	},
}

func init() {
	switchCmd.Flags().String("path", "", "Open a specific path directly (skip picker)")
	switchCmd.Flags().Bool("fresh", false, "Start a fresh harness session, ignoring any stored session ID")
	rootCmd.AddCommand(switchCmd)
}
