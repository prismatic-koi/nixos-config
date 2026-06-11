//go:build darwin

package integration_test

// sandbox_exec_sops_rotation_darwin_test.go — integration coverage for the
// sops-rotation fix applied to the remaining staging-HOME symlinks: the
// .ssh/access-key entry (issue #1573, follow-up to #1410). See the
// Mechanism note below for why this PR's rotation suite only covers .ssh.
//
// After darwin-rebuild switch, sops rotates secrets.d/<N>/ → secrets.d/<N+1>/
// and removes the old directory. The staging-HOME symlinks use
// symlinkIfExists (not symlinkIfResolvable), so they point at the stable sops
// intermediate rather than the concrete secrets.d/<N>/ path. This means reads
// through the staging HOME symlinks continue to work after a rotation.
//
// Note (#2234, Step 3a of #2132): the .aws/config and .aws/credentials
// staging symlinks no longer exist — the aws CLI reads those files via
// AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE at the host XDG paths, and
// rotation safety for those reads rides the counter-independent #2211
// secrets.d allowlist (see TestSandboxExecSecretsDeny_RotationSimulation and
// the aws env-var tests in sandbox_exec_aws_config_envvar_darwin_test.go).
//
// Mechanism note — why this only covers .ssh/access-key in this PR:
// PrepareSandboxExec re-runs PrepareSandboxExecHome internally (lifecycle_
// dispatch.go:233), which idempotently recreates the default staging
// symlinks via symlinkIfExists. When the canonical source path on the host
// exists, that recreation clobbers any test replacement at the staging
// destination BEFORE generateProfile walks the tree, so the collector sees
// the default chain instead of the test-fabricated one. The .aws/credentials
// vehicle worked on previous hosts only because credentials is absent
// (symlinkIfExists short-circuits when SOURCE is missing). For .ssh the
// test now sets SshAccessKeyName to a unique non-existent name so the same
// short-circuit fires and the test's replacement survives. .kube/config has
// no equivalent Config knob, and its staging symlink is itself on the
// chopping block in #2235 (Step 3b), so this PR omits the kube entry from
// the rotation suite. #2235's footprint covers the kube rotation guarantee
// either via its own test or by removing the entry entirely.
//
// Test design
// ───────────
// Each test entry exercises the two-hop chain:
//
//   <stagingHome>/<entry>
//     → <stableIntermediate>                         (stable sops symlink)
//     → <concreteFile>                               (simulates secrets.d/<N>/...)
//
// The stableIntermediate is placed under HOME (via a temp dir created by
// hostCredentialDir), and the concreteFile is placed under HOME as well.
// Using HOME (rather than /private/var/folders) ensures the per-symlink
// (literal <concreteFile>) allow rule emitted by collectStagingHomeSymlinkTargets
// is load-bearing for reads — NOT the broad (subpath "/private/var/folders")
// rule. This makes the negative test meaningful.
//
// The positive test generates the production profile, installs the two-hop
// chain into the staging HOME, then reads the concreteFile via the staging
// HOME path inside sandbox-exec. The negative test mutates the profile to
// remove the (literal <concreteFile>) allow and asserts the read fails —
// proving the positive is not a no-op.
//
// Shared helpers:
//   - requireSandboxExec, requireNixBash, newProfileManagerWithBareRoot,
//     preparePositiveProfile, writeAugmentedPositiveProfile, withMutatedProfile
//     → sandbox_exec_helpers_darwin_test.go
//   - hostCredentialDir, sbplQuoteForTest
//     → sandbox_exec_staging_home_darwin_test.go
//
// See docs/sandbox-exec-testing.md for the convention (#1192).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
)

// sopsRotationEntry describes one staging-HOME entry to test in the rotation
// integration tests.
type sopsRotationEntry struct {
	// stagingRelPath is the path of the symlink relative to stagingHome
	// (e.g. ".ssh/access-key").
	stagingRelPath string
	// intermediateRelPath is the path of the stable intermediate relative to
	// HOME (e.g. ".ssh/prismatic-koi-ed25519").
	intermediateRelPath string
	// sentinel is written into the concrete v2 file so positive tests can
	// assert the right content was read.
	sentinel string
}

// sopsRotationEntries lists the staging-HOME entries still exercised by the
// rotation test (#1573 minus the .aws pair removed in #2234, minus
// .kube/config — see the mechanism note in the file header).
var sopsRotationEntries = []sopsRotationEntry{
	{
		stagingRelPath:      ".ssh/access-key",
		intermediateRelPath: ".ssh/prismatic-koi-ed25519",
		sentinel:            "ssh-ed25519 AAAA1573-access-key-sentinel",
	},
}

// rotationTestAccessKeyName is the unique-per-test-run SshAccessKeyName
// injected into the Manager Config below. Using a unique name whose
// corresponding source path (~/.ssh/<name>) does not exist on the host makes
// PrepareSandboxExecHome's symlinkIfExists short-circuit for .ssh/access-key,
// so the second PrepareSandboxExecHome call inside iso.Prepare does NOT
// recreate (and clobber) the test's replacement staging symlink.
const rotationTestAccessKeyName = "prism-rotation-test-access-key-do-not-create"

// newProfileManagerForRotationTest is the rotation-suite variant of
// newProfileManagerWithBareRoot: same BareRoot wiring (so the ancestor block
// fires and lets the kernel traverse intermediate symlinks under HOME), with
// SshAccessKeyName forced to a unique non-existent name (see
// rotationTestAccessKeyName above for the mechanism).
func newProfileManagerForRotationTest(t *testing.T) *container.Manager {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	guardPath := filepath.Join(home, ".ssh", rotationTestAccessKeyName)
	if _, statErr := os.Lstat(guardPath); statErr == nil {
		t.Skipf("rotation test guard violated: %s exists; remove it or pick a different rotationTestAccessKeyName", guardPath)
	}
	wrap, err := os.MkdirTemp(home, ".prism-1573-rotation-bareroot-wrap-*")
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
		SessionName:      "integ-sandbox-exec-profile-test",
		InstanceID:       instanceID,
		Worktree:         t.TempDir(),
		BareRoot:         bareRoot,
		SshAccessKeyName: rotationTestAccessKeyName,
		GitUserName:      "test-user",
		GitUserEmail:     "test@example.com",
	}
	return container.New(cfg)
}

// setupSopsRotationChain sets up the two-hop symlink chain for a single entry:
//
//	<stableIntermediateDir>/<intermediateFileName>
//	  → <concreteFile>            (contains sentinel content)
//
// Both the intermediate dir and the concrete file are placed under HOME (via
// hostCredentialDir), ensuring the per-symlink (literal ...) allow rule is
// load-bearing for reads. The function returns (intermediateSymlink, concreteFile).
//
// The caller is responsible for installing the staging HOME symlink pointing
// at intermediateSymlink (simulating the symlinkIfExists behavior).
func setupSopsRotationChain(t *testing.T, entry sopsRotationEntry) (intermediateSymlink, concreteFile string) {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}

	// Concrete file under HOME (not /private/var/folders) so the per-symlink
	// literal rule is load-bearing for the negative test.
	concreteDir := hostCredentialDir(t)
	concreteFile = filepath.Join(concreteDir, filepath.Base(entry.stagingRelPath))
	if err := os.WriteFile(concreteFile, []byte(entry.sentinel), 0o600); err != nil {
		t.Fatalf("write concrete file for %s: %v", entry.stagingRelPath, err)
	}

	// Stable intermediate dir under HOME — simulates ~/.ssh/, ~/.config/aws/, etc.
	// Must be under HOME so the BareRoot ancestor block (subpath HOME) provides
	// file-read-metadata, letting the kernel follow the symlink stored here.
	intermediateDir, dirErr := os.MkdirTemp(home, ".prism-1573-intermediate-*")
	if dirErr != nil {
		t.Fatalf("MkdirTemp for intermediateDir (%s): %v", entry.stagingRelPath, dirErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(intermediateDir) })

	intermediateFileName := filepath.Base(entry.intermediateRelPath)
	intermediateSymlink = filepath.Join(intermediateDir, intermediateFileName)
	if err := os.Symlink(concreteFile, intermediateSymlink); err != nil {
		t.Fatalf("create intermediate symlink for %s: %v", entry.stagingRelPath, err)
	}

	return intermediateSymlink, concreteFile
}

// TestSandboxExecSopsRotation_TwoHopChainReadable is the positive integration
// test for the sops-rotation fix (issue #1573).
//
// For each of the rotation-safe staging entries, it sets up the
// two-hop chain:
//
//	<stagingHome>/<entry>
//	  → <stableIntermediate>   (simulates ~/.ssh/<key>, ~/.config/aws/<file>, etc.)
//	  → <concreteFile>         (simulates secrets.d/<N>/...)
//
// Generates the production SBPL profile, then reads each file via the staging
// HOME path inside sandbox-exec. Asserts exit 0 and that the sentinel content
// is visible — confirming the sandbox can follow the full two-hop chain.
//
// Uses newProfileManagerForRotationTest so the BareRoot ancestor block fires and
// grants file-read-metadata on (subpath HOME), enabling the kernel to traverse
// intermediate symlinks under HOME.
func TestSandboxExecSopsRotation_TwoHopChainReadable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManagerForRotationTest(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Install the two-hop chains in the staging HOME.
	type installedEntry struct {
		entry        sopsRotationEntry
		stagingLink  string
		concreteFile string
	}
	var installed []installedEntry
	for _, entry := range sopsRotationEntries {
		intermediateSymlink, concreteFile := setupSopsRotationChain(t, entry)

		stagingLink := filepath.Join(stagingHome, entry.stagingRelPath)
		_ = os.Remove(stagingLink) // replace whatever PrepareSandboxExecHome placed
		if linkErr := os.Symlink(intermediateSymlink, stagingLink); linkErr != nil {
			t.Fatalf("create staging symlink for %s: %v", entry.stagingRelPath, linkErr)
		}
		installed = append(installed, installedEntry{
			entry:        entry,
			stagingLink:  stagingLink,
			concreteFile: concreteFile,
		})
	}

	prepared, _ := preparePositiveProfile(t, m)

	// Verify the profile references the resolved concrete paths (emitted by
	// collectStagingHomeSymlinkTargets via filepath.EvalSymlinks).
	for _, ins := range installed {
		resolved, _ := filepath.EvalSymlinks(ins.concreteFile)
		if resolved == "" {
			resolved = ins.concreteFile
		}
		if !strings.Contains(prepared.content, resolved) {
			t.Fatalf("generated profile does not reference resolved concrete path %q for %s.\n"+
				"collectStagingHomeSymlinkTargets did not pick up the staging symlink chain.\nProfile:\n%s",
				resolved, ins.entry.stagingRelPath, prepared.content)
		}
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	// Read each file from inside the sandbox via the staging HOME path.
	// The kernel must traverse: stagingLink → intermediate → concreteFile.
	for _, ins := range installed {
		cmd := exec.Command(sandboxExecPath, "-f", testProfilePath,
			"/usr/bin/env", fmt.Sprintf("HOME=%s", stagingHome), nixBash, "-c",
			"cat "+shQuote(ins.stagingLink))
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Errorf("read %s via two-hop staging-HOME chain failed under production profile.\n"+
				"This means the sandbox cannot follow the sops-style symlink chain required by #1573.\n"+
				"Exit: %v\nOutput: %s\nProfile: %s",
				ins.entry.stagingRelPath, runErr, string(out), testProfilePath)
			continue
		}
		if !strings.Contains(string(out), ins.entry.sentinel[:10]) {
			t.Errorf("%s read succeeded but output does not contain the sentinel.\nOutput: %s",
				ins.entry.stagingRelPath, string(out))
		}
	}
}

// TestSandboxExecSopsRotation_TwoHopChainDeniedWithoutConcreteAllow is the
// paired negative test. For each of the entries, it removes the
// (literal <concreteFile>) allow line from the profile and asserts that
// reading the file via the staging HOME symlink chain fails.
//
// This proves TestSandboxExecSopsRotation_TwoHopChainReadable is not green by
// accident: the read works specifically because of the (literal <concreteFile>)
// allow emitted by collectStagingHomeSymlinkTargets, not because of a broader
// rule.
//
// The concrete files are under HOME (not /private/var/folders) so the broad
// (subpath "/private/var/folders") rule does NOT cover them — only the
// per-symlink literal allow does. Stripping that literal makes reads fail,
// confirming the two-hop chain is load-bearing.
func TestSandboxExecSopsRotation_TwoHopChainDeniedWithoutConcreteAllow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	for _, entry := range sopsRotationEntries {
		entry := entry // capture loop variable
		t.Run(entry.stagingRelPath, func(t *testing.T) {
			m := newProfileManagerForRotationTest(t)

			stagingHome, err := m.PrepareSandboxExecHome()
			if err != nil {
				t.Fatalf("PrepareSandboxExecHome: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

			intermediateSymlink, concreteFile := setupSopsRotationChain(t, entry)

			stagingLink := filepath.Join(stagingHome, entry.stagingRelPath)
			_ = os.Remove(stagingLink)
			if linkErr := os.Symlink(intermediateSymlink, stagingLink); linkErr != nil {
				t.Fatalf("create staging symlink: %v", linkErr)
			}

			// Resolve to the canonical form generateProfile emits (covers
			// the /var → /private/var alias on Darwin).
			resolved, resolveErr := filepath.EvalSymlinks(concreteFile)
			if resolveErr != nil {
				resolved = concreteFile
			}

			mutatedPath := withMutatedProfile(t, m, func(p string) string {
				// Remove the (literal "<resolved>") line from the allow block.
				// Match only the indented form to avoid accidentally hitting
				// unrelated occurrences.
				toRemove := "  (literal " + sbplQuoteForTest(resolved) + ")\n"
				return strings.ReplaceAll(p, toRemove, "")
			})

			cmd := exec.Command(sandboxExecPath, "-f", mutatedPath,
				"/usr/bin/env", fmt.Sprintf("HOME=%s", stagingHome), nixBash, "-c",
				"cat "+shQuote(stagingLink))
			out, runErr := cmd.CombinedOutput()
			if runErr == nil {
				t.Errorf("%s: read via staging-HOME chain succeeded WITHOUT the (literal ...) allow for the concrete file.\n"+
					"The negative test is not catching the regression — investigate.\n"+
					"Output: %s\nMutated profile: %s", entry.stagingRelPath, string(out), mutatedPath)
			} else {
				t.Logf("ka pai — %s: read correctly denied without concrete-file allow (exit: %v)",
					entry.stagingRelPath, runErr)
			}
		})
	}
}
