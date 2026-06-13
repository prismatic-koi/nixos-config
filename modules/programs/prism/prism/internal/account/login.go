// Port of modules/programs/prism/pi/extensions/anthropic-oauth/auth.ts.
// auth.ts remains the canonical source of truth for the Anthropic OAuth
// protocol; this is a Go mirror. When upstream auth.ts changes, mirror
// the change here.
//
// Implementation of `prism account login <name>` (#2284): PKCE + local
// callback server + token exchange + atomic write into the per-account
// store maintained by the rest of this package.
//
// Security invariants (also enforced by tests):
//
//   - The PKCE verifier never appears in stdout, stderr, or any log
//     line. Only the challenge (derived; safe to surface) ever leaves
//     this file.
//   - The OAuth `code` parameter never appears in any error message
//     returned to the user — error strings include only the response
//     HTTP status, never the request body or the code value.
//   - Access and refresh tokens never appear in stdout, stderr, or any
//     log line at any verbosity. The success message names only the
//     account.
//   - On state mismatch the error is the literal string "OAuth state
//     mismatch" with no echo of either the expected or received state
//     value.

package account

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// OAuth constants — mirror auth.ts. If auth.ts changes, change these.
const (
	oauthClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthAuthorizeURL = "https://claude.ai/oauth/authorize"
	oauthTokenURL     = "https://claude.ai/v1/oauth/token"
	// oauthRedirectURI is the manual-paste fallback redirect_uri,
	// served by Anthropic's platform — used only when the local
	// callback server cannot bind. The active redirect_uri value at
	// runtime is the local http://localhost:<port>/callback computed
	// from the bound listener.
	oauthRedirectURI = "https://platform.claude.com/oauth/code/callback"
	oauthScopes      = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	oauthUserAgent   = "claude-code/2.1.97"
	callbackPort     = 53692
	callbackHost     = "127.0.0.1"
	callbackTimeout  = 5 * time.Minute
	maxTokenRetries  = 2
	initialRetryDelay = 5 * time.Second
	tokenExpirySafety = 5 * time.Minute
	retryAfterCap     = 30 * time.Second
)

// browserLauncher is the OS-dispatch hook for opening the system
// browser. Substitute via opts.Browser in tests.
type browserLauncher func(url string) error

// defaultBrowserLauncher is the runtime browser-launcher. Tests can
// swap this package var directly, but the preferred seam is
// LoginOptions.Browser which never touches process state.
var defaultBrowserLauncher browserLauncher = launchBrowser

func launchBrowser(target string) error {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("xdg-open", target).Start()
	case "darwin":
		return exec.Command("open", target).Start()
	}
	return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
}

// isHeadlessEnv returns true when the current process appears to be
// running in a headless SSH session — no graphical display and an SSH
// connection variable is set. In that case the browser-launch step is
// skipped and the user falls straight through to manual paste.
func isHeadlessEnv() bool {
	return os.Getenv("DISPLAY") == "" &&
		os.Getenv("WAYLAND_DISPLAY") == "" &&
		os.Getenv("SSH_CONNECTION") != ""
}

// LoginOptions controls a single `prism account login` invocation.
// Fields with zero values fall back to their package-default behaviour
// via applyDefaults so callers (and tests) only set the bits they care
// about.
type LoginOptions struct {
	// Use, when true, calls Use(paths, name) after the account file has
	// been written, atomically activating the new account.
	Use bool
	// Port is the TCP port to bind the local callback server on.
	// Defaults to callbackPort (53692); a zero value resolved by
	// applyDefaults means "use the default". To bind a random free
	// port, pass UseRandomPort = true.
	Port int
	// UseRandomPort, when true, binds the callback server on a random
	// free port (net.Listen with port 0). Mutually exclusive with Port.
	UseRandomPort bool

	// Test seams — production callers leave these nil and applyDefaults
	// fills in the package-level defaults.
	TokenURL   string
	AuthURL    string
	HTTPClient *http.Client
	Browser    browserLauncher
	IsHeadless func() bool
	Stdin      io.Reader
	Stdout     io.Writer
	Now        func() time.Time
	Sleep      func(d time.Duration)
	Timeout    time.Duration
}

func (o *LoginOptions) applyDefaults() {
	if o.TokenURL == "" {
		o.TokenURL = oauthTokenURL
	}
	if o.AuthURL == "" {
		o.AuthURL = oauthAuthorizeURL
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Browser == nil {
		o.Browser = defaultBrowserLauncher
	}
	if o.IsHeadless == nil {
		o.IsHeadless = isHeadlessEnv
	}
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Timeout == 0 {
		o.Timeout = callbackTimeout
	}
	if !o.UseRandomPort && o.Port == 0 {
		o.Port = callbackPort
	}
}

// tokenResponse is the JSON shape returned by TOKEN_URL.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// accountBlob is the on-disk shape written to accounts/<name>.json. It
// matches the value of auth.json's "anthropic" key as produced by pi.
type accountBlob struct {
	Type    string `json:"type"`
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Expires int64  `json:"expires"`
}

// Login runs the OAuth + PKCE flow against Anthropic, persists the
// resulting tokens to accounts/<name>.json (mode 0o600), and optionally
// calls Use(paths, name) when opts.Use is true.
//
// On any error before the account file is written, no token file is
// created (atomic write). On error after Use begins, the account file
// is on disk; the caller decides what to do.
func Login(paths Paths, name string, opts LoginOptions) error {
	opts.applyDefaults()

	// Validate name before doing anything network-shaped — the AC
	// requires that an invalid name fails before any port is bound or
	// HTTP request issued.
	if err := validName(name); err != nil {
		return err
	}
	if err := Init(paths); err != nil {
		return err
	}

	verifier, challenge, err := generatePKCE()
	if err != nil {
		return fmt.Errorf("generate PKCE: %w", err)
	}
	state, err := generateState()
	if err != nil {
		return fmt.Errorf("generate state: %w", err)
	}

	// Bind the local callback server. A bind failure here is a
	// first-class error: name the port and suggest --port 0.
	bindPort := opts.Port
	if opts.UseRandomPort {
		bindPort = 0
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", callbackHost, bindPort))
	if err != nil {
		return fmt.Errorf("bind callback server on port %d: %w (try --port 0 for a random free port)", bindPort, err)
	}
	boundPort := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", boundPort)
	authorizeURL := makeAuthorizeURL(opts.AuthURL, challenge, state, redirectURI)

	callbackCh := make(chan callbackResult, 1)
	srv := startCallbackServer(ln, state, callbackCh)
	// Ensure the listening socket is released no matter how we exit.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// Browser launch (or skip in headless mode). The URL is always
	// printed so users with the browser on a different machine can
	// copy-paste it themselves.
	headless := opts.IsHeadless()
	fmt.Fprintf(opts.Stdout, "Open this URL in your browser to authorize prism:\n  %s\n", authorizeURL)
	if headless {
		fmt.Fprintln(opts.Stdout, "(running headless — paste the callback URL or code#state when done)")
	} else {
		if err := opts.Browser(authorizeURL); err != nil {
			// Browser exec failed (e.g. xdg-open missing). The AC
			// requires we fall through silently and let the user paste
			// the callback manually.
			fmt.Fprintln(opts.Stdout, "(could not open browser — paste the callback URL or code#state when done)")
		} else {
			fmt.Fprintln(opts.Stdout, "(opened browser; or paste the callback URL / code#state here when done)")
		}
	}

	// Wait for either the local callback to fire or the user to paste
	// the callback URL on stdin, whichever wins. A 5-minute hard
	// timeout bounds the whole operation.
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()
	inputCh := make(chan inputResult, 2)
	go waitCallback(ctx, callbackCh, inputCh)
	go waitStdin(ctx, opts.Stdin, inputCh)

	var captured inputResult
	select {
	case captured = <-inputCh:
		if captured.err != nil {
			return captured.err
		}
	case <-ctx.Done():
		return fmt.Errorf("login timed out after %s waiting for OAuth callback or manual paste", opts.Timeout)
	}

	// State check — guard against an attacker round-tripping a
	// matched code/state pair from a different flow. The local server
	// also checks this and returns 400 to the browser; this branch
	// covers the manual-paste path.
	if captured.state != state {
		return errors.New("OAuth state mismatch")
	}

	tr, err := exchangeToken(ctx, opts, captured.code, captured.state, verifier, redirectURI)
	if err != nil {
		return err
	}

	// Match pi-side computation exactly:
	//   expires = Date.now() + expires_in * 1000 - 5 * 60 * 1000
	expiresMS := opts.Now().UnixMilli() + tr.ExpiresIn*1000 - int64(tokenExpirySafety/time.Millisecond)

	blob := accountBlob{
		Type:    "oauth",
		Access:  tr.AccessToken,
		Refresh: tr.RefreshToken,
		Expires: expiresMS,
	}
	blobBytes, err := json.MarshalIndent(blob, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal account blob: %w", err)
	}
	blobBytes = append(blobBytes, '\n')
	if err := atomicWriteFile(paths.AccountPath(name), blobBytes, fileMode); err != nil {
		return fmt.Errorf("write account file: %w", err)
	}

	if opts.Use {
		if err := Use(paths, name); err != nil {
			return fmt.Errorf("activate account %s: %w", name, err)
		}
		fmt.Fprintf(opts.Stdout, "account %s saved and activated.\n", name)
		return nil
	}

	fmt.Fprintf(opts.Stdout, "account %s saved. Run 'prism account use %s' to activate.\n", name, name)
	return nil
}

// callbackResult is what the local callback server reports back to the
// main goroutine: either a (code, state) pair or an error (e.g. state
// mismatch).
type callbackResult struct {
	code  string
	state string
	err   error
}

// inputResult is the merged channel value from the two waiters
// (callback + stdin).
type inputResult struct {
	code  string
	state string
	err   error
}

// waitCallback forwards the local server's result onto inputCh,
// respecting ctx cancellation.
func waitCallback(ctx context.Context, in <-chan callbackResult, out chan<- inputResult) {
	select {
	case r := <-in:
		select {
		case out <- inputResult{code: r.code, state: r.state, err: r.err}:
		case <-ctx.Done():
		}
	case <-ctx.Done():
	}
}

// waitStdin parses a single line from stdin (typically the user
// pasting the callback URL) and forwards the parsed result onto
// inputCh. EOF without data is silent — we just rely on the callback
// or the outer timeout.
func waitStdin(ctx context.Context, r io.Reader, out chan<- inputResult) {
	scanner := bufio.NewScanner(r)
	// OAuth callback URLs can be a few KB; bump the buffer ceiling.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		return
	}
	line := scanner.Text()
	parsed, ok := parseAuthInput(line)
	if !ok {
		select {
		case out <- inputResult{err: errors.New("could not parse pasted callback input")}:
		case <-ctx.Done():
		}
		return
	}
	select {
	case out <- inputResult{code: parsed.code, state: parsed.state}:
	case <-ctx.Done():
	}
}

// generatePKCE returns a fresh (verifier, challenge) pair per RFC 7636
// S256. verifier is 32 random bytes base64url-encoded (no padding);
// challenge is the SHA-256 of the verifier (taken over its ASCII
// bytes — not the raw entropy bytes — matching the JS reference
// implementation in auth.ts) base64url-encoded.
func generatePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(digest[:])
	return verifier, challenge, nil
}

// generateState returns 16 cryptographically-random bytes hex-encoded
// (32 hex chars), matching auth.ts's `crypto.randomUUID().replace(/-/g, "")`.
func generateState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// makeAuthorizeURL composes the OAuth authorize URL — same parameter
// set and ordering as auth.ts::makeAuthorizeUrl.
func makeAuthorizeURL(base, challenge, state, redirectURI string) string {
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", oauthClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", oauthScopes)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	return base + "?" + q.Encode()
}

// startCallbackServer wires a one-shot HTTP server onto the supplied
// listener and returns it. The server writes the result of the first
// valid /callback hit (or a state mismatch) onto resultCh.
func startCallbackServer(ln net.Listener, expectedState string, resultCh chan<- callbackResult) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		gotState := r.URL.Query().Get("state")
		if code == "" || gotState == "" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(w, "Missing code or state", http.StatusBadRequest)
			return
		}
		if gotState != expectedState {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(w, "Invalid state", http.StatusBadRequest)
			// Signal the main goroutine so the command exits with a
			// clear error rather than waiting for the timeout. The
			// error string does NOT include either state value.
			select {
			case resultCh <- callbackResult{err: errors.New("OAuth state mismatch")}:
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, callbackSuccessHTML())
		select {
		case resultCh <- callbackResult{code: code, state: gotState}:
		default:
		}
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	return srv
}

func callbackSuccessHTML() string {
	return `<!doctype html>
<html>
  <head><meta charset="utf-8" /><title>Authorization complete</title></head>
  <body>
    <h1>Authorization complete</h1>
    <p>You can close this window and return to prism.</p>
  </body>
</html>`
}

// parsedAuthInput is what parseAuthInput surfaces on success.
type parsedAuthInput struct {
	code  string
	state string
}

// parseAuthInput accepts the three formats from auth.ts::parseAuthInput:
//
//  1. Full URL with ?code=…&state=… (e.g. the redirect URL the browser
//     lands on after authorization).
//  2. `code#state` shorthand (the Anthropic platform-side fallback page
//     renders this for users to copy).
//  3. Raw query string `code=…&state=…`.
//
// Returns (zero, false) when none of the three forms produce both a
// non-empty code and a non-empty state.
func parseAuthInput(input string) (parsedAuthInput, bool) {
	text := strings.TrimSpace(input)
	if text == "" {
		return parsedAuthInput{}, false
	}

	// Form 1: full URL. url.Parse is lenient in Go so we additionally
	// require a non-empty scheme — same as JS `new URL(text)` strictness.
	if u, err := url.Parse(text); err == nil && u.Scheme != "" {
		code := u.Query().Get("code")
		state := u.Query().Get("state")
		if code != "" && state != "" {
			return parsedAuthInput{code: code, state: state}, true
		}
	}

	// Form 2: code#state shorthand. Mirror auth.ts exactly — split on
	// '#', accept iff both halves are non-empty.
	parts := strings.SplitN(text, "#", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parsedAuthInput{code: parts[0], state: parts[1]}, true
	}

	// Form 3: raw query string.
	if vals, err := url.ParseQuery(text); err == nil {
		code := vals.Get("code")
		state := vals.Get("state")
		if code != "" && state != "" {
			return parsedAuthInput{code: code, state: state}, true
		}
	}

	return parsedAuthInput{}, false
}

// exchangeToken POSTs the authorization code to TOKEN_URL and returns
// the parsed response. Retries up to maxTokenRetries on 429 / 5xx,
// honours Retry-After (capped at retryAfterCap), and aborts immediately
// on X-Should-Retry: false.
//
// Error messages MUST NOT include the request body (which contains the
// code and verifier) — only the response HTTP status. The response body
// is server-controlled and does not contain the code/verifier values.
func exchangeToken(ctx context.Context, opts LoginOptions, code, state, verifier, redirectURI string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", oauthClientID)
	form.Set("code", code)
	form.Set("state", state)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)
	body := form.Encode()

	var lastStatus int
	for attempt := 0; attempt <= maxTokenRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.TokenURL, strings.NewReader(body))
		if err != nil {
			return tokenResponse{}, fmt.Errorf("token exchange: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", oauthUserAgent)

		resp, err := opts.HTTPClient.Do(req)
		if err != nil {
			return tokenResponse{}, fmt.Errorf("token exchange: HTTP request failed: %w", err)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			tr, decErr := decodeTokenResponse(resp)
			if decErr != nil {
				return tokenResponse{}, fmt.Errorf("token exchange: parse response: %w", decErr)
			}
			return tr, nil
		}

		lastStatus = resp.StatusCode
		shouldRetryHeader := resp.Header.Get("X-Should-Retry")
		retryAfterHeader := resp.Header.Get("Retry-After")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if shouldRetryHeader == "false" {
			return tokenResponse{}, fmt.Errorf("token exchange failed: HTTP %d", lastStatus)
		}

		retriable := lastStatus == http.StatusTooManyRequests || lastStatus >= 500
		if attempt < maxTokenRetries && retriable {
			opts.Sleep(retryDelay(retryAfterHeader, attempt))
			continue
		}

		return tokenResponse{}, fmt.Errorf("token exchange failed: HTTP %d", lastStatus)
	}
	return tokenResponse{}, fmt.Errorf("token exchange failed after retries: HTTP %d", lastStatus)
}

func decodeTokenResponse(resp *http.Response) (tokenResponse, error) {
	defer resp.Body.Close()
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return tokenResponse{}, err
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		return tokenResponse{}, errors.New("token response missing access_token or refresh_token")
	}
	return tr, nil
}

// retryDelay computes the backoff between attempts. When Retry-After is
// supplied and parses as an integer seconds value, use it (capped at
// retryAfterCap). Otherwise use exponential backoff anchored on
// initialRetryDelay.
func retryDelay(retryAfter string, attempt int) time.Duration {
	if retryAfter != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && secs >= 0 {
			delay := time.Duration(secs) * time.Second
			if delay > retryAfterCap {
				delay = retryAfterCap
			}
			return delay
		}
	}
	return initialRetryDelay * time.Duration(1<<attempt)
}
