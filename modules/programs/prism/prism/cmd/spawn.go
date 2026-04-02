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
//	--agent <name>        opencode agent to use (default: "coordinator" on main, "build" otherwise)

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

var spawnCmd = &cobra.Command{
	Use:   "spawn",
	Short: "Create a new worktree from the current (or named) repo and switch to it",
	RunE:  runSpawn,
}

func init() {
	spawnCmd.Flags().String("branch", "", "Branch name (default: timestamped)")
	spawnCmd.Flags().String("pr", "", "PR number — check out its branch")
	addPromptFlags(spawnCmd)
	spawnCmd.Flags().String("agent", "", `Opencode agent to use (default: "coordinator" on main, "build" otherwise)`)
	spawnCmd.Flags().Bool("attach", false, "Switch the current tmux client to the new session")
	rootCmd.AddCommand(spawnCmd)
}

func runSpawn(cmd *cobra.Command, args []string) error {
	branchFlag, _ := cmd.Flags().GetString("branch")
	prFlag, _ := cmd.Flags().GetString("pr")
	agentFlag, _ := cmd.Flags().GetString("agent")

	promptFlag, err := resolvePrompt(cmd)
	if err != nil {
		return err
	}

	attachFlag, _ := cmd.Flags().GetBool("attach")
	// headless when invoked from a shell/agent rather than the tmux keybinding.
	// The keybinding sets PRISM_SPAWN_PATH; --attach overrides to force a switch.
	fromKeybind := os.Getenv("PRISM_SPAWN_PATH") != ""
	opts := sessionOpts{
		prompt:   promptFlag,
		agent:    agentFlag,
		headless: !fromKeybind && !attachFlag,
	}

	// Resolve the bare repo root from the current pane path.
	bareRoot, err := resolveBareRoot("")
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

	return ensureAndSwitchSession(worktreePath, bareRoot, opts)
}

// resolveBareRoot returns the bare repo root to operate on.
// If repoFlag is set, it is resolved as a shorthand name under ~/code or as a
// full path. If not set, the current pane path is used (existing behaviour).
// repoFlag is currently only used by prism pr.
func resolveBareRoot(repoFlag string) (string, error) {
	if repoFlag != "" {
		return resolveRepo(repoFlag)
	}

	// Fall back to inferring from the current tmux pane path.
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
