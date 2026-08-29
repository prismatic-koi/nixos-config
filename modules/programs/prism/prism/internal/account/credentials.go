// Read-only access to a stored account's OAuth credentials.
//
// Background
// ----------
//
// `prism account usage` refreshes a missing or stale rate-limit snapshot with
// one live `/v1/messages` request. That request needs a bearer token.
//
// This file adds the two reads the refresh path needs. It adds no writer and
// no new on-disk format.
//
// Which file holds the LIVE token
// -------------------------------
//
// There are two copies of the Anthropic OAuth blob and they do NOT stay in
// step:
//
//	~/.pi/agent/auth.json          the LIVE blob. The pi extension rotates the
//	                               access token here on every refresh
//	                               (`anthropic-oauth/credentials.ts`,
//	                               writeCredentials). Read it with
//	                               ReadLiveCredentials.
//	~/.config/prism/accounts/…     a point-in-time COPY. Written only by Init,
//	                               Save, Use, and Login. Nothing rotates it.
//	                               Read it with ReadCredentials.
//
// An Anthropic access token lives 36000 seconds (credentials.ts), so the
// stored copy is expired about ten hours after the snapshot that produced it.
// A refresh path that reads the stored copy therefore works once and then
// reports "expired" forever, while pi holds a perfectly good token.
//
// A naive design reads the token from the accounts directory. That premise
// is wrong for the ACTIVE account and this file does not follow it: prefer the
// live blob, and fall back to the stored copy.
//
// Security
// --------
//
// The rest of this package holds to "no function ever returns or logs a token
// value". That rule cannot hold literally here — the caller needs the token —
// so the rule is narrowed instead:
//
//   - No error returned by this file carries the token, the file contents, or
//     any part of either. The JSON parse failure is reported as a fixed
//     string, NOT as the wrapped encoding/json error, because a type error
//     from encoding/json names the offending struct field and a future field
//     could carry secret material.
//   - Credentials implements fmt.Stringer and GoStringer with a redacted
//     form, so an accidental `%v`, `%s`, `%q`, or `%#v` of the struct prints
//     `account.Credentials{Access:"<redacted>", ...}` rather than the token.
//     Callers must still not print the Access field itself.
package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Sentinel errors for the credential read. Callers match with errors.Is and
// render their own user-facing message — for example, `prism account usage`
// turns ErrTokenExpired into "run `prism account login <name>`".
var (
	// ErrNoCredentials means accounts/<name>.json does not exist.
	ErrNoCredentials = errors.New("account: no stored credentials")
	// ErrNoAccessToken means the file exists and parses but carries no
	// non-empty "access" value.
	ErrNoAccessToken = errors.New("account: stored credentials carry no access token")
	// ErrTokenExpired means the stored "expires" timestamp is in the past.
	ErrTokenExpired = errors.New("account: stored access token has expired")
)

// Credentials is the subset of an account blob that the refresh path needs.
//
// ExpiresAt is the zero time when the blob carried no "expires" field. A zero
// ExpiresAt is treated as "unknown", not as "expired" — see Expired.
type Credentials struct {
	// Access is the OAuth bearer token. SECRET. Never print, log, or embed
	// it in an error.
	Access string
	// ExpiresAt is when Access stops being valid. `prism account login`
	// already subtracts a five-minute safety margin before it stores the
	// value, so a timestamp in the past means "expired or about to expire".
	ExpiresAt time.Time
}

// String renders Credentials with the token redacted so an accidental
// fmt.Sprintf("%v", creds) cannot leak it.
func (c Credentials) String() string {
	present := "absent"
	if c.Access != "" {
		present = "<redacted>"
	}
	return fmt.Sprintf("account.Credentials{Access:%s, ExpiresAt:%s}", present, c.ExpiresAt.UTC().Format(time.RFC3339))
}

// GoString redacts the token under the %#v verb too. Without it, %#v bypasses
// String and prints every field verbatim.
func (c Credentials) GoString() string { return c.String() }

// Expired reports whether the token is known to have expired as of now.
//
// A zero ExpiresAt returns false: an absent "expires" field means the store
// cannot tell, and refusing to try the request would be worse than letting
// the server answer with a 401.
func (c Credentials) Expired(now time.Time) bool {
	if c.ExpiresAt.IsZero() {
		return false
	}
	return !now.Before(c.ExpiresAt)
}

// ReadLiveCredentials reads the "anthropic" blob out of the live auth.json and
// returns its access token and expiry.
//
// This is the token pi itself is using RIGHT NOW. The pi extension rotates it
// in place, so it is the only copy that stays valid — see the package comment
// above for why the stored copy does not.
//
// The blob belongs to whichever account is active, because `prism account use`
// writes accounts/<name>.json into auth.json and updates accounts/current in
// the same operation. That also makes it the right token for a usage refresh:
// the passive capture path records whatever token pi used and files
// the result under accounts/current, so reading the live blob here attributes
// the refreshed numbers exactly as the passive path does.
//
// Errors:
//
//	ErrNoCredentials  auth.json is absent, or carries no "anthropic" key
//	ErrNoAccessToken  the blob parses but has no non-empty "access"
//	other             an I/O or parse failure
func ReadLiveCredentials(p Paths) (Credentials, error) {
	raw, err := readAnthropicBlob(p.AuthJSON)
	if err != nil {
		if errors.Is(err, errNoAnthropicKey) || errors.Is(err, os.ErrNotExist) {
			return Credentials{}, fmt.Errorf("%w in %s", ErrNoCredentials, p.AuthJSON)
		}
		// readAnthropicBlob's parse error names the path, never the contents.
		return Credentials{}, fmt.Errorf("read live credentials: %w", err)
	}
	return parseCredentialBlob(raw, p.AuthJSON)
}

// ReadCredentials reads accounts/<name>.json and returns its access token and
// expiry.
//
// This is the STORED copy, not the live one. Nothing rotates it, so a caller
// that needs a usable token must prefer ReadLiveCredentials and fall back to
// this only when auth.json holds nothing. See the package comment above.
//
// It never creates the accounts directory and never runs the first-run
// migration, so it is safe on a host that has never used `prism account`.
//
// Errors:
//
//	ErrNoCredentials  the file does not exist
//	ErrNoAccessToken  the file parses but has no non-empty "access"
//	other             an invalid name, or an I/O or parse failure
//
// Expiry is NOT enforced here — the caller decides what an expired token
// means. Use Credentials.Expired.
func ReadCredentials(p Paths, name string) (Credentials, error) {
	if err := validName(name); err != nil {
		return Credentials{}, err
	}
	path := p.AccountPath(name)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, fmt.Errorf("account %s: %w at %s", name, ErrNoCredentials, path)
		}
		// os.ReadFile's error carries the path and the syscall errno only —
		// never file contents.
		return Credentials{}, fmt.Errorf("account %s: read credentials: %w", name, err)
	}
	creds, err := parseCredentialBlob(raw, path)
	if err != nil {
		return Credentials{}, fmt.Errorf("account %s: %w", name, err)
	}
	return creds, nil
}

// parseCredentialBlob decodes one Anthropic OAuth blob. path is used for the
// error text only; the blob itself is never echoed.
func parseCredentialBlob(raw []byte, path string) (Credentials, error) {
	var blob struct {
		Access string `json:"access"`
		// Milliseconds since the unix epoch — the unit `prism account login`
		// writes (see login.go: now.UnixMilli() + expires_in*1000 - safety)
		// and the unit pi's auth.json uses.
		Expires int64 `json:"expires"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		// Deliberately NOT %w on the json error: encoding/json type errors
		// name the struct field they choked on, and a future field could be
		// secret-adjacent. A fixed string keeps this path leak-proof by
		// construction.
		return Credentials{}, fmt.Errorf("%s is not valid credential JSON", path)
	}
	if blob.Access == "" {
		return Credentials{}, ErrNoAccessToken
	}

	creds := Credentials{Access: blob.Access}
	if blob.Expires > 0 {
		creds.ExpiresAt = time.UnixMilli(blob.Expires)
	}
	return creds, nil
}
