package cmd

// prism spawn — create a new timestamped worktree from the current (or named)
// repo and switch to it immediately. Bound to prefix+a.
//
// Flags:
//
//	--branch <name>       use a specific branch name instead of a timestamp
//	--pr <number>         check out the branch for a given PR number
//	--repo <name>         repo shorthand name or absolute path (default: inferred from current pane)
//	--prompt <text>       pass an initial prompt to opencode on launch
//	--prompt-file <path>  read the initial prompt from a file
//	--agent <name>        opencode agent to use (default: "coordinator" on main, "worker" otherwise)
//	--profile <name>      model profile to use from ~/.config/prism/profiles.json
//	--model <name>        model identifier override (overrides profile's primary model)
//	--variant <name>      model variant override (overrides all agents' variant)
//	--isolation <mode>    isolation mode: podman, bwrap, sandbox-exec, or host (default: from config.json)
//	--host-mode           deprecated alias for --isolation host
//	--harness <name>      agent harness to use (default: "opencode"; only "opencode" is supported)

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// proxySpawn forwards a spawn request to the host-API sidecar when running
// inside a container (PRISM_HOST_API is set). It reads the same flags as
// runSpawn and POSTs them to /spawn, then prints the returned session name.
//
// The "repo" field is intentionally omitted from the request: the sidecar
// derives the repo from its own session name, so a client running inside a
// container where PRISM_BARE_ROOT is a mount-path name (e.g. "/prism-git")
// does not need to supply the correct repo name. See issue #616.
//
// The "isolation" field forwards the --isolation flag value when explicitly
// set. Validation of unknown values happens client-side here so the error
// surfaces immediately at the proxy boundary with the same error message
// shape as the direct (host-shell) path; see issue #1059. Without this, a
// coordinator running inside a container could silently drop --isolation host
// (or any other valid value) because the proxy never read the flag — the
// sidecar fell back to its default mode and the user only noticed by reading
// the sidecar log line.
func proxySpawn(apiURL string, cmd *cobra.Command) error {
	branchFlag, _ := cmd.Flags().GetString("branch")
	agentFlag, _ := cmd.Flags().GetString("agent")
	profileFlag, _ := cmd.Flags().GetString("profile")
	modelFlag, _ := cmd.Flags().GetString("model")
	variantFlag, _ := cmd.Flags().GetString("variant")
	hostModeFlag, _ := cmd.Flags().GetBool("host-mode")
	harnessFlag, _ := cmd.Flags().GetString("harness")
	ignoreConcurrencyCapFlag, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
	isolationFlag, _ := cmd.Flags().GetString("isolation")

	// Detect explicit-set state for the mutually-exclusive isolation flags so
	// the proxy mirrors the validation behaviour of the direct (host-shell)
	// path in resolveIsolationMode.
	isolationChanged := cmd.Flags().Changed("isolation")
	hostModeChanged := cmd.Flags().Changed("host-mode")

	// Reject simultaneous use of --isolation and --host-mode at the proxy
	// boundary so the user gets the same error as the direct path. Without
	// this, the host-API server would still see both fields populated and
	// would have to re-run the same check.
	if isolationChanged && hostModeChanged {
		return fmt.Errorf("--isolation and --host-mode cannot be used together; --host-mode is a deprecated alias for --isolation host")
	}

	// Validate --isolation client-side so unknown values fail fast with the
	// same error message as the direct path. The platform guards in
	// resolveIsolationMode (bwrap on non-Linux, sandbox-exec on non-Darwin)
	// are intentionally NOT applied here: the proxy is running inside a
	// container, but the spawned session lands on the host, so the host's
	// platform — not the proxy's — is what matters. The host-side prism spawn
	// re-runs resolveIsolationMode and will reject an incompatible mode there.
	if isolationChanged {
		if !config.IsValidIsolationMode(isolationFlag) {
			valid := make([]string, len(config.ValidIsolationModes))
			for i, m := range config.ValidIsolationModes {
				valid[i] = string(m)
			}
			return fmt.Errorf("unknown isolation mode %q; valid values: %s", isolationFlag, strings.Join(valid, ", "))
		}
	}

	promptFlag, err := resolvePrompt(cmd)
	if err != nil {
		return err
	}
	var resp struct {
		SessionName string `json:"session_name"`
	}
	body := map[string]any{
		"branch":                 branchFlag,
		"prompt":                 promptFlag,
		"agent":                  agentFlag,
		"profile":                profileFlag,
		"model":                  modelFlag,
		"variant":                variantFlag,
		"host_mode":              hostModeFlag,
		"harness":                harnessFlag,
		"ignore_concurrency_cap": ignoreConcurrencyCapFlag,
	}
	// Only forward "isolation" when explicitly set. An empty value would tell
	// the host-side spawn to fall back to config.json, which is correct only
	// when the user really did not pass the flag — we must distinguish the
	// "absent" and "empty" cases here.
	if isolationChanged {
		body["isolation"] = isolationFlag
	}
	if err := proxyToHostAPI(apiURL, "/spawn", body, &resp); err != nil {
		return err
	}
	fmt.Printf("session %q created\n", resp.SessionName)
	return nil
}

var spawnCmd = &cobra.Command{
	Use:   "spawn",
	Short: "Create a new worktree from the current (or named) repo and switch to it",
	RunE:  runSpawn,
}

func init() {
	spawnCmd.Flags().String("branch", "", "Branch name (default: timestamped)")
	spawnCmd.Flags().String("pr", "", "PR number — check out its branch")
	spawnCmd.Flags().String("repo", "", "Repo shorthand name or absolute path (default: inferred from current pane)")
	addPromptFlags(spawnCmd)
	spawnCmd.Flags().String("agent", "", `Opencode agent to use (default: "coordinator" on main, "worker" otherwise)`)
	spawnCmd.Flags().Bool("attach", false, "Switch the current tmux client to the new session")
	spawnCmd.Flags().String("profile", "", "Model profile name from ~/.config/prism/profiles.json (e.g. anthropic, gemini-hybrid)")
	spawnCmd.Flags().String("model", "", "Model identifier override (e.g. anthropic/claude-sonnet-4-6); overrides profile's primary model")
	spawnCmd.Flags().String("variant", "", "Model variant override for all agents (e.g. high, max, minimal)")
	spawnCmd.Flags().String("isolation", "", "Isolation mode: podman, bwrap, sandbox-exec, or host (default: from ~/.config/prism/config.json)")
	spawnCmd.Flags().Bool("host-mode", false, "Deprecated alias for --isolation host; bypass container mode and run opencode directly in the tmux pane")
	spawnCmd.Flags().String("harness", "opencode", "Agent harness to use; valid values are determined by registered harnesses")
	spawnCmd.Flags().Bool("ignore-concurrency-cap", false, "Bypass the soft concurrency cap and spawn even when >= 6 containers are in flight")
	rootCmd.AddCommand(spawnCmd)
}

// resolveIsolationMode returns the effective isolation mode for a spawn
// invocation, applying flag precedence and validation via registry.Resolve:
//
//  1. --isolation flag (explicit override), validated against known values
//  2. --host-mode flag (deprecated alias for "host")
//  3. cfg.DefaultIsolationMode (from config.json; compiled-in default "host")
//
// Returns an error if both --isolation and --host-mode are set, or if
// --isolation has an unknown value, or if the resolved mode is "bwrap" on
// a non-Linux platform, or if the resolved mode is "sandbox-exec" on a
// non-Darwin platform.
//
// D1 (issue #1133): platform availability is checked via the registered
// Isolator's Available() method — but only for non-container modes. The
// podman binary/socket/image checks live in the runSpawn caller (gated by
// isoCaps.IsContainer) so resolveIsolationMode keeps its pre-refactor
// surface (no podman daemon required to resolve the mode under test).
func resolveIsolationMode(cmd *cobra.Command, cfg config.Config) (config.IsolationMode, error) {
	isolationFlag, _ := cmd.Flags().GetString("isolation")
	hostModeFlag, _ := cmd.Flags().GetBool("host-mode")

	mode, err := container.Resolve(container.ResolveInput{
		IsolationFlag:        isolationFlag,
		IsolationFlagChanged: cmd.Flags().Changed("isolation"),
		HostModeFlag:         hostModeFlag,
		HostModeFlagChanged:  cmd.Flags().Changed("host-mode"),
		ConfigDefault:        cfg.DefaultIsolationMode,
	})
	if err != nil {
		return "", err
	}
	// Skip Available() for container modes — the container availability
	// check (podman binary, socket, image) runs later in runSpawn so it
	// happens after the worktree-irrelevant pre-flight is complete and so
	// resolveIsolationMode itself stays usable on hosts without podman.
	if container.CapabilitiesFor(mode).IsContainer {
		return mode, nil
	}
	iso, err := container.For(mode, container.ConstructorOpts{})
	if err != nil {
		return "", err
	}
	if err := iso.Available(); err != nil {
		return "", err
	}
	return mode, nil
}

func runSpawn(cmd *cobra.Command, args []string) error {
	// Silence the cobra usage block for runtime errors. Flag parse errors
	// (unknown flags, wrong argument count) are handled before RunE is called
	// and still print usage — this only silences errors returned from RunE.
	cmd.SilenceUsage = true

	if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
		return proxySpawn(apiURL, cmd)
	}

	branchFlag, _ := cmd.Flags().GetString("branch")
	prFlag, _ := cmd.Flags().GetString("pr")
	repoFlag, _ := cmd.Flags().GetString("repo")
	agentFlag, _ := cmd.Flags().GetString("agent")
	profileFlag, _ := cmd.Flags().GetString("profile")
	modelFlag, _ := cmd.Flags().GetString("model")
	variantFlag, _ := cmd.Flags().GetString("variant")
	harnessFlag, _ := cmd.Flags().GetString("harness")

	// Validate harness BEFORE any session state is created (no worktree, no
	// tmux session, no DB row).
	if _, ok := harness.Lookup(harnessFlag); !ok {
		return fmt.Errorf("unknown harness %q: valid harnesses: %s", harnessFlag, strings.Join(harness.Names(), ", "))
	}

	promptFlag, err := resolvePrompt(cmd)
	if err != nil {
		return err
	}

	attachFlag, _ := cmd.Flags().GetBool("attach")
	// headless when invoked from a shell/agent rather than the tmux keybinding.
	// The keybinding sets PRISM_SPAWN_PATH; --attach overrides to force a switch.
	fromKeybind := os.Getenv("PRISM_SPAWN_PATH") != ""
	cfg := config.Load()

	// Resolve the effective isolation mode. This validates --isolation,
	// maps --host-mode to "host", and falls back to config.json.
	// Done BEFORE any side effects (no worktree, no tmux session, no DB row).
	isolationMode, err := resolveIsolationMode(cmd, cfg)
	if err != nil {
		return err
	}

	// Look up the isolation capabilities for this mode. All per-mode branching
	// below reads from isoCaps rather than comparing against raw mode constants.
	isoCaps := container.CapabilitiesFor(isolationMode)

	// Container availability check: when container mode is active verify
	// podman is available before touching anything. resolveIsolationMode
	// has already validated platform availability for bwrap / sandbox-exec
	// (D1: iso.Available()); the podman binary/socket/image checks remain
	// here because they fire only when isoCaps.IsContainer.
	if isoCaps.IsContainer {
		iso, isoErr := container.For(isolationMode, container.ConstructorOpts{})
		if isoErr != nil {
			return isoErr
		}
		if err := iso.Available(); err != nil {
			return err
		}
	}

	// Concurrency cap checks: BEFORE any container-creation side effects
	// (no worktree, no tmux session, no DB row on refusal).
	// The podman cap guards host memory against container overhead.
	// The bwrap cap guards against process-count exhaustion from uncapped
	// bwrap sessions (each is a host process with no per-session memory ceil).
	// The sandbox-exec cap mirrors the bwrap cap for Darwin sessions.
	//
	// D2 (issue #1133): the per-mode if-mode-X branches collapse into a
	// single runConcurrencyCap dispatch.
	if err := runConcurrencyCap(cmd, "spawn", isolationMode, isoCaps); err != nil {
		return err
	}

	// Load the profiles file. It carries container role configs, model profile
	// overrides, and agent_env_vars for host-mode injection. Always attempt to
	// load; treat missing file as fatal only when container mode or --profile
	// is active (those paths strictly require the file). For host-mode sessions
	// without a profile flag, a missing file is non-fatal — agent env vars
	// simply won't be injected.
	var pf *config.ProfilesFile
	{
		var loadErr error
		pf, loadErr = config.LoadProfiles()
		if loadErr != nil {
			if isoCaps.NeedsConfigBlob || profileFlag != "" {
				return loadErr
			}
			fmt.Fprintf(os.Stderr, "[prism spawn] warning: could not load profiles.json (agent env vars will not be injected): %v\n", loadErr)
			pf = nil
		}
	}
	// Resolve the active profile applying the runtime-state file precedence
	// from #1207:
	//
	//   1. Explicit --profile flag (highest)
	//   2. Runtime state file at $XDG_STATE_HOME/prism/active-profile
	//   3. pf.Default (the nix-configured default, lowest)
	//
	// resolvedProfile threads through every downstream consumer that
	// previously took profileFlag directly: BuildConfigContent (so the
	// rendered OPENCODE_CONFIG_CONTENT carries the runtime profile),
	// RequireSlot (so the spawn-time slot guard runs against the actually
	// active profile), and ApplyModelOverrides (so --model overrides target
	// the runtime profile's primary tier rather than the nix default's).
	//
	// Errors from the state-file read path are surfaced — corrupt state is
	// a real problem, not a fallthrough condition.
	resolvedProfile, profileSource, err := config.ResolveActiveProfile(pf, profileFlag)
	if err != nil {
		return err
	}
	_ = profileSource // intentionally unused — kept for future debug logging

	configContent, err := config.BuildConfigContent(pf, resolvedProfile, modelFlag, variantFlag)
	if err != nil {
		return err
	}

	// Resolve the bare repo root from the current pane path (or --repo flag).
	bareRoot, err := resolveBareRoot(repoFlag)
	if err != nil {
		return err
	}

	// Resolve the branch name.
	branch, err := resolveBranch(bareRoot, branchFlag, prFlag)
	if err != nil {
		return err
	}

	// Validate the active profile defines a slot for the session's role
	// before spending I/O on worktree creation (#1206 edge case AC). The
	// active profile here is whatever ResolveActiveProfile returned above —
	// flag → state file → nix default — so a stale runtime state file
	// pointing at an invalid profile is rejected here with the same shape
	// of error as a bad --profile flag (#1207 edge-case AC).
	//
	// The session role is the explicit --agent flag when set, otherwise
	// inferred from the branch name (main → coordinator, anything else →
	// worker, mirroring session.DefaultAgent).
	if pf != nil && resolvedProfile != "" {
		plannedRole := agentFlag
		if plannedRole == "" {
			if branch == "main" {
				plannedRole = "coordinator"
			} else {
				plannedRole = "worker"
			}
		}
		if err := config.RequireSlot(pf, resolvedProfile, plannedRole); err != nil {
			// When the failure stems from the state file (not the flag),
			// add a hint pointing at `prism profile use` so users have a
			// clear recovery path (#1207 edge-case AC).
			if profileFlag == "" && profileSource == "state-file" {
				return fmt.Errorf("%w\nhint: the active profile is set via the runtime state file ($XDG_STATE_HOME/prism/active-profile). Run `prism profile use <name>` to switch to a valid profile",
					err)
			}
			return err
		}
	}

	// Create the worktree (handles local, remote-tracking, and new branches).
	worktreePath, err := git.CreateWorktree(bareRoot, branch)
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	// In container/bwrap/sandbox-exec mode, inject the role-specific opencode.json
	// blob as the harness config env var so it takes precedence (level 6) over any
	// project-level opencode.jsonc. This ensures agent identity is always
	// determined by Nix config, not by the project file.
	//
	// effectiveRole is derived the same way the session uses: explicit --agent
	// flag wins; otherwise "coordinator" on the main branch, "worker" elsewhere.
	// This must run after worktreePath is known so that DefaultAgent can inspect
	// the directory name (e.g. "main").
	//
	// This block fires for podman, bwrap, and sandbox-exec isolation modes.
	// Host-mode sessions skip it because they run opencode directly with the
	// host's real ~/.config/opencode/opencode.json via xdg.configFile.
	//
	// DefaultAgent (not DefaultAgentForSession) is intentional: at spawn time the
	// session has no DB row yet, so this call ASSIGNS the role rather than reading it.
	if isoCaps.NeedsConfigBlob && pf != nil {
		effectiveRole := session.DefaultAgent(worktreePath, agentFlag)
		// Non-worktree paths (effectiveRole == "") use the coordinator config blob
		// so that build/plan agents are available, but pass no --agent flag.
		lookupRole := effectiveRole
		if lookupRole == "" {
			lookupRole = "coordinator"
		}
		roleConfig, roleErr := config.ContainerConfigForRole(pf, lookupRole)
		if roleErr != nil {
			return roleErr
		}
		if roleConfig != "" {
			// Role config governs agent identity and permissions (system prompt,
			// tool allow-list). Re-apply any --model/--variant overrides on top
			// so that user-supplied flags are not silently discarded.
			patched, patchErr := config.ApplyModelOverrides(roleConfig, resolvedProfile, modelFlag, variantFlag, pf)
			if patchErr != nil {
				return patchErr
			}
			configContent = patched
		} else if effectiveRole == "worker" || effectiveRole == "coordinator" {
			// worker and coordinator are container-level roles that must have a
			// config blob; an empty result means the system config is stale.
			fmt.Fprintf(os.Stderr, "[prism spawn] warning: no container role config for %q in profiles.json — rebuild the system config to generate it\n", effectiveRole)
		}
		// Other agent names (plan, review, explore, …) are subagents that
		// don't have dedicated container blobs — empty result is expected.
	}

	// For bwrap sessions, write the opencode.json config file to disk now so
	// it is present before the agent pane opens. prism agent-run reconstructs
	// a container.Manager from DB state (which does not carry ConfigContent),
	// so the file must be written here at spawn time via the deterministic temp
	// path. The bwrap.go mount-emission block checks file existence (os.Stat)
	// rather than cfg.ConfigContent, so it picks this up correctly.
	//
	// Podman mode does NOT need this write — the sidecar's Create() path at
	// container.go:580 already writes the file before the container starts.
	// Host mode does NOT need this write — it uses ~/.config/opencode/opencode.json
	// directly via xdg.configFile.
	//
	// IMPORTANT: the path key used here must match the one used by Manager
	// internally. Manager.name = container.NameForSession(tmuxSessionName)
	// (e.g. "prism-nixos-config-feat"), and Manager.opencodeConfigFilePath()
	// calls OpencodeConfigFilePath(m.name). The Isolator.WriteHarnessConfigBlob
	// method translates the prism session name to the container name internally
	// so this call site stays mode-agnostic (D3, issue #1133).
	if isoCaps.NeedsConfigBlob && configContent != "" {
		tmuxSessionName := session.NameFor(worktreePath, bareRoot)
		iso, isoErr := container.For(isolationMode, container.ConstructorOpts{Name: tmuxSessionName})
		if isoErr != nil {
			return fmt.Errorf("spawn: %w", isoErr)
		}
		if err := iso.WriteHarnessConfigBlob(tmuxSessionName, configContent); err != nil {
			return fmt.Errorf("spawn: %w", err)
		}
	}

	// Build SpawnOpts and call session.SpawnSession — the single shared
	// primitive for creating a prism session end-to-end (port allocation,
	// DB seed with root_agent_name, tmux session creation, sidecar startup).
	// See #849 §3.1 and #859.
	sessionName := session.NameFor(worktreePath, bareRoot)
	// DefaultAgent (not DefaultAgentForSession) is intentional here: at spawn
	// time the session does not yet have a DB row, so there is no root_agent_name
	// to read back. This call ASSIGNS the agent type for the new session (written
	// to DB via SpawnOpts.AgentRole → UpsertStatusSeedRootAgentName). Reads of an
	// existing session's type use DefaultAgentForSession (e.g. in restore.go).
	agentRole := session.DefaultAgent(worktreePath, agentFlag)
	headless := !fromKeybind && !attachFlag

	// Resolve harness-specific env var names and runtime env vars.
	// These are populated from the harness adapter and threaded through
	// SpawnOpts → Opts → buildDirectOpencodeCmd so that no
	// opencode-specific string literals appear in the session package.
	// harnessFlag was already validated above, so the error is unreachable.
	h, _ := harness.New(harnessFlag, "", nil, "", "")
	spawnOpts := session.SpawnOpts{
		SessionName:      sessionName,
		Repo:             deriveRepo(worktreePath),
		Worktree:         worktreePath,
		AgentRole:        agentRole,
		Prompt:           promptFlag,
		ConfigContent:    configContent,
		Layout:           session.LayoutFull,
		IsolationMode:    string(isolationMode),
		PluginHostPath:   cfg.SidecarPluginPath,
		ConfigEnvVarName: h.ConfigEnvVar(),
		RuntimeEnvVars:   h.RuntimeEnv(),
		HarnessName:      harnessFlag,
		// ForceFresh=true: spawn always wants a new instance. If a session
		// with the same name already exists it is a stale zombie and should
		// be killed.
		ForceFresh: true,
		Headless:   headless,
		// ReadinessTimeout=DefaultReadinessTimeout (30s) gates SpawnSession's
		// return on opencode actually binding its port (#1051 AC-14).
		// Single-worker spawns benefit from the same readiness check that
		// review fan-outs do: an operator running `prism spawn --branch foo`
		// sees a clear "failed to start: not ready within 30s" instead of
		// "session created" followed by a session that idles forever
		// because opencode never came up.
		ReadinessTimeout: session.DefaultReadinessTimeout,
	}
	// AgentEnvVars only applies to host-mode sessions; container sessions
	// receive env vars via podman --env flags in the sidecar.
	if pf != nil && !isoCaps.IsContainer {
		spawnOpts.AgentEnvVars = pf.AgentEnvVars
	}

	d, dbErr := openDB()
	if dbErr != nil {
		return fmt.Errorf("spawn: open db: %w", dbErr)
	}
	defer d.Close()

	if err := session.SpawnSession(d, spawnOpts); err != nil {
		return err
	}

	if headless {
		fmt.Printf("session %q created\n", sessionName)
		return nil
	}
	return session.Attach(sessionName)
}

// resolveBareRoot returns the bare repo root to operate on.
// If repoFlag is set, it is resolved as a shorthand name under ~/code or as a
// full path. If not set, the current pane path is used (existing behaviour).
func resolveBareRoot(repoFlag string) (string, error) {
	if repoFlag != "" {
		return resolveRepo(repoFlag)
	}

	// Fall back to inferring from the current tmux pane path.
	// PRISM_BARE_ROOT is injected by the sidecar into container environments
	// where the bare repo is mounted at a fixed path (/prism-git) that is not
	// a parent of the worktree (/workspace). The parent-walk heuristic below
	// cannot find it, so honour this override directly. Accepts both the prism
	// project root (containing .bare/) and a raw bare git dir (e.g. /prism-git
	// itself in container mode).
	if bareRootEnv := os.Getenv("PRISM_BARE_ROOT"); bareRootEnv != "" {
		if git.IsBareRepo(bareRootEnv) || git.IsRawBareGitDir(bareRootEnv) {
			return bareRootEnv, nil
		}
		return "", fmt.Errorf("PRISM_BARE_ROOT=%q is not a prism bare repo", bareRootEnv)
	}

	panePathRaw := os.Getenv("PRISM_SPAWN_PATH")
	if panePathRaw == "" {
		var err error
		panePathRaw, err = tmux.CurrentPanePath()
		if err != nil || panePathRaw == "" {
			return "", fmt.Errorf("could not determine current pane path")
		}
	}

	bareRoot := git.BareRoot(panePathRaw)
	if bareRoot == "" {
		if git.IsBareRepo(panePathRaw) {
			bareRoot = panePathRaw
		}
	}
	if bareRoot == "" {
		if git.IsInsideRegularRepo(panePathRaw) {
			return "", fmt.Errorf("this repo isn't using the worktree layout yet\nuse C-f to convert it first")
		}
		return "", fmt.Errorf("not inside a git repo")
	}
	return bareRoot, nil
}

// resolveRepo resolves a repo shorthand (e.g. "nixos-config") to a full path
// under ~/code, or accepts an absolute path directly.
func resolveRepo(nameOrPath string) (string, error) {
	// Absolute or home-relative path — use as-is.
	candidate := expandHome(nameOrPath)
	if filepath.IsAbs(candidate) {
		if git.IsBareRepo(candidate) {
			return candidate, nil
		}
		return "", fmt.Errorf("not a prism bare repo: %s", candidate)
	}

	// Shorthand: look under the configured project locations.
	for _, loc := range switchProjectLocations() {
		p := filepath.Join(expandHome(loc), nameOrPath)
		if git.IsBareRepo(p) {
			return p, nil
		}
	}

	return "", fmt.Errorf(
		"repo %q not found under ~/code\nhint: run `prism clone <url>` to add it",
		nameOrPath,
	)
}

// resolveBranch determines the branch name to use for the new worktree.
// Priority: --pr flag > --branch flag > timestamped default.
func resolveBranch(bareRoot, branchFlag, prFlag string) (string, error) {
	if prFlag != "" {
		// Fetch latest remote refs so the PR branch is available.
		fmt.Printf("fetching origin for %s...\n", filepath.Base(bareRoot))
		if err := git.FetchRemote(bareRoot); err != nil {
			return "", fmt.Errorf("fetch: %w", err)
		}
		branch, err := git.PRBranch(bareRoot, prFlag)
		if err != nil {
			return "", fmt.Errorf("resolve PR branch: %w", err)
		}
		return branch, nil
	}

	if branchFlag != "" {
		sanitised := git.SanitiseBranch(branchFlag)
		if sanitised == "" {
			return "", fmt.Errorf("branch name %q is empty after sanitisation", branchFlag)
		}
		return sanitised, nil
	}

	// Default: zettelkasten timestamp.
	return time.Now().Format("20060102T1504"), nil
}
