//go:build darwin

package integration_test

// sandbox_exec_chromium_workdir_darwin_test.go — integration coverage for
// the chromium Library skeleton in the per-session work dir (issue #2247,
// Step 4 of the #2132 staging-HOME elimination train):
//
//   - Work-dir Library writable: an in-sandbox write under
//     <sessionDir>/Library/Application Support/Google/ succeeds under the
//     production profile. The skeleton deliberately has NO dedicated SBPL
//     rule — the write rides the existing (subpath "<sessionDir>") RW allow
//     (§6), which is what CFFIXED_USER_HOME=<sessionDir> points chromium's
//     NSHomeDirectory()-derived writes at.
//   - Paired strip negative: re-targeting the (subpath "<sessionDir>")
//     entry makes the same write fail — proving the positive rides that
//     grant and is not green by accident. Re-targeting the quoted path
//     (rather than deleting the whole §6 block) follows the established
//     #2213 sibling convention for this rule
//     (TestSandboxExecSessionWorkDir_DeniedWithoutSubpath): the sessionDir
//     entry shares its allow block with the worktree/bare-root/host-API
//     rules, and the skeleton has no block of its own to strip — the
//     precise claim under test is "the Library write rides THIS entry".
//   - Host-Library denied: an in-sandbox write to the REAL
//     ~/Library/Application Support/Google is denied under the production
//     profile — the negative that pins option B's security property (no
//     host-Library grant exists; the daily-driver Chrome profile stays
//     unreachable). Paired with the no-host-Library profile assertion at
//     unit level (TestGenerateProfile_NoHostLibraryRulesForChromium).
//
// The #2207 capability-probe gating applies via requireSandboxExec; see
// docs/sandbox-exec-testing.md (#1192) for the helper conventions.
//
// Shared helpers:
//   - requireSandboxExec, requireNixBash, newProfileManager,
//     newProfileManagerWithBareRoot, preparePositiveProfile,
//     writeAugmentedPositiveProfile, withMutatedProfile, shQuote
//     → sandbox_exec_helpers_darwin_test.go
//   - sbplQuoteForTest → sandbox_exec_staging_home_darwin_test.go

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// chromiumWorkDirFixture prepares the production profile for m and returns
// the session work dir plus the prepared profile. It asserts the #2247
// layout before any sandbox is launched: the two Google skeleton dirs
// exist inside the work dir, and the profile contains the
// (subpath "<sessionDir>") rule the skeleton rides but no rule referencing
// the host ~/Library.
func chromiumWorkDirFixture(t *testing.T, m *container.Manager) (string, preparedProfile) {
	t.Helper()

	prepared, _ := preparePositiveProfile(t, m)

	sessionDir, err := m.SessionWorkDir()
	if err != nil {
		t.Fatalf("SessionWorkDir: %v", err)
	}

	for _, d := range container.SessionWorkDirChromiumDirs(sessionDir) {
		info, statErr := os.Lstat(d)
		if statErr != nil {
			t.Fatalf("chromium skeleton dir %q missing after PrepareSandboxExec: %v", d, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			t.Fatalf("chromium skeleton dir %q must be a real directory: mode=%v", d, info.Mode())
		}
	}

	// The capability under test: the existing work-dir subpath rule.
	if want := "(subpath " + sbplQuoteForTest(sessionDir) + ")"; !strings.Contains(prepared.content, want) {
		t.Fatalf("generated profile missing %q.\nProfile:\n%s", want, prepared.content)
	}
	// No host-Library grant may exist (option B's security property).
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if hostLibrary := filepath.Join(home, "Library"); strings.Contains(prepared.content, hostLibrary) {
			t.Fatalf("generated profile references the host home Library %q — #2247 must add no host-Library grants.\nProfile:\n%s",
				hostLibrary, prepared.content)
		}
	}

	return sessionDir, prepared
}

// TestSandboxExecChromiumWorkDir_LibraryWritable is the positive
// integration test for the #2247 AC: an in-sandbox write under
// <sessionDir>/Library/... succeeds under the production profile. It
// mimics chromium's first write shape (mkdir "Chrome for Testing" under the
// Application Support skeleton, then create a file inside it) and asserts
// the bytes are visible on the host afterwards.
func TestSandboxExecChromiumWorkDir_LibraryWritable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)
	sessionDir, prepared := chromiumWorkDirFixture(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	probeDir := filepath.Join(sessionDir, "Library", "Application Support", "Google", "Chrome for Testing")
	probe := filepath.Join(probeDir, "prism-2247-write-probe.tmp")

	// Leaf-only mkdir (NOT mkdir -p): the parent skeleton dir is
	// host-prepped by PrepareSessionWorkDir (asserted by the fixture), so a
	// single mkdir(2) syscall against the granted subtree suffices. A deep
	// absolute `mkdir -p` issues a mkdir(2) per path component — and under
	// deny-default each EXISTING but ungranted ancestor (/Users, ...)
	// returns EPERM rather than EEXIST, which mkdir -p treats as fatal
	// (observed on the first host run of this test). Launch CWD is the
	// session work dir so the in-sandbox process starts in a granted
	// directory, mirroring production (agent CWD = granted worktree).
	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "mkdir "+shQuote(probeDir)+" && echo prism-2247 > "+shQuote(probe))
	cmd.Dir = sessionDir
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("work-dir Library write failed under production profile.\n"+
			"Exit: %v\nOutput: %s\nProfile: %s", runErr, string(out), testProfilePath)
	}
	got, readErr := os.ReadFile(probe)
	if readErr != nil {
		t.Errorf("write probe exited 0 but probe file is missing on the host: %v", readErr)
	} else if strings.TrimSpace(string(got)) != "prism-2247" {
		t.Errorf("probe file content = %q, want %q", strings.TrimSpace(string(got)), "prism-2247")
	}
}

// TestSandboxExecChromiumWorkDir_DeniedWithoutSessionDirSubpath is the
// paired strip negative: with the (subpath "<sessionDir>") entry re-targeted
// at a non-existent sibling, the same Library write fails — proving the
// positive rides the work-dir grant specifically and that NO other rule
// (and no new rule from this step) covers the chromium skeleton.
//
// Mutation strategy: ReplaceAll on the quoted path rather than deleting the
// line — this keeps the SBPL syntactically valid regardless of where the
// entry sits in its allow block, and sandbox-exec silently ignores rules
// for non-existent paths (the established #2213 convention for this rule).
func TestSandboxExecChromiumWorkDir_DeniedWithoutSessionDirSubpath(t *testing.T) {
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
			sbplQuoteForTest(sessionDir+".prism-2247-disabled"))
	})

	probeDir := filepath.Join(sessionDir, "Library", "Application Support", "Google", "Chrome for Testing")
	probe := filepath.Join(probeDir, "prism-2247-write-probe-denied.tmp")

	// Same command shape as the positive (leaf-only mkdir, CWD =
	// sessionDir). Under the mutated profile the launch dir's own grant is
	// gone, so bash starts with an unresolvable CWD — bash tolerates that
	// (warns and continues; node/git would not, which is why this negative
	// must stay bash-based) and the assertion lands on the leaf operations
	// being denied.
	cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
		nixBash, "-c", "mkdir "+shQuote(probeDir)+" && echo prism-2247 > "+shQuote(probe))
	cmd.Dir = sessionDir
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("work-dir Library write succeeded WITHOUT the (subpath \"<sessionDir>\") rule.\n"+
			"The chromium skeleton must have no other write capability (issue #2247).\n"+
			"Output: %s\nMutated profile: %s", string(out), mutatedPath)
	} else {
		t.Logf("ka pai — work-dir Library write correctly denied without the sessionDir subpath rule (exit: %v)", runErr)
	}
}

// TestSandboxExecChromiumHostLibrary_WriteDenied pins option B's security
// property (issue #2247, design #2132 §4 Step 4): an in-sandbox write to
// the REAL ~/Library/Application Support/Google — the daily-driver Chrome
// profile's parent — is denied under the production profile. No
// host-Library grant exists; chromium state is confined to the per-session
// work dir.
//
// Uses the BareRoot manager variant deliberately: its ancestor block grants
// file-read-metadata/file-test-existence up to / (the most permissive
// realistic profile shape), so a pass here proves the write denial does not
// depend on traversal also being blocked.
func TestSandboxExecChromiumHostLibrary_WriteDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	hostGoogleDir := filepath.Join(home, "Library", "Application Support", "Google")

	// The deny must be exercised against an EXISTING directory — if the
	// parent is absent the write fails with ENOENT regardless of sandbox
	// policy and the test proves nothing. Create it when absent (e.g. a
	// host with no Chrome installed) and remove ONLY what we created.
	if _, statErr := os.Stat(hostGoogleDir); statErr != nil {
		if mkErr := os.MkdirAll(hostGoogleDir, 0o700); mkErr != nil {
			t.Skipf("cannot create %s for the deny probe: %v", hostGoogleDir, mkErr)
		}
		t.Cleanup(func() { _ = os.Remove(hostGoogleDir) }) // rmdir — only removes if still empty
	}

	m := newProfileManagerWithBareRoot(t)
	sessionDir, prepared := chromiumWorkDirFixture(t, m)
	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Uniquely-named probe so a failure cannot collide with real Chrome
	// state; if the write unexpectedly succeeds we remove it.
	probe := filepath.Join(hostGoogleDir, fmt.Sprintf(".prism-2247-deny-probe-%d", time.Now().UnixNano()))

	cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
		nixBash, "-c", "echo leak > "+shQuote(probe))
	cmd.Dir = sessionDir
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		_ = os.Remove(probe)
		t.Fatalf("in-sandbox write to the REAL ~/Library/Application Support/Google SUCCEEDED under the production profile.\n"+
			"A host-Library grant exists — this violates option B's security property (issue #2247).\n"+
			"Output: %s\nProfile: %s", string(out), testProfilePath)
	}
	if _, statErr := os.Stat(probe); statErr == nil {
		_ = os.Remove(probe)
		t.Fatalf("probe file exists in the real host Library despite non-zero exit — investigate: %s", probe)
	}
	t.Logf("ka pai — host-Library write correctly denied under the production profile (exit: %v)", runErr)
}
