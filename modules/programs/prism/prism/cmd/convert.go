package cmd

// prism convert — convert a regular git repo to the prism bare+worktree layout.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
)

var convertCmd = &cobra.Command{
	Use:   "convert [path]",
	Short: "Convert a regular git repo to the prism bare+worktree layout",
	Long: `Convert a regular git clone into the prism bare+worktree layout.

The repo at PATH (defaults to the current directory) is converted in-place:
  - A bare clone is created at <path>/.bare
  - A .git file pointing to .bare is written at <path>/.git
  - The working tree is moved into <path>/<branch>/
  - The worktree index is populated so git status is clean immediately

This is the same operation performed by the C-f context switcher when you
select "[convert to bare+worktree layout]" on a regular repo.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var path string
		if len(args) == 1 {
			path = args[0]
		} else {
			var err error
			path, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("could not determine current directory: %w", err)
			}
		}

		path, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("could not resolve path: %w", err)
		}

		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("path does not exist: %s", path)
		}

		worktreePath, err := git.ConvertToBare(path, func(msg string) {
			fmt.Println(msg)
		})
		if err != nil {
			return fmt.Errorf("conversion failed: %w", err)
		}

		fmt.Printf("done — worktree at %s\n", worktreePath)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(convertCmd)
}
