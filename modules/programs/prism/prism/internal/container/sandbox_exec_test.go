package container

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newSandboxExecManager creates a Manager and injects a sandboxExecIsolator
// so tests can drive BuildArgs/PrepareSandboxExec end-to-end without a real
// macOS host. The test fixture intentionally does not stub HOME or any of the
// bwrap fixture's fake credential paths because the minimal SBPL profile in
// this PR does not consume them — staging HOME, credentials, and caches
// land in #1017.
func newSandboxExecManager(cfg Config) *Manager {
	m := New(cfg)
	m.isolator = newSandboxExecIsolator(m.name)
	return m
}

// ── generateProfile content assertions ──────────────────────────────────────

// TestGenerateProfile_VersionAndDenyDefault verifies that the profile begins
// with the SBPL header that locks deny-by-default semantics: (version 1)
// followed by (deny default). This is non-negotiable per #1012 — every
// other clause is interpreted relative to deny-by-default.
func TestGenerateProfile_VersionAndDenyDefault(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	// (version 1) MUST be the first non-empty content.
	if !strings.HasPrefix(profile, "(version 1)\n") {
		t.Errorf("profile must begin with (version 1)\\n; got prefix %q", firstNChars(profile, 64))
	}

	// (deny default) MUST follow immediately after (version 1).
	if !strings.HasPrefix(profile, "(version 1)\n(deny default)\n") {
		t.Errorf("profile must start with (version 1) then (deny default); got prefix %q", firstNChars(profile, 64))
	}
}

// TestGenerateProfile_ReadOnlySystemRoots verifies that every read-only
// system root listed in the AC appears as a (subpath ...) inside an
// (allow file-read* ...) clause.
func TestGenerateProfile_ReadOnlySystemRoots(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	// Locate the (allow file-read* ... block (the first one — there is only
	// one in this PR, but extracting it explicitly insulates the assertion
	// from accidental clauses appearing later in the file).
	allowReadBlock := extractClause(t, profile, "(allow file-read*")

	expected := []string{
		`(subpath "/nix")`,
		`(subpath "/usr")`,
		`(subpath "/System")`,
		`(subpath "/Library")`,
		`(subpath "/private/etc")`,
		`(subpath "/private/var/db/dyld")`,
		`(subpath "/private/var/db/timezone")`,
	}
	for _, want := range expected {
		if !strings.Contains(allowReadBlock, want) {
			t.Errorf("(allow file-read* ...) block missing %q; block:\n%s", want, allowReadBlock)
		}
	}
}

// TestGenerateProfile_SensitiveSubtreeDenies verifies that the two deny
// subpaths from the AC appear inside a (deny file-read* file-write* ...)
// clause. These mirror the bwrap --tmpfs shadows of /etc/wireguard and
// /etc/wpa_supplicant.
func TestGenerateProfile_SensitiveSubtreeDenies(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	denyBlock := extractClause(t, profile, "(deny file-read* file-write*")
	expected := []string{
		`(subpath "/private/etc/wireguard")`,
		`(subpath "/private/etc/wpa_supplicant")`,
	}
	for _, want := range expected {
		if !strings.Contains(denyBlock, want) {
			t.Errorf("(deny file-read* file-write* ...) block missing %q; block:\n%s", want, denyBlock)
		}
	}
}

// TestGenerateProfile_ProcessAndIPCAllows verifies that the profile contains
// the seven process/IPC primitives required for node and opencode to run.
// These are listed verbatim in the AC; the test asserts substring presence
// rather than coupling to the exact whitespace in the generator output.
func TestGenerateProfile_ProcessAndIPCAllows(t *testing.T) {
	m := newSandboxExecManager(Config{SessionName: "repo@main"})
	profile := generateProfile(m)

	wantSubstrings := []string{
		"(allow process-exec*",
		"process-fork",
		"signal",
		"mach-lookup",
		"mach-register",
		"sysctl-read",
		"iokit-open",
		"ipc-posix-shm",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing required substring %q; full profile:\n%s", want, profile)
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

// TestGenerateProfile_NoOutOfScopeRules guards against accidentally pulling
// PR 3 (#1017) or PR 4 (#1018) content into this PR. The minimal read-only
// profile in this PR must NOT contain staging-HOME, worktree, credential, or
// cache rules.
func TestGenerateProfile_NoOutOfScopeRules(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName: "repo@main",
		// Set fields that staging-HOME (#1017) will eventually consume.
		// generateProfile must NOT emit any clauses derived from them in
		// this PR.
		Worktree: "/tmp/fake-worktree",
		BareRoot: "/tmp/fake-bare",
	})
	profile := generateProfile(m)

	for _, forbidden := range []string{
		"<HOME>",
		"<STAGING_HOME>",
		"<WORKTREE>",
		"<BARE_REPO>",
		"<RESOLVED_SSH_ACCESS_KEY>",
		"file-write*",
		// file-write* should appear ONLY inside the deny clause; the test
		// for that clause above already gates it. A bare file-write* allow
		// is out of scope for this PR.
	} {
		// "file-write*" is a special case: it appears in the deny clause
		// "(deny file-read* file-write* ..." which is in scope. So we
		// instead check that there is NO "(allow file-write*" anywhere.
		if forbidden == "file-write*" {
			if strings.Contains(profile, "(allow file-write*") {
				t.Errorf("profile contains (allow file-write* ...) which is out of scope for #1016; defer to #1017")
			}
			continue
		}
		if strings.Contains(profile, forbidden) {
			t.Errorf("profile contains out-of-scope token %q; defer to #1017/#1018", forbidden)
		}
	}

	// Worktree path itself must not appear — staging HOME (#1017) is what
	// will introduce host paths into the profile.
	if strings.Contains(profile, "/tmp/fake-worktree") || strings.Contains(profile, "/tmp/fake-bare") {
		t.Errorf("profile contains a host path from cfg; this is staging-HOME territory and belongs to #1017; profile:\n%s", profile)
	}
}

// ── PrepareSandboxExec ──────────────────────────────────────────────────────

// TestPrepareSandboxExec_WritesProfileAndReturnsArgs verifies that
// PrepareSandboxExec materialises the profile to a temp file under the
// per-session state dir and returns args of the shape
// ["sandbox-exec", "-f", <profile_path>, <harness>, ...].
func TestPrepareSandboxExec_WritesProfileAndReturnsArgs(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@feat",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	t.Cleanup(func() {
		_ = os.Remove(m.sandboxExecProfilePath())
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
	// string so future PRs can swap "opencode" for an absolute path without
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

// TestSandboxExecBuildArgs_OpencodePortFlag verifies that BuildArgs emits
// --port <AllocatedPort> --hostname 127.0.0.1 in the harness arguments,
// mirroring the bwrap path's invocation contract.
func TestSandboxExecBuildArgs_OpencodePortFlag(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14111,
	})
	s := &sandboxExecIsolator{name: m.name}
	args := s.BuildArgs(m)

	if !sliceContainsPair(args, "--port", "14111") {
		t.Errorf("expected --port 14111 in args: %v", args)
	}
	if !sliceContainsPair(args, "--hostname", "127.0.0.1") {
		t.Errorf("expected --hostname 127.0.0.1 in args: %v", args)
	}
}

// TestSandboxExecBuildArgs_PortFallback verifies that when AllocatedPort is
// zero, BuildArgs falls back to ContainerPort. This mirrors bwrap.BuildArgs
// and protects against malformed sessions.
func TestSandboxExecBuildArgs_PortFallback(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 0,
	})
	s := &sandboxExecIsolator{name: m.name}
	args := s.BuildArgs(m)

	wantPort := containerPortString()
	if !sliceContainsPair(args, "--port", wantPort) {
		t.Errorf("expected fallback --port %s in args: %v", wantPort, args)
	}
}

// TestSandboxExecBuildArgs_AgentRolePassedThrough verifies that AgentRole
// is appended as --agent <role> when non-empty, and omitted when empty —
// matching the bwrap path.
func TestSandboxExecBuildArgs_AgentRolePassedThrough(t *testing.T) {
	for _, tc := range []struct {
		role string
		want bool
	}{
		{"worker", true},
		{"coordinator", true},
		{"review-code", true},
		{"", false},
	} {
		t.Run("AgentRole="+tc.role, func(t *testing.T) {
			m := newSandboxExecManager(Config{
				SessionName:   "repo@main",
				AllocatedPort: 14010,
				AgentRole:     tc.role,
			})
			s := &sandboxExecIsolator{name: m.name}
			args := s.BuildArgs(m)

			has := sliceContainsPair(args, "--agent", tc.role)
			if tc.want && !has {
				t.Errorf("expected --agent %q in args (role non-empty): %v", tc.role, args)
			}
			if !tc.want && hasFlag(args, "--agent") {
				t.Errorf("--agent must be absent when role is empty; got: %v", args)
			}
		})
	}
}

// TestSandboxExecBuildArgs_InitialPromptPassedThrough verifies that
// InitialPrompt becomes --prompt <text> when non-empty, and is omitted when
// empty — matching the bwrap path.
func TestSandboxExecBuildArgs_InitialPromptPassedThrough(t *testing.T) {
	m := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14010,
		InitialPrompt: "do the thing",
	})
	s := &sandboxExecIsolator{name: m.name}
	args := s.BuildArgs(m)
	if !sliceContainsPair(args, "--prompt", "do the thing") {
		t.Errorf("expected --prompt 'do the thing' in args: %v", args)
	}

	mEmpty := newSandboxExecManager(Config{
		SessionName:   "repo@main",
		AllocatedPort: 14010,
	})
	sEmpty := &sandboxExecIsolator{name: mEmpty.name}
	argsEmpty := sEmpty.BuildArgs(mEmpty)
	if hasFlag(argsEmpty, "--prompt") {
		t.Errorf("--prompt must be absent when InitialPrompt is empty; got: %v", argsEmpty)
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
	if args[3] != "opencode" {
		t.Errorf("expected args[3] to be the harness binary 'opencode'; got %q in %v", args[3], args)
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
