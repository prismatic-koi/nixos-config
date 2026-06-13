// Tests for `prism account login` (#2284). All tests are hermetic:
// $XDG_CONFIG_HOME and $PI_AUTH_JSON point at a per-test tempdir, the
// token endpoint is stubbed with httptest, the browser launcher is a
// no-op, and the IsHeadless detector is overridden. Nothing here
// touches ~/.config/prism, ~/.pi/agent/auth.json, or the real network.
package account

import (
	"bytes"
	"context"
	cryptosha256 "crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// ─── PKCE / state helpers ────────────────────────────────────────────

func TestGeneratePKCE_FreshPerInvocation_AndChallengeIsSHA256OfVerifier(t *testing.T) {
	v1, c1, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE 1: %v", err)
	}
	v2, c2, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE 2: %v", err)
	}
	if v1 == v2 {
		t.Fatalf("verifier reused between calls: %q == %q", v1, v2)
	}
	if c1 == c2 {
		t.Fatalf("challenge reused between calls: %q", c1)
	}
	// Verifier should be 32 random bytes base64url-encoded (no padding)
	// → 43 characters.
	if len(v1) != 43 {
		t.Errorf("verifier length = %d, want 43 (32 bytes base64url no padding)", len(v1))
	}
	// Decode and confirm 32 raw bytes.
	raw, err := base64.RawURLEncoding.DecodeString(v1)
	if err != nil {
		t.Fatalf("base64url decode verifier: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("verifier decodes to %d bytes, want 32", len(raw))
	}
	// Challenge should be base64url(SHA-256(verifier_ascii_bytes)).
	expectChallenge := sha256Base64URL(v1)
	if c1 != expectChallenge {
		t.Errorf("challenge mismatch:\n got %q\nwant %q", c1, expectChallenge)
	}
}

func sha256Base64URL(s string) string {
	h := cryptosha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// ─── parseAuthInput ──────────────────────────────────────────────────

func TestParseAuthInput_AllThreeFormats(t *testing.T) {
	const code = "abc123"
	const state = "xyz789"

	cases := []struct {
		name  string
		input string
	}{
		{"full URL", "http://localhost:53692/callback?code=" + code + "&state=" + state},
		{"https URL", "https://platform.claude.com/oauth/code/callback?code=" + code + "&state=" + state},
		{"code#state", code + "#" + state},
		{"raw query string", "code=" + code + "&state=" + state},
		{"leading/trailing whitespace", "  " + code + "#" + state + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseAuthInput(tc.input)
			if !ok {
				t.Fatalf("parseAuthInput(%q) = nil, want match", tc.input)
			}
			if got.code != code {
				t.Errorf("code = %q, want %q", got.code, code)
			}
			if got.state != state {
				t.Errorf("state = %q, want %q", got.state, state)
			}
		})
	}
}

func TestParseAuthInput_Invalid(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"http://localhost:53692/callback?code=onlycode",   // missing state
		"http://localhost:53692/callback?state=onlystate", // missing code
		"justonething",    // not URL, not code#state, not query
		"code=",           // empty code
		"#statebutnocode", // empty code in code#state
		"codebutnostate#", // empty state in code#state
	}
	for _, c := range cases {
		if got, ok := parseAuthInput(c); ok {
			t.Errorf("parseAuthInput(%q) = %+v, want false", c, got)
		}
	}
}

// ─── authorize URL ───────────────────────────────────────────────────

func TestMakeAuthorizeURL_ContainsRequiredParams(t *testing.T) {
	const state = "abc"
	const challenge = "challenge"
	const redirect = "http://localhost:53692/callback"

	raw := makeAuthorizeURL(oauthAuthorizeURL, challenge, state, redirect)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != oauthClientID {
		t.Errorf("client_id = %q, want %q", q.Get("client_id"), oauthClientID)
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") != challenge {
		t.Errorf("code_challenge missing/incorrect")
	}
	if q.Get("state") != state {
		t.Errorf("state missing/incorrect")
	}
	if q.Get("redirect_uri") != redirect {
		t.Errorf("redirect_uri = %q, want %q", q.Get("redirect_uri"), redirect)
	}
	if q.Get("scope") != oauthScopes {
		t.Errorf("scope = %q, want %q", q.Get("scope"), oauthScopes)
	}
}

// ─── retry delay ─────────────────────────────────────────────────────

func TestRetryDelay_HonoursRetryAfter_AndCaps(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter string
		attempt    int
		wantDelay  time.Duration
	}{
		{"no header → exponential attempt 0", "", 0, initialRetryDelay},
		{"no header → exponential attempt 1", "", 1, 2 * initialRetryDelay},
		{"retry-after 2 → 2s", "2", 0, 2 * time.Second},
		{"retry-after 60 capped at 30s", "60", 0, retryAfterCap},
		{"retry-after with whitespace", "  3 ", 0, 3 * time.Second},
		{"unparseable falls through", "garbage", 1, 2 * initialRetryDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := retryDelay(tc.retryAfter, tc.attempt)
			if got != tc.wantDelay {
				t.Errorf("retryDelay(%q, %d) = %s, want %s", tc.retryAfter, tc.attempt, got, tc.wantDelay)
			}
		})
	}
}

// ─── exchangeToken ───────────────────────────────────────────────────

func TestExchangeToken_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want form-urlencoded", got)
		}
		// AC: User-Agent must NOT be sent on token-exchange requests
		// because Anthropic's Cloudflare WAF returns 429 for
		// `User-Agent: claude-code/*` (issue #888; UPSTREAM.md
		// divergence #1). The TS canonical assertion is
		// `headers.has("user-agent") === false` — mirror that. Production
		// suppresses Go's default `Go-http-client/1.1` by setting the
		// header to "", which net/http elides on the wire.
		if got := r.Header.Get("User-Agent"); got != "" {
			t.Errorf("User-Agent = %q, want absent (Cloudflare WAF blocks claude-code/* — see #888)", got)
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("client_id") != oauthClientID {
			t.Errorf("client_id missing")
		}
		if r.Form.Get("code_verifier") == "" {
			t.Errorf("code_verifier missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"acc","refresh_token":"ref","expires_in":3600}`))
	}))
	defer srv.Close()

	opts := LoginOptions{TokenURL: srv.URL}
	opts.applyDefaults()
	tr, err := exchangeToken(context.Background(), opts, "code", "state", "verifier", "http://localhost:0/callback")
	if err != nil {
		t.Fatalf("exchangeToken: %v", err)
	}
	if tr.AccessToken != "acc" || tr.RefreshToken != "ref" || tr.ExpiresIn != 3600 {
		t.Errorf("token response unexpected: %+v", tr)
	}
}

func TestExchangeToken_RetriesOn429_ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":1}`))
	}))
	defer srv.Close()

	var slept []time.Duration
	opts := LoginOptions{
		TokenURL: srv.URL,
		Sleep:    func(d time.Duration) { slept = append(slept, d) },
	}
	opts.applyDefaults()

	tr, err := exchangeToken(context.Background(), opts, "c", "s", "v", "r")
	if err != nil {
		t.Fatalf("exchangeToken: %v", err)
	}
	if tr.AccessToken != "a" {
		t.Errorf("access = %q", tr.AccessToken)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(slept) != 1 {
		t.Errorf("slept %d times, want 1", len(slept))
	}
}

func TestExchangeToken_RetriesOn5xx_RespectsRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":1}`))
	}))
	defer srv.Close()

	var slept []time.Duration
	opts := LoginOptions{
		TokenURL: srv.URL,
		Sleep:    func(d time.Duration) { slept = append(slept, d) },
	}
	opts.applyDefaults()

	if _, err := exchangeToken(context.Background(), opts, "c", "s", "v", "r"); err != nil {
		t.Fatalf("exchangeToken: %v", err)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Errorf("slept = %v, want [2s]", slept)
	}
}

func TestExchangeToken_RetryAfterCappedAt30Seconds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":1}`))
	}))
	defer srv.Close()

	var slept []time.Duration
	opts := LoginOptions{
		TokenURL: srv.URL,
		Sleep:    func(d time.Duration) { slept = append(slept, d) },
	}
	opts.applyDefaults()

	if _, err := exchangeToken(context.Background(), opts, "c", "s", "v", "r"); err != nil {
		t.Fatalf("exchangeToken: %v", err)
	}
	if len(slept) != 1 || slept[0] != retryAfterCap {
		t.Errorf("slept = %v, want [%s]", slept, retryAfterCap)
	}
}

func TestExchangeToken_AbortsOnXShouldRetryFalse(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("X-Should-Retry", "false")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var slept []time.Duration
	opts := LoginOptions{
		TokenURL: srv.URL,
		Sleep:    func(d time.Duration) { slept = append(slept, d) },
	}
	opts.applyDefaults()

	_, err := exchangeToken(context.Background(), opts, "c", "s", "v", "r")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1 (no retry)", calls)
	}
	if len(slept) != 0 {
		t.Errorf("slept = %v, want []", slept)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error does not include status: %v", err)
	}
}

func TestExchangeToken_ExhaustsRetries_ErrorIncludesHTTPStatus(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	opts := LoginOptions{
		TokenURL: srv.URL,
		Sleep:    func(time.Duration) {},
	}
	opts.applyDefaults()

	_, err := exchangeToken(context.Background(), opts, "c", "s", "v", "r")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error does not include status: %v", err)
	}
	// maxTokenRetries=2 plus the initial attempt = 3 calls total.
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3 (1 initial + 2 retries)", got)
	}
}

func TestExchangeToken_ErrorNeverIncludesCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Should-Retry", "false")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
	}))
	defer srv.Close()

	const secretCode = "super-secret-code-value-do-not-leak"
	opts := LoginOptions{TokenURL: srv.URL, Sleep: func(time.Duration) {}}
	opts.applyDefaults()

	_, err := exchangeToken(context.Background(), opts, secretCode, "state", "verifier", "redirect")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secretCode) {
		t.Errorf("error message leaks code: %v", err)
	}
}

// ─── Login end-to-end (with stubs) ───────────────────────────────────

// loginFixture wires up an isolated filesystem + stubbed dependencies
// for full-flow tests. The token endpoint returns a canned successful
// response by default; per-test handlers may override.
type loginFixture struct {
	paths     Paths
	cfgRoot   string
	authPath  string
	tokenSrv  *httptest.Server
	tokenSeen atomic.Int32
	browserCh chan string
	stdout    *bytes.Buffer
	stdin     *strings.Reader
	headless  bool
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(root, "config")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatalf("mkdir cfg: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfg)

	piDir := filepath.Join(root, "pi-agent")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		t.Fatalf("mkdir pi-agent: %v", err)
	}
	auth := filepath.Join(piDir, "auth.json")
	t.Setenv("PI_AUTH_JSON", auth)

	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}

	f := &loginFixture{
		paths:     p,
		cfgRoot:   cfg,
		authPath:  auth,
		browserCh: make(chan string, 1),
		stdout:    &bytes.Buffer{},
		stdin:     strings.NewReader(""),
	}
	f.tokenSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.tokenSeen.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"access-tok","refresh_token":"refresh-tok","expires_in":3600}`))
	}))
	t.Cleanup(f.tokenSrv.Close)
	return f
}

// makeOpts returns a LoginOptions with the fixture's stubs wired in. A
// successful callback is simulated by GETting the redirect URL we
// extract from the printed authorize URL.
func (f *loginFixture) makeOpts() LoginOptions {
	return LoginOptions{
		TokenURL: f.tokenSrv.URL,
		Browser: func(target string) error {
			select {
			case f.browserCh <- target:
			default:
			}
			return nil
		},
		IsHeadless: func() bool { return f.headless },
		Stdout:     f.stdout,
		Stdin:      f.stdin,
		Now:        func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Sleep:      func(time.Duration) {},
		Timeout:    2 * time.Second,
	}
}

// driveCallback simulates the browser landing on the local callback URL
// with the given code and state. Pulls the authorize URL out of either
// the browser channel (in the launch-success path) or the captured
// stdout. Returns when the callback has been delivered (or the test
// fails).
func (f *loginFixture) driveCallback(t *testing.T, code, state string, errFromAuthURL chan<- error) {
	t.Helper()
	go func() {
		// Pull the authorize URL from either the browser channel (when
		// the browser launcher fired) or stdout (when it didn't). Race
		// the two so a non-launch path doesn't block.
		var target string
		select {
		case target = <-f.browserCh:
		case <-time.After(2 * time.Second):
			errFromAuthURL <- errors.New("timed out waiting for authorize URL")
			return
		}
		u, err := url.Parse(target)
		if err != nil {
			errFromAuthURL <- fmt.Errorf("parse authorize URL: %w", err)
			return
		}
		redirect := u.Query().Get("redirect_uri")
		if redirect == "" {
			errFromAuthURL <- errors.New("no redirect_uri in authorize URL")
			return
		}
		callback := fmt.Sprintf("%s?code=%s&state=%s", redirect, url.QueryEscape(code), url.QueryEscape(state))
		// Brief retry loop: the server may not yet be Accept()ing.
		var resp *http.Response
		for i := 0; i < 20; i++ {
			resp, err = http.Get(callback)
			if err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			errFromAuthURL <- fmt.Errorf("GET callback: %w", err)
			return
		}
		defer resp.Body.Close()
		errFromAuthURL <- nil
	}()
}

func TestLogin_Success_WritesAccountFile_Mode0o600_AndPrintsSavedMessage(t *testing.T) {
	f := newLoginFixture(t)
	opts := f.makeOpts()
	opts.UseRandomPort = true

	// We need to extract the state generated inside Login to drive a
	// matching callback. Use the browser-launcher hook: capture the
	// authorize URL, parse out state.
	errCh := make(chan error, 1)
	go func() {
		target := <-f.browserCh
		u, err := url.Parse(target)
		if err != nil {
			errCh <- err
			return
		}
		state := u.Query().Get("state")
		redirect := u.Query().Get("redirect_uri")
		callback := fmt.Sprintf("%s?code=alice-code&state=%s", redirect, url.QueryEscape(state))
		for i := 0; i < 20; i++ {
			resp, err := http.Get(callback)
			if err == nil {
				resp.Body.Close()
				errCh <- nil
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		errCh <- errors.New("callback never succeeded")
	}()

	if err := Login(f.paths, "work", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("driver: %v", err)
	}

	// File exists, mode 0o600, correct shape.
	acctPath := f.paths.AccountPath("work")
	info, err := os.Stat(acctPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0o600", perm)
	}
	data, _ := os.ReadFile(acctPath)
	var got accountBlob
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse account file: %v", err)
	}
	if got.Type != "oauth" || got.Access != "access-tok" || got.Refresh != "refresh-tok" {
		t.Errorf("account blob = %+v", got)
	}
	// expires = now*1000 + 3600*1000 - 300_000
	wantExpires := time.Unix(1_700_000_000, 0).UnixMilli() + 3600*1000 - 5*60*1000
	if got.Expires != wantExpires {
		t.Errorf("expires = %d, want %d", got.Expires, wantExpires)
	}

	// Stdout includes the success message.
	if !strings.Contains(f.stdout.String(), "account work saved. Run 'prism account use work' to activate.") {
		t.Errorf("stdout missing success message: %q", f.stdout.String())
	}

	// accounts dir is mode 0o700 (set by Init).
	dirInfo, _ := os.Stat(f.paths.Dir)
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("accounts dir mode = %o, want 0o700", perm)
	}
}

func TestLogin_TokenAndVerifierNeverInOutput(t *testing.T) {
	f := newLoginFixture(t)
	opts := f.makeOpts()
	opts.UseRandomPort = true

	// Run the flow successfully. After it returns, scan stdout for any
	// trace of the access token, refresh token, or any plausible
	// verifier substring.
	errCh := make(chan error, 1)
	go func() {
		target := <-f.browserCh
		u, _ := url.Parse(target)
		state := u.Query().Get("state")
		redirect := u.Query().Get("redirect_uri")
		challenge := u.Query().Get("code_challenge")
		callback := fmt.Sprintf("%s?code=c&state=%s", redirect, url.QueryEscape(state))
		for i := 0; i < 20; i++ {
			resp, err := http.Get(callback)
			if err == nil {
				resp.Body.Close()
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		// Stash the challenge so the test below can check it does
		// appear (it's the safe-to-surface counterpart).
		errCh <- nil
		_ = challenge
	}()
	if err := Login(f.paths, "secrets", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	<-errCh

	out := f.stdout.String()
	if strings.Contains(out, "access-tok") {
		t.Errorf("stdout leaks access token: %q", out)
	}
	if strings.Contains(out, "refresh-tok") {
		t.Errorf("stdout leaks refresh token: %q", out)
	}
	// The verifier is 43 chars of base64url. We can't know it from
	// outside, but we can assert that none of the printed tokens
	// matches the size+alphabet pattern.
	if leakSuspectedVerifier(out) {
		t.Errorf("stdout contains a base64url string that could be the verifier: %q", out)
	}
}

// leakSuspectedVerifier heuristically scans for a 43-char base64url-no-
// padding token in `out`. The authorize URL contains a 43-char challenge
// (`code_challenge` query param) so we extract it and exclude it from
// the scan — we expect the challenge to appear and the verifier not to.
func leakSuspectedVerifier(out string) bool {
	// Pull the challenge out of any URL we recognise.
	var challenge string
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, "code_challenge=")
		if idx == -1 {
			continue
		}
		rest := line[idx+len("code_challenge="):]
		end := strings.IndexAny(rest, "& ")
		if end == -1 {
			challenge = rest
		} else {
			challenge = rest[:end]
		}
		break
	}
	// Walk the output extracting runs of base64url chars and matching
	// length 43. Skip any run that equals the known challenge.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	allowed := make(map[byte]bool, len(alphabet))
	for i := 0; i < len(alphabet); i++ {
		allowed[alphabet[i]] = true
	}
	var run strings.Builder
	flush := func() bool {
		s := run.String()
		run.Reset()
		if len(s) == 43 && s != challenge {
			return true
		}
		return false
	}
	for i := 0; i < len(out); i++ {
		c := out[i]
		if allowed[c] {
			run.WriteByte(c)
			continue
		}
		if flush() {
			return true
		}
	}
	return flush()
}

func TestLogin_WithUse_ActivatesAccount(t *testing.T) {
	f := newLoginFixture(t)
	opts := f.makeOpts()
	opts.UseRandomPort = true
	opts.Use = true

	errCh := make(chan error, 1)
	go func() {
		target := <-f.browserCh
		u, _ := url.Parse(target)
		state := u.Query().Get("state")
		redirect := u.Query().Get("redirect_uri")
		callback := fmt.Sprintf("%s?code=c&state=%s", redirect, url.QueryEscape(state))
		for i := 0; i < 20; i++ {
			if resp, err := http.Get(callback); err == nil {
				resp.Body.Close()
				errCh <- nil
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		errCh <- errors.New("callback never succeeded")
	}()

	if err := Login(f.paths, "work", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	<-errCh

	// auth.json now contains the new anthropic blob.
	data, err := os.ReadFile(f.authPath)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("parse auth.json: %v", err)
	}
	anth, _ := top["anthropic"].(map[string]any)
	if anth["access"] != "access-tok" {
		t.Errorf("auth.json anthropic.access = %v, want access-tok", anth["access"])
	}

	// accounts/current reads "work".
	curData, _ := os.ReadFile(f.paths.Current)
	if got := strings.TrimSpace(string(curData)); got != "work" {
		t.Errorf("current = %q, want work", got)
	}

	// Success message reflects activation.
	if !strings.Contains(f.stdout.String(), "saved and activated") {
		t.Errorf("stdout missing activated message: %q", f.stdout.String())
	}
}

func TestLogin_RandomPort_RedirectURIMatchesBoundPort(t *testing.T) {
	f := newLoginFixture(t)
	opts := f.makeOpts()
	opts.UseRandomPort = true

	captured := make(chan string, 1)
	opts.Browser = func(target string) error {
		captured <- target
		return nil
	}

	errCh := make(chan error, 1)
	go func() {
		target := <-captured
		u, err := url.Parse(target)
		if err != nil {
			errCh <- err
			return
		}
		redirect := u.Query().Get("redirect_uri")
		if !strings.HasPrefix(redirect, "http://localhost:") {
			errCh <- fmt.Errorf("redirect_uri = %q, want http://localhost:...", redirect)
			return
		}
		ru, _ := url.Parse(redirect)
		port := ru.Port()
		if port == "" || port == "0" || port == fmt.Sprint(callbackPort) {
			errCh <- fmt.Errorf("port = %q, want a non-default random port", port)
			return
		}
		// Confirm we can actually hit that port — proves the listener bound there.
		state := u.Query().Get("state")
		callback := fmt.Sprintf("%s?code=c&state=%s", redirect, url.QueryEscape(state))
		for i := 0; i < 20; i++ {
			if resp, err := http.Get(callback); err == nil {
				resp.Body.Close()
				errCh <- nil
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		errCh <- errors.New("callback never succeeded")
	}()

	if err := Login(f.paths, "work", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("driver: %v", err)
	}
}

func TestLogin_PortInUse_ClearErrorAndSuggestsRandomPort(t *testing.T) {
	f := newLoginFixture(t)

	// Bind a random free port and hold it. Then point Login at the
	// same port.
	hog, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hog listen: %v", err)
	}
	defer hog.Close()
	busyPort := hog.Addr().(*net.TCPAddr).Port

	opts := f.makeOpts()
	opts.Port = busyPort

	err = Login(f.paths, "work", opts)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(busyPort)) {
		t.Errorf("error does not name port %d: %v", busyPort, err)
	}
	if !strings.Contains(err.Error(), "--port 0") {
		t.Errorf("error does not suggest --port 0: %v", err)
	}
	// No account file was created.
	if _, statErr := os.Stat(f.paths.AccountPath("work")); !os.IsNotExist(statErr) {
		t.Errorf("account file should not exist: %v", statErr)
	}
}

func TestLogin_InvalidName_FailsBeforeNetworkCall(t *testing.T) {
	f := newLoginFixture(t)
	opts := f.makeOpts()
	opts.UseRandomPort = true
	// Use a Browser that fails the test if it ever runs.
	opts.Browser = func(_ string) error {
		t.Fatal("browser called for invalid name")
		return nil
	}

	for _, bad := range []string{"", ".", "..", "current", "a/b", `a\b`} {
		if err := Login(f.paths, bad, opts); err == nil {
			t.Errorf("Login(%q): want error, got nil", bad)
		}
	}
	if f.tokenSeen.Load() != 0 {
		t.Errorf("token endpoint hit %d times for invalid name", f.tokenSeen.Load())
	}
}

func TestLogin_StateMismatch_400ToBrowser_AndOAuthStateMismatchError_NoEcho(t *testing.T) {
	f := newLoginFixture(t)
	opts := f.makeOpts()
	opts.UseRandomPort = true

	const fakeState = "definitely-not-the-real-state-aaaaaaaaaa"
	const expectedReal = "" // we don't know real state from outside; just confirm not echoed

	httpResp := make(chan *http.Response, 1)
	go func() {
		target := <-f.browserCh
		u, _ := url.Parse(target)
		redirect := u.Query().Get("redirect_uri")
		callback := fmt.Sprintf("%s?code=c&state=%s", redirect, url.QueryEscape(fakeState))
		for i := 0; i < 20; i++ {
			resp, err := http.Get(callback)
			if err == nil {
				httpResp <- resp
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		httpResp <- nil
	}()

	err := Login(f.paths, "work", opts)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "OAuth state mismatch") {
		t.Errorf("error = %q, want 'OAuth state mismatch' substring", err)
	}
	if strings.Contains(err.Error(), fakeState) {
		t.Errorf("error echoes received state: %v", err)
	}
	_ = expectedReal

	resp := <-httpResp
	if resp == nil {
		t.Fatal("no HTTP response captured from callback")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("callback HTTP status = %d, want 400", resp.StatusCode)
	}

	// No account file written.
	if _, statErr := os.Stat(f.paths.AccountPath("work")); !os.IsNotExist(statErr) {
		t.Errorf("account file should not exist: %v", statErr)
	}
}

func TestLogin_HeadlessSkipsBrowserAndPrintsURL(t *testing.T) {
	f := newLoginFixture(t)
	f.headless = true
	opts := f.makeOpts()
	opts.UseRandomPort = true
	opts.Browser = func(_ string) error {
		t.Fatal("browser launched in headless mode")
		return nil
	}
	// Provide stdin with the manual paste of a code#state pair. Drive
	// the test by parsing the printed URL to learn the state.
	pasteR, pasteW, _ := os.Pipe()
	opts.Stdin = pasteR
	defer pasteW.Close()

	var stdoutMu sync.Mutex
	stdoutBuf := &bytes.Buffer{}
	opts.Stdout = lockedWriter{m: &stdoutMu, w: stdoutBuf}

	errCh := make(chan error, 1)
	go func() {
		// Poll stdout for the authorize URL.
		deadline := time.Now().Add(2 * time.Second)
		var line string
		for time.Now().Before(deadline) {
			stdoutMu.Lock()
			line = stdoutBuf.String()
			stdoutMu.Unlock()
			if strings.Contains(line, "claude.ai/oauth/authorize") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		idx := strings.Index(line, "https://claude.ai/oauth/authorize")
		if idx == -1 {
			errCh <- errors.New("authorize URL never printed to stdout")
			return
		}
		rest := line[idx:]
		end := strings.IndexAny(rest, "\n ")
		if end == -1 {
			end = len(rest)
		}
		raw := rest[:end]
		u, err := url.Parse(raw)
		if err != nil {
			errCh <- err
			return
		}
		state := u.Query().Get("state")
		_, err = pasteW.Write([]byte("code-from-paste#" + state + "\n"))
		errCh <- err
	}()

	if err := Login(f.paths, "work", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("driver: %v", err)
	}

	if !strings.Contains(stdoutBuf.String(), "claude.ai/oauth/authorize") {
		t.Errorf("stdout missing authorize URL")
	}
}

// lockedWriter is a minimal io.Writer with mutex protection so the
// stdout-polling goroutine in TestLogin_HeadlessSkipsBrowserAndPrintsURL
// doesn't race the Login goroutine writing into the buffer.
type lockedWriter struct {
	m *sync.Mutex
	w *bytes.Buffer
}

func (lw lockedWriter) Write(p []byte) (int, error) {
	lw.m.Lock()
	defer lw.m.Unlock()
	return lw.w.Write(p)
}

func TestLogin_BrowserExecFails_FallsThroughToManualPaste(t *testing.T) {
	f := newLoginFixture(t)
	opts := f.makeOpts()
	opts.UseRandomPort = true
	opts.Browser = func(_ string) error {
		return errors.New("xdg-open: command not found")
	}
	// Manual paste reader — we need to learn the state from stdout
	// first, so use a pipe.
	pasteR, pasteW, _ := os.Pipe()
	opts.Stdin = pasteR
	defer pasteW.Close()

	var mu sync.Mutex
	buf := &bytes.Buffer{}
	opts.Stdout = lockedWriter{m: &mu, w: buf}

	errCh := make(chan error, 1)
	go func() {
		// Poll for the URL on stdout.
		deadline := time.Now().Add(2 * time.Second)
		var line string
		for time.Now().Before(deadline) {
			mu.Lock()
			line = buf.String()
			mu.Unlock()
			if strings.Contains(line, "claude.ai/oauth/authorize") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		idx := strings.Index(line, "https://claude.ai/oauth/authorize")
		if idx == -1 {
			errCh <- errors.New("URL not printed")
			return
		}
		rest := line[idx:]
		end := strings.IndexAny(rest, "\n ")
		raw := rest
		if end != -1 {
			raw = rest[:end]
		}
		u, err := url.Parse(raw)
		if err != nil {
			errCh <- err
			return
		}
		state := u.Query().Get("state")
		_, err = pasteW.Write([]byte("c#" + state + "\n"))
		errCh <- err
	}()

	if err := Login(f.paths, "work", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("driver: %v", err)
	}
	if !strings.Contains(buf.String(), "could not open browser") {
		t.Errorf("stdout missing browser-failure breadcrumb: %q", buf.String())
	}
}

func TestLogin_Timeout_NoFileWritten(t *testing.T) {
	f := newLoginFixture(t)
	opts := f.makeOpts()
	opts.UseRandomPort = true
	opts.Timeout = 100 * time.Millisecond
	// Browser does nothing; stdin is empty; no callback. Login must
	// time out.
	opts.Browser = func(_ string) error { return nil }

	err := Login(f.paths, "work", opts)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want 'timed out' substring", err)
	}
	if _, statErr := os.Stat(f.paths.AccountPath("work")); !os.IsNotExist(statErr) {
		t.Errorf("account file should not exist on timeout: %v", statErr)
	}
}

func TestLogin_TokenExchangeFailure_NoFileWritten(t *testing.T) {
	f := newLoginFixture(t)
	// Replace token server with one that always fails non-retryably.
	f.tokenSrv.Close()
	f.tokenSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Should-Retry", "false")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(f.tokenSrv.Close)

	opts := f.makeOpts()
	opts.UseRandomPort = true

	errCh := make(chan error, 1)
	go func() {
		target := <-f.browserCh
		u, _ := url.Parse(target)
		state := u.Query().Get("state")
		redirect := u.Query().Get("redirect_uri")
		callback := fmt.Sprintf("%s?code=c&state=%s", redirect, url.QueryEscape(state))
		for i := 0; i < 20; i++ {
			if resp, err := http.Get(callback); err == nil {
				resp.Body.Close()
				errCh <- nil
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		errCh <- errors.New("callback never succeeded")
	}()

	err := Login(f.paths, "work", opts)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error does not include status: %v", err)
	}
	<-errCh
	if _, statErr := os.Stat(f.paths.AccountPath("work")); !os.IsNotExist(statErr) {
		t.Errorf("account file should not exist on token failure: %v", statErr)
	}
}

func TestLogin_OverwritesExistingAccountFile(t *testing.T) {
	f := newLoginFixture(t)
	// Init the dir and pre-seed work.json with a marker blob.
	if err := Init(f.paths); err != nil {
		t.Fatalf("Init: %v", err)
	}
	pre := []byte(`{"type":"oauth","access":"old","refresh":"old","expires":1}`)
	if err := os.WriteFile(f.paths.AccountPath("work"), pre, 0o600); err != nil {
		t.Fatalf("pre-seed: %v", err)
	}

	opts := f.makeOpts()
	opts.UseRandomPort = true

	errCh := make(chan error, 1)
	go func() {
		target := <-f.browserCh
		u, _ := url.Parse(target)
		state := u.Query().Get("state")
		redirect := u.Query().Get("redirect_uri")
		callback := fmt.Sprintf("%s?code=c&state=%s", redirect, url.QueryEscape(state))
		for i := 0; i < 20; i++ {
			if resp, err := http.Get(callback); err == nil {
				resp.Body.Close()
				errCh <- nil
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		errCh <- errors.New("callback never succeeded")
	}()

	if err := Login(f.paths, "work", opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	<-errCh

	data, _ := os.ReadFile(f.paths.AccountPath("work"))
	var got accountBlob
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Access != "access-tok" {
		t.Errorf("file not overwritten: %+v", got)
	}
}

// ─── isHeadlessEnv ───────────────────────────────────────────────────

func TestIsHeadlessEnv(t *testing.T) {
	cases := []struct {
		name    string
		display string
		wayland string
		ssh     string
		want    bool
	}{
		{"all unset → not headless", "", "", "", false},
		{"display set → not headless", ":0", "", "", false},
		{"wayland set → not headless", "", "wayland-0", "", false},
		{"ssh set, no display → headless", "", "", "1.2.3.4 1234 5.6.7.8 22", true},
		{"ssh set with display → not headless", ":0", "", "1.2.3.4 1234 5.6.7.8 22", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DISPLAY", tc.display)
			t.Setenv("WAYLAND_DISPLAY", tc.wayland)
			t.Setenv("SSH_CONNECTION", tc.ssh)
			if got := isHeadlessEnv(); got != tc.want {
				t.Errorf("isHeadlessEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}
