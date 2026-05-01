package container

// pi_invocation.go — helpers for launching PI on the PTY inside bwrap.
//
// When a bwrap session has harness=pi, agent-run must:
//  1. Generate a per-session system-prompt file from the active profile's slot
//     for the session's role.
//  2. Bind-mount that file read-only into the bwrap sandbox.
//  3. Bind-mount the prism PI extension directory read-only into the bwrap
//     sandbox.
//  4. Invoke PI (not opencode) with the appropriate flags as the sandbox
//     terminator.
//
// This file provides PIInvocation (analogous to HarnessInvocation) and
// WriteSystemPromptFile which writes the content read from the profile slot's
// SystemPromptPath to a stable per-session temp file.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/config"
)

const (
	// piSystemPromptSandboxDefault is the in-sandbox path at which the
	// system-prompt file is bind-mounted when the caller has not overridden
	// PISystemPromptSandboxPath.
	piSystemPromptSandboxDefault = "/tmp/prism-system-prompt.md"

	// piExtensionSandboxDefault is the in-sandbox directory path at which
	// the PI extension directory is bind-mounted when the caller has not
	// overridden PIExtensionSandboxDir.
	piExtensionSandboxDefault = "/etc/prism/pi-extensions"

	// piExtensionFilename is the basename of the prism PI extension file
	// inside the extension directory.
	piExtensionFilename = "prism.ts"

	// piSystemPromptFileName is the filename written inside the per-session
	// run directory for the system-prompt temp file.
	piSystemPromptFileName = "system-prompt.md"
)

// PIInvocation returns the trailing arg slice that launches PI with the
// profile-derived flags. It is the PI analogue of HarnessInvocation.
//
// The returned slice begins with "pi" and includes:
//
//	--provider <cfg.PIProvider>           (when non-empty)
//	--model    <cfg.PIModel>              (when non-empty)
//	--thinking <cfg.PIThinking>           (when non-empty)
//	--append-system-prompt <sandboxPath>  (always; sandboxPath is derived from cfg)
//	--extension <extensionPath>           (always; path is derived from cfg)
//	--no-session                          (prism manages session state)
//	--prompt   <cfg.InitialPrompt>        (when non-empty)
//
// sandboxPath for the system prompt and the extension path are derived from
// the Config fields with the package-level defaults as fallback.
func PIInvocation(cfg Config) []string {
	args := []string{"pi"}

	if cfg.PIProvider != "" {
		args = append(args, "--provider", cfg.PIProvider)
	}
	if cfg.PIModel != "" {
		args = append(args, "--model", cfg.PIModel)
	}
	if cfg.PIThinking != "" {
		args = append(args, "--thinking", cfg.PIThinking)
	}

	// System-prompt path inside the sandbox.
	systemPromptSandboxPath := cfg.PISystemPromptSandboxPath
	if systemPromptSandboxPath == "" {
		systemPromptSandboxPath = piSystemPromptSandboxDefault
	}
	args = append(args, "--append-system-prompt", systemPromptSandboxPath)

	// Extension path inside the sandbox (directory + filename).
	extensionSandboxDir := cfg.PIExtensionSandboxDir
	if extensionSandboxDir == "" {
		extensionSandboxDir = piExtensionSandboxDefault
	}
	extensionSandboxPath := filepath.Join(extensionSandboxDir, piExtensionFilename)
	args = append(args, "--extension", extensionSandboxPath)

	// PI manages its own session continuity; prism owns session state.
	args = append(args, "--no-session")

	if cfg.InitialPrompt != "" {
		args = append(args, "--prompt", cfg.InitialPrompt)
	}

	return args
}

// WriteSystemPromptFile generates the per-session system-prompt temp file from
// the profile slot for the session's role and returns the host path and the
// canonical in-sandbox path.
//
// The file is written to:
//
//	~/.local/state/prism/run/<sessionDirName>/system-prompt.md
//
// which is co-located with agent-run.log and hostapi.sock. The file content
// is read from slot.SystemPromptPath on the host filesystem.
//
// Error conditions:
//   - slot.SystemPromptPath is empty → clear error (edge-case AC)
//   - slot.SystemPromptPath file cannot be read → clear error
//   - per-session run directory cannot be created → error
//   - temp file cannot be written → error
//
// The in-sandbox path is always piSystemPromptSandboxDefault
// (/tmp/prism-system-prompt.md).
func WriteSystemPromptFile(slot config.RoleSlot, sessionName string) (hostPath, sandboxPath string, err error) {
	if slot.SystemPromptPath == "" {
		return "", "", fmt.Errorf(
			"pi: session %q: profile slot for this role has no systemPromptPath — "+
				"set systemPromptPath in the profile's slot for role %q",
			sessionName, "this role",
		)
	}

	// Read the system-prompt source file.
	content, err := os.ReadFile(slot.SystemPromptPath)
	if err != nil {
		return "", "", fmt.Errorf(
			"pi: session %q: read systemPromptPath %q: %w",
			sessionName, slot.SystemPromptPath, err,
		)
	}

	// Resolve the per-session run directory.
	runDir, err := sessionRunDir(sessionName)
	if err != nil {
		return "", "", fmt.Errorf("pi: session %q: resolve run dir: %w", sessionName, err)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return "", "", fmt.Errorf("pi: session %q: create run dir %s: %w", sessionName, runDir, err)
	}

	dest := filepath.Join(runDir, piSystemPromptFileName)
	if err := os.WriteFile(dest, content, 0o600); err != nil {
		return "", "", fmt.Errorf("pi: session %q: write system prompt file %s: %w", sessionName, dest, err)
	}

	return dest, piSystemPromptSandboxDefault, nil
}

// ValidatePIExtensionDir checks that the PI extension directory exists and
// contains the expected prism.ts file. Called at spawn time so that a missing
// extension fails early with a clear error rather than silently launching PI
// without it.
func ValidatePIExtensionDir(hostDir string) error {
	if hostDir == "" {
		return fmt.Errorf("pi: extension host directory is empty — set PIExtensionHostDir in the container config")
	}
	extPath := filepath.Join(hostDir, piExtensionFilename)
	if _, err := os.Stat(extPath); err != nil {
		return fmt.Errorf("pi: extension file %q is missing or unreadable: %w — "+
			"ensure the prism PI extension (P2.EXTENSION) is built and the path is correct",
			extPath, err)
	}
	return nil
}

// piHarnessPipePath returns the per-session pipe socket path
// (~/.local/state/prism/run/<sessionDirName>/pipe.sock).
//
// This is the path the PI extension connects to for the sidecar wire protocol
// (P2.SIDECAR). It is inlined here (mirroring sessionRunDir) to avoid a
// circular import with internal/session.
func piHarnessPipePath(sessionName string) string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// Fallback: use /tmp — better than an empty string.
			return "/tmp/prism-pipe-" + sessionName + ".sock"
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(sessionName))
	dirName := hex.EncodeToString(sum[:])[:12]
	return filepath.Join(stateHome, "prism", "run", dirName, "pipe.sock")
}

// sessionRunDir returns the per-session run directory path
// (~/.local/state/prism/run/<sessionDirName>/).
//
// The path uses the same XDG_STATE_HOME derivation and SHA256 session-dir
// naming convention as session.AgentRunLogPath — inlined here to avoid a
// circular import (internal/session imports internal/container).
func sessionRunDir(sessionName string) (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(sessionName))
	dirName := hex.EncodeToString(sum[:])[:12] // matches session.sessionDirHashLen
	return filepath.Join(stateHome, "prism", "run", dirName), nil
}

// PIExtensionSandboxPath returns the in-sandbox absolute path to the prism PI
// extension file, given the sandbox directory override (or the package default
// when override is empty).
func PIExtensionSandboxPath(sandboxDirOverride string) string {
	dir := sandboxDirOverride
	if dir == "" {
		dir = piExtensionSandboxDefault
	}
	return filepath.Join(dir, piExtensionFilename)
}

// appendPIBwrapMounts appends the bwrap bind-mount args needed for a PI
// session to args and returns the extended slice. Specifically:
//
//   - System-prompt file: cfg.PISystemPromptHostPath → cfg.PISystemPromptSandboxPath
//     (read-only). Also ensures the /tmp tmpfs is shadowed away below the
//     sandbox-level tmpfs already established by baseline --tmpfs /tmp.
//   - Extension directory: cfg.PIExtensionHostDir → cfg.PIExtensionSandboxDir
//     (read-only).
//
// Returns an error when either host path is empty (indicating the caller
// forgot to populate the Config fields) or the extension directory cannot be
// validated.
func appendPIBwrapMounts(args []string, cfg Config) ([]string, error) {
	// ── System-prompt file ───────────────────────────────────────────────────
	if cfg.PISystemPromptHostPath == "" {
		return nil, fmt.Errorf("pi: PISystemPromptHostPath is empty — WriteSystemPromptFile must be called before BuildArgs")
	}
	sandboxPromptPath := cfg.PISystemPromptSandboxPath
	if sandboxPromptPath == "" {
		sandboxPromptPath = piSystemPromptSandboxDefault
	}
	// The sandbox already has a tmpfs on /tmp from the baseline args; a file
	// bind-mount inside /tmp is allowed on top of it.
	args = append(args, "--ro-bind", cfg.PISystemPromptHostPath, sandboxPromptPath)

	// ── Extension directory ──────────────────────────────────────────────────
	if cfg.PIExtensionHostDir == "" {
		return nil, fmt.Errorf("pi: PIExtensionHostDir is empty — set the extension directory in the container config")
	}
	if err := ValidatePIExtensionDir(cfg.PIExtensionHostDir); err != nil {
		return nil, err
	}
	sandboxExtDir := cfg.PIExtensionSandboxDir
	if sandboxExtDir == "" {
		sandboxExtDir = piExtensionSandboxDefault
	}
	// The parent directory (/etc/prism when using the default) may not exist
	// inside the sandbox. /etc is ro-bind-mounted from the host, but
	// /etc/prism is a prism-specific directory that does not exist on the
	// host, so it will not appear inside the sandbox either. bwrap requires
	// the mount-point parent to exist before the bind mount is applied, so
	// we create both the parent and the target with --dir unconditionally.
	parent := filepath.Dir(sandboxExtDir)
	args = append(args, "--dir", parent)
	args = append(args, "--dir", sandboxExtDir)
	args = append(args, "--ro-bind", cfg.PIExtensionHostDir, sandboxExtDir)

	return args, nil
}
