// Package container manages sandbox lifecycle and mount preparation for
// prism agent sessions.
// This file defines the shared MountSpec shape and StandardSandboxMounts walk
// used by the sandbox mount-emission paths.
//
// The common decision tree — "which host artefact lives at which canonical
// in-sandbox path, with which read/write/optional/symlink-resolve flags" — is
// expressed once here, mode-agnostic. Each isolator walks the slice and
// translates the entries into its own argument grammar via per-mode appenders:
//
//   - AppendBwrapBind(args, spec) → "--ro-bind|--bind SRC DST"
//
// Today only the bwrap appender is wired through the slice; sandbox-exec
// has no bind-mount mechanism — it delivers the same capabilities via
// explicit SBPL grants on the real host paths emitted by generateProfile
// (sandbox_exec.go) plus env-var injection at host XDG paths.
package container

import (
	"os"
	"path/filepath"

	"github.com/prismatic-koi/prism/internal/usage"
)

// MountSpec describes one host artefact that needs to appear inside the
// sandbox interior at a canonical path. It is mode-agnostic — each isolator
// emits the syntax it needs via its own appender.
//
// The SandboxPath is the path the agent inside the sandbox sees. For bwrap
// and sandbox-exec, HOME-relative paths are rooted at the host user's $HOME.
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

	// SELinuxRelabel is ignored by bwrap and sandbox-exec — neither
	// participates in SELinux labelling. Kept for shape compatibility.
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
// directory — both bwrap and sandbox-exec run with the real host $HOME).
// It is used as the prefix for every HOME-relative
// SandboxPath in the returned slice.
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
	// (generateProfile section 5e).
	//
	// Note: this slice is only walked by bwrap (AppendBwrapBind). The mode
	// parameter is retained for potential future per-mode divergences.
	awsSSOReadOnly := false
	awsCLIReadOnly := false
	_ = mode // mode is retained for future per-mode mount tweaks

	// ── prism usage snapshot dir: host source and in-sandbox destination ──
	//
	// Source (host). usage.DirForHome is the single source of truth for the
	// resolution order — $XDG_STATE_HOME first, then <home>/.local/state —
	// shared with usage.DefaultDir (the writer) and usageSnapshotPath() in
	// pi/extensions/prism.ts (the reader). Resolving it here rather than
	// hardcoding ~/.local/state is load-bearing: on a host that exports a
	// non-default $XDG_STATE_HOME the snapshots live somewhere else entirely
	// and a hardcoded source would bind an empty directory.
	//
	// Destination (in-sandbox). Deliberately NOT the source path: it is
	// $HOME-relative, because that is where the in-sandbox reader looks.
	// bwrap starts the sandbox with --clearenv and re-adds only PATH, HOME,
	// USER, LOGNAME, LANG, LC_ALL and SHELL (standardSandboxEnvArgs), so
	// XDG_STATE_HOME is UNSET inside a bwrap sandbox and both readers fall
	// back to $HOME/.local/state. Binding Src→Dst therefore also repairs the
	// custom-$XDG_STATE_HOME host case: the interior path stays canonical
	// whatever the host exported.
	//
	// If either half cannot be resolved there is nothing to mount, or nowhere
	// to mount it, so both stay empty and resolveMountHostPath skips the entry
	// rather than emitting a bind rooted at "/".
	usageHostDir, usageSandboxDir := "", ""
	if hostDir := usage.DirForHome(hostHome); hostDir != "" && sandboxHomeDir != "" {
		usageHostDir = hostDir
		usageSandboxDir = filepath.Join(sandboxHomeDir, ".local", "state", "prism", "usage")
	}

	specs := []MountSpec{
		// ── ~/.config/claude (RW, conditional, Dst==Src at XDG path) ─────
		// claude-code's config dir (settings, history, .claude.json, OAuth
		// token refreshes), resolved via the CLAUDE_CONFIG_DIR env var at
		// the host XDG path — declared in agent.envVars by the nix module.
		//
		// The bwrap mount namespace is additive from an empty root, so the
		// env-var route still needs the directory delivered INTO the
		// namespace: bind it Dst==Src so CLAUDE_CONFIG_DIR resolves to a
		// real read-write directory in-namespace, with writes flowing
		// through to the host.
		// Unlike the aws/kube XDG entries this is a plain host directory,
		// not a sops symlink — no EvalSymlinks. Mounted only when present
		// (bwrap aborts on missing --bind sources); on a host without the
		// dir, claude-code simply creates an ephemeral in-namespace dir at
		// the env var path.
		//
		// sandbox-exec does not walk this slice (see the package comment):
		// there generateProfile emits an explicit RW
		// (subpath ~/.config/claude) grant on the real host path.
		{
			HostPath:          filepath.Join(hostHome, ".config", "claude"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".config", "claude"),
			OptionalIfMissing: true,
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
		// Parity with sandbox-exec's §5e RW grant on the real ~/.npm
		// (generateProfile in sandbox_exec.go).
		{
			HostPath:          filepath.Join(hostHome, ".npm"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".npm"),
			OptionalIfMissing: true,
		},

		// ── ~/.cache/nix (RW, conditional) ────────────────────────────────────────────
		// Pre-populated nix flake input cache. Read-write because nix
		// writes to its SQLite databases during evaluation.
		//
		// OptionalIfMissing: bwrap ABORTS on a missing bind source, so a
		// missing ~/.cache/nix must be skipped rather than bound. When
		// absent, nix simply creates an ephemeral in-namespace dir.
		{
			HostPath:          filepath.Join(hostHome, ".cache", "nix"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".cache", "nix"),
			OptionalIfMissing: true,
			SELinuxRelabel:    true,
		},

		// ── ~/.cache/bun (RW, conditional) ──────────────────────────────
		// bun transpiler cache — bun writes transpile outputs and lockfile
		// updates here on plugin load. RW, Dst==Src, skipped when absent.
		//
		// sandbox-exec does not walk this slice (see the package comment):
		// there generateProfile emits an explicit RW (subpath ~/.cache/bun)
		// grant on the real host path (section 5e).
		{
			HostPath:          filepath.Join(hostHome, ".cache", "bun"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".cache", "bun"),
			OptionalIfMissing: true,
		},

		// ── AWS readonly-config (RO, EvalSymlinks, Dst==Src at XDG path) ─
		// The aws CLI resolves its config and shared-credentials files via
		// the AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE env vars at the
		// host XDG paths (~/.config/aws/readonly-config and
		// ~/.config/aws/credentials, declared in agent.envVars by the nix
		// module).
		//
		// The bwrap mount namespace is additive from an empty root, so the
		// env-var route still needs the file content delivered INTO the
		// namespace: bind the XDG paths Dst==Src so the env vars resolve to
		// a readable file in-sandbox. EvalSymlinks pins the sops-resolved
		// target inode; resolution failure (file absent — credentials is absent on
		// the current host) silently skips the mount while the env var still
		// flows, which the aws CLI tolerates (config-only operation).
		//
		// sandbox-exec does not walk this slice (see the package comment):
		// there the real host paths are visible modulo SBPL and the read
		// rides the secrets.d allowlist.
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
		// by the nix module).
		//
		// Same shape as the AWS XDG binds above: the bwrap mount
		// namespace is additive from an empty root, so the env-var route
		// still needs the file content delivered INTO the namespace — bind
		// the XDG path Dst==Src so KUBECONFIG resolves to a readable file
		// in-sandbox. EvalSymlinks pins the sops-resolved target inode;
		// resolution failure (file absent on host) silently skips the mount
		// while the env var still flows.
		//
		// kubectl's cache is redirected via KUBECACHEDIR (see BuildArgs in
		// bwrap.go) so no writable kube path is needed in-namespace.
		//
		// sandbox-exec does not walk this slice (see the package comment):
		// there the real host paths are visible modulo SBPL and the read
		// rides the secrets.d allowlist.
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
		// role system prompt.
		//
		// Mounted UNCONDITIONALLY for every role, including review-*. The dir
		// is pure markdown with no secrets, so there is no isolation value in
		// hiding sibling role prompts from a review agent. Read-only — agents
		// never write their own prompt.
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

		// ── prism profiles.json (RO, conditional, single file) ──────────
		// Host: ~/.config/prism/profiles.json (deployed via pi.nix from the
		// rendered nix profile data — declarative, non-secret). The CLI's
		// `prism profile list`, `prism profile show`, and the
		// available_profiles section of `prism agent-context` all open this
		// file directly via internal/config/profiles.go::LoadProfiles. The
		// mutation surface (`prism profile use`) routes through the host API
		// instead and does not need an in-sandbox file, but the read surface
		// does — without this mount the same commands fail from inside the
		// sandbox with the (misleading) "not found — run the system rebuild"
		// error.
		//
		// Read-only — the file is owned by the host nix module; nothing
		// in-sandbox may mutate it. Single-file scope (no surrounding
		// directory widening): we already mount the sibling agents/ subdir;
		// adding profiles.json on its own keeps the rest of
		// ~/.config/prism/ (e.g. ~/.config/prism/accounts/, runtime-mutable
		// state) out of the sandbox by default.
		//
		// OptionalIfMissing covers fresh installs before first `nh switch`,
		// where the file does not exist yet. In that case LoadProfiles
		// still returns its existing "profiles: <path> not found — run the
		// system rebuild to generate it" error (it ReadFile's the same
		// path), so the host-side missing-file message is preserved
		// verbatim.
		//
		// SandboxPath == HostPath under bwrap (sandboxHomeDir == hostHome),
		// matching profilesFilePath()'s XDG_CONFIG_HOME-else-$HOME/.config
		// resolution inside the sandbox.
		//
		// sandbox-exec does not walk this slice (see the package comment):
		// there generateProfile emits an explicit RO (literal ...) grant on
		// the real host path in section 5h of sandbox_exec.go.
		{
			HostPath:          filepath.Join(hostHome, ".config", "prism", "profiles.json"),
			SandboxPath:       filepath.Join(sandboxHomeDir, ".config", "prism", "profiles.json"),
			ReadOnly:          true,
			OptionalIfMissing: true,
		},

		// ── prism usage snapshot dir (RO, conditional, leaf only) ───────
		// Host: $XDG_STATE_HOME/prism/usage (see the resolution block
		// above). Holds <account>.json per account plus current.json, a
		// copy of the active account's snapshot written by the sidecar
		// endpoint POST /usage/snapshot.
		//
		// Without this mount the bottom-bar usage segment
		// (pi/extensions/prism.ts::readUsageSnapshot) finds no file and
		// renders nothing in EVERY sandboxed session — which is most
		// sessions. The reader already degrades silently on
		// a missing file, so the failure was invisible.
		//
		// READ-ONLY. The display only reads, and every write goes through
		// the sidecar host-API endpoint, so nothing in-sandbox needs write
		// access. RO also means a compromised session cannot forge usage
		// figures on the host. RO must not silently become RW.
		//
		// LEAF ONLY. The bind is the usage directory itself, never a
		// parent: $XDG_STATE_HOME/prism holds prism.db (the full session
		// database) and run/ (every session's host-API socket dir), and
		// $XDG_STATE_HOME itself holds unrelated application state. Widening
		// this entry to a parent would be a far larger grant than the
		// display needs and would defeat the per-session socket isolation.
		//
		// DIRECTORY, not the current.json file. A file-level bind pins the
		// original inode, and the writer replaces the file by atomic
		// rename — a session would be stuck with the snapshot that existed
		// at spawn time. A directory bind is inode-transparent, so a
		// snapshot written on the host mid-session becomes visible without
		// a restart (same reasoning as the host-API socket dir bind in
		// bwrap.go).
		//
		// OptionalIfMissing because bwrap ABORTS on a missing bind source.
		// prepareVolumeDirs pre-creates the directory
		// host-side so the bind is normally always active — this guard
		// covers the case where that creation failed (it is non-fatal by
		// design, e.g. an unwritable HOME in the nix build sandbox). The
		// session then starts normally, just without the mount, and the
		// bottom bar degrades silently exactly as it does today.
		//
		// sandbox-exec does not walk this slice (see the package comment):
		// there generateProfile emits an explicit RO (subpath ...) grant on
		// the real host path in section 5j of sandbox_exec.go.
		{
			HostPath:          usageHostDir,
			SandboxPath:       usageSandboxDir,
			ReadOnly:          true,
			OptionalIfMissing: true,
		},
	}

	// ── Go module cache + build cache (RW, conditional, Dst==Src) ────────
	// ~/go/pkg/mod (GOMODCACHE) and ~/.cache/go-build (GOCACHE). This mirrors
	// the Darwin sandbox-exec grant (generateProfile section 5k). bwrap does
	// not FAIL without them — it rebuilds cold into the sandbox interior on
	// every session, so both caches are shared to keep builds warm.
	//
	// PATHS come from goCacheDirsForGOOS (go_cache.go), the same platform-
	// aware list generateProfile walks on Darwin. One list, so the two
	// platforms and the four consumers (two grants, two pre-creation paths)
	// cannot drift. goosLinux is pinned rather than read from runtime.GOOS
	// because this slice is walked only by bwrap and bwrap is Linux-only
	// (config.ValidIsolationModes; see the package comment) — pinning also
	// keeps the emitted argv deterministic when the suite runs on Darwin.
	// Note the GOCACHE path differs from Darwin's: os.UserCacheDir() is
	// ~/.cache on Linux, not ~/Library/Caches.
	//
	// READ-WRITE is load-bearing for both. go writes module downloads and its
	// lock file under GOMODCACHE, and build/test outputs under GOCACHE; a
	// read-only bind does not make `go build ./...` work. Concurrency is safe
	// by design — the module cache uses a lock file and the build cache is
	// content-addressed, both built for concurrent multi-process access,
	// which is exactly what parallel workers plus the host shell do.
	//
	// LEAF DIRECTORIES ONLY, never a parent — (subpath ~/Library/Caches) is
	// too broad:
	//   - NOT ~/go: that would expose ~/go/bin, where `go install` drops
	//     binaries that are typically on the host's PATH. A sandboxed agent
	//     must not be able to plant an executable the user later runs.
	//   - NOT ~/.cache: the user's whole cache tree. The sandbox binds the
	//     leaves it needs (nix, bun, prism/clipboard) and nothing else.
	//
	// EXEC ASYMMETRY WITH DARWIN, accepted. goCacheDir carries
	// execDenied, true for the module cache: on Darwin the profile emits
	// (deny process-exec* file-map-executable) for ~/go/pkg/mod, so the agent
	// can write module source there but cannot run anything it plants among
	// it. A bwrap bind mount CANNOT express that — bwrap's grammar has no
	// per-bind noexec and no noexec remount — so the field is read here for
	// documentation value only and the Linux module cache is exec-capable
	// where the Darwin one is not. No mechanism is invented to emulate it.
	//
	// That is acceptable, for three reasons:
	//   1. It grants the agent no capability it lacks. The sandbox already
	//      permits exec from every other writable path it holds — the
	//      worktree, /tmp, ~/.npm (npx runs cached JS), ~/.cache/bun. An
	//      agent that wants to run a binary it wrote does not need the
	//      module cache to do it.
	//   2. The host-side risk is identical on both platforms: both grants are
	//      WRITE, so
	//      on either platform a compromised agent can mutate an extracted
	//      module that a later HOST build compiles (go verifies module zips
	//      against go.sum on download, but does not re-verify an already-
	//      extracted tree — that is what `go mod verify` is for). Darwin's
	//      deny narrows in-sandbox execution only; it does not narrow that.
	//   3. The Darwin deny is coupled to the GOTOOLCHAIN=local pin (see
	//      goCacheDir.execDenied and GoToolchainEnv) — the pin exists so the
	//      deny does not break the toolchain auto-download path. bwrap has
	//      neither half of that mechanism, so nothing here is left
	//      half-wired: the Linux sandbox behaves as an unpinned toolchain
	//      always has, except that a downloaded toolchain now persists in the
	//      shared cache instead of being discarded with the session interior.
	//
	// OptionalIfMissing because bwrap ABORTS on a missing bind source, as on
	// the ~/.cache/nix entry above. prepareVolumeDirs
	// pre-creates BOTH directories host-side so the binds are normally always
	// active — that is what makes the cache warm on a machine that has never
	// run go, where a skipped mount would instead leave every session cold
	// forever. This guard covers the case where that creation failed (it is
	// non-fatal by design, e.g. the unwritable HOME of the nix build sandbox):
	// the session then starts normally, just without the mount, and go falls
	// back to building into the ephemeral sandbox interior.
	//
	// HOST PERSISTENCE is the other half of "warm" on these machines. Both
	// Linux hosts wipe the root btrfs subvolume on every boot, so a cache
	// directory that is not persisted starts empty after each reboot.
	// ~/.cache/go-build rides the ".cache" entry in
	// modules/system/impermanence.nix; ~/go/pkg/mod has its own entry in
	// modules/programs/prism/prism-tui.nix, because ~/go is not persisted.
	// Remove either entry and this mount still works, but only within one
	// boot.
	//
	// sandbox-exec does not walk this slice (see the package comment): there
	// generateProfile emits explicit RW (subpath ...) grants on the same two
	// logical caches at their Darwin paths, in section 5k.
	for _, dir := range goCacheDirsForGOOS(hostHome, goosLinux) {
		if sandboxHomeDir == "" {
			break
		}
		// SandboxPath is the host path re-rooted at the in-sandbox $HOME —
		// Dst==Src under bwrap, where sandboxHomeDir == hostHome. Deriving it
		// with filepath.Rel keeps the single source of truth intact: the leaf
		// names are spelled once, in go_cache.go. A Rel failure means the
		// path is not under hostHome, which cannot happen for a path this
		// package built from hostHome — skip rather than emit a bind rooted
		// at "/".
		rel, err := filepath.Rel(hostHome, dir.path)
		if err != nil {
			continue
		}
		specs = append(specs, MountSpec{
			HostPath:          dir.path,
			SandboxPath:       filepath.Join(sandboxHomeDir, rel),
			OptionalIfMissing: true,
		})
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
