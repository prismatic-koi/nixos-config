package container

// pi_invocation.go — helpers for launching PI on the PTY inside bwrap.
//
// When a bwrap session has harness=pi, agent-run must:
//  1. Make the pi agent dir at the canonical in-sandbox path
//     /run/prism/pi-agent and set PI_CODING_AGENT_DIR to that path. The dir
//     is private to the sandbox. It holds read-only binds of the entries of
//     the host ~/.pi/agent (settings.json, themes/, AGENTS.md, skills/, and
//     more), read-write binds of auth.json and atlassian-mcp-oauth.json,
//     and a read-write bind of the sessions dir of the worktree. See
//     pi_agent_dir.go for the write list and the reasons (#3284). Bind the
//     shared lock dir and the Notion store dir read-write at their host
//     paths (pi_shared_state.go, #3289, #3290).
//     The role system-prompt is injected at runtime by the prism PI extension
//     (pi/extensions/prism.ts, before_agent_start), which reads
//     ~/.config/prism/agents/<role>.md.
//  2. Bind-mount the prism PI extension directory read-only into the bwrap
//     sandbox.
//  3. Invoke PI with the appropriate flags as the sandbox terminator.
//
// `nh switch` mid-session behaviour: the read-only binds hold the files
// that the host entries named when the sandbox started. A change on the host
// becomes visible to a pi session after a respawn.
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

	"github.com/prismatic-koi/prism/internal/config"
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

// PIOverrides carries the per-spawn overrides for the pi model axes. An
// empty field means "no override". AgentModel is the per-role
// `--model-override` entry and outranks Model, the session-wide `--model`.
type PIOverrides struct {
	Provider   string
	Model      string
	AgentModel string
	Variant    string
}

// PIModelAxes is the resolved provider, model, and thinking level for pi.
type PIModelAxes struct {
	Provider string
	Model    string
	Thinking string
}

// ResolvePIModelAxes applies the overrides to a profile slot. It is the
// single place that decides the provider, model, and thinking level for
// every isolation mode: populatePIConfig (bwrap, sandbox-exec) and the
// host-mode command builder both call it. Precedence for the model:
// AgentModel, then Model, then slot.Model.
func ResolvePIModelAxes(slot config.RoleSlot, ov PIOverrides) PIModelAxes {
	axes := PIModelAxes{Provider: slot.Provider, Model: slot.Model, Thinking: slot.Thinking}
	if ov.Provider != "" {
		axes.Provider = ov.Provider
	}
	if ov.Model != "" {
		axes.Model = ov.Model
	}
	if ov.AgentModel != "" {
		axes.Model = ov.AgentModel
	}
	if ov.Variant != "" {
		axes.Thinking = ov.Variant
	}
	return axes
}

// PIModelFlags renders the --provider, --model, and --thinking flags, in
// that order, and omits any flag whose value is empty. agentModel, when
// non-empty, wins over model. PIInvocation and the host-mode command
// builder both render through this function.
func PIModelFlags(provider, agentModel, model, thinking string) []string {
	var args []string
	if provider != "" {
		args = append(args, "--provider", provider)
	}
	if agentModel != "" {
		model = agentModel
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	return args
}

// PIInvocation returns the trailing arg slice that launches PI with the
// profile-derived flags. It is the PI analogue of HarnessInvocation.
//
// The returned slice begins with "pi" and includes:
//
//	--provider <cfg.PIProvider>           (when non-empty)
//	--model    <cfg.AgentModel or cfg.PIModel>
//	                                      (when non-empty; AgentModel — the
//	                                       per-role `--model-override` entry —
//	                                       wins over PIModel)
//	--thinking <cfg.PIThinking>           (when non-empty)
//	--extension <extensionPath>           (always; path is derived from cfg)
//	--approve or --no-approve             (always; cfg.PIApproveProject)
//	--agent    <cfg.AgentRole>            (when non-empty; consumed by the
//	                                       prism PI extension via
//	                                       pi.registerFlag("agent") to source
//	                                       the role system prompt at
//	                                       before_agent_start)
//	--session  <cfg.HarnessSessionID>     (when non-empty AND on-disk session
//	                                       file exists — see piResolveResumeSession)
//	<cfg.InitialPrompt>                   (bare positional arg, when non-empty)
//
// The role system prompt is NOT delivered via this config directory. It is
// injected at runtime by the prism PI extension (pi/extensions/prism.ts,
// before_agent_start), which reads ~/.config/prism/agents/<role>.md and
// appends it to PI's default system prompt. The config directory referenced
// by PI_CODING_AGENT_DIR only carries settings.json / themes / AGENTS.md /
// skills / auth.json.
//
// Conversation resume: when cfg.HarnessSessionID is non-empty,
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

	args = append(args, PIModelFlags(cfg.PIProvider, cfg.AgentModel, cfg.PIModel, cfg.PIThinking)...)

	// Extension path inside the sandbox (directory + filename).
	extensionSandboxDir := cfg.PIExtensionSandboxDir
	if extensionSandboxDir == "" {
		extensionSandboxDir = piExtensionSandboxDefault
	}
	extensionSandboxPath := filepath.Join(extensionSandboxDir, piExtensionFilename)
	args = append(args, "--extension", extensionSandboxPath)
	args = append(args, PIProjectTrustFlag(cfg.PIApproveProject))

	// --agent <role> for the prism PI extension's role-prompt injection.
	// pi has no native concept of agents — the extension
	// registers --agent via pi.registerFlag at factory entry and reads the
	// bound value synchronously in its before_agent_start handler. Skipping
	// this flag when AgentRole is empty matches the edge-case AC: a session
	// with no role file falls back to pi's base system prompt unchanged.
	if cfg.AgentRole != "" {
		args = append(args, "--agent", cfg.AgentRole)
	}

	// --exclude-tools <names> for role-scoped builtin tool restriction.
	// Review roles never legitimately call write/edit; see
	// internal/config/agent_tool_roles.go for the rationale and the role
	// list. A role with no exclusions (including "", worker, coordinator)
	// emits no flag and keeps pi's default builtin tool set.
	if excluded := config.ExcludedToolsForRole(cfg.AgentRole); len(excluded) > 0 {
		args = append(args, "--exclude-tools", strings.Join(excluded, ","))
	}

	// --session <id> for conversation resume. Skipped silently when
	// HarnessSessionID is empty (fresh session); on missing-file the helper
	// logs a warning and returns ok=false so pi starts a new conversation.
	if cfg.HarnessSessionID != "" {
		if ResolvePIResumeSession(cfg) {
			args = append(args, "--session", cfg.HarnessSessionID)
		}
	}

	if cfg.InitialPrompt != "" {
		args = append(args, PIPromptLead(cfg.InitialPrompt)+cfg.InitialPrompt)
	}

	return args
}

// PIProjectTrustFlag returns the project-trust flag for pi: "--approve"
// when approve is true, else "--no-approve". Every pi command line that prism
// builds carries exactly one of the two. A command-line flag wins over every
// other trust source of pi (trust.json, a project_trust extension handler,
// defaultProjectTrust), so a value that an agent writes there has no effect
// on a session that prism starts (issue #3216).
func PIProjectTrustFlag(approve bool) string {
	if approve {
		return "--approve"
	}
	return "--no-approve"
}

// PIPromptLead returns the prefix to put before a prompt passed to pi as a
// positional argument. pi has no `--` terminator and reads a leading `-` as
// an option and a leading `@` as a file reference. One leading space makes
// pi read the prompt as text. The result is "" for any other prompt.
func PIPromptLead(prompt string) string {
	if strings.HasPrefix(prompt, "-") || strings.HasPrefix(prompt, "@") {
		return " "
	}
	return ""
}

// ResolvePIResumeSession returns true when the on-disk pi session JSONL for
// cfg.HarnessSessionID exists under the host sessions root and pi can
// be told to resume it. Returns false when the file is missing, in which case
// it also writes a tagged warning line to the per-session agent-run log so
// the operator can see why the conversation didn't resume. It also returns
// false, with a warning line, when CheckPIResumeCwd refuses the resume: pi
// runs in the cwd of the session-file header, and a sandboxed session can
// write that header (#3284).
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
			if checkErr := CheckPIResumeCwd(cfg); checkErr != nil {
				piLogResumeRefusal(cfg, checkErr)
				return false
			}
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
//	bwrap        → the sessions dir of the worktree in the host root is
//	               bound into the sandbox at
//	               $PI_CODING_AGENT_DIR/sessions/<encoded-cwd> (see
//	               appendPIAgentDirBwrapMounts).
//	sandbox-exec → the dispatcher injects PI_CODING_AGENT_DIR=<host
//	               ~/.pi/agent> into the sandbox env and sandbox-exec shares
//	               the host filesystem (cmd/agent_run_sandbox_exec_darwin.go),
//	               so pi writes to the host root there too.
//
// PI_CODING_AGENT_DIR mirrors pi's own ENV_AGENT_DIR honouring (pi 0.79
// dist/core/session-manager.js getDefaultAgentDir / getDefaultSessionDirPath
// — see internal/harness/pi/archive.go for the full citation). The prism
// developer host sets it system-wide to /run/prism/pi-agent.
//
// The Config parameter is kept for signature stability with callers and
// the archive-side mirror; resolution does not depend on any cfg field.
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
// branch name does not resume the cleaned session's pi conversation.
// DB-side severance alone is sufficient for the bug —
// PIInvocation only appends `--session <id>` when HarnessSessionID is
// non-empty AND ResolvePIResumeSession finds a matching JSONL. That is
// confirmed empirically by pi's transcript rollover:
// pi rolls to a new UUID/file mid-session, this removal only ever matches
// the DB's latest id, and the earlier rollover files that survive every
// close have never caused a dud auto-resume. Removing the on-disk
// transcript merely keeps ~/.pi/agent/sessions/ from accumulating dead
// conversations across reused branch names — and because the transcript is
// also what pi's interactive /resume reads, cleanup invokes this ONLY on
// hard-cleanup paths (severModeHard in cmd/cleanup.go); soft closes
// preserve the transcript and rely on the DB clear alone.
//
// Resolution mirrors piResumeSessionsRoot: every isolation mode (host,
// bwrap, sandbox-exec) resolves to the host sessions root —
// $PI_CODING_AGENT_DIR/sessions when set, else `~/.pi/agent/sessions/`.
// For sandbox-exec this removal is load-bearing, not redundant: pi writes
// its transcripts to the host root (the dispatcher injects
// PI_CODING_AGENT_DIR into the sandbox env), so the per-session work-dir
// wipe in RemoveSessionWorkDir never touches them.
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
// the same worktree path (e.g. the legitimate-resume case where a
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
// and want honest summary output: `prism reset` snapshots every
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
// A bwrap sandbox sees the host dir through read-only binds in a private
// dir at /run/prism/pi-agent. A sandbox-exec profile allows a read of the
// host dir and a write of an exact list of paths in it. pi_agent_dir.go
// holds the list and the reasons (#3284).
//
// The role system-prompt is injected at runtime by the prism PI extension
// (pi/extensions/prism.ts, before_agent_start) from
// ~/.config/prism/agents/<role>.md.
// A role with no <role>.md file simply starts with PI's default prompt — the
// extension returns no override, no error (edge-case AC).
func EnsurePIAgentConfigDir() (hostDir, sandboxDir string, err error) {
	home, hErr := os.UserHomeDir()
	if hErr != nil || home == "" {
		return "", "", fmt.Errorf("pi: resolve user home: %w", hErr)
	}
	hostDir = filepath.Join(home, ".pi", "agent")
	// Create the host dir if absent so bwrap has a valid bind source on a
	// fresh install. Mode 0o700 keeps the shared config dir owner-only.
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
// This is the path the PI extension connects to for the sidecar wire
// protocol. It is inlined here (mirroring sessionRunDir) to avoid a
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
// Used by the host-mode pi launch in internal/session/session.go so host
// mode emits --extension. Without it the prism extension never loads and
// role-prompt injection (plus the sidecar bridge, status bar, doom-loop
// guard, etc.) silently no-ops. The container path already appends
// --extension via PIInvocation above.
func PIExtensionHostPath(hostDir string) string {
	if hostDir == "" {
		return ""
	}
	return filepath.Join(hostDir, piExtensionFilename)
}

// appendPIBwrapMounts appends the bwrap bind-mount args needed for a PI
// session to args and returns the extended slice. Specifically:
//
//   - PI agent config directory: a private dir at cfg.PIAgentConfigSandboxDir
//     that holds read-only binds of the entries of cfg.PIAgentConfigHostDir,
//     read-write binds of the credential files, and a read-write bind of the
//     sessions dir of cfg.Worktree (appendPIAgentDirBwrapMounts,
//     pi_agent_dir.go). Sets PI_CODING_AGENT_DIR to the sandbox path.
//   - Shared state dirs: cfg.PILockHostDir and cfg.PINotionHostDir,
//     read-write, at the same path (pi_shared_state.go).
//   - Extension directory: cfg.PIExtensionHostDir → cfg.PIExtensionSandboxDir
//     (read-only).
//
// Returns an error when the extension host path is empty (indicating the caller
// forgot to populate the Config fields), the extension directory cannot be
// validated, or the agent dir cannot be read.
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
	// A worker must not write the shared host ~/.pi/agent: every later
	// session loads code and prompts from it (#3284). pi must still make its
	// lock dirs in the root of the agent dir, so the root in the sandbox is a
	// private dir, not a bind of the host dir. See pi_agent_dir.go.
	agentConfigHostDir := cfg.PIAgentConfigHostDir
	agentConfigSandboxDir := cfg.PIAgentConfigSandboxDir
	if agentConfigSandboxDir == "" {
		agentConfigSandboxDir = piAgentConfigSandboxDefault
	}
	if agentConfigHostDir != "" {
		var err error
		args, err = appendPIAgentDirBwrapMounts(args, agentConfigHostDir, agentConfigSandboxDir, cfg.Worktree)
		if err != nil {
			return nil, err
		}
		args = append(args, "--setenv", "PI_CODING_AGENT_DIR", agentConfigSandboxDir)
	}

	// ── Credential files at the host path (read-write) ───────────────────
	// pi and the extensions read the credential files at
	// $PI_CODING_AGENT_DIR (bound above). These binds at the host path serve
	// a code path that resolves $HOME/.pi/agent/<file> directly in the
	// sandbox. They are the same host files as the binds above, so they add
	// no write access. preparePIAgentDir creates the files on the host, so
	// that a login in the sandbox reaches the host.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, name := range piAgentCredentialFiles {
			path := filepath.Join(home, ".pi", "agent", name)
			if fi, statErr := os.Lstat(path); statErr == nil && fi.Mode().IsRegular() {
				args = append(args, "--bind", path, path)
			}
		}
	}

	// ── Shared state dirs (read-write, Dst==Src) ─────────────────────────
	// The lock dir and the Notion store dir are the same for every session
	// on the host (pi_shared_state.go). A dir bind, not a file bind: the
	// Notion extension renames a temp file onto its store file, and a
	// rename onto a bind mount fails. Dst==Src, because the sandbox HOME is
	// the host HOME and the extensions compute both paths from HOME.
	for _, dir := range []string{cfg.PILockHostDir, cfg.PINotionHostDir} {
		if dir == "" {
			continue
		}
		if fi, statErr := os.Lstat(dir); statErr == nil && fi.IsDir() {
			args = append(args, "--bind", dir, dir)
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
