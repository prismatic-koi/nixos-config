package container

// Tests for ResourceNamePrefixForSession — the single source of truth
// for the per-session prefix the podman proxy injects into container and
// volume names, and the prefix cmd/cleanup_sweep.go sweeps on.
//
// The load-bearing property is that the prefix is PODMAN-SAFE. A stub
// upstream in a proxy test accepts any name, so a test that only asserts
// "the proxy injected something starting with the prefix" cannot catch a
// prefix real podman refuses. These tests assert the shape itself
// against podman's own validation regex.

import (
	"regexp"
	"testing"
)

// podmanNameRegex is libpod's `define.NameRegex`, the pattern podman
// validates a container or volume name against before it creates one.
// A name that fails this is rejected upstream, which makes the create
// endpoint unusable and the matching cleanup sweep dead code.
//
// Copied here deliberately rather than imported: podman is not a
// dependency of this repo, and the point of the test is to pin our
// output against podman's documented contract.
var podmanNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func TestResourceNamePrefixForSession_ProducesPodmanSafeNames(t *testing.T) {
	// Every shape a real session name takes. The first two are the
	// common cases; the review-child and slash-branch shapes are the
	// ones that made the raw-name prefix fail.
	sessions := []string{
		"nixos-config@main",
		"nixos-config@podman-proxy-resource-caps",
		"nixos-config@feature/nested-branch",
		"nixos-config@main~review-1-review-goal",
		"repo@branch.with.dots",
		"obsidian",
	}
	for _, session := range sessions {
		t.Run(session, func(t *testing.T) {
			prefix := ResourceNamePrefixForSession(session)

			// The prefix itself must be podman-safe, and so must a
			// full auto-injected name (prefix + 8 hex chars), which is
			// what the proxy actually sends upstream.
			if !podmanNameRegex.MatchString(prefix) {
				t.Errorf("prefix %q is not a valid podman name — podman rejects every create the proxy injects it into", prefix)
			}
			injected := prefix + "a1b2c3d4"
			if !podmanNameRegex.MatchString(injected) {
				t.Errorf("auto-injected name %q is not a valid podman name", injected)
			}
		})
	}
}

// TestResourceNamePrefixForSession_RejectedCharactersAreReplaced pins
// the specific characters that must not survive into the prefix. This
// is the regression guard: `@` appears in EVERY worktree session name,
// so a prefix built from the raw name is broken for every real session,
// not an edge case.
func TestResourceNamePrefixForSession_RejectedCharactersAreReplaced(t *testing.T) {
	prefix := ResourceNamePrefixForSession("repo@branch/with.parts~child")
	for _, bad := range []string{"@", "/", "~"} {
		if contains(prefix, bad) {
			t.Errorf("prefix %q still contains %q, which podman rejects in a resource name", prefix, bad)
		}
	}
	if want := "prism-repo-branch-with-parts-child-"; prefix != want {
		t.Errorf("prefix: got %q, want %q", prefix, want)
	}
}

// TestResourceNamePrefixForSession_IsNameForSessionPlusDash pins the
// derivation. The sandbox name and the resource prefix must stay in
// agreement — a second, independent sanitiser is exactly the drift this
// helper exists to prevent.
func TestResourceNamePrefixForSession_IsNameForSessionPlusDash(t *testing.T) {
	const session = "nixos-config@main"
	if got, want := ResourceNamePrefixForSession(session), NameForSession(session)+"-"; got != want {
		t.Errorf("prefix: got %q, want %q", got, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
