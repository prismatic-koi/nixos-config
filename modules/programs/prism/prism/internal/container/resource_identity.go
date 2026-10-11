package container

// Resource identity for the containers that `prism container` creates, and
// for the containers and volumes that the removed podman API proxy created.
//
// # Why a name prefix is not an identity
//
// Before this file existed, a session claimed a container or a volume by
// name-prefix equality: the proxy admitted a name that started with
// `prism-<sanitised-session>-`, and `prism cleanup` swept every name
// that started with the same string. Prefix equality is a HEURISTIC for
// identity, and two distinct live sessions collide under it in two
// independent ways (issue #2951):
//
//  1. FOLDING. sanitiseSessionName folds `@`, `/`, `.`, and `~` all to
//     `-`, so `repo@feat/x` and `repo@feat-x` produce the same prefix.
//  2. NESTING. One session name can be a strict prefix of another.
//     Session `foo` gets `prism-foo-`; live session `foo-bar` gets
//     `prism-foo-bar-`. The volume `prism-foo-bar-data` belongs to
//     `foo-bar` and also starts with `prism-foo-`.
//
// Both let one session claim another LIVE session's resources, in both
// directions: a sweep reaches a colliding sibling's volume, which holds
// data, and the guard that suppresses that sweep leaks the resources it
// spares.
//
// No separator rule closes either one. `isAllowedBindSource` closes the
// substring trap for host PATHS by requiring an exact match or a match
// on the entry plus `/`, but that technique does not transfer here:
// `prism-foo-bar-data` already carries the separator after `prism-foo-`,
// and it is a legitimate name for session `foo-bar`. The ambiguity is in
// the name space itself.
//
// # The identity
//
// A session incarnation's instance ID is its identity. It is a UUID, it
// is unique across every session on the host, and no two incarnations
// share one. This file records that identity ON the resource, by
// encoding it in the name prefix at a FIXED, left-anchored position:
//
//	prism-<instance token>-<sanitised session name>-<free-form suffix>
//
// `ResourceOwnerToken` reads the owner back out with an exact segment
// parse — the text between `prism-` and the next `-` — and never with a
// prefix test. That is what makes ownership a decision about identity
// rather than about a shared string. Position matters: the token has to
// be left-anchored, because the text AFTER the sanitised session name is
// free-form (the volume policy admits any user-chosen suffix inside the
// prefix), so a token placed later in the name cannot be located without
// a prefix test, and a prefix test is the thing being replaced.
//
// # The containment invariant
//
// The proxy still admits a create request by testing the name against
// the session's prefix. That is SOUND, and it is deliberately not the
// same statement as "ownership is prefix equality":
//
//	admitted ⊆ owned
//
// A name that starts with `prism-<token>-<decor>-` necessarily has owner
// token `<token>`, because `<token>` is 32 hex characters and carries no
// `-` for the segment parse to stop at early. So every name the proxy
// admits is owned by the admitting session, while the sweep decides what
// it owns by the exact parse. The proxy therefore stays a stdlib-only
// package with one string comparison, and the identity decision lives in
// exactly one place. TestResourceIdentity_AdmittedNamesAreOwned pins the
// invariant.
//
// # Sessions with no usable identity
//
// `InstanceTokenForID` returns "" for anything that is not a canonical
// UUID. Production instance IDs always are (`uuid.New().String()`), so
// this covers out-of-tree callers, ad-hoc bring-up, and tests. Such a
// session falls back to `ResourceNamePrefixForSession`, keeps the
// pre-identity behaviour, and is swept by the legacy half of
// cmd/cleanup_sweep.go. Failing closed here — refusing to run the proxy
// — would be the wrong direction: the agent does not choose its own
// instance ID, so an unusable one is a prism configuration fault and not
// an attack.

import (
	"regexp"
	"strings"
)

// ResourceNamePrefixRoot is the literal every prism-created container
// and volume name starts with. It is the anchor the cleanup sweep hands
// to podman's own name filter to narrow the listing before the exact
// ownership parse runs in Go.
const ResourceNamePrefixRoot = "prism-"

// canonicalUUIDPattern matches the canonical 8-4-4-4-12 hyphenated UUID
// form, which is what `uuid.New().String()` produces and therefore what
// every production instance ID looks like.
//
// The match is deliberately narrower than `uuid.Parse`, which also
// accepts the unhyphenated, URN, and brace-wrapped forms. Accepting
// those would make the ID → token map non-injective: two DIFFERENT
// instance-ID strings would produce the SAME token, and the sweep
// compares instance IDs as strings. A narrow match keeps one token per
// instance-ID string.
var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// instanceTokenPattern matches a well-formed instance token: exactly the
// 32 lowercase hex characters InstanceTokenForID produces.
//
// The fixed width and the absence of `-` are both load-bearing.
// ResourceOwnerToken reads the segment between `prism-` and the next
// `-`, so a token carrying a `-` would be cut in half, and a
// variable-width token would let a sanitised session name that happens
// to be hex be read as an owner.
var instanceTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// InstanceTokenForID returns the resource-name token for a session
// incarnation's instance ID: the canonical UUID with its hyphens
// removed, lowercased. It returns "" when instanceID is not a canonical
// UUID, which the callers read as "this session has no usable identity"
// and answer with the legacy name prefix.
//
// The encoding is a hyphen strip rather than a hash or a truncation on
// purpose. It is REVERSIBLE and it is legible: an operator who reads
// `prism-3f2a1b0c12345678...` off `podman volume ls` can re-insert the
// hyphens and look the session up directly, which a digest would not
// allow. It is also injective over its domain, so no collision argument
// is needed to call the token an identity.
func InstanceTokenForID(instanceID string) string {
	if !canonicalUUIDPattern.MatchString(instanceID) {
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(instanceID, "-", ""))
}

// ResourceNamePrefixForOwner returns the per-session name prefix of the
// resources that prism creates for the session incarnation identified by
// instanceID. The legacy cleanup sweep parses the same prefix out of the
// names of the containers and volumes that the removed podman API proxy
// created.
//
// The shape is `prism-<instance token>-<sanitised session name>-`.
//
// The sanitised session name is DECORATION. It carries no ownership
// meaning once the token is in the name, and it survives only so an
// operator reading `podman ps` can tell which session a container
// belongs to without a database lookup. That is why truncating or
// changing it is safe and changing the token is not.
//
// When instanceID yields no token the function falls back to
// ResourceNamePrefixForSession, so an out-of-tree caller or a test with
// a non-UUID instance ID keeps the pre-identity behaviour rather than
// receiving a prefix with an empty segment in it.
func ResourceNamePrefixForOwner(instanceID, sessionName string) string {
	token := InstanceTokenForID(instanceID)
	if token == "" {
		return ResourceNamePrefixForSession(sessionName)
	}
	return ResourceNamePrefixRoot + token + "-" + sanitiseSessionName(sessionName) + "-"
}

// ResourceOwnerToken returns the instance token encoded in a container
// or volume name, and reports whether the name carries one at all.
//
// The parse is EXACT: it takes the segment between the leading `prism-`
// and the next `-`, and accepts it only when it matches
// instanceTokenPattern. It is not a prefix test, and it cannot be turned
// into one — that is the whole point of the function.
//
// ok=false means the name is not identity-scoped. Two populations land
// there: a resource created before this change, which carries only the
// legacy `prism-<sanitised session>-` prefix, and any name on the host
// that is not a prism resource at all. Neither can be attributed to a
// session by identity, so the caller must decide what to do with them —
// cmd/cleanup_sweep.go answers with the legacy prefix rule plus the
// sibling guard.
func ResourceOwnerToken(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, ResourceNamePrefixRoot)
	if !ok {
		return "", false
	}
	token, _, found := strings.Cut(rest, "-")
	if !found {
		// No separator after the candidate token, so the name cannot
		// have been produced by a prefix (every prefix ends in `-`).
		return "", false
	}
	if !instanceTokenPattern.MatchString(token) {
		return "", false
	}
	return token, true
}

// ResourceIsOwnedBy reports whether the container or volume called name
// belongs to the session incarnation identified by instanceID.
//
// It is false for every name that carries no token, including every
// resource created before this change. A caller that must also reach
// those needs the legacy rule as a separate, explicit decision — this
// function never guesses on their behalf.
func ResourceIsOwnedBy(name, instanceID string) bool {
	token := InstanceTokenForID(instanceID)
	if token == "" {
		return false
	}
	got, ok := ResourceOwnerToken(name)
	return ok && got == token
}
