package container

// argv_redact_test.go — the package-wide, test-only redaction for argv and
// env dumps, plus its own coverage.
//
// Why this exists. bwrapIsolator.BuildArgs embeds the output of
// credentialEnvVars in the bwrap argv, so a built argv carries live host
// secrets:
//
//	--setenv ANTHROPIC_API_KEY  <key>
//	--setenv OPENROUTER_API_KEY <key>
//	--setenv GITHUB_TOKEN       <PAT>
//
// A test that formats the raw argv with %v therefore writes those values to
// the developer's terminal and to the CI log every time it fails — the two
// worst places for a secret to land. The exposure is real: it was observed
// live on the worker.
//
// The rule for this package: no test formats a whole argv or env slice
// directly. Every such dump goes through redactedArgs first. External test
// files (package container_test) use the RedactedArgsForTest wrapper in
// export_test.go.
//
// The redaction is keyed on the variable NAME, not on the value:
//
//   - It keeps the name and masks only the value, so a test can still assert
//     that a variable is present in a dump ("--setenv GITHUB_TOKEN
//     <redacted>").
//   - It leaves every other element untouched — bind triples included — so
//     mount failures stay debuggable.
//   - It preserves the length and the order of the slice, so a dump made with
//     no credential in the environment is identical to the raw argv.
//
// A value-scanning redactor was rejected: it corrupts any unrelated argument
// that happens to contain the same text (a store path, a session name), which
// makes a failure message misleading in exactly the case a reader most needs
// it to be exact.

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// redactedValuePlaceholder replaces the value of a credential-bearing
// variable in a dump. It is not a valid value for any of those variables, so
// its presence in a failure message is unambiguous.
const redactedValuePlaceholder = "<redacted>"

// credentialEnvNames is the set of env-var NAMES whose values must never
// reach a test failure message. It is derived from the production forwarding
// list (credentialForwardEnvKeys) and the production token variable names, so
// a credential added to credentialEnvVars is redacted here without a second
// edit. TestRedactedArgs_RedactsEveryCredentialEnvVarsEntry pins that link.
//
// githubTokenEnvKey and gitlabTokenEnvKey are named explicitly because
// neither is forwarded verbatim: both are resolved host-side from a sops
// file and injected as a value, so neither appears in
// credentialForwardEnvKeys.
var credentialEnvNames = func() map[string]bool {
	m := make(map[string]bool, len(credentialForwardEnvKeys)+2)
	for _, k := range credentialForwardEnvKeys {
		m[k] = true
	}
	m[githubTokenEnvKey] = true
	m[gitlabTokenEnvKey] = true
	return m
}()

// isCredentialEnvName reports whether name identifies a variable whose value
// is a credential. The match on credentialEnvNames is exact; the
// PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE> family matches on its prefix because
// the key half is host-specific.
func isCredentialEnvName(name string) bool {
	return credentialEnvNames[name] || strings.HasPrefix(name, prismGitHubTokenEnvPrefix)
}

// redactedArgs returns a copy of args in which the value of every
// credential-bearing variable is replaced by redactedValuePlaceholder. Use it
// for every dump of a whole argv or env slice in this package.
//
// Two element shapes are recognised:
//
//	"--setenv", NAME, VALUE   the bwrap injection form (three elements)
//	"NAME=VALUE"              the exec-env form (credentialEnvVars,
//	                          AppendSandboxEnvVarsKV, MinimalIsolatedExecEnv)
//
// Everything else is copied verbatim. args is never mutated; a nil argument
// returns nil.
func redactedArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		// bwrap form: the value sits two elements after the flag.
		if a == "--setenv" && i+2 < len(out) && isCredentialEnvName(out[i+1]) {
			out[i+2] = redactedValuePlaceholder
			continue
		}
		// exec-env form. The name must match a credential name exactly, so
		// an unrelated element that merely contains "=" (a path, a git
		// config assignment) is left alone.
		if name, _, found := strings.Cut(a, "="); found && isCredentialEnvName(name) {
			out[i] = name + "=" + redactedValuePlaceholder
		}
	}
	return out
}

// clearCredentialEnv blanks every host variable that can put a credential
// into a built argv, so a test starts from a known-empty baseline and can
// never pick up a real value from the developer's shell. t.Setenv restores
// the prior values at test end.
func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, k := range credentialForwardEnvKeys {
		t.Setenv(k, "")
	}
	t.Setenv(githubTokenEnvKey, "")
	t.Setenv(gitlabTokenEnvKey, "")
	// The PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE> set is host-specific — read it
	// off the live environment rather than hard-coding the account list.
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, prismGitHubTokenEnvPrefix) {
			t.Setenv(name, "")
		}
	}
}

// Synthetic credential values. Every test in this file installs these with
// t.Setenv, so no real host secret is ever read, formatted, or asserted on.
const (
	syntheticAnthropicKey  = "sk-ant-synthetic-0000000000"
	syntheticOpenRouterKey = "sk-or-synthetic-1111111111"
	syntheticGitHubToken   = "ghp_synthetic222222222222"
	syntheticGitLabToken   = "glpat-synthetic33333333333"
)

// TestRedactedArgs_MasksValueKeepsName is the core unit: the bwrap triple
// keeps its flag and its variable name, and loses only the value.
func TestRedactedArgs_MasksValueKeepsName(t *testing.T) {
	args := []string{
		"--setenv", "ANTHROPIC_API_KEY", syntheticAnthropicKey,
		"--setenv", "OPENROUTER_API_KEY", syntheticOpenRouterKey,
		"--setenv", "GITHUB_TOKEN", syntheticGitHubToken,
		"--setenv", "PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", syntheticGitHubToken,
		"--setenv", "PRISM_SESSION_NAME", "repo@main",
	}
	want := []string{
		"--setenv", "ANTHROPIC_API_KEY", redactedValuePlaceholder,
		"--setenv", "OPENROUTER_API_KEY", redactedValuePlaceholder,
		"--setenv", "GITHUB_TOKEN", redactedValuePlaceholder,
		"--setenv", "PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER", redactedValuePlaceholder,
		"--setenv", "PRISM_SESSION_NAME", "repo@main",
	}
	if got := redactedArgs(args); !reflect.DeepEqual(got, want) {
		t.Errorf("redactedArgs() = %v, want %v", got, want)
	}
}

// TestRedactedArgs_MasksExecEnvForm covers the K=V shape used by
// credentialEnvVars and the sandbox-exec dispatch path.
func TestRedactedArgs_MasksExecEnvForm(t *testing.T) {
	env := []string{
		"ANTHROPIC_API_KEY=" + syntheticAnthropicKey,
		"GITHUB_TOKEN=" + syntheticGitHubToken,
		"PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER=" + syntheticGitHubToken,
		"GIT_EDITOR=true",
		"PATH=/usr/bin:/bin",
	}
	want := []string{
		"ANTHROPIC_API_KEY=" + redactedValuePlaceholder,
		"GITHUB_TOKEN=" + redactedValuePlaceholder,
		"PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER=" + redactedValuePlaceholder,
		"GIT_EDITOR=true",
		"PATH=/usr/bin:/bin",
	}
	if got := redactedArgs(env); !reflect.DeepEqual(got, want) {
		t.Errorf("redactedArgs() = %v, want %v", got, want)
	}
}

// TestRedactedArgs_LeavesNonCredentialArgsUntouched is the edge case from the
// issue: a credential value that also appears inside an unrelated argument is
// redacted where it is a credential, and the unrelated argument keeps its
// exact text. Bind triples in particular must survive verbatim — they are
// what a mount failure is diagnosed against.
func TestRedactedArgs_LeavesNonCredentialArgsUntouched(t *testing.T) {
	dir := "/tmp/" + syntheticGitHubToken + "-worktree"
	args := []string{
		"--ro-bind", dir, dir,
		"--bind", dir + "/.git", dir + "/.git",
		"--setenv", "GITHUB_TOKEN", syntheticGitHubToken,
		"--setenv", "PRISM_SPAWN_PATH", dir,
		"--chdir", dir,
		"--", "pi",
	}
	got := redactedArgs(args)

	// The credential position is masked.
	if got[8] != redactedValuePlaceholder {
		t.Errorf("GITHUB_TOKEN value not redacted; got[8] = %q", got[8])
	}
	// Every other element is byte-identical to the input, including the ones
	// that carry the credential text as a substring.
	for i := range args {
		if i == 8 {
			continue
		}
		if got[i] != args[i] {
			t.Errorf("non-credential arg %d changed: got %q, want %q", i, got[i], args[i])
		}
	}
	// The input slice itself is not mutated.
	if args[8] != syntheticGitHubToken {
		t.Error("redactedArgs mutated its argument")
	}
}

// TestRedactedArgs_ShapeUnchangedWithoutCredentials is the edge case for a
// clean environment: with no credential in the argv the redacted dump is the
// argv, element for element. Nothing is dropped, reordered, or reformatted.
func TestRedactedArgs_ShapeUnchangedWithoutCredentials(t *testing.T) {
	clearCredentialEnv(t)

	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      t.TempDir(),
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	got := redactedArgs(args)
	if len(got) != len(args) {
		t.Fatalf("redacted dump has %d elements, want %d", len(got), len(args))
	}
	if !reflect.DeepEqual(got, args) {
		// Report positions only — never the values.
		for i := range args {
			if got[i] != args[i] {
				t.Errorf("element %d differs with no credential set", i)
			}
		}
	}
	if redactedArgs(nil) != nil {
		t.Error("redactedArgs(nil) must return nil")
	}
}

// TestBwrapBuildArgs_RedactedDumpHidesCredentialValues is the security AC,
// end to end: build a real bwrap argv with synthetic credentials in the
// environment, and prove the redacted dump names every variable but prints no
// value. The Fatalf guards on the raw argv are the no-op check — they fail if
// the argv ever stops carrying the credential, which would make the rest of
// the test vacuous. This test also fails if a raw value is reintroduced into
// the dump, which is the regression it exists to catch.
func TestBwrapBuildArgs_RedactedDumpHidesCredentialValues(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", syntheticAnthropicKey)
	t.Setenv("OPENROUTER_API_KEY", syntheticOpenRouterKey)
	t.Setenv(githubTokenEnvKey, syntheticGitHubToken)
	t.Setenv(gitlabTokenEnvKey, syntheticGitLabToken)

	worktree := t.TempDir()
	m, _, cleanup := bwrapFixture(t, Config{
		SessionName:   "repo@main",
		Worktree:      worktree,
		AllocatedPort: 14010,
	})
	defer cleanup()

	b := &bwrapIsolator{name: m.name}
	args := b.BuildArgs(m)

	secrets := map[string]string{
		"ANTHROPIC_API_KEY":  syntheticAnthropicKey,
		"OPENROUTER_API_KEY": syntheticOpenRouterKey,
		"GITHUB_TOKEN":       syntheticGitHubToken,
		"GITLAB_TOKEN":       syntheticGitLabToken,
	}

	// No-op guard: the raw argv must really carry each value, otherwise the
	// assertions below prove nothing. Only the NAME is reported on failure.
	raw := fmt.Sprintf("%v", args)
	for name, value := range secrets {
		if !strings.Contains(raw, value) {
			t.Fatalf("fixture invariant broken: the raw argv carries no %s value, so this test cannot prove the redaction works", name)
		}
	}

	dump := fmt.Sprintf("%v", redactedArgs(args))
	for name, value := range secrets {
		if strings.Contains(dump, value) {
			t.Errorf("redacted dump still contains the %s value", name)
		}
		// The variable is still named, so a presence assertion stays writable
		// against the redacted form.
		if want := "--setenv " + name + " " + redactedValuePlaceholder; !strings.Contains(dump, want) {
			t.Errorf("redacted dump does not name %s (want %q in the dump)", name, want)
		}
	}

	// The bind triples stay fully visible: mount assertions remain
	// debuggable against the redacted dump.
	if want := "--bind " + worktree + " " + worktree; !strings.Contains(dump, want) {
		t.Errorf("redacted dump lost the worktree bind triple (want %q)", want)
	}
}

// TestRedactedArgs_RedactsEveryCredentialEnvVarsEntry is the drift guard. It
// drives the loop off the production forwarding list, so a credential added
// to credentialEnvVars is exercised here automatically and fails the build
// until redactedArgs covers it.
func TestRedactedArgs_RedactsEveryCredentialEnvVarsEntry(t *testing.T) {
	clearCredentialEnv(t)
	for i, k := range credentialForwardEnvKeys {
		t.Setenv(k, fmt.Sprintf("synthetic-forwarded-value-%d", i))
	}
	t.Setenv(githubTokenEnvKey, syntheticGitHubToken)
	t.Setenv(gitlabTokenEnvKey, syntheticGitLabToken)

	m := New(Config{SessionName: "repo@main", AllocatedPort: 14010, AgentRole: "worker"})
	vars, err := m.credentialEnvVars()
	if err != nil {
		t.Fatalf("credentialEnvVars: %v", err)
	}
	// +2: the GitHub and GitLab tokens, neither of which is in the
	// forwarding list.
	if want := len(credentialForwardEnvKeys) + 2; len(vars) != want {
		t.Fatalf("credentialEnvVars returned %d entries, want %d — the drift guard below would be vacuous", len(vars), want)
	}

	for _, kv := range vars {
		name, value, _ := strings.Cut(kv, "=")

		// Exec-env form.
		if got := redactedArgs([]string{kv})[0]; got != name+"="+redactedValuePlaceholder {
			t.Errorf("credentialEnvVars entry %s is not redacted in the exec-env form", name)
		}
		// bwrap form, as BuildArgs emits it.
		if got := redactedArgs([]string{"--setenv", name, value}); got[2] != redactedValuePlaceholder {
			t.Errorf("credentialEnvVars entry %s is not redacted in the bwrap --setenv form", name)
		}
	}
}
