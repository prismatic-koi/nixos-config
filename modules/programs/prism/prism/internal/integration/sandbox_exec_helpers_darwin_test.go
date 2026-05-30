//go:build darwin

package integration_test

// sandbox_exec_helpers_darwin_test.go — shared helpers for the SBPL profile
// integration tests under internal/integration/. This file centralises the
// sandbox-exec lookup, Nix-bash resolver, profile-Manager constructor,
// test-harness profile augmentation, and the withMutatedProfile helper that
// negative-case tests rely on.
//
// All test entry points live in sandbox_exec_*_darwin_test.go siblings. See
// docs/sandbox-exec-testing.md for the convention these helpers exist to
// support (issue #1192).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// sandboxExecPath is the absolute path of Apple's sandbox-exec binary. The
// integration tests invoke it directly rather than via PATH so the tests are
// pinned to the Apple-shipped version (third-party shims under PATH are
// rejected by SIP and would skew the test signal anyway).
const sandboxExecPath = "/usr/bin/sandbox-exec"

// requireSandboxExec skips the test when /usr/bin/sandbox-exec is not present
// or when the test is running inside the Nix build sandbox (where sandbox-exec
// is itself restricted and cannot apply SBPL profiles). It also probes for
// the nested-sandbox-exec case (running under an outer prism sandbox-exec
// session): kqueue / sandbox_apply refuses to nest, producing the same
// "sandbox_apply: Operation not permitted" symptom.
func requireSandboxExec(t *testing.T) {
	t.Helper()

	// The Nix build sandbox sets NIX_BUILD_TOP to a path under
	// /nix/var/nix/builds/ and sets HOME to /homeless-shelter. Running
	// sandbox-exec inside the Nix sandbox produces "sandbox_apply: Operation
	// not permitted" because the Nix sandboxed builder cannot nest a second
	// sandbox-exec invocation.
	if nixBuildTop := os.Getenv("NIX_BUILD_TOP"); nixBuildTop != "" {
		t.Skipf("skipping sandbox-exec integration test inside Nix build sandbox (NIX_BUILD_TOP=%s)", nixBuildTop)
	}

	if _, err := os.Stat(sandboxExecPath); err != nil {
		t.Skipf("sandbox-exec not found at %s: %v", sandboxExecPath, err)
	}

	// Detect the nested-sandbox case (test is running inside an outer
	// prism sandbox-exec session). Probe with a permissive profile and
	// /bin/echo — if sandbox_apply itself fails, every test in this
	// file would fail for environmental reasons unrelated to the rule
	// under test. Skip rather than emit a misleading red.
	probeProfile := "(version 1)\n(allow default)\n"
	probePath := filepath.Join(t.TempDir(), "probe.sb")
	if err := os.WriteFile(probePath, []byte(probeProfile), 0o600); err != nil {
		t.Skipf("cannot write sandbox-exec probe profile: %v", err)
	}
	probeCmd := exec.Command(sandboxExecPath, "-f", probePath, "/bin/echo", "probe-ok")
	if out, err := probeCmd.CombinedOutput(); err != nil && strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
		t.Skipf("skipping sandbox-exec integration test — nested sandbox-exec is blocked in this environment (likely running inside an outer prism sandbox-exec session): %s", string(out))
	}
}

// requireNixBash resolves the Nix-built bash binary via PATH → symlink chain
// and returns its /nix/store/... absolute path. Skips the test when bash is
// not found or does not resolve to a Nix store path.
//
// We use a Nix-built binary rather than an Apple-signed system binary
// (/bin/bash) because Apple-signed binaries SIGABRT in dyld4::CacheFinder
// under a deny-default SBPL profile — a separate issue tracked in #1190.
func requireNixBash(t *testing.T) string {
	t.Helper()

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not found in PATH: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(bashPath)
	if err != nil {
		t.Skipf("EvalSymlinks(%q): %v", bashPath, err)
	}

	if !strings.HasPrefix(resolved, "/nix/store/") {
		t.Skipf("bash resolves to %q which is not a /nix/store/ path — cannot use as test binary (Apple-signed binaries SIGABRT under deny-default sandbox, see #1190)", resolved)
	}

	return resolved
}

// newProfileManager creates a Manager with a minimal Config suitable for
// generating a sandbox-exec profile in integration tests. It does NOT set
// BareRoot — the BareRoot-ancestor block emits a (subpath "/") allow for
// file-test-existence/file-read-metadata which is broad enough to mask the
// effect of removing other system-paths allow rules in negative tests
// (Apple's sandbox-exec evaluates symlink-resolution-and-execve against
// the ancestor block, defeating the per-subpath rule under test).
//
// Tests that need the BareRoot ancestor allow (typically positive tests
// that read symlink targets under HOME) should use
// newProfileManagerWithBareRoot.
//
// Note: container.New() initialises the Manager with a podmanIsolator by
// default, but Manager.PrepareSandboxExec() creates its own
// sandboxExecIsolator internally, so no isolator override is needed.
func newProfileManager(t *testing.T) *container.Manager {
	t.Helper()
	// Sanitise the test name for use as an InstanceID: remove characters that
	// are invalid in filesystem paths (e.g. "/" from subtest names).
	instanceID := "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg := container.Config{
		SessionName: "integ-sandbox-exec-profile-test",
		InstanceID:  instanceID,
		Worktree:    t.TempDir(),
		// Required since #1960: writeGitconfig refuses to start a
		// session without [user] in the gitconfig. The integration
		// suite does not care about identity, so we inject a sentinel
		// matching the default used by newSandboxExecManager in the
		// container package's own tests.
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	}
	return container.New(cfg)
}

// newProfileManagerWithBareRoot is the variant of newProfileManager that
// configures a BareRoot two levels deep under HOME so the generator's
// BareRoot-ancestor block fires. The block grants
// file-test-existence/file-read-metadata up to /, which is necessary for
// path-traversal of resolved symlink targets that live under HOME.
//
// Use this for tests that read files via the staging-HOME symlink chain
// (e.g. credential reads through $HOME/.aws/credentials → host path).
//
// Do NOT use this for negative tests against system-paths allow rules:
// the (subpath "/") metadata allow is broad enough that the kernel will
// permit symlink-resolution-and-read of paths like /etc/hosts even when
// (subpath "/etc") is removed, masking the regression the test is trying
// to catch.
func newProfileManagerWithBareRoot(t *testing.T) *container.Manager {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	// Two-level dir structure under HOME: <HOME>/<wrap>/<bareRoot>. The
	// outer wrap dir is the ancestor that the generator emits a
	// (subpath ...) allow for. Without the wrap level, BareRoot lives
	// directly under HOME and the ancestor loop in generateProfile finds
	// zero ancestors, skipping the block entirely.
	wrap, err := os.MkdirTemp(home, ".prism-1192-bareroot-wrap-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for BareRoot wrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })

	bareRoot, err := os.MkdirTemp(wrap, "bareroot-*")
	if err != nil {
		t.Fatalf("MkdirTemp(wrap) for BareRoot: %v", err)
	}

	instanceID := "integ-sbx-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg := container.Config{
		SessionName: "integ-sandbox-exec-profile-test",
		InstanceID:  instanceID,
		Worktree:    t.TempDir(),
		BareRoot:    bareRoot,
		// Required since #1960 — see newProfileManager.
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	}
	return container.New(cfg)
}

// newProfileManagerWithBareRootAndPi is the variant used by pi-specific
// integration tests that need the SBPL profile to include the pi-gated
// (subpath ~/.pi/agent) RW allow. Identical to newProfileManagerWithBareRoot
// but sets Harness="pi" on the Config.
func newProfileManagerWithBareRootAndPi(t *testing.T) *container.Manager {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	wrap, err := os.MkdirTemp(home, ".prism-2034-bareroot-pi-wrap-*")
	if err != nil {
		t.Fatalf("MkdirTemp(home) for BareRoot wrap: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrap) })

	bareRoot, err := os.MkdirTemp(wrap, "bareroot-*")
	if err != nil {
		t.Fatalf("MkdirTemp(wrap) for BareRoot: %v", err)
	}

	instanceID := "integ-sbx-pi-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg := container.Config{
		SessionName:  "integ-sandbox-exec-pi-profile-test",
		InstanceID:   instanceID,
		Worktree:     t.TempDir(),
		BareRoot:     bareRoot,
		Harness:      "pi",
		GitUserName:  "test-user",
		GitUserEmail: "test@example.com",
	}
	return container.New(cfg)
}

// augmentProfileForTest takes the SBPL profile content generated by
// Manager.PrepareSandboxExec() and appends the minimum extras needed for a
// Nix-built binary to start under the sandbox during testing:
//
//   - (literal "/") — lets dyld read the root directory inode during startup.
//     Without this, any binary SIGABRTs before executing user code when the
//     profile uses specific subpath rules rather than (subpath "/").
//   - (subpath "/dev") — /dev/null and /dev/tty are accessed by bash
//     shell-init.
//   - (subpath "/private/tmp") with file-write* — bash needs a writable temp
//     dir.
//
// These additions are testing infrastructure only; they are NOT added to the
// production profile. They do not affect whether the rule under test is
// active — that is determined solely by the rule (or its absence) in the
// generated profile.
func augmentProfileForTest(profile string) string {
	extras := "\n" +
		";; --- test-harness extras (not in production profile) ---\n" +
		"(allow file-read* (literal \"/\") (subpath \"/dev\"))\n" +
		"(allow file-read* file-write* (subpath \"/private/tmp\"))\n"
	return profile + extras
}

// preparedProfile is the result of Manager.PrepareSandboxExec(): the
// generated profile path plus the original (un-augmented) content. Tests use
// this struct as the input to writeAugmentedProfile / withMutatedProfile.
type preparedProfile struct {
	// path is the absolute path to the SBPL profile that PrepareSandboxExec
	// wrote on disk. The profile is the production profile — it does NOT
	// include the test-harness extras.
	path string

	// content is the original profile content (read from disk after
	// PrepareSandboxExec returned). Helpers operate on this string and
	// write a new file rather than mutating path in place, so the original
	// profile is preserved for debugging if a test fails.
	content string
}

// preparePositiveProfile invokes Manager.PrepareSandboxExec() and reads the
// generated profile from disk. It registers cleanups for the staging HOME and
// the generated profile file. Returns the prepared profile (path + content)
// and the args slice — callers that need the args (e.g. to inspect the
// argument shape) can use args; tests that only need the profile content can
// ignore it.
func preparePositiveProfile(t *testing.T, m *container.Manager) (preparedProfile, []string) {
	t.Helper()

	args, err := m.PrepareSandboxExec()
	t.Cleanup(func() {
		if stagingHome, homeErr := m.SandboxExecHomePath(); homeErr == nil {
			_ = os.RemoveAll(stagingHome)
		}
		if len(args) >= 3 {
			_ = os.Remove(args[2])
		}
	})
	if err != nil {
		t.Fatalf("PrepareSandboxExec: %v", err)
	}

	if len(args) < 3 || args[0] != "sandbox-exec" || args[1] != "-f" {
		t.Fatalf("PrepareSandboxExec returned unexpected args shape: %v", args)
	}

	content, err := os.ReadFile(args[2])
	if err != nil {
		t.Fatalf("read generated profile: %v", err)
	}

	return preparedProfile{path: args[2], content: string(content)}, args
}

// writeAugmentedPositiveProfile writes the production profile content with
// the test-harness extras appended to a sibling of the generated profile
// path, registers a cleanup for the new file, and returns the new path.
// Used by positive integration tests that need to run a Nix-built binary
// under the production profile (the extras let the binary start; they do
// not affect whether the rule under test is active).
func writeAugmentedPositiveProfile(t *testing.T, p preparedProfile) string {
	t.Helper()
	augmented := augmentProfileForTest(p.content)
	testProfilePath := p.path + ".integ-test"
	if err := os.WriteFile(testProfilePath, []byte(augmented), 0o600); err != nil {
		t.Fatalf("write test profile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(testProfilePath) })
	return testProfilePath
}

// withMutatedProfile generates the SBPL profile via
// Manager.PrepareSandboxExec(), applies mutate to the profile content (e.g.
// remove a specific allow rule), augments it with the test-harness extras
// (so a Nix-built binary can still start under the sandbox), writes the
// mutated profile to a temp file alongside the generated profile, and
// returns the absolute path to the mutated profile.
//
// The helper registers cleanups so callers do not have to track temp files
// or staging HOMEs manually.
//
// This is the canonical entry point for negative-case integration tests:
// a negative test mutates the profile to remove the rule under test, then
// runs the same operation as the corresponding positive test, and asserts
// failure — proving the positive test is not green by accident.
//
// mutate must be deterministic and must not modify shared state. If mutate
// returns a string that does not differ from the input (i.e. the
// substitution targeted a rule that is not present), withMutatedProfile
// fails the test with t.Fatalf — silent no-op mutations are a common source
// of bogus passes.
func withMutatedProfile(t *testing.T, m *container.Manager, mutate func(string) string) string {
	t.Helper()

	prepared, _ := preparePositiveProfile(t, m)

	mutated := mutate(prepared.content)
	if mutated == prepared.content {
		t.Fatalf("withMutatedProfile: mutate returned identical content — the substitution did not match anything in the profile.\nProfile:\n%s",
			prepared.content)
	}

	augmented := augmentProfileForTest(mutated)
	mutatedPath := prepared.path + ".integ-mutated"
	if err := os.WriteFile(mutatedPath, []byte(augmented), 0o600); err != nil {
		t.Fatalf("write mutated profile: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(mutatedPath) })
	return mutatedPath
}

// shQuote returns s wrapped in single quotes, with any embedded single
// quotes escaped using the standard '\'' Bourne-shell idiom. Used when
// composing `bash -c '<script>'` invocations from absolute paths that may
// contain spaces.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
