package cmd

// prism switch — context switcher (replaces cli.tmux.contextSwitcher)
//
// Project layout is read at runtime from ~/.config/prism/config.json
// (or $PRISM_CONFIG_FILE) via the internal/config package.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// ── session management ────────────────────────────────────────────────────────

// writeHarnessConfigBlobFor dispatches the per-mode "write opencode.json blob
// to the deterministic per-session temp path" step through the registered
// Isolator (D3, issue #1133). cmdName is used in the error wrapper so the
// caller's command name surfaces in user-facing messages.
//
// content == "" is treated as a no-op so callers can call this unconditionally
// after the NeedsConfigBlob gate; the gate / empty-content combination is
// what each pre-refactor branch checked before calling WriteOpencodeConfig.
func writeHarnessConfigBlobFor(mode config.IsolationMode, sessionName, content, cmdName string) error {
	if content == "" {
		return nil
	}
	iso, err := container.For(mode, container.ConstructorOpts{Name: sessionName})
	if err != nil {
		return fmt.Errorf("%s: %w", cmdName, err)
	}
	if err := iso.WriteHarnessConfigBlob(sessionName, content); err != nil {
		return fmt.Errorf("%s: %w", cmdName, err)
	}
	return nil
}

// injectContainerConfig loads the role-specific opencode.json blob from
// profiles.json and sets opts.ConfigContent when sandboxed mode is active.
// This mirrors the pattern in spawn.go and must be called after the final
// worktree path is known (path is the directory passed to ensureAndSwitch).
//
// pf must be non-nil when called; callers are responsible for loading it when
// the effective isolation mode is podman or bwrap.
func injectContainerConfig(path string, pf *config.ProfilesFile, opts *session.Opts, cmdName string) error {
	effectiveRole := session.DefaultAgent(path, opts.Agent)
	// Non-worktree paths (effectiveRole == "") use the coordinator config blob
	// so that build/plan agents are available, but pass no --agent flag.
	lookupRole := effectiveRole
	if lookupRole == "" {
		lookupRole = "coordinator"
	}
	roleConfig, err := config.ContainerConfigForRole(pf, lookupRole)
	if err != nil {
		return err
	}
	if roleConfig != "" {
		opts.ConfigContent = roleConfig
	} else if effectiveRole == "worker" || effectiveRole == "coordinator" {
		fmt.Fprintf(os.Stderr, "[%s] warning: no container role config for %q in profiles.json — rebuild the system config to generate it\n", cmdName, effectiveRole)
	}
	return nil
}

// ensureAndSwitch creates the session if it doesn't exist (with the appropriate
// layout) and then switches the current client to it, unless opts.Headless is set.
// For full-layout sessions, a port is allocated from the 14000–14999 range and
// passed through to opencode via BuildOpencodeCmd.
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
		// scratchpad layout ever gains an opencode agent window.
		opts.SessionName = sessionName
	} else {
		directory = expandHome(path)
		sessionName = session.NameFor(directory, projectRoot)
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
		// back to the simple HasSession check (legacy no-op behaviour).
		fmt.Fprintf(os.Stderr, "warning: could not open DB for startup guard: %v\n", dbErr)
	} else {
		defer d.Close()
		opts.DB = d
	}

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
	if !opts.ForceFresh && tmux.HasSession(sessionName) {
		if d == nil {
			// No DB — can't determine liveness. Treat as live (legacy no-op):
			// attach without touching anything.
			if opts.Headless {
				return nil
			}
			return session.Attach(sessionName)
		}
		st, _ := d.CurrentStatus(sessionName)
		isLive := st != nil && time.Since(st.LastSeen) < 60*time.Second
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
		opts.ForceFresh = true
	}

	// Pre-generate a UUID instance_id and write it to agent_status before
	// starting the sidecar. The sidecar is launched inside session.Create
	// (before tmux-session-start fires), so the instance_id must be in the DB
	// before the sidecar reads it. We also pass it via opts.InstanceID so
	// StartSidecarWithOpts can forward it as --instance-id to the sidecar
	// process without needing a DB read.
	//
	// This block is reached only for new sessions or stale zombies (the live
	// early-exit above handles the attach-to-live case).
	if d != nil && opts.Layout == session.LayoutFull {
		instanceID := uuid.New().String()
		// Ensure the agent_status row exists before writing instance_id.
		// allocatePortForSession already upserts it, but call UpsertStatus
		// here defensively in case port allocation is skipped.
		repo := deriveRepo(directory)
		if repo == "" {
			if idx := strings.Index(sessionName, "@"); idx > 0 {
				repo = sessionName[:idx]
			}
		}
		_ = d.UpsertStatus(sessionName, repo, directory, "idle", nil, nil)
		if err := d.SetInstanceID(sessionName, instanceID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not set instance_id for %q: %v\n", sessionName, err)
		} else {
			opts.InstanceID = instanceID
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
		port, err := allocatePortForSession(sessionName, directory)
		if err != nil {
			// Non-fatal: log and continue without a port. opencode will still
			// work, just without the serve API.
			fmt.Fprintf(os.Stderr, "warning: port allocation failed for %q: %v\n", sessionName, err)
		} else {
			opts.Port = port
		}
	}

	if err := session.Create(sessionName, directory, opts); err != nil {
		return err
	}

	if opts.Headless {
		fmt.Printf("session %q created\n", sessionName)
		return nil
	}

	return session.Attach(sessionName)
}

// allocatePortForSession ensures the agent_status row exists for sessionName
// and then allocates a port from the DB. If the session already has a port
// allocated (e.g. on restore), it returns the existing port.
func allocatePortForSession(sessionName, directory string) (int, error) {
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
	if existing != nil && existing.HarnessPort != nil {
		return *existing.HarnessPort, nil
	}

	// Ensure the agent_status row exists (idempotent upsert).
	repo := deriveRepo(directory)
	if repo == "" {
		// Not inside a project worktree — derive from session name.
		if idx := strings.Index(sessionName, "@"); idx > 0 {
			repo = sessionName[:idx]
		}
	}
	if err := d.UpsertStatus(sessionName, repo, directory, "idle", nil, nil); err != nil {
		return 0, fmt.Errorf("upsert status: %w", err)
	}

	return d.AllocatePort(sessionName)
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
		cfg := config.Load()

		// Derive the effective isolation mode from config via registry.Resolve.
		// switch has no --isolation flag — the machine default is used.
		isoMode, isoErr := container.Resolve(container.ResolveInput{
			ConfigDefault: cfg.DefaultIsolationMode,
		})
		if isoErr != nil {
			return isoErr
		}

		// Look up the isolation capabilities for this mode. All per-mode branching
		// below reads from isoCaps rather than comparing against raw mode constants.
		isoCaps := container.CapabilitiesFor(isoMode)

		// Load profiles.json for container/bwrap/sandbox-exec config injection and
		// agent env var injection (host mode). Always attempt to load; treat missing
		// file as fatal when sandboxed (podman, bwrap, or sandbox-exec), since those
		// paths require the role config blob.
		var pf *config.ProfilesFile
		{
			var pfErr error
			pf, pfErr = config.LoadProfiles()
			if pfErr != nil {
				if isoCaps.NeedsConfigBlob {
					return pfErr
				}
				fmt.Fprintf(os.Stderr, "[prism switch] warning: could not load profiles.json (agent env vars will not be injected): %v\n", pfErr)
				pf = nil
			}
		}

		// Populate harness-specific env var names from the adapter so that
		// no opencode-specific string literals appear in session.go.
		// "opencode" is the only registered harness; fall back gracefully.
		switchHarness, _ := harness.New("opencode", "", nil, "", "")
		opts := session.Opts{
			Fresh:            fresh,
			IsolationMode:    string(isoMode),
			PluginHostPath:   cfg.SidecarPluginPath,
			ConfigEnvVarName: switchHarness.ConfigEnvVar(),
			RuntimeEnvVars:   switchHarness.RuntimeEnv(),
		}
		// AgentEnvVars only applies to host-mode sessions; sandboxed sessions
		// receive env vars via podman --env flags in the sidecar (podman) or
		// via the bwrap environment pass-through.
		if pf != nil && !isoCaps.NeedsConfigBlob {
			opts.AgentEnvVars = pf.AgentEnvVars
		}

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
				if isoCaps.NeedsConfigBlob && pf != nil {
					if err := injectContainerConfig(worktrees[0], pf, &o, "prism switch"); err != nil {
						return err
					}
				}
				if isoCaps.NeedsConfigBlob {
					if err := writeHarnessConfigBlobFor(isoMode, session.NameFor(worktrees[0], p), o.ConfigContent, "switch"); err != nil {
						return err
					}
				}
				return ensureAndSwitch(worktrees[0], p, o)
			}
			if bareRoot := git.BareRoot(p); bareRoot != "" {
				o := opts
				if isoCaps.NeedsConfigBlob && pf != nil {
					if err := injectContainerConfig(p, pf, &o, "prism switch"); err != nil {
						return err
					}
				}
				if isoCaps.NeedsConfigBlob {
					if err := writeHarnessConfigBlobFor(isoMode, session.NameFor(p, bareRoot), o.ConfigContent, "switch"); err != nil {
						return err
					}
				}
				return ensureAndSwitch(p, bareRoot, o)
			}
			o := opts
			if isoCaps.NeedsConfigBlob && pf != nil {
				if err := injectContainerConfig(p, pf, &o, "prism switch"); err != nil {
					return err
				}
			}
			if isoCaps.NeedsConfigBlob {
				if err := writeHarnessConfigBlobFor(isoMode, session.NameFor(p, ""), o.ConfigContent, "switch"); err != nil {
					return err
				}
			}
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
			return handleCloneRepo(pf, opts, isoCaps)

		default:
			p := chosen.path
			switch {
			case git.IsBareRepo(p):
				return handleBareRepo(p, pf, opts, isoCaps)
			case git.IsRegularRepo(p):
				return handleRegularRepo(p, pf, opts, isoCaps)
			default:
				o := opts
				if isoCaps.NeedsConfigBlob && pf != nil {
					if err := injectContainerConfig(p, pf, &o, "prism switch"); err != nil {
						return err
					}
				}
				if isoCaps.NeedsConfigBlob {
					if err := writeHarnessConfigBlobFor(isoMode, session.NameFor(p, ""), o.ConfigContent, "switch"); err != nil {
						return err
					}
				}
				return ensureAndSwitch(p, "", o)
			}
		}
	},
}

func init() {
	switchCmd.Flags().String("path", "", "Open a specific path directly (skip picker)")
	switchCmd.Flags().Bool("fresh", false, "Start a fresh opencode session, ignoring any stored session ID")
	rootCmd.AddCommand(switchCmd)
}
