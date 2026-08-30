//go:build darwin

package integration_test

// sandbox_exec_aws_config_envvar_darwin_test.go — integration coverage for
// the env-var delivery of the aws config/credentials files.
//
// The staging-HOME .aws/config and .aws/credentials symlinks are gone. The
// aws CLI resolves both files via AWS_CONFIG_FILE /
// AWS_SHARED_CREDENTIALS_FILE pointing at the host XDG paths
// (~/.config/aws/readonly-config and ~/.config/aws/credentials). No literal
// SBPL grant exists on the XDG symlink paths (it is inert — SBPL evaluates
// resolved targets): the in-sandbox read of the resolved sops target rides
// the broad (subpath "/private/var/folders") allow narrowed by the secrets.d
// allowlist, whose aws-readonly-config exception is derived from the same
// stable XDG source path.
//
// This file tests:
//
//  1. Positive: a real config-resolving aws invocation —
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
//  2. Negative: stripping the aws-readonly-config require-not exception
//     from the secrets.d deny makes the same invocation fail — proving the
//     allowlist exception is the load-bearing grant for the env-var route
//     (sandbox-exec testing convention).
//
// Capability-probe gating applies via requireSandboxExec. Shared helpers
// live in sandbox_exec_helpers_darwin_test.go. The allowlist parse helpers
// live in sandbox_exec_secrets_deny_darwin_test.go.

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
// Apple-signed or homebrew binary SIGABRTs or skews the signal under the
// deny-default profile — same rationale as requireNixBash).
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
// into a secrets.d/<N>/ path — the mechanism under test), parses a named
// profile from it, AND verifies the host's SSO session is live enough for
// `aws configure list --profile <name>` to exit 0 (the in-sandbox assertion).
// Skips when any precondition is missing — including expired SSO, where the
// failure is environmental and not distinguishable from a sandbox-side
// regression.
//
// Returns (configPath, resolvedTarget, profileName). configPath is the
// stable XDG symlink path (what AWS_CONFIG_FILE carries in production).
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

	// SSO precheck: when the profile uses a `sso_session = ...` block,
	// `aws configure list --profile <name>` triggers an STS token refresh
	// during botocore credential resolution. An expired host SSO token
	// makes the test exit 255 with "Token has expired and refresh failed"
	// — environmental, not a sandbox-side regression. Probe the host
	// directly (no sandbox involved) and skip when the precondition fails.
	awsBin, lookErr := exec.LookPath("aws")
	if lookErr != nil {
		t.Skipf("aws CLI not in PATH on host: %v", lookErr)
	}
	probe := exec.Command(awsBin, "configure", "list", "--profile", profileName)
	probe.Env = append(os.Environ(), "AWS_EC2_METADATA_DISABLED=true")
	if probeOut, probeErr := probe.CombinedOutput(); probeErr != nil {
		lower := strings.ToLower(string(probeOut))
		switch {
		case strings.Contains(lower, "token has expired"),
			strings.Contains(lower, "refresh failed"),
			strings.Contains(lower, "sso login"),
			strings.Contains(lower, "sso session"):
			t.Skipf("host SSO precondition not met for profile %q (expired/unavailable token): %v\nHost-side `aws configure list` output:\n%s\nRun `aws sso login --sso-session <name>` and retry.", profileName, probeErr, probeOut)
		default:
			t.Skipf("host-side `aws configure list --profile %s` failed: %v\nOutput:\n%s", profileName, probeErr, probeOut)
		}
	}
	return configPath, resolvedTarget, profileName
}

// awsConfigureListCmd builds the in-sandbox `aws configure list` invocation
// with the production env-var shape: HOME at the real host home,
// AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE at the host XDG paths. AWS_EC2_METADATA_DISABLED keeps the credential chain from probing
// IMDS (hermeticity — irrelevant to config resolution).
func awsConfigureListCmd(t *testing.T, profilePath, awsBin, nixBash, configPath, profileName string) *exec.Cmd {
	t.Helper()
	home, _ := os.UserHomeDir()
	credentialsPath := filepath.Join(home, ".config", "aws", "credentials")
	script := shQuote(awsBin) + " configure list --profile " + shQuote(profileName)
	return exec.Command(sandboxExecPath, "-f", profilePath,
		"/usr/bin/env",
		"HOME="+realUserHome(t),
		"AWS_CONFIG_FILE="+configPath,
		"AWS_SHARED_CREDENTIALS_FILE="+credentialsPath,
		"AWS_EC2_METADATA_DISABLED=true",
		nixBash, "-c", script)
}

// TestSandboxExecAWSConfig_EnvVarResolution is the positive integration test
// for the env-var delivery route. It runs a real
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
	// the ancestor block's file-read-metadata allow (same as the
	// stable-chain tests).
	m := newProfileManagerWithBareRoot(t)

	prepared, _ := preparePositiveProfile(t, m)

	// The profile must carry the aws-readonly-config allowlist exception —
	// the grant the env-var route rides on.
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

	cmd := awsConfigureListCmd(t, testProfilePath, awsBin, nixBash, configPath, profileName)
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
// is the paired negative test (sandbox-exec testing convention). It strips
// the aws-readonly-config require-not exception from the secrets.d deny and
// asserts the same aws invocation fails — proving the
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

	resolvedName := secretsDNameForTest(t, configPath)

	// Remove the require-not exception line for the aws config name,
	// mirroring the generator's emission format exactly (generateProfile +
	// regexQuotePath).
	exceptionLine := `    (require-not (regex #"/secrets\.d/[0-9]+/` + regexQuoteForTest(resolvedName) + `$"))` + "\n"
	mutatedPath := withMutatedProfile(t, m, func(p string) string {
		return strings.ReplaceAll(p, exceptionLine, "")
	})

	cmd := awsConfigureListCmd(t, mutatedPath, awsBin, nixBash, configPath, profileName)
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
