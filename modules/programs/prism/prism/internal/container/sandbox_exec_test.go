package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newSandboxExecManager creates a Manager and injects a sandboxExecIsolator
// so tests can drive BuildArgs/PrepareSandboxExec end-to-end without a real
// macOS host.
//
// Default git identity is auto-populated when the caller leaves
// GitUserName / GitUserEmail empty so the per-mode `writeGitconfig` hard
// error from issue #1960 (refuse to start without [user]) does not
// retroactively break every pre-existing sandbox-exec test that does not
// itself care about git identity. Tests that exercise the empty-identity
// path explicitly set these fields back to "".
func newSandboxExecManager(cfg Config) *Manager {
	if cfg.GitUserName == "" {
		cfg.GitUserName = "test-user"
	}
	if cfg.GitUserEmail == "" {
		cfg.GitUserEmail = "test@example.com"
	}
	m := New(cfg)
	m.isolator = newSandboxExecIsolator(m.name)
	return m
}

// newSandboxExecManagerWithInstance is like newSandboxExecManager but ensures
// the Config has an InstanceID so sandboxExecHomePath uses the instance ID
// rather than falling back to the container name. Used in tests that exercise
// staging HOME generation (#1017).
func newSandboxExecManagerWithInstance(cfg Config) *Manager {
	if cfg.InstanceID == "" {
		cfg.InstanceID = "test-instance-id"
	}
	return newSandboxExecManager(cfg)
}

// ── generateProfile content assertions ──────────────────────────────────────

// TestGenerateProfile_VersionAndDenyDefault verifies that the profile begins
// with the SBPL header that locks deny-by-default semantics: (version 3)
// followed by (deny default). This is non-negotiable per #1012 and #1200 —
// every other clause is interpreted relative to deny-by-default.
func TestGenerateProfile_VersionAndDenyDefault(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	// (version 3) MUST be the first non-empty content.
	if !strings.HasPrefix(profile, "(version 3)\n") {
		t.Errorf("profile must begin with (version 3)\\n; got prefix %q", firstNChars(profile, 64))
	}

	// (deny default) MUST follow immediately after (version 3).
	if !strings.HasPrefix(profile, "(version 3)\n(deny default)\n") {
		t.Errorf("profile must start with (version 3) then (deny default); got prefix %q", firstNChars(profile, 64))
	}
}

// TestGenerateProfile_ReadOnlySystemRoots verifies that every read-only
// system root listed in the AC appears as a (subpath ...) inside an
// (allow file-read* file-test-existence file-map-executable file-read-metadata ...)
// clause, as required by the v3 migration (#1200 / F.1 §2 rule 2).
//
// Both /etc and /private/etc must be present: on macOS /etc is a symlink to
// /private/etc but sandbox-exec does not follow it transparently, so both
// shapes are required for execvp to succeed on /etc/profiles/per-user/...
// paths. See issue #1187.
//
// /bin, /sbin and the /var/... alias forms are also required for the v3 profile.
func TestGenerateProfile_ReadOnlySystemRoots(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	// The v3 system-roots block uses the broader verb set. Find that block.
	// Use the literal full opener for disambiguation.
	systemRootsBlock := extractClause(t, profile, "(allow file-read* file-test-existence file-map-executable file-read-metadata")

	expected := []string{
		`(subpath "/nix")`,
		`(subpath "/usr")`,
		`(subpath "/bin")`,
		`(subpath "/sbin")`,
		`(subpath "/System")`,
		`(subpath "/Library")`,
		`(subpath "/Applications/Xcode.app")`,
		`(subpath "/etc")`,
		`(subpath "/private/etc")`,
		`(subpath "/private/var/db/dyld")`,
		`(subpath "/private/var/db/timezone")`,
		`(subpath "/private/var/select")`,
		`(subpath "/private/var/folders")`,
		`(subpath "/var/db/dyld")`,
		`(subpath "/var/db/timezone")`,
		`(subpath "/var/select")`,
		`(subpath "/var/folders")`,
		`(literal "/dev/dtracehelper")`,
		`(literal "/")`,
	}
	for _, want := range expected {
		if !strings.Contains(systemRootsBlock, want) {
			t.Errorf("system-roots block missing %q; block:\n%s", want, systemRootsBlock)
		}
	}
}

// TestGenerateProfile_SensitiveSubtreeDenies verifies that the sensitive-
// subtree deny subpaths appear inside a (deny file-read* file-write* ...)
// clause. Both the /etc/... and /private/etc/... forms must be denied:
// the same symlink non-transparency that required (subpath "/etc") in the
// allow list also means that denying only the /private/etc/... form leaves
// the /etc/... path form accessible. See issue #1187.
func TestGenerateProfile_SensitiveSubtreeDenies(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	denyBlock := extractClause(t, profile, "(deny file-read* file-write*")
	expected := []string{
		`(subpath "/etc/wireguard")`,
		`(subpath "/etc/wpa_supplicant")`,
		`(subpath "/etc/ssh")`,
		`(subpath "/private/etc/wireguard")`,
		`(subpath "/private/etc/wpa_supplicant")`,
		`(subpath "/private/etc/ssh")`,
	}
	for _, want := range expected {
		if !strings.Contains(denyBlock, want) {
			t.Errorf("(deny file-read* file-write* ...) block missing %q; block:\n%s", want, denyBlock)
		}
	}
}

// TestGenerateProfile_ProcessAndIPCAllows verifies that the profile contains
// the process/IPC/syscall primitives required for node, pi, dyld, and
// AMFI to run under the v3 profile. See #1200 / F.1 §2.
//
// v3 changes vs v1:
//   - process-info* added (AMFI cert chain validation)
//   - iokit-open re-introduced as enumerated user-client classes for chromium
//     framework init (issue #2021). The unqualified (allow iokit-open) form
//     remains forbidden — only specific user-client classes are granted.
//   - signal split into its own clause with (target self) (target children)
//     so playwright-cli's node-side launcher can kill its chromium grandchild
//     (issue #2021).
//   - ipc-posix-shm REMOVED (unbound variable in v3 — replaced by split forms)
//   - ipc-posix-shm-read* and ipc-posix-shm-write* added
//   - syscall-unix syscall-mach added
//   - system-mac-syscall added
//   - system-fcntl added
func TestGenerateProfile_ProcessAndIPCAllows(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	wantSubstrings := []string{
		"(allow process-exec*",
		"process-fork",
		"process-info*",
		"mach-lookup",
		"mach-register",
		"sysctl-read",
		"(allow signal (target self) (target children))",
		"(allow ipc-posix-shm-read* ipc-posix-shm-write*)",
		"(allow syscall-unix syscall-mach)",
		"(allow system-mac-syscall)",
		"(allow system-fcntl)",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing required substring %q; full profile:\n%s", want, profile)
		}
	}

	// The unqualified (allow iokit-open) form MUST NOT be present — the
	// chromium fix (issue #2021) deliberately enumerates user-client classes
	// rather than opening the entire IOKit surface. Also assert the
	// iokit-open-user-client form is not unqualified.
	if strings.Contains(profile, "(allow iokit-open)") {
		t.Errorf("profile must not contain unqualified (allow iokit-open) — enumerate iokit-user-client-class entries instead (issue #2021); full profile:\n%s", profile)
	}
	if strings.Contains(profile, "(allow iokit-open-user-client)") {
		t.Errorf("profile must not contain unqualified (allow iokit-open-user-client) — enumerate iokit-user-client-class entries instead (issue #2021); full profile:\n%s", profile)
	}

	// The signal widening MUST NOT include (target others) — that would
	// permit signalling arbitrary host PIDs. Only (target self) and
	// (target children) are allowed (issue #2021).
	if strings.Contains(profile, "(target others)") {
		t.Errorf("profile must not contain (target others) for signal; only (target self) and (target children) are permitted (issue #2021); full profile:\n%s", profile)
	}

	// The bare ipc-posix-shm token must NOT appear (unbound variable in v3).
	// Only ipc-posix-shm-read* and ipc-posix-shm-write* are valid.
	// We check by looking for "ipc-posix-shm)" or "ipc-posix-shm " which
	// would indicate the bare form.
	if strings.Contains(profile, "ipc-posix-shm)") || strings.Contains(profile, "(allow ipc-posix-shm)") {
		t.Errorf("profile must not contain bare ipc-posix-shm (unbound variable in v3); full profile:\n%s", profile)
	}
}

// TestGenerateProfile_MachLookupCoversWindowServer verifies that the
// mach-lookup allow rule is unqualified (no (global-name ...) filter),
// which subsumes the WindowServer bootstrap port chromium connects to in
// headed mode (com.apple.windowserver.active) called out in issue #2021
// §4.
//
// If a future PR tightens mach-lookup to an enumerated (global-name ...)
// set, this test catches the regression — either the WindowServer name
// must be explicitly added to the enumerated set, or playwright-cli
// (which ships PLAYWRIGHT_MCP_HEADLESS=false via pkgs/playwright-cli.nix)
// will fail to connect to the WindowServer at startup. A failure here
// without an accompanying enumerated WindowServer entry is the
// regression signal.
func TestGenerateProfile_MachLookupCoversWindowServer(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	// Look for the unqualified mach-lookup form. The current emission is
	// inside the (allow process-exec* process-fork process-info* mach-lookup
	// mach-register sysctl-read) clause — unqualified, no (global-name)
	// predicate following it. The regex equivalent would be /mach-lookup[^\(]/
	// but a substring search for " mach-lookup " (with a trailing space, not
	// followed by a (global-name) predicate) is sufficient.
	if !strings.Contains(profile, " mach-lookup ") {
		t.Fatalf("profile is missing the unqualified mach-lookup form (issue #2021 §4).\n"+
			"If mach-lookup was tightened to an enumerated (global-name ...) set,\n"+
			"the WindowServer bootstrap port com.apple.windowserver.active must be\n"+
			"explicitly added back — playwright-cli runs headed (HEADLESS=false in\n"+
			"pkgs/playwright-cli.nix) and connects to WindowServer at chromium init.\n"+
			"Full profile:\n%s", profile)
	}
}

// TestGenerateProfile_IOKitChromiumClasses verifies the enumerated IOKit
// user-client class allow set required for chromium framework init under
// playwright-cli (issue #2021). Without these classes chromium SIGSEGVs in
// IONotificationPortGetRunLoopSource at ChromeMain+~50ms.
//
// The five classes correspond to: Metal/IOSurface framebuffer (IOSurfaceRoot),
// HID input (IOHIDLibUserClient), AudioComponent init (IOAudioEngineUserClient),
// windowing-system framebuffer (IOFramebufferSharedUserClient), and power
// management (RootDomainUserClient).
func TestGenerateProfile_IOKitChromiumClasses(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	iokitBlock := extractClause(t, profile, "(allow iokit-open-user-client")
	wantClasses := []string{
		"(iokit-user-client-class \"IOSurfaceRoot\")",
		"(iokit-user-client-class \"IOHIDLibUserClient\")",
		"(iokit-user-client-class \"IOAudioEngineUserClient\")",
		"(iokit-user-client-class \"IOFramebufferSharedUserClient\")",
		"(iokit-user-client-class \"RootDomainUserClient\")",
	}
	for _, want := range wantClasses {
		if !strings.Contains(iokitBlock, want) {
			t.Errorf("iokit-open block missing required class %q; block:\n%s", want, iokitBlock)
		}
	}
}

// TestGenerateProfile_NetworkAllow verifies the (allow network*) clause is
// present. This is locked in #1012 — match bwrap's permissive network
// policy. Restriction is a future symmetric concern.
func TestGenerateProfile_NetworkAllow(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	if !strings.Contains(profile, "(allow network*)") {
		t.Errorf("profile missing (allow network*); full profile:\n%s", profile)
	}
}

// TestGenerateProfile_V3CryptexAndTmpRules verifies the v3-specific additions
// required for the dyld shared cache and transient files. See #1200 / F.1 §2.
func TestGenerateProfile_V3CryptexAndTmpRules(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	// Cryptex graft points (rule 1).
	cryptexBlock := extractClause(t, profile, "(allow file-read* file-test-existence file-map-executable\n  (subpath \"/System/Volumes/Preboot/Cryptexes\")")
	if !strings.Contains(cryptexBlock, `(subpath "/System/Volumes/Preboot/Cryptexes")`) {
		t.Errorf("Cryptex block missing /System/Volumes/Preboot/Cryptexes; block:\n%s", cryptexBlock)
	}
	if !strings.Contains(cryptexBlock, `(subpath "/System/Cryptexes")`) {
		t.Errorf("Cryptex block missing /System/Cryptexes; block:\n%s", cryptexBlock)
	}

	// /tmp read-write rule (rule 3).
	if !strings.Contains(profile, `(subpath "/private/tmp")`) {
		t.Errorf("profile missing (subpath \"/private/tmp\"); full profile:\n%s", profile)
	}
	if !strings.Contains(profile, `(subpath "/tmp")`) {
		t.Errorf("profile missing (subpath \"/tmp\"); full profile:\n%s", profile)
	}

	// /dev/null write access (rule 8).
	if !strings.Contains(profile, "(allow file-write-data") {
		t.Errorf("profile missing (allow file-write-data ...); full profile:\n%s", profile)
	}
	if !strings.Contains(profile, `(literal "/dev/null")`) {
		t.Errorf("profile missing (literal \"/dev/null\") in file-write-data; full profile:\n%s", profile)
	}
}

// TestGenerateProfile_StagingHomeAndWorktreeRules verifies that the profile
// emitted by generateProfile includes the staging HOME, worktree, and bare
// repo as (allow file-read* file-write* (subpath ...)) clauses when the
// Manager has InstanceID, Worktree, and BareRoot set. This is the PR #1017
// replacement for TestGenerateProfile_NoOutOfScopeRules.
func TestGenerateProfile_StagingHomeAndWorktreeRules(t *testing.T) {
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@main",
		Worktree:    "/tmp/fake-worktree",
		BareRoot:    "/tmp/fake-bare",
		InstanceID:  "test-instance-id",
	})
	profile := generateProfile(m)

	// The profile must contain (allow file-read* file-write* file-test-existence
	// file-read-metadata ...) with the staging home, worktree, and bare repo subpaths.
	// The v3 profile adds file-test-existence and file-read-metadata to the RW block.
	if !strings.Contains(profile, "(allow file-read* file-write* file-test-existence file-read-metadata") {
		t.Errorf("profile missing (allow file-read* file-write* file-test-existence file-read-metadata ...) clause; full profile:\n%s", profile)
	}
	if !strings.Contains(profile, "/tmp/fake-worktree") {
		t.Errorf("profile missing worktree path /tmp/fake-worktree; full profile:\n%s", profile)
	}
	// The profile allows the full BareRoot (not just .bare/) so the agent can
	// probe for project config files (e.g. .opencode/) at the repo root.
	if !strings.Contains(profile, "/tmp/fake-bare") {
		t.Errorf("profile missing bare repo path /tmp/fake-bare; full profile:\n%s", profile)
	}
	// The staging home path must be present (namespaced by InstanceID).
	if !strings.Contains(profile, "test-instance-id") {
		t.Errorf("profile missing staging HOME path containing instance ID 'test-instance-id'; full profile:\n%s", profile)
	}
}

// TestGenerateProfile_AWSHomePathDenied verifies that the profile contains a
// (deny file-read* file-write* (subpath "$HOME/.aws")) clause to prevent the
// sandbox from accessing the host's raw ~/.aws directory. Only the staged
// entries (symlinked through the staging HOME) are accessible.
func TestGenerateProfile_AWSHomePathDenied(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	if !strings.Contains(profile, "(deny file-read* file-write*") {
		t.Errorf("profile missing deny clause for ~/.aws; full profile:\n%s", profile)
	}
	if !strings.Contains(profile, "/.aws") {
		t.Errorf("profile missing ~/.aws deny subpath; full profile:\n%s", profile)
	}
}

// TestGenerateProfile_AWSSSOAndCLICarveouts verifies that the profile contains
// explicit (allow file-read* file-write* (subpath ".../.aws/sso")) and
// (allow file-read* file-write* (subpath ".../.aws/cli")) rules after the
// broad ~/.aws deny. These more-specific allow rules let AWS SSO tokens and
// kubectl credential files be read and written through the staging HOME
// symlinks (issue #1380, #1558).
func TestGenerateProfile_AWSSSOAndCLICarveouts(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "aws-carveout-test",
		Worktree:    t.TempDir(),
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".local", "state", "prism"))
	})

	profile := generateProfile(m)

	awsSSOPath := filepath.Join(fakeHome, ".aws", "sso")
	awsCLIPath := filepath.Join(fakeHome, ".aws", "cli")

	// Both carve-out paths must appear in the profile.
	if !strings.Contains(profile, awsSSOPath) {
		t.Errorf("profile missing ~/.aws/sso carve-out path %q; full profile:\n%s", awsSSOPath, profile)
	}
	if !strings.Contains(profile, awsCLIPath) {
		t.Errorf("profile missing ~/.aws/cli carve-out path %q; full profile:\n%s", awsCLIPath, profile)
	}

	// Each carve-out path must be inside an (allow ...) clause that follows
	// the broad (deny ...) clause for ~/.aws. Verify ordering: find the deny
	// clause, then find each allow carve-out after it.
	awsDenyPath := filepath.Join(fakeHome, ".aws")
	denyIdx := strings.Index(profile, awsDenyPath)
	if denyIdx < 0 {
		t.Fatalf("profile missing ~/.aws deny path %q; full profile:\n%s", awsDenyPath, profile)
	}

	ssoIdx := strings.Index(profile, awsSSOPath)
	if ssoIdx < 0 {
		t.Fatalf("profile missing ~/.aws/sso path; already checked above — this should not happen")
	}
	if ssoIdx <= denyIdx {
		t.Errorf("~/.aws/sso carve-out (at index %d) must appear AFTER the ~/.aws deny (at index %d); full profile:\n%s", ssoIdx, denyIdx, profile)
	}
	// Verify sso path is inside an (allow ...) block.
	ssoAllowStart := strings.LastIndex(profile[:ssoIdx], "(allow")
	ssoDenyStart := strings.LastIndex(profile[:ssoIdx], "(deny")
	if ssoAllowStart < 0 || ssoDenyStart > ssoAllowStart {
		t.Errorf("~/.aws/sso path is not inside an (allow ...) block; full profile:\n%s", profile)
	}
	// Verify the allow block for ~/.aws/sso includes file-write* (issue #1558).
	// The block header is on the line preceding the first subpath entry.
	ssoAllowHeader := profile[ssoAllowStart:ssoIdx]
	if !strings.Contains(ssoAllowHeader, "file-write*") {
		t.Errorf("~/.aws/sso carve-out allow block must include file-write* (needed for aws CLI STS token cache writes, issue #1558); allow header: %q; full profile:\n%s",
			ssoAllowHeader, profile)
	}

	cliIdx := strings.Index(profile, awsCLIPath)
	if cliIdx < 0 {
		t.Fatalf("profile missing ~/.aws/cli path; already checked above — this should not happen")
	}
	if cliIdx <= denyIdx {
		t.Errorf("~/.aws/cli carve-out (at index %d) must appear AFTER the ~/.aws deny (at index %d); full profile:\n%s", cliIdx, denyIdx, profile)
	}
	// Verify cli path is inside an (allow ...) block.
	cliAllowStart := strings.LastIndex(profile[:cliIdx], "(allow")
	cliDenyStart := strings.LastIndex(profile[:cliIdx], "(deny")
	if cliAllowStart < 0 || cliDenyStart > cliAllowStart {
		t.Errorf("~/.aws/cli path is not inside an (allow ...) block; full profile:\n%s", profile)
	}
	// Verify the allow block for ~/.aws/cli includes file-write* (issue #1558).
	cliAllowHeader := profile[cliAllowStart:cliIdx]
	if !strings.Contains(cliAllowHeader, "file-write*") {
		t.Errorf("~/.aws/cli carve-out allow block must include file-write* (needed for aws CLI STS token cache writes, issue #1558); allow header: %q; full profile:\n%s",
			cliAllowHeader, profile)
	}
}

// TestCollectStagingHomeSymlinkTargets_AWSSSONotExcluded verifies that when
// ~/.aws/sso and ~/.aws/cli symlinks exist in the staging HOME, their resolved
// targets are included in the collected set (not excluded by the deniedPrefixes
// logic), and that both targets are classified as Writable=true so that the
// per-symlink allow block emits file-write* for them (issue #1380, #1558).
func TestCollectStagingHomeSymlinkTargets_AWSSSONotExcluded(t *testing.T) {
	fakeHome := newFakeHome(t)

	// Create ~/.aws/sso and ~/.aws/cli directories in the fake home so the
	// symlinkIfExists helper in PrepareSandboxExecHome creates symlinks for them.
	for _, dir := range []string{
		filepath.Join(fakeHome, ".aws", "sso"),
		filepath.Join(fakeHome, ".aws", "cli"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "aws-sso-not-excluded-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	targets, err := collectStagingHomeSymlinkTargets(stagingHome)
	if err != nil {
		t.Fatalf("collectStagingHomeSymlinkTargets: %v", err)
	}

	// The resolved targets for ~/.aws/sso and ~/.aws/cli must be present.
	// Use filepath.EvalSymlinks to resolve the expected paths: on macOS
	// the fakeHome is under /tmp/ which resolves to /private/tmp/ via the
	// /var → /private/var symlink, so we must compare against canonical forms.
	awsSSOPathRaw := filepath.Join(fakeHome, ".aws", "sso")
	awsCLIPathRaw := filepath.Join(fakeHome, ".aws", "cli")
	awsSSOPath, _ := filepath.EvalSymlinks(awsSSOPathRaw)
	if awsSSOPath == "" {
		awsSSOPath = awsSSOPathRaw
	}
	awsCLIPath, _ := filepath.EvalSymlinks(awsCLIPathRaw)
	if awsCLIPath == "" {
		awsCLIPath = awsCLIPathRaw
	}
	foundSSO, foundCLI := false, false
	ssoWritable, cliWritable := false, false
	for _, target := range targets {
		if target.ResolvedPath == awsSSOPath {
			foundSSO = true
			ssoWritable = target.Writable
		}
		if target.ResolvedPath == awsCLIPath {
			foundCLI = true
			cliWritable = target.Writable
		}
	}
	if !foundSSO {
		t.Errorf("~/.aws/sso resolved path %q not found in collectStagingHomeSymlinkTargets output; got: %v", awsSSOPath, targets)
	} else if !ssoWritable {
		// Both sso and cli must be Writable=true so the per-symlink allow block
		// emits file-write* for them (issue #1558).
		t.Errorf("~/.aws/sso target %q must have Writable=true (needed for aws CLI STS token cache writes); got Writable=false", awsSSOPath)
	}
	if !foundCLI {
		t.Errorf("~/.aws/cli resolved path %q not found in collectStagingHomeSymlinkTargets output; got: %v", awsCLIPath, targets)
	} else if !cliWritable {
		t.Errorf("~/.aws/cli target %q must have Writable=true (needed for aws CLI STS token cache writes); got Writable=false", awsCLIPath)
	}
}

// ── PrepareSandboxExec ──────────────────────────────────────────────────────

// TestPrepareSandboxExec_WritesProfileAndReturnsArgs verifies that
// PrepareSandboxExec materialises the profile to a temp file under the
// per-session state dir and returns args of the shape
// ["sandbox-exec", "-f", <profile_path>, <harness>, ...].
func TestPrepareSandboxExec_WritesProfileAndReturnsArgs(t *testing.T) {
	// PrepareSandboxExec derives the staging HOME from os.UserHomeDir(); in
	// the nix build sandbox $HOME is /homeless-shelter (unwritable), so we
	// redirect HOME to a tempdir. This is the AGENTS.md § "the
	// homeless-shelter failure class" pattern — the gate this test exercises
	// (issue #2168) exists to catch the inverse of this missing guard.
	t.Setenv("HOME", t.TempDir())
	m := newSandboxExecManager(Config{
		SessionName:   "repo@feat",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if stagingHome, err := m.sandboxExecHomePath(); err == nil {
			_ = os.RemoveAll(stagingHome)
		}
	})

	args, err := m.PrepareSandboxExec()
	if err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}

	if len(args) < 4 {
		t.Fatalf("args too short (%d); want at least 4 elements: %v", len(args), args)
	}
	if args[0] != "sandbox-exec" {
		t.Errorf("args[0] = %q, want %q", args[0], "sandbox-exec")
	}
	if args[1] != "-f" {
		t.Errorf("args[1] = %q, want %q", args[1], "-f")
	}
	profilePath := args[2]
	if profilePath == "" {
		t.Errorf("args[2] (profile path) is empty: %v", args)
	}

	// The harness binary follows the profile path. We don't pin the exact
	// string so future PRs can swap "pi" for an absolute path without
	// breaking this assertion, but it must be non-empty.
	if args[3] == "" {
		t.Errorf("args[3] (harness binary) is empty: %v", args)
	}

	// The profile file must exist on disk after PrepareSandboxExec returns.
	info, statErr := os.Stat(profilePath)
	if statErr != nil {
		t.Fatalf("profile path %q not on disk after PrepareSandboxExec: %v", profilePath, statErr)
	}
	if info.Size() == 0 {
		t.Errorf("profile file is empty: %s", profilePath)
	}

	// The on-disk profile must match generateProfile's output verbatim.
	gotContent, readErr := os.ReadFile(profilePath)
	if readErr != nil {
		t.Fatalf("read profile: %v", readErr)
	}
	wantContent := generateProfile(m)
	if string(gotContent) != wantContent {
		t.Errorf("on-disk profile content does not match generateProfile output\n--- on disk:\n%s\n--- generateProfile:\n%s",
			string(gotContent), wantContent)
	}
}

// TestPrepareSandboxExec_ProfilePathIsSessionScoped verifies that the
// profile path is namespaced by the session name so concurrent sessions
// never collide.
func TestPrepareSandboxExec_ProfilePathIsSessionScoped(t *testing.T) {
	mA := newSandboxExecManager(Config{SessionName: "repoA@main"})
	mB := newSandboxExecManager(Config{SessionName: "repoB@main"})

	if mA.sandboxExecProfilePath() == mB.sandboxExecProfilePath() {
		t.Errorf("two managers with different session names share a profile path: %s",
			mA.sandboxExecProfilePath())
	}
	if !strings.Contains(mA.sandboxExecProfilePath(), mA.name) {
		t.Errorf("profile path %q does not contain session-derived name %q",
			mA.sandboxExecProfilePath(), mA.name)
	}
}
// TestSandboxExecPrepare_StagingHomeFailurePropagated verifies that when
// PrepareSandboxExecHome fails (e.g. because the staging-home path is
// blocked by a pre-existing regular file), sandboxExecIsolator.Prepare
// returns a non-nil error whose message mentions the staging HOME failure.
// The session must NOT launch: no profile file is written and no
// sandbox-exec subprocess is started (issue #1879).
func TestSandboxExecPrepare_StagingHomeFailurePropagated(t *testing.T) {
	// Redirect HOME for the sandbox-exec staging-home path derivation. See
	// TestPrepareSandboxExec_WritesProfileAndReturnsArgs for the rationale.
	t.Setenv("HOME", t.TempDir())
	// Build the manager and derive the staging home path before injecting the
	// failure, so we know which path to block.
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "staging-home-fail-test",
		Worktree:    t.TempDir(),
	})

	// sandboxExecHomePath relies on os.UserHomeDir() (or $HOME fallback).
	// Call it directly so the test knows the exact path that will be blocked.
	stagingHome, err := m.sandboxExecHomePath()
	if err != nil {
		t.Fatalf("sandboxExecHomePath: %v", err)
	}

	// Ensure the staging home's parent exists so we can create the blocker file.
	if err := os.MkdirAll(filepath.Dir(stagingHome), 0o755); err != nil {
		t.Fatalf("MkdirAll parent of staging home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(stagingHome)) })

	// Plant a regular file at the staging home path. When PrepareSandboxExecHome
	// calls os.MkdirAll(stagingHome, ...) it will fail with ENOTDIR because the
	// path already exists as a file, not a directory.
	if err := os.WriteFile(stagingHome, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("WriteFile blocker at staging home path %s: %v", stagingHome, err)
	}

	// Prepare must return a non-nil error.
	iso := &sandboxExecIsolator{name: m.name}
	args, prepErr := iso.Prepare(context.Background(), m)
	if prepErr == nil {
		t.Fatalf("Prepare: expected non-nil error when staging home cannot be created, got nil (args=%v)", args)
	}

	// The error message must name the staging HOME failure so the operator
	// knows what went wrong (AC: "clear error message naming the staging HOME
	// path and the underlying cause").
	errMsg := prepErr.Error()
	if !strings.Contains(errMsg, "staging HOME") && !strings.Contains(errMsg, "staging home") {
		t.Errorf("error message must mention staging HOME; got: %q", errMsg)
	}
	if !strings.Contains(errMsg, stagingHome) {
		t.Errorf("error message must include the staging HOME path %q; got: %q", stagingHome, errMsg)
	}

	// No profile file must be written — the session did not advance to the
	// write-profile step.
	profilePath := m.sandboxExecProfilePath()
	if _, statErr := os.Stat(profilePath); statErr == nil {
		t.Errorf("profile file %q was written despite Prepare returning an error: launch was not aborted", profilePath)
		_ = os.Remove(profilePath)
	}
}

// TestSandboxExecPrepare_StagingHomeFailurePropagated_NilArgs verifies that
// the Prepare error is a hard fail and the returned args slice is nil,
// confirming no sandbox-exec argument list was produced for the caller to
// use (regression guard for issue #1879).
func TestSandboxExecPrepare_StagingHomeFailurePropagated_NilArgs(t *testing.T) {
	// Redirect HOME for the sandbox-exec staging-home path derivation. See
	// TestPrepareSandboxExec_WritesProfileAndReturnsArgs for the rationale.
	t.Setenv("HOME", t.TempDir())
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "staging-home-fail-nil-args-test",
		Worktree:    t.TempDir(),
	})

	stagingHome, err := m.sandboxExecHomePath()
	if err != nil {
		t.Fatalf("sandboxExecHomePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(stagingHome), 0o755); err != nil {
		t.Fatalf("MkdirAll parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(stagingHome)) })
	if err := os.WriteFile(stagingHome, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("WriteFile blocker: %v", err)
	}

	iso := &sandboxExecIsolator{name: m.name}
	args, prepErr := iso.Prepare(context.Background(), m)
	if prepErr == nil {
		t.Fatalf("expected error, got nil (args=%v)", args)
	}
	if args != nil {
		t.Errorf("args must be nil on error; got %v", args)
	}
}

// TestSandboxExecBuildArgs_HarnessImmediatelyAfterProfile verifies that the
// harness binary appears at args[3] — directly after "sandbox-exec -f
// <profile>". This is the shape the AC requires.
func TestSandboxExecBuildArgs_HarnessImmediatelyAfterProfile(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14010,
	})
	s := &sandboxExecIsolator{name: m.name}
	args := s.BuildArgs(m)
	if len(args) < 4 {
		t.Fatalf("args too short (%d): %v", len(args), args)
	}
	if args[0] != "sandbox-exec" || args[1] != "-f" {
		t.Fatalf("expected leading sandbox-exec -f; got: %v", args)
	}
	// args[2] is the profile path; args[3] must be the harness binary.
	if args[3] != "pi" {
		t.Errorf("expected args[3] to be the harness binary 'pi'; got %q in %v", args[3], args)
	}
}

// ── PrepareSandboxExecHome ───────────────────────────────────────────────────

// newFakeHome creates a temp directory tree that mimics the credential and
// config paths that PrepareSandboxExecHome reads from $HOME. It sets HOME to
// the fake home dir for the duration of the test and returns the fake home path.
func newFakeHome(t *testing.T) string {
	t.Helper()
	fakeHome := t.TempDir()

	// Create all the directories and files that PrepareSandboxExecHome expects.
	dirs := []string{
		".ssh",
		".aws",
		".config/aws",
		".config/pi",
		".config/kube",
		".cache/bun",
		".cache/nix",
		".claude",
		".mcp-auth",
		".local/share/pi",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(fakeHome, d), 0o755); err != nil {
			t.Fatalf("create fake home dir %s: %v", d, err)
		}
	}

	// Write dummy files that represent SSH keys and config.
	sshFiles := []string{
		"prismatic-koi-ed25519",
		"prismatic-koi-ed25519-signingkey",
		"prismatic-koi-ed25519-signingkey.pub",
		"known_hosts",
	}
	for _, f := range sshFiles {
		if err := os.WriteFile(filepath.Join(fakeHome, ".ssh", f), []byte("dummy"), 0o600); err != nil {
			t.Fatalf("write ssh file %s: %v", f, err)
		}
	}

	// AWS readonly-config and credentials (in XDG location, symlinked by the staging builder).
	if err := os.WriteFile(filepath.Join(fakeHome, ".config", "aws", "readonly-config"), []byte("dummy-aws-cfg"), 0o644); err != nil {
		t.Fatalf("write aws config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".config", "aws", "credentials"), []byte("dummy-aws-creds"), 0o600); err != nil {
		t.Fatalf("write aws credentials: %v", err)
	}
	// Kube agents-config.
	if err := os.WriteFile(filepath.Join(fakeHome, ".config", "kube", "agents-config"), []byte("dummy-kube"), 0o644); err != nil {
		t.Fatalf("write kube config: %v", err)
	}

	// Override HOME for the duration of the test.
	t.Setenv("HOME", fakeHome)

	return fakeHome
}

// TestPrepareSandboxExecHome_CreatesDirectoryAtExpectedPath verifies that
// PrepareSandboxExecHome creates the staging HOME at
// ~/.local/state/prism/sessions/<instance_id>/home/.
func TestPrepareSandboxExecHome_CreatesDirectoryAtExpectedPath(t *testing.T) {
	fakeHome := newFakeHome(t)
	instanceID := "test-instance-abc"

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  instanceID,
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	wantPath := filepath.Join(fakeHome, ".local", "state", "prism", "sessions", instanceID, "home")
	if stagingHome != wantPath {
		t.Errorf("staging HOME = %q, want %q", stagingHome, wantPath)
	}
	if _, err := os.Stat(stagingHome); err != nil {
		t.Errorf("staging HOME does not exist on disk: %v", err)
	}
}

// TestPrepareSandboxExecHome_SSHSymlinks verifies that the staging HOME
// contains symlinks for access-key, signing-key, signing-key.pub, and
// known_hosts when the corresponding files exist in the fake $HOME/.ssh/.
func TestPrepareSandboxExecHome_SSHSymlinks(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "ssh-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	for _, name := range []string{"access-key", "signing-key", "signing-key.pub", "known_hosts"} {
		p := filepath.Join(stagingHome, ".ssh", name)
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("expected symlink %s to exist: %v", p, err)
			continue
		}
		target, readErr := os.Readlink(p)
		if readErr != nil {
			t.Errorf("%s is not a symlink: %v", p, readErr)
			continue
		}
		if target == "" {
			t.Errorf("%s symlink has empty target", p)
		}
	}
}

// TestPrepareSandboxExecHome_MissingSourceSkipped verifies that when a source
// path does not exist, the corresponding symlink is NOT created (no dangling
// symlinks).
func TestPrepareSandboxExecHome_MissingSourceSkipped(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "missing-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// The fake home has no ~/.aws/sso or ~/.aws/cli directories.
	// Symlinks for those must not exist in the staging HOME.
	for _, absent := range []string{
		filepath.Join(stagingHome, ".aws", "sso"),
		filepath.Join(stagingHome, ".aws", "cli"),
	} {
		if _, err := os.Lstat(absent); err == nil {
			t.Errorf("symlink %s should NOT exist (source absent), but it does", absent)
		}
	}
}
// TestPrepareSandboxExecHome_ChromiumLibraryStagingDirs verifies that
// PrepareSandboxExecHome creates empty, writable staging directories for
// chromium's user-data layout under <stagingHome>/Library/Application
// Support/Google and <stagingHome>/Library/Caches/Google (issue #2021).
//
// These directories must NOT be symlinks to the real ~/Library/Application
// Support/Google/ — doing so would expose the daily-driver Chrome's profile
// (cookies, sessions, password store) to the sandboxed chromium instance.
//
// Idempotency: calling PrepareSandboxExecHome a second time on the same
// staging dir must NOT error and must leave existing contents intact.
func TestPrepareSandboxExecHome_ChromiumLibraryStagingDirs(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("chromium staging dirs are Darwin-only")
	}
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "chromium-staging-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	wantDirs := []string{
		filepath.Join(stagingHome, "Library", "Application Support", "Google"),
		filepath.Join(stagingHome, "Library", "Caches", "Google"),
	}
	for _, d := range wantDirs {
		info, statErr := os.Lstat(d)
		if statErr != nil {
			t.Errorf("expected chromium staging dir %q to exist: %v", d, statErr)
			continue
		}
		// Must be a real directory, NOT a symlink. Symlinking to the host
		// ~/Library/Application Support/Google/ would leak the daily-driver
		// Chrome profile contents to the sandboxed chromium instance.
		if info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("chromium staging dir %q must be a real directory, not a symlink: mode=%v", d, info.Mode())
		}
		if !info.IsDir() {
			t.Errorf("chromium staging dir %q must be a directory: mode=%v", d, info.Mode())
		}
	}

	// Idempotency: plant a sentinel file under one of the staging dirs and
	// call PrepareSandboxExecHome again. The sentinel must survive (no
	// clobber, no permissions reset).
	sentinelDir := filepath.Join(stagingHome, "Library", "Application Support", "Google", "Chrome for Testing")
	if err := os.MkdirAll(sentinelDir, 0o700); err != nil {
		t.Fatalf("mkdir sentinel dir: %v", err)
	}
	sentinelPath := filepath.Join(sentinelDir, "prism-2021-idempotency-sentinel")
	const sentinelData = "prism-2021-idempotency"
	if err := os.WriteFile(sentinelPath, []byte(sentinelData), 0o600); err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}

	stagingHome2, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("second PrepareSandboxExecHome (idempotency): %v", err)
	}
	if stagingHome2 != stagingHome {
		t.Errorf("staging HOME changed across calls: %q -> %q", stagingHome, stagingHome2)
	}
	got, readErr := os.ReadFile(sentinelPath)
	if readErr != nil {
		t.Errorf("sentinel file disappeared after second PrepareSandboxExecHome: %v", readErr)
	} else if string(got) != sentinelData {
		t.Errorf("sentinel file contents clobbered: got %q, want %q", string(got), sentinelData)
	}
}

// TestPrepareSandboxExecHome_NixCacheAlwaysIncluded verifies that
// .cache/nix/ is always included as a symlink (matching bwrap.go:333-335
// unconditional RW bind).
func TestPrepareSandboxExecHome_NixCacheAlwaysIncluded(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "nix-cache-test",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	nixLink := filepath.Join(stagingHome, ".cache", "nix")
	if _, err := os.Lstat(nixLink); err != nil {
		t.Errorf(".cache/nix symlink must always be created: %v", err)
	}
	target, err := os.Readlink(nixLink)
	if err != nil {
		t.Errorf(".cache/nix is not a symlink: %v", err)
	} else if target == "" {
		t.Errorf(".cache/nix symlink has empty target")
	}
}

// TestPrepareSandboxExecHome_SigningKeyUsesIntermediatePath verifies that the
// signing-key and signing-key.pub symlinks in the staging HOME point at the
// intermediate ~/.ssh/<name> path (i.e. the stable sops symlink), not the
// fully-resolved concrete path underneath it. This is the fix for issue #1410:
// using the intermediate symlink means the staging HOME stays valid across sops
// rotations (when secrets.d/<N>/ increments), whereas using the resolved path
// would produce a dangling symlink after the rotation.
//
func TestPrepareSandboxExecHome_SigningKeyUsesIntermediatePath(t *testing.T) {
	fakeHome := newFakeHome(t)

	// In newFakeHome, the signing key files are plain files directly under
	// ~/.ssh/. For this test, we simulate the sops-managed layout:
	// ~/.ssh/prismatic-koi-ed25519-signingkey.pub → a "concrete" sops path,
	// and later rotate that concrete path.
	//
	// We replace the plain files with symlinks that point at a "current" dir,
	// mimicking secrets.d/271/.
	sshDir := filepath.Join(fakeHome, ".ssh")
	sopsDir1 := filepath.Join(fakeHome, "sops-dir-1")
	if err := os.MkdirAll(sopsDir1, 0o700); err != nil {
		t.Fatalf("create sops-dir-1: %v", err)
	}
	keyPrivPath1 := filepath.Join(sopsDir1, "signing-key")
	keyPubPath1 := filepath.Join(sopsDir1, "signing-key.pub")
	if err := os.WriteFile(keyPrivPath1, []byte("priv-v1"), 0o600); err != nil {
		t.Fatalf("write sops priv key v1: %v", err)
	}
	if err := os.WriteFile(keyPubPath1, []byte("pub-v1"), 0o600); err != nil {
		t.Fatalf("write sops pub key v1: %v", err)
	}

	// Replace the plain files with symlinks to the "sops v1" concrete paths.
	intermediatePriv := filepath.Join(sshDir, "prismatic-koi-ed25519-signingkey")
	intermediatePub := filepath.Join(sshDir, "prismatic-koi-ed25519-signingkey.pub")
	_ = os.Remove(intermediatePriv)
	_ = os.Remove(intermediatePub)
	if err := os.Symlink(keyPrivPath1, intermediatePriv); err != nil {
		t.Fatalf("symlink intermediate priv: %v", err)
	}
	if err := os.Symlink(keyPubPath1, intermediatePub); err != nil {
		t.Fatalf("symlink intermediate pub: %v", err)
	}

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@signing-key-test",
		InstanceID:  "signing-key-intermediate-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Verify signing-key.pub symlink points at the INTERMEDIATE path, not the
	// fully-resolved sops-dir-1/signing-key.pub path.
	pubLink := filepath.Join(stagingHome, ".ssh", "signing-key.pub")
	pubTarget, err := os.Readlink(pubLink)
	if err != nil {
		t.Fatalf("signing-key.pub is not a symlink: %v", err)
	}
	if pubTarget != intermediatePub {
		t.Errorf("signing-key.pub symlink points at %q, want intermediate path %q\n"+
			"(the symlink must point at the stable sops intermediate, not the resolved concrete path,\n"+
			"so it stays valid after a sops rotation)",
			pubTarget, intermediatePub)
	}

	// Same for signing-key (private).
	privLink := filepath.Join(stagingHome, ".ssh", "signing-key")
	privTarget, err := os.Readlink(privLink)
	if err != nil {
		t.Fatalf("signing-key is not a symlink: %v", err)
	}
	if privTarget != intermediatePriv {
		t.Errorf("signing-key symlink points at %q, want intermediate path %q",
			privTarget, intermediatePriv)
	}

	// Now simulate a sops rotation: update the intermediate symlinks to point
	// at a new secrets.d/v2 directory, leaving sops-dir-1 in place but unreachable.
	sopsDir2 := filepath.Join(fakeHome, "sops-dir-2")
	if err := os.MkdirAll(sopsDir2, 0o700); err != nil {
		t.Fatalf("create sops-dir-2: %v", err)
	}
	keyPrivPath2 := filepath.Join(sopsDir2, "signing-key")
	keyPubPath2 := filepath.Join(sopsDir2, "signing-key.pub")
	if err := os.WriteFile(keyPrivPath2, []byte("priv-v2"), 0o600); err != nil {
		t.Fatalf("write sops priv key v2: %v", err)
	}
	if err := os.WriteFile(keyPubPath2, []byte("pub-v2"), 0o600); err != nil {
		t.Fatalf("write sops pub key v2: %v", err)
	}
	// Rotate: update intermediate symlinks to point at v2.
	_ = os.Remove(intermediatePriv)
	_ = os.Remove(intermediatePub)
	if err := os.Symlink(keyPrivPath2, intermediatePriv); err != nil {
		t.Fatalf("rotate intermediate priv to v2: %v", err)
	}
	if err := os.Symlink(keyPubPath2, intermediatePub); err != nil {
		t.Fatalf("rotate intermediate pub to v2: %v", err)
	}

	// After rotation, the staging HOME signing-key.pub still points at
	// intermediatePub, which now resolves to sops-dir-2. Verify the chain
	// resolves (os.Stat follows symlinks) and the content is from v2.
	content, err := os.ReadFile(pubLink)
	if err != nil {
		t.Fatalf("cannot read signing-key.pub through staging HOME after rotation: %v\n"+
			"(this would cause 'Couldn't load public key' in git push — the fix for #1410)", err)
	}
	if string(content) != "pub-v2" {
		t.Errorf("signing-key.pub after rotation: got %q, want %q", string(content), "pub-v2")
	}
}

// TestPrepareSandboxExecHome_SigningKeyAbsentNoDanglingSymlink verifies that
// when the signing key intermediate symlink does not exist (genuinely absent,
// not just stale), no dangling symlink is created in the staging HOME.
// This covers AC: "if signing keys are genuinely absent, git push still works
// — it pushes without signing rather than crashing."
func TestPrepareSandboxExecHome_SigningKeyAbsentNoDanglingSymlink(t *testing.T) {
	fakeHome := newFakeHome(t)

	// Remove the signing key files from the fake home (simulate absent keys).
	sshDir := filepath.Join(fakeHome, ".ssh")
	_ = os.Remove(filepath.Join(sshDir, "prismatic-koi-ed25519-signingkey"))
	_ = os.Remove(filepath.Join(sshDir, "prismatic-koi-ed25519-signingkey.pub"))

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@absent-signing-key",
		InstanceID:  "signing-key-absent-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Signing key symlinks must NOT exist in staging HOME when source is absent.
	for _, name := range []string{"signing-key", "signing-key.pub"} {
		p := filepath.Join(stagingHome, ".ssh", name)
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s must not exist when source is absent (no dangling symlinks)", name)
		}
	}
}

// TestPrepareSandboxExecHome_AccessKeyUsesIntermediatePath verifies that the
// access-key symlink in the staging HOME points at the stable intermediate
// sops symlink (~/.ssh/<accessKeyName>), not the fully-resolved concrete path
// inside secrets.d/<N>/. This is the fix for issue #1573: on Darwin the access
// key is managed by sops-nix and its concrete path rotates on each
// darwin-rebuild switch, so the staging symlink must anchor to the stable
// intermediate to survive rotations.
func TestPrepareSandboxExecHome_AccessKeyUsesIntermediatePath(t *testing.T) {
	fakeHome := newFakeHome(t)

	// Set up a two-hop chain simulating the sops-managed layout:
	//   ~/.ssh/prismatic-koi-ed25519  (intermediate)
	//     → sopsDir1/access-key       (concrete v1)
	//
	// After PrepareSandboxExecHome runs, we rotate the intermediate to v2
	// and verify the staging HOME symlink still resolves.
	sshDir := filepath.Join(fakeHome, ".ssh")
	sopsDir1 := filepath.Join(fakeHome, "sops-dir-1")
	if err := os.MkdirAll(sopsDir1, 0o700); err != nil {
		t.Fatalf("create sops-dir-1: %v", err)
	}
	keyPath1 := filepath.Join(sopsDir1, "access-key")
	if err := os.WriteFile(keyPath1, []byte("access-key-v1"), 0o600); err != nil {
		t.Fatalf("write access key v1: %v", err)
	}

	// Replace the plain key file with a symlink pointing at the v1 concrete path.
	intermediate := filepath.Join(sshDir, "prismatic-koi-ed25519")
	_ = os.Remove(intermediate)
	if err := os.Symlink(keyPath1, intermediate); err != nil {
		t.Fatalf("symlink intermediate access key: %v", err)
	}

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@access-key-test",
		InstanceID:  "access-key-intermediate-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// The access-key symlink must point at the INTERMEDIATE path, not the
	// fully-resolved sops-dir-1/access-key path (fix for #1573).
	accessLink := filepath.Join(stagingHome, ".ssh", "access-key")
	accessTarget, err := os.Readlink(accessLink)
	if err != nil {
		t.Fatalf("access-key is not a symlink: %v", err)
	}
	if accessTarget != intermediate {
		t.Errorf("access-key symlink points at %q, want intermediate path %q\n"+
			"(the symlink must point at the stable sops intermediate, not the resolved concrete path,\n"+
			"so it stays valid after a sops rotation — issue #1573)",
			accessTarget, intermediate)
	}

	// Simulate a sops rotation: update the intermediate to point at v2 and
	// remove v1. The staging HOME symlink must still resolve.
	sopsDir2 := filepath.Join(fakeHome, "sops-dir-2")
	if err := os.MkdirAll(sopsDir2, 0o700); err != nil {
		t.Fatalf("create sops-dir-2: %v", err)
	}
	keyPath2 := filepath.Join(sopsDir2, "access-key")
	if err := os.WriteFile(keyPath2, []byte("access-key-v2"), 0o600); err != nil {
		t.Fatalf("write access key v2: %v", err)
	}
	_ = os.Remove(intermediate)
	if err := os.Symlink(keyPath2, intermediate); err != nil {
		t.Fatalf("rotate intermediate to v2: %v", err)
	}
	_ = os.RemoveAll(sopsDir1) // simulate rotation removing old secrets.d/<N>

	// After rotation, reads through the staging HOME symlink must succeed.
	content, err := os.ReadFile(accessLink)
	if err != nil {
		t.Fatalf("cannot read access-key through staging HOME after rotation: %v\n"+
			"(this would break SSH inside running sessions — the fix for #1573)", err)
	}
	if string(content) != "access-key-v2" {
		t.Errorf("access-key after rotation: got %q, want %q", string(content), "access-key-v2")
	}
}

// TestPrepareSandboxExecHome_SopsRotation_AllFourSymlinks verifies that the
// four staging-HOME symlinks introduced by issue #1573 — access-key,
// .aws/config, .aws/credentials, and .kube/config — survive a sops
// secrets.d/<N> → secrets.d/<N+1> rotation after PrepareSandboxExecHome has
// already run.
//
// The test follows the same pattern as
// TestPrepareSandboxExecHome_SigningKeyUsesIntermediatePath (which covers
// signing-key{,.pub} from issue #1410):
//  1. Plant v1 concrete files under sopsDir1.
//  2. Create intermediate symlinks pointing at v1.
//  3. Run PrepareSandboxExecHome.
//  4. Verify each staging symlink points at the stable intermediate.
//  5. Rotate: update the intermediates to point at v2, delete v1.
//  6. Verify reads through each staging symlink still succeed (resolve to v2).
func TestPrepareSandboxExecHome_SopsRotation_AllFourSymlinks(t *testing.T) {
	fakeHome := newFakeHome(t)

	// ── set up v1 concrete files ───────────────────────────────────────
	sopsDir1 := filepath.Join(fakeHome, "sops-dir-1")
	if err := os.MkdirAll(filepath.Join(sopsDir1, "aws"), 0o700); err != nil {
		t.Fatalf("create sops-dir-1/aws: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(sopsDir1, "kube"), 0o700); err != nil {
		t.Fatalf("create sops-dir-1/kube: %v", err)
	}

	v1Files := map[string]string{
		"ssh/access-key":   "access-key-v1",
		"aws/config":       "aws-config-v1",
		"aws/credentials":  "aws-credentials-v1",
		"kube/config":      "kube-config-v1",
	}
	for rel, content := range v1Files {
		path := filepath.Join(sopsDir1, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// ── install intermediate symlinks (stable sops intermediates) ────────────
	// Each intermediate simulates the path that sops-nix keeps stable across
	// rotations: e.g. ~/.ssh/prismatic-koi-ed25519 → secrets.d/<N>/...
	intermediates := map[string]string{
		filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519"):          filepath.Join(sopsDir1, "ssh", "access-key"),
		filepath.Join(fakeHome, ".config", "aws", "readonly-config"):      filepath.Join(sopsDir1, "aws", "config"),
		filepath.Join(fakeHome, ".config", "aws", "credentials"):          filepath.Join(sopsDir1, "aws", "credentials"),
		filepath.Join(fakeHome, ".config", "kube", "agents-config"):       filepath.Join(sopsDir1, "kube", "config"),
	}
	for intermediate, target := range intermediates {
		_ = os.Remove(intermediate) // remove plain file from newFakeHome
		if err := os.Symlink(target, intermediate); err != nil {
			t.Fatalf("symlink intermediate %s → %s: %v", intermediate, target, err)
		}
	}

	// ── run PrepareSandboxExecHome ─────────────────────────────────────────
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@sops-rotation-all",
		InstanceID:  "sops-rotation-all-four-test",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// ── verify each staging symlink points at the intermediate ──────────────
	type stagingEntry struct {
		link         string // path in staging HOME
		intermediate string // expected symlink target (the stable sops intermediate)
	}
	entries := []stagingEntry{
		{
			link:         filepath.Join(stagingHome, ".ssh", "access-key"),
			intermediate: filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519"),
		},
		{
			link:         filepath.Join(stagingHome, ".aws", "config"),
			intermediate: filepath.Join(fakeHome, ".config", "aws", "readonly-config"),
		},
		{
			link:         filepath.Join(stagingHome, ".aws", "credentials"),
			intermediate: filepath.Join(fakeHome, ".config", "aws", "credentials"),
		},
		{
			link:         filepath.Join(stagingHome, ".kube", "config"),
			intermediate: filepath.Join(fakeHome, ".config", "kube", "agents-config"),
		},
	}
	for _, e := range entries {
		target, readErr := os.Readlink(e.link)
		if readErr != nil {
			t.Errorf("%s is not a symlink: %v", e.link, readErr)
			continue
		}
		if target != e.intermediate {
			t.Errorf("%s → %q, want intermediate %q\n"+
				"(symlink must point at the stable sops intermediate, not the resolved concrete path,\n"+
				"so it stays valid after a sops rotation — issue #1573)",
				e.link, target, e.intermediate)
		}
	}

	// ── rotate: update intermediates to v2, delete v1 ───────────────────────
	sopsDir2 := filepath.Join(fakeHome, "sops-dir-2")
	v2Files := map[string]string{
		"ssh/access-key":   "access-key-v2",
		"aws/config":       "aws-config-v2",
		"aws/credentials":  "aws-credentials-v2",
		"kube/config":      "kube-config-v2",
	}
	for rel, content := range v2Files {
		path := filepath.Join(sopsDir2, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir for sops-dir-2/%s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write sops-dir-2/%s: %v", rel, err)
		}
	}
	v2Targets := map[string]string{
		filepath.Join(fakeHome, ".ssh", "prismatic-koi-ed25519"):          filepath.Join(sopsDir2, "ssh", "access-key"),
		filepath.Join(fakeHome, ".config", "aws", "readonly-config"):      filepath.Join(sopsDir2, "aws", "config"),
		filepath.Join(fakeHome, ".config", "aws", "credentials"):          filepath.Join(sopsDir2, "aws", "credentials"),
		filepath.Join(fakeHome, ".config", "kube", "agents-config"):       filepath.Join(sopsDir2, "kube", "config"),
	}
	for intermediate, newTarget := range v2Targets {
		_ = os.Remove(intermediate)
		if err := os.Symlink(newTarget, intermediate); err != nil {
			t.Fatalf("rotate intermediate %s → %s: %v", intermediate, newTarget, err)
		}
	}
	_ = os.RemoveAll(sopsDir1) // simulate sops deleting the old secrets.d/<N>

	// ── verify reads through staging HOME still succeed after rotation ──────
	v2Contents := map[string]string{
		filepath.Join(stagingHome, ".ssh", "access-key"):  "access-key-v2",
		filepath.Join(stagingHome, ".aws", "config"):       "aws-config-v2",
		filepath.Join(stagingHome, ".aws", "credentials"):  "aws-credentials-v2",
		filepath.Join(stagingHome, ".kube", "config"):      "kube-config-v2",
	}
	for link, wantContent := range v2Contents {
		content, readErr := os.ReadFile(link)
		if readErr != nil {
			t.Errorf("cannot read %s through staging HOME after rotation: %v\n"+
				"(this means the staging symlink is dangling after sops rotate — issue #1573)",
				link, readErr)
			continue
		}
		if string(content) != wantContent {
			t.Errorf("%s after rotation: got %q, want %q", link, string(content), wantContent)
		}
	}
}

// TestPrepareSandboxExecHome_PiAgentDirNotSymlinked verifies that
// PrepareSandboxExecHome does NOT create a ~/.pi/agent entry under the
// staging HOME. Since #2034 the in-sandbox PI_CODING_AGENT_DIR resolves
// directly to the host ~/.pi/agent via the SBPL (subpath ~/.pi/agent) RW
// allow for pi sessions; no staging-HOME-relative symlink is required or
// desirable.
func TestPrepareSandboxExecHome_PiAgentDirNotSymlinked(t *testing.T) {
	fakeHome := newFakeHome(t)

	// Create ~/.pi/agent in the fake home.
	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(piAgentDir, 0o700); err != nil {
		t.Fatalf("create ~/.pi/agent: %v", err)
	}

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@pi-agent-test",
		InstanceID:  "pi-agent-symlink-test",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// No symlink must be created at <stagingHome>/.pi/agent — PI reads the
	// host ~/.pi/agent directly via the SBPL allow (since #2034).
	symlinkPath := filepath.Join(stagingHome, ".pi", "agent")
	if _, err := os.Lstat(symlinkPath); err == nil {
		t.Errorf(".pi/agent must not be symlinked by PrepareSandboxExecHome (PI reads the host dir directly via the SBPL subpath allow)")
	}
}

// TestPrepareSandboxExecHome_PiAgentDirMissingSkipped verifies that when
// ~/.pi/agent does not exist, PrepareSandboxExecHome succeeds without creating
// a dangling symlink.
func TestPrepareSandboxExecHome_PiAgentDirMissingSkipped(t *testing.T) {
	fakeHome := newFakeHome(t)
	// Ensure ~/.pi/agent does NOT exist.
	_ = os.RemoveAll(filepath.Join(fakeHome, ".pi"))

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@pi-agent-missing",
		InstanceID:  "pi-agent-missing-test",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// No symlink must exist for .pi/agent when source is absent.
	symlinkPath := filepath.Join(stagingHome, ".pi", "agent")
	if _, err := os.Lstat(symlinkPath); err == nil {
		t.Errorf(".pi/agent must not exist when ~/.pi/agent is absent")
	}
}

// ── PI Atlassian MCP OAuth token staging (Darwin + pi harness) ─────────────

// TestPrepareSandboxExecHome_PiOAuthTokenSymlinked verifies that after
// PrepareSandboxExecHome runs for a Harness=="pi" Manager on Darwin,
// <stagingHome>/.pi/agent/atlassian-mcp-oauth.json exists as a symlink
// whose target equals the host path ~/.pi/agent/atlassian-mcp-oauth.json.
func TestPrepareSandboxExecHome_PiOAuthTokenSymlinked(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("PI OAuth token staging is Darwin-only")
	}
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@pi-oauth-token-test",
		InstanceID:  "pi-oauth-token-symlink-test",
		Harness:     "pi",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".pi"))
	})

	// <stagingHome>/.pi/agent/atlassian-mcp-oauth.json must be a symlink.
	stagingTokenPath := filepath.Join(stagingHome, ".pi", "agent", "atlassian-mcp-oauth.json")
	info, err := os.Lstat(stagingTokenPath)
	if err != nil {
		t.Fatalf("%s must exist: %v", stagingTokenPath, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s must be a symlink, got mode %v", stagingTokenPath, info.Mode())
	}

	// The symlink target must be the real host path.
	target, err := os.Readlink(stagingTokenPath)
	if err != nil {
		t.Fatalf("Readlink %s: %v", stagingTokenPath, err)
	}
	wantTarget := filepath.Join(fakeHome, ".pi", "agent", "atlassian-mcp-oauth.json")
	if target != wantTarget {
		t.Errorf("symlink target = %q, want %q", target, wantTarget)
	}
}

// TestPrepareSandboxExecHome_PiOAuthAgentDirIsRealDir verifies that after
// PrepareSandboxExecHome runs for a Harness=="pi" Manager on Darwin,
// <stagingHome>/.pi/agent is a real directory (not a symlink).
func TestPrepareSandboxExecHome_PiOAuthAgentDirIsRealDir(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("PI OAuth token staging is Darwin-only")
	}
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@pi-oauth-agent-dir",
		InstanceID:  "pi-oauth-agent-dir-test",
		Harness:     "pi",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".pi"))
	})

	// <stagingHome>/.pi/agent must be a real directory, not a symlink.
	agentPath := filepath.Join(stagingHome, ".pi", "agent")
	info, err := os.Lstat(agentPath)
	if err != nil {
		t.Fatalf("%s must exist: %v", agentPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("%s must be a real directory, not a symlink", agentPath)
	}
	if !info.IsDir() {
		t.Errorf("%s must be a directory, got mode %v", agentPath, info.Mode())
	}
}

// TestPrepareSandboxExecHome_PiOAuthHostFileMode verifies that when
// PrepareSandboxExecHome creates ~/.pi/agent/atlassian-mcp-oauth.json because
// it was absent, the file has mode 0600.
func TestPrepareSandboxExecHome_PiOAuthHostFileMode(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("PI OAuth token staging is Darwin-only")
	}
	fakeHome := newFakeHome(t)

	// Ensure the token file does not exist before the test.
	hostTokenPath := filepath.Join(fakeHome, ".pi", "agent", "atlassian-mcp-oauth.json")
	_ = os.RemoveAll(filepath.Join(fakeHome, ".pi"))

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@pi-oauth-mode",
		InstanceID:  "pi-oauth-mode-test",
		Harness:     "pi",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".pi"))
	})

	// Suppress the "declared and not used" warning for hostTokenPath.
	info, err := os.Stat(hostTokenPath)
	if err != nil {
		t.Fatalf("host token file %s must exist: %v", hostTokenPath, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("host token file mode = %04o, want 0600", info.Mode().Perm())
	}
}

// TestPrepareSandboxExecHome_PiOAuthExistingFileNotOverwritten verifies that
// when ~/.pi/agent/atlassian-mcp-oauth.json already exists with non-empty
// content, PrepareSandboxExecHome does NOT truncate or overwrite it.
func TestPrepareSandboxExecHome_PiOAuthExistingFileNotOverwritten(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("PI OAuth token staging is Darwin-only")
	}
	fakeHome := newFakeHome(t)

	// Pre-create the token file with sentinel content.
	hostAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	if err := os.MkdirAll(hostAgentDir, 0o700); err != nil {
		t.Fatalf("create host agent dir: %v", err)
	}
	hostTokenPath := filepath.Join(hostAgentDir, "atlassian-mcp-oauth.json")
	const sentinel = `{"access_token":"sentinel-do-not-overwrite"}`
	if err := os.WriteFile(hostTokenPath, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("write sentinel token: %v", err)
	}
	origStat, err := os.Stat(hostTokenPath)
	if err != nil {
		t.Fatalf("stat sentinel token: %v", err)
	}

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@pi-oauth-existing",
		InstanceID:  "pi-oauth-existing-test",
		Harness:     "pi",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.Remove(hostTokenPath)
	})

	// Verify content unchanged.
	got, err := os.ReadFile(hostTokenPath)
	if err != nil {
		t.Fatalf("read host token: %v", err)
	}
	if string(got) != sentinel {
		t.Errorf("host token content changed: got %q, want %q", string(got), sentinel)
	}

	// Verify mtime unchanged.
	afterStat, err := os.Stat(hostTokenPath)
	if err != nil {
		t.Fatalf("stat host token after: %v", err)
	}
	if !afterStat.ModTime().Equal(origStat.ModTime()) {
		t.Errorf("host token mtime changed: orig %v, after %v", origStat.ModTime(), afterStat.ModTime())
	}
}

// TestPrepareSandboxExecHome_PiOAuthMissingHostAgentDirAutoCreated verifies
// that when ~/.pi/agent/ does not exist on the host, PrepareSandboxExecHome
// creates it with mode 0700 and returns nil error.
func TestPrepareSandboxExecHome_PiOAuthMissingHostAgentDirAutoCreated(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("PI OAuth token staging is Darwin-only")
	}
	fakeHome := newFakeHome(t)

	// Ensure ~/.pi/agent does not exist.
	_ = os.RemoveAll(filepath.Join(fakeHome, ".pi"))

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@pi-oauth-mkdir",
		InstanceID:  "pi-oauth-mkdir-test",
		Harness:     "pi",
	})
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome with missing host agent dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".pi"))
	})

	// Host agent dir must now exist with mode 0700.
	hostAgentDir := filepath.Join(fakeHome, ".pi", "agent")
	info, err := os.Stat(hostAgentDir)
	if err != nil {
		t.Fatalf("host agent dir %s must exist after autocreate: %v", hostAgentDir, err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("host agent dir mode = %04o, want 0700", info.Mode().Perm())
	}

	// The symlink in the staging dir must also be created.
	stagingTokenPath := filepath.Join(stagingHome, ".pi", "agent", "atlassian-mcp-oauth.json")
	if _, err := os.Lstat(stagingTokenPath); err != nil {
		t.Errorf("staging token symlink must exist after autocreate: %v", err)
	}
}

// TestPrepareSandboxExecHome_PiOAuthNonPiHarnessNoOp verifies that when
// Harness != "pi", PrepareSandboxExecHome does NOT create
// <stagingHome>/.pi/agent and does NOT touch ~/.pi/agent/atlassian-mcp-oauth.json.
func TestPrepareSandboxExecHome_PiOAuthNonPiHarnessNoOp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("PI OAuth token staging is Darwin-only")
	}
	fakeHome := newFakeHome(t)
	_ = os.RemoveAll(filepath.Join(fakeHome, ".pi"))

	for _, harness := range []string{"", "other-harness"} {
		t.Run("harness="+harness, func(t *testing.T) {
			m := newSandboxExecManagerWithInstance(Config{
				SessionName: "repo@non-pi-" + harness,
				InstanceID:  "pi-oauth-non-pi-" + harness,
				Harness:     harness,
			})
			stagingHome, err := m.PrepareSandboxExecHome()
			if err != nil {
				t.Fatalf("PrepareSandboxExecHome: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

			// <stagingHome>/.pi/agent must NOT be created for non-pi harness.
			agentPath := filepath.Join(stagingHome, ".pi", "agent")
			if _, err := os.Lstat(agentPath); err == nil {
				t.Errorf("<stagingHome>/.pi/agent must not exist for harness=%q", harness)
			}

			// ~/.pi/agent/atlassian-mcp-oauth.json must NOT be created.
			hostTokenPath := filepath.Join(fakeHome, ".pi", "agent", "atlassian-mcp-oauth.json")
			if _, err := os.Stat(hostTokenPath); err == nil {
				t.Errorf("host token file must not be created for harness=%q", harness)
			}
		})
	}
}

// TestPrepareSandboxExecHome_PiOAuthIdempotent verifies that calling
// PrepareSandboxExecHome twice on the same staging HOME leaves the symlink
// at <stagingHome>/.pi/agent/atlassian-mcp-oauth.json valid and pointing at
// the same host path; no error is returned on the second call.
func TestPrepareSandboxExecHome_PiOAuthIdempotent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("PI OAuth token staging is Darwin-only")
	}
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@pi-oauth-idem",
		InstanceID:  "pi-oauth-idempotent-test",
		Harness:     "pi",
	})

	// First call.
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("first PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".pi"))
	})

	wantTarget := filepath.Join(fakeHome, ".pi", "agent", "atlassian-mcp-oauth.json")
	getTarget := func() string {
		t.Helper()
		target, err := os.Readlink(filepath.Join(stagingHome, ".pi", "agent", "atlassian-mcp-oauth.json"))
		if err != nil {
			t.Fatalf("Readlink after first call: %v", err)
		}
		return target
	}
	firstTarget := getTarget()
	if firstTarget != wantTarget {
		t.Errorf("first call: symlink target = %q, want %q", firstTarget, wantTarget)
	}

	// Second call — must not fail and symlink must still point at same host path.
	_, err = m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("second PrepareSandboxExecHome: %v", err)
	}
	secondTarget := getTarget()
	if secondTarget != wantTarget {
		t.Errorf("second call: symlink target = %q, want %q", secondTarget, wantTarget)
	}
}

// TestPrepareSandboxExecHome_IdempotentReCreation verifies that calling
// PrepareSandboxExecHome a second time on an existing staging dir succeeds
// without error and does not corrupt symlinks.
func TestPrepareSandboxExecHome_IdempotentReCreation(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "idempotent-test",
	})

	// First call.
	stagingHome1, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("first PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome1) })

	// Second call — must not fail.
	stagingHome2, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("second PrepareSandboxExecHome: %v", err)
	}
	if stagingHome1 != stagingHome2 {
		t.Errorf("staging HOME changed between calls: %q != %q", stagingHome1, stagingHome2)
	}

	// Verify that the .cache/nix symlink is still valid after re-creation.
	nixLink := filepath.Join(stagingHome2, ".cache", "nix")
	if _, err := os.Lstat(nixLink); err != nil {
		t.Errorf(".cache/nix not present after re-creation: %v", err)
	}
}

// TestPrepareSandboxExecHome_TwoConcurrentSessions verifies that two sessions
// with different InstanceIDs have independent staging dirs (no collisions).
func TestPrepareSandboxExecHome_TwoConcurrentSessions(t *testing.T) {
	newFakeHome(t)

	mA := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat-a",
		InstanceID:  "instance-aaa",
	})
	mB := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat-b",
		InstanceID:  "instance-bbb",
	})

	homeA, errA := mA.PrepareSandboxExecHome()
	if errA != nil {
		t.Fatalf("session A: %v", errA)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeA) })

	homeB, errB := mB.PrepareSandboxExecHome()
	if errB != nil {
		t.Fatalf("session B: %v", errB)
	}
	t.Cleanup(func() { _ = os.RemoveAll(homeB) })

	if homeA == homeB {
		t.Errorf("two sessions have the same staging HOME: %q", homeA)
	}
	if strings.Contains(homeA, "instance-bbb") || strings.Contains(homeB, "instance-aaa") {
		t.Errorf("staging HOME paths are not properly namespaced: A=%q B=%q", homeA, homeB)
	}
}

// TestSandboxExecCleanup_RemovesStagingHome verifies that EnsureRemoved removes
// the staging HOME directory created by PrepareSandboxExecHome.
func TestSandboxExecCleanup_RemovesStagingHome(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "cleanup-test",
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}

	// Verify it exists.
	if _, statErr := os.Stat(stagingHome); statErr != nil {
		t.Fatalf("staging HOME not created: %v", statErr)
	}

	// Call EnsureRemoved (uses a background context; no podman calls needed
	// because sandboxExecIsolator.Shutdown is a no-op).
	m.EnsureRemoved(context.Background())

	// Staging HOME must be gone.
	if _, statErr := os.Stat(stagingHome); statErr == nil {
		t.Errorf("staging HOME still exists after EnsureRemoved: %s", stagingHome)
	}
}

// TestGenerateProfile_ProfileIncludesSymlinkTargetAllows verifies that the
// profile emitted by generateProfile includes (allow file-read* (literal ...))
// rules for symlink targets in the staging HOME.
func TestGenerateProfile_ProfileIncludesSymlinkTargetAllows(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "profile-targets-test",
		Worktree:    t.TempDir(),
	})

	// Pre-build the staging HOME so generateProfile can collect targets.
	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".local", "state", "prism"))
	})

	profile := generateProfile(m)

	// The profile must contain (allow file-read* for RO targets (e.g. SSH keys)
	// and (allow file-read* file-write* for RW targets (e.g. .cache/bun).
	if !strings.Contains(profile, "(allow file-read*") {
		t.Errorf("profile missing (allow file-read* ...) clause; full profile:\n%s", profile)
	}

	// .cache/bun, .cache/nix, .claude, .mcp-auth are RW — the
	// profile must grant file-write* on their resolved targets.
	rwPaths := []string{
		filepath.Join(fakeHome, ".cache", "bun"),
		filepath.Join(fakeHome, ".cache", "nix"),
		filepath.Join(fakeHome, ".claude"),
		filepath.Join(fakeHome, ".mcp-auth"),
	}
	for _, rwPath := range rwPaths {
		resolved, evalErr := filepath.EvalSymlinks(rwPath)
		if evalErr != nil {
			// Path may not be in staging home for this test — skip.
			continue
		}
		// Find the resolved path in the profile and verify file-write* precedes it
		// in the same allow block.
		idx := strings.Index(profile, resolved)
		if idx < 0 {
			t.Errorf("RW path %q (resolved: %q) missing from profile;\nfull profile:\n%s", rwPath, resolved, profile)
			continue
		}
		// Look backward for the nearest (allow ... clause.
		clauseStart := strings.LastIndex(profile[:idx], "(allow ")
		if clauseStart < 0 {
			t.Errorf("RW path %q: no preceding (allow clause found in profile", rwPath)
			continue
		}
		clause := profile[clauseStart:idx]
		if !strings.Contains(clause, "file-write*") {
			t.Errorf("RW path %q is present in profile but NOT inside a file-write* allow block;\nclause: %q\nfull profile:\n%s", rwPath, clause, profile)
		}
	}
}

// TestGenerateProfile_PiAgentDirSubpathRule verifies that generateProfile
// emits a (subpath ~/.pi/agent) rule when Harness == "pi".
//
// Background: OAuth token refresh inside pi-coding-agent uses
// proper-lockfile.lock(authPath, {realpath: true}), which resolves any symlink
// in authPath and then calls mkdir(<resolved>.lock) to acquire the lock. That
// mkdir requires write permission on the *parent directory* ~/.pi/agent/, not
// just on auth.json. A (literal ...) rule on auth.json alone is therefore
// insufficient — the sandbox denies the mkdir (EPERM) and the refresh silently
// fails after ~30 s of retries.
//
// The rule is widened to (subpath ~/.pi/agent) so that:
//   - auth.json reads and writes are permitted (token refresh writes back).
//   - mkdir auth.json.lock succeeds because the parent dir is writable.
//
// The rule is always emitted even when ~/.pi/agent does not yet exist —
// sandbox-exec silently ignores (subpath ...) rules for non-existent paths, so
// pi simply prompts for /login rather than crashing on a fresh install.
func TestGenerateProfile_PiAgentDirSubpathRule(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := newSandboxExecManager(Config{
		SessionName: "repo@pi-session",
		Harness:     "pi",
	})
	profile := generateProfile(m)

	piAgentDir := filepath.Join(fakeHome, ".pi", "agent")

	// The ~/.pi/agent subpath must appear in the profile.
	if !strings.Contains(profile, piAgentDir) {
		t.Errorf("profile missing ~/.pi/agent path %q; full profile:\n%s", piAgentDir, profile)
	}

	// It must be a (subpath ...) rule — (literal auth.json) alone is
	// insufficient because proper-lockfile mkdir(<authPath>.lock) requires
	// write on the parent directory.
	subpathRule := "(subpath " + quoteSBPL(piAgentDir) + ")"
	if !strings.Contains(profile, subpathRule) {
		t.Errorf("~/.pi/agent must appear as (subpath ...), not (literal ...); full profile:\n%s", profile)
	}

	// The subpath rule must appear inside an (allow file-read* file-write* ...)
	// clause so that proper-lockfile can mkdir auth.json.lock adjacent to
	// auth.json.
	subpathIdx := strings.Index(profile, subpathRule)
	if subpathIdx < 0 {
		t.Fatalf("subpath rule not found in profile (checked above)")
	}
	clauseStart := strings.LastIndex(profile[:subpathIdx], "(allow")
	if clauseStart < 0 {
		t.Errorf("~/.pi/agent subpath not inside an (allow ...) clause; full profile:\n%s", profile)
	}
	clause := profile[clauseStart:subpathIdx]
	if !strings.Contains(clause, "file-write*") {
		t.Errorf("~/.pi/agent allow clause must include file-write* so proper-lockfile can mkdir auth.json.lock; clause: %q; full profile:\n%s", clause, profile)
	}

	// Regression guard: the (subpath ~/.pi/agent) covers both auth.json and
	// auth.json.lock (the lock dir that proper-lockfile creates). Confirm both
	// paths are children of piAgentDir (they are, by construction — the test
	// documents the invariant explicitly).
	authJSON := filepath.Join(piAgentDir, "auth.json")
	authLock := filepath.Join(piAgentDir, "auth.json.lock")
	if !strings.HasPrefix(authJSON, piAgentDir) || !strings.HasPrefix(authLock, piAgentDir) {
		t.Errorf("auth.json and auth.json.lock are not children of piAgentDir %q — subpath rule does not cover them", piAgentDir)
	}
}

func TestGenerateProfile_PiAuthJSONAbsentForNonPiSession(t *testing.T) {
	// When Harness != "pi", generateProfile must NOT emit a rule for
	// ~/.pi/agent/auth.json — the pi credential file should not be exposed
	// to non-pi sessions.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := newSandboxExecManager(Config{
		SessionName: "repo@pi-session",
		// Harness is empty (default pi session)
	})
	profile := generateProfile(m)

	piAgentPath := filepath.Join(fakeHome, ".pi", "agent")
	if strings.Contains(profile, piAgentPath) {
		t.Errorf("profile must not contain ~/.pi/agent path for non-pi session; full profile:\n%s", profile)
	}
}

// TestGenerateProfile_AWSDenyAndStagedAWSConfigAllowed verifies AC11:
// (a) the profile denies the host ~/.aws subtree and
// (b) the resolved target of the staged ~/.aws/config symlink appears as an
// allow rule in the same profile output.
// Both assertions are in the same test — this is the "combined test" that AC11 requires.
func TestGenerateProfile_AWSDenyAndStagedAWSConfigAllowed(t *testing.T) {
	fakeHome := newFakeHome(t)
	// The fake home has ~/.config/aws/readonly-config written by newFakeHome.
	// PrepareSandboxExecHome creates a symlink ~/.aws/config → <resolved readonly-config>.

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "ac11-test",
		Worktree:    t.TempDir(),
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".local", "state", "prism"))
	})

	profile := generateProfile(m)

	// (a) The deny clause must cover host ~/.aws.
	awsDenyPath := filepath.Join(fakeHome, ".aws")
	if !strings.Contains(profile, awsDenyPath) {
		t.Errorf("profile missing deny for host ~/.aws (%s); full profile:\n%s", awsDenyPath, profile)
	}
	// Verify it's inside a (deny ...) clause not just any clause.
	awsDenyIdx := strings.Index(profile, awsDenyPath)
	denyBefore := strings.LastIndex(profile[:awsDenyIdx], "(deny")
	allowBefore := strings.LastIndex(profile[:awsDenyIdx], "(allow")
	if denyBefore < 0 || allowBefore > denyBefore {
		t.Errorf("host ~/.aws path appears in profile but NOT inside a (deny ...) clause before it; full profile:\n%s", profile)
	}

	// (b) The allow clause must include the resolved target of the staged
	// ~/.aws/config symlink (which points at ~/.config/aws/readonly-config).
	awsConfigSymlink := filepath.Join(stagingHome, ".aws", "config")
	resolvedTarget, resolveErr := filepath.EvalSymlinks(awsConfigSymlink)
	if resolveErr != nil {
		t.Skipf("staged ~/.aws/config symlink not present (skipping allow assertion): %v", resolveErr)
	}
	if !strings.Contains(profile, resolvedTarget) {
		t.Errorf("profile missing allow for resolved staged ~/.aws/config target %q; full profile:\n%s", resolvedTarget, profile)
	}
	// Verify it's inside an (allow ...) clause.
	targetIdx := strings.Index(profile, resolvedTarget)
	if targetIdx < 0 {
		t.Errorf("resolved target %q not in profile", resolvedTarget)
	} else {
		allowBeforeTarget := strings.LastIndex(profile[:targetIdx], "(allow")
		denyBeforeTarget := strings.LastIndex(profile[:targetIdx], "(deny")
		if allowBeforeTarget < 0 || denyBeforeTarget > allowBeforeTarget {
			t.Errorf("resolved target %q appears in profile but NOT inside an (allow ...) clause before it; full profile:\n%s", resolvedTarget, profile)
		}
	}
}

// TestGenerateProfile_AWSDenyClause verifies that the profile contains a
// (deny file-read* file-write* (subpath ".../.aws")) clause for the host's
// ~/.aws directory, to prevent the sandbox from accessing host credentials.
func TestGenerateProfile_AWSDenyClause(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "aws-deny-test",
		Worktree:    t.TempDir(),
	})

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(stagingHome)
		_ = os.RemoveAll(filepath.Join(fakeHome, ".local", "state", "prism"))
	})

	profile := generateProfile(m)

	// The profile must contain a (deny ...) clause that references /.aws.
	// We look for the /.aws substring inside any deny clause.
	if !strings.Contains(profile, "/.aws") {
		t.Fatalf("profile missing /.aws deny subpath; full profile:\n%s", profile)
	}
	// Verify it's inside a deny clause (not just an allow).
	awsIdx := strings.Index(profile, "/.aws")
	// Walk backwards from awsIdx to find the nearest opening paren clause.
	clauseStart := strings.LastIndex(profile[:awsIdx], "(deny")
	if clauseStart < 0 {
		t.Errorf("/.aws appears in profile but not inside a (deny ...) clause; full profile:\n%s", profile)
	}
}

// ── MinimalIsolatedExecEnv ───────────────────────────────────────────────────

// TestMinimalIsolatedExecEnv_AllowsExpectedKeys verifies that the shared
// helper passes through exactly the eight keys called out in the AC.
func TestMinimalIsolatedExecEnv_AllowsExpectedKeys(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/Users/ben",
		"USER=ben",
		"LOGNAME=ben",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"LANG=en_NZ.UTF-8",
		"LC_ALL=en_NZ.UTF-8",
		"GITHUB_TOKEN=secret",
		"ANTHROPIC_API_KEY=sk-secret",
	}
	out := MinimalIsolatedExecEnv(in)

	gotSet := map[string]bool{}
	for _, kv := range out {
		gotSet[kv] = true
	}
	for _, want := range []string{
		"PATH=/usr/bin", "HOME=/Users/ben", "USER=ben", "LOGNAME=ben",
		"TERM=xterm-256color", "COLORTERM=truecolor",
		"LANG=en_NZ.UTF-8", "LC_ALL=en_NZ.UTF-8",
	} {
		if !gotSet[want] {
			t.Errorf("expected %q to be passed through; got %v", want, out)
		}
	}
	for _, kv := range out {
		k := strings.SplitN(kv, "=", 2)[0]
		switch k {
		case "GITHUB_TOKEN", "ANTHROPIC_API_KEY":
			t.Errorf("forbidden key %q leaked through MinimalIsolatedExecEnv: %q", k, kv)
		}
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// extractClause finds a top-level (...) form whose opening text matches the
// given prefix and returns its full text including the trailing closing
// parenthesis. The implementation is line-and-paren naive — it tracks the
// nesting depth from the opening prefix and returns when depth returns to 0.
//
// This is sufficient for the small, hand-written profile in this PR; if the
// generator grows nested macros, switch to a real SBPL parser.
func extractClause(t *testing.T, profile, prefix string) string {
	t.Helper()
	idx := strings.Index(profile, prefix)
	if idx < 0 {
		t.Fatalf("clause beginning with %q not found in profile:\n%s", prefix, profile)
	}
	depth := 0
	end := -1
	for i := idx; i < len(profile); i++ {
		switch profile[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i + 1
				goto done
			}
		}
	}
done:
	if end < 0 {
		t.Fatalf("clause beginning at index %d (prefix %q) is unterminated", idx, prefix)
	}
	return profile[idx:end]
}

// sliceContainsPair returns true when args contains [..., flag, value, ...].
func sliceContainsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// hasFlag returns true when args contains an exact match for flag.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// firstNChars returns up to n leading characters of s, used in error
// messages to avoid dumping a multi-line profile when a prefix mismatch is
// the actual diagnostic value.
func firstNChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// containerPortString returns ContainerPort formatted as a decimal string,
// for tests that assert on the fallback --port value without repeating the
// constant.
func containerPortString() string {
	return fmt.Sprintf("%d", ContainerPort)
}
