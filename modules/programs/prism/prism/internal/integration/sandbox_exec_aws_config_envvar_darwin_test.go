//go:build darwin

package integration_test

// sandbox_exec_aws_config_envvar_darwin_test.go — integration coverage for
// the env-var delivery of the aws config/credentials files (issue #2234,
// Step 3a of #2132).
//
// The staging-HOME .aws/config and .aws/credentials symlinks are gone. The
// aws CLI resolves both files via AWS_CONFIG_FILE /
// AWS_SHARED_CREDENTIALS_FILE pointing at the host XDG paths
// (~/.config/aws/readonly-config and ~/.config/aws/credentials). Per the
// #2132 §2 mechanism note, no literal SBPL grant exists on the XDG symlink
// paths (it would be inert — SBPL evaluates resolved targets): the
// in-sandbox read of the resolved sops target rides the broad
// (subpath "/private/var/folders") allow narrowed by the #2211 secrets.d
// allowlist, whose aws-readonly-config exception is derived from the same
// stable XDG source path.
//
// This file tests:
//
//  1. Shape: after PrepareSandboxExecHome, the staging HOME contains no
//     .aws/config or .aws/credentials symlink (issue #2234 AC).
//
//  2. Positive: a real config-resolving aws invocation —
//     `aws configure list --profile <name>` with <name> parsed from the
//     host config — exits 0 inside sandbox-exec with the env vars pointing
//     at the host XDG paths. The command exits non-zero when the config
//     file cannot be read (botocore treats an unreadable config as absent
//     and then fails to find the named profile), so exit 0 proves the
//     config file was actually read in-sandbox, not just that the env var
//     was present. AWS_SHARED_CREDENTIALS_FILE points at the host XDG
//     credentials path, which is absent on the current host — the edge-case
//     AC that config-only operation works is exercised by the same
//     invocation.
//
//  3. Negative: stripping the aws-readonly-config require-not exception
//     from the #2211 secrets.d deny makes the same invocation fail —
//     proving the allowlist exception is the load-bearing grant for the
//     env-var route (sandbox-exec testing convention, #1192).
//
// Capability-probe gating (#2207) applies via requireSandboxExec. Shared
// helpers live in sandbox_exec_helpers_darwin_test.go; the allowlist parse
// helpers live in sandbox_exec_secrets_deny_darwin_test.go.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// requireNixAws resolves the aws CLI binary via PATH → symlink chain and
// returns the PATH-resolved path (suitable for direct invocation — awscli2
// from nixpkgs is a wrapper chain that must be entered at the top). Skips
// the test when aws is not found or does not resolve into /nix/store (an
// Apple-signed or homebrew binary would SIGABRT or skew the signal under
// the deny-default profile — same rationale as requireNixBash, #1190).
func requireNixAws(t *testing.T) string {
	t.Helper()

	awsPath, err := exec.LookPath("aws")
	if err != nil {
		t.Skipf("aws CLI not found in PATH: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(awsPath)
	if err != nil {
		t.Skipf("EvalSymlinks(%q): %v", awsPath, err)
	}
	if !strings.HasPrefix(resolved, "/nix/store/") {
		t.Skipf("aws resolves to %q which is not a /nix/store/ path — cannot use as test binary under the deny-default sandbox", resolved)
	}
	return awsPath
}

// awsProfileRe matches a named profile section header in an aws config file.
var awsProfileRe = regexp.MustCompile(`(?m)^\[profile ([^\]]+)\]`)

// awsHostConfigForTest locates the host XDG aws config
// (~/.config/aws/readonly-config), requires it to be sops-backed (resolving
// into a secrets.d/<N>/ path — the mechanism under test), and parses a named
// profile from it. Skips when any precondition is missing.
//
// Returns (configPath, resolvedTarget, profileName). configPath is the
// stable XDG symlink path (what AWS_CONFIG_FILE carries in production);
// resolvedTarget is the EvalSymlinks form used to derive the allowlist
// exception name in the negative test.
func awsHostConfigForTest(t *testing.T) (configPath, resolvedTarget, profileName string) {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("cannot determine user home: %v", err)
	}
	configPath = filepath.Join(home, ".config", "aws", "readonly-config")

	resolvedTarget, err = filepath.EvalSymlinks(configPath)
	if err != nil {
		t.Skipf("host aws config %s absent or unresolvable: %v", configPath, err)
	}
	if !strings.Contains(resolvedTarget, "/secrets.d/") {
		t.Skipf("host aws config resolves to %q which is not sops-backed — the #2211 allowlist mechanism under test does not apply", resolvedTarget)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Skipf("cannot read host aws config from the test process: %v", err)
	}
	match := awsProfileRe.FindSubmatch(content)
	if match == nil {
		t.Skipf("host aws config has no [profile <name>] section — cannot distinguish a real config read from botocore's empty-config default behaviour")
	}
	profileName = string(match[1])
	return configPath, resolvedTarget, profileName
}

// awsConfigureListCmd builds the in-sandbox `aws configure list` invocation
// with the production env-var shape: HOME at the staging HOME (still present
// until Step 5 of #2132), AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE at
// the host XDG paths. AWS_EC2_METADATA_DISABLED keeps the credential chain
// from probing IMDS (hermeticity — irrelevant to config resolution).
func awsConfigureListCmd(profilePath, stagingHome, awsBin, nixBash, configPath, profileName string) *exec.Cmd {
	home, _ := os.UserHomeDir()
	credentialsPath := filepath.Join(home, ".config", "aws", "credentials")
	script := shQuote(awsBin) + " configure list --profile " + shQuote(profileName)
	return exec.Command(sandboxExecPath, "-f", profilePath,
		"/usr/bin/env",
		"HOME="+stagingHome,
		"AWS_CONFIG_FILE="+configPath,
		"AWS_SHARED_CREDENTIALS_FILE="+credentialsPath,
		"AWS_EC2_METADATA_DISABLED=true",
		nixBash, "-c", script)
}

// TestSandboxExecAWSConfig_StagingSymlinksGone asserts the issue #2234 shape
// of the staging HOME on the real host: PrepareSandboxExecHome creates no
// .aws/config or .aws/credentials symlink. (The .aws/sso and .aws/cli RW
// symlinks are Step 3e scope and may still be present.)
func TestSandboxExecAWSConfig_StagingSymlinksGone(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}

	m := newProfileManager(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	for _, rel := range []string{".aws/config", ".aws/credentials"} {
		staged := filepath.Join(stagingHome, filepath.FromSlash(rel))
		if _, lstatErr := os.Lstat(staged); lstatErr == nil {
			t.Errorf("staging HOME has %s entry — removed in #2234 (env-var route), must not be recreated", rel)
		}
	}
}

// TestSandboxExecAWSConfig_EnvVarResolution is the positive integration test
// for the env-var delivery route (issue #2234 functional AC). It runs a real
// config-resolving aws invocation inside sandbox-exec under the production
// profile, with AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE pointing at
// the host XDG paths, and asserts the named profile from the host config is
// found (exit 0).
func TestSandboxExecAWSConfig_EnvVarResolution(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	awsBin := requireNixAws(t)

	configPath, _, profileName := awsHostConfigForTest(t)

	// BareRoot variant: traversing the XDG symlink under the real HOME needs
	// the ancestor block's file-read-metadata allow (same as the #2211
	// stable-chain tests).
	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	// Shape check inside the real flow: the staging symlinks are gone.
	for _, rel := range []string{".aws/config", ".aws/credentials"} {
		if _, lstatErr := os.Lstat(filepath.Join(stagingHome, filepath.FromSlash(rel))); lstatErr == nil {
			t.Fatalf("staging HOME has %s entry — removed in #2234, must not be recreated", rel)
		}
	}

	prepared, _ := preparePositiveProfile(t, m)

	// The profile must carry the aws-readonly-config allowlist exception —
	// the grant the env-var route rides on (issue #2211 / #2234).
	resolvedName := secretsDNameForTest(t, configPath)
	found := false
	for _, name := range parseSecretsDAllowlist(prepared.content) {
		if name == resolvedName {
			found = true
		}
	}
	if !found {
		t.Fatalf("profile allowlist does not carry the aws config exception %q — collectSecretsDAllowlistNames regressed (issue #2234).\nProfile:\n%s",
			resolvedName, prepared.content)
	}

	testProfilePath := writeAugmentedPositiveProfile(t, prepared)

	cmd := awsConfigureListCmd(testProfilePath, stagingHome, awsBin, nixBash, configPath, profileName)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("aws configure list --profile %s failed in-sandbox under the production profile.\n"+
			"The aws CLI could not resolve the config via AWS_CONFIG_FILE=%s (issue #2234 AC).\n"+
			"Exit: %v\nOutput: %s\nProfile: %s",
			profileName, configPath, runErr, string(out), testProfilePath)
	}
	t.Logf("ka pai — aws resolved profile %q from %s in-sandbox via AWS_CONFIG_FILE", profileName, configPath)
}

// TestSandboxExecAWSConfig_EnvVarResolutionDeniedWithoutAllowlistException
// is the paired negative test (sandbox-exec testing convention, #1192). It
// strips the aws-readonly-config require-not exception from the #2211
// secrets.d deny and asserts the same aws invocation fails — proving the
// positive is not green by accident: the allowlist exception is the
// load-bearing grant for the env-var config read.
func TestSandboxExecAWSConfig_EnvVarResolutionDeniedWithoutAllowlistException(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	requireSandboxExec(t)
	nixBash := requireNixBash(t)
	awsBin := requireNixAws(t)

	configPath, _, profileName := awsHostConfigForTest(t)

	m := newProfileManagerWithBareRoot(t)

	stagingHome, err := m.PrepareSandboxExecHome()
	if err != nil {
		t.Fatalf("PrepareSandboxExecHome: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stagingHome) })

	resolvedName := secretsDNameForTest(t, configPath)

	// Remove the require-not exception line for the aws config name,
	// mirroring the generator's emission format exactly (generateProfile +
	// regexQuotePath).
	exceptionLine := `    (require-not (regex #"/secrets\.d/[0-9]+/` + regexQuoteForTest(resolvedName) + `$"))` + "\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, exceptionLine, "")
	})

	cmd := awsConfigureListCmd(mutatedPath, stagingHome, awsBin, nixBash, configPath, profileName)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Errorf("aws configure list --profile %s succeeded WITHOUT the %q allowlist exception.\n"+
			"The exception is not the load-bearing grant — investigate.\n"+
			"Output: %s\nMutated profile: %s", profileName, resolvedName, string(out), mutatedPath)
	} else {
		t.Logf("ka pai — aws config read correctly denied without the allowlist exception (exit: %v)", runErr)
	}
}

// secretsDNameForTest extracts the secrets.d-relative name from the resolved
// target of src (e.g. …/secrets.d/<N>/aws-readonly-config →
// "aws-readonly-config"), mirroring container.secretsDRelativeName. Skips
// when src does not resolve into a secrets.d path.
func secretsDNameForTest(t *testing.T, src string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		t.Skipf("EvalSymlinks(%s): %v", src, err)
	}
	const marker = "/secrets.d/"
	idx := strings.Index(resolved, marker)
	if idx < 0 {
		t.Skipf("%s does not resolve into a secrets.d path: %s", src, resolved)
	}
	rest := resolved[idx+len(marker):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		t.Skipf("unexpected secrets.d path shape: %s", resolved)
	}
	return rest[slash+1:]
}

// regexQuoteForTest is a test-local copy of the generator's regexQuotePath
// (unexported in internal/container). It must produce identical output for
// the exception-line substring match in the negative test to find the line.
func regexQuoteForTest(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '|', '^', '$':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
