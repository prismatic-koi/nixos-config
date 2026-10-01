package container

// Tests for resource identity — the instance-ID token the podman proxy
// encodes into every container and volume name, and the exact parse that
// reads it back out (issue #2951).
//
// The two collision shapes the identity replaces are the spine of this
// file. Each gets a pair of sessions whose SANITISED names collide, and
// each asserts that neither session can claim the other's resources once
// the token is in the name:
//
//	FOLDING — `repo@feat/x` and `repo@feat-x` sanitise to one string.
//	NESTING — `foo` is a strict prefix of `foo-bar`.
//
// TestResourceIdentity_AdmittedNamesAreOwned pins the containment
// invariant the proxy relies on. The proxy still admits a create request
// with one prefix comparison; that stays sound only while every name
// inside a session's prefix parses back to that session's token.

import (
	"strings"
	"testing"
)

// Two canonical UUIDs standing in for two session incarnations. They are
// literals rather than uuid.New() calls so a failure message names a
// stable value, and so the expected tokens can be written out in full.
const (
	iidA = "3f2a1b0c-1234-5678-9abc-def012345678"
	iidB = "7d4e5f60-abcd-4321-8765-0fedcba98765"

	tokenA = "3f2a1b0c123456789abcdef012345678"
	tokenB = "7d4e5f60abcd432187650fedcba98765"
)

// ── InstanceTokenForID ────────────────────────────────────────────────────

func TestInstanceTokenForID_StripsHyphensFromCanonicalUUID(t *testing.T) {
	if got := InstanceTokenForID(iidA); got != tokenA {
		t.Errorf("InstanceTokenForID(%q) = %q, want %q", iidA, got, tokenA)
	}
}

// TestInstanceTokenForID_LowercasesUppercaseUUID pins that the token
// space is lowercase-only. instanceTokenPattern matches lowercase hex,
// so an uppercase instance ID that produced an uppercase token would
// parse back as "not identity-scoped" and its resources would fall to
// the legacy rule.
func TestInstanceTokenForID_LowercasesUppercaseUUID(t *testing.T) {
	upper := strings.ToUpper(iidA)
	if got := InstanceTokenForID(upper); got != tokenA {
		t.Errorf("InstanceTokenForID(%q) = %q, want %q", upper, got, tokenA)
	}
}

// TestInstanceTokenForID_RejectsNonCanonicalForms pins the narrow match.
// uuid.Parse accepts all of these and maps several of them onto the SAME
// 16 bytes, which would make two different instance-ID strings share one
// token. The sweep compares instance IDs as strings, so a shared token
// would let one incarnation claim another's resources.
func TestInstanceTokenForID_RejectsNonCanonicalForms(t *testing.T) {
	for _, id := range []string{
		"",
		"test-instance-id",
		"3f2a1b0c123456789abcdef012345678",       // unhyphenated
		"{3f2a1b0c-1234-5678-9abc-def012345678}", // brace-wrapped
		"urn:uuid:3f2a1b0c-1234-5678-9abc-def012345678", // URN
		"3f2a1b0c-1234-5678-9abc-def01234567",           // one char short
		"3f2a1b0c-1234-5678-9abc-def0123456789",         // one char long
		"3f2a1b0g-1234-5678-9abc-def012345678",          // non-hex
	} {
		t.Run(id, func(t *testing.T) {
			if got := InstanceTokenForID(id); got != "" {
				t.Errorf("InstanceTokenForID(%q) = %q, want \"\" (no usable identity)", id, got)
			}
		})
	}
}

// ── ResourceOwnerToken ────────────────────────────────────────────────────

// TestResourceOwnerToken_ParsesTheSegmentExactly is the core of the
// change. The parse must read the token as a whole segment at a fixed
// position, never as a prefix, or both collision shapes come back.
func TestResourceOwnerToken_ParsesTheSegmentExactly(t *testing.T) {
	cases := []struct {
		name      string
		wantToken string
		wantOK    bool
		why       string
	}{
		{"prism-" + tokenA + "-foo-a1b2c3d4", tokenA, true, "auto-injected name"},
		{"prism-" + tokenA + "-foo-my-postgres", tokenA, true, "user-chosen suffix with hyphens"},
		{"prism-" + tokenA + "-foo-", tokenA, true, "the bare prefix itself"},
		{"prism-" + tokenB + "-foo-bar-data", tokenB, true, "nested-looking name owned by B"},
		{"prism-foo-bar-data", "", false, "legacy name, no token"},
		{"prism-foo-a1b2c3d4", "", false, "legacy auto-name, no token"},
		{"user-prism-" + tokenA + "-foo-data", "", false, "substring trap: token is not at the anchored position"},
		{"prism-" + tokenA, "", false, "token with no separator after it cannot come from a prefix"},
		{"prism-", "", false, "root only"},
		{"", "", false, "empty"},
		{"postgres-data", "", false, "not a prism resource"},
		{"prism-3f2a1b0c-1234-5678-9abc-def012345678-foo-x", "", false, "hyphenated UUID in the name is not a token"},
		{"prism-" + tokenA + "0-foo-x", "", false, "33 hex characters is not a token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotToken, gotOK := ResourceOwnerToken(tc.name)
			if gotOK != tc.wantOK || gotToken != tc.wantToken {
				t.Errorf("ResourceOwnerToken(%q) = (%q, %v), want (%q, %v) — %s",
					tc.name, gotToken, gotOK, tc.wantToken, tc.wantOK, tc.why)
			}
		})
	}
}

// ── The two collision shapes ──────────────────────────────────────────────

// TestResourceIdentity_NestedSessionNamesDoNotCollide is the NESTING
// shape from issue #2951's coordinator comment. Session `foo` and live
// session `foo-bar` produce legacy prefixes where one contains the
// other, so `prism-foo-bar-data` belongs to `foo-bar` and also starts
// with `prism-foo-`.
//
// With the token in the name neither session can reach the other's
// resources, in either direction.
func TestResourceIdentity_NestedSessionNamesDoNotCollide(t *testing.T) {
	foo := ResourceNamePrefixForOwner(iidA, "foo")
	fooBar := ResourceNamePrefixForOwner(iidB, "foo-bar")

	if strings.HasPrefix(fooBar, foo) || strings.HasPrefix(foo, fooBar) {
		t.Fatalf("one prefix still nests inside the other: %q and %q", foo, fooBar)
	}

	fooBarVolume := fooBar + "data"
	if ResourceIsOwnedBy(fooBarVolume, iidA) {
		t.Errorf("SECURITY: session foo (%s) claims %q, which belongs to live session foo-bar", iidA, fooBarVolume)
	}
	if !ResourceIsOwnedBy(fooBarVolume, iidB) {
		t.Errorf("session foo-bar does not own its own volume %q", fooBarVolume)
	}

	// The reverse direction: foo may legitimately name a resource whose
	// decorative half looks like foo-bar's. It stays foo's.
	fooVolume := foo + "bar-data"
	if ResourceIsOwnedBy(fooVolume, iidB) {
		t.Errorf("SECURITY: session foo-bar claims %q, which belongs to session foo", fooVolume)
	}
	if !ResourceIsOwnedBy(fooVolume, iidA) {
		t.Errorf("session foo does not own its own volume %q", fooVolume)
	}
}

// TestResourceIdentity_FoldedSessionNamesDoNotCollide is the FOLDING
// shape from the issue title. `repo@feat/x` and `repo@feat-x` sanitise
// to one string, so the legacy prefix cannot tell them apart at all.
func TestResourceIdentity_FoldedSessionNamesDoNotCollide(t *testing.T) {
	const slashed = "repo@feat/x"
	const hyphened = "repo@feat-x"

	// The premise: the legacy prefix really does fold these together.
	// Without this check the rest of the test could pass vacuously on a
	// pair that never collided.
	if ResourceNamePrefixForSession(slashed) != ResourceNamePrefixForSession(hyphened) {
		t.Fatalf("test premise broken: %q and %q no longer fold to the same legacy prefix", slashed, hyphened)
	}

	a := ResourceNamePrefixForOwner(iidA, slashed)
	b := ResourceNamePrefixForOwner(iidB, hyphened)
	if a == b {
		t.Fatalf("identity prefixes still collide: %q", a)
	}

	if ResourceIsOwnedBy(b+"pgdata", iidA) {
		t.Errorf("SECURITY: %q claims a volume of %q", slashed, hyphened)
	}
	if ResourceIsOwnedBy(a+"pgdata", iidB) {
		t.Errorf("SECURITY: %q claims a volume of %q", hyphened, slashed)
	}
}

// TestResourceIdentity_SameSessionNameDifferentIncarnations pins that
// the identity is the INCARNATION, not the session name. A session that is
// opened again after its previous incarnation ended, or that the `prism
// restore` fallback restarts, gets a new instance ID, so its old resources
// stop being owned by the new incarnation — which is why
// cmd/cleanup_sweep.go sweeps the union of every incarnation's token rather
// than just the current one.
func TestResourceIdentity_SameSessionNameDifferentIncarnations(t *testing.T) {
	old := ResourceNamePrefixForOwner(iidA, "repo@main") + "pgdata"
	if !ResourceIsOwnedBy(old, iidA) {
		t.Fatalf("%q is not owned by the incarnation that created it", old)
	}
	if ResourceIsOwnedBy(old, iidB) {
		t.Errorf("%q is owned by a later incarnation; the sweep must union the tokens instead", old)
	}
}

// ── The containment invariant ─────────────────────────────────────────────

// TestResourceIdentity_AdmittedNamesAreOwned pins the invariant that
// makes the proxy's one prefix comparison sound:
//
//	admitted ⊆ owned
//
// The proxy admits a create request when the resource name starts with
// the session's prefix. That is a prefix test, and this change is about
// not deciding ownership with a prefix test — so the two are reconciled
// here: every name inside the prefix parses back to the SAME token, so
// the admitted set is a subset of the owned set and the proxy can never
// admit a name the sweep would attribute elsewhere.
//
// If the token ever moves off its left-anchored position, gains a `-`,
// or loses its fixed width, this test fails.
func TestResourceIdentity_AdmittedNamesAreOwned(t *testing.T) {
	sessions := []string{
		"foo",
		"foo-bar",
		"nixos-config@main",
		"repo@feat/x",
		"repo@feat-x",
		"repo.git@main",
		"nixos-config@main~review-1~review-code",
	}
	// Every suffix shape the create policies admit inside the prefix:
	// the auto-injected 8 hex, a user-chosen name, a name that looks
	// like another session's, and the empty suffix (the bare prefix,
	// which applyVolumeNamePolicy admits and podman accepts).
	suffixes := []string{"", "a1b2c3d4", "my-postgres", "cache.v2", "foo-bar-data", "prism-foo-data"}

	for _, session := range sessions {
		prefix := ResourceNamePrefixForOwner(iidA, session)
		for _, suffix := range suffixes {
			name := prefix + suffix
			if !ResourceIsOwnedBy(name, iidA) {
				t.Errorf("CONTAINMENT VIOLATION: the proxy admits %q for session %q, but the sweep does not own it", name, session)
			}
			if ResourceIsOwnedBy(name, iidB) {
				t.Errorf("CONTAINMENT VIOLATION: %q is admitted for one incarnation and owned by another", name)
			}
		}
	}
}

// ── Podman-safety and the fallback ────────────────────────────────────────

// TestResourceNamePrefixForOwner_ProducesPodmanSafeNames repeats the
// check ResourceNamePrefixForSession already carries, because the token
// adds 33 characters and one more `-` to every name the proxy sends
// upstream. A stub upstream in a proxy test accepts any name, so nothing
// else in the suite would catch a prefix real podman refuses.
func TestResourceNamePrefixForOwner_ProducesPodmanSafeNames(t *testing.T) {
	for _, session := range []string{
		"nixos-config@main",
		"nixos-config@feature/nested-branch",
		"nixos-config@main~review-1-review-goal",
		"repo@branch.with.dots",
		"obsidian",
	} {
		t.Run(session, func(t *testing.T) {
			prefix := ResourceNamePrefixForOwner(iidA, session)
			if !podmanNameRegex.MatchString(prefix) {
				t.Errorf("prefix %q is not a valid podman name", prefix)
			}
			if !podmanNameRegex.MatchString(prefix + "a1b2c3d4") {
				t.Errorf("auto-injected name %q is not a valid podman name", prefix+"a1b2c3d4")
			}
		})
	}
}

// TestResourceNamePrefixForOwner_KeepsTheSessionNameAsDecoration pins
// that the readable half survives. It carries no ownership meaning, but
// it is the only thing that lets an operator read `podman ps` and tell
// which session a container came from without a database lookup.
func TestResourceNamePrefixForOwner_KeepsTheSessionNameAsDecoration(t *testing.T) {
	prefix := ResourceNamePrefixForOwner(iidA, "nixos-config@main")
	if want := "prism-" + tokenA + "-nixos-config-main-"; prefix != want {
		t.Errorf("prefix: got %q, want %q", prefix, want)
	}
}

// TestResourceNamePrefixForOwner_FallsBackWithoutUsableIdentity pins the
// documented degradation. An instance ID that is not a canonical UUID
// yields no token, and the caller gets the legacy prefix rather than one
// with an empty segment in it — `prism--foo-` would be a valid podman
// name and a permanently unattributable resource.
func TestResourceNamePrefixForOwner_FallsBackWithoutUsableIdentity(t *testing.T) {
	for _, id := range []string{"", "test-instance-id"} {
		got := ResourceNamePrefixForOwner(id, "foo")
		if want := ResourceNamePrefixForSession("foo"); got != want {
			t.Errorf("ResourceNamePrefixForOwner(%q, \"foo\") = %q, want the legacy prefix %q", id, got, want)
		}
	}
}

// TestResourceIsOwnedBy_LegacyNamesAreNeverOwned pins that identity
// says nothing about a resource created before this change. Those names
// carry no token, so attributing them is the legacy rule's job — and
// this function must not guess on its behalf.
func TestResourceIsOwnedBy_LegacyNamesAreNeverOwned(t *testing.T) {
	for _, name := range []string{
		ResourceNamePrefixForSession("foo") + "a1b2c3d4",
		ResourceNamePrefixForSession("foo") + "pgdata",
	} {
		if ResourceIsOwnedBy(name, iidA) {
			t.Errorf("ResourceIsOwnedBy(%q, %q) = true, want false (pre-identity name)", name, iidA)
		}
	}
}
