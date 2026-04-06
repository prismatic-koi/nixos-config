// Package session provides composable session lifecycle operations for prism.
// It extracts the core create/attach/name logic that was previously
// embedded in the monolithic ensureAndSwitchSession function in cmd/switch.go.
package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// Opts carries optional parameters for session creation.
type Opts struct {
	// Prompt is passed to opencode via --prompt at startup.
	Prompt string
	// Agent is the opencode agent name (e.g. "coordinator", "worker").
	// When empty, DefaultAgent is called to derive a default from the directory.
	Agent string
	// Headless: if true, the session is created but no client is switched to it.
	Headless bool
	// Fresh: if true, opencode skips any stored session ID and starts fresh.
	Fresh bool
	// OpencodeSession is the opencode session ID to resume; "" means fresh start.
	OpencodeSession string
	// Layout controls which window layout to set up on creation.
	Layout Layout
	// SessionName is the canonical prism session name. When set, it is passed
	// to opencode via the PRISM_SESSION_NAME environment variable so the plugin
	// can skip its own session-name derivation.
	SessionName string
	// Port is the allocated opencode serve port. When non-zero, BuildOpencodeCmd
	// includes --port <n> and --hostname 127.0.0.1 in the opencode launch command.
	// In container mode, the port is also passed to the sidecar so it knows which
	// host port to bind.
	Port int
	// ContainerMode, when true, switches the agent window command from
	// "opencode --agent <name> --port <n>" to "opencode attach http://localhost:<n>".
	// The sidecar is responsible for starting the container and signalling readiness
	// before the attach command is sent to the tmux pane.
	ContainerMode bool
	// PluginHostPath is the host-side path to the opencode plugin file that
	// is bind-mounted into the container. Empty string = no plugin. Only used
	// when ContainerMode is true.
	PluginHostPath string
	// SkipStatusSeed, when true, causes setupFullLayout to skip the
	// "prism event tmux-session-start" call that seeds agent_status.
	// Used by the restore path, which manages agent_status directly via the
	// already-open DB handle rather than forking a subprocess.
	SkipStatusSeed bool
}

// Layout selects the window layout used when creating a new session.
type Layout int

const (
	// LayoutBare sets up a minimal session with only the default shell window.
	// Used by the dashboard dead-session recovery path.
	// This is the zero value; an uninitialised Opts{} will never accidentally
	// trigger the full three-window layout.
	LayoutBare Layout = iota

	// LayoutScratchpad sets up a single "term" window with no agent or editor.
	LayoutScratchpad

	// LayoutFull sets up the standard three-window layout:
	// window 0 "edit" (nvim), window 1 "agent" (opencode), window 2 "term".
	LayoutFull
)

// DefaultAgent returns the agent to use for the given directory.
// If explicit is non-empty it is returned unchanged.
// Otherwise "coordinator" is returned for the "main" worktree and "worker" for
// everything else.
func DefaultAgent(directory, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if filepath.Base(directory) == "main" {
		return "coordinator"
	}
	return "worker"
}

// BuildOpencodeCmd returns the opencode launch command string for the given opts.
//
// When opts.ContainerMode is true, the command is "opencode attach http://localhost:<port>"
// — a lightweight TUI client that connects to the containerised opencode serve
// instance (AC-17, AC-22). The port in the URL is opts.Port (never hardcoded).
//
// When opts.ContainerMode is false (default), the command launches opencode
// directly as before. PRISM_SESSION_NAME is prepended when opts.SessionName is
// set, and --port / --hostname are appended when opts.Port is non-zero.
func BuildOpencodeCmd(opts Opts) string {
	if opts.ContainerMode {
		if opts.Port == 0 {
			// Shouldn't happen — callers must allocate a port before enabling
			// container mode. Fall back to the non-container command.
			return buildDirectOpencodeCmd(opts)
		}
		// AC-17: opencode attach with the allocated port (not 4096).
		return fmt.Sprintf("opencode attach http://localhost:%d", opts.Port)
	}
	return buildDirectOpencodeCmd(opts)
}

// buildDirectOpencodeCmd returns the opencode direct-launch command (pre-container mode).
func buildDirectOpencodeCmd(opts Opts) string {
	agent := opts.Agent
	if agent == "" {
		agent = "worker"
	}
	cmd := "opencode --agent " + agent
	if opts.Port != 0 {
		cmd += fmt.Sprintf(" --port %d --hostname 127.0.0.1", opts.Port)
	}
	if opts.OpencodeSession != "" && !opts.Fresh {
		cmd += " -s " + opts.OpencodeSession
	}
	if opts.Prompt != "" {
		cmd += " --prompt " + shellQuote(opts.Prompt)
	}
	if opts.SessionName != "" {
		cmd = "PRISM_SESSION_NAME=" + shellQuote(opts.SessionName) + " " + cmd
	}
	return cmd
}

// shellQuote wraps s in single quotes, escaping any single quotes within.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// Create creates a new tmux session at the given directory with the given name
// and sets up the window layout specified by opts.Layout.
//
// If the session already exists, Create is a no-op and returns nil.
func Create(name, directory string, opts Opts) error {
	if tmux.HasSession(name) {
		return nil
	}

	if err := tmux.NewSessionDetached(name, directory); err != nil {
		return fmt.Errorf("new-session: %w", err)
	}

	switch opts.Layout {
	case LayoutFull:
		if err := setupFullLayout(name, directory, opts); err != nil {
			return err
		}

	case LayoutScratchpad:
		_ = tmux.RenameWindow(name+":0", "term")

	default:
		// LayoutBare (and any unknown value): no extra setup — leave the
		// default shell window as-is.
	}

	return nil
}

// setupFullLayout configures the three-window layout for a project session:
// window 0 "edit" (nvim auto-launched), window 1 "agent" (opencode),
// window 2 "term". Seeds agent_status via prism event tmux-session-start.
//
// In container mode (opts.ContainerMode), the sidecar is started first and the
// agent window runs a readiness-wait loop before running "opencode attach".
// This ensures the container is healthy before the TUI client tries to connect.
func setupFullLayout(name, directory string, opts Opts) error {
	_ = tmux.RenameWindow(name+":0", "edit")

	nvimCmd := NvimCmd(directory)
	_ = tmux.SendKeys(name+":0", nvimCmd)

	_ = tmux.NewWindow(name, 1, "agent", directory)

	// Start the sidecar before sending the agent window command.
	// In container mode the sidecar creates the container; we must start it
	// before the attach command is queued so readiness signalling works.
	if opts.Port == 0 {
		fmt.Fprintf(os.Stderr, "warning: sidecar skipped for %q — no port allocated\n", name)
	} else {
		sidecarOpts := StartSidecarOpts{
			Port:           opts.Port,
			ContainerMode:  opts.ContainerMode,
			AgentRole:      opts.Agent,
			Worktree:       directory,
			PluginHostPath: opts.PluginHostPath,
			InitialPrompt:  opts.Prompt,
		}
		if err := StartSidecarWithOpts(name, sidecarOpts); err != nil {
			// Non-fatal: log and continue. The session is created regardless.
			fmt.Fprintf(os.Stderr, "warning: could not start sidecar for %q: %v\n", name, err)
		}
	}

	// Build and send the agent window command.
	// In container mode, prepend a readiness wait so the pane blocks until
	// the sidecar has health-checked the container (AC-18, AC-19, AC-20).
	agentCmd := BuildOpencodeCmd(opts)
	if opts.ContainerMode && opts.Port != 0 {
		readyPath, pathErr := SidecarReadyPath(name)
		sidPath, sidErr := SidecarSessionPath(name)
		if pathErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not determine ready path for %q, skipping readiness wait: %v\n", name, pathErr)
		} else {
			// Remove any stale ready and session ID files from a previous
			// session lifecycle before the pane script starts polling.
			_ = os.Remove(readyPath)
			if sidErr == nil {
				_ = os.Remove(sidPath)
			} else {
				sidPath = ""
			}
			agentCmd = buildReadinessWaitCmd(readyPath, sidPath, agentCmd)
		}
	}
	// opts.SessionName must be set by the caller before setupFullLayout is
	// invoked — both ensureAndSwitch and restoreProjectSession do this.
	// BuildOpencodeCmd uses it to prefix PRISM_SESSION_NAME for the plugin.
	_ = tmux.SendKeys(name+":1", agentCmd)

	if !opts.SkipStatusSeed {
		self, selfErr := os.Executable()
		if selfErr != nil {
			return fmt.Errorf("resolve prism binary: %w", selfErr)
		}
		if err := exec.Command(self, "event", "tmux-session-start",
			"--session", name,
			"--worktree", directory).Run(); err != nil {
			return fmt.Errorf("seed agent_status: %w", err)
		}
	}

	_ = tmux.NewWindow(name, 2, "term", directory)

	focusIdx := 1
	if strings.Contains(directory, "obsidian") {
		focusIdx = 0
	}
	_ = tmux.SelectWindow(name, focusIdx)

	return nil
}

// buildReadinessWaitCmd builds a shell command that polls for the sidecar
// readiness file and, once found, runs the given attach command.
// If the wait times out (120s), it prints an error and exits (AC-20).
//
// sidPath is the optional path to the sidecar session ID file written by
// deliverInitialPrompt. When present, its contents are appended as -s <sid>
// to the attach command so opencode opens directly into the agent's session.
func buildReadinessWaitCmd(readyPath, sidPath, attachCmd string) string {
	// Poll every 0.5s for up to 120s (240 iterations).
	// On success, read the session ID (if any) and exec the attach command.
	return fmt.Sprintf(
		`i=0; while [ ! -f %s ] && [ $i -lt 240 ]; do sleep 0.5; i=$((i+1)); done; `+
			`if [ ! -f %s ]; then `+
			`echo "prism: container did not become ready within 120s" >&2; exit 1; `+
			`fi; `+
			`if [ -f %s ]; then _sid=$(cat %s); %s -s "$_sid"; else %s; fi`,
		shellQuote(readyPath), shellQuote(readyPath),
		shellQuote(sidPath), shellQuote(sidPath), attachCmd, attachCmd,
	)
}

// NvimCmd returns the nvim command to run in the edit window for the given
// directory. It auto-selects a file to open based on directory contents.
func NvimCmd(directory string) string {
	nvimCmd := "nvim"
	if des, err := os.ReadDir(directory); err == nil {
		var files []string
		for _, de := range des {
			if !de.IsDir() {
				files = append(files, filepath.Join(directory, de.Name()))
			}
		}
		switch {
		case len(files) == 1:
			nvimCmd = "nvim " + shellQuote(files[0])
		case strings.Contains(directory, "obsidian"):
			nvimCmd = "nvim +'Obsidian today'"
		default:
			readme := filepath.Join(directory, "README.md")
			if _, err := os.Stat(readme); err == nil {
				nvimCmd = "nvim " + shellQuote(readme)
			}
		}
	}
	return nvimCmd
}

// Attach switches a tmux client to the named session, or attaches directly if
// running outside tmux.
func Attach(name string) error {
	if os.Getenv("TMUX") == "" {
		return tmux.AttachSession(name)
	}

	client, _ := tmux.CurrentClient()
	if client != "" {
		return tmux.SwitchClient(client, name)
	}
	_, err := tmux.SwitchClientCurrent(name)
	return err
}

// NameFor derives the tmux session name for a given directory and optional
// project root. dir must already be expanded (no ~).
//
// When projectRoot is set (prism bare+worktree layout), the session name is
// "<repo>@<branch>" where branch is the full branch name with "/" replaced by
// "--" for tmux compatibility.
//
// Falls back to the short commit hash for detached HEAD, and to
// filepath.Base of the worktree directory when git is not available.
func NameFor(dir, projectRoot string) string {
	if projectRoot != "" {
		projName := strings.ReplaceAll(filepath.Base(projectRoot), ".", "_")
		branch := worktreeBranchComponent(dir)
		return projName + "@" + branch
	}
	return strings.ReplaceAll(filepath.Base(dir), ".", "_")
}

// worktreeBranchComponent returns the branch component of a session name for
// the worktree at dir.
func worktreeBranchComponent(dir string) string {
	ref, err := git.SymbolicRef(dir)
	if err == nil {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		return strings.ReplaceAll(branch, "/", "--")
	}
	if hash, err := git.ShortHash(dir); err == nil {
		return hash
	}
	return filepath.Base(dir)
}
