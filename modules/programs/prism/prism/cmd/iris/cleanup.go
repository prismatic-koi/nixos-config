// cleanup.go — `iris cleanup <session>` subcommand (D-10 parity).
//
// Removes the artefacts of an iris session: archive JSONL, per-session
// run dir + tmpdir, worktree + branch (optional), DB row end-state.
// This is the iris analogue of `prism cleanup` and never invokes any
// prism code path — see internal/iris/cleanup.go for the implementation
// and contract.

package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/iris"
)

var (
	cleanupRemoveWorktree bool
	cleanupPIAgentDir     string
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup <session>",
	Short: "Clean up an iris session (archive JSONL, remove run dir, end DB row)",
	Long: `Clean up the named iris session. Cleanup is best-effort: each step
runs independently, and partial failures are surfaced in the summary
output rather than aborting the command.

Steps:

  1. Archive the pi JSONL into ~/code/archives/iris/<session>/<instance>/raw/session.jsonl
  2. Mark the sessions row ended (end_state="finished") if not already terminal.
  3. Remove the per-session run dir at ~/.local/state/iris/run/<instance>/.
  4. Optionally remove the worktree directory and local git branch
     (use --remove-worktree).

The coordinator's main worktree is always protected — cleanup refuses to
remove a worktree whose basename is "main" under a prism .bare layout.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCleanup(args[0])
	},
}

func init() {
	cleanupCmd.Flags().BoolVar(&cleanupRemoveWorktree, "remove-worktree", false, "Remove the worktree directory and local git branch (coordinator worktree is always protected)")
	cleanupCmd.Flags().StringVar(&cleanupPIAgentDir, "pi-agent-dir", "", "Override the pi agent dir (default: ~/.pi/agent/)")
	rootCmd.AddCommand(cleanupCmd)
}

func runCleanup(sessionName string) error {
	p := iris.ResolvePaths()
	database, err := iris.OpenDB(p.DB)
	if err != nil {
		return fmt.Errorf("iris cleanup: open db: %w", err)
	}
	defer database.Close()

	res, err := iris.CleanupSession(context.Background(), iris.CleanupConfig{
		Database:       database,
		RunDir:         p.RunDir,
		LogDir:         p.LogDir,
		ArchiveRoot:    p.ArchiveRoot,
		PIAgentDir:     cleanupPIAgentDir,
		RemoveWorktree: cleanupRemoveWorktree,
	}, sessionName)
	if err != nil {
		return fmt.Errorf("iris cleanup: %w", err)
	}

	fmt.Printf("iris cleanup: %s\n", sessionName)
	if res.ArchivePath != "" {
		fmt.Printf("  archive:        %s\n", res.ArchivePath)
	} else {
		fmt.Printf("  archive:        (skipped — no pi JSONL found)\n")
	}
	fmt.Printf("  run dir:        removed=%v\n", res.RunDirRemoved)
	fmt.Printf("  log file:       removed=%v\n", res.LogFileRemoved)
	fmt.Printf("  session row:    ended=%v\n", res.SessionRowRemoved)
	fmt.Printf("  worktree:       removed=%v\n", res.WorktreeRemoved)
	fmt.Printf("  branch:         removed=%v\n", res.BranchRemoved)
	if len(res.Errors) > 0 {
		fmt.Println("  errors:")
		for _, e := range res.Errors {
			fmt.Printf("    - %v\n", e)
		}
	}
	return nil
}
