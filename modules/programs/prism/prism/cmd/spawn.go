package cmd

// prism spawn — create a new timestamped worktree from the current (or named)
// repo and switch to it immediately. Bound to prefix+a.
//
// Flags:
//
//	--branch <name>       use a specific branch name instead of a timestamp
//	--pr <number>         check out the branch for a given PR number
//	--prompt <text>       pass an initial prompt to opencode on launch
//	--prompt-file <path>  read the initial prompt from a file
//	--agent <name>        opencode agent to use (default: "coordinator" on main, "worker" otherwise)
//	--profile <name>      model profile to use (from ~/.config/prism/profiles.json)
//	--model <name>        model identifier override (overrides profile's primary model)
//	--variant <name>      model variant override (overrides all agents' variant)
//	--host-mode           bypass container mode and run opencode directly in the tmux pane

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
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
func proxySpawn(apiURL string, cmd *cobra.Command) error {
	branchFlag, _ := cmd.Flags().GetString("branch")
	agentFlag, _ := cmd.Flags().GetString("agent")
	profileFlag, _ := cmd.Flags().GetString("profile")
	modelFlag, _ := cmd.Flags().GetString("model")
	variantFlag, _ := cmd.Flags().GetString("variant")
	hostModeFlag, _ := cmd.Flags().GetBool("host-mode")
	promptFlag, err := resolvePrompt(cmd)
	if err != nil {
		return err
	}
	var resp struct {
		SessionName string `json:"session_name"`
	}
	if err := proxyToHostAPI(apiURL, "/spawn", map[string]any{
		"branch":    branchFlag,
		"prompt":    promptFlag,
		"agent":     agentFlag,
		"profile":   profileFlag,
		"model":     modelFlag,
		"variant":   variantFlag,
		"host_mode": hostModeFlag,
	}, &resp); err != nil {
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
	spawnCmd.Flags().Bool("host-mode", false, "Bypass container mode and run opencode directly in the tmux pane")
	rootCmd.AddCommand(spawnCmd)
}

func runSpawn(cmd *cobra.Command, args []string) error {
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
	hostModeFlag, _ := cmd.Flags().GetBool("host-mode")

	promptFlag, err := resolvePrompt(cmd)
	if err != nil {
		return err
	}

	attachFlag, _ := cmd.Flags().GetBool("attach")
	// headless when invoked from a shell/agent rather than the tmux keybinding.
	// The keybinding sets PRISM_SPAWN_PATH; --attach overrides to force a switch.
	fromKeybind := os.Getenv("PRISM_SPAWN_PATH") != ""
	cfg := config.Load()

	// Determine the effective container mode: cfg.ContainerMode can be
	// overridden to false when --host-mode is passed.
	effectiveContainerMode := cfg.ContainerMode
	if hostModeFlag {
		effectiveContainerMode = false
	}

	// Container availability check: when container mode is active (and
	// --host-mode is not) verify podman is available before touching anything.
	if cfg.ContainerMode && !hostModeFlag {
		if err := container.CheckAvailability(); err != nil {
			return err
		}
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
			if effectiveContainerMode || profileFlag != "" {
				return loadErr
			}
			fmt.Fprintf(os.Stderr, "[prism spawn] warning: could not load profiles.json (agent env vars will not be injected): %v\n", loadErr)
			pf = nil
		}
	}
	configContent, err := config.BuildConfigContent(pf, profileFlag, modelFlag, variantFlag)
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

	// Create the worktree (handles local, remote-tracking, and new branches).
	worktreePath, err := git.CreateWorktree(bareRoot, branch)
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	// In container mode, inject the role-specific opencode.json blob as
	// OPENCODE_CONFIG_CONTENT so it takes precedence (level 6) over any
	// project-level opencode.jsonc. This ensures agent identity is always
	// determined by Nix config, not by the project file.
	//
	// effectiveRole is derived the same way the session uses: explicit --agent
	// flag wins; otherwise "coordinator" on the main branch, "worker" elsewhere.
	// This must run after worktreePath is known so that DefaultAgent can inspect
	// the directory name (e.g. "main").
	if effectiveContainerMode && pf != nil {
		effectiveRole := session.DefaultAgent(worktreePath, agentFlag)
		roleConfig, roleErr := config.ContainerConfigForRole(pf, effectiveRole)
		if roleErr != nil {
			return roleErr
		}
		if roleConfig != "" {
			// Role config supersedes profile/model overrides for identity &
			// permissions; use it as the primary config content.
			configContent = roleConfig
		} else if effectiveRole == "worker" || effectiveRole == "coordinator" {
			// worker and coordinator are container-level roles that must have a
			// config blob; an empty result means the system config is stale.
			fmt.Fprintf(os.Stderr, "[prism spawn] warning: no container role config for %q in profiles.json — rebuild the system config to generate it\n", effectiveRole)
		}
		// Other agent names (plan, review, explore, …) are subagents that
		// don't have dedicated container blobs — empty result is expected.
	}

	opts := session.Opts{
		Prompt:         promptFlag,
		Agent:          agentFlag,
		ConfigContent:  configContent,
		Headless:       !fromKeybind && !attachFlag,
		ContainerMode:  effectiveContainerMode,
		PluginHostPath: cfg.SidecarPluginPath,
	}
	// AgentEnvVars only applies to host-mode sessions; container sessions
	// receive env vars via podman --env flags in the sidecar.
	if pf != nil && !effectiveContainerMode {
		opts.AgentEnvVars = pf.AgentEnvVars
	}

	if err := ensureAndSwitch(worktreePath, bareRoot, opts); err != nil {
		return err
	}

	// Persist host_mode in the DB so cleanup can skip container teardown.
	if hostModeFlag {
		sessionName := session.NameFor(worktreePath, bareRoot)
		if d, dbErr := openDB(); dbErr == nil {
			if setErr := d.SetHostMode(sessionName, true); setErr != nil {
				fmt.Fprintf(os.Stderr, "[prism] spawn: set host_mode: %v\n", setErr)
			}
			d.Close()
		}
	}

	return nil
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
