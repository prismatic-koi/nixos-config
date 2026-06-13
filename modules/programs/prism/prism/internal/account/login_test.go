package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withBrowserLauncher(t *testing.T, launcher browserLauncher) {
	t.Helper()
	old := defaultBrowserLauncher
	defaultBrowserLauncher = launcher
	t.Cleanup(func() { defaultBrowserLauncher = old })
}

type safeBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	writes chan struct{}
}

func newSafeBuffer() *safeBuffer {
	return &safeBuffer{writes: make(chan struct{}, 16)}
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	select {
	case b.writes <- struct{}{}:
	default:
	}
	return n, err
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForOutput(t *testing.T, b *safeBuffer, substr string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		out := b.String()
		if strings.Contains(out, substr) {
			return out
		}
		select {
		case <-b.writes:
		case <-deadline:
			t.Fatalf("timed out waiting for output containing %q; got %q", substr, out)
		}
	}
}

func extractAuthorizeURL(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, oauthAuthorizeURL+"?") {
			return line
		}
	}
	t.Fatalf("authorize URL not found in output %q", out)
	return ""
}

func parseAuthorizeURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize URL %q: %v", raw, err)
	}
	return u
}

func stateFromAuthorizeURL(t *testing.T, raw string) string {
	t.Helper()
	state := parseAuthorizeURL(t, raw).Query().Get("state")
	if state == "" {
		t.Fatalf("authorize URL missing state: %s", raw)
	}
	return state
}

func callbackURLForAuthorizeURL(t *testing.T, raw, code, state string) string {
	t.Helper()
	redirectURI := parseAuthorizeURL(t, raw).Query().Get("redirect_uri")
	if redirectURI == "" {
		t.Fatalf("authorize URL missing redirect_uri: %s", raw)
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		t.Fatalf("parse redirect_uri %q: %v", redirectURI, err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("redirect_uri host %q does not include a port: %v", u.Host, err)
	}
	return fmt.Sprintf("http://%s:%s/callback?code=%s&state=%s", callbackHost, port, url.QueryEscape(code), url.QueryEscape(state))
}

func accountBlob(t *testing.T, p Paths, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(p.AccountPath(name))
	if err != nil {
		t.Fatalf("read account %s: %v", name, err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse account %s: %v", name, err)
	}
	return got
}

func TestGeneratePKCE_FreshVerifierAndChallenge(t *testing.T) {
	first, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE #1: %v", err)
	}
	second, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE #2: %v", err)
	}
	if first.verifier == second.verifier {
		t.Fatalf("verifier reused across invocations: %q", first.verifier)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(first.verifier)
	if err != nil {
		t.Fatalf("verifier is not raw base64url: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("decoded verifier length = %d, want 32", len(decoded))
	}
	digest := sha256.Sum256([]byte(first.verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if first.challenge != wantChallenge {
		t.Fatalf("challenge = %q, want SHA-256 base64url %q", first.challenge, wantChallenge)
	}
}

func TestParseAuthInput_AcceptsAuthTSFormats(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{name: "full URL", input: "https://platform.claude.com/oauth/code/callback?code=abc&state=xyz"},
		{name: "code hash state", input: "abc#xyz"},
		{name: "raw query", input: "code=abc&state=xyz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseAuthInput(tc.input)
			if !ok {
				t.Fatalf("parseAuthInput(%q) failed", tc.input)
			}
			if got.code != "abc" || got.state != "xyz" {
				t.Fatalf("parseAuthInput(%q) = %+v, want code abc state xyz", tc.input, got)
			}
		})
	}
}

func TestLogin_BrowserCallbackWritesAccountFileAndTokenRequest(t *testing.T) {
	p := fixture(t)
	writeAuthJSON(t, p, map[string]any{"github-copilot": map[string]any{"token": "gh"}})

	authorizeCh := make(chan string, 1)
	withBrowserLauncher(t, func(raw string) error {
		authorizeCh <- raw
		return nil
	})

	var tokenForm url.Values
	var tokenHeaders http.Header
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("token method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("Content-Type = %q, want application/x-www-form-urlencoded", got)
		}
		if got := r.Header.Get("User-Agent"); got != "" {
			t.Fatalf("User-Agent = %q, want absent", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		tokenForm = r.PostForm
		tokenHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer tokenServer.Close()

	var stdout bytes.Buffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- Login(context.Background(), p, "work", LoginOptions{
			Port:     0,
			Stdin:    strings.NewReader(""),
			Stdout:   &stdout,
			TokenURL: tokenServer.URL,
			Timeout:  2 * time.Second,
			Now:      func() time.Time { return time.UnixMilli(1_000_000) },
		})
	}()

	var authorizeURL string
	select {
	case authorizeURL = <-authorizeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("browser launcher was not called")
	}

	authURL := parseAuthorizeURL(t, authorizeURL)
	q := authURL.Query()
	if authURL.String() == "" || authURL.Scheme+"://"+authURL.Host+authURL.Path != oauthAuthorizeURL {
		t.Fatalf("authorize URL = %s, want base %s", authorizeURL, oauthAuthorizeURL)
	}
	if q.Get("client_id") != oauthClientID {
		t.Fatalf("client_id = %q, want %q", q.Get("client_id"), oauthClientID)
	}
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type = %q, want code", q.Get("response_type"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatalf("authorize URL missing code_challenge or state: %s", authorizeURL)
	}

	state := q.Get("state")
	callbackResp, err := http.Get(callbackURLForAuthorizeURL(t, authorizeURL, "super-secret-code", state))
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	callbackBody, _ := io.ReadAll(callbackResp.Body)
	_ = callbackResp.Body.Close()
	if callbackResp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d body %q, want 200", callbackResp.StatusCode, callbackBody)
	}
	if !strings.Contains(strings.ToLower(string(callbackBody)), "you can close this window") {
		t.Fatalf("callback page did not include close-window text: %q", callbackBody)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("Login: %v", err)
	}

	if tokenForm.Get("grant_type") != "authorization_code" || tokenForm.Get("client_id") != oauthClientID {
		t.Fatalf("unexpected token form: %v", tokenForm)
	}
	if tokenForm.Get("code") != "super-secret-code" || tokenForm.Get("state") != state {
		t.Fatalf("token form did not include callback code/state")
	}
	redirectURI := q.Get("redirect_uri")
	if tokenForm.Get("redirect_uri") != redirectURI {
		t.Fatalf("token redirect_uri = %q, want authorize redirect_uri %q", tokenForm.Get("redirect_uri"), redirectURI)
	}
	verifier := tokenForm.Get("code_verifier")
	if verifier == "" {
		t.Fatal("token form missing code_verifier")
	}
	digest := sha256.Sum256([]byte(verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if q.Get("code_challenge") != wantChallenge {
		t.Fatalf("authorize challenge = %q, want SHA-256 of verifier %q", q.Get("code_challenge"), wantChallenge)
	}
	if tokenHeaders.Get("User-Agent") != "" {
		t.Fatalf("token request User-Agent = %q, want absent", tokenHeaders.Get("User-Agent"))
	}

	got := accountBlob(t, p, "work")
	if got["type"] != "oauth" || got["access"] != "new-access" || got["refresh"] != "new-refresh" {
		t.Fatalf("account blob mismatch: %v", got)
	}
	if got["expires"] != float64(4_300_000) {
		t.Fatalf("expires = %v, want 4300000", got["expires"])
	}
	info, err := os.Stat(p.AccountPath("work"))
	if err != nil {
		t.Fatalf("stat account file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("account file mode = %o, want 0600", perm)
	}
	dirInfo, err := os.Stat(p.Dir)
	if err != nil {
		t.Fatalf("stat account dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("account dir mode = %o, want 0700", perm)
	}

	out := stdout.String()
	if !strings.Contains(out, "account work saved. Run 'prism account use work' to activate.") {
		t.Fatalf("stdout missing saved message: %q", out)
	}
	for _, secret := range []string{"super-secret-code", verifier, "new-access", "new-refresh"} {
		if strings.Contains(out, secret) {
			t.Fatalf("stdout leaked secret %q in %q", secret, out)
		}
	}
}

func TestLoginUse_ActivatesSavedAccount(t *testing.T) {
	p := fixture(t)
	writeAuthJSON(t, p, map[string]any{
		"anthropic":      sampleAnthropic("old"),
		"github-copilot": map[string]any{"token": "gh"},
	})

	stdinReader, stdinWriter := io.Pipe()
	withBrowserLauncher(t, func(raw string) error {
		go func() {
			_, _ = fmt.Fprintf(stdinWriter, "manual-code#%s\n", stateFromAuthorizeURL(t, raw))
			_ = stdinWriter.Close()
		}()
		return nil
	})

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"active-access","refresh_token":"active-refresh","expires_in":600}`)
	}))
	defer tokenServer.Close()

	var stdout bytes.Buffer
	if err := Login(context.Background(), p, "work", LoginOptions{
		Use:      true,
		Port:     0,
		Stdin:    stdinReader,
		Stdout:   &stdout,
		TokenURL: tokenServer.URL,
		Timeout:  2 * time.Second,
	}); err != nil {
		t.Fatalf("Login --use: %v", err)
	}

	live := readAuthJSON(t, p)
	anthropic, ok := live["anthropic"].(map[string]any)
	if !ok {
		t.Fatalf("live auth missing anthropic object: %v", live)
	}
	if anthropic["access"] != "active-access" || anthropic["refresh"] != "active-refresh" {
		t.Fatalf("live auth not activated: %v", anthropic)
	}
	if _, ok := live["github-copilot"]; !ok {
		t.Fatalf("github-copilot sibling key was not preserved: %v", live)
	}
	cur, ok, err := Current(p)
	if err != nil || !ok || cur != "work" {
		t.Fatalf("Current = (%q, %v, %v), want work/true/nil", cur, ok, err)
	}
}

func TestLoginUse_ReloginActiveAccountKeepsSavedBlobFresh(t *testing.T) {
	p := fixture(t)
	writeAuthJSON(t, p, map[string]any{
		"anthropic":      sampleAnthropic("old-work"),
		"github-copilot": map[string]any{"token": "gh"},
	})
	if err := Init(p); err != nil {
		t.Fatalf("Init: %v", err)
	}
	oldWorkBlob, err := json.Marshal(sampleAnthropic("old-work"))
	if err != nil {
		t.Fatalf("marshal old work blob: %v", err)
	}
	if err := atomicWriteFile(p.AccountPath("work"), oldWorkBlob, fileMode); err != nil {
		t.Fatalf("write old work account: %v", err)
	}
	if err := atomicWriteFile(p.Current, []byte("work\n"), fileMode); err != nil {
		t.Fatalf("write current: %v", err)
	}

	stdinReader, stdinWriter := io.Pipe()
	withBrowserLauncher(t, func(raw string) error {
		go func() {
			_, _ = fmt.Fprintf(stdinWriter, "manual-code#%s\n", stateFromAuthorizeURL(t, raw))
			_ = stdinWriter.Close()
		}()
		return nil
	})

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new-work-access","refresh_token":"new-work-refresh","expires_in":600}`)
	}))
	defer tokenServer.Close()

	if err := Login(context.Background(), p, "work", LoginOptions{
		Use:      true,
		Port:     0,
		Stdin:    stdinReader,
		Stdout:   io.Discard,
		TokenURL: tokenServer.URL,
		Timeout:  2 * time.Second,
	}); err != nil {
		t.Fatalf("Login --use active account: %v", err)
	}

	live := readAuthJSON(t, p)
	anthropic, ok := live["anthropic"].(map[string]any)
	if !ok {
		t.Fatalf("live auth missing anthropic object: %v", live)
	}
	if anthropic["access"] != "new-work-access" || anthropic["refresh"] != "new-work-refresh" {
		t.Fatalf("live auth not refreshed: %v", anthropic)
	}
	saved := accountBlob(t, p, "work")
	if saved["access"] != "new-work-access" || saved["refresh"] != "new-work-refresh" {
		t.Fatalf("saved active account blob was not refreshed: %v", saved)
	}
	cur, ok, err := Current(p)
	if err != nil || !ok || cur != "work" {
		t.Fatalf("Current = (%q, %v, %v), want work/true/nil", cur, ok, err)
	}
}

func TestLoginPortZero_AuthorizeRedirectUsesBoundPort(t *testing.T) {
	p := fixture(t)
	var capturedAuthorizeURL string
	withBrowserLauncher(t, func(raw string) error {
		capturedAuthorizeURL = raw
		return errors.New("force manual fallback")
	})

	stdinReader, stdinWriter := io.Pipe()
	stdout := newSafeBuffer()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access","refresh_token":"refresh","expires_in":600}`)
	}))
	defer tokenServer.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Login(context.Background(), p, "work", LoginOptions{
			Port:     0,
			Stdin:    stdinReader,
			Stdout:   stdout,
			TokenURL: tokenServer.URL,
			Timeout:  2 * time.Second,
		})
	}()

	waitForOutput(t, stdout, oauthAuthorizeURL)
	manualURL := extractAuthorizeURL(t, stdout.String())
	_, _ = fmt.Fprintf(stdinWriter, "manual-code#%s\n", stateFromAuthorizeURL(t, manualURL))
	_ = stdinWriter.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("Login: %v", err)
	}

	redirectURI := parseAuthorizeURL(t, capturedAuthorizeURL).Query().Get("redirect_uri")
	if redirectURI == "" {
		t.Fatalf("captured authorize URL missing redirect_uri: %s", capturedAuthorizeURL)
	}
	if !strings.Contains(redirectURI, ":") || strings.Contains(redirectURI, fmt.Sprintf(":%d/", callbackPort)) {
		t.Fatalf("--port 0 redirect_uri = %q, want random bound port", redirectURI)
	}
}

func TestLogin_HeadlessSkipsBrowserAndPrintsManualAuthorizeURL(t *testing.T) {
	p := fixture(t)
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("SSH_CONNECTION", "client server")

	var launcherCalls int32
	withBrowserLauncher(t, func(raw string) error {
		atomic.AddInt32(&launcherCalls, 1)
		return nil
	})

	stdinReader, stdinWriter := io.Pipe()
	stdout := newSafeBuffer()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if got := r.PostForm.Get("redirect_uri"); got != oauthRedirectURI {
			t.Fatalf("manual fallback redirect_uri = %q, want %q", got, oauthRedirectURI)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"headless-access","refresh_token":"headless-refresh","expires_in":600}`)
	}))
	defer tokenServer.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Login(context.Background(), p, "headless", LoginOptions{
			Port:     0,
			Stdin:    stdinReader,
			Stdout:   stdout,
			TokenURL: tokenServer.URL,
			Timeout:  2 * time.Second,
		})
	}()

	out := waitForOutput(t, stdout, oauthAuthorizeURL)
	manualURL := extractAuthorizeURL(t, out)
	if got := parseAuthorizeURL(t, manualURL).Query().Get("redirect_uri"); got != oauthRedirectURI {
		t.Fatalf("printed manual redirect_uri = %q, want %q", got, oauthRedirectURI)
	}
	_, _ = fmt.Fprintf(stdinWriter, "manual-code#%s\n", stateFromAuthorizeURL(t, manualURL))
	_ = stdinWriter.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("Login headless: %v", err)
	}
	if atomic.LoadInt32(&launcherCalls) != 0 {
		t.Fatalf("browser launcher called in headless SSH mode")
	}
	got := accountBlob(t, p, "headless")
	if got["access"] != "headless-access" {
		t.Fatalf("account blob mismatch: %v", got)
	}
}

func TestLogin_BrowserLaunchFailureFallsBackToManualPaste(t *testing.T) {
	p := fixture(t)
	t.Setenv("DISPLAY", ":1")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("SSH_CONNECTION", "")

	withBrowserLauncher(t, func(raw string) error {
		return errors.New("xdg-open missing")
	})

	stdinReader, stdinWriter := io.Pipe()
	stdout := newSafeBuffer()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"fallback-access","refresh_token":"fallback-refresh","expires_in":600}`)
	}))
	defer tokenServer.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Login(context.Background(), p, "fallback", LoginOptions{
			Port:     0,
			Stdin:    stdinReader,
			Stdout:   stdout,
			TokenURL: tokenServer.URL,
			Timeout:  2 * time.Second,
		})
	}()

	out := waitForOutput(t, stdout, oauthAuthorizeURL)
	manualURL := extractAuthorizeURL(t, out)
	_, _ = fmt.Fprintf(stdinWriter, "manual-code#%s\n", stateFromAuthorizeURL(t, manualURL))
	_ = stdinWriter.Close()
	if err := <-errCh; err != nil {
		t.Fatalf("Login fallback: %v", err)
	}
	got := accountBlob(t, p, "fallback")
	if got["refresh"] != "fallback-refresh" {
		t.Fatalf("account blob mismatch: %v", got)
	}
}

func TestLogin_InvalidNamesRejectedBeforeInitOrNetwork(t *testing.T) {
	for _, name := range []string{"", "bad/name", `bad\\name`, ".", "..", "current", "work.prod", "work..prod"} {
		t.Run(name, func(t *testing.T) {
			p := fixture(t)
			var launcherCalls int32
			withBrowserLauncher(t, func(raw string) error {
				atomic.AddInt32(&launcherCalls, 1)
				return nil
			})
			err := Login(context.Background(), p, name, LoginOptions{
				Port:     0,
				Stdin:    strings.NewReader(""),
				Stdout:   io.Discard,
				TokenURL: "http://127.0.0.1:1/token",
				Timeout:  10 * time.Millisecond,
			})
			if err == nil {
				t.Fatalf("Login(%q): want error, got nil", name)
			}
			if atomic.LoadInt32(&launcherCalls) != 0 {
				t.Fatalf("browser launcher called for invalid name %q", name)
			}
			if _, statErr := os.Stat(p.Dir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Init ran for invalid name %q: stat accounts dir err %v", name, statErr)
			}
		})
	}
}

func TestLogin_OverwritesExistingAccount(t *testing.T) {
	p := fixture(t)
	if err := Init(p); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.WriteFile(p.AccountPath("work"), []byte(`{"type":"oauth","access":"old","refresh":"old","expires":1}`), 0o600); err != nil {
		t.Fatalf("write old account: %v", err)
	}

	stdinReader, stdinWriter := io.Pipe()
	withBrowserLauncher(t, func(raw string) error {
		go func() {
			_, _ = fmt.Fprintf(stdinWriter, "manual-code#%s\n", stateFromAuthorizeURL(t, raw))
			_ = stdinWriter.Close()
		}()
		return nil
	})
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new","refresh_token":"new-refresh","expires_in":600}`)
	}))
	defer tokenServer.Close()

	if err := Login(context.Background(), p, "work", LoginOptions{
		Port:     0,
		Stdin:    stdinReader,
		Stdout:   io.Discard,
		TokenURL: tokenServer.URL,
		Timeout:  2 * time.Second,
	}); err != nil {
		t.Fatalf("Login overwrite: %v", err)
	}
	got := accountBlob(t, p, "work")
	if got["access"] != "new" || got["refresh"] != "new-refresh" {
		t.Fatalf("account was not overwritten: %v", got)
	}
}

func TestLogin_StateMismatchCallbackReturns400AndDoesNotWriteFile(t *testing.T) {
	p := fixture(t)
	authorizeCh := make(chan string, 1)
	withBrowserLauncher(t, func(raw string) error {
		authorizeCh <- raw
		return nil
	})
	var tokenCalls int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenCalls, 1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer tokenServer.Close()

	var stdout bytes.Buffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- Login(context.Background(), p, "work", LoginOptions{
			Port:     0,
			Stdin:    strings.NewReader(""),
			Stdout:   &stdout,
			TokenURL: tokenServer.URL,
			Timeout:  2 * time.Second,
		})
	}()

	var authorizeURL string
	select {
	case authorizeURL = <-authorizeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("browser launcher was not called")
	}
	expectedState := stateFromAuthorizeURL(t, authorizeURL)
	callbackResp, err := http.Get(callbackURLForAuthorizeURL(t, authorizeURL, "leaked-code", "wrong-state"))
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	callbackBody, _ := io.ReadAll(callbackResp.Body)
	_ = callbackResp.Body.Close()
	if callbackResp.StatusCode != http.StatusBadRequest || !strings.Contains(string(callbackBody), "Invalid state") {
		t.Fatalf("callback mismatch response = %d %q, want 400 Invalid state", callbackResp.StatusCode, callbackBody)
	}

	err = <-errCh
	if !errors.Is(err, errOAuthStateMismatch) {
		t.Fatalf("Login error = %v, want OAuth state mismatch", err)
	}
	for _, leaked := range []string{expectedState, "wrong-state", "leaked-code"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("state mismatch error leaked %q: %v", leaked, err)
		}
	}
	if atomic.LoadInt32(&tokenCalls) != 0 {
		t.Fatalf("token endpoint called after state mismatch")
	}
	if _, statErr := os.Stat(p.AccountPath("work")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("account file written after state mismatch: %v", statErr)
	}
}

func TestLogin_PortAlreadyBoundReturnsClearError(t *testing.T) {
	p := fixture(t)
	listener, err := net.Listen("tcp", net.JoinHostPort(callbackHost, "0"))
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	var launcherCalls int32
	withBrowserLauncher(t, func(raw string) error {
		atomic.AddInt32(&launcherCalls, 1)
		return nil
	})

	err = Login(context.Background(), p, "work", LoginOptions{
		Port:     port,
		Stdin:    strings.NewReader(""),
		Stdout:   io.Discard,
		TokenURL: "http://127.0.0.1:1/token",
		Timeout:  10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Login with bound port: want error, got nil")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("port %d", port)) || !strings.Contains(err.Error(), "--port 0") {
		t.Fatalf("bound-port error = %v, want port number and --port 0 hint", err)
	}
	if atomic.LoadInt32(&launcherCalls) != 0 {
		t.Fatalf("browser launcher called after listen failure")
	}
	if _, statErr := os.Stat(p.AccountPath("work")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("account file written after listen failure: %v", statErr)
	}
}

func TestLogin_TimeoutClosesServerAndDoesNotWriteFile(t *testing.T) {
	p := fixture(t)
	var authorizeURL string
	withBrowserLauncher(t, func(raw string) error {
		authorizeURL = raw
		return nil
	})

	err := Login(context.Background(), p, "work", LoginOptions{
		Port:     0,
		Stdin:    strings.NewReader(""),
		Stdout:   io.Discard,
		TokenURL: "http://127.0.0.1:1/token",
		Timeout:  25 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Login timeout error = %v, want timeout", err)
	}
	if _, statErr := os.Stat(p.AccountPath("work")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("account file written after timeout: %v", statErr)
	}

	redirectURI := parseAuthorizeURL(t, authorizeURL).Query().Get("redirect_uri")
	u, err := url.Parse(redirectURI)
	if err != nil {
		t.Fatalf("parse redirect_uri: %v", err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split redirect host %q: %v", u.Host, err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(callbackHost, port))
	if err != nil {
		t.Fatalf("callback socket was not released after timeout: %v", err)
	}
	_ = listener.Close()
}

func TestTokenExchangeRetriesHonourRetryAfterAndShouldRetryFalse(t *testing.T) {
	t.Run("retries retryable statuses and caps Retry-After", func(t *testing.T) {
		var attempts int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempt := atomic.AddInt32(&attempts, 1)
			switch attempt {
			case 1:
				w.Header().Set("Retry-After", "45")
				w.WriteHeader(http.StatusTooManyRequests)
			case 2:
				w.WriteHeader(http.StatusBadGateway)
			case 3:
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"access_token":"access","refresh_token":"refresh","expires_in":60}`)
			default:
				t.Fatalf("unexpected extra token attempt %d", attempt)
			}
		}))
		defer server.Close()

		var sleeps []time.Duration
		creds, err := exchangeAuthorizationCode(context.Background(), exchangeOptions{
			client:      server.Client(),
			tokenURL:    server.URL,
			code:        "code",
			state:       "state",
			redirectURI: "http://localhost/callback",
			verifier:    "verifier",
			now:         func() time.Time { return time.UnixMilli(1_000) },
			retrySleep: func(ctx context.Context, d time.Duration) error {
				sleeps = append(sleeps, d)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("exchangeAuthorizationCode: %v", err)
		}
		if attempts != 3 {
			t.Fatalf("attempts = %d, want 3", attempts)
		}
		if len(sleeps) != 2 || sleeps[0] != 30*time.Second || sleeps[1] != 10*time.Second {
			t.Fatalf("sleeps = %v, want [30s 10s]", sleeps)
		}
		if creds.Expires != 1_000+60_000-tokenExpirySafety.Milliseconds() {
			t.Fatalf("expires = %d, want safety-adjusted expiry", creds.Expires)
		}
	})

	t.Run("x-should-retry false aborts immediately", func(t *testing.T) {
		var attempts int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.Header().Set("X-Should-Retry", "false")
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		var slept bool
		_, err := exchangeAuthorizationCode(context.Background(), exchangeOptions{
			client:      server.Client(),
			tokenURL:    server.URL,
			code:        "secret-code",
			state:       "state",
			redirectURI: "http://localhost/callback",
			verifier:    "verifier",
			now:         time.Now,
			retrySleep: func(ctx context.Context, d time.Duration) error {
				slept = true
				return nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
			t.Fatalf("error = %v, want final HTTP 503", err)
		}
		if strings.Contains(err.Error(), "secret-code") {
			t.Fatalf("token exchange error leaked authorization code: %v", err)
		}
		if attempts != 1 || slept {
			t.Fatalf("attempts=%d slept=%v, want one attempt and no sleep", attempts, slept)
		}
	})
}

func TestLogin_TokenFailureAfterRetriesDoesNotWriteFile(t *testing.T) {
	p := fixture(t)
	stdinReader, stdinWriter := io.Pipe()
	withBrowserLauncher(t, func(raw string) error {
		go func() {
			_, _ = fmt.Fprintf(stdinWriter, "secret-code#%s\n", stateFromAuthorizeURL(t, raw))
			_ = stdinWriter.Close()
		}()
		return nil
	})

	var attempts int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer tokenServer.Close()

	err := Login(context.Background(), p, "work", LoginOptions{
		Port:     0,
		Stdin:    stdinReader,
		Stdout:   io.Discard,
		TokenURL: tokenServer.URL,
		Timeout:  2 * time.Second,
		RetrySleep: func(ctx context.Context, d time.Duration) error {
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("Login token failure error = %v, want final HTTP 503", err)
	}
	if strings.Contains(err.Error(), "secret-code") {
		t.Fatalf("token failure error leaked authorization code: %v", err)
	}
	if attempts != maxTokenRetries+1 {
		t.Fatalf("attempts = %d, want %d", attempts, maxTokenRetries+1)
	}
	if _, statErr := os.Stat(p.AccountPath("work")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("account file written after token failure: %v", statErr)
	}
}

func TestLogin_FileLivesUnderAccountsDir(t *testing.T) {
	p := fixture(t)
	stdinReader, stdinWriter := io.Pipe()
	withBrowserLauncher(t, func(raw string) error {
		go func() {
			_, _ = fmt.Fprintf(stdinWriter, "manual-code#%s\n", stateFromAuthorizeURL(t, raw))
			_ = stdinWriter.Close()
		}()
		return nil
	})
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access","refresh_token":"refresh","expires_in":600}`)
	}))
	defer tokenServer.Close()

	if err := Login(context.Background(), p, "work", LoginOptions{
		Port:     0,
		Stdin:    stdinReader,
		Stdout:   io.Discard,
		TokenURL: tokenServer.URL,
		Timeout:  2 * time.Second,
	}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	wantDir := filepath.Clean(p.Dir)
	gotDir := filepath.Clean(filepath.Dir(p.AccountPath("work")))
	if gotDir != wantDir {
		t.Fatalf("account file dir = %s, want %s", gotDir, wantDir)
	}
	info, err := os.Stat(p.AccountPath("work"))
	if err != nil {
		t.Fatalf("stat account file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("account file mode = %o, want 0600", info.Mode().Perm())
	}
}
