// Package container manages sandbox lifecycle and mount preparation for
// prism agent sessions.
// This file defines the shared MountSpec shape and StandardSandboxMounts walk
// used by the sandbox mount-emission paths (issue #1149 A2.M1; design
// proposal A2 §3.1, §3.S6).
//
// The common decision tree — "which host artefact lives at which canonical
// in-sandbox path, with which read/write/optional/symlink-resolve flags" — is
// expressed once here, mode-agnostic. Each isolator walks the slice and
// translates the entries into its own argument grammar via per-mode appenders:
//
//   - AppendBwrapBind(args, spec) → "--ro-bind|--bind SRC DST"
//
// Today only the bwrap appender is wired through the slice; the
// sandbox-exec staging HOME is built via collectStagingHomeSymlinkTargets in
// sandbox_exec_home.go, which serves the same purpose with a different
// implementation strategy (sandbox-exec has no bind-mount mechanism, so the
// staging HOME is materialised as real symlinks rather than emitted as mount
// arguments).
package container

import (
	"os"
	"path/filepath"
)

// MountSpec describes one host artefact that needs to appear inside the
// sandbox interior at a canonical path. It is mode-agnostic — each isolator
// emits the syntax it needs via its own appender.
//
// The SandboxPath is the path the agent inside the sandbox sees. For bwrap
// and sandbox-exec, HOME-relative paths are rooted at the host user's $HOME
// (or staging HOME for sandbox-exec).
// StandardSandboxMounts computes the SandboxPath using the sandboxHomeDir
// argument the caller provides, so the per-mode HOME prefix is captured at
// the call site rather than baked into MountSpec.
type MountSpec struct {
	// HostPath is the absolute host path of the artefact. Empty means
	// "skip this mount" — used so that conditional mounts can be returned
	// in a unified slice without forcing every entry to be present.
	HostPath string

	// SandboxPath is the absolute path the artefact must appear at inside
	// the sandbox. May or may not equal HostPath depending on the mode.
	SandboxPath string

	// ReadOnly mounts the artefact read-only when true. Bwrap emits
	// "--ro-bind"; sandbox-exec emits the file-read* allow rule
	// without file-write*.
	ReadOnly bool

	// EvalSymlinks resolves the host path through filepath.EvalSymlinks
	// before emission. Required for sops-managed artefacts (where the host
	// path is a /run/secrets/... symlink that the sandbox cannot follow).
	// Resolution failure → silent skip (treated as "the artefact does not
	// exist on this host", matching the original conditional-mount pattern
	// for AWS / Kube / SSH keys).
	EvalSymlinks bool

	// OptionalIfMissing skips the mount silently when HostPath does not
	// exist (os.Stat fails). Used for AWS / Kube / Claude / MCP-auth /
	// auth.json — artefacts that may be present on some hosts and
	// not others. Does not apply when EvalSymlinks is true (resolution
	// failure already implies "missing").
	OptionalIfMissing bool

	// SELinuxRelabel marked the mount for an SELinux ":Z" relabel in the
	// removed container path. Ignored by bwrap and sandbox-exec — neither
	// participates in SELinux labelling. Retained for shape compatibility.
	SELinuxRelabel bool
}

// resolveMountHostPath applies the EvalSymlinks / OptionalIfMissing
// conditionality rules to a MountSpec and returns:
//
//   - the path that should be used in the emitted argument (post-resolution
//     when EvalSymlinks is true; the original HostPath otherwise);
//   - a boolean indicating whether the mount should be emitted at all.
//
// HostPath == "" → skip silently (allows callers to build the slice with
// conditional entries inline). EvalSymlinks failure → skip silently (the
// artefact does not exist on this host). OptionalIfMissing && os.Stat fails
// → skip silently. All other failures fall through to "emit the path
// verbatim and let the kernel produce the real error".
func resolveMountHostPath(spec MountSpec) (string, bool) {
	if spec.HostPath == "" {
		return "", false
	}
	if spec.EvalSymlinks {
		resolved, err := filepath.EvalSymlinks(spec.HostPath)
		if err != nil {
			return "", false
		}
		return resolved, true
	}
	if spec.OptionalIfMissing {
		if _, err := os.Stat(spec.HostPath); err != nil {
			return "", false
		}
	}
	return spec.HostPath, true
}

// StandardSandboxMounts returns the canonical mount set for a given session
// configuration, mode-agnostic. Each isolator walks the slice and emits
// per-mode syntax via its own appender.
//
// sandboxHomeDir is the in-sandbox $HOME path (the host user's home
// directory for bwrap, or the staging HOME for sandbox-exec). It is used as
// the prefix for every HOME-relative SandboxPath in the returned slice.
//
// hostHome is the absolute host home directory (the source of HOME-relative
// host artefacts). When empty, host-relative entries fall back to
// os.UserHomeDir() and finally $HOME.
//
// mode is the isolation mode of the calling isolator. A small number of
// entries have a per-mode RO/RW divergence that is intentional (see the
// per-entry comments below). Passing the mode here keeps the divergence in
// one place rather than scattering per-mode tweaks at the call site.
//
// The returned slice is split into logical groups by adjacency for ease of
// reading at the call site, but callers should treat it as an opaque list of
// mounts.
func StandardSandboxMounts(cfg Config, sandboxHomeDir, hostHome string, mode isolationMode) []MountSpec {
	if hostHome == "" {
		if h, err := os.UserHomeDir(); err == nil {
			hostHome = h
		} else {
			hostHome = os.Getenv("HOME")
		}
	}

	// Per-mode RO/RW classification for the AWS SSO/CLI cache directories.
	//
	// These mounts are read-write (awsSSOReadOnly = false, awsCLIReadOnly =
	// false) because the aws CLI must be able to write STS token cache entries
	// to ~/.aws/cli/cache/ and refresh SSO tokens in ~/.aws/sso/ from inside
	// the sandbox. Without write access the CLI fails with EPERM, and kubectl
	// against EKS also breaks (its exec-credential plugin shells out to aws).
	//
	// bwrap uses --bind (RW) for both dirs. sandbox-exec mirrors this by
	// emitting (allow file-read* file-write* (subpath ~/.aws/sso)) and
	// (allow file-read* file-write* (subpath ~/.aws/cli)) in generateProfile
	// (issue #1558) and by listing .aws/sso and .aws/cli as writable in
	// collectStagingHomeSymlinkTargets.
	//
	// Note: this slice is only walked by bwrap (AppendBwrapBind). The mode
	// parameter is retained for potential future per-mode divergences.
	awsSSOReadOnly := false
	awsCLIReadOnly := false
	_ = mode // mode is retained for future per-mode mount tweaks

	specs := []MountSpec{
		// ── ~/.claude (RW) ───────────────────────────────────────────────
		// Anthropic API credentials directory. Read-write so the
		// pi-anthropic-oauth extension can refresh OAuth tokens. Mounted
		// unconditionally — the agent fails gracefully when absent.
		{
			HostPath:    filepath.Join(hostHome, ".claude"),
			SandboxPath: filepath.Join(sandboxHomeDir, ".claude"),
		},

		// ── ~/.mcp-auth (RW, conditional) ────────────────────────────────
		// mcp-remote OAuth tokens. Mounted only when the directory exists
		// on the host — hosts without Atlassian MCP configured are
		// unaffected.
		{
			HostPath:          filepath.Join(hostHome, ".mcp-auth"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".mcp-auth"),
			OptionalIfMissing: true,
		},

		// ── ~/.npm (RW, conditional) ─────────────────────────────────────
		// npx caches downloaded packages (e.g. mcp-remote) under
		// ~/.npm/_npx/. Without this mount, an npx-fetched tool inside
		// the sandbox cannot find its cache and attempts to re-download —
		// which then fails under the sandbox's network restrictions.
		//
		// Read-write so npx can populate the cache on first use. Mounted
		// only when the directory exists on the host — hosts that have
		// never run npm/npx are unaffected. Dst==Src under bwrap (where
		// sandboxHomeDir == hostHome) so the canonical $HOME/.npm path
		// inside the sandbox matches the host path.
		//
		// Parity with sandbox-exec's staging-HOME entry at
		// sandbox_exec_home.go:309-316. See issue #2127.
		{
			HostPath:          filepath.Join(hostHome, ".npm"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".npm"),
			OptionalIfMissing: true,
		},

		// ── ~/.cache/nix (RW) ────────────────────────────────────────────
		// Pre-populated nix flake input cache. Read-write because nix
		// writes to its SQLite databases during evaluation.
		{
			HostPath:       filepath.Join(hostHome, ".cache", "nix"),
			SandboxPath:    filepath.Join(sandboxHomeDir, ".cache", "nix"),
			SELinuxRelabel: true,
		},

		// ── AWS readonly-config (RO, EvalSymlinks, Dst==Src at XDG path) ─
		// The aws CLI resolves its config and shared-credentials files via
		// the AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE env vars at the
		// host XDG paths (~/.config/aws/readonly-config and
		// ~/.config/aws/credentials, declared in agent.envVars by the nix
		// module). The former RO bind-mounts at the canonical
		// $HOME/.aws/{config,credentials} paths were dropped in issue #2234
		// (Step 3a of #2132, bwrap convergence per design decision §5.1) —
		// see env.go for the matching un-suppression.
		//
		// The bwrap mount namespace is additive from an empty root, so the
		// env-var route still needs the file content delivered INTO the
		// namespace: bind the XDG paths Dst==Src so the env vars resolve to
		// a readable file in-sandbox. EvalSymlinks pins the sops-resolved
		// target inode (same rotation semantics as the previous canonical
		// binds); resolution failure (file absent — credentials is absent on
		// the current host) silently skips the mount while the env var still
		// flows, which the aws CLI tolerates (config-only operation).
		//
		// sandbox-exec does not walk this slice (see the package comment):
		// there the real host paths are visible modulo SBPL and the read
		// rides the #2211 secrets.d allowlist.
		{
			HostPath:     filepath.Join(hostHome, ".config", "aws", "readonly-config"),
			SandboxPath:  filepath.Join(sandboxHomeDir, ".config", "aws", "readonly-config"),
			ReadOnly:     true,
			EvalSymlinks: true,
		},

		// ── AWS credentials (RO, EvalSymlinks, Dst==Src at XDG path) ────
		{
			HostPath:     filepath.Join(hostHome, ".config", "aws", "credentials"),
			SandboxPath:  filepath.Join(sandboxHomeDir, ".config", "aws", "credentials"),
			ReadOnly:     true,
			EvalSymlinks: true,
		},

		// ── AWS SSO cache (conditional, per-mode RO) ────────────────────
		// Always written to ~/.aws/sso by the AWS CLI (regardless of
		// AWS_CONFIG_FILE). Mounted at $HOME/.aws/sso. RW on bwrap
		// (rationale: see awsSSOReadOnly above).
		{
			HostPath:          filepath.Join(hostHome, ".aws", "sso"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".aws", "sso"),
			ReadOnly:          awsSSOReadOnly,
			OptionalIfMissing: true,
		},

		// ── AWS CLI cache (conditional, per-mode RO) ────────────────────
		{
			HostPath:          filepath.Join(hostHome, ".aws", "cli"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".aws", "cli"),
			ReadOnly:          awsCLIReadOnly,
			OptionalIfMissing: true,
		},

		// ── Kube agents config (RO, EvalSymlinks, Dst==Src at XDG path) ─
		// kubectl resolves its config via the KUBECONFIG env var at the host
		// XDG path (~/.config/kube/agents-config, declared in agent.envVars
		// by the nix module). The former RO bind-mount at the canonical
		// $HOME/.kube/config path was dropped in issue #2235 (Step 3b of
		// #2132, bwrap convergence per design decision §5.1) — see env.go for
		// the matching un-suppression.
		//
		// Same shape as the AWS XDG binds above (#2234): the bwrap mount
		// namespace is additive from an empty root, so the env-var route
		// still needs the file content delivered INTO the namespace — bind
		// the XDG path Dst==Src so KUBECONFIG resolves to a readable file
		// in-sandbox. EvalSymlinks pins the sops-resolved target inode (same
		// rotation semantics as the previous canonical bind); resolution
		// failure (file absent on host) silently skips the mount while the
		// env var still flows.
		//
		// kubectl's cache is redirected via KUBECACHEDIR (see BuildArgs in
		// bwrap.go) so no writable kube path is needed in-namespace.
		//
		// sandbox-exec does not walk this slice (see the package comment):
		// there the real host paths are visible modulo SBPL and the read
		// rides the #2211 secrets.d allowlist.
		{
			HostPath:     filepath.Join(hostHome, ".config", "kube", "agents-config"),
			SandboxPath:  filepath.Join(sandboxHomeDir, ".config", "kube", "agents-config"),
			ReadOnly:     true,
			EvalSymlinks: true,
		},

		// ── Clipboard staging dir (RO, conditional) ──────────────────────
		// Images staged by `prism clipboard paste-image`. Dst==Src — the
		// agent reads at the same absolute path the host writes to.
		{
			HostPath:          filepath.Join(hostHome, ".cache", "prism", "clipboard"),
			SandboxPath:       filepath.Join(hostHome, ".cache", "prism", "clipboard"),
			ReadOnly:          true,
			OptionalIfMissing: true,
		},

		// ── prism agent role prompts (RO, conditional) ───────────────────
		// Host: ~/.config/prism/agents/ (deployed via pi.nix:240 →
		// xdg.configFile."prism/agents".source = ./agents). One markdown file
		// per role (coordinator.md, worker.md, review-*.md). The prism PI
		// extension reads <role>.md at before_agent_start and injects it as the
		// role system prompt (issue #2032).
		//
		// Mounted UNCONDITIONALLY for every role, including review-*. The dir
		// is pure markdown with no secrets, so there is no isolation value in
		// hiding sibling role prompts from a review agent (locked decision on
		// design #2031). Read-only — agents never write their own prompt.
		//
		// SandboxPath == HostPath: under bwrap sandboxHomeDir == hostHome, so
		// the extension's XDG_CONFIG_HOME-else-$HOME/.config resolution lands
		// on exactly this path inside the sandbox.
		{
			HostPath:          filepath.Join(hostHome, ".config", "prism", "agents"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".config", "prism", "agents"),
			ReadOnly:          true,
			OptionalIfMissing: true,
		},
	}

	return specs
}

// AppendBwrapBind appends a bwrap --ro-bind or --bind argument triple for the
// given MountSpec. It applies the EvalSymlinks / OptionalIfMissing rules via
// resolveMountHostPath and emits the correct flag based on spec.ReadOnly.
// SELinuxRelabel is ignored — bwrap does not participate in SELinux labelling.
// Returns args unchanged when the mount is skipped.
//
// Free function (not a method on bwrapIsolator) — the emitter is stateless
// and does not need access to isolator state.
func AppendBwrapBind(args []string, spec MountSpec) []string {
	src, ok := resolveMountHostPath(spec)
	if !ok {
		return args
	}
	flag := "--bind"
	if spec.ReadOnly {
		flag = "--ro-bind"
	}
	return append(args, flag, src, spec.SandboxPath)
}
