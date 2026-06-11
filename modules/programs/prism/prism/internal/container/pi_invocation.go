package container

// pi_invocation.go — helpers for launching PI on the PTY inside bwrap.
//
// When a bwrap session has harness=pi, agent-run must:
//  1. Bind-mount the user's host ~/.pi/agent directory read-WRITE into the
//     sandbox at the canonical in-sandbox path /run/prism/pi-agent and set
//     PI_CODING_AGENT_DIR to that path. The single shared mount carries
//     settings.json, themes/, AGENTS.md, skills/, auth.json, and
//     atlassian-mcp-oauth.json — all identical across sessions.
//     Per-session staging dirs were eliminated in design #2031 PR3 (#2034)
//     once APPEND_SYSTEM.md was no longer needed (PR2 #2038). The role
//     system-prompt is injected at runtime by the prism PI extension
//     (pi/extensions/prism.ts, before_agent_start), which reads
//     ~/.config/prism/agents/<role>.md.
//
//     RW (not RO) is required for OAuth token refresh: pi-coding-agent uses
//     proper-lockfile with realpath:true, which mkdir's <auth.json>.lock
//     in the PARENT directory to acquire the lock. With an RO parent the
//     lock mkdir EPERMs and refresh silently fails after ~30s of retries.
//     The sandbox-exec SBPL profile has the same RW grant on (subpath
//     ~/.pi/agent), gated on Harness == "pi" (sandbox_exec.go section 6a).
//  2. Bind-mount the prism PI extension directory read-only into the bwrap
//     sandbox.
//  3. Invoke PI with the appropriate flags as the sandbox terminator.
//
// `nh switch` mid-session behaviour: because ~/.pi/agent is now a live
// shared mount (rather than a per-session COPY of settings.json/themes/AGENTS.md),
// changes to those files on the host become visible to a running PI session
// without requiring a respawn. This is the design call locked in #2034 —
// the content is the user's own config and changing rarely mid-session, and
// the simplification payoff is significant.
//
// This file provides PIInvocation (analogous to HarnessInvocation) and
// EnsurePIAgentConfigDir which prepares the shared host directory.

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
	// the shared PI agent config directory (~/.pi/agent on the host) is
	// bind-mounted when the caller has not overridden PIAgentConfigSandboxDir.
	// PI_CODING_AGENT_DIR is set to this path so PI discovers settings.json /
	// themes / AGENTS.md / skills.
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
//	--agent    <cfg.AgentRole>            (when non-empty; consumed by the
//	                                       prism PI extension via
//	                                       pi.registerFlag("agent") to source
//	                                       the role system prompt at
//	                                       before_agent_start — issue #2064)
//	--session  <cfg.HarnessSessionID>     (when non-empty AND on-disk session
//	                                       file exists — see piResolveResumeSession)
//	<cfg.InitialPrompt>                   (bare positional arg, when non-empty)
//
// The role system prompt is NOT delivered via this staging directory. It is
// injected at runtime by the prism PI extension (pi/extensions/prism.ts,
// before_agent_start), which reads ~/.config/prism/agents/<role>.md and
// appends it to PI's default system prompt. The staging directory referenced
// by PI_CODING_AGENT_DIR only carries settings.json / themes / AGENTS.md /
// skills / auth.json.
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

	// --agent <role> for the prism PI extension's role-prompt injection
	// (issue #2064). pi has no native concept of agents — the extension
	// registers --agent via pi.registerFlag at factory entry and reads the
	// bound value synchronously in its before_agent_start handler. Skipping
	// this flag when AgentRole is empty matches the edge-case AC: a session
	// with no role file falls back to pi's base system prompt unchanged.
	if cfg.AgentRole != "" {
		args = append(args, "--agent", cfg.AgentRole)
	}

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
// cfg.HarnessSessionID exists under the host sessions root and pi can
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
// The sessions root mirrors internal/harness/pi/archive.go's
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
// writes session JSONL files under. Every isolation mode resolves to the SAME
// host root — $PI_CODING_AGENT_DIR/sessions when the env var is set, else
// <home>/.pi/agent/sessions — mirroring
// internal/harness/pi/archive.go::piSessionsRoot:
//
//	host         → pi runs against the host environment directly.
//	bwrap        → the host's PI sessions root is overlay-mounted into the
//	               sandbox at $PI_CODING_AGENT_DIR/sessions (see
//	               appendPIBwrapMounts and #1985).
//	sandbox-exec → the dispatcher injects PI_CODING_AGENT_DIR=<host
//	               ~/.pi/agent> into the sandbox env and sandbox-exec shares
//	               the host filesystem (cmd/agent_run_sandbox_exec_darwin.go),
//	               so pi writes to the host root there too. A pre-#2210
//	               branch here resolved sandbox-exec to the per-session
//	               staging HOME (<stagingHome>/.pi/agent/sessions); that
//	               formula had been stale since #1286 and meant resume never
//	               found the transcript (issue #2210).
//
// PI_CODING_AGENT_DIR mirrors pi's own ENV_AGENT_DIR honouring (pi 0.79
// dist/core/session-manager.js getDefaultAgentDir / getDefaultSessionDirPath
// — see internal/harness/pi/archive.go for the full citation). The prism
// developer host sets it system-wide to /run/prism/pi-agent.
//
// The Config parameter is retained for signature stability with callers and
// the archive-side mirror; resolution no longer depends on any cfg field.
//
// Returns ok=false only when host resolution fails (no home dir, and
// PI_CODING_AGENT_DIR is unset).
func piResumeSessionsRoot(_ Config) (string, bool) {
	return piResumeHostSessionsRoot()
}

// piResumeHostSessionsRoot returns the host-side PI sessions directory:
// $PI_CODING_AGENT_DIR/sessions when the env var is set (non-empty), else
// <UserHomeDir>/.pi/agent/sessions. Mirrors pi 0.79's data-root resolution
// (ENV_AGENT_DIR honouring with a ~/.pi/agent/ fallback).
//
// Duplicated from internal/harness/pi.hostPISessionsRoot — see the note on
// hostPISessionsRoot for why we cannot share a single helper. The two
// implementations MUST stay in sync.
func piResumeHostSessionsRoot() (string, bool) {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions"), true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".pi", "agent", "sessions"), true
}

// RemovePiResumeJSONL deletes any pi session-transcript JSONL file under the
// resolved sessions root whose basename ends in `_<HarnessSessionID>.jsonl`,
// for the encoded-cwd subdirectory derived from cfg.Worktree.
//
// This is the FS-side companion to db.ClearHarnessSessionID: together they
// sever the pi resume linkage so that re-spawning a NEW session on the SAME
// branch name does not resume the cleaned session's pi conversation
// (issue #2035). DB-side severance alone is sufficient for the bug —
// PIInvocation only appends `--session <id>` when HarnessSessionID is
// non-empty AND ResolvePIResumeSession finds a matching JSONL — but
// removing the on-disk transcript closes the second path explicitly and
// keeps ~/.pi/agent/sessions/ from accumulating dead conversations across
// reused branch names.
//
// Resolution mirrors piResumeSessionsRoot: every isolation mode (host,
// bwrap, sandbox-exec) resolves to the host sessions root —
// $PI_CODING_AGENT_DIR/sessions when set, else `~/.pi/agent/sessions/`.
// For sandbox-exec this removal is load-bearing, not redundant: pi writes
// its transcripts to the host root (the dispatcher injects
// PI_CODING_AGENT_DIR into the sandbox env), so the staging-HOME wipe in
// RemoveSandboxExecStagingHome never touches them (issue #2210).
//
// Best-effort and non-fatal:
//
//   - Returns nil silently when HarnessSessionID or Worktree is empty (caller
//     contract: nothing to scope to).
//   - Returns nil silently when the sessions root cannot be resolved
//     (e.g. no home dir on the host) — cleanup must still succeed.
//   - Returns nil silently when the encoded-cwd directory does not exist
//     (fresh-session-then-cleanup, sandbox already torn down, etc.).
//   - An error is returned only when a matching file was found but
//     os.Remove failed for some reason other than "not exist" — the caller
//     may log and continue.
//
// The function deliberately scopes by `_<HarnessSessionID>.jsonl` suffix
// rather than wiping the whole encoded-cwd dir: other sibling sessions on
// the same worktree path (e.g. the legitimate-resume case in #1838 where a
// still-active session is being restarted) must not be touched.
func RemovePiResumeJSONL(cfg Config) error {
	_, err := RemovePiResumeJSONLCount(cfg)
	return err
}

// RemovePiResumeJSONLCount is RemovePiResumeJSONL additionally reporting the
// number of transcript files actually removed. The error semantics, scoping
// and best-effort contract are identical to RemovePiResumeJSONL (which is a
// thin wrapper over this function) — see its doc comment for the full
// reference.
//
// The count exists for callers that aggregate removals across many sessions
// and want honest summary output: `prism reset` (issue #2220) snapshots every
// (worktree, harness_session_id) pair being reset and removes exactly those
// transcripts from the shared host sessions root, reporting the total. The
// per-session cleanup paths keep using the error-only wrapper.
func RemovePiResumeJSONLCount(cfg Config) (int, error) {
	if cfg.HarnessSessionID == "" || cfg.Worktree == "" {
		return 0, nil
	}
	root, ok := piResumeSessionsRoot(cfg)
	if !ok {
		return 0, nil
	}
	cwdDir := filepath.Join(root, encodePiCWD(cfg.Worktree))
	entries, err := os.ReadDir(cwdDir)
	if err != nil {
		// Missing dir / unreadable — nothing to do. Cleanup is best-effort.
		return 0, nil
	}
	suffix := "_" + cfg.HarnessSessionID + ".jsonl"
	removed := 0
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		path := filepath.Join(cwdDir, e.Name())
		if rmErr := os.Remove(path); rmErr != nil {
			if !os.IsNotExist(rmErr) && firstErr == nil {
				firstErr = fmt.Errorf("remove pi resume jsonl %s: %w", path, rmErr)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
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

// EnsurePIAgentConfigDir returns the host path to the shared PI agent config
// directory (~/.pi/agent) and the canonical in-sandbox path
// (piAgentConfigSandboxDefault). It creates the host directory if it does not
// yet exist so bwrap has a valid bind source on a fresh install.
//
// Design #2031, PR3 (#2034): the per-session staging directory previously
// built by StagePIAgentConfigDir has been collapsed into a single shared
// read-WRITE mount of ~/.pi/agent at /run/prism/pi-agent. Every session of
// every role mounts the same host directory — settings.json, themes/,
// AGENTS.md, skills/, and auth.json are all shared/identical, so there is
// nothing per-session left to stage.
//
// The parent mount is RW (not RO) because pi-coding-agent's OAuth token
// refresh uses proper-lockfile with realpath:true, which mkdir's
// <auth.json>.lock on the PARENT directory to acquire the lock. An RO
// parent would EPERM the lock mkdir and silently fail refresh after ~30s
// of retries. See top-of-file doc and appendPIBwrapMounts for the full
// rationale; the sandbox-exec SBPL profile satisfies the same constraint
// via (allow file-read* file-write* … (subpath ~/.pi/agent)) gated on
// Harness == "pi" (sandbox_exec.go section 6a).
//
// The role system-prompt is injected at runtime by the prism PI extension
// (pi/extensions/prism.ts, before_agent_start) from
// ~/.config/prism/agents/<role>.md (design #2031, PR1 #2037 + PR2 #2038).
// A role with no <role>.md file simply starts with PI's default prompt — the
// extension returns no override, no error (edge-case AC).
func EnsurePIAgentConfigDir() (hostDir, sandboxDir string, err error) {
	home, hErr := os.UserHomeDir()
	if hErr != nil || home == "" {
		return "", "", fmt.Errorf("pi: resolve user home: %w", hErr)
	}
	hostDir = filepath.Join(home, ".pi", "agent")
	// Create the host dir if absent so bwrap has a valid bind source on a
	// fresh install. Mode 0o700 mirrors what StagePIAgentConfigDir formerly
	// did for the per-session staging dir.
	if mkErr := os.MkdirAll(hostDir, 0o700); mkErr != nil {
		return "", "", fmt.Errorf("pi: create shared agent dir %s: %w", hostDir, mkErr)
	}
	return hostDir, piAgentConfigSandboxDefault, nil
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

// PIExtensionHostPath returns the host-side absolute path to the prism PI
// extension file given the extension directory (i.e. cfg.PIExtensionDir as
// populated from prism's config.json by Nix). Returns "" when hostDir is
// empty so the host-mode launch path can fall back to a no-flag invocation
// rather than emit a stray --extension argument with no value.
//
// Used by the host-mode pi launch in internal/session/session.go to close
// the gap fixed by #2065 — host-mode previously emitted `pi --agent worker`
// with no --extension, so the prism extension never loaded and role-prompt
// injection (plus the sidecar bridge, status bar, doom-loop guard, etc.)
// silently no-op'd. The container path already appends --extension via
// PIInvocation above.
func PIExtensionHostPath(hostDir string) string {
	if hostDir == "" {
		return ""
	}
	return filepath.Join(hostDir, piExtensionFilename)
}

// appendPIBwrapMounts appends the bwrap bind-mount args needed for a PI
// session to args and returns the extended slice. Specifically:
//
//   - PI agent config directory: cfg.PIAgentConfigHostDir → cfg.PIAgentConfigSandboxDir
//     (read-WRITE — see OAuth proper-lockfile rationale in the in-function
//     comment block below and at the top of this file). Sets
//     PI_CODING_AGENT_DIR env var to the sandbox path so PI discovers
//     settings.json / themes / AGENTS.md / skills / auth.json.
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
	// Bind-mount the shared host ~/.pi/agent directory READ-WRITE at the
	// canonical in-sandbox path and set PI_CODING_AGENT_DIR to it so PI
	// discovers settings.json / themes / AGENTS.md / skills / auth.json.
	//
	// Why RW (not RO):
	//
	//   - OAuth token refresh inside pi-coding-agent uses proper-lockfile
	//     with realpath:true. After resolving auth.json's realpath, the
	//     library calls mkdir(<resolved-auth-path>.lock) on the PARENT
	//     directory to acquire the lock. The parent dir must therefore be
	//     writable, not just auth.json itself. This is the same constraint
	//     the sandbox-exec SBPL profile satisfies via
	//     (allow file-read* file-write* ... (subpath ~/.pi/agent)) gated
	//     on Harness == "pi" (see sandbox_exec.go section 6a).
	//
	//   - The directory is the user's own config dir. There is no isolation
	//     gain in making it RO from the sandbox; the agent already has full
	//     RW access to the worktree.
	//
	// Design #2031 PR3 (#2034) collapsed the former per-session staging dir
	// into this single shared mount. See pi_invocation.go top-of-file doc
	// comment for the `nh switch` mid-session implication.
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
		args = append(args, "--bind", agentConfigHostDir, agentConfigSandboxDir)
		args = append(args, "--setenv", "PI_CODING_AGENT_DIR", agentConfigSandboxDir)

		// ── PI sessions overlay (read-write, global, #1985) ─────────────────
		// Ensure the host's ~/.pi/agent/sessions/ exists so pi can write its
		// per-cwd JSONL transcripts there. Pre-#2034 this needed a dedicated
		// --bind overlay because the parent mount was a per-session staging
		// dir that did not contain a sessions/ subdir. Post-#2034 the parent
		// mount IS ~/.pi/agent (RW), so writes to $PI_CODING_AGENT_DIR/
		// sessions/ already land on the host via the same bind — we just need
		// to make sure the subdirectory exists on the host before pi starts.
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			hostSessionsDir := filepath.Join(home, ".pi", "agent", "sessions")
			_ = os.MkdirAll(hostSessionsDir, 0o700)
		}
	}

	// ── PI auth.json bind-mount (read-write, host path) ─────────────────
	// The parent mount above already exposes auth.json RW at
	// $PI_CODING_AGENT_DIR/auth.json (which is what PI reads). Retain the
	// host-path RW bind so any code path that resolves $HOME/.pi/agent/
	// auth.json directly inside the sandbox (e.g. via os.UserHomeDir()) also
	// works. The bind is only emitted when the file exists on the host —
	// when it does not, pi prompts for login instead of crashing.
	if home, err := os.UserHomeDir(); err == nil {
		authPath := filepath.Join(home, ".pi", "agent", "auth.json")
		if _, statErr := os.Stat(authPath); statErr == nil {
			args = append(args, "--bind", authPath, authPath)
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
		// Host-path bind retained for callers that resolve via $HOME directly.
		// The in-sandbox shared mount above already exposes this file RW at
		// $PI_CODING_AGENT_DIR/atlassian-mcp-oauth.json.
		if _, statErr := os.Stat(atlasMCPPath); statErr == nil {
			args = append(args, "--bind", atlasMCPPath, atlasMCPPath)
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
