// Read-only access to a stored account's OAuth credentials (issue #2541,
// parent #2537).
//
// Background
// ----------
//
// `prism account usage` refreshes a missing or stale rate-limit snapshot with
// one live `/v1/messages` request. That request needs a bearer token, and the
// token lives in the account blob this package already owns
// (`~/.config/prism/accounts/<name>.json`).
//
// This file adds the one read the refresh path needs. It adds no writer and
// no new on-disk format.
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

// ReadCredentials reads accounts/<name>.json and returns its access token and
// expiry.
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
		return Credentials{}, fmt.Errorf("account %s: %s is not valid credential JSON", name, path)
	}
	if blob.Access == "" {
		return Credentials{}, fmt.Errorf("account %s: %w", name, ErrNoAccessToken)
	}

	creds := Credentials{Access: blob.Access}
	if blob.Expires > 0 {
		creds.ExpiresAt = time.UnixMilli(blob.Expires)
	}
	return creds, nil
}
