package cmd

// prism pr <number> — shorthand for `prism spawn --pr <number>`.
//
// Checks out the branch for the given PR number in the current (or named) repo,
// creates a worktree for it, and switches to a new tmux session.
//
// Additional flags mirror prism spawn:
//
//	--repo <name>         target repo by folder name under ~/code (or full path)
//	--prompt <text>       pass an initial prompt to opencode on launch
//	--prompt-file <path>  read the initial prompt from a file
//	--agent <name>        opencode agent to use (default: "coordinator" on main, "worker" otherwise)
//	--attach              switch the current tmux client to the new session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	_ "github.com/prismatic-koi/prism/internal/harness/opencode"
	"github.com/prismatic-koi/prism/internal/session"
)

var prCmd = &cobra.Command{
	Use:   "pr <number>",
	Short: "Check out a PR branch as a new worktree and switch to it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		prNumber := args[0]
		repoFlag, _ := cmd.Flags().GetString("repo")
		agentFlag, _ := cmd.Flags().GetString("agent")
		attachFlag, _ := cmd.Flags().GetBool("attach")

		promptFlag, err := resolvePrompt(cmd)
		if err != nil {
			return err
		}

		bareRoot, err := resolveBareRoot(repoFlag)
		if err != nil {
			return err
		}

		// In container mode, proxy the spawn to the host API after resolving
		// the PR number to a branch locally (git is accessible from containers).
		if apiURL := os.Getenv("PRISM_HOST_API"); apiURL != "" {
			branch, branchErr := resolveBranch(bareRoot, "", prNumber)
			if branchErr != nil {
				return branchErr
			}
			ignoreConcurrencyCapFlag, _ := cmd.Flags().GetBool("ignore-concurrency-cap")
			repo := filepath.Base(bareRoot)
			var resp struct {
				SessionName string `json:"session_name"`
			}
			if proxyErr := proxyToHostAPI(apiURL, "/spawn", map[string]any{
				"repo":                   repo,
				"branch":                 branch,
				"prompt":                 promptFlag,
				"agent":                  agentFlag,
				"ignore_concurrency_cap": ignoreConcurrencyCapFlag,
			}, &resp); proxyErr != nil {
				return proxyErr
			}
			fmt.Printf("session %q created\n", resp.SessionName)
			return nil
		}

		cfg := config.Load()
		isoMode := cfg.DefaultIsolationMode

		// Look up the isolation capabilities for this mode. All per-mode branching
		// below reads from isoCaps rather than comparing against raw mode constants.
		isoCaps := container.CapabilitiesFor(isoMode)

		// Concurrency cap checks: BEFORE any container-creation side effects
		// (no worktree, no tmux session, no DB row on refusal).
		if err := checkConcurrencyCap(cmd, "pr", isoCaps.IsContainer); err != nil {
			return err
		}
		if isoMode == config.IsolationBwrap {
			if err := checkBwrapConcurrencyCap(cmd, "pr"); err != nil {
				return err
			}
		}

		branch, err := resolveBranch(bareRoot, "", prNumber)
		if err != nil {
			return err
		}

		fmt.Printf("checking out PR #%s → branch %q\n", prNumber, branch)

		worktreePath, err := git.CreateWorktree(bareRoot, branch)
		if err != nil {
			return fmt.Errorf("create worktree: %w", err)
		}

		// In container/bwrap/sandbox-exec mode, inject the role-specific
		// opencode.json blob as the harness config env var so it takes precedence
		// (level 6) over any project-level opencode.jsonc. This mirrors the pattern
		// in spawn.go.
		//
		// This block fires for podman, bwrap, and sandbox-exec isolation modes.
		// Host-mode sessions skip it because they run opencode directly with the
		// host's real ~/.config/opencode/opencode.json via xdg.configFile.
		var configContent string
		if isoCaps.NeedsConfigBlob {
			pf, pfErr := config.LoadProfiles()
			if pfErr != nil {
				return pfErr
			}
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
				// Role config governs agent identity and permissions. Re-apply
				// any --model/--variant overrides on top so they are not lost.
				// prism pr has no --model/--profile/--variant flags today, but
				// the call is a no-op when all overrides are empty, ensuring
				// forward-compatibility if those flags are added later.
				patched, patchErr := config.ApplyModelOverrides(roleConfig, "", "", "", pf)
				if patchErr != nil {
					return patchErr
				}
				configContent = patched
			} else if effectiveRole == "worker" || effectiveRole == "coordinator" {
				fmt.Fprintf(os.Stderr, "[prism pr] warning: no container role config for %q in profiles.json — rebuild the system config to generate it\n", effectiveRole)
			}
		}

		// For bwrap sessions, write the opencode.json config file to disk now
		// so it is present before the agent pane opens. prism agent-run
		// reconstructs a container.Manager from DB state (which does not carry
		// ConfigContent), so the file must be written here via the deterministic
		// temp path. The bwrap.go mount-emission block checks file existence
		// (os.Stat) rather than cfg.ConfigContent, so it picks this up correctly.
		//
		// Podman mode does NOT need this write — the sidecar's Create() path
		// already writes the file before the container starts. Host mode does
		// NOT need this write — it uses ~/.config/opencode/opencode.json
		// directly via xdg.configFile. sandbox-exec mode does NOT yet use this
		// path — config delivery for sandbox-exec is deferred to #1016 (no
		// bwrap-equivalent mount mechanism exists yet).
		//
		// IMPORTANT: the path key used here must match the one used by Manager
		// internally. Manager.name = container.NameForSession(tmuxSessionName),
		// and Manager.opencodeConfigFilePath() calls OpencodeConfigFilePath(m.name).
		// So we must pass the container name (not the raw tmux session name) to
		// WriteOpencodeConfig. This mirrors the pattern in spawn.go.
		if isoCaps.NeedsConfigBlob && configContent != "" {
			tmuxSessionName := session.NameFor(worktreePath, bareRoot)
			containerName := container.NameForSession(tmuxSessionName)
			if err := container.WriteOpencodeConfig(containerName, configContent); err != nil {
				return fmt.Errorf("prism pr: %w", err)
			}
		}

		prHarness, _ := harness.New("opencode", "", nil, "", "")
		opts := session.Opts{
			Prompt:           promptFlag,
			Agent:            agentFlag,
			Headless:         !attachFlag,
			IsolationMode:    string(isoMode),
			PluginHostPath:   cfg.SidecarPluginPath,
			ConfigContent:    configContent,
			ConfigEnvVarName: prHarness.ConfigEnvVar(),
			RuntimeEnvVars:   prHarness.RuntimeEnv(),
			// ForceFresh=true: prism pr creates a new worktree; if a session
			// with the same name exists it is a stale zombie and should be
			// killed, matching the same semantics as prism spawn.
			ForceFresh: true,
		}
		return ensureAndSwitch(worktreePath, bareRoot, opts)
	},
}

func init() {
	prCmd.Flags().String("repo", "", "Target repo name under ~/code, or full path")
	addPromptFlags(prCmd)
	prCmd.Flags().String("agent", "", `Opencode agent to use (default: "coordinator" on main, "worker" otherwise)`)
	prCmd.Flags().Bool("attach", false, "Switch the current tmux client to the new session")
	prCmd.Flags().Bool("ignore-concurrency-cap", false, "Bypass the soft concurrency cap and spawn even when >= 6 containers are in flight")
	rootCmd.AddCommand(prCmd)
}
