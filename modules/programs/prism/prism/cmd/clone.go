package cmd

// prism clone — bare+worktree clone (replaces cli.git.worktreeClone)

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
)

var (
	reHTTPS = regexp.MustCompile(`https?://[^/]+/[^/]+/([^/]+?)(?:\.git)?$`)
	reSSH   = regexp.MustCompile(`git@[^:]+:(?:[^/]+/)?([^/]+?)(?:\.git)?$`)
)

func repoNameFromURL(url string) string {
	if m := reHTTPS.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	if m := reSSH.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return ""
}

var cloneCmd = &cobra.Command{
	Use:   "clone <repo-url> [directory]",
	Short: "Clone a repo into the prism bare+worktree layout",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		repoURL := args[0]

		var targetDir string
		if len(args) >= 2 {
			targetDir = args[1]
		} else {
			name := repoNameFromURL(repoURL)
			if name == "" {
				return fmt.Errorf("could not parse repository name from URL: %s", repoURL)
			}
			targetDir = name
		}

		targetDir = filepath.Clean(targetDir)

		err := git.CloneWorktree(repoURL, targetDir, func(msg string) {
			fmt.Println(msg)
		})
		if err != nil {
			return err
		}

		fmt.Printf("cloned into %s\n", targetDir)
		fmt.Printf("default branch checked out in %s/<branch>\n",
			strings.TrimSuffix(targetDir, "/"))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(cloneCmd)
}
