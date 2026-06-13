package container

// session_work_dir.go — per-session work directory for generated git/ssh
// configs (issue #2213, Step 2 of the #2132 staging-HOME elimination train).
//
// The session work dir lives at
//
//	<XDG_STATE_HOME>/prism/sessions/<instance_id>/
//
// honouring $XDG_STATE_HOME first (XDG Base Directory Specification), and
// falling back to $HOME/.local/state when XDG_STATE_HOME is unset.
// Production sandbox-exec dispatch sets XDG_STATE_HOME=$HOME/.local/state
// explicitly (see cmd/agent_run_sandbox_exec_darwin.go), so the production
// path remains ~/.local/state/prism/sessions/<instance_id>/.
//
// It contains no symlinks: just the generated regular files that git and
// ssh need inside a sandbox-exec session:
//
//	<sessionDir>/ssh-config        — minimal ssh config for github.com
//	<sessionDir>/gitconfig         — git identity + signing config
//	<sessionDir>/allowed_signers   — for git verify-commit (when signing
//	                                 is configured)
//
// On Darwin it additionally holds the chromium Library skeleton (issue
// #2247, Step 4 of #2132):
//
//	<sessionDir>/Library/Application Support/Google/
//	<sessionDir>/Library/Caches/Google/
//
// — empty real directories that CFFIXED_USER_HOME (set by the dispatcher to
// <sessionDir>) points chromium's NSHomeDirectory() at, so its crash
// database, code cache, profile, and SingletonLock land per-session and
// ephemeral rather than under the real ~/Library.
//
// The files are wired into the sandbox via env vars set by the dispatcher
// (cmd/agent_run_sandbox_exec_darwin.go):
//
//	GIT_CONFIG_GLOBAL=<sessionDir>/gitconfig
//	GIT_SSH_COMMAND="<sshBin> -F <sessionDir>/ssh-config"
//
// Embedded key paths are the STABLE sops symlink paths
// (~/.ssh/<SshAccessKeyName>, ~/.ssh/<SshSigningKeyName>.pub) — never
// filepath.EvalSymlinks-resolved secrets.d/<N> paths (sops rotates
// secrets.d/<N> on every darwin-rebuild switch, which would leave the
// embedded path dangling mid-session — issues #1410/#1573) and never
// staging-HOME paths (the staging HOME is deleted in Step 5 of #2132).
// The in-sandbox read of the resolved key content rides on the existing
// (subpath "/private/var/folders") allow in the SBPL profile, which
// survives rotation.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// SessionWorkDirPath returns the per-session work directory path for the
// given instance ID:
//
//	<XDG_STATE_HOME>/prism/sessions/<instanceID>/
//
// honouring $XDG_STATE_HOME first per the XDG Base Directory Specification,
// and falling back to $HOME/.local/state when XDG_STATE_HOME is unset. This
// mirrors xdgStateBase() in dispatch.go and the same pattern used by
// internal/sidecar tests (see AGENTS.md "Test-suite isolation (#1608)").
//
// In production on Darwin, agent_run_sandbox_exec_darwin.go's
// buildSandboxExecHomeEnv explicitly sets XDG_STATE_HOME=$HOME/.local/state
// before exec'ing into the sandbox, so the production path is unchanged.
// On a normal `nh switch` Darwin shell that does NOT export XDG_STATE_HOME
// the fallback also resolves to $HOME/.local/state — same path as before.
//
// The XDG_STATE_HOME branch is what makes the homeless-shelter nix-build
// sandbox tractable: in that environment $HOME=/homeless-shelter (read-only)
// and any path derived from it fails on os.MkdirAll. Tests that need a
// writable session work dir set XDG_STATE_HOME to a t.TempDir() so this
// function returns a writable path regardless of $HOME (issue #2263).
//
// Callers that have an instance_id but no Manager (e.g. cmd/cleanup.go)
// use this directly.
func SessionWorkDirPath(instanceID string) (string, error) {
	if instanceID == "" {
		return "", fmt.Errorf("container: session work dir: instanceID is empty")
	}
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		return filepath.Join(stateHome, "prism", "sessions", instanceID), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return "", fmt.Errorf("container: session work dir: cannot determine user home directory (XDG_STATE_HOME unset and $HOME unresolved)")
	}
	return filepath.Join(home, ".local", "state", "prism", "sessions", instanceID), nil
}

// sessionWorkDirPath returns the session work dir path for this Manager's
// session. InstanceID namespaces the directory so concurrent sessions have
// independent work dirs; it falls back to the container name when
// InstanceID is unset (e.g. tests that construct a Manager directly
// without a full spawn lifecycle).
func (m *Manager) sessionWorkDirPath() (string, error) {
	instanceID := m.cfg.InstanceID
	if instanceID == "" {
		instanceID = m.name
	}
	return SessionWorkDirPath(instanceID)
}

// SessionWorkDir is the exported version of sessionWorkDirPath. Used by
// cmd/agent_run_sandbox_exec_darwin.go to build the GIT_CONFIG_GLOBAL and
// GIT_SSH_COMMAND env vars for the sandboxed process.
func (m *Manager) SessionWorkDir() (string, error) {
	return m.sessionWorkDirPath()
}

// SessionWorkDirSshConfigPath returns the path of the generated ssh config
// inside the given session work dir.
func SessionWorkDirSshConfigPath(sessionDir string) string {
	return filepath.Join(sessionDir, "ssh-config")
}

// SessionWorkDirGitconfigPath returns the path of the generated gitconfig
// inside the given session work dir.
func SessionWorkDirGitconfigPath(sessionDir string) string {
	return filepath.Join(sessionDir, "gitconfig")
}

// SessionWorkDirAllowedSignersPath returns the path of the generated
// allowed_signers file inside the given session work dir.
func SessionWorkDirAllowedSignersPath(sessionDir string) string {
	return filepath.Join(sessionDir, "allowed_signers")
}

// SessionWorkDirKubeCacheDirPath returns the kubectl cache directory inside
// the given session work dir (issue #2235, Step 3b of #2132):
//
//	<sessionDir>/kube-cache
//
// kubectl writes its discovery/http cache to $HOME/.kube/cache by default,
// which exists on the host and would EPERM under the deny-default SBPL
// profile. KUBECACHEDIR (supported since kubectl ≈ 1.26) redirects the
// cache here instead. The directory is not pre-created — kubectl
// MkdirAll's it on first use, which the SBPL profile's
// (subpath <sessionDir>) RW allow already covers; the cache stays
// per-session and ephemeral (the work dir is removed by
// RemoveSessionWorkDir on cleanup).
func SessionWorkDirKubeCacheDirPath(sessionDir string) string {
	return filepath.Join(sessionDir, "kube-cache")
}

// SessionWorkDirKubeEnv returns the env var pair (K=V form) that redirects
// kubectl's cache inside the sandbox at the session work dir:
//
//	KUBECACHEDIR=<sessionDir>/kube-cache
//
// Injected by the sandbox-exec dispatcher
// (cmd/agent_run_sandbox_exec_darwin.go) alongside SessionWorkDirGitEnv.
// The kube config itself is delivered via KUBECONFIG from agent.envVars
// (issue #2235); only the cache redirect is session-derived and therefore
// injected here. The bwrap equivalent lives in bwrap.go BuildArgs
// (KUBECACHEDIR=/tmp/kube-cache — the per-session tmpfs).
func SessionWorkDirKubeEnv(sessionDir string) []string {
	return []string{
		"KUBECACHEDIR=" + SessionWorkDirKubeCacheDirPath(sessionDir),
	}
}

// SessionWorkDirChromiumDirs returns the chromium Library skeleton
// directories inside the given session work dir (issue #2247, Step 4 of
// #2132):
//
//	<sessionDir>/Library/Application Support/Google
//	<sessionDir>/Library/Caches/Google
//
// The sandbox-exec dispatcher sets CFFIXED_USER_HOME=<sessionDir>, which
// redirects CoreFoundation's NSHomeDirectory() — chromium (Google Chrome
// for Testing, invoked via playwright-cli) derives its user-data and cache
// roots from it and writes its crash database, code cache, profile, and
// SingletonLock under these directories. They must be real directories,
// never symlinks to the host ~/Library/Application Support/Google/ — that
// path holds the daily-driver Chrome profile (cookies, sessions, password
// store) and exposing it to a sandboxed chromium would be a material
// confidentiality leak.
//
// No dedicated SBPL rule exists for the skeleton: writes ride the existing
// (subpath <sessionDir>) RW allow in the profile (§6).
func SessionWorkDirChromiumDirs(sessionDir string) []string {
	return []string{
		filepath.Join(sessionDir, "Library", "Application Support", "Google"),
		filepath.Join(sessionDir, "Library", "Caches", "Google"),
	}
}

// SessionWorkDirGitEnv returns the env var pairs (K=V form) that point git
// and ssh inside the sandbox at the generated work-dir configs:
//
//	GIT_CONFIG_GLOBAL=<sessionDir>/gitconfig
//	GIT_SSH_COMMAND=<sshBin> -F <sessionDir>/ssh-config
//
// sshBin should be cfg.SshBin (the Nix-built openssh binary); when empty,
// "ssh" (PATH resolution) is used as the fallback.
//
// Known gap, accepted in #2132 Step 2: libgit2/go-git-class tools ignore
// GIT_CONFIG_GLOBAL and fall back to $HOME/XDG-derived git config inside
// the sandbox. Those readers do not need the generated identity/signing
// config — reads work without any global gitconfig — so the fallback is
// benign (acceptance probe: `nix flake metadata` in-sandbox).
func SessionWorkDirGitEnv(sessionDir, sshBin string) []string {
	if sshBin == "" {
		sshBin = "ssh"
	}
	return []string{
		"GIT_CONFIG_GLOBAL=" + SessionWorkDirGitconfigPath(sessionDir),
		"GIT_SSH_COMMAND=" + sshBin + " -F " + SessionWorkDirSshConfigPath(sessionDir),
	}
}

// PrepareSessionWorkDir creates the per-session work directory and writes
// the generated ssh-config, gitconfig, and (when signing is configured)
// allowed_signers files into it. Returns the absolute path to the work dir.
//
// All failures are hard errors: the work-dir configs are the only route to
// git identity and ssh auth inside a sandbox-exec session, so a session must
// not start without them. In particular, a missing git identity surfaces the
// writeGitconfigArtefacts hard error from issue #1960 (refuse to start a
// session without [user] in the gitconfig).
//
// Calling PrepareSessionWorkDir a second time is idempotent: the directory
// creation is a no-op and the generated files are overwritten.
func (m *Manager) PrepareSessionWorkDir() (string, error) {
	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return "", fmt.Errorf("container: session work dir: create %s: %w", sessionDir, err)
	}

	// ── Chromium Library skeleton (issue #2247, Step 4 of #2132) ────────────
	// Darwin-only: CFFIXED_USER_HOME is a CoreFoundation mechanism, so the
	// skeleton is meaningless on other platforms. Failures are logged but do
	// not fail session prep — chromium/playwright-cli is an optional
	// capability, unlike the git/ssh configs below (which are hard errors).
	// MkdirAll is idempotent: existing contents from a prior re-spawn are
	// preserved.
	if runtime.GOOS == "darwin" {
		for _, d := range SessionWorkDirChromiumDirs(sessionDir) {
			if err := os.MkdirAll(d, 0o700); err != nil {
				log.Printf("container: session work dir: create chromium skeleton dir %s: %v", d, err)
			}
		}
	}

	if err := m.writeSshConfigToDir(sessionDir); err != nil {
		return "", fmt.Errorf("container: session work dir: %w", err)
	}
	if err := m.writeGitconfigToDir(sessionDir); err != nil {
		return "", fmt.Errorf("container: session work dir: %w", err)
	}
	return sessionDir, nil
}

// writeSshConfigToDir writes the generated ssh config to
// <sessionDir>/ssh-config. The IdentityFile is the STABLE sops symlink path
// ~/.ssh/<SshAccessKeyName> — deliberately not resolved via
// filepath.EvalSymlinks, so the embedded path survives sops secrets.d/<N>
// rotation (issues #1410/#1573):
//
//	~/.ssh/<accessKeyName>                              (stable sops symlink)
//	  → /private/var/folders/.../secrets.d/<current>/…  (sops re-targets this)
//
// The SBPL profile grants (allow file-read* (subpath "/private/var/folders"))
// so the sandbox can follow the chain to whatever secrets.d/<N> is current,
// even after a rotation that occurred after session spawn.
//
// ssh is pointed at this file via `-F` in GIT_SSH_COMMAND (see
// SessionWorkDirGitEnv); openssh resolves its default UserKnownHostsFile
// against the real home (getpwuid → pw_dir, not $HOME), which is why the
// SBPL profile carries a literal read-only grant for ~/.ssh/known_hosts.
func (m *Manager) writeSshConfigToDir(sessionDir string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return fmt.Errorf("write ssh-config: cannot determine user home directory")
	}
	accessKeyName := m.cfg.SshAccessKeyName
	if accessKeyName == "" {
		accessKeyName = "prismatic-koi-ed25519"
	}
	identityFile := filepath.Join(home, ".ssh", accessKeyName)
	sshConfig := "Host github.com\n" +
		"  StrictHostKeyChecking accept-new\n" +
		"  IdentityFile " + identityFile + "\n" +
		"  IdentitiesOnly yes\n"
	dst := SessionWorkDirSshConfigPath(sessionDir)
	_ = os.Remove(dst) // idempotent: replace any prior file
	if err := os.WriteFile(dst, []byte(sshConfig), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

// writeGitconfigToDir generates the gitconfig and (when signing is
// available) allowed_signers files into the session work dir:
//
//	<sessionDir>/gitconfig
//	<sessionDir>/allowed_signers   (when signing is configured)
//
// It is a thin wrapper around writeGitconfigArtefacts — the canonical
// generator also used by the per-mode writeGitconfig — with:
//
//   - user.signingKey embedding the STABLE sops symlink path
//     ~/.ssh/<SshSigningKeyName>.pub (never the EvalSymlinks-resolved
//     secrets.d/<N> path, which rotates — #1410/#1573 — and never a
//     staging-HOME path).
//   - gpg.ssh.allowedSignersFile embedding <sessionDir>/allowed_signers.
//
// Note (#2132): this deliberately breaks the former "uniform generic
// key-path layout across isolation modes" property — sandbox-exec sessions
// now see the real host key names rather than the generic signing-key.pub
// alias. bwrap keeps its existing generic-name bind-mount layout.
func (m *Manager) writeGitconfigToDir(sessionDir string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return fmt.Errorf("write gitconfig: cannot determine user home directory")
	}
	signingKeyName := m.cfg.SshSigningKeyName
	if signingKeyName == "" {
		signingKeyName = "prismatic-koi-ed25519-signingkey"
	}
	return m.writeGitconfigArtefacts(
		filepath.Join(home, ".ssh", signingKeyName+".pub"),
		SessionWorkDirAllowedSignersPath(sessionDir),
		SessionWorkDirGitconfigPath(sessionDir),
		SessionWorkDirAllowedSignersPath(sessionDir),
	)
}

// RemoveSessionWorkDir removes the session work directory tree for the
// given instance ID. It is idempotent — calling it when the directory does
// not exist is a no-op.
//
// Called from cmd/cleanup.go so the generated work-dir configs do not
// accumulate after sessions end. Any legacy staging-HOME remnant from a
// pre-Step-5-of-#2132 session (nested at <sessionDir>/home/) is swept too.
func RemoveSessionWorkDir(instanceID string) {
	sessionDir, err := SessionWorkDirPath(instanceID)
	if err != nil {
		return // can't derive path — nothing to do
	}
	_ = os.RemoveAll(sessionDir)
}
