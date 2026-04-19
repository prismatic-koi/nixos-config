package cmd

// prism agent-run — exec the bwrap sandbox for a bwrap-mode agent session.
//
// This command is invoked by the tmux agent window when the resolved isolation
// mode is "bwrap". It reconstructs the container.Manager from the session's DB
// row and config, writes the same temp files that Manager.Create() writes for
// podman sessions (SSH config, gitconfig, opencode.json), and then execs:
//
//	bwrap <args...>
//
// The bwrap subprocess runs opencode directly inside the sandbox. It is owned
// by this process (and thus by the tmux pane) — not by the sidecar. The sidecar
// is already running for bwrap sessions, handling SSE, state transitions, and
// the host-API socket.
//
// On non-Linux platforms the command fails immediately with a clear error
// because bubblewrap is Linux-only.
//
// Flags:
//
//	--session <name>   prism session name (e.g. "nixos-config@feature")

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/session"
)

var agentRunCmd = &cobra.Command{
	Use:   "agent-run",
	Short: "Exec the bwrap sandbox for a bwrap-mode agent session (internal)",
	Long: `Exec the bwrap sandbox for a bwrap-mode agent session.

This command is an internal implementation detail invoked by the tmux agent
window when the isolation mode is "bwrap". It is not intended for direct user
invocation.

It reconstructs the container config from the session's DB row, writes temp
files (SSH config, gitconfig), builds the bwrap argument list, and execs bwrap
directly — replacing the current process with the bwrap sandbox running opencode.`,
	RunE: runAgentRun,
}

func init() {
	agentRunCmd.Flags().String("session", "", "Prism session name (e.g. nixos-config@main)")
	_ = agentRunCmd.MarkFlagRequired("session")
	rootCmd.AddCommand(agentRunCmd)
}

func runAgentRun(cmd *cobra.Command, args []string) error {
	// Fail fast on non-Linux: bubblewrap is Linux-only.
	if runtime.GOOS != "linux" {
		return fmt.Errorf("prism agent-run: bwrap isolation requires Linux; current platform is %s", runtime.GOOS)
	}

	sessionName, _ := cmd.Flags().GetString("session")

	// Open the prism database to look up session state.
	d, err := openDB()
	if err != nil {
		return fmt.Errorf("agent-run: open database: %w", err)
	}
	status, err := d.CurrentStatus(sessionName)
	d.Close()
	if err != nil {
		return fmt.Errorf("agent-run: query session %q: %w", sessionName, err)
	}
	if status == nil {
		return fmt.Errorf("agent-run: session %q not found in DB", sessionName)
	}

	// Verify this is actually a bwrap session.
	isoMode := status.EffectiveIsolationMode()
	if isoMode != "bwrap" {
		return fmt.Errorf("agent-run: session %q has isolation mode %q, not bwrap; this command is only for bwrap sessions", sessionName, isoMode)
	}

	// Load prism config for git identity and SSH key names.
	cfg := config.Load()

	// Reconstruct the container.Config from the session state.
	worktree := status.Worktree
	if worktree == "" {
		return fmt.Errorf("agent-run: session %q has no recorded worktree", sessionName)
	}
	bareRoot := git.BareRoot(worktree)
	var worktreeGitDir string
	if bareRoot != "" {
		worktreeGitDir = filepath.Join(bareRoot, ".bare", "worktrees", filepath.Base(worktree))
	}

	// Resolve port from the DB status. AllocatedPort is used for the opencode
	// --port flag in the bwrap args.
	port := 0
	if status.OpencodePort != nil {
		port = *status.OpencodePort
	}

	// Resolve agent role from status.
	agentRole := ""
	if status.RootAgentName != nil {
		agentRole = *status.RootAgentName
	}
	if agentRole == "" {
		agentRole = session.DefaultAgent(worktree, "")
	}

	// Look up the host-API socket path so bwrap can mount it.
	hostAPISockPath := ""
	if sockPath, sockErr := session.SidecarHostAPIPath(sessionName); sockErr == nil {
		hostAPISockPath = sockPath
	}

	ctrCfg := container.Config{
		SessionName:       sessionName,
		Worktree:          worktree,
		BareRoot:          bareRoot,
		WorktreeGitDir:    worktreeGitDir,
		AllocatedPort:     port,
		AgentRole:         agentRole,
		GitUserName:       cfg.GitUserName,
		GitUserEmail:      cfg.GitUserEmail,
		SshAccessKeyName:  cfg.SshAccessKeyName,
		SshSigningKeyName: cfg.SshSigningKeyName,
		HostAPISockPath:   hostAPISockPath,
	}

	// Construct the Manager. PrepareBwrap will write temp files and build args.
	m := container.New(ctrCfg)

	bwrapArgs, err := m.PrepareBwrap()
	if err != nil {
		return fmt.Errorf("agent-run: prepare bwrap args: %w", err)
	}

	// Locate the bwrap binary.
	bwrapBin, err := findBwrap()
	if err != nil {
		return fmt.Errorf("agent-run: %w", err)
	}

	// exec replaces the current process with bwrap. argv[0] must be the binary path.
	argv := append([]string{bwrapBin}, bwrapArgs...)
	return syscall.Exec(bwrapBin, argv, os.Environ())
}

// findBwrap locates the bwrap binary on PATH or in well-known Nix store paths.
func findBwrap() (string, error) {
	// Try PATH first.
	if path, err := exec.LookPath("bwrap"); err == nil {
		return path, nil
	}
	// Fall back to common NixOS store locations.
	candidates := []string{
		"/run/current-system/sw/bin/bwrap",
		"/usr/bin/bwrap",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("bwrap binary not found on PATH or in well-known locations; ensure bubblewrap is installed (pkgs.bubblewrap)")
}
