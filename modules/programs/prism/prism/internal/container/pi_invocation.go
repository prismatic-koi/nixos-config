package container

// pi_invocation.go — helpers for launching PI on the PTY inside bwrap.
//
// When a bwrap session has harness=pi, agent-run must:
//  1. Stage a per-session PI agent config directory on the host before bwrap
//     launches. If a system prompt is configured for the session's role, write
//     APPEND_SYSTEM.md into that directory. PI discovers APPEND_SYSTEM.md
//     automatically via the PI_CODING_AGENT_DIR environment variable — no
//     --append-system-prompt CLI flag is needed.
//  2. Bind-mount the staging directory read-only into the bwrap sandbox at a
//     fixed in-sandbox path and set PI_CODING_AGENT_DIR to that path.
//  3. Bind-mount the prism PI extension directory read-only into the bwrap
//     sandbox.
//  4. Invoke PI (not opencode) with the appropriate flags as the sandbox
//     terminator.
//
// This file provides PIInvocation (analogous to HarnessInvocation) and
// StagePIAgentConfigDir which prepares the per-session staging directory and
// optionally writes APPEND_SYSTEM.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/config"
)

const (
	// piAgentConfigSandboxDefault is the in-sandbox directory path at which
	// the per-session PI agent config directory is bind-mounted when the caller
	// has not overridden PIAgentConfigSandboxDir. PI_CODING_AGENT_DIR is set to
	// this path so PI discovers APPEND_SYSTEM.md automatically.
	piAgentConfigSandboxDefault = "/run/prism/pi-agent"

	// piAgentConfigSubdir is the subdirectory name created under the per-session
	// run directory to hold the PI agent config staging files.
	piAgentConfigSubdir = "pi-agent"

	// piAppendSystemFilename is the filename written into the PI agent config
	// staging directory to append a role prompt to PI's default system prompt.
	// PI discovers this file automatically when PI_CODING_AGENT_DIR is set.
	piAppendSystemFilename = "APPEND_SYSTEM.md"

	// piExtensionSandboxDefault is the in-sandbox directory path at which
	// the PI extension directory is bind-mounted when the caller has not
	// overridden PIExtensionSandboxDir.
	piExtensionSandboxDefault = "/etc/prism/pi-extensions"

	// piExtensionFilename is the basename of the prism PI extension file
	// inside the extension directory.
	piExtensionFilename = "prism.ts"
)

// PIInvocation returns the trailing arg slice that launches PI with the
// profile-derived flags. It is the PI analogue of HarnessInvocation.
//
// The returned slice begins with "pi" and includes:
//
//	--provider <cfg.PIProvider>           (when non-empty)
//	--model    <cfg.PIModel>              (when non-empty)
//	--thinking <cfg.PIThinking>           (when non-empty)
//	--extension <extensionPath>           (always; path is derived from cfg)
//	--no-session                          (prism manages session state)
//	<cfg.InitialPrompt>                   (bare positional arg, when non-empty)
//
// The system prompt is delivered via PI_CODING_AGENT_DIR (set in the bwrap
// environment) pointing at the per-session staging directory that contains
// APPEND_SYSTEM.md. No --append-system-prompt flag is needed — PI discovers
// APPEND_SYSTEM.md automatically from its agent config directory.
func PIInvocation(cfg Config) []string {
	// Use the resolved binary path when set; fall back to the bare name for
	// back-compat in test/host-mode contexts where the binary is on PATH.
	binary := cfg.PIBinaryPath
	if binary == "" {
		binary = "pi"
	}
	args := []string{binary}

	if cfg.PIProvider != "" {
		args = append(args, "--provider", cfg.PIProvider)
	}
	if cfg.PIModel != "" {
		args = append(args, "--model", cfg.PIModel)
	}
	if cfg.PIThinking != "" {
		args = append(args, "--thinking", cfg.PIThinking)
	}

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
		args = append(args, cfg.InitialPrompt)
	}

	return args
}

// StagePIAgentConfigDir prepares the per-session PI agent config staging
// directory and, when a system prompt is configured, writes APPEND_SYSTEM.md
// into it. It returns the host path to the staging directory and the canonical
// in-sandbox path (piAgentConfigSandboxDefault).
//
// The staging directory is created at:
//
//	~/.local/state/prism/run/<sessionDirHash>/pi-agent/
//
// which is a subdirectory of the per-session run directory (co-located with
// agent-run.log and hostapi.sock). This path falls under the existing SBPL
// run-dir (subpath ...) rule on Darwin — no additional sandbox-exec profile
// rule is required (confirmed issue #1285).
//
// If slot.SystemPromptPath is empty or the file does not exist, the staging
// directory is still created but APPEND_SYSTEM.md is omitted — PI will start
// without a role prompt rather than erroring (edge-case AC).
//
// The staging directory is isolated per session (via the sessionDirHash), so
// two concurrent spawns for different roles never share a staging dir.
func StagePIAgentConfigDir(slot config.RoleSlot, sessionName string) (hostDir, sandboxDir string, err error) {
	runDir, err := sessionRunDir(sessionName)
	if err != nil {
		return "", "", fmt.Errorf("pi: session %q: resolve run dir: %w", sessionName, err)
	}

	stagingDir := filepath.Join(runDir, piAgentConfigSubdir)
	if mkErr := os.MkdirAll(stagingDir, 0o700); mkErr != nil {
		return "", "", fmt.Errorf("pi: session %q: create pi-agent staging dir %s: %w", sessionName, stagingDir, mkErr)
	}

	// Write APPEND_SYSTEM.md only when a system prompt path is configured and
	// the file exists. Missing or empty SystemPromptPath is silently skipped —
	// PI starts without the role prompt rather than failing.
	if slot.SystemPromptPath != "" {
		content, readErr := os.ReadFile(slot.SystemPromptPath)
		if readErr != nil {
			// Non-fatal: log a warning and skip writing APPEND_SYSTEM.md.
			// The staging dir is still returned so PI gets PI_CODING_AGENT_DIR.
			_ = readErr // PI will start without the role prompt.
		} else {
			dest := filepath.Join(stagingDir, piAppendSystemFilename)
			if writeErr := os.WriteFile(dest, content, 0o600); writeErr != nil {
				return "", "", fmt.Errorf("pi: session %q: write %s: %w", sessionName, dest, writeErr)
			}
		}
	}

	return stagingDir, piAgentConfigSandboxDefault, nil
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
//   - PI agent config directory: cfg.PIAgentConfigHostDir → cfg.PIAgentConfigSandboxDir
//     (read-only). Sets PI_CODING_AGENT_DIR env var to the sandbox path so PI
//     discovers APPEND_SYSTEM.md automatically.
//   - Extension directory: cfg.PIExtensionHostDir → cfg.PIExtensionSandboxDir
//     (read-only).
//
// Returns an error when the extension host path is empty (indicating the caller
// forgot to populate the Config fields) or the extension directory cannot be
// validated.
func appendPIBwrapMounts(args []string, cfg Config) ([]string, error) {
	// ── PI binary (read-only) ────────────────────────────────────────────────
	// The pi binary lives in the Nix store and is not reachable
	// inside the bwrap sandbox purely via PATH (/nix is bind-mounted but the
	// profile symlink farm may not resolve the specific store path the binary
	// lives at when the profile is not in scope). Bind-mounting the resolved
	// absolute path guarantees the binary is accessible at that exact path
	// inside the sandbox, which is the value PIInvocation uses as argv[0].
	if cfg.PIBinaryPath == "" {
		return nil, fmt.Errorf("pi: PIBinaryPath is empty — resolve pi before launching bwrap")
	}
	args = append(args, "--ro-bind", cfg.PIBinaryPath, cfg.PIBinaryPath)

	// ── PI agent config directory ────────────────────────────────────────────
	// The staging directory is always created by StagePIAgentConfigDir before
	// bwrap launches. Bind-mount it read-only and set PI_CODING_AGENT_DIR to
	// the in-sandbox path so PI discovers APPEND_SYSTEM.md automatically.
	agentConfigHostDir := cfg.PIAgentConfigHostDir
	agentConfigSandboxDir := cfg.PIAgentConfigSandboxDir
	if agentConfigSandboxDir == "" {
		agentConfigSandboxDir = piAgentConfigSandboxDefault
	}
	if agentConfigHostDir != "" {
		// Ensure the in-sandbox mount-point parent exists (bwrap requires the
		// parent of the bind-mount target to exist in the sandbox namespace).
		parent := filepath.Dir(agentConfigSandboxDir)
		args = append(args, "--dir", parent)
		args = append(args, "--dir", agentConfigSandboxDir)
		args = append(args, "--ro-bind", agentConfigHostDir, agentConfigSandboxDir)
		args = append(args, "--setenv", "PI_CODING_AGENT_DIR", agentConfigSandboxDir)
	}

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
