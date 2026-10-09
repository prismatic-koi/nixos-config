//go:build darwin

package integration_test

// sandbox_exec_container_cli_darwin_test.go — the podman and docker exec deny
// of the sandbox-exec profile (section 21c of generateProfile, #3065).
//
// The pi deny list blocks a `podman` or `docker` command that it can see.
// The profile deny stops a client that the deny list cannot see: `bash -c`
// with an absolute path, or a symlink with another name. The tests plant
// copies of Nix bash under the denied names in TMPDIR, which the profile
// grants (section 3b) and from which exec otherwise works.
//
// The positive test runs each planted name under the production profile
// and expects exec to fail, while a control copy under another name runs.
// The negative test removes the deny and expects the planted podman to run.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// containerCLIExecDenyBlock is the exact section-21c clause of
// generateProfile.
const containerCLIExecDenyBlock = "(deny process-exec*\n" +
	"  (regex #\"/podman$\")\n" +
	"  (regex #\"/docker$\")\n" +
	"  (regex #\"/\\.podman-wrapped$\")\n" +
	"  (regex #\"/\\.docker-wrapped$\"))\n"

// containerCLIRanSentinel is what a planted binary prints when it runs.
const containerCLIRanSentinel = "prism-3065-container-cli-ran"

// runPlantedInSandbox runs `bash -c '"$BIN" -c "printf <sentinel>"'` under
// the profile. The planted binary is a copy of bash, so it prints the
// sentinel when exec succeeds.
func runPlantedInSandbox(t *testing.T, profilePath, nixBash, bin string) (string, error) {
	t.Helper()
	cmd := exec.Command(sandboxExecPath, "-f", profilePath,
		nixBash, "-c", `"$BIN" -c "printf `+containerCLIRanSentinel+`"`)
	cmd.Env = append(os.Environ(), "BIN="+bin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestSandboxExecContainerCLI_DeniedUnderProductionProfile: under the
// production profile, a binary named podman, docker, .podman-wrapped, or
// .docker-wrapped does not run, also through a symlink with another name.
// A copy of the same binary under another name in the same directory runs.
func TestSandboxExecContainerCLI_DeniedUnderProductionProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)
	prepared, _ := preparePositiveProfile(t, m)
	if !strings.Contains(prepared.content, containerCLIExecDenyBlock) {
		t.Fatalf("generated profile does not contain the section-21c deny:\n%s\nProfile:\n%s",
			containerCLIExecDenyBlock, prepared.content)
	}
	profilePath := writeAugmentedPositiveProfile(t, prepared)

	dir := t.TempDir()
	control := plantExecutableForTest(t, nixBash, dir, "ctr-probe")
	out, err := runPlantedInSandbox(t, profilePath, nixBash, control)
	if err != nil || !strings.Contains(out, containerCLIRanSentinel) {
		t.Fatalf("the control binary %s did not run, so a failure of the denied names proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			control, err, out, profilePath)
	}

	podman := plantExecutableForTest(t, nixBash, dir, "podman")
	alias := filepath.Join(dir, "ctr-alias")
	if err := os.Symlink(podman, alias); err != nil {
		t.Fatalf("symlink %s -> %s: %v", alias, podman, err)
	}
	denied := []string{
		podman,
		plantExecutableForTest(t, nixBash, dir, "docker"),
		plantExecutableForTest(t, nixBash, dir, ".podman-wrapped"),
		plantExecutableForTest(t, nixBash, dir, ".docker-wrapped"),
		alias,
	}
	for _, bin := range denied {
		t.Run(filepath.Base(bin), func(t *testing.T) {
			out, err := runPlantedInSandbox(t, profilePath, nixBash, bin)
			if err == nil || strings.Contains(out, containerCLIRanSentinel) {
				t.Errorf("%s ran under the production profile. Check that the section-21c deny follows section 9's (allow process-exec* ...).\nExit: %v\nOutput: %s\nProfile: %s",
					bin, err, out, profilePath)
			}
		})
	}
}

// TestSandboxExecContainerCLI_RunsWithoutDeny is the paired negative: with
// the section-21c deny removed, the planted podman runs. This proves that
// the deny is what stops it.
func TestSandboxExecContainerCLI_RunsWithoutDeny(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)

	m := newProfileManager(t)
	profilePath := withMutatedProfile(t, m, func(p string) string {
		return strings.Replace(p, containerCLIExecDenyBlock, "", 1)
	})

	podman := plantExecutableForTest(t, nixBash, t.TempDir(), "podman")
	out, err := runPlantedInSandbox(t, profilePath, nixBash, podman)
	if err != nil || !strings.Contains(out, containerCLIRanSentinel) {
		t.Fatalf("without the deny the planted podman still did not run, so the positive test proves nothing.\nExit: %v\nOutput: %s\nProfile: %s",
			err, out, profilePath)
	}
}
