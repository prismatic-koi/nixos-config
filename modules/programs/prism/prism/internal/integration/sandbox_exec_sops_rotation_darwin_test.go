//go:build darwin

package integration_test

// sandbox_exec_sops_rotation_darwin_test.go — integration coverage for sops
// rotation safety on the env-var delivery routes (#1410/#1573 property,
// post-#2132 mechanism).
//
// After darwin-rebuild switch, sops rotates secrets.d/<N>/ → secrets.d/<N+1>/
// and removes the old directory. Post Step 5 of #2132 there is no staging
// HOME and no per-symlink (literal …) target allow: every sops-backed
// credential read inside the sandbox goes through a STABLE host source path
// (~/.ssh/<keyname> embedded in the work-dir ssh-config/gitconfig;
// ~/.config/aws/* and ~/.config/kube/* via env vars) and rides the broad
// (subpath "/private/var/folders") allow narrowed by the #2211 secrets.d
// allowlist. Rotation safety is carried by the allowlist exceptions being
// counter-independent ([0-9]+ regexes derived from secret NAMES).
//
// Coverage map:
//   - The ssh-key stable chains (access + signing keys) are read in-sandbox
//     by TestSandboxExecSecretsDeny_AllowlistedStableChainsReadable
//     (sandbox_exec_secrets_deny_darwin_test.go).
//   - Generic counter-rotation of an allowlisted name is proven by
//     TestSandboxExecSecretsDeny_RotationSimulation and its load-bearing
//     negative.
//   - This file pins the kube config variant explicitly (issue #2235
//     edge-case AC): the KUBECONFIG env-var route must survive a rotation,
//     and the kube require-not exception must be the load-bearing grant.
//
// The kube tests derive the kube secret NAME from the real host source,
// plant a fake secrets.d tree under the per-user TMPDIR (where the
// production deny/exception regexes apply with no profile mutation and no
// host-state writes), and prove reads of that name survive a counter
// rotation.
//
// Each positive has a paired profile-mutation negative proving it is not a
// no-op (docs/sandbox-exec-testing.md, #1192). The #2207 capability-probe
// gating applies via requireSandboxExec.
//
// Shared helpers:
//   - requireSandboxExec, requireNixBash, newProfileManager,
//     preparePositiveProfile, writeAugmentedPositiveProfile,
//     withMutatedProfile, sbplQuoteForTest
//     → sandbox_exec_helpers_darwin_test.go
//   - parseSecretsDAllowlist, setupFakeSecretsTree, writeFakeSecretsCounter,
//     fakeAllowedSentinel, sandboxCatDiscard
//     → sandbox_exec_secrets_deny_darwin_test.go
//   - secretsDNameForTest, regexQuoteForTest
//     → sandbox_exec_aws_config_envvar_darwin_test.go

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// kubeRotationHostSource returns the stable host XDG path of the kube
// agents config and its secrets.d-relative name, skipping when the source
// is absent or not sops-backed on this host (the #2211 allowlist mechanism
// under test does not apply then). This is the guard for the kube rotation
// tests' indirection invariant: the secret NAME is derived from the same
// stable source that feeds collectSecretsDAllowlistNames, so the fake-tree
// reads below exercise exactly the exception the production profile carries
// for the kube config.
func kubeRotationHostSource(t *testing.T) (configPath, secretName string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	configPath = filepath.Join(home, ".config", "kube", "agents-config")
	secretName = secretsDNameForTest(t, configPath)
	return configPath, secretName
}

// TestSandboxExecSopsRotation_KubeConfigAllowlistCounterIndependent is the
// kube positive rotation entry (issue #2235 edge-case AC: a sops rotation
// after spawn does not break kube config reads). The kube config rides the
// #2211 allowlist: KUBECONFIG points at the stable XDG symlink, and the
// in-sandbox read of the resolved secrets.d target is permitted by the
// counter-independent ([0-9]+) require-not exception for the kube secret
// name. The test derives that name from the real host source, plants a
// fake secrets.d tree at counters 100 → 101, and asserts reads of the
// kube-named secret succeed at both counters under the production profile
// — the fake counters carry no per-path allows, so the exception is the
// only mechanism that can permit the read.
func TestSandboxExecSopsRotation_KubeConfigAllowlistCounterIndependent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, kubeName := kubeRotationHostSource(t)

	m := newProfileManager(t)

	prepared, _ := preparePositiveProfile(t, m)

	// The profile must carry the kube exception — the grant the env-var
	// route rides on (issue #2211 / #2235).
	found := false
	for _, name := range parseSecretsDAllowlist(prepared.content) {
		if name == kubeName {
			found = true
		}
	}
	if !found {
		t.Fatalf("profile allowlist does not carry the kube config exception %q — collectSecretsDAllowlistNames regressed (issue #2235).\nProfile:\n%s",
			kubeName, prepared.content)
	}

	base := setupFakeSecretsTree(t, kubeName, "100")
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	readAtCounter := func(counter string) {
		t.Helper()
		target := filepath.Join(base, "secrets.d", counter, filepath.FromSlash(kubeName))
		cmd := exec.Command(sandboxExecPath, "-f", profilePath,
			nixBash, "-c", "cat "+shQuote(target))
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Errorf("counter %s: in-sandbox read of kube secret name %q failed — the kube exception is not counter-independent (#1410/#1573 regression via #2235).\nExit: %v\nOutput: %s",
				counter, kubeName, runErr, out)
			return
		}
		if !strings.Contains(string(out), fakeAllowedSentinel) {
			t.Errorf("counter %s: kube secret read exited 0 but sentinel missing.\nOutput: %s", counter, out)
		}
	}

	// Generation 100, then simulate a sops rotation: write 101, prune 100.
	readAtCounter("100")
	writeFakeSecretsCounter(t, base, "101", kubeName)
	if err := os.RemoveAll(filepath.Join(base, "secrets.d", "100")); err != nil {
		t.Fatalf("prune fake counter 100: %v", err)
	}
	readAtCounter("101")
}

// TestSandboxExecSopsRotation_KubeConfigExceptionLoadBearing is the paired
// negative for the kube rotation entry (sandbox-exec testing convention,
// #1192): with the kube require-not exception stripped from the profile,
// the same fake-counter read fails — proving the exception (not some
// broader rule) is what permits the kube config read in the positive test.
func TestSandboxExecSopsRotation_KubeConfigExceptionLoadBearing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	_, kubeName := kubeRotationHostSource(t)

	m := newProfileManager(t)

	base := setupFakeSecretsTree(t, kubeName, "100")
	target := filepath.Join(base, "secrets.d", "100", filepath.FromSlash(kubeName))

	exceptionLine := `    (require-not (regex #"/secrets\.d/[0-9]+/` + regexQuoteForTest(kubeName) + `$"))` + "\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, exceptionLine, "")
	})

	runErr, out := sandboxCatDiscard(mutatedPath, nixBash, target)
	if runErr == nil {
		t.Errorf("in-sandbox read of kube secret name %q succeeded WITHOUT the allowlist exception.\n"+
			"The exception is not the load-bearing grant — the positive rotation test is a no-op.\n"+
			"Mutated profile: %s", kubeName, mutatedPath)
	} else {
		t.Logf("ka pai — kube secret read correctly denied without the exception (exit: %v, stderr: %s)", runErr, out)
	}
}
