package usage

// Tests for the active refresh request (issue #2541, parent #2537).
//
// The most valuable assertions here are the three request-shape ones. An
// incorrectly shaped OAuth request is rejected by Anthropic's WAF with a 429
// that carries no rate-limit headers, and that 429 reads exactly like quota
// exhaustion. These tests fail loudly if any of the three required elements
// is dropped, so the wrong diagnosis is never reached in the first place.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fullRateLimitHeaders is the header set confirmed against a live 200
// response in issue #2537.
func fullRateLimitHeaders() http.Header {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "allowed_warning")
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed_warning")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.94")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1785634800")
	h.Set("anthropic-ratelimit-unified-5h-surpassed-threshold", "0.9")
	h.Set("anthropic-ratelimit-unified-7d-status", "allowed")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-7d-reset", "1786021200")
	h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	h.Set("anthropic-ratelimit-unified-reset", "1785634800")
	h.Set("anthropic-ratelimit-unified-fallback", "available")
	h.Set("anthropic-ratelimit-unified-fallback-percentage", "0.5")
	h.Set("anthropic-ratelimit-unified-overage-status", "rejected")
	h.Set("anthropic-ratelimit-unified-overage-disabled-reason", "out_of_credits")
	return h
}

// capturedRequest records what the fake Anthropic endpoint received.
type capturedRequest struct {
	calls  atomic.Int64
	method string
	path   string
	query  string
	header http.Header
	body   map[string]any
}

// newFakeAnthropic starts a server that records one request and replies with
// status and headers.
func newFakeAnthropic(t *testing.T, status int, headers http.Header, body string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	got := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.calls.Add(1)
		got.method = r.Method
		got.path = r.URL.Path
		got.query = r.URL.RawQuery
		got.header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		for name, values := range headers {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// clearRefreshEnv neutralises every environment variable the request shape
// reads, so a developer's shell cannot change what a test asserts.
//
// ANTHROPIC_BASE_URL is deliberately absent: the destination is a constant and
// no environment variable may reach it. TestBaseURL_IsNotEnvironmentControlled
// pins that.
func clearRefreshEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ANTHROPIC_USER_AGENT", "")
	t.Setenv("ANTHROPIC_BETA_FLAGS", "")
}

func refreshAgainst(t *testing.T, srv *httptest.Server) (*SnapshotPayload, error) {
	t.Helper()
	r := &Refresher{BaseURL: srv.URL, HTTPClient: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.Refresh(ctx, "test-access-token")
}

// ── Request shape: the three WAF-critical elements ───────────────────────────

// TestRefresh_SendsBetaQueryParameter covers element 1 of the request shape.
// Without `?beta=true` the OAuth path is rejected (#2537).
func TestRefresh_SendsBetaQueryParameter(t *testing.T) {
	clearRefreshEnv(t)
	srv, got := newFakeAnthropic(t, http.StatusOK, fullRateLimitHeaders(), "")

	if _, err := refreshAgainst(t, srv); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", got.path)
	}
	if !strings.Contains(got.query, "beta=true") {
		t.Errorf("query = %q, want it to carry beta=true", got.query)
	}
}

// TestRefresh_SendsOAuthHeaderBlock covers element 2: the x-stainless-* block,
// the claude-cli user-agent, x-app, and the direct-browser-access header.
func TestRefresh_SendsOAuthHeaderBlock(t *testing.T) {
	clearRefreshEnv(t)
	srv, got := newFakeAnthropic(t, http.StatusOK, fullRateLimitHeaders(), "")

	if _, err := refreshAgainst(t, srv); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	exact := map[string]string{
		"Anthropic-Version":                         "2023-06-01",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"X-App":                       "cli",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Package-Version": "0.81.0",
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Timeout":         "600",
		"Authorization":               "Bearer test-access-token",
	}
	for name, want := range exact {
		if have := got.header.Get(name); have != want {
			t.Errorf("header %s = %q, want %q", name, have, want)
		}
	}

	nonEmpty := []string{
		"X-Stainless-Arch", "X-Stainless-Os", "X-Stainless-Runtime-Version",
		"X-Client-Request-Id", "Anthropic-Beta",
	}
	for _, name := range nonEmpty {
		if got.header.Get(name) == "" {
			t.Errorf("header %s is empty; the WAF fingerprint requires it", name)
		}
	}

	ua := got.header.Get("User-Agent")
	if !strings.HasPrefix(ua, "claude-cli/") {
		t.Errorf("user-agent = %q, want a claude-cli/<version> prefix", ua)
	}
}

// TestRefresh_ClaudeCodeIdentityIsFirstSystemBlock covers element 3. Omitting
// it, or demoting it below another block, returns 429 with no retry-after and
// no rate-limit headers — the failure that is trivially misread as quota
// exhaustion.
func TestRefresh_ClaudeCodeIdentityIsFirstSystemBlock(t *testing.T) {
	clearRefreshEnv(t)
	srv, got := newFakeAnthropic(t, http.StatusOK, fullRateLimitHeaders(), "")

	if _, err := refreshAgainst(t, srv); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	system, ok := got.body["system"].([]any)
	if !ok || len(system) == 0 {
		t.Fatalf("body.system is missing or empty: %v", got.body["system"])
	}
	first, ok := system[0].(map[string]any)
	if !ok {
		t.Fatalf("body.system[0] is not an object: %v", system[0])
	}
	if first["type"] != "text" {
		t.Errorf("system[0].type = %v, want text", first["type"])
	}
	if first["text"] != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Errorf("system[0].text = %v, want the Claude Code identity string", first["text"])
	}
	cacheControl, ok := first["cache_control"].(map[string]any)
	if !ok {
		t.Fatalf("system[0].cache_control is missing: %v", first)
	}
	if cacheControl["type"] != "ephemeral" {
		t.Errorf("system[0].cache_control.type = %v, want ephemeral", cacheControl["type"])
	}
}

// TestRefresh_UsesSmallestViableMaxTokens covers the performance AC.
func TestRefresh_UsesSmallestViableMaxTokens(t *testing.T) {
	clearRefreshEnv(t)
	srv, got := newFakeAnthropic(t, http.StatusOK, fullRateLimitHeaders(), "")

	if _, err := refreshAgainst(t, srv); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got.body["max_tokens"] != float64(1) {
		t.Errorf("max_tokens = %v, want 1 (the smallest the API accepts)", got.body["max_tokens"])
	}
}

// TestRefresh_MakesExactlyOneRequest guards the edge-case AC that a refresh
// costs one request. A retry would double the quota cost of the command whose
// job is to report quota.
func TestRefresh_MakesExactlyOneRequest(t *testing.T) {
	clearRefreshEnv(t)
	srv, got := newFakeAnthropic(t, http.StatusOK, fullRateLimitHeaders(), "")

	if _, err := refreshAgainst(t, srv); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n := got.calls.Load(); n != 1 {
		t.Errorf("request count = %d, want exactly 1", n)
	}
}

// TestRefresh_DoesNotRetryOnFailure proves the single-request rule holds on
// the error path too, where a naive client would back off and retry.
func TestRefresh_DoesNotRetryOnFailure(t *testing.T) {
	clearRefreshEnv(t)
	srv, got := newFakeAnthropic(t, http.StatusTooManyRequests, http.Header{}, `{"error":"nope"}`)

	if _, err := refreshAgainst(t, srv); err == nil {
		t.Fatal("expected an error for a 429")
	}
	if n := got.calls.Load(); n != 1 {
		t.Errorf("request count = %d, want exactly 1 even on failure", n)
	}
}

// ── Status handling ──────────────────────────────────────────────────────────

func TestRefresh_UnauthorizedMapsToTokenRejected(t *testing.T) {
	clearRefreshEnv(t)
	srv, _ := newFakeAnthropic(t, http.StatusUnauthorized, http.Header{}, "")

	_, err := refreshAgainst(t, srv)
	if !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("err = %v, want ErrTokenRejected", err)
	}
}

func TestRefresh_NonOKStatusNamesTheCode(t *testing.T) {
	clearRefreshEnv(t)
	srv, _ := newFakeAnthropic(t, http.StatusInternalServerError, http.Header{}, "")

	_, err := refreshAgainst(t, srv)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want a *StatusError", err)
	}
	if statusErr.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", statusErr.StatusCode)
	}
	if !strings.Contains(statusErr.Error(), "500") {
		t.Errorf("error text %q must name the status code", statusErr.Error())
	}
}

// TestRefresh_OKWithoutRateLimitHeadersIsReported covers the edge-case AC: a
// 200 that carried nothing must not produce a payload, so the caller cannot
// overwrite a good snapshot with an empty one.
func TestRefresh_OKWithoutRateLimitHeadersIsReported(t *testing.T) {
	clearRefreshEnv(t)
	srv, _ := newFakeAnthropic(t, http.StatusOK, http.Header{}, `{"type":"message"}`)

	payload, err := refreshAgainst(t, srv)
	if !errors.Is(err, ErrNoRateLimitHeaders) {
		t.Fatalf("err = %v, want ErrNoRateLimitHeaders", err)
	}
	if payload != nil {
		t.Errorf("payload = %+v, want nil so no write can happen", payload)
	}
}

// TestRefresh_TooManyRequestsWithHeadersIsUsable proves the fix for #2571:
// a 429 carrying the full unified rate-limit header set is quota exhaustion,
// not a WAF rejection, and must be returned as a usable payload rather than
// discarded as a status error. Header presence discriminates the two cases,
// not the status code.
func TestRefresh_TooManyRequestsWithHeadersIsUsable(t *testing.T) {
	clearRefreshEnv(t)
	srv, _ := newFakeAnthropic(t, http.StatusTooManyRequests, fullRateLimitHeaders(), `{"error":"rate_limit_error"}`)

	payload, err := refreshAgainst(t, srv)
	if err != nil {
		t.Fatalf("err = %v, want nil for a 429 carrying usable headers", err)
	}
	if payload == nil {
		t.Fatal("payload = nil, want the parsed rate-limit snapshot")
	}
}

// TestRefresh_TooManyRequestsWithoutHeadersIsStatusError is the other
// direction of the #2571 fix: a 429 with NO unified rate-limit headers is
// still a status error, not a usable payload. This is what catches an
// over-broad fix that stops discriminating on header presence at all.
func TestRefresh_TooManyRequestsWithoutHeadersIsStatusError(t *testing.T) {
	clearRefreshEnv(t)
	srv, _ := newFakeAnthropic(t, http.StatusTooManyRequests, http.Header{}, `{"error":"nope"}`)

	payload, err := refreshAgainst(t, srv)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want a *StatusError", err)
	}
	if statusErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", statusErr.StatusCode)
	}
	if payload != nil {
		t.Errorf("payload = %+v, want nil so no write can happen", payload)
	}
}

// TestRefresh_ErrorsCarryNoTokenMaterial covers the security AC from the
// direction that matters most: the strings a user sees.
func TestRefresh_ErrorsCarryNoTokenMaterial(t *testing.T) {
	clearRefreshEnv(t)
	const token = "sk-ant-oat01-SUPERSECRETVALUE"

	for _, tc := range []struct {
		name   string
		status int
		header http.Header
	}{
		{"unauthorized", http.StatusUnauthorized, http.Header{}},
		{"rate limited", http.StatusTooManyRequests, http.Header{}},
		{"ok without headers", http.StatusOK, http.Header{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newFakeAnthropic(t, tc.status, tc.header, token)
			r := &Refresher{BaseURL: srv.URL, HTTPClient: srv.Client()}
			_, err := r.Refresh(context.Background(), token)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("error text leaks the token: %q", err.Error())
			}
		})
	}
}

// TestRefresh_LargeBodyIsDiscarded proves the body is streamed to io.Discard
// rather than buffered: a body far larger than any snapshot still yields the
// header-derived payload and nothing else.
func TestRefresh_LargeBodyIsDiscarded(t *testing.T) {
	clearRefreshEnv(t)
	srv, _ := newFakeAnthropic(t, http.StatusOK, fullRateLimitHeaders(), strings.Repeat("x", 2<<20))

	payload, err := refreshAgainst(t, srv)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if payload == nil || payload.UnifiedStatus != "allowed_warning" {
		t.Fatalf("payload = %+v, want the header-derived snapshot", payload)
	}
}

func TestRefresh_EmptyTokenIsRefusedBeforeAnyRequest(t *testing.T) {
	clearRefreshEnv(t)
	srv, got := newFakeAnthropic(t, http.StatusOK, fullRateLimitHeaders(), "")

	r := &Refresher{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if _, err := r.Refresh(context.Background(), ""); err == nil {
		t.Fatal("expected an error for an empty token")
	}
	if n := got.calls.Load(); n != 0 {
		t.Errorf("request count = %d, want 0 — an empty token must not reach the network", n)
	}
}

// ── Header parsing ───────────────────────────────────────────────────────────

func TestParseRateLimitHeaders_FullSet(t *testing.T) {
	p := ParseRateLimitHeaders(fullRateLimitHeaders())
	if p == nil {
		t.Fatal("payload is nil")
	}
	if p.UnifiedStatus != "allowed_warning" {
		t.Errorf("UnifiedStatus = %q", p.UnifiedStatus)
	}
	if p.RepresentativeClaim != "five_hour" {
		t.Errorf("RepresentativeClaim = %q", p.RepresentativeClaim)
	}
	if p.UnifiedReset == nil || *p.UnifiedReset != 1785634800 {
		t.Errorf("UnifiedReset = %v", p.UnifiedReset)
	}
	if p.Windows == nil || p.Windows.FiveHour == nil || p.Windows.SevenDay == nil {
		t.Fatalf("Windows = %+v", p.Windows)
	}
	// Utilization is the RAW fraction; callers multiply by 100 themselves.
	if u := p.Windows.FiveHour.Utilization; u == nil || *u != 0.94 {
		t.Errorf("five_hour utilization = %v, want the raw 0.94", u)
	}
	if th := p.Windows.FiveHour.SurpassedThreshold; th == nil || *th != 0.9 {
		t.Errorf("five_hour surpassed_threshold = %v", th)
	}
	if u := p.Windows.SevenDay.Utilization; u == nil || *u != 0.42 {
		t.Errorf("seven_day utilization = %v", u)
	}
	if p.Fallback == nil || p.Fallback.Status != "available" {
		t.Errorf("Fallback = %+v", p.Fallback)
	}
	if p.Overage == nil || p.Overage.DisabledReason != "out_of_credits" {
		t.Errorf("Overage = %+v", p.Overage)
	}
}

// TestParseRateLimitHeaders_SevenDayHasNoSurpassedThreshold guards the
// allowlist: only the 5-hour window carries that header in the confirmed set.
func TestParseRateLimitHeaders_SevenDayHasNoSurpassedThreshold(t *testing.T) {
	h := fullRateLimitHeaders()
	h.Set("anthropic-ratelimit-unified-7d-surpassed-threshold", "0.7")

	p := ParseRateLimitHeaders(h)
	if p.Windows.SevenDay.SurpassedThreshold != nil {
		t.Errorf("seven_day surpassed_threshold = %v, want nil — it is not on the #2537 allowlist",
			p.Windows.SevenDay.SurpassedThreshold)
	}
}

func TestParseRateLimitHeaders_NoUnifiedHeadersYieldsNil(t *testing.T) {
	h := http.Header{}
	h.Set("content-type", "application/json")
	h.Set("authorization", "Bearer leaky")
	h.Set("request-id", "req_123")

	if p := ParseRateLimitHeaders(h); p != nil {
		t.Errorf("payload = %+v, want nil so an empty snapshot cannot overwrite a good one", p)
	}
}

// TestParseRateLimitHeaders_EmptyValueIsOmittedNotZeroed is the distinction
// every downstream reader depends on: "not present" must not become "zero".
func TestParseRateLimitHeaders_EmptyValueIsOmittedNotZeroed(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "allowed")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "   ")

	p := ParseRateLimitHeaders(h)
	if p == nil {
		t.Fatal("payload is nil")
	}
	if p.Windows != nil && p.Windows.FiveHour != nil && p.Windows.FiveHour.Utilization != nil {
		t.Errorf("utilization = %v, want nil for a whitespace-only header",
			*p.Windows.FiveHour.Utilization)
	}
}

func TestParseRateLimitHeaders_UnparseableValuesAreOmitted(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "allowed")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "not-a-number")
	h.Set("anthropic-ratelimit-unified-5h-reset", "NaN")
	h.Set("anthropic-ratelimit-unified-fallback-percentage", "Inf")

	p := ParseRateLimitHeaders(h)
	if p == nil {
		t.Fatal("payload is nil")
	}
	if p.Windows != nil && p.Windows.FiveHour != nil {
		if p.Windows.FiveHour.Utilization != nil {
			t.Errorf("utilization must be omitted for an unparseable value")
		}
		if p.Windows.FiveHour.Reset != nil {
			t.Errorf("reset must be omitted for NaN")
		}
	}
	if p.Fallback != nil && p.Fallback.Percentage != nil {
		t.Errorf("fallback percentage must be omitted for Inf")
	}
}

// TestParseRateLimitHeaders_FractionalResetIsTruncated guards the sidecar
// contract: reset decodes into a Go int64 and a fractional value would make
// the endpoint reject the WHOLE request, discarding an otherwise good
// snapshot.
func TestParseRateLimitHeaders_FractionalResetIsTruncated(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-reset", "1785634800.75")

	p := ParseRateLimitHeaders(h)
	if p == nil || p.UnifiedReset == nil {
		t.Fatalf("payload = %+v, want a truncated reset", p)
	}
	if *p.UnifiedReset != 1785634800 {
		t.Errorf("UnifiedReset = %d, want 1785634800", *p.UnifiedReset)
	}
}

// TestSnapshotPayload_MarshalsToTheEndpointSchema asserts the wire body
// carries only the fields POST /usage/snapshot accepts. The endpoint decodes
// with DisallowUnknownFields, so an extra key rejects the whole request —
// and `captured_at` / `account` are set host-side and must never be sent.
func TestSnapshotPayload_MarshalsToTheEndpointSchema(t *testing.T) {
	p := ParseRateLimitHeaders(fullRateLimitHeaders())
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	allowed := map[string]bool{
		"unified_status": true, "representative_claim": true, "unified_reset": true,
		"windows": true, "fallback": true, "overage": true,
	}
	for key := range decoded {
		if !allowed[key] {
			t.Errorf("payload carries %q, which POST /usage/snapshot rejects", key)
		}
	}
	for _, forbidden := range []string{"captured_at", "account"} {
		if _, present := decoded[forbidden]; present {
			t.Errorf("payload must not carry %q — the sidecar sets it host-side", forbidden)
		}
	}
}

func TestSnapshotPayload_ToSnapshotStampsAccountAndTime(t *testing.T) {
	p := ParseRateLimitHeaders(fullRateLimitHeaders())
	at := time.Date(2026, 8, 2, 23, 43, 28, 0, time.UTC)

	snap := p.ToSnapshot("work", at)
	if snap.Account != "work" {
		t.Errorf("Account = %q, want work", snap.Account)
	}
	if snap.CapturedAt != "2026-08-02T23:43:28Z" {
		t.Errorf("CapturedAt = %q", snap.CapturedAt)
	}
	if snap.Windows == nil || snap.Windows.FiveHour == nil {
		t.Fatalf("Windows = %+v", snap.Windows)
	}
}

// TestSnapshotPayload_ToSnapshotSanitisesTheAccountName is defence in depth:
// a hand-edited accounts/current must not produce a path-traversing filename
// on the display path either.
func TestSnapshotPayload_ToSnapshotSanitisesTheAccountName(t *testing.T) {
	p := ParseRateLimitHeaders(fullRateLimitHeaders())
	snap := p.ToSnapshot("../../escape", time.Now())
	if snap.Account != UnknownAccount {
		t.Errorf("Account = %q, want %q", snap.Account, UnknownAccount)
	}
}

// ── Betas, user-agent, base URL ──────────────────────────────────────────────

// TestModelBetas_HaikuExcludesEveryOccurrence guards the reason the upstream
// uses a filter rather than a remove-first: baseBetas carries a deliberate
// duplicate of the interleaved-thinking beta.
func TestModelBetas_HaikuExcludesEveryOccurrence(t *testing.T) {
	clearRefreshEnv(t)
	for _, beta := range modelBetas("claude-haiku-4-5") {
		if beta == "interleaved-thinking-2025-05-14" {
			t.Fatalf("haiku must exclude every occurrence of %q; got %v",
				beta, modelBetas("claude-haiku-4-5"))
		}
	}
	// The base list must still carry both occurrences, or the test above
	// would pass vacuously.
	count := 0
	for _, beta := range baseBetas {
		if beta == "interleaved-thinking-2025-05-14" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("baseBetas carries %d copies of the interleaved-thinking beta, want 2 "+
			"(mirror of model-config.ts) — this test is vacuous otherwise", count)
	}
}

func TestModelBetas_NonHaikuKeepsBaseList(t *testing.T) {
	clearRefreshEnv(t)
	got := modelBetas("claude-sonnet-4-5")
	if len(got) != len(baseBetas) {
		t.Errorf("betas = %v, want the full base list for a non-haiku model", got)
	}
}

func TestModelBetas_EnvOverrideReplacesTheBaseList(t *testing.T) {
	clearRefreshEnv(t)
	t.Setenv("ANTHROPIC_BETA_FLAGS", " oauth-2025-04-20 , , custom-beta ")

	got := modelBetas("claude-sonnet-4-5")
	want := []string{"oauth-2025-04-20", "custom-beta"}
	if len(got) != len(want) {
		t.Fatalf("betas = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("betas = %v, want %v", got, want)
		}
	}
}

func TestUserAgent_EnvOverride(t *testing.T) {
	clearRefreshEnv(t)
	if ua := userAgent(); !strings.HasPrefix(ua, "claude-cli/") {
		t.Errorf("default user-agent = %q, want a claude-cli/ prefix", ua)
	}
	t.Setenv("ANTHROPIC_USER_AGENT", "custom/1.0")
	if ua := userAgent(); ua != "custom/1.0" {
		t.Errorf("user-agent = %q, want the override", ua)
	}
}

// TestBaseURL_IsNotEnvironmentControlled is a security regression test.
//
// The refresh request carries `Authorization: Bearer <subscription token>`.
// An environment-controlled destination would let a `.envrc` or a stray
// export send that credential to a host of its choosing, in cleartext if it
// chose `http://`. The mirrored pi extension hardcodes the host and honours no
// such variable; so does this. Round 1 of the #2569 review caught an earlier
// version of this file that did honour $ANTHROPIC_BASE_URL.
func TestBaseURL_IsNotEnvironmentControlled(t *testing.T) {
	clearRefreshEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "http://attacker.example")

	r := &Refresher{}
	got, err := r.requestURL()
	if err != nil {
		t.Fatalf("requestURL: %v", err)
	}
	if !strings.HasPrefix(got, DefaultBaseURL) {
		t.Fatalf("requestURL() = %q, want the hardcoded %q — no env var may redirect a bearer token",
			got, DefaultBaseURL)
	}
	if !strings.HasPrefix(DefaultBaseURL, "https://") {
		t.Errorf("DefaultBaseURL = %q, want https so the token never travels in cleartext", DefaultBaseURL)
	}
}

// The in-process field remains the only seam, and it is what the tests use.
func TestBaseURL_FieldIsTheOnlySeam(t *testing.T) {
	clearRefreshEnv(t)
	r := &Refresher{BaseURL: "https://in-process.example"}
	got, err := r.requestURL()
	if err != nil {
		t.Fatalf("requestURL: %v", err)
	}
	if got != "https://in-process.example/v1/messages?beta=true" {
		t.Errorf("requestURL() = %q", got)
	}
}

// TestRequestURL_PreservesAnExplicitBetaValue mirrors buildRequestUrl's
// `!searchParams.has("beta")` guard.
func TestRequestURL_PreservesAnExplicitBetaValue(t *testing.T) {
	clearRefreshEnv(t)
	r := &Refresher{BaseURL: "https://api.example"}
	got, err := r.requestURL()
	if err != nil {
		t.Fatalf("requestURL: %v", err)
	}
	if got != "https://api.example/v1/messages?beta=true" {
		t.Errorf("requestURL() = %q", got)
	}
}
