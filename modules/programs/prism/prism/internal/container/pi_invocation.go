package container

// pi_invocation.go — helpers for launching PI on the PTY inside bwrap.
//
// When a bwrap session has harness=pi, agent-run must:
//  1. Resolve the single shared PI agent config directory (~/.pi/agent). This
//     directory carries auth.json, settings.json, themes/, AGENTS.md, and
//     skills/ — it no longer carries any role prompt. The role system-prompt
//     is injected at runtime by the prism PI extension
//     (pi/extensions/prism.ts, before_agent_start), which reads
//     ~/.config/prism/agents/<role>.md; the former APPEND_SYSTEM.md file is no
//     longer written (design #2031, PR2 #2033).
//
//     Because every remaining item in this directory is shared/identical
//     across roles and sessions (design #2031, PR3 #2034), there is no
//     per-session staging directory: the host's ~/.pi/agent is mounted
//     directly into the sandbox at the canonical in-sandbox path.
//  2. Bind-mount ~/.pi/agent read-only into the bwrap sandbox at the fixed
//     in-sandbox path (/run/prism/pi-agent) and set PI_CODING_AGENT_DIR to
//     that path. auth.json (and atlassian-mcp-oauth.json) are then re-bound
//     read-write on top so OAuth token refreshes performed inside the sandbox
//     write through to the host files.
//  3. Bind-mount the prism PI extension directory read-only into the bwrap
//     sandbox.
//  4. Invoke PI with the appropriate flags as the sandbox
//     terminator.
//
// This file provides PIInvocation (analogous to HarnessInvocation) and
// SharedPIAgentConfigDir which resolves the shared host config directory.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// piAgentConfigSandboxDefault is the in-sandbox directory path at which
	// the shared PI agent config directory (~/.pi/agent) is bind-mounted when
	// the caller has not overridden PIAgentConfigSandboxDir. PI_CODING_AGENT_DIR
	// is set to this path so PI discovers settings.json / themes / AGENTS.md /
	// skills.
	piAgentConfigSandboxDefault = "/run/prism/pi-agent"

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
//	--session  <cfg.HarnessSessionID>     (when non-empty AND on-disk session
//	                                       file exists — see piResolveResumeSession)
//	<cfg.InitialPrompt>                   (bare positional arg, when non-empty)
//
// The role system prompt is NOT delivered via this config directory. It is
// injected at runtime by the prism PI extension (pi/extensions/prism.ts,
// before_agent_start), which reads ~/.config/prism/agents/<role>.md and
// appends it to PI's default system prompt. The shared directory referenced
// by PI_CODING_AGENT_DIR (~/.pi/agent) only carries settings.json / themes /
// AGENTS.md / skills / auth.json.
//
// Conversation resume (issue #1838): when cfg.HarnessSessionID is non-empty,
// PIInvocation looks up the on-disk session JSONL via
// piResolveResumeSession. If found, --session <id> is appended immediately
// before any positional InitialPrompt arg so pi reopens the prior turns.
// If the file is missing, a warning line is written to the per-session
// agent-run log and pi starts a fresh conversation — restore is
// best-effort and must never fail because of a missing resume target.
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

	// --session <id> for conversation resume (#1838). Skipped silently when
	// HarnessSessionID is empty (fresh session); on missing-file the helper
	// logs a warning and returns ok=false so pi starts a new conversation.
	if cfg.HarnessSessionID != "" {
		if ResolvePIResumeSession(cfg) {
			args = append(args, "--session", cfg.HarnessSessionID)
		}
	}

	if cfg.InitialPrompt != "" {
		args = append(args, cfg.InitialPrompt)
	}

	return args
}

// ResolvePIResumeSession returns true when the on-disk pi session JSONL for
// cfg.HarnessSessionID exists under the mode-aware sessions root and pi can
// be told to resume it. Returns false when the file is missing, in which case
// it also writes a tagged warning line to the per-session agent-run log so
// the operator can see why the conversation didn't resume.
//
// Callers may pass:
//
//   - a fully-populated Config (as PIInvocation does for bwrap and sandbox-exec);
//   - or a minimal Config with just SessionName, Worktree, and HarnessSessionID
//     set (as the host-mode launch path in internal/session does, since it has
//     no container fields).
//
// The mode-aware sessions root mirrors internal/harness/pi/archive.go's
// piSessionsRoot helper (see that file for the authoritative reference). It
// is duplicated here rather than imported because internal/harness/pi already
// imports internal/container, so the reverse dependency would create an
// import cycle. The two implementations MUST stay in sync — if you change
// one, change the other.
//
// Caller contract: HarnessSessionID is non-empty. An empty HarnessSessionID
// must be filtered out upstream (PIInvocation and buildDirectAgentCmd both
// handle that).
func ResolvePIResumeSession(cfg Config) bool {
	return piResolveResumeSession(cfg)
}

func piResolveResumeSession(cfg Config) bool {
	root, ok := piResumeSessionsRoot(cfg)
	if !ok {
		// Couldn't resolve a sessions root — don't append --session, but
		// don't write a warning either: the empty/host-only fallback path
		// isn't an error, it's a no-op.
		return false
	}

	// pi names its session files <timestamp>_<uuid>.jsonl inside an
	// encoded-cwd subdirectory. Worktree determines the encoded-cwd dir.
	// Scan it for an entry ending in _<HarnessSessionID>.jsonl.
	if cfg.Worktree == "" {
		return false
	}
	cwdDir := filepath.Join(root, encodePiCWD(cfg.Worktree))
	suffix := "_" + cfg.HarnessSessionID + ".jsonl"

	entries, err := os.ReadDir(cwdDir)
	if err != nil {
		piLogResumeWarning(cfg, cwdDir, fmt.Sprintf("scan %s: %v", cwdDir, err))
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			return true
		}
	}
	piLogResumeWarning(cfg, cwdDir, fmt.Sprintf("no file matching *%s in %s", suffix, cwdDir))
	return false
}

// piResumeSessionsRoot returns the host-side sessions root directory that pi
// writes session JSONL files under, given the isolation mode implied by cfg.
// The three branches mirror internal/harness/pi/archive.go::piSessionsRoot:
//
//	host         → <home>/.pi/agent/sessions
//	bwrap        → <home>/.pi/agent/sessions  (overlay-mounted into the sandbox
//	                                            at $PI_CODING_AGENT_DIR/sessions;
//	                                            see appendPIBwrapMounts and #1985)
//	sandbox-exec → <stagingHome>/.pi/agent/sessions
//
// Mode is inferred from cfg fields rather than carried explicitly:
//
//   - sandbox-exec is identified by InstanceID being set AND
//     PIAgentConfigSandboxDir == PIAgentConfigHostDir (sandbox-exec shares the
//     host FS, so populatePIConfig deliberately collapses the two paths).
//   - bwrap is identified by SessionName being set AND
//     PIAgentConfigSandboxDir != PIAgentConfigHostDir. As of #1985 bwrap
//     overlays the host's ~/.pi/agent/sessions/ directly under the
//     in-sandbox $PI_CODING_AGENT_DIR, so the host-side resolution is the
//     same as host mode — collapsed into the fallback branch below.
//   - host is the fallback when neither condition matches.
//
// Returns ok=false only when host-mode resolution fails (no home dir).
func piResumeSessionsRoot(cfg Config) (string, bool) {
	// sandbox-exec: per-session staging HOME.
	if cfg.InstanceID != "" && cfg.PIAgentConfigSandboxDir != "" &&
		cfg.PIAgentConfigSandboxDir == cfg.PIAgentConfigHostDir {
		stagingHome, err := SandboxExecStagingHomePath(cfg.InstanceID)
		if err != nil || stagingHome == "" {
			return "", false
		}
		return filepath.Join(stagingHome, ".pi", "agent", "sessions"), true
	}

	// bwrap and host: both resolve to the real home's ~/.pi/agent/sessions/.
	// (Before #1985 bwrap pointed at <XDG_STATE_HOME>/prism/run/<hash>/pi-agent/
	// sessions/; that directory disappeared on `prism cleanup`, taking the
	// history with it. The sessions subtree is now overlay-bound from the
	// host's ~/.pi/agent/sessions/ in appendPIBwrapMounts so writes persist
	// across prism-session lifetimes.)
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".pi", "agent", "sessions"), true
}

// encodePiCWD mirrors internal/harness/pi.EncodePiCWD: pi's session-dir naming
// formula (`--${cwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--`).
//
// Duplicated here — see piResumeSessionsRoot's note on why we cannot import
// internal/harness/pi. The two implementations MUST stay in sync.
func encodePiCWD(cwd string) string {
	stripped := strings.TrimLeft(cwd, "/\\")
	replaced := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(stripped)
	return "--" + replaced + "--"
}

// piLogResumeWarning appends a single tagged warning line to the per-session
// agent-run log. Best-effort: any failure to resolve or open the log path is
// swallowed silently — the warning is purely informational and must never
// take down a bwrap arg-build that would otherwise succeed.
//
// The tag matches the convention used elsewhere in agent-run
// ("[agent-run] warning: …") so operators can grep for resume issues.
func piLogResumeWarning(cfg Config, lookupPath, detail string) {
	if cfg.SessionName == "" {
		return
	}
	logPath, err := piAgentRunLogPath(cfg.SessionName)
	if err != nil {
		return
	}
	if mkErr := os.MkdirAll(filepath.Dir(logPath), 0o700); mkErr != nil {
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f,
		"[agent-run] warning: pi session %s not found at %s — starting fresh conversation (%s)\n",
		cfg.HarnessSessionID, lookupPath, detail,
	)
}

// piAgentRunLogPath is the container-side mirror of
// internal/session.AgentRunLogPath. Duplicated to keep this file free of an
// internal/session dependency (which would in turn pull in db / tmux / … from
// a leaf invocation builder).
func piAgentRunLogPath(sessionName string) (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "prism", "run", sessionDirName(sessionName), "agent-run.log"), nil
}

// SharedPIAgentConfigDir returns the single shared PI agent config directory
// on the host (~/.pi/agent) and the canonical in-sandbox path
// (piAgentConfigSandboxDefault) at which it is mounted.
//
// Design #2031 (PR3 #2034) collapsed the former per-session staging directory
// into this single shared mount. Every item the directory carries —
// auth.json, settings.json, themes/, AGENTS.md, skills/ — is identical across
// all roles and sessions, so there is no longer any need to build, copy into,
// or isolate a per-session directory:
//
//   - bwrap: ~/.pi/agent is bind-mounted read-only at /run/prism/pi-agent
//     (appendPIBwrapMounts), with auth.json / atlassian-mcp-oauth.json
//     re-bound read-write on top so OAuth token refreshes write through to
//     the host. skills/ is a symlink to a Nix-store path, which resolves
//     because /nix is bind-mounted into the sandbox.
//   - sandbox-exec: the host FS is shared, so the host path and the
//     in-sandbox path collapse to the same value (~/.pi/agent). The SBPL
//     read-allow for ~/.pi is already emitted via the staging-HOME symlink
//     target list (collectStagingHomeSymlinkTargets), and auth.json /
//     atlassian-mcp-oauth.json get explicit read-write allows via
//     collectSandboxExecReadWritePaths — see populatePIConfigSandboxExec.
//
// The role system-prompt is NOT carried here: it is injected at runtime by
// the prism PI extension (pi/extensions/prism.ts, before_agent_start) from
// ~/.config/prism/agents/<role>.md (design #2031). A role with no <role>.md
// file simply starts with PI's default prompt — the extension returns no
// override, no error (edge-case AC).
//
// `nh switch` mid-session: because ~/.pi/agent is now mounted live (rather
// than copied into a per-session staging dir), a home-manager switch that
// rewrites settings.json / AGENTS.md / themes while a PI session is running
// is visible to that session on its next read. This is deemed acceptable —
// the content is the user's own config, rarely changes mid-session, and PI
// reads these files at most once per turn — and is the deliberate
// simplification payoff of #2034. auth.json was already a live
// (symlinked/bound) file under the old scheme, so its semantics are
// unchanged.
func SharedPIAgentConfigDir() (hostDir, sandboxDir string, err error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", "", fmt.Errorf("pi: resolve home dir for shared pi-agent config: %w", err)
	}
	return filepath.Join(home, ".pi", "agent"), piAgentConfigSandboxDefault, nil
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
//   - PI agent config directory: cfg.PIAgentConfigHostDir (~/.pi/agent) →
//     cfg.PIAgentConfigSandboxDir (read-only). Sets PI_CODING_AGENT_DIR env
//     var to the sandbox path so PI discovers settings.json / themes /
//     AGENTS.md / skills. auth.json and atlassian-mcp-oauth.json are then
//     re-bound read-write on top so OAuth token refreshes write through.
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
	// The shared host config directory (~/.pi/agent, resolved by
	// SharedPIAgentConfigDir) is bind-mounted READ-ONLY at the in-sandbox path
	// and PI_CODING_AGENT_DIR is set to it so PI discovers settings.json /
	// themes / AGENTS.md / skills. There is no per-session staging directory:
	// every item is shared/identical across roles (design #2031, PR3 #2034).
	// Writable files (auth.json, atlassian-mcp-oauth.json) are re-bound
	// read-write on top of this RO mount below.
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

		// ── PI sessions overlay (read-write, global, #1985) ─────────────────
		// Overlay the host's ~/.pi/agent/sessions/ onto
		// <agentConfigSandboxDir>/sessions/ so pi-written JSONL transcripts
		// land in the user's global per-cwd history directory rather than the
		// per-prism-session staging dir (which disappears on `prism cleanup`).
		//
		// This bind MUST be emitted after the staging-dir bind above so that
		// bwrap applies it as an overlay (later --bind entries take effect on
		// top of earlier ones at the same mount point). The staging dir keeps
		// providing settings.json / themes / AGENTS.md / skills / auth.json;
		// only the `sessions/` subdirectory is redirected.
		//
		// We create the host directory if it does not exist so a fresh install
		// (no prior pi history) does not fail the spawn — bwrap requires the
		// bind source to exist. Best-effort: if MkdirAll fails (e.g. read-only
		// home), we skip the overlay rather than aborting the launch — pi will
		// fall back to writing into the staging dir, matching pre-#1985
		// behaviour for that one session.
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			hostSessionsDir := filepath.Join(home, ".pi", "agent", "sessions")
			if mkErr := os.MkdirAll(hostSessionsDir, 0o700); mkErr == nil {
				sandboxSessionsDir := filepath.Join(agentConfigSandboxDir, "sessions")
				args = append(args, "--bind", hostSessionsDir, sandboxSessionsDir)
			}
		}
	}

	// ── PI auth.json bind-mount (read-write) ─────────────────────────────
	// ~/.pi/agent is bind-mounted READ-ONLY above, so auth.json under the
	// in-sandbox config dir (<agentConfigSandboxDir>/auth.json) would be
	// read-only too. OAuth token refreshes performed inside the session must
	// write through to the host file, so we re-bind the host auth.json
	// READ-WRITE on top of the RO parent at the in-sandbox path that pi reads
	// via PI_CODING_AGENT_DIR. The bind is only emitted when the file exists
	// on the host — when it does not, pi prompts for login instead of
	// crashing. This MUST come after the RO parent bind so bwrap applies it as
	// an overlay (later binds win at the same mount point).
	if home, err := os.UserHomeDir(); err == nil {
		authPath := filepath.Join(home, ".pi", "agent", "auth.json")
		if _, statErr := os.Stat(authPath); statErr == nil {
			sandboxAuthPath := filepath.Join(agentConfigSandboxDir, "auth.json")
			args = append(args, "--bind", authPath, sandboxAuthPath)
		}
	}

	// ── PI atlassian-mcp-oauth.json bind-mount (read-write) ───────────────
	// The Atlassian MCP extension stores OAuth tokens at
	// ~/.pi/agent/atlassian-mcp-oauth.json. Inside a bwrap sandbox,
	// os.UserHomeDir() resolves to the sandbox $HOME, so any token written
	// by /login-atlassian would be lost when the sandbox exits.
	// We touch the file (create if absent, mode 0o600) so that bwrap can
	// bind-mount it. This means the first /login-atlassian inside the sandbox
	// writes directly to the host path and tokens are persisted across
	// sessions. The same pattern is used for auth.json above, except we
	// always create the target here — OAuth logins are the normal first-run
	// path and a missing file would silently drop tokens on the floor.
	if home, err := os.UserHomeDir(); err == nil {
		atlasMCPPath := filepath.Join(home, ".pi", "agent", "atlassian-mcp-oauth.json")
		// Touch the file (create with mode 0o600 if absent). Best-effort:
		// if the parent dir does not exist or the write fails we skip the
		// bind rather than failing the whole container start.
		if _, statErr := os.Stat(atlasMCPPath); os.IsNotExist(statErr) {
			// Ensure parent directory exists before creating the file.
			_ = os.MkdirAll(filepath.Dir(atlasMCPPath), 0o700)
			if f, createErr := os.OpenFile(atlasMCPPath, os.O_CREATE|os.O_EXCL, 0o600); createErr == nil {
				_ = f.Close()
			}
		}
		// Re-bind read-write on top of the RO ~/.pi/agent parent at the
		// in-sandbox path pi reads via PI_CODING_AGENT_DIR (see auth.json above).
		if _, statErr := os.Stat(atlasMCPPath); statErr == nil {
			sandboxAtlasMCPPath := filepath.Join(agentConfigSandboxDir, "atlassian-mcp-oauth.json")
			args = append(args, "--bind", atlasMCPPath, sandboxAtlasMCPPath)
		}
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
