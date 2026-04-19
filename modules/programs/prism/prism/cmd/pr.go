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
	"github.com/prismatic-koi/prism/internal/git"
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
			repo := filepath.Base(bareRoot)
			var resp struct {
				SessionName string `json:"session_name"`
			}
			if proxyErr := proxyToHostAPI(apiURL, "/spawn", map[string]any{
				"repo":   repo,
				"branch": branch,
				"prompt": promptFlag,
				"agent":  agentFlag,
			}, &resp); proxyErr != nil {
				return proxyErr
			}
			fmt.Printf("session %q created\n", resp.SessionName)
			return nil
		}

		cfg := config.Load()
		isoMode := cfg.EffectiveIsolationMode()
		effectiveContainerMode := isoMode == config.IsolationPodman
		conCapped := isoMode == config.IsolationPodman || isoMode == config.IsolationBwrap

		// Concurrency cap check: BEFORE any container-creation side effects
		// (no worktree, no tmux session, no DB row on refusal).
		if err := checkConcurrencyCap(cmd, "pr", conCapped); err != nil {
			return err
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

		// In container/bwrap mode, inject the role-specific opencode.json blob
		// as OPENCODE_CONFIG_CONTENT so it takes precedence (level 6) over any
		// project-level opencode.jsonc. This mirrors the pattern in spawn.go.
		var configContent string
		if effectiveContainerMode {
			pf, pfErr := config.LoadProfiles()
			if pfErr != nil {
				return pfErr
			}
			effectiveRole := session.DefaultAgent(worktreePath, agentFlag)
			roleConfig, roleErr := config.ContainerConfigForRole(pf, effectiveRole)
			if roleErr != nil {
				return roleErr
			}
			if roleConfig != "" {
				configContent = roleConfig
			} else if effectiveRole == "worker" || effectiveRole == "coordinator" {
				fmt.Fprintf(os.Stderr, "[prism pr] warning: no container role config for %q in profiles.json — rebuild the system config to generate it\n", effectiveRole)
			}
		}

		opts := session.Opts{
			Prompt:         promptFlag,
			Agent:          agentFlag,
			Headless:       !attachFlag,
			ContainerMode:  effectiveContainerMode,
			IsolationMode:  string(isoMode),
			PluginHostPath: cfg.SidecarPluginPath,
			ConfigContent:  configContent,
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
