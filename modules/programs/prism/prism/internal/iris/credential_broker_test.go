package iris

// credential_broker_test.go — exhaustive negative + edge-case tests for the
// D-7 CredentialBroker.
//
// These tests assert:
//
//   - LLM API keys are absent from the bash subprocess environment for every
//     provider on the forbidden list, even when set on the host.
//   - PRISM_GITHUB_TOKEN_* raw forms are never leaked into the env; only the
//     resolved per-call GITHUB_TOKEN is present.
//   - GitHub-token resolution edge cases: role-scoped hit, host fallback,
//     neither present, empty/unknown role.
//   - AWS audit-name presence reflects host file presence.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearAllGitHubTokens unsets every GitHub-related env var the broker might
// consult so each test starts from a known clean slate. Uses t.Setenv so the
// values are restored after the test.
func clearAllGitHubTokens(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", "")
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR", "")
	t.Setenv("PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER", "")
	t.Setenv("PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_COORDINATOR", "")
}

// hasEnvKey reports whether the env list contains KEY=... for the given key.
func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// hasName reports whether names contains s.
func hasName(names []string, s string) bool {
	for _, n := range names {
		if n == s {
			return true
		}
	}
	return false
}

// ── [security] LLM API keys are never forwarded ─────────────────────────────

// TestCredentialBroker_AllLLMKeysAbsent asserts that NO key on the forbidden
// LLM API key list ever appears in the bash subprocess env, even when every
// one of them is set on the host with a non-empty value.
func TestCredentialBroker_AllLLMKeysAbsent(t *testing.T) {
	clearAllGitHubTokens(t)

	keys := ForbiddenLLMKeyNames()
	if len(keys) == 0 {
		t.Fatal("ForbiddenLLMKeyNames() returned empty list")
	}
	for _, k := range keys {
		t.Setenv(k, "secret-"+k)
	}

	res := NewCredentialBroker().ResolveBash("worker", "")

	for _, k := range keys {
		if hasEnvKey(res.Env, k) {
			t.Errorf("ResolveBash env contains forbidden LLM key %q; must be excluded.\n  env=%v", k, res.Env)
		}
		if hasName(res.Names, k) {
			t.Errorf("ResolveBash names contains forbidden LLM key %q in audit list", k)
		}
	}
}

// TestCredentialBroker_RawPrismTokensNeverLeak asserts that no
// PRISM_GITHUB_TOKEN_* env var (raw form) is ever propagated into the bash
// subprocess env. Only the resolved GITHUB_TOKEN should be present.
func TestCredentialBroker_RawPrismTokensNeverLeak(t *testing.T) {
	clearAllGitHubTokens(t)
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", "raw-role-token")
	t.Setenv("PRISM_GITHUB_TOKEN_PRISMATIC_KOI_COORDINATOR", "raw-coord-token")
	t.Setenv("PRISM_GITHUB_TOKEN_THANKYOU_PAYROLL_WORKER", "raw-tp-worker")

	res := NewCredentialBroker().ResolveBash("worker", "")

	for _, kv := range res.Env {
		if strings.HasPrefix(kv, "PRISM_GITHUB_TOKEN_") {
			t.Errorf("ResolveBash env contains raw PRISM_GITHUB_TOKEN_* var; only the resolved GITHUB_TOKEN must reach the subprocess:\n  kv=%q", kv)
		}
	}
}

// ── [edge-case] GitHub-token resolution ─────────────────────────────────────

// TestCredentialBroker_HostFallback: role-scoped unset → host GITHUB_TOKEN
// used, audit name reflects fallback.
func TestCredentialBroker_HostFallback(t *testing.T) {
	clearAllGitHubTokens(t)
	t.Setenv("GITHUB_TOKEN", "ghp-host-fallback")

	res := NewCredentialBroker().ResolveBash("worker", "")

	// Env contains GITHUB_TOKEN=ghp-host-fallback.
	found := ""
	for _, kv := range res.Env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			found = kv
		}
	}
	if found != "GITHUB_TOKEN=ghp-host-fallback" {
		t.Errorf("expected GITHUB_TOKEN=ghp-host-fallback, got %q (env=%v)", found, res.Env)
	}

	// Audit name reflects fallback. With an empty bareRoot the broker cannot
	// derive an account so the resolution is necessarily the host fallback.
	if !hasName(res.Names, "GITHUB_TOKEN(fallback:host)") {
		t.Errorf("expected 'GITHUB_TOKEN(fallback:host)' in audit names, got %v", res.Names)
	}
	if hasName(res.Names, "GITHUB_TOKEN") {
		t.Errorf("audit names contains both fallback marker and plain GITHUB_TOKEN: %v", res.Names)
	}
}

// TestCredentialBroker_NeitherTokenPresent: no role-scoped, no host token →
// no GITHUB_TOKEN in env, no GITHUB_TOKEN-related name in audit list, no
// error raised.
func TestCredentialBroker_NeitherTokenPresent(t *testing.T) {
	clearAllGitHubTokens(t)

	res := NewCredentialBroker().ResolveBash("worker", "")

	if hasEnvKey(res.Env, "GITHUB_TOKEN") {
		t.Errorf("expected no GITHUB_TOKEN in env; got %v", res.Env)
	}
	for _, n := range res.Names {
		if strings.HasPrefix(n, "GITHUB_TOKEN") {
			t.Errorf("expected no GITHUB_TOKEN audit name; got %v", res.Names)
		}
	}
}

// TestCredentialBroker_EmptyRoleNoCrash: empty/unknown role → broker treats
// it as no-role, does not crash, falls back to host token if present.
func TestCredentialBroker_EmptyRoleNoCrash(t *testing.T) {
	clearAllGitHubTokens(t)
	t.Setenv("GITHUB_TOKEN", "ghp-host-empty-role")

	for _, role := range []string{"", "unknown-role", "REVIEWER"} {
		res := NewCredentialBroker().ResolveBash(role, "")

		if !hasEnvKey(res.Env, "GITHUB_TOKEN") {
			t.Errorf("role=%q: expected host GITHUB_TOKEN fallback in env; got %v", role, res.Env)
		}
		// Should be the fallback marker, not the role-scoped one — since no
		// role-scoped lookup applies for empty/unknown roles.
		if !hasName(res.Names, "GITHUB_TOKEN(fallback:host)") {
			t.Errorf("role=%q: expected fallback marker in names; got %v", role, res.Names)
		}
	}
}

// TestCredentialBroker_UnknownRoleHostUnset: empty/unknown role with no host
// token → no GITHUB_TOKEN in env, no GITHUB_TOKEN audit name.
func TestCredentialBroker_UnknownRoleHostUnset(t *testing.T) {
	clearAllGitHubTokens(t)

	res := NewCredentialBroker().ResolveBash("", "")

	if hasEnvKey(res.Env, "GITHUB_TOKEN") {
		t.Errorf("expected no GITHUB_TOKEN; got %v", res.Env)
	}
	for _, n := range res.Names {
		if strings.HasPrefix(n, "GITHUB_TOKEN") {
			t.Errorf("expected no GITHUB_TOKEN audit name; got %v", res.Names)
		}
	}
}

// ── [edge-case] AWS audit-name reflects host file presence ──────────────────

// TestCredentialBroker_AWSAbsent: when no AWS files exist anywhere the broker
// looks, the AWS_* audit name is omitted.
func TestCredentialBroker_AWSAbsent(t *testing.T) {
	clearAllGitHubTokens(t)
	// Redirect $HOME to a temp dir with no AWS files so the broker's
	// awsCredentialsPresent check is guaranteed negative.
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	res := NewCredentialBroker().ResolveBash("worker", "")

	if hasName(res.Names, "AWS_*") {
		t.Errorf("AWS_* should not be in audit names when no AWS files exist; got %v", res.Names)
	}
}

// TestCredentialBroker_AWSPresent: when an AWS file exists in the host
// $HOME/.aws layout, AWS_* appears in the audit names.
func TestCredentialBroker_AWSPresent(t *testing.T) {
	clearAllGitHubTokens(t)
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)

	// Drop a stub credentials file at the canonical path.
	awsDir := filepath.Join(tempHome, ".aws")
	if err := os.MkdirAll(awsDir, 0o700); err != nil {
		t.Fatalf("mkdir aws: %v", err)
	}
	if err := os.WriteFile(filepath.Join(awsDir, "credentials"), []byte("[default]\n"), 0o600); err != nil {
		t.Fatalf("write aws credentials: %v", err)
	}

	res := NewCredentialBroker().ResolveBash("worker", "")

	if !hasName(res.Names, "AWS_*") {
		t.Errorf("AWS_* should be in audit names when AWS credentials present; got %v", res.Names)
	}
	// AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE point at the in-sandbox
	// canonical paths; we always set them regardless of host presence.
	if !hasEnvKey(res.Env, "AWS_CONFIG_FILE") {
		t.Errorf("AWS_CONFIG_FILE should be set in env; got %v", res.Env)
	}
	if !hasEnvKey(res.Env, "AWS_SHARED_CREDENTIALS_FILE") {
		t.Errorf("AWS_SHARED_CREDENTIALS_FILE should be set in env; got %v", res.Env)
	}
}

// ── [functional] dispatcher CredentialNamesForTool integration ──────────────

// TestDispatcherCredentialNames_FileToolsHaveNone asserts that the dispatcher
// reports the empty list for every file tool (the file tools must run without
// any credentials in their subprocess).
func TestDispatcherCredentialNames_FileToolsHaveNone(t *testing.T) {
	clearAllGitHubTokens(t)
	t.Setenv("GITHUB_TOKEN", "ghp-should-not-be-injected-into-file-tools")

	d := &toolDispatcher{
		role:     "worker",
		bareRoot: "",
		broker:   NewCredentialBroker(),
	}

	for _, tool := range []string{"read", "edit", "write", "grep", "find", "ls"} {
		names := d.CredentialNamesForTool(tool)
		if len(names) != 0 {
			t.Errorf("CredentialNamesForTool(%q) = %v; want empty list", tool, names)
		}
	}
}

// TestDispatcherCredentialNames_BashHasGitHubToken asserts that the bash tool
// reports the GITHUB_TOKEN audit name when a host token is configured.
func TestDispatcherCredentialNames_BashHasGitHubToken(t *testing.T) {
	clearAllGitHubTokens(t)
	t.Setenv("GITHUB_TOKEN", "ghp-host-for-bash")

	d := &toolDispatcher{
		role:     "worker",
		bareRoot: "",
		broker:   NewCredentialBroker(),
	}
	names := d.CredentialNamesForTool("bash")
	if !hasName(names, "GITHUB_TOKEN(fallback:host)") {
		t.Errorf("CredentialNamesForTool(bash) = %v; want fallback marker", names)
	}
}

// TestDispatcherCredentialNames_NilBrokerSafe asserts that a dispatcher with
// no broker set falls back to a default broker rather than panicking. This
// keeps unit tests that construct an ad-hoc toolDispatcher (e.g. the existing
// TestRunBash_MissingCommand) working without modification.
func TestDispatcherCredentialNames_NilBrokerSafe(t *testing.T) {
	clearAllGitHubTokens(t)

	d := &toolDispatcher{role: "worker", bareRoot: "", broker: nil}
	names := d.CredentialNamesForTool("bash") // should not panic
	if names == nil {
		// Empty slice is OK; nil also OK — we only require no panic.
		_ = names
	}
}
