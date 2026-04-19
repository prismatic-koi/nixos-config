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

	// Read the initial prompt from the pane env var set by session.go at
	// window-creation time. When non-empty, populate InitialPrompt so that
	// bwrap.go's BuildArgs appends --prompt to the opencode invocation.
	// The env var is set via tmux's -e flag in tmux.NewWindow, which means
	// it lives only in this pane's environment and dies with the pane.
	applyInitialPromptEnvVar(&ctrCfg)

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

	// exec replaces the current process with bwrap. argv[0] must be the binary
	// path. The child env is filtered to a minimal allow-list so that nothing
	// from the invoking shell leaks into bwrap's own process environment.
	// This is defence-in-depth: bwrap itself will also run with --clearenv
	// (see internal/container/bwrap.go) so the sandbox interior is wiped
	// regardless. Stripping here ensures secrets never appear in bwrap's
	// own /proc/<pid>/environ either, and means "ps aux" / "bwrap --help"
	// style debugging can't accidentally expose them.
	argv := append([]string{bwrapBin}, bwrapArgs...)
	return syscall.Exec(bwrapBin, argv, minimalBwrapExecEnv(os.Environ()))
}

// minimalBwrapExecEnv filters a hostEnv slice (K=V pairs, as returned by
// os.Environ()) down to a minimal allow-list that bwrap itself needs to run.
// The returned env is what the bwrap *process* sees; it is NOT the sandbox
// interior env (bwrap starts the sandbox with --clearenv and rebuilds the
// interior env from explicit --setenv pairs).
//
// Allow-list rationale:
//
//   - PATH:     bwrap uses PATH to locate interpreters when parsing some
//     arguments, and tools that bwrap itself spawns during setup rely on it.
//   - HOME, USER, LOGNAME: used in default paths, error messages, and (more
//     importantly) by any subcommand bwrap shells out to internally.
//   - TERM, LANG, LC_ALL: avoid bwrap logging locale/terminal warnings.
//
// Everything else — including PRISM_GITHUB_TOKEN_*, GITHUB_TOKEN,
// GITHUB_PACKAGES_TOKEN, ANTHROPIC_API_KEY, OPENROUTER_API_KEY, and any
// other secret a prism coordinator might export — is dropped.
func minimalBwrapExecEnv(hostEnv []string) []string {
	allow := map[string]bool{
		"PATH":    true,
		"HOME":    true,
		"USER":    true,
		"LOGNAME": true,
		"TERM":    true,
		"LANG":    true,
		"LC_ALL":  true,
	}
	out := make([]string, 0, len(allow))
	for _, kv := range hostEnv {
		// Split once on '='; malformed pairs (no '=') are skipped.
		eq := -1
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				eq = i
				break
			}
		}
		if eq <= 0 {
			continue
		}
		if allow[kv[:eq]] {
			out = append(out, kv)
		}
	}
	return out
}

// applyInitialPromptEnvVar reads PRISM_INITIAL_PROMPT from the process
// environment and, when non-empty, sets cfg.InitialPrompt. This feeds the
// existing bwrap.go BuildArgs --prompt append at lines 466-468 without
// requiring any new persistent state: the env var is set by tmux.NewWindow
// via the -e flag and exists only for the lifetime of this pane.
func applyInitialPromptEnvVar(cfg *container.Config) {
	if initialPrompt := os.Getenv("PRISM_INITIAL_PROMPT"); initialPrompt != "" {
		cfg.InitialPrompt = initialPrompt
	}
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
