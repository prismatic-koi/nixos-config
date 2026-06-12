//go:build darwin

package integration_test

// sandbox_exec_session_work_dir_darwin_test.go — integration coverage for
// the per-session work dir introduced by issue #2213 (Step 2 of #2132):
//
//   - Work-dir RW + config readability: the sandbox can read the generated
//     <sessionDir>/gitconfig and write files under <sessionDir> via the
//     (subpath "<sessionDir>") rule. The generated configs embed only stable
//     ~/.ssh/<keyname> paths — never staging-HOME or secrets.d/<N> paths.
//   - GIT_CONFIG_GLOBAL: a Nix-built git inside the sandbox resolves its
//     global config from <sessionDir>/gitconfig.
//   - known_hosts: the real ~/.ssh/known_hosts is readable via the explicit
//     read-only (literal ...) grant — and the profile never contains
//     (subpath "<HOME>/.ssh").
//
// Each positive test has a paired profile-mutation negative test proving the
// positive is not green by accident (docs/sandbox-exec-testing.md, #1192).
// The #2207 capability-probe gating applies via requireSandboxExec.
//
// Shared helpers:
//   - requireSandboxExec, requireNixBash, newProfileManager,
//     newProfileManagerWithBareRoot, preparePositiveProfile,
//     writeAugmentedPositiveProfile, withMutatedProfile
//     → sandbox_exec_helpers_darwin_test.go
//   - sbplQuoteForTest → sandbox_exec_staging_home_darwin_test.go

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// requireNixGit resolves the Nix-built git binary via PATH → symlink chain
// and returns its /nix/store/... absolute path. Skips the test when git is
// not found or does not resolve to a Nix store path (Apple-signed binaries
// SIGABRT under the deny-default test profile shape — see requireNixBash).
func requireNixGit(t *testing.T) string {
	t.Helper()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found in PATH: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(gitPath)
	if err != nil {
		t.Skipf("EvalSymlinks(%q): %v", gitPath, err)
	}
	if !strings.HasPrefix(resolved, "/nix/store/") {
		t.Skipf("git resolves to %q which is not a /nix/store/ path — cannot use as test binary", resolved)
	}
	return resolved
}

// sessionWorkDirFixture prepares the production profile for m and returns
// the session work dir path plus the prepared profile. It asserts the
// work-dir configs were generated and contain no forbidden path classes
// (staging-HOME paths, secrets.d/<N> paths) — the content half of the
// #2213 AC — before any sandbox is launched.
func sessionWorkDirFixture(t *testing.T, m *container.Manager) (string, preparedProfile) {
	t.Helper()

	prepared, _ := preparePositiveProfile(t, m)

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}
	// The legacy staging-HOME path (deleted in Step 5 of #2132) — generated
	// configs must never reference anything under it.
	legacyStagingHome := filepath.Join(sessionDir, "home")

	for _, name := range []string{"ssh-config", "gitconfig"} {
		content, readErr := os.ReadFile(filepath.Join(sessionDir, name))
		if readErr != nil {
			t.Fatalf("generated %s missing from work dir: %v", name, readErr)
		}
		if strings.Contains(string(content), legacyStagingHome) {
			t.Fatalf("generated %s embeds the legacy staging-HOME path %q:\n%s", name, legacyStagingHome, content)
		}
		if strings.Contains(string(content), "secrets.d") {
			t.Fatalf("generated %s embeds a resolved secrets.d path (#1410/#1573):\n%s", name, content)
		}
	}

	// The profile must carry the work-dir subpath rule.
	if want := "(subpath " + sbplQuoteForTest(sessionDir) + ")"; !strings.Contains(prepared.content, want) {
		t.Fatalf("generated profile missing %q.\nProfile:\n%s", want, prepared.content)
	}

	return sessionDir, prepared
}

// TestSandboxExecSessionWorkDir_ConfigsReadableAndWritable is the positive
// integration test for the (subpath "<sessionDir>") rule (#2213). From
// inside sandbox-exec it reads the generated gitconfig and writes a probe
// file into the work dir, asserting exit 0.
func TestSandboxExecSessionWorkDir_ConfigsReadableAndWritable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)
	sessionDir, prepared := sessionWorkDirFixture(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	gitconfigPath := filepath.Join(sessionDir, "gitconfig")
	probe := filepath.Join(sessionDir, "prism-2213-write-probe.tmp")

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "cat "+shQuote(gitconfigPath)+" && echo hi > "+shQuote(probe))
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("work-dir read+write failed under production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s", runErr, string(out), testProfilePath)
	}
	if !strings.Contains(string(out), "[user]") {
		t.Errorf("in-sandbox gitconfig read succeeded but output lacks [user]; output:\n%s", out)
	}
	if _, statErr := os.Stat(probe); statErr != nil {
		t.Errorf("write probe exited 0 but probe file is missing: %v", statErr)
	}
}

// TestSandboxExecSessionWorkDir_DeniedWithoutSubpath is the paired negative
// test. It re-targets the (subpath "<sessionDir>") entry at a non-existent
// sibling path and asserts that both the gitconfig read and the work-dir
// write fail — proving ConfigsReadableAndWritable (and the GIT_CONFIG_GLOBAL
// positive below, which reads through the same rule) are not green by
// accident.
//
// Mutation strategy: ReplaceAll on the quoted path rather than deleting the
// line — this keeps the SBPL syntactically valid regardless of where the
// entry sits in its allow block, and sandbox-exec silently ignores rules
// for non-existent paths.
func TestSandboxExecSessionWorkDir_DeniedWithoutSubpath(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)
	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			sbplQuoteForTest(sessionDir),
			sbplQuoteForTest(sessionDir+".prism-2213-disabled"))
	})

	gitconfigPath := filepath.Join(sessionDir, "gitconfig")
	probe := filepath.Join(sessionDir, "prism-2213-write-probe-denied.tmp")

	// Read and write probed separately so a partial regression (e.g. read
	// allowed via some unrelated rule) is reported precisely.
	readCmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		nixBash, "-c", "cat "+shQuote(gitconfigPath))
	if out, runErr := readCmd.CombinedOutput(); runErr == nil {
		t.Errorf("gitconfig read succeeded WITHOUT the work-dir (subpath ...) rule.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	}

	writeCmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		nixBash, "-c", "echo hi > "+shQuote(probe))
	if out, runErr := writeCmd.CombinedOutput(); runErr == nil {
		t.Errorf("work-dir write succeeded WITHOUT the work-dir (subpath ...) rule.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — work-dir access correctly denied without the subpath rule (exit: %v)", runErr)
	}
}

// TestSandboxExecSessionWorkDir_GitConfigGlobalUsable verifies the env-wiring
// half of the #2213 AC end-to-end: a Nix-built git inside the sandbox, with
// GIT_CONFIG_GLOBAL pointing at the work-dir gitconfig, resolves the
// configured identity from it. The read rides the same (subpath
// "<sessionDir>") rule negatively covered by DeniedWithoutSubpath above.
//
// git hard-fails at startup ("fatal: Unable to read current working
// directory") when the sandboxed CWD is unresolvable, so the launch must
// use cmd.Dir = sessionDir (a granted directory, mirroring production
// where the agent's CWD is the granted worktree) plus the getcwd ancestor
// extras — see sandbox_exec_launch_dir_darwin_test.go. With the old
// fixture shape (CWD inherited from the go-test binary, ungranted) this
// test could never have passed a host run — a pre-existing hole from
// #2221, surfaced by the first host run that exercised it (#2247).
func TestSandboxExecSessionWorkDir_GitConfigGlobalUsable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixGit := requireNixGit(t)

	m := newProfileManager(t)
	sessionDir, prepared := sessionWorkDirFixture(t, m)
	testProfilePath := writeAugmentedPositiveProfileWithLaunchDir(t, prepared, sessionDir)

	gitEnv := container.SessionWorkDirGitEnv(sessionDir, "")
	// GIT_CEILING_DIRECTORIES stops git's repository discovery before it
	// crosses out of the launch dir: discovery walks up from CWD statting
	// .git at each level, and the stat of <parent>/.git (sessions/.git)
	// lands on a CHILD of an ancestor node — which the launch-dir extras
	// deliberately do not grant — so it returns EPERM, which git treats as
	// fatal ("fatal: error reading .../sessions/.git", observed on the
	// round-2 host run). With the ceiling set to the parent, git checks
	// <sessionDir>/.git (granted subtree → clean ENOENT) and stops without
	// touching the parent. The leading empty entry (":") tells git the
	// entry needs no symlink resolution (git(1)), avoiding realpath
	// syscalls on the ancestor chain. Production sessions never need this:
	// the agent's CWD is a real worktree, so discovery succeeds at the
	// first level.
	ceiling := "GIT_CEILING_DIRECTORIES=:" + filepath.Dir(sessionDir)
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		"/usr/bin/env", gitEnv[0], // GIT_CONFIG_GLOBAL=<sessionDir>/gitconfig
		ceiling,
		nixGit, "config", "--global", "user.name")
	cmd.Dir = sessionDir
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("git config --global user.name failed under production profile with GIT_CONFIG_GLOBAL.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s", runErr, string(out), testProfilePath)
	}
	// newProfileManager configures GitUserName "test-user".
	if got := strings.TrimSpace(string(out)); got != "test-user" {
		t.Errorf("git config --global user.name = %q, want %q (gitconfig not resolved from the work dir?)",
			got, "test-user")
	}
}

// knownHostsFixture returns the real ~/.ssh/known_hosts path. It skips when
// the file does not exist or is itself a symlink (an SBPL literal grant on a
// symlink path is inert — policy evaluates the resolved target at open time
// — so the rule under test would not be load-bearing on such a host).
func knownHostsFixture(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	knownHosts := filepath.Join(home, ".ssh", "known_hosts")
	info, err := os.Lstat(knownHosts)
	if err != nil {
		t.Skipf("~/.ssh/known_hosts not present on this host: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Skipf("~/.ssh/known_hosts is a symlink on this host — the literal grant under test is not load-bearing")
	}
	return knownHosts
}

// TestSandboxExecKnownHosts_ReadableViaLiteral is the positive test for the
// explicit read-only (literal "~/.ssh/known_hosts") grant (#2213): ssh's
// default UserKnownHostsFile resolves against the real home (getpwuid →
// pw_dir), so the real file must be readable in-sandbox without the
// staging-HOME symlink walk.
//
// Uses the BareRoot variant so the ancestor block grants file-read-metadata
// on (subpath HOME) for path traversal — but NOT file-read-data, which is
// what keeps the paired negative test meaningful.
func TestSandboxExecKnownHosts_ReadableViaLiteral(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	knownHosts := knownHostsFixture(t)

	m := newProfileManagerWithBareRoot(t)
	prepared, _ := preparePositiveProfile(t, m)

	if want := "(literal " + sbplQuoteForTest(knownHosts) + ")"; !strings.Contains(prepared.content, want) {
		t.Fatalf("generated profile missing the known_hosts literal %q.\nProfile:\n%s", want, prepared.content)
	}
	// The profile must never grant (subpath "<HOME>/.ssh") — it would cover
	// non-sops private keys (e.g. the daily-driver key).
	if forbidden := "(subpath " + sbplQuoteForTest(filepath.Dir(knownHosts)) + ")"; strings.Contains(prepared.content, forbidden) {
		t.Fatalf("generated profile contains forbidden rule %q.\nProfile:\n%s", forbidden, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "cat "+shQuote(knownHosts)+" > /dev/null")
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("read of real ~/.ssh/known_hosts failed under production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s", runErr, string(out), testProfilePath)
	}
}

// TestSandboxExecKnownHosts_DeniedWithoutLiteral is the paired negative
// test. It re-targets every rule referencing the known_hosts path (the
// explicit #2213 literal) at a non-existent path, then asserts the read
// fails.
//
// Re-targeting (ReplaceAll on the quoted path) rather than line deletion
// keeps the SBPL valid regardless of each rule's position in its block.
func TestSandboxExecKnownHosts_DeniedWithoutLiteral(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	knownHosts := knownHostsFixture(t)

	m := newProfileManagerWithBareRoot(t)

	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p,
			sbplQuoteForTest(knownHosts),
			sbplQuoteForTest(knownHosts+".prism-2213-disabled"))
	})

	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		nixBash, "-c", "cat "+shQuote(knownHosts)+" > /dev/null")
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("read of ~/.ssh/known_hosts succeeded WITHOUT its (literal ...) allow.\n"+
			"The negative test is not catching the regression — investigate.\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — known_hosts read correctly denied without the literal allow (exit: %v)", runErr)
	}
}
