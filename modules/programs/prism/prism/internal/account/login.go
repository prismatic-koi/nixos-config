// Port of modules/programs/prism/pi/extensions/anthropic-oauth/auth.ts.
// auth.ts remains the canonical source of truth for the Anthropic OAuth
// protocol; this is a Go mirror. When upstream auth.ts changes, mirror
// the change here.
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
	"sync"
	"syscall"
	"time"
)

const (
	oauthClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthAuthorizeURL = "https://claude.ai/oauth/authorize"
	oauthTokenURL     = "https://claude.ai/v1/oauth/token"
	oauthRedirectURI  = "https://platform.claude.com/oauth/code/callback" // manual fallback
	oauthScopes       = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	oauthUserAgent    = "claude-code/2.1.97" // mirror auth.ts; bump when auth.ts bumps
	callbackPort      = 53692
	callbackHost      = "127.0.0.1"
	callbackTimeout   = 5 * time.Minute
	maxTokenRetries   = 2
	initialRetryDelay = 5 * time.Second
	tokenExpirySafety = 5 * time.Minute
)

// DefaultCallbackPort is the auth.ts default local OAuth callback port.
const DefaultCallbackPort = callbackPort

// browserLauncher is the injectable seam for tests. Production dispatches to
// the platform's usual browser opener; tests replace defaultBrowserLauncher
// with a fake that records the authorize URL or forces manual-paste fallback.
type browserLauncher func(url string) error

var defaultBrowserLauncher browserLauncher = launchBrowser

func launchBrowser(url string) error {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	}
	return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
}

// LoginOptions configures Login. Port 0 asks the kernel for a random free
// callback port; callers that want the auth.ts default should pass
// callbackPort. Other zero values select production defaults. Tests inject
// Stdin/Stdout, HTTPClient, TokenURL, Timeout, Now, and RetrySleep to keep the
// OAuth flow hermetic and fast.
type LoginOptions struct {
	Use        bool
	Port       int
	Stdin      io.Reader
	Stdout     io.Writer
	HTTPClient *http.Client
	TokenURL   string
	Timeout    time.Duration
	Now        func() time.Time
	RetrySleep func(context.Context, time.Duration) error
}

type pkcePair struct {
	verifier  string
	challenge string
}

type parsedAuthInput struct {
	code  string
	state string
}

type oauthCredentials struct {
	Type    string `json:"type"`
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Expires int64  `json:"expires"`
}

var errOAuthStateMismatch = errors.New("OAuth state mismatch")

// Login runs the Anthropic OAuth PKCE flow, writes the resulting credentials
// to accounts/<name>.json, and optionally activates them via Use.
func Login(ctx context.Context, p Paths, name string, opts LoginOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validLoginName(name); err != nil {
		return err
	}
	opts = normaliseLoginOptions(opts)
	if opts.Port < 0 || opts.Port > 65535 {
		return fmt.Errorf("callback port must be between 0 and 65535")
	}

	if err := Init(p); err != nil {
		return err
	}

	pkce, err := generatePKCE()
	if err != nil {
		return fmt.Errorf("generate PKCE challenge: %w", err)
	}
	state, err := generateState()
	if err != nil {
		return fmt.Errorf("generate OAuth state: %w", err)
	}

	localAuthorization, err := createLocalAuthorization(state, opts.Port)
	if err != nil {
		return formatCallbackListenError(opts.Port, err)
	}
	defer localAuthorization.cancel()

	redirectURI := localAuthorization.redirectURI
	authorizeURL := makeAuthorizeURL(pkce.challenge, state, redirectURI)
	manualOnly := false

	if isHeadlessSSH() {
		manualOnly = true
	} else if err := defaultBrowserLauncher(authorizeURL); err != nil {
		manualOnly = true
	}

	if manualOnly {
		localAuthorization.cancel()
		redirectURI = oauthRedirectURI
		authorizeURL = makeAuthorizeURL(pkce.challenge, state, redirectURI)
	}

	parsed, err := waitForAuthorization(ctx, waitForAuthorizationOptions{
		stdin:              opts.Stdin,
		stdout:             opts.Stdout,
		authorizeURL:       authorizeURL,
		state:              state,
		localAuthorization: localAuthorization,
		timeout:            opts.Timeout,
		printAuthorizeURL:  manualOnly,
	})
	if err != nil {
		return err
	}

	creds, err := exchangeAuthorizationCode(ctx, exchangeOptions{
		client:      opts.HTTPClient,
		tokenURL:    opts.TokenURL,
		code:        parsed.code,
		state:       parsed.state,
		redirectURI: redirectURI,
		verifier:    pkce.verifier,
		now:         opts.Now,
		retrySleep:  opts.RetrySleep,
	})
	if err != nil {
		return err
	}

	blob, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("account login %s: marshal account file: %w", name, err)
	}
	blob = append(blob, '\n')

	if err := atomicWriteFile(p.AccountPath(name), blob, fileMode); err != nil {
		return fmt.Errorf("account login %s: write account file: %w", name, err)
	}

	if opts.Use {
		if err := activateLogin(p, name, blob); err != nil {
			return err
		}
		return nil
	}

	fmt.Fprintf(opts.Stdout, "account %s saved. Run 'prism account use %s' to activate.\n", name, name)
	return nil
}

func activateLogin(p Paths, name string, blob []byte) error {
	cur, hasCur, err := Current(p)
	if err != nil {
		return fmt.Errorf("account use %s: %w", name, err)
	}
	if hasCur && cur == name {
		merged, err := mergeAnthropicBlob(p.AuthJSON, blob)
		if err != nil {
			return fmt.Errorf("account use %s: merge %s: %w", name, p.AuthJSON, err)
		}
		if err := atomicWriteFile(p.AuthJSON, merged, fileMode); err != nil {
			return fmt.Errorf("account use %s: write %s: %w", name, p.AuthJSON, err)
		}
		if err := atomicWriteFile(p.Current, []byte(name+"\n"), fileMode); err != nil {
			return fmt.Errorf("account use %s: write %s: %w", name, p.Current, err)
		}
		return nil
	}
	return Use(p, name)
}

func validLoginName(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if strings.ContainsRune(name, '.') {
		return fmt.Errorf("account name %q must not contain dots", name)
	}
	return nil
}

func normaliseLoginOptions(opts LoginOptions) LoginOptions {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.TokenURL == "" {
		opts.TokenURL = oauthTokenURL
	}
	if opts.Timeout == 0 {
		opts.Timeout = callbackTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RetrySleep == nil {
		opts.RetrySleep = sleepContext
	}
	return opts
}

func generatePKCE() (pkcePair, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return pkcePair{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(bytes)
	digest := sha256.Sum256([]byte(verifier))
	return pkcePair{
		verifier:  verifier,
		challenge: base64.RawURLEncoding.EncodeToString(digest[:]),
	}, nil
}

func generateState() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func makeAuthorizeURL(challenge, state, redirectURI string) string {
	params := url.Values{}
	params.Set("code", "true")
	params.Set("client_id", oauthClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", oauthScopes)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	return oauthAuthorizeURL + "?" + params.Encode()
}

func parseAuthInput(input string) (parsedAuthInput, bool) {
	text := strings.TrimSpace(input)

	if u, err := url.Parse(text); err == nil && u.Scheme != "" && u.Host != "" {
		code := u.Query().Get("code")
		state := u.Query().Get("state")
		if code != "" && state != "" {
			return parsedAuthInput{code: code, state: state}, true
		}
	}

	split := strings.Split(text, "#")
	if len(split) == 2 && split[0] != "" && split[1] != "" {
		return parsedAuthInput{code: split[0], state: split[1]}, true
	}

	params, err := url.ParseQuery(text)
	if err != nil {
		return parsedAuthInput{}, false
	}
	code := params.Get("code")
	state := params.Get("state")
	if code != "" && state != "" {
		return parsedAuthInput{code: code, state: state}, true
	}
	return parsedAuthInput{}, false
}

func isHeadlessSSH() bool {
	return os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("SSH_CONNECTION") != ""
}

type localAuthorization struct {
	redirectURI string
	server      *http.Server
	resultCh    chan authorizationResult
	completeOne sync.Once
	closeOne    sync.Once
}

type authorizationResult struct {
	input string
	err   error
}

func createLocalAuthorization(state string, port int) (*localAuthorization, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(callbackHost, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port

	local := &localAuthorization{
		redirectURI: fmt.Sprintf("http://localhost:%d/callback", actualPort),
		resultCh:    make(chan authorizationResult, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		code := r.URL.Query().Get("code")
		gotState := r.URL.Query().Get("state")
		if code == "" || gotState == "" {
			http.Error(w, "Missing code or state", http.StatusBadRequest)
			return
		}
		if gotState != state {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			local.complete(authorizationResult{err: errOAuthStateMismatch})
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, makeCallbackPage())

		host := r.Host
		if host == "" {
			host = net.JoinHostPort(callbackHost, strconv.Itoa(actualPort))
		}
		callbackURL := url.URL{Scheme: "http", Host: host, Path: r.URL.Path, RawQuery: r.URL.RawQuery}
		local.complete(authorizationResult{input: callbackURL.String()})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	server := &http.Server{Handler: mux}
	local.server = server

	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			local.complete(authorizationResult{err: fmt.Errorf("OAuth callback server error: %w", serveErr)})
		}
	}()

	return local, nil
}

func (l *localAuthorization) complete(result authorizationResult) {
	l.completeOne.Do(func() {
		l.resultCh <- result
	})
}

func (l *localAuthorization) cancel() {
	l.closeOne.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := l.server.Shutdown(ctx); err != nil {
			_ = l.server.Close()
		}
	})
}

func makeCallbackPage() string {
	return `<!doctype html>
<html>
  <head><meta charset="utf-8" /><title>Authorization complete</title></head>
  <body>
    <h1>Authorization complete</h1>
    <p>You can close this window and return to prism.</p>
  </body>
</html>`
}

type waitForAuthorizationOptions struct {
	stdin              io.Reader
	stdout             io.Writer
	authorizeURL       string
	state              string
	localAuthorization *localAuthorization
	timeout            time.Duration
	printAuthorizeURL  bool
}

func waitForAuthorization(ctx context.Context, opts waitForAuthorizationOptions) (parsedAuthInput, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	if opts.printAuthorizeURL {
		fmt.Fprintf(opts.stdout, "Open this URL to sign in:\n%s\n", opts.authorizeURL)
	}
	fmt.Fprintln(opts.stdout, "Paste the callback URL or code#state (or wait for browser redirect):")

	manualCh := readManualAuthInput(opts.stdin)
	var callbackCh <-chan authorizationResult
	if opts.localAuthorization != nil {
		callbackCh = opts.localAuthorization.resultCh
	}

	for manualCh != nil || callbackCh != nil {
		select {
		case <-ctx.Done():
			if opts.localAuthorization != nil {
				opts.localAuthorization.cancel()
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return parsedAuthInput{}, fmt.Errorf("OAuth authorization timed out after %s", opts.timeout)
			}
			return parsedAuthInput{}, ctx.Err()
		case result := <-callbackCh:
			if opts.localAuthorization != nil {
				opts.localAuthorization.cancel()
			}
			if result.err != nil {
				return parsedAuthInput{}, result.err
			}
			parsed, ok := parseAuthInput(result.input)
			if !ok {
				return parsedAuthInput{}, errors.New("Could not parse authorization callback input.")
			}
			if parsed.state != opts.state {
				return parsedAuthInput{}, errOAuthStateMismatch
			}
			return parsed, nil
		case result := <-manualCh:
			if !result.hasInput {
				manualCh = nil
				continue
			}
			if opts.localAuthorization != nil {
				opts.localAuthorization.cancel()
			}
			if result.err != nil {
				return parsedAuthInput{}, result.err
			}
			parsed, ok := parseAuthInput(result.input)
			if !ok {
				return parsedAuthInput{}, errors.New("Could not parse authorization callback input.")
			}
			if parsed.state != opts.state {
				return parsedAuthInput{}, errOAuthStateMismatch
			}
			return parsed, nil
		}
	}

	<-ctx.Done()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return parsedAuthInput{}, fmt.Errorf("OAuth authorization timed out after %s", opts.timeout)
	}
	return parsedAuthInput{}, ctx.Err()
}

type manualAuthResult struct {
	input    string
	err      error
	hasInput bool
}

func readManualAuthInput(r io.Reader) <-chan manualAuthResult {
	ch := make(chan manualAuthResult, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" || err == nil {
			ch <- manualAuthResult{input: line, hasInput: true}
			return
		}
		if errors.Is(err, io.EOF) {
			ch <- manualAuthResult{hasInput: false}
			return
		}
		ch <- manualAuthResult{err: fmt.Errorf("read authorization input: %w", err), hasInput: true}
	}()
	return ch
}

type exchangeOptions struct {
	client      *http.Client
	tokenURL    string
	code        string
	state       string
	redirectURI string
	verifier    string
	now         func() time.Time
	retrySleep  func(context.Context, time.Duration) error
}

func exchangeAuthorizationCode(ctx context.Context, opts exchangeOptions) (oauthCredentials, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", oauthClientID)
	form.Set("code", opts.code)
	form.Set("state", opts.state)
	form.Set("redirect_uri", opts.redirectURI)
	form.Set("code_verifier", opts.verifier)

	var finalStatus int
	for attempt := 0; attempt <= maxTokenRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.tokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return oauthCredentials{}, fmt.Errorf("Token exchange failed: create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		// auth.ts deliberately does not send USER_AGENT on token exchange: the
		// claude.ai token endpoint rejects claude-code/* user agents. Suppress
		// Go's implicit default too so the wire format stays header-minimal.
		req.Header.Set("User-Agent", "")

		resp, err := opts.client.Do(req)
		if err != nil {
			return oauthCredentials{}, fmt.Errorf("Token exchange failed: %w", err)
		}

		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			defer resp.Body.Close()
			var data struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				ExpiresIn    int64  `json:"expires_in"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				return oauthCredentials{}, fmt.Errorf("Token exchange failed: parse response: %w", err)
			}
			if data.AccessToken == "" || data.RefreshToken == "" || data.ExpiresIn == 0 {
				return oauthCredentials{}, errors.New("Token exchange failed: response missing required fields")
			}
			expires := opts.now().UnixMilli() + data.ExpiresIn*1000 - tokenExpirySafety.Milliseconds()
			return oauthCredentials{Type: "oauth", Access: data.AccessToken, Refresh: data.RefreshToken, Expires: expires}, nil
		}

		finalStatus = resp.StatusCode
		shouldRetryHeader := resp.Header.Get("X-Should-Retry")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if shouldRetryHeader == "false" {
			return oauthCredentials{}, fmt.Errorf("Token exchange failed: HTTP %d", resp.StatusCode)
		}
		if attempt < maxTokenRetries && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) {
			delay := retryDelay(resp.Header.Get("Retry-After"), attempt)
			if err := opts.retrySleep(ctx, delay); err != nil {
				return oauthCredentials{}, err
			}
			continue
		}
		return oauthCredentials{}, fmt.Errorf("Token exchange failed: HTTP %d", resp.StatusCode)
	}

	return oauthCredentials{}, fmt.Errorf("Token exchange failed: HTTP %d", finalStatus)
}

func retryDelay(retryAfter string, attempt int) time.Duration {
	if retryAfter != "" {
		seconds, err := strconv.ParseFloat(strings.TrimSpace(retryAfter), 64)
		if err == nil && seconds >= 0 {
			delay := time.Duration(seconds * float64(time.Second))
			if delay > 30*time.Second {
				return 30 * time.Second
			}
			return delay
		}
	}
	return initialRetryDelay * time.Duration(1<<attempt)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func formatCallbackListenError(port int, err error) error {
	if errors.Is(err, syscall.EADDRINUSE) && port != 0 {
		return fmt.Errorf("OAuth callback port %d is already in use; retry with --port 0 for a random free port", port)
	}
	return fmt.Errorf("start OAuth callback server on %s:%d: %w", callbackHost, port, err)
}
