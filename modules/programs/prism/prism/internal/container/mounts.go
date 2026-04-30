// Package container manages the podman container lifecycle for prism sidecar.
// This file defines the shared MountSpec shape and StandardSandboxMounts walk
// used by the podman and bwrap mount-emission paths (issue #1149 A2.M1; design
// proposal A2 §3.1, §3.S6).
//
// The common decision tree — "which host artefact lives at which canonical
// in-sandbox path, with which read/write/optional/symlink-resolve flags" — is
// expressed once here, mode-agnostic. Each isolator walks the slice and
// translates the entries into its own argument grammar via per-mode appenders:
//
//   - podmanIsolator.appendVolume(args, spec)        → "--volume SRC:DST[:Z][:ro]"
//   - bwrapIsolator.appendBind(args, spec)           → "--ro-bind|--bind SRC DST"
//   - sandboxExecIsolator.appendAllow(profile, spec) → "(subpath \"...\")" in SBPL
//
// Today only the podman and bwrap appenders are wired through the slice; the
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
// The SandboxPath is the path the agent inside the sandbox sees. For podman,
// HOME-relative paths are rooted at /root/...; for bwrap and sandbox-exec
// they are rooted at the host user's $HOME (or staging HOME for sandbox-exec).
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

	// ReadOnly mounts the artefact read-only when true. Podman emits ":ro";
	// bwrap emits "--ro-bind"; sandbox-exec emits the file-read* allow rule
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
	// opencode auth.json — artefacts that may be present on some hosts and
	// not others. Does not apply when EvalSymlinks is true (resolution
	// failure already implies "missing").
	OptionalIfMissing bool

	// SELinuxRelabel adds the ":Z" label suffix on podman so that the
	// container can read/write the bind-mounted directory on
	// SELinux-enforcing hosts (Fedora, RHEL). Ignored by bwrap and
	// sandbox-exec — neither participates in SELinux labelling.
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
// sandboxHomeDir is the in-sandbox $HOME path (e.g. "/root" for podman or the
// host user's home directory for bwrap). It is used as the prefix for every
// HOME-relative SandboxPath in the returned slice.
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

	// Per-mode RO/RW divergence for the AWS SSO/CLI cache directories:
	//
	//   - podman mounts them read-only because the container has its own net
	//     namespace; the SSO token is consumed inside the container but
	//     refresh happens host-side via `aws sso login`.
	//   - bwrap mounts them read-write because the bwrap sandbox shares the
	//     host net namespace and may itself perform an SSO refresh that
	//     writes to the cache (the original bwrap.go used --bind, not
	//     --ro-bind).
	//
	// This is incidentally-different — both modes accept reads from the
	// cache; the writeability flag is the only difference. Encoding it here
	// keeps the per-mode-tweak knowledge in one file rather than scattering
	// "RW for bwrap, RO for podman" comments at every call site.
	awsSSOReadOnly := mode == isolationPodman
	awsCLIReadOnly := mode == isolationPodman

	specs := []MountSpec{
		// ── ~/.claude (RW) ───────────────────────────────────────────────
		// Anthropic API credentials directory. Read-write so the
		// opencode-claude-auth plugin can refresh OAuth tokens. Mounted
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

		// ── ~/.cache/nix (RW) ────────────────────────────────────────────
		// Pre-populated nix flake input cache. Read-write because nix
		// writes to its SQLite databases during evaluation.
		{
			HostPath:       filepath.Join(hostHome, ".cache", "nix"),
			SandboxPath:    filepath.Join(sandboxHomeDir, ".cache", "nix"),
			SELinuxRelabel: true,
		},

		// ── AWS readonly-config (RO, EvalSymlinks) ───────────────────────
		// Host: ~/.config/aws/readonly-config (XDG, sops-managed symlink).
		// Sandbox: $HOME/.aws/config (canonical AWS CLI default path).
		{
			HostPath:     filepath.Join(hostHome, ".config", "aws", "readonly-config"),
			SandboxPath:  filepath.Join(sandboxHomeDir, ".aws", "config"),
			ReadOnly:     true,
			EvalSymlinks: true,
		},

		// ── AWS credentials (RO, EvalSymlinks) ───────────────────────────
		{
			HostPath:     filepath.Join(hostHome, ".config", "aws", "credentials"),
			SandboxPath:  filepath.Join(sandboxHomeDir, ".aws", "credentials"),
			ReadOnly:     true,
			EvalSymlinks: true,
		},

		// ── AWS SSO cache (conditional, per-mode RO) ────────────────────
		// Always written to ~/.aws/sso by the AWS CLI (regardless of
		// AWS_CONFIG_FILE). Mounted at $HOME/.aws/sso. RO on podman
		// (rationale: see awsSSOReadOnly above), RW on bwrap.
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

		// ── Kube agents config (RO, EvalSymlinks) ────────────────────────
		// Host: ~/.config/kube/agents-config (XDG, sops-managed symlink).
		// Sandbox: $HOME/.kube/config (canonical kubectl default path).
		{
			HostPath:     filepath.Join(hostHome, ".config", "kube", "agents-config"),
			SandboxPath:  filepath.Join(sandboxHomeDir, ".kube", "config"),
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
	}

	return specs
}

// StandardOpencodeConfigAllowlist returns the static list of entries in
// ~/.config/opencode/ that should be mounted into the sandbox. Excluded:
// opencode.json (mounted separately from a per-session temp file), package.json,
// bun.lock, package-lock.json, node_modules/ (bun ecosystem files the sandbox
// manages itself).
//
// agents/ is excluded for review-* agents (see container.go:1019-1025 for the
// full rationale).
func StandardOpencodeConfigAllowlist(isReview bool) []string {
	allowlist := []string{
		"AGENTS.md",
		"plugins",
		"skills",
		"command",
		"tui.json",
		".gitignore",
		"mcp-atlassian-slim-proxy.mjs",
	}
	if !isReview {
		allowlist = append(allowlist, "agents")
	}
	return allowlist
}

// appendPodmanVolume appends a podman --volume argument pair for the given
// MountSpec. It applies the EvalSymlinks / OptionalIfMissing rules via
// resolveMountHostPath and emits "--volume SRC:DST[:Z][:ro]" when the mount
// should be active. Returns args unchanged when the mount is skipped.
//
// Podman has a subtle file-vs-directory distinction for SELinux-relabelled
// mounts: directory mounts that the container should be able to populate use
// "--mount type=bind,...", but for the canonical-name remap pattern used by
// StandardSandboxMounts (where DST is created lazily by podman), --volume
// SRC:DST[:Z][:ro] works for both files and directories. The existing
// container.go always used --volume for these, so we keep that.
//
// Free function (not a method on podmanIsolator) because the emitter is
// stateless and the podman mount block is exercised by tests that build a
// Manager with a non-podman isolator, then call buildRunArgs directly. A
// free function lets those tests continue to work without an awkward
// type-cast or fresh-isolator instantiation.
func appendPodmanVolume(args []string, spec MountSpec) []string {
	src, ok := resolveMountHostPath(spec)
	if !ok {
		return args
	}
	flags := ""
	if spec.SELinuxRelabel {
		flags += ":Z"
	}
	if spec.ReadOnly {
		flags += ":ro"
	}
	return append(args, "--volume", src+":"+spec.SandboxPath+flags)
}

// appendBwrapBind appends a bwrap --ro-bind or --bind argument triple for the
// given MountSpec. It applies the EvalSymlinks / OptionalIfMissing rules via
// resolveMountHostPath and emits the correct flag based on spec.ReadOnly.
// SELinuxRelabel is ignored — bwrap does not participate in SELinux labelling.
// Returns args unchanged when the mount is skipped.
//
// Free function (not a method on bwrapIsolator) — the emitter is stateless
// and a free function is symmetric with appendPodmanVolume above.
func appendBwrapBind(args []string, spec MountSpec) []string {
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
