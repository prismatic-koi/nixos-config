package container

// session_work_dir_test.go — unit tests for the per-session work dir
// (issue #2213, Step 2 of #2132): path layout, generated-config content,
// stable sops symlink-path embedding (the #1410/#1573 rotation property),
// env-var wiring, and SBPL profile rules.
//
// Darwin-only integration coverage (real /usr/bin/sandbox-exec, positive +
// profile-mutation negative pairs) lives in
// internal/integration/sandbox_exec_session_work_dir_darwin_test.go per
// docs/sandbox-exec-testing.md.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSessionWorkDirPath_Layout verifies the work dir path shape
// (~/.local/state/prism/sessions/<instance_id>/ — no home/ suffix, no
// symlinks) and the invariant that the sandbox-exec staging HOME is nested
// directly under it at <sessionDir>/home.
func TestSessionWorkDirPath_Layout(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	const instanceID = "work-dir-layout-test"
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  instanceID,
	})

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}
	want := filepath.Join(fakeHome, ".local", "state", "prism", "sessions", instanceID)
	if sessionDir != want {
		t.Errorf("sessionWorkDirPath = %q, want %q", sessionDir, want)
	}

	// The exported method and package-level function must agree.
	exported, err := m.SessionWorkDir()
	if err != nil || exported != sessionDir {
		t.Errorf("SessionWorkDir() = (%q, %v), want (%q, nil)", exported, err, sessionDir)
	}
	pkgLevel, err := SessionWorkDirPath(instanceID)
	if err != nil || pkgLevel != sessionDir {
		t.Errorf("SessionWorkDirPath(%q) = (%q, %v), want (%q, nil)", instanceID, pkgLevel, err, sessionDir)
	}

}

// TestSessionWorkDirPath_EmptyInstanceIDFallsBackToName: tests that
// construct a Manager without a full spawn lifecycle get a name-derived
// work dir rather than an error.
func TestSessionWorkDirPath_EmptyInstanceIDFallsBackToName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newSandboxExecManager(Config{SessionName: "repo@feat"})

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}
	if !strings.Contains(sessionDir, m.name) {
		t.Errorf("work dir %q does not contain the session-derived name %q", sessionDir, m.name)
	}
}

// TestPrepareSessionWorkDir_WritesConfigsWithStablePaths is the core content
// assertion for the AC: the generated gitconfig, ssh-config, and
// allowed_signers live under ~/.local/state/prism/sessions/<id>/ and contain
// no occurrence of the staging-HOME path or any secrets.d/<N> path — only
// stable ~/.ssh/<keyname> paths and work-dir paths.
func TestPrepareSessionWorkDir_WritesConfigsWithStablePaths(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "work-dir-content-test",
	})

	sessionDir, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("PrepareSessionWorkDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })

	// The legacy staging-HOME path (deleted in Step 5 of #2132) — generated
	// configs must never reference anything under it.
	legacyStagingHome := filepath.Join(sessionDir, "home")

	// ── ssh-config ────────────────────────────────────────────────────────
	sshConfigBytes, err := os.ReadFile(SessionWorkDirSshConfigPath(sessionDir))
	if err != nil {
		t.Fatalf("read generated ssh-config: %v", err)
	}
	sshConfig := string(sshConfigBytes)
	// IdentityFile must be the STABLE sops symlink path (default key name).
	wantIdentity := "IdentityFile " + filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519")
	if !strings.Contains(sshConfig, wantIdentity+"\n") {
		t.Errorf("ssh-config missing %q; content:\n%s", wantIdentity, sshConfig)
	}

	// ── gitconfig ─────────────────────────────────────────────────────────
	gitconfigBytes, err := os.ReadFile(SessionWorkDirGitconfigPath(sessionDir))
	if err != nil {
		t.Fatalf("read generated gitconfig: %v", err)
	}
	gitconfig := string(gitconfigBytes)
	for _, want := range []string{
		"[user]",
		"name = test-user",
		"email = test@example.com",
		// signingKey must be the STABLE sops symlink path (newFakeHome
		// creates the default-named signing key pair, so signing is on).
		"signingKey = " + filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519-signingkey.pub"),
		// allowedSignersFile must point into the work dir.
		"allowedSignersFile = " + SessionWorkDirAllowedSignersPath(sessionDir),
		"gpgsign = true",
	} {
		if !strings.Contains(gitconfig, want) {
			t.Errorf("gitconfig missing %q; content:\n%s", want, gitconfig)
		}
	}

	// ── allowed_signers ───────────────────────────────────────────────────
	signersBytes, err := os.ReadFile(SessionWorkDirAllowedSignersPath(sessionDir))
	if err != nil {
		t.Fatalf("read generated allowed_signers: %v", err)
	}
	if !strings.HasPrefix(string(signersBytes), "test@example.com ") {
		t.Errorf("allowed_signers does not start with the git email; content: %q", string(signersBytes))
	}

	// ── Forbidden path classes (the load-bearing AC assertion) ───────────
	for name, content := range map[string]string{
		"ssh-config":      sshConfig,
		"gitconfig":       gitconfig,
		"allowed_signers": string(signersBytes),
	} {
		if strings.Contains(content, legacyStagingHome) {
			t.Errorf("%s embeds the legacy staging-HOME path %q — must use stable host paths; content:\n%s",
				name, legacyStagingHome, content)
		}
		if strings.Contains(content, "secrets.d") {
			t.Errorf("%s embeds a resolved secrets.d path — breaks on sops rotation (#1410/#1573); content:\n%s",
				name, content)
		}
	}
}

// TestPrepareSessionWorkDir_CustomKeyNamesHonoured verifies that
// cfg.SshAccessKeyName / cfg.SshSigningKeyName override the default key
// names in the embedded paths.
func TestPrepareSessionWorkDir_CustomKeyNamesHonoured(t *testing.T) {
	fakeHome := newFakeHome(t)

	// Create the custom-named keys in the fake ~/.ssh.
	for _, f := range []string{"custom-access", "custom-signing", "custom-signing.pub"} {
		if err := os.WriteFile(filepath.Join(fakeHome, ".ssh", f), []byte("dummy"), 0o600); err != nil {
			t.Fatalf("write custom key %s: %v", f, err)
		}
	}

	m := newSandboxExecManagerWithInstance(Config{
		SessionName:       "repo@feat",
		InstanceID:        "work-dir-custom-keys-test",
		SshAccessKeyName:  "custom-access",
		SshSigningKeyName: "custom-signing",
	})

	sessionDir, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("PrepareSessionWorkDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })

	sshConfig, err := os.ReadFile(SessionWorkDirSshConfigPath(sessionDir))
	if err != nil {
		t.Fatalf("read ssh-config: %v", err)
	}
	if want := filepath.Join(fakeHome, ".ssh", "custom-access"); !strings.Contains(string(sshConfig), want) {
		t.Errorf("ssh-config does not embed custom access key path %q; content:\n%s", want, sshConfig)
	}

	gitconfig, err := os.ReadFile(SessionWorkDirGitconfigPath(sessionDir))
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	if want := filepath.Join(fakeHome, ".ssh", "custom-signing.pub"); !strings.Contains(string(gitconfig), want) {
		t.Errorf("gitconfig does not embed custom signing key path %q; content:\n%s", want, gitconfig)
	}
}

// TestPrepareSessionWorkDir_SurvivesSopsRotation pins the #1410/#1573
// two-hop property at the unit level: the embedded paths must point at the
// stable ~/.ssh/<keyname> symlink, so that after a simulated
// secrets.d/<N> → secrets.d/<N+1> rotation the embedded path still resolves
// to live content. (The Darwin integration suite proves the in-sandbox read
// rides the /private/var/folders allow; this test proves we never embed the
// concrete rotating path in the first place.)
func TestPrepareSessionWorkDir_SurvivesSopsRotation(t *testing.T) {
	fakeHome := newFakeHome(t)
	sshDir := filepath.Join(fakeHome, ".ssh")

	// Simulate the sops layout: concrete keys live in secrets.d/<N>/ and
	// ~/.ssh/<keyname> is a stable symlink to the current generation.
	secretsV1 := filepath.Join(fakeHome, "secrets.d", "1")
	if err := os.MkdirAll(secretsV1, 0o700); err != nil {
		t.Fatalf("mkdir secrets.d/1: %v", err)
	}
	for _, k := range []string{"prismatic-koi-ed25519", "prismatic-koi-ed25519-signingkey", "prismatic-koi-ed25519-signingkey.pub"} {
		concrete := filepath.Join(secretsV1, k)
		if err := os.WriteFile(concrete, []byte("v1-"+k), 0o600); err != nil {
			t.Fatalf("write concrete key: %v", err)
		}
		// Replace the regular files newFakeHome created with symlinks into
		// secrets.d/1 (the realistic sops layout).
		link := filepath.Join(sshDir, k)
		_ = os.Remove(link)
		if err := os.Symlink(concrete, link); err != nil {
			t.Fatalf("symlink %s: %v", link, err)
		}
	}

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "work-dir-rotation-test",
	})
	sessionDir, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("PrepareSessionWorkDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })

	sshConfig, err := os.ReadFile(SessionWorkDirSshConfigPath(sessionDir))
	if err != nil {
		t.Fatalf("read ssh-config: %v", err)
	}
	gitconfig, err := os.ReadFile(SessionWorkDirGitconfigPath(sessionDir))
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}

	// The concrete secrets.d path must NOT be embedded anywhere.
	for name, content := range map[string]string{"ssh-config": string(sshConfig), "gitconfig": string(gitconfig)} {
		if strings.Contains(content, "secrets.d") {
			t.Fatalf("%s embeds a concrete secrets.d path — would dangle after rotation; content:\n%s", name, content)
		}
	}

	// Simulate a darwin-rebuild switch: rotate secrets.d/1 → secrets.d/2 and
	// re-target the stable symlinks. The embedded paths (written BEFORE the
	// rotation) must still resolve to the new generation's content.
	secretsV2 := filepath.Join(fakeHome, "secrets.d", "2")
	if err := os.MkdirAll(secretsV2, 0o700); err != nil {
		t.Fatalf("mkdir secrets.d/2: %v", err)
	}
	for _, k := range []string{"prismatic-koi-ed25519", "prismatic-koi-ed25519-signingkey", "prismatic-koi-ed25519-signingkey.pub"} {
		concrete := filepath.Join(secretsV2, k)
		if err := os.WriteFile(concrete, []byte("v2-"+k), 0o600); err != nil {
			t.Fatalf("write v2 concrete key: %v", err)
		}
		link := filepath.Join(sshDir, k)
		_ = os.Remove(link)
		if err := os.Symlink(concrete, link); err != nil {
			t.Fatalf("re-target symlink %s: %v", link, err)
		}
	}
	if err := os.RemoveAll(secretsV1); err != nil {
		t.Fatalf("remove secrets.d/1: %v", err)
	}

	// Extract the embedded IdentityFile path and read through it.
	var identityFile string
	for _, line := range strings.Split(string(sshConfig), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "IdentityFile "); ok {
			identityFile = rest
		}
	}
	if identityFile == "" {
		t.Fatalf("ssh-config has no IdentityFile line; content:\n%s", sshConfig)
	}
	got, err := os.ReadFile(identityFile)
	if err != nil {
		t.Fatalf("embedded IdentityFile %q does not resolve after rotation: %v", identityFile, err)
	}
	if string(got) != "v2-prismatic-koi-ed25519" {
		t.Errorf("embedded IdentityFile resolves to %q after rotation, want the v2 content", string(got))
	}
}

// TestPrepareSessionWorkDir_Idempotent verifies that a second call succeeds
// and overwrites the generated files in place.
func TestPrepareSessionWorkDir_Idempotent(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "work-dir-idempotent-test",
	})

	first, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("first PrepareSessionWorkDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(first) })

	second, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("second PrepareSessionWorkDir: %v", err)
	}
	if first != second {
		t.Errorf("work dir path changed between calls: %q vs %q", first, second)
	}
	if _, err := os.Stat(SessionWorkDirGitconfigPath(second)); err != nil {
		t.Errorf("gitconfig missing after second call: %v", err)
	}
}

// TestRemoveSessionWorkDir verifies removal of the work dir tree, including
// any legacy staging-HOME remnant nested under it (pre-Step-5-of-#2132
// sessions) and (on Darwin) the chromium Library skeleton with any
// per-session chromium state inside it — the edge-case AC for issue #2247:
// session cleanup keeps chromium prefs/state ephemeral.
func TestRemoveSessionWorkDir(t *testing.T) {
	newFakeHome(t)

	const instanceID = "work-dir-remove-test"
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  instanceID,
	})

	sessionDir, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("PrepareSessionWorkDir: %v", err)
	}
	// Simulate a legacy staging-HOME remnant from a pre-Step-5 session —
	// nothing creates this dir any more, but cleanup of old sessions must
	// still sweep it.
	legacyStagingHome := filepath.Join(sessionDir, "home")
	if err := os.MkdirAll(filepath.Join(legacyStagingHome, ".ssh"), 0o700); err != nil {
		t.Fatalf("plant legacy staging remnant: %v", err)
	}

	// Plant per-session chromium state inside the skeleton (Darwin-only —
	// the skeleton is only created there) so the removal assertion covers
	// populated chromium dirs, not just empty ones.
	var chromiumState string
	if runtime.GOOS == "darwin" {
		chromiumState = filepath.Join(sessionDir, "Library", "Application Support", "Google",
			"Chrome for Testing", "prism-2247-cleanup-sentinel")
		if err := os.MkdirAll(filepath.Dir(chromiumState), 0o700); err != nil {
			t.Fatalf("mkdir chromium state dir: %v", err)
		}
		if err := os.WriteFile(chromiumState, []byte("ephemeral"), 0o600); err != nil {
			t.Fatalf("plant chromium state sentinel: %v", err)
		}
	}

	RemoveSessionWorkDir(instanceID)

	if _, err := os.Stat(sessionDir); err == nil {
		t.Errorf("session work dir still exists after RemoveSessionWorkDir: %s", sessionDir)
	}
	if _, err := os.Stat(legacyStagingHome); err == nil {
		t.Errorf("legacy staging-HOME remnant still exists after RemoveSessionWorkDir: %s", legacyStagingHome)
	}
	if chromiumState != "" {
		if _, err := os.Stat(chromiumState); err == nil {
			t.Errorf("chromium per-session state still exists after RemoveSessionWorkDir: %s", chromiumState)
		}
	}

	// Idempotent: a second removal is a no-op.
	RemoveSessionWorkDir(instanceID)
}

// TestSessionWorkDirChromiumDirs pins the skeleton path shape (issue #2247,
// Step 4 of #2132): exactly the two Google dirs CF-derived chromium writes
// under, both inside the session work dir.
func TestSessionWorkDirChromiumDirs(t *testing.T) {
	const dir = "/Users/u/.local/state/prism/sessions/abc"

	got := SessionWorkDirChromiumDirs(dir)
	want := []string{
		dir + "/Library/Application Support/Google",
		dir + "/Library/Caches/Google",
	}
	if len(got) != len(want) {
		t.Fatalf("SessionWorkDirChromiumDirs returned %d dirs, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SessionWorkDirChromiumDirs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestPrepareSessionWorkDir_ChromiumLibrarySkeleton is the work-dir half of
// the issue #2247 AC: after session prep, the two Google skeleton dirs
// exist inside the session work dir as real directories (never symlinks —
// a symlink to the host ~/Library/Application Support/Google/ would leak
// the daily-driver Chrome profile), and a second prep call preserves any
// chromium state written in between (re-spawn idempotency).
func TestPrepareSessionWorkDir_ChromiumLibrarySkeleton(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the chromium skeleton is Darwin-only (CFFIXED_USER_HOME is a CoreFoundation mechanism)")
	}
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "chromium-workdir-skeleton-test",
	})

	sessionDir, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("PrepareSessionWorkDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })

	for _, d := range SessionWorkDirChromiumDirs(sessionDir) {
		info, statErr := os.Lstat(d)
		if statErr != nil {
			t.Errorf("expected chromium skeleton dir %q to exist after session prep: %v", d, statErr)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("chromium skeleton dir %q must be a real directory, not a symlink: mode=%v", d, info.Mode())
		}
		if !info.IsDir() {
			t.Errorf("chromium skeleton dir %q must be a directory: mode=%v", d, info.Mode())
		}
	}

	// Idempotency: plant a sentinel under the skeleton and re-prep. The
	// sentinel must survive (MkdirAll is a no-op on existing dirs; chromium
	// state from a prior spawn is preserved across re-spawns).
	sentinelDir := filepath.Join(sessionDir, "Library", "Application Support", "Google", "Chrome for Testing")
	if err := os.MkdirAll(sentinelDir, 0o700); err != nil {
		t.Fatalf("mkdir sentinel dir: %v", err)
	}
	sentinelPath := filepath.Join(sentinelDir, "prism-2247-idempotency-sentinel")
	const sentinelData = "prism-2247-idempotency"
	if err := os.WriteFile(sentinelPath, []byte(sentinelData), 0o600); err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}

	second, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("second PrepareSessionWorkDir (idempotency): %v", err)
	}
	if second != sessionDir {
		t.Errorf("session work dir changed across calls: %q -> %q", sessionDir, second)
	}
	got, readErr := os.ReadFile(sentinelPath)
	if readErr != nil {
		t.Errorf("sentinel file disappeared after second PrepareSessionWorkDir: %v", readErr)
	} else if string(got) != sentinelData {
		t.Errorf("sentinel file contents clobbered: got %q, want %q", string(got), sentinelData)
	}
}

// TestGenerateProfile_NoHostLibraryRulesForChromium is the profile
// diff-level AC for issue #2247: Step 4 adds NO new SBPL rules. The
// chromium skeleton rides the existing (subpath <sessionDir>) RW allow, so
// the profile must contain no rule referencing the user's real ~/Library
// (in particular not ~/Library/Application Support/Google — the
// daily-driver Chrome profile) while the system /Library allow and the
// session work dir rule are unchanged.
func TestGenerateProfile_NoHostLibraryRulesForChromium(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "chromium-profile-test",
		Worktree:    t.TempDir(),
	})

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}

	profile := generateProfile(m)

	// No rule may reference the host home Library subtree. Substring check
	// on the unquoted path prefix catches (subpath ...), (literal ...), and
	// (regex ...) forms alike. The sessionDir rule cannot false-positive
	// here: <sessionDir>/Library is never emitted as a rule (the skeleton
	// deliberately has no dedicated rule), and the quoted sessionDir path
	// itself does not contain "Library".
	if hostLibrary := filepath.Join(fakeHome, "Library"); strings.Contains(profile, hostLibrary) {
		t.Errorf("profile references the host home Library %q — #2247 must add no host-Library grants; full profile:\n%s",
			hostLibrary, profile)
	}

	// Sanity: the assertion above is about ~/Library, not the system
	// /Library allow, which must remain.
	if !strings.Contains(profile, "(subpath \"/Library\")") {
		t.Errorf("system (subpath \"/Library\") allow missing — the no-host-Library assertion is checking the wrong thing; full profile:\n%s", profile)
	}

	// The capability the skeleton rides: the existing session work dir rule.
	if want := "(subpath " + quoteSBPL(sessionDir) + ")"; !strings.Contains(profile, want) {
		t.Errorf("profile missing the session work dir rule %q that the chromium skeleton rides; full profile:\n%s", want, profile)
	}
}

// TestSessionWorkDirGitEnv verifies the env-var pairs the dispatcher injects
// (AC: the sandbox env carries GIT_CONFIG_GLOBAL=<sessionDir>/gitconfig and
// GIT_SSH_COMMAND referencing <sessionDir>/ssh-config).
func TestSessionWorkDirGitEnv(t *testing.T) {
	const dir = "/Users/u/.local/state/prism/sessions/abc"

	got := SessionWorkDirGitEnv(dir, "/nix/store/xyz/bin/ssh")
	want := []string{
		"GIT_CONFIG_GLOBAL=" + dir + "/gitconfig",
		"GIT_SSH_COMMAND=/nix/store/xyz/bin/ssh -F " + dir + "/ssh-config",
	}
	if len(got) != len(want) {
		t.Fatalf("SessionWorkDirGitEnv returned %d vars, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SessionWorkDirGitEnv[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Empty sshBin falls back to PATH-resolved "ssh".
	gotDefault := SessionWorkDirGitEnv(dir, "")
	if gotDefault[1] != "GIT_SSH_COMMAND=ssh -F "+dir+"/ssh-config" {
		t.Errorf("SessionWorkDirGitEnv with empty sshBin = %q, want PATH-resolved ssh", gotDefault[1])
	}
}

// TestSessionWorkDirKubeEnv verifies the kubectl cache redirect the
// dispatcher injects (issue #2235, Step 3b of #2132): the sandbox env
// carries KUBECACHEDIR=<sessionDir>/kube-cache so kubectl's discovery/http
// cache lands inside the session work dir (already RW-granted in the SBPL
// profile) instead of the host's ~/.kube/cache.
func TestSessionWorkDirKubeEnv(t *testing.T) {
	const dir = "/Users/u/.local/state/prism/sessions/abc"

	got := SessionWorkDirKubeEnv(dir)
	want := []string{"KUBECACHEDIR=" + dir + "/kube-cache"}
	if len(got) != len(want) {
		t.Fatalf("SessionWorkDirKubeEnv returned %d vars, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SessionWorkDirKubeEnv[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if p := SessionWorkDirKubeCacheDirPath(dir); p != dir+"/kube-cache" {
		t.Errorf("SessionWorkDirKubeCacheDirPath = %q, want %q", p, dir+"/kube-cache")
	}
}

// TestGenerateProfile_SessionWorkDirAndKnownHostsRules pins the SBPL profile
// AC: the profile contains (subpath "<sessionDir>") and a read-only literal
// for ~/.ssh/known_hosts, and contains no (subpath "<HOME>/.ssh") — the
// real ~/.ssh may hold non-sops private keys that must stay unreadable.
func TestGenerateProfile_SessionWorkDirAndKnownHostsRules(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "work-dir-profile-test",
		Worktree:    t.TempDir(),
	})

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}

	profile := generateProfile(m)

	// RW subpath for the session work dir (covers the nested staging HOME).
	if want := "(subpath " + quoteSBPL(sessionDir) + ")"; !strings.Contains(profile, want) {
		t.Errorf("profile missing the session work dir rule %q; full profile:\n%s", want, profile)
	}

	// Read-only literal for the real known_hosts.
	knownHostsLiteral := "(literal " + quoteSBPL(filepath.Join(fakeHome, ".ssh", "known_hosts")) + ")"
	if !strings.Contains(profile, knownHostsLiteral) {
		t.Errorf("profile missing the known_hosts literal %q; full profile:\n%s", knownHostsLiteral, profile)
	}
	// The known_hosts grant must be read-only: locate its allow block and
	// assert it carries no file-write*.
	idx := strings.Index(profile, knownHostsLiteral)
	blockStart := strings.LastIndex(profile[:idx], "(allow ")
	if blockStart == -1 {
		t.Fatalf("cannot locate the allow block containing the known_hosts literal")
	}
	block := profile[blockStart : idx+len(knownHostsLiteral)]
	if strings.Contains(block, "file-write") {
		t.Errorf("known_hosts allow block grants write access — must be read-only:\n%s", block)
	}

	// NEVER (subpath ~/.ssh).
	if forbidden := "(subpath " + quoteSBPL(filepath.Join(fakeHome, ".ssh")) + ")"; strings.Contains(profile, forbidden) {
		t.Errorf("profile contains forbidden rule %q — it would cover future non-sops private keys; full profile:\n%s",
			forbidden, profile)
	}
}

// TestSandboxExecWriteGitconfig_IsolatorWritesWorkDirGitconfig verifies that
// the Isolator-interface WriteGitconfig for sandbox-exec produces the
// work-dir gitconfig (post-#2213 there is no staging-HOME gitconfig).
func TestSandboxExecWriteGitconfig_IsolatorWritesWorkDirGitconfig(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "work-dir-isolator-gitconfig-test",
	})

	iso := &sandboxExecIsolator{name: m.name}
	if err := iso.WriteGitconfig(m); err != nil {
		t.Fatalf("WriteGitconfig: %v", err)
	}
	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })

	content, err := os.ReadFile(SessionWorkDirGitconfigPath(sessionDir))
	if err != nil {
		t.Fatalf("read work-dir gitconfig after WriteGitconfig: %v", err)
	}
	if !strings.Contains(string(content), "[user]") {
		t.Errorf("work-dir gitconfig missing [user] section; content:\n%s", content)
	}
}
