package account

// Tests for ReadCredentials.
//
// The security assertions carry the most weight here: the refresh path is the
// first and only caller in this package that handles a token value in the
// clear, so the guards that keep it out of errors and out of formatted output
// need to be pinned by tests rather than by convention.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secretToken = "sk-ant-oat01-DO-NOT-LEAK-THIS"

// credentialsFixture points $XDG_CONFIG_HOME and $PI_AUTH_JSON at a tempdir
// and creates the accounts directory.
func credentialsFixture(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("PI_AUTH_JSON", filepath.Join(root, "auth.json"))
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	return p
}

func writeAccountBlob(t *testing.T, p Paths, name, body string) {
	t.Helper()
	if err := os.WriteFile(p.AccountPath(name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s.json: %v", name, err)
	}
}

func TestReadCredentials_ReturnsAccessAndExpiry(t *testing.T) {
	p := credentialsFixture(t)
	expires := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	writeAccountBlob(t, p, "work", fmt.Sprintf(
		`{"type":"oauth","access":%q,"refresh":"r","expires":%d}`,
		secretToken, expires.UnixMilli()))

	creds, err := ReadCredentials(p, "work")
	if err != nil {
		t.Fatalf("ReadCredentials: %v", err)
	}
	if creds.Access != secretToken {
		t.Errorf("Access = %q, want the stored token", creds.Access)
	}
	if !creds.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %s, want %s", creds.ExpiresAt.UTC(), expires)
	}
}

func TestReadCredentials_MissingFile(t *testing.T) {
	p := credentialsFixture(t)

	_, err := ReadCredentials(p, "absent")
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
}

func TestReadCredentials_NoAccessField(t *testing.T) {
	p := credentialsFixture(t)
	writeAccountBlob(t, p, "work", `{"type":"oauth","refresh":"r","expires":1}`)

	_, err := ReadCredentials(p, "work")
	if !errors.Is(err, ErrNoAccessToken) {
		t.Fatalf("err = %v, want ErrNoAccessToken", err)
	}
}

func TestReadCredentials_AbsentExpiryIsZeroAndNotExpired(t *testing.T) {
	p := credentialsFixture(t)
	writeAccountBlob(t, p, "work", fmt.Sprintf(`{"type":"oauth","access":%q}`, secretToken))

	creds, err := ReadCredentials(p, "work")
	if err != nil {
		t.Fatalf("ReadCredentials: %v", err)
	}
	if !creds.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %s, want the zero time", creds.ExpiresAt)
	}
	// An unknown expiry must not be treated as expired: refusing to try the
	// request would be worse than letting the server answer.
	if creds.Expired(time.Now()) {
		t.Error("an absent expiry must not count as expired")
	}
}

func TestCredentials_Expired(t *testing.T) {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	creds := Credentials{Access: "t", ExpiresAt: base}

	if creds.Expired(base.Add(-time.Second)) {
		t.Error("a token one second before its expiry must not be expired")
	}
	if !creds.Expired(base) {
		t.Error("a token at its expiry instant must count as expired")
	}
	if !creds.Expired(base.Add(time.Hour)) {
		t.Error("a token an hour past its expiry must be expired")
	}
}

// TestReadCredentials_MalformedJSONErrorCarriesNoFileContents pins the
// deliberate choice not to wrap the encoding/json error: a type error names
// the struct field it choked on, and this string is printed to a terminal.
func TestReadCredentials_MalformedJSONErrorCarriesNoFileContents(t *testing.T) {
	p := credentialsFixture(t)
	writeAccountBlob(t, p, "work", `{"access": `+secretToken)

	_, err := ReadCredentials(p, "work")
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error leaks file contents: %q", err.Error())
	}
}

// TestCredentials_FormattingRedactsTheToken covers the security AC from the
// direction that actually bites: an accidental %v / %s / %q / %#v of the
// struct somewhere downstream.
func TestCredentials_FormattingRedactsTheToken(t *testing.T) {
	creds := Credentials{Access: secretToken, ExpiresAt: time.Unix(0, 0)}

	for _, verb := range []string{"%v", "%s", "%q", "%#v", "%+v"} {
		rendered := fmt.Sprintf(verb, creds)
		if strings.Contains(rendered, secretToken) {
			t.Errorf("fmt.Sprintf(%q, creds) leaks the token: %s", verb, rendered)
		}
		if !strings.Contains(rendered, "redacted") {
			t.Errorf("fmt.Sprintf(%q, creds) = %s, want a redaction marker", verb, rendered)
		}
	}
}

func TestCredentials_FormattingMarksAnAbsentToken(t *testing.T) {
	rendered := fmt.Sprintf("%v", Credentials{})
	if !strings.Contains(rendered, "absent") {
		t.Errorf("rendered = %s, want an absent marker for an empty token", rendered)
	}
}

// TestReadCredentials_RejectsAPathTraversingName is defence in depth: the
// name comes from accounts/current, which a hand edit can corrupt.
func TestReadCredentials_RejectsAPathTraversingName(t *testing.T) {
	p := credentialsFixture(t)

	for _, name := range []string{"../escape", "a/b", "..", ""} {
		if _, err := ReadCredentials(p, name); err == nil {
			t.Errorf("ReadCredentials(%q) succeeded; want a rejection", name)
		}
	}
}

// ── ReadLiveCredentials ─────────────────────────────────────────────────────

func writeLiveAuthJSON(t *testing.T, p Paths, anthropicBlob string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.AuthJSON), 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	body := `{"github-copilot":{"type":"oauth","access":"other"},"anthropic":` + anthropicBlob + `}`
	if err := os.WriteFile(p.AuthJSON, []byte(body), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

func TestReadLiveCredentials_ReadsTheAnthropicBlob(t *testing.T) {
	p := credentialsFixture(t)
	expires := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	writeLiveAuthJSON(t, p, fmt.Sprintf(
		`{"type":"oauth","access":%q,"refresh":"r","expires":%d}`,
		secretToken, expires.UnixMilli()))

	creds, err := ReadLiveCredentials(p)
	if err != nil {
		t.Fatalf("ReadLiveCredentials: %v", err)
	}
	if creds.Access != secretToken {
		t.Errorf("Access = %q, want the live token", creds.Access)
	}
	if !creds.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %s, want %s", creds.ExpiresAt.UTC(), expires)
	}
}

// The live blob and the stored copy diverge in practice: the pi extension
// rotates the live one and never writes back to the accounts directory. The
// two readers must therefore return different values, not agree by accident.
func TestReadLiveCredentials_IsIndependentOfTheStoredCopy(t *testing.T) {
	p := credentialsFixture(t)
	writeAccountBlob(t, p, "work", `{"type":"oauth","access":"stored-and-stale","expires":1}`)
	writeLiveAuthJSON(t, p, fmt.Sprintf(`{"type":"oauth","access":%q,"expires":99999999999999}`, secretToken))

	live, err := ReadLiveCredentials(p)
	if err != nil {
		t.Fatalf("ReadLiveCredentials: %v", err)
	}
	stored, err := ReadCredentials(p, "work")
	if err != nil {
		t.Fatalf("ReadCredentials: %v", err)
	}
	if live.Access == stored.Access {
		t.Fatal("the two readers returned the same token; the fixture is not exercising the divergence")
	}
	if live.Expired(time.Now()) {
		t.Error("the live token must not read as expired")
	}
	if !stored.Expired(time.Now()) {
		t.Error("the stored copy must read as expired; that is the bug the live reader avoids")
	}
}

func TestReadLiveCredentials_MissingFileOrKey(t *testing.T) {
	t.Run("auth.json absent", func(t *testing.T) {
		p := credentialsFixture(t)
		if _, err := ReadLiveCredentials(p); !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("err = %v, want ErrNoCredentials", err)
		}
	})

	t.Run("no anthropic key", func(t *testing.T) {
		p := credentialsFixture(t)
		if err := os.MkdirAll(filepath.Dir(p.AuthJSON), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p.AuthJSON, []byte(`{"github-copilot":{}}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := ReadLiveCredentials(p); !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("err = %v, want ErrNoCredentials", err)
		}
	})

	t.Run("anthropic key without access", func(t *testing.T) {
		p := credentialsFixture(t)
		writeLiveAuthJSON(t, p, `{"type":"oauth","refresh":"r"}`)
		if _, err := ReadLiveCredentials(p); !errors.Is(err, ErrNoAccessToken) {
			t.Fatalf("err = %v, want ErrNoAccessToken", err)
		}
	})
}

func TestReadLiveCredentials_MalformedAuthJSONCarriesNoFileContents(t *testing.T) {
	p := credentialsFixture(t)
	if err := os.MkdirAll(filepath.Dir(p.AuthJSON), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p.AuthJSON, []byte(`{"anthropic": `+secretToken), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := ReadLiveCredentials(p)
	if err == nil {
		t.Fatal("expected an error for malformed auth.json")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error leaks file contents: %q", err.Error())
	}
}
