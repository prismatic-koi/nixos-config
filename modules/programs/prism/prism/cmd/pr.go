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
//	--agent <name>        opencode agent to use (default: "coordinator" on main, "build" otherwise)
//	--attach              switch the current tmux client to the new session

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
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

		branch, err := resolveBranch(bareRoot, "", prNumber)
		if err != nil {
			return err
		}

		fmt.Printf("checking out PR #%s → branch %q\n", prNumber, branch)

		worktreePath, err := git.CreateWorktree(bareRoot, branch)
		if err != nil {
			return fmt.Errorf("create worktree: %w", err)
		// Propagate .pre-commit-config.yaml if it exists as a symlink in main.
		_ = git.PropagatePreCommitConfig(bareRoot, worktreePath)
		}

		opts := sessionOpts{
			prompt:   promptFlag,
			agent:    agentFlag,
			headless: !attachFlag,
		}
		return ensureAndSwitchSession(worktreePath, bareRoot, opts)
	},
}

func init() {
	prCmd.Flags().String("repo", "", "Target repo name under ~/code, or full path")
	addPromptFlags(prCmd)
	prCmd.Flags().String("agent", "", `Opencode agent to use (default: "coordinator" on main, "build" otherwise)`)
	prCmd.Flags().Bool("attach", false, "Switch the current tmux client to the new session")
	rootCmd.AddCommand(prCmd)
}
