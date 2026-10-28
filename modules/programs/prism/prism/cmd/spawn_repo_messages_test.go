package cmd

// Message-content tests for issue #2982's recovery-guidance requirement:
// each failure produced by the repo-resolution / self-delivery paths must
// name the recovery action, not just the fault. See spawn_repo_proxy_test.go
// for the request-forwarding coverage and
// internal/sidecar/host_api_spawn_repo_test.go for the host-API-level
// coverage of the same two failure modes.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveRepo_UnresolvableValue_NamesNoSessionCreated verifies that an
// unresolvable --repo value produces an error naming the bad value and
// stating that no session was created, so nothing was delivered anywhere.
func TestResolveRepo_UnresolvableValue_NamesNoSessionCreated(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // ~/code will not exist under this HOME
	_, err := resolveRepo("does-not-exist")
	if err == nil {
		t.Fatal("resolveRepo: got nil error, want an error for an unresolvable repo")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error does not name the unresolvable value: %v", err)
	}
	if !strings.Contains(err.Error(), "no session was created") {
		t.Errorf("error does not state that no session was created: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing was delivered") {
		t.Errorf("error does not state that nothing was delivered: %v", err)
	}
}

// TestSelfDeliveryError_NamesCallerAndRecovery verifies that the
// self-delivery refusal:
//   - names the resolved target session and the calling session
//   - states that no prompt was delivered
//   - instructs the caller to re-send the original prompt text, not
//     rewrite it from memory (issue #2982's field evidence: a caller that
//     reconstructed a four-item request from memory lost one item)
func TestSelfDeliveryError_NamesCallerAndRecovery(t *testing.T) {
	const target = "home-ops@main"
	const caller = "home-ops@main"
	err := selfDeliveryError(target, caller)
	if err == nil {
		t.Fatal("selfDeliveryError: got nil, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, target) {
		t.Errorf("error does not name the resolved target session: %v", msg)
	}
	if !strings.Contains(msg, caller) {
		t.Errorf("error does not name the calling session: %v", msg)
	}
	if !strings.Contains(msg, "no prompt was delivered") {
		t.Errorf("error does not state that no prompt was delivered: %v", msg)
	}
	if !strings.Contains(msg, "re-send the original prompt text") {
		t.Errorf("error does not instruct the caller to re-send the original text: %v", msg)
	}
	if strings.Contains(msg, "rewrite") == false {
		t.Errorf("error does not warn against rewriting from memory: %v", msg)
	}
}

// TestResolveRepo_RefusesRootInsideProject: a .bare that a session makes in
// its own worktree must not become the root of `prism spawn`. The host-side
// spawn runs `git worktree add` in that root.
func TestResolveRepo_RefusesRootInsideProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := filepath.Join(home, "code", "proj")
	planted := filepath.Join(proj, "main", "x")
	for _, d := range []string{filepath.Join(proj, ".bare"), filepath.Join(planted, ".bare")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	if got, err := resolveRepo("proj"); err != nil || got != proj {
		t.Fatalf("resolveRepo(proj) = %q, %v; want %q", got, err, proj)
	}
	if got, err := resolveRepo(proj); err != nil || got != proj {
		t.Fatalf("resolveRepo(%s) = %q, %v; want the root", proj, got, err)
	}
	for _, name := range []string{"proj/main/x", planted} {
		got, err := resolveRepo(name)
		if err == nil || !strings.Contains(err.Error(), "inside the prism project") {
			t.Errorf("resolveRepo(%q) = %q, %v; want a refusal", name, got, err)
		}
	}
}
