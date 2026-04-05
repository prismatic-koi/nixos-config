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
	Port int
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
// When opts.SessionName is set, the returned string is prefixed with
// PRISM_SESSION_NAME=<name> so that the opencode plugin can read the canonical
// session name without having to re-derive it from the filesystem.
// When opts.Port is non-zero, --port and --hostname 127.0.0.1 are included so
// that opencode starts its HTTP serve API on the allocated port.
func BuildOpencodeCmd(opts Opts) string {
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
func setupFullLayout(name, directory string, opts Opts) error {
	_ = tmux.RenameWindow(name+":0", "edit")

	nvimCmd := NvimCmd(directory)
	_ = tmux.SendKeys(name+":0", nvimCmd)

	_ = tmux.NewWindow(name, 1, "agent", directory)
	// opts.SessionName must be set by the caller before setupFullLayout is
	// invoked — both ensureAndSwitch and restoreProjectSession do this.
	// BuildOpencodeCmd uses it to prefix PRISM_SESSION_NAME for the plugin.
	_ = tmux.SendKeys(name+":1", BuildOpencodeCmd(opts))

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
