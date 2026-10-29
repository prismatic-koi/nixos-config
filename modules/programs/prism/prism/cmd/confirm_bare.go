package cmd

// prism confirm-bare — add the .bare of a project to the bare repo record.

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	prismSession "github.com/prismatic-koi/prism/internal/session"
)

var confirmBareCmd = &cobra.Command{
	Use:   "confirm-bare <project-root>",
	Short: "Tell prism that prism made the .bare of a project",
	Long: `Add <project-root>/.bare to the bare repo record, in the prism state dir.

On the host, prism runs git only in a .bare that is in the record. prism
clone and prism convert add each .bare that they make. The first record
gets each .bare that the session start records show, but not a .bare in a
project with no worktree session in prism.db, or in a project that was a
plain clone with a session in it.

Run this command one time for such a project, in a shell on the host. Run
it only if you made the .bare with prism clone or prism convert, and no
session ran in a plain clone of the project since then. A session that
runs in a plain clone can write the project root, so it can make a .bare
with a filter driver or a hook that host git runs.

The command refuses while a session runs in the project root or in a dir
above it: when its session record has no end time, or when tmux lists it.
If a session record refuses and the session does not run, end the record
with "prism cleanup --yes --session <name>", then run this command again.

On Darwin, if a sandboxed session ran in a plain clone of the project,
restart the host before you run this command. A restart ends a process
that the session detached (#3375).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		gitDir, err := git.ConfirmBare(args[0], sessionRunsInTmux)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "recorded %s\n", gitDir)
		return nil
	},
}

// sessionRunsInTmux asks the tmux server of the host for the session. A
// sandbox has no access to that server, so a session cannot change the
// answer.
func sessionRunsInTmux(name string) (bool, error) {
	switch prismSession.TmuxSessionPresence(name) {
	case prismSession.TmuxPresent:
		return true, nil
	case prismSession.TmuxGone:
		return false, nil
	default:
		return false, errors.New("tmux gave no clear answer")
	}
}

func init() {
	rootCmd.AddCommand(confirmBareCmd)
}
