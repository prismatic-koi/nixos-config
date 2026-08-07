package container

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
// the Config has an InstanceID so sessionWorkDirPath uses the instance ID
// rather than falling back to the container name. Used in tests that exercise
// the per-session work dir and profile generation.
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
	if strings.Contains(profile, "(allow iokit-open-service)") {
		t.Errorf("profile must not contain unqualified (allow iokit-open-service) — only the IOPMrootDomain registry entry is granted (issue #2249); full profile:\n%s", profile)
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

// TestGenerateProfile_IOKitOpenServiceIOPMrootDomain verifies the
// iokit-open-service allow for the IOPMrootDomain registry entry
// (issue #2249). Current Chrome for Testing acquires its power-management
// port via iokit-open-service on IOPMrootDomain — a different operation
// class from the iokit-open-user-client RootDomainUserClient allow — and
// SIGSEGVs during early init when it is denied.
//
// The allow must be scoped to exactly the IOPMrootDomain registry entry:
// no unqualified (allow iokit-open-service), no registry-entry-class-prefix
// wildcard, and no additional registry entries beyond IOPMrootDomain.
func TestGenerateProfile_IOKitOpenServiceIOPMrootDomain(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	serviceBlock := extractClause(t, profile, "(allow iokit-open-service")
	if !strings.Contains(serviceBlock, "(iokit-registry-entry-class \"IOPMrootDomain\")") {
		t.Errorf("iokit-open-service block missing the IOPMrootDomain registry-entry-class filter (issue #2249); block:\n%s", serviceBlock)
	}

	// Exactly one registry-entry filter is granted — the block must not
	// grow extra entries or switch to a prefix wildcard without a paired
	// integration test per docs/sandbox-exec-testing.md.
	if got := strings.Count(serviceBlock, "iokit-registry-entry-class"); got != 1 {
		t.Errorf("iokit-open-service block must contain exactly one iokit-registry-entry-class filter, got %d; block:\n%s", got, serviceBlock)
	}
	if strings.Contains(serviceBlock, "iokit-registry-entry-class-prefix") {
		t.Errorf("iokit-open-service block must not use a registry-entry-class-prefix wildcard (issue #2249); block:\n%s", serviceBlock)
	}

	// The iokit-open-service operation must appear exactly once in the
	// profile — a second occurrence would indicate an unqualified or
	// duplicate grant sneaking in elsewhere.
	if got := strings.Count(profile, "iokit-open-service"); got != 1 {
		t.Errorf("profile must contain exactly one iokit-open-service occurrence, got %d; full profile:\n%s", got, profile)
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

// TestGenerateProfile_SessionWorkDirAndWorktreeRules verifies that the
// profile emitted by generateProfile includes the per-session work dir,
// worktree, and bare repo as (allow file-read* file-write* (subpath ...))
// clauses when the Manager has InstanceID, Worktree, and BareRoot set.
func TestGenerateProfile_SessionWorkDirAndWorktreeRules(t *testing.T) {
	// Drive the work-dir base via $XDG_STATE_HOME (issue #2295). Since
	// PR #2277, sessionWorkDirPath honours XDG_STATE_HOME first and falls
	// back to $HOME/.local/state only when XDG_STATE_HOME is unset; setting
	// HOME alone does not control where the work dir lands on a developer
	// machine whose shell exports XDG_STATE_HOME. We also point HOME at a
	// tempdir so any home-derived carve-outs in the profile do not leak the
	// host's real home into test output. Matches the established pattern in
	// pi_invocation_resume_test.go.
	fakeStateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", fakeStateHome)
	t.Setenv("HOME", t.TempDir())
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@main",
		Worktree:    "/tmp/fake-worktree",
		BareRoot:    "/tmp/fake-bare",
		InstanceID:  "test-instance-id",
	})
	profile := generateProfile(m)

	// The profile must contain (allow file-read* file-write* file-test-existence
	// file-read-metadata ...) with the work dir, worktree, and bare repo subpaths.
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
	// The per-session work dir subpath must be present (namespaced by
	// InstanceID). Resolved from $XDG_STATE_HOME per PR #2277.
	sessionDir := filepath.Join(fakeStateHome, "prism", "sessions", "test-instance-id")
	if !strings.Contains(profile, "(subpath "+quoteSBPL(sessionDir)+")") {
		t.Errorf("profile missing the session work dir subpath %q; full profile:\n%s", sessionDir, profile)
	}
}

// TestGenerateProfile_NoStagingHomeGrant is the Step 5 of #2132 (issue
// #2250) profile-shape AC: the generated profile contains NO staging-home
// (subpath <sessionDir>/home) grant — the per-session writable scope is
// exactly the work-dir (subpath <sessionDir>) rule from PR #2221.
func TestGenerateProfile_NoStagingHomeGrant(t *testing.T) {
	// Drive the work-dir base via $XDG_STATE_HOME (issue #2295). See
	// TestGenerateProfile_SessionWorkDirAndWorktreeRules for the rationale.
	fakeStateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", fakeStateHome)
	t.Setenv("HOME", t.TempDir())
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@main",
		Worktree:    "/tmp/fake-worktree",
		InstanceID:  "test-instance-id",
	})
	profile := generateProfile(m)

	sessionDir := filepath.Join(fakeStateHome, "prism", "sessions", "test-instance-id")
	stagingHome := filepath.Join(sessionDir, "home")

	// The legacy staging-home path must not appear anywhere in the profile —
	// not as a subpath grant, not as a literal.
	if strings.Contains(profile, stagingHome) {
		t.Errorf("profile references the deleted staging-home path %q (Step 5 of #2132); full profile:\n%s", stagingHome, profile)
	}
	// The work-dir grant is present and is the per-session writable scope.
	if !strings.Contains(profile, "(subpath "+quoteSBPL(sessionDir)+")") {
		t.Errorf("profile missing the work-dir (subpath %q) grant (PR #2221); full profile:\n%s", sessionDir, profile)
	}
}

// TestGenerateProfile_AWSHomePathDenied verifies that the profile contains a
// (deny file-read* file-write* (subpath "$HOME/.aws")) clause to prevent the
// sandbox from accessing the host's raw ~/.aws directory. Only the sso/cli
// carve-outs are accessible (issue #1380/#1558).
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
// kubectl credential files be read and written at the real host paths
// (issue #1380, #1558) — since #2245 (Step 3e of #2132) the carve-outs are
// the SOLE in-sandbox capability for the two dirs (no staging symlinks).
func TestGenerateProfile_AWSSSOAndCLICarveouts(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "aws-carveout-test",
		Worktree:    t.TempDir(),
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

// TestGenerateProfile_NixTrustedSettingsReadAllow verifies that the profile
// contains a read-only, single-file (literal) allow for
// ~/.local/share/nix/trusted-settings.json (issue #2201). Flake-CLI nix
// commands consult this file whenever the target flake declares a nixConfig
// block; without the allow the read fails EPERM under deny-default and nix
// aborts the entire eval.
//
// The rule must be:
//   - a (literal ...), not a (subpath ...) — single-file scope only;
//   - read-only — no file-write* anywhere near it (nix only writes the file
//     from an interactive trust prompt, which should remain blocked).
//
// This substring assertion is necessary but not sufficient — the paired
// Darwin integration tests in
// internal/integration/sandbox_exec_nix_trusted_settings_darwin_test.go
// prove the rule is load-bearing against /usr/bin/sandbox-exec per
// docs/sandbox-exec-testing.md.
func TestGenerateProfile_NixTrustedSettingsReadAllow(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	trustedSettings := filepath.Join(fakeHome, ".local", "share", "nix", "trusted-settings.json")

	// The exact rule block as emitted by generateProfile: a read-only allow
	// with file-test-existence (so nix's pathExists probe gets ENOENT, not
	// EPERM, when the file is missing) on a single literal path.
	wantBlock := "(allow file-read* file-test-existence\n" +
		"  (literal \"" + trustedSettings + "\"))\n"
	if !strings.Contains(profile, wantBlock) {
		t.Errorf("profile missing the nix trusted-settings read-only allow block:\n%s\nfull profile:\n%s", wantBlock, profile)
	}

	// The path must appear exactly once — in the read-only block above. A
	// second occurrence would mean it leaked into another (potentially
	// writable) clause.
	if got := strings.Count(profile, trustedSettings); got != 1 {
		t.Errorf("trusted-settings path must appear exactly once in the profile (read-only literal); found %d occurrences.\nfull profile:\n%s", got, profile)
	}

	// Single-file scope: the path must never be granted as a subpath.
	if strings.Contains(profile, "(subpath \""+trustedSettings+"\")") {
		t.Errorf("trusted-settings path must be a (literal ...), not a (subpath ...); full profile:\n%s", profile)
	}
}

// TestGenerateProfile_PrismProfilesJSONReadAllow verifies that the profile
// contains a read-only, single-file (literal) allow for
// ~/.config/prism/profiles.json (issue #2286). The CLI's `prism profile
// list` / `prism profile show` and the `available_profiles` section of
// `prism agent-context` open this file directly via
// internal/config/profiles.go::LoadProfiles; without the allow the read
// fails EPERM under deny-default and the user sees a misleading
// "not found — run the system rebuild" message from inside any sandbox
// session.
//
// The rule must be:
//   - a (literal ...), not a (subpath ...) — single-file scope only, so
//     the rest of ~/.config/prism/ (e.g. accounts/, runtime-mutable state
//     from #2283) stays out of the sandbox by default;
//   - read-only — no file-write* anywhere near it (nothing in-sandbox
//     may mutate the host's declarative profile config).
//
// This substring assertion is necessary but not sufficient — the paired
// Darwin integration tests in
// internal/integration/sandbox_exec_profiles_json_darwin_test.go prove the
// rule is load-bearing against /usr/bin/sandbox-exec per
// docs/sandbox-exec-testing.md.
func TestGenerateProfile_PrismProfilesJSONReadAllow(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	profilesJSON := filepath.Join(fakeHome, ".config", "prism", "profiles.json")

	// The exact rule block as emitted by generateProfile: a read-only allow
	// with file-test-existence (so LoadProfiles' missing-file branch gets
	// ENOENT, not EPERM, when the file is absent on a fresh install) on a
	// single literal path.
	wantBlock := "(allow file-read* file-test-existence\n" +
		"  (literal \"" + profilesJSON + "\"))\n"
	if !strings.Contains(profile, wantBlock) {
		t.Errorf("profile missing the prism profiles.json read-only allow block:\n%s\nfull profile:\n%s", wantBlock, profile)
	}

	// The path must appear exactly once — in the read-only block above. A
	// second occurrence would mean it leaked into another (potentially
	// writable) clause.
	if got := strings.Count(profile, profilesJSON); got != 1 {
		t.Errorf("profiles.json path must appear exactly once in the profile (read-only literal); found %d occurrences.\nfull profile:\n%s", got, profile)
	}

	// Single-file scope: the path must never be granted as a subpath.
	if strings.Contains(profile, "(subpath \""+profilesJSON+"\")") {
		t.Errorf("profiles.json path must be a (literal ...), not a (subpath ...); full profile:\n%s", profile)
	}

	// RO must not silently become RW: confirm the literal rule does not sit
	// inside any allow clause that carries file-write*. Defensive scan that
	// matches the shape of the 3f RW-leak check in
	// TestGenerateProfile_RORealPathGrants3f.
	rule := "(literal \"" + profilesJSON + "\")"
	idx := 0
	for {
		rel := strings.Index(profile[idx:], rule)
		if rel < 0 {
			break
		}
		at := idx + rel
		clauseStart := strings.LastIndex(profile[:at], "(allow ")
		if clauseStart >= 0 && strings.Contains(profile[clauseStart:at], "file-write*") {
			t.Errorf("profiles.json literal appears inside a file-write* allow block — RO must not become RW;\nclause: %q", profile[clauseStart:at])
		}
		idx = at + len(rule)
	}
}

// TestGenerateProfile_UsageStateDirReadAllow verifies that the profile
// contains a read-only (subpath ...) allow for the prism usage snapshot
// directory (issue #2572).
//
// The bottom-bar usage segment reads current.json out of that directory
// (pi/extensions/prism.ts::readUsageSnapshot, issue #2540). Under
// deny-default the open failed and the reader — which degrades silently by
// design — rendered nothing, so the feature was invisible in every
// sandboxed session.
//
// The rule must be:
//   - a (subpath ...) on the LEAF usage directory — the writer replaces
//     current.json by atomic rename and `prism account usage` also reads
//     the sibling <account>.json files;
//   - never a grant on a PARENT: $XDG_STATE_HOME/prism holds prism.db and
//     run/ (every session's host-API socket dir, isolated per session by
//     security fix #960);
//   - read-only — no file-write* anywhere near it. Every writer goes
//     through the sidecar endpoint POST /usage/snapshot (issue #2538).
//
// This substring assertion is necessary but not sufficient — the paired
// Darwin integration tests in
// internal/integration/sandbox_exec_usage_dir_darwin_test.go prove the rule
// is load-bearing against /usr/bin/sandbox-exec per
// docs/sandbox-exec-testing.md.
func TestGenerateProfile_UsageStateDirReadAllow(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	usageDir := filepath.Join(fakeHome, ".local", "state", "prism", "usage")

	wantBlock := "(allow file-read* file-test-existence file-read-metadata\n" +
		"  (subpath \"" + usageDir + "\"))\n"
	if !strings.Contains(profile, wantBlock) {
		t.Errorf("profile missing the prism usage-dir read-only allow block:\n%s\nfull profile:\n%s", wantBlock, profile)
	}

	// Exactly once — a second occurrence would mean the path leaked into
	// another (potentially writable) clause.
	if got := strings.Count(profile, usageDir); got != 1 {
		t.Errorf("usage dir must appear exactly once in the profile (read-only subpath); found %d occurrences.\nfull profile:\n%s", got, profile)
	}

	// RO must not silently become RW: the subpath rule must not sit inside
	// any allow clause carrying file-write*.
	rule := "(subpath \"" + usageDir + "\")"
	idx := 0
	for {
		rel := strings.Index(profile[idx:], rule)
		if rel < 0 {
			break
		}
		at := idx + rel
		clauseStart := strings.LastIndex(profile[:at], "(allow ")
		if clauseStart >= 0 && strings.Contains(profile[clauseStart:at], "file-write*") {
			t.Errorf("usage dir subpath appears inside a file-write* allow block — RO must not become RW;\nclause: %q", profile[clauseStart:at])
		}
		idx = at + len(rule)
	}

	// Security AC: no path outside the usage directory becomes readable.
	// Check the HOME-side ancestor chain — the parents the issue calls out
	// as the dangerous widening. (Ancestors above HOME are not checked here:
	// on a Linux test host the fake HOME sits under /tmp, which section 3
	// grants for unrelated reasons; on Darwin, where this profile actually
	// runs, /tmp is not an ancestor of HOME.)
	for _, ancestor := range []string{
		filepath.Join(fakeHome, ".local", "state", "prism"),
		filepath.Join(fakeHome, ".local", "state"),
		filepath.Join(fakeHome, ".local"),
		fakeHome,
	} {
		for _, form := range []string{
			"(subpath \"" + ancestor + "\")",
			"(literal \"" + ancestor + "\")",
		} {
			if strings.Contains(profile, form) {
				t.Errorf("profile grants %s — an ancestor of the usage dir. Grant the LEAF only.\nfull profile:\n%s", form, profile)
			}
		}
	}
}

// TestGenerateProfile_UsageStateDirHonoursXDGStateHome is the AC that the
// grant resolves $XDG_STATE_HOME the same way pi/extensions/prism.ts does
// rather than assuming ~/.local/state. On a host exporting a non-default
// $XDG_STATE_HOME the snapshots live elsewhere, and a hardcoded path would
// grant an empty directory while the real one stayed unreadable.
func TestGenerateProfile_UsageStateDirHonoursXDGStateHome(t *testing.T) {
	fakeHome := newFakeHome(t)
	stateHome := t.TempDir()
	// newFakeHome clears XDG_STATE_HOME; this test exercises the other branch.
	t.Setenv("XDG_STATE_HOME", stateHome)

	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	xdgUsageDir := filepath.Join(stateHome, "prism", "usage")
	if !strings.Contains(profile, "(subpath \""+xdgUsageDir+"\")") {
		t.Errorf("profile does not grant the $XDG_STATE_HOME-resolved usage dir %q.\nfull profile:\n%s", xdgUsageDir, profile)
	}

	homeUsageDir := filepath.Join(fakeHome, ".local", "state", "prism", "usage")
	if strings.Contains(profile, homeUsageDir) {
		t.Errorf("profile grants the hardcoded %q even though $XDG_STATE_HOME is set to %q.\nfull profile:\n%s",
			homeUsageDir, stateHome, profile)
	}
}

// ── PrepareSandboxExec ──────────────────────────────────────────────────────

// TestPrepareSandboxExec_WritesProfileAndReturnsArgs verifies that
// PrepareSandboxExec materialises the profile to a temp file under the
// per-session state dir and returns args of the shape
// ["sandbox-exec", "-f", <profile_path>, <harness>, ...].
func TestPrepareSandboxExec_WritesProfileAndReturnsArgs(t *testing.T) {
	// PrepareSandboxExec derives the session work dir from os.UserHomeDir();
	// in the nix build sandbox $HOME is /homeless-shelter (unwritable), so we
	// redirect HOME to a tempdir. This is the AGENTS.md § "the
	// homeless-shelter failure class" pattern — the gate this test exercises
	// (issue #2168) exists to catch the inverse of this missing guard.
	t.Setenv("HOME", t.TempDir())
	// Clear XDG_STATE_HOME so the HOME-derived fallback in SessionWorkDirPath
	// is what's exercised here (the test asserts the work-dir path against
	// HOME). A developer env that exports XDG_STATE_HOME would otherwise
	// route the work dir off the tempdir HOME (issue #2263).
	t.Setenv("XDG_STATE_HOME", "")
	m := newSandboxExecManager(Config{
		SessionName:   "repo@feat",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, err := m.sessionWorkDirPath(); err == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	args, err := m.PrepareSandboxExec()
	if err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}

	if len(args) < 4 {
		t.Fatalf("args too short (%d); want at least 4 elements: %v", len(args), redactedArgs(args))
	}
	if args[0] != "sandbox-exec" {
		t.Errorf("args[0] = %q, want %q", args[0], "sandbox-exec")
	}
	if args[1] != "-f" {
		t.Errorf("args[1] = %q, want %q", args[1], "-f")
	}
	profilePath := args[2]
	if profilePath == "" {
		t.Errorf("args[2] (profile path) is empty: %v", redactedArgs(args))
	}

	// The harness binary follows the profile path. We don't pin the exact
	// string so future PRs can swap "pi" for an absolute path without
	// breaking this assertion, but it must be non-empty.
	if args[3] == "" {
		t.Errorf("args[3] (harness binary) is empty: %v", redactedArgs(args))
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

// TestSandboxExecPrepare_WorkDirFailurePropagated verifies that when
// PrepareSessionWorkDir fails (e.g. because the work-dir path is blocked by
// a pre-existing regular file), sandboxExecIsolator.Prepare returns a
// non-nil error whose message mentions the work-dir failure. The session
// must NOT launch: no profile file is written and no sandbox-exec
// subprocess is started (issue #1879 hard-fail posture).
func TestSandboxExecPrepare_WorkDirFailurePropagated(t *testing.T) {
	// Redirect HOME for the work-dir path derivation. See
	// TestPrepareSandboxExec_WritesProfileAndReturnsArgs for the rationale.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", "")
	// Build the manager and derive the work-dir path before injecting the
	// failure, so we know which path to block.
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "work-dir-fail-test",
		Worktree:    t.TempDir(),
	})

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}

	// Ensure the work dir's parent exists so we can create the blocker file.
	if err := os.MkdirAll(filepath.Dir(sessionDir), 0o755); err != nil {
		t.Fatalf("MkdirAll parent of work dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(sessionDir)) })

	// Plant a regular file at the work-dir path. When PrepareSessionWorkDir
	// calls os.MkdirAll(sessionDir, ...) it will fail with ENOTDIR because
	// the path already exists as a file, not a directory.
	if err := os.WriteFile(sessionDir, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("WriteFile blocker at work-dir path %s: %v", sessionDir, err)
	}

	// Prepare must return a non-nil error.
	iso := &sandboxExecIsolator{name: m.name}
	args, prepErr := iso.Prepare(context.Background(), m)
	if prepErr == nil {
		t.Fatalf("Prepare: expected non-nil error when the work dir cannot be created, got nil (args=%v)", redactedArgs(args))
	}

	// The error message must name the work-dir failure so the operator knows
	// what went wrong.
	errMsg := prepErr.Error()
	if !strings.Contains(errMsg, "session work dir") {
		t.Errorf("error message must mention the session work dir; got: %q", errMsg)
	}
	if !strings.Contains(errMsg, sessionDir) {
		t.Errorf("error message must include the work-dir path %q; got: %q", sessionDir, errMsg)
	}

	// No profile file must be written — the session did not advance to the
	// write-profile step.
	profilePath := m.sandboxExecProfilePath()
	if _, statErr := os.Stat(profilePath); statErr == nil {
		t.Errorf("profile file %q was written despite Prepare returning an error: launch was not aborted", profilePath)
		_ = os.Remove(profilePath)
	}
}

// TestSandboxExecPrepare_WorkDirFailurePropagated_NilArgs verifies that
// the Prepare error is a hard fail and the returned args slice is nil,
// confirming no sandbox-exec argument list was produced for the caller to
// use (regression guard for issue #1879).
func TestSandboxExecPrepare_WorkDirFailurePropagated_NilArgs(t *testing.T) {
	// Redirect HOME for the work-dir path derivation. See
	// TestPrepareSandboxExec_WritesProfileAndReturnsArgs for the rationale.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", "")
	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "work-dir-fail-nil-args-test",
		Worktree:    t.TempDir(),
	})

	sessionDir, err := m.sessionWorkDirPath()
	if err != nil {
		t.Fatalf("sessionWorkDirPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sessionDir), 0o755); err != nil {
		t.Fatalf("MkdirAll parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(sessionDir)) })
	if err := os.WriteFile(sessionDir, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("WriteFile blocker: %v", err)
	}

	iso := &sandboxExecIsolator{name: m.name}
	args, prepErr := iso.Prepare(context.Background(), m)
	if prepErr == nil {
		t.Fatalf("expected error, got nil (args=%v)", redactedArgs(args))
	}
	if args != nil {
		t.Errorf("args must be nil on error; got %v", redactedArgs(args))
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
		t.Fatalf("args too short (%d): %v", len(args), redactedArgs(args))
	}
	if args[0] != "sandbox-exec" || args[1] != "-f" {
		t.Fatalf("expected leading sandbox-exec -f; got: %v", redactedArgs(args))
	}
	// args[2] is the profile path; args[3] must be the harness binary.
	if args[3] != "pi" {
		t.Errorf("expected args[3] to be the harness binary 'pi'; got %q in %v", args[3], redactedArgs(args))
	}
}

// ── per-session prep (work dir; no staging HOME — Step 5 of #2132) ───────────────────────────────────────────────────

// newFakeHome creates a temp directory tree that mimics the credential and
// config paths that generateProfile and the work-dir writers read from
// $HOME. It sets HOME to the fake home dir for the duration of the test and
// returns the fake home path.
func newFakeHome(t *testing.T) string {
	t.Helper()
	fakeHome := t.TempDir()

	dirs := []string{
		".ssh",
		".aws",
		".config/aws",
		".config/pi",
		".config/kube",
		".cache/bun",
		".cache/nix",
		// .claude is the OLD (pre-#2243) canonical claude dir; the XDG path
		// claude-code reaches via CLAUDE_CONFIG_DIR is .config/claude
		// (issue #2243).
		".claude",
		".config/claude",
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

	// AWS readonly-config and credentials (in XDG location, delivered via
	// AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE env vars — issue #2234).
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
	// Clear XDG_STATE_HOME so the session work dir resolves under fakeHome
	// (the HOME-derived fallback in SessionWorkDirPath — issue #2263). Tests
	// that exercise the XDG_STATE_HOME branch set it explicitly.
	t.Setenv("XDG_STATE_HOME", "")

	return fakeHome
}

// TestPrepareSandboxExec_CreatesNoStagingHomeDir is the Step 5 of #2132
// (issue #2250) filesystem-shape AC at unit level: full session prep
// creates the per-session work dir but NO
// ~/.local/state/prism/sessions/<id>/home/ directory — the staging-HOME
// mechanism is deleted.
func TestPrepareSandboxExec_CreatesNoStagingHomeDir(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName:   "repo@feat",
		InstanceID:    "no-staging-home-test",
		Worktree:      t.TempDir(),
		AllocatedPort: 14012,
	})
	t.Cleanup(func() { _ = os.Remove(m.sandboxExecProfilePath()) })

	if _, err := m.PrepareSandboxExec(); err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}

	sessionDir := filepath.Join(fakeHome, ".local", "state", "prism", "sessions", "no-staging-home-test")
	t.Cleanup(func() { _ = os.RemoveAll(sessionDir) })

	// The work dir exists with the generated configs.
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("session work dir not created: %v", err)
	}

	// The staging HOME does NOT exist — nothing creates <sessionDir>/home/
	// any more (issue #2250 functional AC).
	if _, err := os.Lstat(filepath.Join(sessionDir, "home")); err == nil {
		t.Errorf("staging HOME dir %s/home was created — the mechanism was deleted in Step 5 of #2132", sessionDir)
	}
}

// TestSandboxExecCleanup_RemovesSessionWorkDir verifies that EnsureRemoved
// removes the per-session work dir created by PrepareSessionWorkDir, leaving
// no orphaned per-session directory (issue #2250 edge-case AC at unit
// level).
func TestSandboxExecCleanup_RemovesSessionWorkDir(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "cleanup-test",
	})

	sessionDir, err := m.PrepareSessionWorkDir()
	if err != nil {
		t.Fatalf("PrepareSessionWorkDir: %v", err)
	}

	// Verify it exists.
	if _, statErr := os.Stat(sessionDir); statErr != nil {
		t.Fatalf("session work dir not created: %v", statErr)
	}

	// Call EnsureRemoved (uses a background context;
	// sandboxExecIsolator.Shutdown is a no-op).
	m.EnsureRemoved(context.Background())

	// The work dir must be gone.
	if _, statErr := os.Stat(sessionDir); statErr == nil {
		t.Errorf("session work dir still exists after EnsureRemoved: %s", sessionDir)
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

// TestGenerateProfile_AWSDenyPresent_NoXDGTargetAllows verifies the
// post-#2234 AWS shape of the profile:
// (a) the profile denies the host ~/.aws subtree, and
// (b) the profile carries no allow referencing the resolved aws XDG targets
// (the read capability rides the env-var route + #2211 allowlist instead —
// a literal grant on the XDG symlink path would be inert per the #2132 §2
// mechanism note).
func TestGenerateProfile_AWSDenyPresent_NoXDGTargetAllows(t *testing.T) {
	fakeHome := newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "ac11-test",
		Worktree:    t.TempDir(),
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

	// (b) The resolved XDG targets must not appear anywhere in the profile.
	for _, xdgSrc := range []string{
		filepath.Join(fakeHome, ".config", "aws", "readonly-config"),
		filepath.Join(fakeHome, ".config", "aws", "credentials"),
	} {
		resolved, resolveErr := filepath.EvalSymlinks(xdgSrc)
		if resolveErr != nil {
			t.Fatalf("fixture: EvalSymlinks(%s): %v", xdgSrc, resolveErr)
		}
		if strings.Contains(profile, resolved) {
			t.Errorf("profile references resolved aws XDG target %q — the read rides the env-var route + #2211 allowlist (#2234); full profile:\n%s",
				resolved, profile)
		}
	}
}

// TestGenerateProfile_ClaudeConfigDirRWSubpathRule verifies that
// generateProfile emits an RW (subpath ~/.config/claude) rule (issue #2243,
// Step 3c of #2132). claude-code resolves its config dir (and .claude.json)
// via CLAUDE_CONFIG_DIR at the host XDG path; the dir is a plain host
// directory (not sops-backed), so this explicit grant — not the #2211
// secrets.d allowlist — is the sole in-sandbox capability for the path.
//
// The rule must be emitted even when the dir does not yet exist —
// sandbox-exec silently ignores (subpath ...) rules for non-existent paths
// (same shape as the ~/.pi/agent rule), and the nix module's hm activation
// creates the dir on managed hosts.
func TestGenerateProfile_ClaudeConfigDirRWSubpathRule(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// Deliberately do NOT create ~/.config/claude — the rule must be
	// emitted unconditionally.

	m := newSandboxExecManager(Config{
		SessionName: "repo@claude-grant",
	})
	profile := generateProfile(m)

	claudeConfigDir := filepath.Join(fakeHome, ".config", "claude")
	subpathRule := "(subpath " + quoteSBPL(claudeConfigDir) + ")"
	idx := strings.Index(profile, subpathRule)
	if idx < 0 {
		t.Fatalf("profile missing %s rule; full profile:\n%s", subpathRule, profile)
	}

	// The rule must sit inside an (allow ... file-write* ...) clause — RW,
	// not RO — and inside an allow, not a deny.
	clauseStart := strings.LastIndex(profile[:idx], "(allow ")
	denyStart := strings.LastIndex(profile[:idx], "(deny ")
	if clauseStart < 0 || denyStart > clauseStart {
		t.Fatalf("~/.config/claude subpath rule is not inside an (allow ...) clause; full profile:\n%s", profile)
	}
	clause := profile[clauseStart:idx]
	if !strings.Contains(clause, "file-write*") {
		t.Errorf("~/.config/claude allow clause lacks file-write* (must be RW — claude writes config/history/token refreshes); clause: %q", clause)
	}
	if !strings.Contains(clause, "file-read*") {
		t.Errorf("~/.config/claude allow clause lacks file-read*; clause: %q", clause)
	}
}

// TestGenerateProfile_RWRealPathGrants3e verifies that generateProfile emits
// the section-5e RW grant block on the real host paths for ~/.cache/nix,
// ~/.cache/bun, ~/.npm, and ~/.mcp-auth (issue #2245, Step 3e of #2132).
// The block replaces the dropped RW staging symlinks; none of the paths is
// sops-backed, so this grant — not the #2211 allowlist — is the sole
// in-sandbox capability.
//
// The block must be emitted even when none of the dirs exists on the host —
// sandbox-exec silently ignores (subpath ...) rules for non-existent paths,
// and fresh machines (e.g. no ~/.mcp-auth) must not lose the grant shape.
func TestGenerateProfile_RWRealPathGrants3e(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// Deliberately create NONE of the four dirs — emission is unconditional.

	m := newSandboxExecManager(Config{SessionName: "repo@3e-grants"})
	profile := generateProfile(m)

	wantBlock := "(allow file-read* file-write* file-test-existence file-read-metadata\n" +
		"  (subpath " + quoteSBPL(filepath.Join(fakeHome, ".cache", "nix")) + ")\n" +
		"  (subpath " + quoteSBPL(filepath.Join(fakeHome, ".cache", "bun")) + ")\n" +
		"  (subpath " + quoteSBPL(filepath.Join(fakeHome, ".npm")) + ")\n" +
		"  (subpath " + quoteSBPL(filepath.Join(fakeHome, ".mcp-auth")) + "))\n"
	if !strings.Contains(profile, wantBlock) {
		t.Errorf("profile missing the 3e RW grant block:\n%s\nfull profile:\n%s", wantBlock, profile)
	}
}

// TestGenerateProfile_RORealPathGrants3f verifies that generateProfile emits
// the section-5f RO grant block on the real host paths for
// ~/.cache/prism/clipboard and ~/.config/prism/agents (issue #2245, Step 3f
// of #2132). The block replaces the dropped RO staging symlinks. It must be
// read-only — RO must not silently become RW — and emitted even when the
// dirs do not exist on the host.
func TestGenerateProfile_RORealPathGrants3f(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// Deliberately create NEITHER dir — emission is unconditional.

	m := newSandboxExecManager(Config{SessionName: "repo@3f-grants"})
	profile := generateProfile(m)

	wantBlock := "(allow file-read* file-test-existence\n" +
		"  (subpath " + quoteSBPL(filepath.Join(fakeHome, ".cache", "prism", "clipboard")) + ")\n" +
		"  (subpath " + quoteSBPL(filepath.Join(fakeHome, ".config", "prism", "agents")) + "))\n"
	if !strings.Contains(profile, wantBlock) {
		t.Errorf("profile missing the 3f RO grant block:\n%s\nfull profile:\n%s", wantBlock, profile)
	}

	// RO means RO: neither path may appear inside a block that carries
	// file-write*.
	for _, path := range []string{
		filepath.Join(fakeHome, ".cache", "prism", "clipboard"),
		filepath.Join(fakeHome, ".config", "prism", "agents"),
	} {
		rule := "(subpath " + quoteSBPL(path) + ")"
		idx := 0
		for {
			rel := strings.Index(profile[idx:], rule)
			if rel < 0 {
				break
			}
			at := idx + rel
			clauseStart := strings.LastIndex(profile[:at], "(allow ")
			if clauseStart >= 0 && strings.Contains(profile[clauseStart:at], "file-write*") {
				t.Errorf("3f path %q appears inside a file-write* allow block — RO must not become RW;\nclause: %q", path, profile[clauseStart:at])
			}
			idx = at + len(rule)
		}
	}
}

// TestGenerateProfile_NixProfileROGrant verifies the section-5g grant for
// ~/.nix-profile (issue #2245, Step 3f of #2132). ~/.nix-profile is a
// symlink on the host, and SBPL filters evaluate the resolved target for
// open-class operations, so the grant must carry BOTH a literal allow on the
// symlink node (readlink/lstat) and an RO rule on the EvalSymlinks-resolved
// target (reads through the link). Both must be read-only.
func TestGenerateProfile_NixProfileROGrant(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Mirror the real host shape: ~/.nix-profile → a real directory.
	profileTarget := filepath.Join(fakeHome, "nix-profile-target")
	if err := os.MkdirAll(profileTarget, 0o700); err != nil {
		t.Fatalf("create profile target: %v", err)
	}
	nixProfile := filepath.Join(fakeHome, ".nix-profile")
	if err := os.Symlink(profileTarget, nixProfile); err != nil {
		t.Fatalf("symlink ~/.nix-profile: %v", err)
	}

	m := newSandboxExecManager(Config{SessionName: "repo@nix-profile-grant"})
	profile := generateProfile(m)

	resolved, evalErr := filepath.EvalSymlinks(nixProfile)
	if evalErr != nil {
		t.Fatalf("fixture: EvalSymlinks(~/.nix-profile): %v", evalErr)
	}
	wantBlock := "(allow file-read* file-test-existence\n" +
		"  (literal " + quoteSBPL(nixProfile) + ")\n" +
		"  (subpath " + quoteSBPL(resolved) + "))\n"
	if !strings.Contains(profile, wantBlock) {
		t.Errorf("profile missing the 5g nix-profile RO block (literal link node + resolved subpath):\n%s\nfull profile:\n%s", wantBlock, profile)
	}

	// Neither the link node nor the resolved target may sit in a
	// file-write* block.
	for _, rule := range []string{
		"(literal " + quoteSBPL(nixProfile) + ")",
		"(subpath " + quoteSBPL(resolved) + ")",
	} {
		idx := strings.Index(profile, rule)
		if idx < 0 {
			continue // already reported above
		}
		clauseStart := strings.LastIndex(profile[:idx], "(allow ")
		if clauseStart >= 0 && strings.Contains(profile[clauseStart:idx], "file-write*") {
			t.Errorf("nix-profile rule %q appears inside a file-write* allow block — must be RO", rule)
		}
	}
}

// TestGenerateProfile_NixProfileAbsent_LiteralStillEmitted covers the
// fresh-machine edge case for section 5g: when ~/.nix-profile does not exist
// on the host, EvalSymlinks fails, no resolved-target rule is emitted, and
// the literal allow on the (future) symlink node is still present — profile
// generation must not break (sandbox-exec ignores rules for non-existent
// paths).
func TestGenerateProfile_NixProfileAbsent_LiteralStillEmitted(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// No ~/.nix-profile created.

	m := newSandboxExecManager(Config{SessionName: "repo@nix-profile-absent"})
	profile := generateProfile(m)

	nixProfile := filepath.Join(fakeHome, ".nix-profile")
	wantBlock := "(allow file-read* file-test-existence\n" +
		"  (literal " + quoteSBPL(nixProfile) + "))\n"
	if !strings.Contains(profile, wantBlock) {
		t.Errorf("profile missing the literal-only 5g block when ~/.nix-profile is absent:\n%s\nfull profile:\n%s", wantBlock, profile)
	}
}

// TestPrepareSandboxExec_MinimalHomeNoOptionalDirs is the absent-host-dirs
// edge-case AC for issue #2245: on a host with NONE of the 3d/3e/3f source
// dirs (fresh machine — no ~/.mcp-auth, ~/.npm, caches, ~/.nix-profile,
// ~/.pi, prism config), full session prep (work dir + profile write) must
// succeed and the generated profile must still carry the 3e/3f grant
// blocks.
func TestPrepareSandboxExec_MinimalHomeNoOptionalDirs(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_STATE_HOME", "")

	m := newSandboxExecManager(Config{
		SessionName:   "repo@minimal-home",
		Worktree:      t.TempDir(),
		AllocatedPort: 14011,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
		if sessionDir, err := m.sessionWorkDirPath(); err == nil {
			_ = os.RemoveAll(sessionDir)
		}
	})

	args, err := m.PrepareSandboxExec()
	if err != nil {
		t.Fatalf("PrepareSandboxExec on a minimal home must succeed: %v", err)
	}
	if len(args) < 3 {
		t.Fatalf("unexpected args shape: %v", redactedArgs(args))
	}
	profileBytes, readErr := os.ReadFile(args[2])
	if readErr != nil {
		t.Fatalf("read generated profile: %v", readErr)
	}
	profile := string(profileBytes)
	for _, want := range []string{
		"(subpath " + quoteSBPL(filepath.Join(fakeHome, ".mcp-auth")) + ")",
		"(subpath " + quoteSBPL(filepath.Join(fakeHome, ".npm")) + ")",
		"(subpath " + quoteSBPL(filepath.Join(fakeHome, ".config", "prism", "agents")) + ")",
		"(literal " + quoteSBPL(filepath.Join(fakeHome, ".nix-profile")) + ")",
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("minimal-home profile missing %s;\nfull profile:\n%s", want, profile)
		}
	}
}

// TestGenerateProfile_AWSDenyClause verifies that the profile contains a
// (deny file-read* file-write* (subpath ".../.aws")) clause for the host's
// ~/.aws directory, to prevent the sandbox from accessing host credentials.
func TestGenerateProfile_AWSDenyClause(t *testing.T) {
	newFakeHome(t)

	m := newSandboxExecManagerWithInstance(Config{
		SessionName: "repo@feat",
		InstanceID:  "aws-deny-test",
		Worktree:    t.TempDir(),
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
			t.Errorf("expected %q to be passed through; got %v", want, redactedArgs(out))
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

// ── sops secrets.d deny + named re-allow exceptions (issue #2211) ────────────

// secretsDDenyReadHeader is the verbatim opening of the secrets.d read-deny
// rule as emitted by generateProfile. Kept as a constant so the assertions
// below stay in lockstep with the generator.
const secretsDDenyReadHeader = "(deny file-read*\n" +
	"  (require-all\n" +
	"    (require-any\n" +
	"      (regex #\"^/var/folders/.*/secrets\\.d/\")\n" +
	"      (regex #\"^/private/var/folders/.*/secrets\\.d/\"))\n"

// secretsDDenyWriteRule is the verbatim secrets.d write-deny rule.
const secretsDDenyWriteRule = "(deny file-write*\n" +
	"  (regex #\"^/var/folders/.*/secrets\\.d/\")\n" +
	"  (regex #\"^/private/var/folders/.*/secrets\\.d/\"))\n"

// TestGenerateProfile_SecretsDDenyBlocksPresent verifies that the profile
// contains the secrets.d write-deny and read-deny rules covering both the
// /var and /private/var symlink forms, and that the read-deny appears AFTER
// the broad /private/var/folders allow it narrows (the deny-after-broader-
// allow precedence shape, issue #2211).
func TestGenerateProfile_SecretsDDenyBlocksPresent(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	if !strings.Contains(profile, secretsDDenyWriteRule) {
		t.Errorf("profile missing the secrets.d write-deny rule:\n%s\nfull profile:\n%s",
			secretsDDenyWriteRule, profile)
	}
	if !strings.Contains(profile, secretsDDenyReadHeader) {
		t.Errorf("profile missing the secrets.d read-deny rule header:\n%s\nfull profile:\n%s",
			secretsDDenyReadHeader, profile)
	}

	broadAllow := "  (subpath \"/private/var/folders\")\n"
	broadIdx := strings.Index(profile, broadAllow)
	denyIdx := strings.Index(profile, secretsDDenyReadHeader)
	if broadIdx < 0 {
		t.Fatalf("profile missing the broad /private/var/folders allow; full profile:\n%s", profile)
	}
	if denyIdx < broadIdx {
		t.Errorf("secrets.d read-deny (index %d) must appear after the broad /private/var/folders allow (index %d) — SBPL deny-after-broader-allow shape; full profile:\n%s",
			denyIdx, broadIdx, profile)
	}
}

// fakeSopsChain replaces the plain-file credential sources in a newFakeHome
// tree with sops-style symlink chains:
//
//	<fakeHome>/<sourceRel>  →  <secretsRoot>/secrets.d/<counter>/<secretName>
//
// creating the concrete secret file with dummy content. Returns the secrets.d
// base dir (the parent of the counter dir).
func fakeSopsChain(t *testing.T, fakeHome, secretsBase, counter string, entries map[string]string) {
	t.Helper()
	for sourceRel, secretName := range entries {
		concrete := filepath.Join(secretsBase, counter, filepath.FromSlash(secretName))
		if err := os.MkdirAll(filepath.Dir(concrete), 0o700); err != nil {
			t.Fatalf("create fake secrets dir for %s: %v", secretName, err)
		}
		if err := os.WriteFile(concrete, []byte("dummy-secret"), 0o600); err != nil {
			t.Fatalf("write fake secret %s: %v", secretName, err)
		}
		source := filepath.Join(fakeHome, filepath.FromSlash(sourceRel))
		_ = os.Remove(source) // replace the plain file newFakeHome created
		if err := os.Symlink(concrete, source); err != nil {
			t.Fatalf("symlink fake sops chain %s → %s: %v", source, concrete, err)
		}
	}
}

// TestGenerateProfile_SecretsDAllowlistDerivedFromStableSources verifies
// that, when the stable host sources resolve into a sops secrets.d tree,
// generateProfile emits a require-not exception for exactly each derived
// secret name — counter-independent ([0-9]+) and $-anchored — and nothing
// else (no wildcard matching future secrets, issue #2211 AC).
func TestGenerateProfile_SecretsDAllowlistDerivedFromStableSources(t *testing.T) {
	fakeHome := newFakeHome(t)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")
	fakeSopsChain(t, fakeHome, secretsBase, "389", map[string]string{
		".ssh/prismatic-koi-ed25519":                "ssh/prismatic-koi-ed25519",
		".ssh/prismatic-koi-ed25519.pub":            "ssh/prismatic-koi-ed25519.pub",
		".ssh/prismatic-koi-ed25519-signingkey":     "ssh/prismatic-koi-ed25519-signingkey",
		".ssh/prismatic-koi-ed25519-signingkey.pub": "ssh/prismatic-koi-ed25519-signingkey.pub",
		".config/aws/readonly-config":               "aws-readonly-config",
		".config/kube/agents-config":                "workreadonlykube",
	})
	// ~/.config/aws/credentials stays the plain file newFakeHome created —
	// it resolves to itself (not a secrets.d path) and must produce no
	// exception. Remove it entirely to also cover the absent-source path
	// (currently absent on the real host).
	if err := os.Remove(filepath.Join(fakeHome, ".config", "aws", "credentials")); err != nil {
		t.Fatalf("remove fake aws credentials: %v", err)
	}

	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	expected := []string{
		`    (require-not (regex #"/secrets\.d/[0-9]+/ssh/prismatic-koi-ed25519$"))` + "\n",
		`    (require-not (regex #"/secrets\.d/[0-9]+/ssh/prismatic-koi-ed25519\.pub$"))` + "\n",
		`    (require-not (regex #"/secrets\.d/[0-9]+/ssh/prismatic-koi-ed25519-signingkey$"))` + "\n",
		`    (require-not (regex #"/secrets\.d/[0-9]+/ssh/prismatic-koi-ed25519-signingkey\.pub$"))` + "\n",
		`    (require-not (regex #"/secrets\.d/[0-9]+/aws-readonly-config$"))` + "\n",
		`    (require-not (regex #"/secrets\.d/[0-9]+/workreadonlykube$"))` + "\n",
	}
	for _, exp := range expected {
		if !strings.Contains(profile, exp) {
			t.Errorf("profile missing expected secrets.d allowlist exception:\n%s\nfull profile:\n%s", exp, profile)
		}
	}

	// Exactly the six inventoried exceptions — nothing else. A higher count
	// would mean a wildcard or an un-inventoried name slipped in.
	if got := strings.Count(profile, "(require-not "); got != len(expected) {
		t.Errorf("expected exactly %d require-not exceptions, got %d; full profile:\n%s",
			len(expected), got, profile)
	}
	// The concrete counter must never be baked into an exception — that
	// would break the #1410/#1573 rotation property.
	if strings.Contains(profile, `/secrets\.d/389/`) {
		t.Errorf("profile bakes the concrete secrets.d counter into a regex (must use [0-9]+); full profile:\n%s", profile)
	}
}

// TestGenerateProfile_SecretsDAllowlistEmptyWithoutSopsChains verifies that
// on a host whose credential sources are plain files (no sops), the secrets.d
// deny rules are still emitted but carry zero exceptions.
func TestGenerateProfile_SecretsDAllowlistEmptyWithoutSopsChains(t *testing.T) {
	_ = newFakeHome(t) // plain files only — nothing resolves into secrets.d

	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	if !strings.Contains(profile, secretsDDenyWriteRule) || !strings.Contains(profile, secretsDDenyReadHeader) {
		t.Fatalf("secrets.d deny rules must be emitted even with no allowlist sources; full profile:\n%s", profile)
	}
	if got := strings.Count(profile, "(require-not "); got != 0 {
		t.Errorf("expected zero require-not exceptions on a non-sops host, got %d; full profile:\n%s", got, profile)
	}
}

// TestGenerateProfile_SecretsDAllowlistEscapesRegexMeta verifies that regex
// metacharacters in a derived secret name are escaped so the emitted
// exception matches the name literally.
func TestGenerateProfile_SecretsDAllowlistEscapesRegexMeta(t *testing.T) {
	fakeHome := newFakeHome(t)
	secretsBase := filepath.Join(t.TempDir(), "secrets.d")
	fakeSopsChain(t, fakeHome, secretsBase, "7", map[string]string{
		".ssh/we.ird+key": "ssh/we.ird+key",
	})

	m := newSandboxExecManager(Config{
		SessionName:      "repo@main",
		SshAccessKeyName: "we.ird+key",
	})
	profile := generateProfile(m)

	exp := `    (require-not (regex #"/secrets\.d/[0-9]+/ssh/we\.ird\+key$"))` + "\n"
	if !strings.Contains(profile, exp) {
		t.Errorf("profile missing escaped exception %q; full profile:\n%s", exp, profile)
	}
}

// TestSecretsDRelativeName covers the resolved-path → secret-name extraction.
func TestSecretsDRelativeName(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"/private/var/folders/hr/x/T/secrets.d/389/github_token", "github_token", true},
		{"/private/var/folders/hr/x/T/secrets.d/389/ssh/prismatic-koi-ed25519", "ssh/prismatic-koi-ed25519", true},
		{"/var/folders/hr/x/T/secrets.d/1/a", "a", true},
		{"/private/var/folders/hr/x/T/secrets.d/age-keys.txt", "", false}, // no counter dir
		{"/private/var/folders/hr/x/T/secrets.d/v389/name", "", false},    // non-numeric counter
		{"/private/var/folders/hr/x/T/secrets.d/389/", "", false},         // empty name
		{"/home/user/.ssh/plain-key", "", false},                          // not a secrets.d path
		{"/private/var/folders/hr/x/T/secrets.d/389", "", false},          // counter only, no name
	}
	for _, tc := range cases {
		gotName, gotOK := secretsDRelativeName(tc.in)
		if gotName != tc.wantName || gotOK != tc.wantOK {
			t.Errorf("secretsDRelativeName(%q) = (%q, %v); want (%q, %v)",
				tc.in, gotName, gotOK, tc.wantName, tc.wantOK)
		}
	}
}

// TestRegexQuotePath covers the AppleMatch metacharacter escaping helper.
func TestRegexQuotePath(t *testing.T) {
	cases := map[string]string{
		"plain-name":      "plain-name",
		"ssh/key.pub":     `ssh/key\.pub`,
		"we.ird+key":      `we\.ird\+key`,
		`back\slash`:      `back\\slash`,
		"a(b)[c]{d}|e^f$": `a\(b\)\[c\]\{d\}\|e\^f\$`,
		"q?u*e":           `q\?u\*e`,
	}
	for in, want := range cases {
		if got := regexQuotePath(in); got != want {
			t.Errorf("regexQuotePath(%q) = %q; want %q", in, got, want)
		}
	}
}

// ── section 5k: Go module cache + build cache (issue #2621) ─────────────────

// TestGenerateProfile_GoCacheRWGrant verifies that generateProfile emits the
// section-5k read-write grant for the two Go cache directories, so the repo
// AGENTS.md quality gate (`go build ./...`, `go test ./...`) runs inside a
// Darwin worker with no extra environment setup (issue #2621).
//
// The paths are asserted as LITERALS here, not derived from goCacheDirs, so
// that a change to either the helper or the emitted block has to be a
// deliberate edit of both. The grant must be read-write: go writes module
// downloads plus cache/lock under GOMODCACHE and build outputs under GOCACHE,
// so a read-only grant would not make the documented command work.
//
// The block is emitted unconditionally — the fake home here contains neither
// directory, matching a fresh host. sandbox-exec silently ignores
// (subpath ...) rules for non-existent paths.
//
// This substring assertion is necessary but not sufficient — the paired
// Darwin integration tests in
// internal/integration/sandbox_exec_go_cache_darwin_test.go prove the rule is
// load-bearing against /usr/bin/sandbox-exec per docs/sandbox-exec-testing.md.
func TestGenerateProfile_GoCacheRWGrant(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// Deliberately create NEITHER dir — emission is unconditional.

	m := newSandboxExecManager(Config{SessionName: "repo@go-cache"})
	profile := generateProfile(m)

	// One clause per directory: the two do not carry the same operations.
	// GOMODCACHE holds module source and must NOT be executable; GOCACHE
	// stores linked binaries and must be.
	wantModCache := "(allow file-read* file-write* file-test-existence file-read-metadata\n" +
		"  (subpath " + quoteSBPL(filepath.Join(fakeHome, "go", "pkg", "mod")) + "))\n"
	if !strings.Contains(profile, wantModCache) {
		t.Errorf("profile missing the section-5k GOMODCACHE grant:\n%s\nfull profile:\n%s", wantModCache, profile)
	}

	wantBuildCache := "(allow file-read* file-write* file-test-existence file-read-metadata file-map-executable\n" +
		"  (subpath " + quoteSBPL(filepath.Join(fakeHome, "Library", "Caches", "go-build")) + "))\n"
	if !strings.Contains(profile, wantBuildCache) {
		t.Errorf("profile missing the section-5k GOCACHE grant (with file-map-executable):\n%s\nfull profile:\n%s", wantBuildCache, profile)
	}
}

// TestGenerateProfile_GoCacheExecutabilityIsAsymmetric pins the deliberate
// split in the section-5k grant (issue #2621).
//
// GOCACHE carries file-map-executable because the build cache stores linked
// executables and cmd/go can serve a test binary straight out of it on a warm
// build; in a (version 3) profile file-read* does not imply
// file-map-executable, so without the flag a warm-cache run can fail EPERM
// where the cold run that linked into $WORK succeeded.
//
// GOMODCACHE must NOT carry it: the module cache holds source, nothing execs
// out of it, and an executable-mappable module cache would let a sandboxed
// process run code it planted among the dependency sources.
func TestGenerateProfile_GoCacheExecutabilityIsAsymmetric(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	m := newSandboxExecManager(Config{SessionName: "repo@go-cache-exec"})
	profile := generateProfile(m)

	modCache := filepath.Join(fakeHome, "go", "pkg", "mod")
	buildCache := filepath.Join(fakeHome, "Library", "Caches", "go-build")

	// clauseFor returns the (allow ...) clause containing the given subpath.
	clauseFor := func(path string) string {
		rule := "(subpath " + quoteSBPL(path) + ")"
		at := strings.Index(profile, rule)
		if at < 0 {
			t.Fatalf("profile has no rule for %q.\nfull profile:\n%s", path, profile)
		}
		start := strings.LastIndex(profile[:at], "(allow ")
		if start < 0 {
			t.Fatalf("rule for %q is not inside an (allow ...) clause.\nfull profile:\n%s", path, profile)
		}
		return profile[start : at+len(rule)]
	}

	if clause := clauseFor(buildCache); !strings.Contains(clause, "file-map-executable") {
		t.Errorf("GOCACHE clause lacks file-map-executable — a warm-cache `go test` can fail EPERM;\nclause: %q", clause)
	}
	if clause := clauseFor(modCache); strings.Contains(clause, "file-map-executable") {
		t.Errorf("GOMODCACHE clause carries file-map-executable — the module cache holds source and must not be executable;\nclause: %q", clause)
	}
}

// TestGenerateProfile_GoCacheGrantsNothingOutsideTheCaches is the security AC
// of issue #2621: the widening covers the two Go cache leaves and nothing
// else.
//
// Each ancestor is checked in both (subpath ...) and (literal ...) form:
//
//   - ~/go — would expose ~/go/bin, where `go install` drops binaries that
//     are typically on the host's PATH. A sandboxed agent must not be able to
//     plant an executable the user later runs.
//   - ~/go/pkg — the GOPATH package root, wider than the module cache.
//   - ~/Library/Caches — the user's entire cache tree (browser, applications).
//   - ~/Library, ~ — self-evidently out of bounds.
//
// ~/Library/Application Support/go (GOENV plus the Go telemetry dir) is
// checked as an explicit non-grant too: go tolerates an unreadable env file
// and telemetry is best-effort, so neither needs a grant.
func TestGenerateProfile_GoCacheGrantsNothingOutsideTheCaches(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// No BareRoot: the section-6b ancestor block grants (subpath HOME) for
	// file-test-existence when BareRoot is set, which would mask this check.
	m := newSandboxExecManager(Config{SessionName: "repo@go-cache-scope"})
	profile := generateProfile(m)

	for _, forbidden := range []string{
		filepath.Join(fakeHome, "go"),
		filepath.Join(fakeHome, "go", "pkg"),
		filepath.Join(fakeHome, "Library", "Caches"),
		filepath.Join(fakeHome, "Library"),
		filepath.Join(fakeHome, "Library", "Application Support", "go"),
		fakeHome,
	} {
		for _, form := range []string{
			"(subpath " + quoteSBPL(forbidden) + ")",
			"(literal " + quoteSBPL(forbidden) + ")",
		} {
			if strings.Contains(profile, form) {
				t.Errorf("profile grants %s — outside the two Go cache dirs. Grant the LEAVES only.\nfull profile:\n%s", form, profile)
			}
		}
	}

	// Each granted leaf appears exactly once: a second occurrence would mean
	// the path leaked into another clause.
	for _, dir := range goCacheDirs(fakeHome) {
		if got := strings.Count(profile, quoteSBPL(dir.path)); got != 1 {
			t.Errorf("Go cache dir %s must appear exactly once in the profile; found %d occurrences.\nfull profile:\n%s", dir.path, got, profile)
		}
	}
}

// TestGoCacheDirs_AreTheGoDarwinDefaults pins the resolved paths to the Go
// toolchain's Darwin defaults (issue #2621).
//
// The values are exact, not approximate: the sandbox env forwards no GO*
// variable and go's env file under ~/Library/Application Support is not
// granted, so the in-sandbox toolchain always resolves GOMODCACHE to
// $HOME/go/pkg/mod and GOCACHE to $HOME/Library/Caches/go-build. If Go ever
// changes those defaults, this test is the tripwire.
func TestGoCacheDirs_AreTheGoDarwinDefaults(t *testing.T) {
	got := goCacheDirs("/Users/example")
	want := []goCacheDir{
		{path: "/Users/example/go/pkg/mod", mapExecutable: false},
		{path: "/Users/example/Library/Caches/go-build", mapExecutable: true},
	}
	if len(got) != len(want) {
		t.Fatalf("goCacheDirs returned %d entries (%v); want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].path != want[i].path {
			t.Errorf("goCacheDirs[%d].path = %q; want %q", i, got[i].path, want[i].path)
		}
		if got[i].mapExecutable != want[i].mapExecutable {
			t.Errorf("goCacheDirs[%d].mapExecutable = %v; want %v (%s)",
				i, got[i].mapExecutable, want[i].mapExecutable, want[i].path)
		}
	}

	// Empty home yields no entries — the caller must not emit a grant rooted
	// at "/" or an SBPL clause with no path filter.
	if dirs := goCacheDirs(""); dirs != nil {
		t.Errorf("goCacheDirs(\"\") = %v; want nil", dirs)
	}
}

// TestGenerateProfile_GoCacheGrantAbsentWithoutHome verifies that an
// unresolvable home emits no Go cache clause at all — never an (allow ...)
// with no path filter, which SBPL would read as "everything" (issue #2621).
func TestGenerateProfile_GoCacheGrantAbsentWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")

	m := newSandboxExecManager(Config{SessionName: "repo@go-cache-nohome"})
	profile := generateProfile(m)

	if strings.Contains(profile, "go-build") {
		t.Errorf("profile references go-build with no resolvable home:\n%s", profile)
	}
	if strings.Contains(profile, "(allow file-read* file-write* file-test-existence file-read-metadata\n)") {
		t.Errorf("profile contains a filter-less allow clause:\n%s", profile)
	}
}

// TestEnsureGoCacheDirs_CreatesBothDirs verifies that the Prepare-time
// helper materialises the two directories the section-5k grant covers
// (issue #2621).
//
// This is load-bearing, not cosmetic: a (subpath ...) grant on a path that
// does not exist is a silent no-op, and the sandboxed process cannot create
// the path itself because MkdirAll would first have to mkdir the ungranted
// parents (~/go, ~/go/pkg, ~/Library/Caches) and gets EPERM. Without the
// host-side creation, a machine that has never run go outside a sandbox
// still fails the documented quality gate.
func TestEnsureGoCacheDirs_CreatesBothDirs(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	ensureGoCacheDirs()

	for _, dir := range goCacheDirs(fakeHome) {
		info, err := os.Stat(dir.path)
		if err != nil {
			t.Errorf("ensureGoCacheDirs did not create %s: %v", dir.path, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s exists but is not a directory", dir.path)
		}
	}
}

// TestEnsureGoCacheDirs_UnwritableHomeIsNotFatal verifies the best-effort
// posture (issue #2621): the Go caches are a build convenience, unlike the
// work-dir git/ssh configs whose absence hard-fails Prepare, so a home that
// cannot be written to must not stop a session from starting. This is the
// homeless-shelter shape ($HOME unwritable inside the nix build sandbox).
func TestEnsureGoCacheDirs_UnwritableHomeIsNotFatal(t *testing.T) {
	base := t.TempDir()
	unwritable := filepath.Join(base, "ro-home")
	if err := os.MkdirAll(unwritable, 0o500); err != nil {
		t.Fatalf("create read-only fake home: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o700) })
	t.Setenv("HOME", unwritable)

	// Must return normally — the only contract is "does not panic, does not
	// abort the caller".
	ensureGoCacheDirs()

	if _, err := os.Stat(filepath.Join(unwritable, "go", "pkg", "mod")); err == nil {
		t.Skip("fake home turned out to be writable (running as root?) — nothing to assert")
	}
}
