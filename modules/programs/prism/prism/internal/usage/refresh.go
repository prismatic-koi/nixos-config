// Active refresh of a missing or stale usage snapshot.
//
// Why this exists
// ---------------
//
// Passive capture only records a snapshot when a session actually
// talks to Anthropic. An account nobody has used recently therefore has no
// snapshot at all, and `prism account usage` shows nothing. This file makes
// ONE live `/v1/messages` request to fill that gap.
//
// The WAF trap
// ------------
//
// Anthropic's WAF fingerprints the Claude Code OAuth request shape. A request
// that omits any required element is rejected, and the rejection carries NO
// rate-limit headers. The rejection status is 429, which reads exactly like
// quota exhaustion and is not. If you see a 429 from this code, suspect the
// request shape first.
//
// Three elements are easy to miss, and all three are load-bearing:
//
//  1. the `?beta=true` query parameter on `/v1/messages`;
//  2. the `x-stainless-*` block, the `claude-cli/<version>` user-agent,
//     `x-app: cli`, and `anthropic-dangerous-direct-browser-access: true`;
//  3. the Claude Code identity string as the FIRST `system` block, with
//     `cache_control: {type: ephemeral}`.
//
// Mirror discipline
// -----------------
//
// The canonical implementation of the request shape is TypeScript, in the
// vendored `anthropic-oauth` pi extension. Go cannot import it, so this file
// is a hand-maintained mirror, in the same way `internal/account/login.go`
// mirrors `auth.ts`. Every constant below names the file and symbol it
// mirrors. When the extension changes, change this file in the same commit.
//
//	oauth-headers.ts  getUserAgent, getStainlessHeaders, buildRequestUrl,
//	                  buildOAuthHeaders
//	model-config.ts   config.ccVersion, config.baseBetas, config.modelOverrides
//	betas.ts          getModelBetas
//	stream.ts:84      CLAUDE_CODE_IDENTITY and its cache_control block
//	ratelimit.ts      parseUnifiedRateLimitHeaders
//
// Security
// --------
//
// The token reaches exactly one place: the `authorization` request header. It
// is never logged, never placed in an error, and never returned. The errors
// this file produces carry a status code or a fixed string, nothing else.
package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── Mirrored constants ───────────────────────────────────────────────────────

const (
	// DefaultBaseURL is the Anthropic API root.
	//
	// It is a CONSTANT, not an environment override. The value mirrors the
	// hardcoded `baseUrl` the pi extension registers
	// (anthropic-oauth/index.ts, pi.registerProvider), which honours no such
	// variable either.
	//
	// An environment-controlled destination here would be a token-exfiltration
	// lever: this request carries `Authorization: Bearer <subscription token>`,
	// so anything that sets the variable — a `.envrc`, a stray export — could
	// send a long-lived credential to a host of its choosing, in cleartext if
	// it chose `http://`. Refresher.BaseURL is the seam for tests; nothing
	// reads an env var to reach it.
	DefaultBaseURL = "https://api.anthropic.com"

	// messagesPath and betaQuery together form element 1 of the request
	// shape. Mirror of oauth-headers.ts::buildRequestUrl, which appends
	// `?beta=true` to `/v1/messages` and to nothing else.
	messagesPath = "/v1/messages"

	// anthropicVersion mirrors buildOAuthHeaders' `anthropic-version`.
	anthropicVersion = "2023-06-01"

	// ccVersion mirrors model-config.ts::config.ccVersion. It appears in the
	// user-agent, which the WAF inspects. Bump it when model-config.ts bumps.
	ccVersion = "2.1.257"

	// stainlessPackageVersion mirrors oauth-headers.ts::getStainlessHeaders'
	// `x-stainless-package-version`.
	stainlessPackageVersion = "0.81.0"

	// stainlessRuntimeVersion stands in for Node's `process.version`, which
	// getStainlessHeaders reads at request time. This process is Go, not
	// Node, so the value is a constant that mirrors the Node major the pi
	// runtime ships with (nodejs_24). It states a plausible runtime rather
	// than a truthful one; the header is part of the fingerprint block, and
	// omitting it is the failure mode that matters.
	stainlessRuntimeVersion = "v24.11.0"

	// claudeCodeIdentity mirrors stream.ts::CLAUDE_CODE_IDENTITY. This exact
	// string, as the first system block with an ephemeral cache_control, is
	// element 3 of the request shape. Omitting it returns 429 with no
	// retry-after and no rate-limit headers.
	claudeCodeIdentity = "You are Claude Code, Anthropic's official CLI for Claude."

	// refreshModelID is the model the probe names. Haiku is the cheapest
	// model on the subscription path, and the response is discarded, so the
	// only thing that matters is that the request is well-formed and
	// accepted.
	refreshModelID = "claude-haiku-4-5"

	// refreshMaxTokens is the smallest value the API accepts. The response
	// body is discarded — only the headers are wanted — so one token is one
	// too many already and cannot be reduced further.
	refreshMaxTokens = 1

	// refreshProbeText is the user turn. One character: the shortest input
	// that is still a valid non-empty text block.
	refreshProbeText = "."

	// DefaultTimeout bounds the whole refresh request. `prism account usage`
	// is an interactive command, so a wedged socket must not hold the
	// terminal.
	DefaultTimeout = 20 * time.Second

	// discardLimit caps how much of the response body is drained. The body is
	// never wanted, and a well-formed reply at max_tokens=1 is a few hundred
	// bytes, so this only bounds a pathological server.
	discardLimit = 1 << 20 // 1 MiB
)

// baseBetas mirrors model-config.ts::config.baseBetas. Order is part of the
// wire form: the header is this list, comma-joined.
var baseBetas = []string{
	"claude-code-20250219",
	"oauth-2025-04-20",
	"interleaved-thinking-2025-05-14",
	"prompt-caching-scope-2026-01-05",
	"context-management-2025-06-27",
	"advisor-tool-2026-03-01",
	"thinking-token-count-2026-05-13",
	"extended-cache-ttl-2025-04-11",
}

// haikuExcludedBetas mirrors model-config.ts::config.modelOverrides.haiku's
// `exclude` list. The override key is matched with a substring test against
// the lowercased model id, exactly as getModelOverride does.
var haikuExcludedBetas = []string{"effort-2025-11-24"}

// ── Errors ───────────────────────────────────────────────────────────────────

// StatusError reports a non-200 reply from the Anthropic API.
//
// It names the status code and nothing else. In particular it does not carry
// the response body: an error body can echo request material, and this type
// is printed straight to the user's terminal.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Anthropic API returned HTTP %d", e.StatusCode)
}

var (
	// ErrTokenRejected is returned for a 401. The stored token is expired or
	// revoked; the caller tells the user to log in again.
	ErrTokenRejected = errors.New("the stored access token was rejected (HTTP 401)")

	// ErrNoRateLimitHeaders is returned for a 200 that carried no usable
	// `anthropic-ratelimit-unified-*` header. The request succeeded but told
	// us nothing, so the existing snapshot must be left alone.
	ErrNoRateLimitHeaders = errors.New("the response carried no rate-limit headers")
)

// ── Wire payload ─────────────────────────────────────────────────────────────

// SnapshotPayload is the request body of POST /usage/snapshot, the sidecar
// endpoint. It is byte-for-byte the shape the pi extension
// sends (ratelimit.ts::RateLimitSnapshot) and the shape the sidecar accepts
// (internal/sidecar/host_api.go::usageSnapshotRequest).
//
// `captured_at` and `account` are deliberately absent. The sidecar sets both
// host-side, and it decodes with DisallowUnknownFields, so sending either
// would be rejected outright.
//
// The nested types are the persisted ones from usage.go. Their JSON tags are
// identical to the endpoint's schema, so reusing them keeps one definition of
// the wire names instead of two that can drift.
type SnapshotPayload struct {
	UnifiedStatus       string    `json:"unified_status,omitempty"`
	RepresentativeClaim string    `json:"representative_claim,omitempty"`
	UnifiedReset        *int64    `json:"unified_reset,omitempty"`
	Windows             *Windows  `json:"windows,omitempty"`
	Fallback            *Fallback `json:"fallback,omitempty"`
	Overage             *Overage  `json:"overage,omitempty"`
	// OrganizationID and WorkspaceID mirror `anthropic-organization-id` and
	// `anthropic-workspace-id` — see the field comment on usage.Snapshot.
	OrganizationID string `json:"organization_id,omitempty"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
}

// ToSnapshot builds the persisted object for display purposes only.
//
// The authoritative persisted copy is the one the sidecar writes. This method
// exists so the caller can render the freshly fetched numbers even when the
// POST to the sidecar could not be delivered — losing the display because the
// write path was unavailable would be the worst of both worlds.
func (p *SnapshotPayload) ToSnapshot(account string, capturedAt time.Time) Snapshot {
	return Snapshot{
		CapturedAt:          FormatCapturedAt(capturedAt),
		Account:             SanitizeAccountName(account),
		UnifiedStatus:       p.UnifiedStatus,
		RepresentativeClaim: p.RepresentativeClaim,
		UnifiedReset:        p.UnifiedReset,
		Windows:             p.Windows,
		Fallback:            p.Fallback,
		Overage:             p.Overage,
		OrganizationID:      p.OrganizationID,
		WorkspaceID:         p.WorkspaceID,
	}
}

// ── Header parsing ───────────────────────────────────────────────────────────

// ParseRateLimitHeaders builds a payload from a response's headers, or returns
// nil when the response carried no usable `anthropic-ratelimit-unified-*`
// header.
//
// Go mirror of ratelimit.ts::parseUnifiedRateLimitHeaders, including its two
// rules that matter downstream:
//
//   - An absent, empty, or unparseable header OMITS its field rather than
//     zero-filling it, so a reader can tell "not present" from "zero".
//   - A response whose unified headers are all absent or all unparseable
//     yields nil, not an empty payload. An information-free payload must
//     never overwrite a good snapshot.
//
// The header names read here are exactly the allowlist, plus
// `anthropic-organization-id` and `anthropic-workspace-id`. There is no bulk
// sweep of http.Header anywhere in this file — that
// shape would collect `authorization` along with everything else.
func ParseRateLimitHeaders(h http.Header) *SnapshotPayload {
	p := &SnapshotPayload{
		UnifiedStatus:       headerString(h, "anthropic-ratelimit-unified-status"),
		RepresentativeClaim: headerString(h, "anthropic-ratelimit-unified-representative-claim"),
		UnifiedReset:        headerUnixSeconds(h, "anthropic-ratelimit-unified-reset"),
	}

	fiveHour := parseWindowHeaders(h, "5h")
	sevenDay := parseWindowHeaders(h, "7d")
	if fiveHour != nil || sevenDay != nil {
		p.Windows = &Windows{FiveHour: fiveHour, SevenDay: sevenDay}
	}

	fallbackStatus := headerString(h, "anthropic-ratelimit-unified-fallback")
	fallbackPct := headerFloat(h, "anthropic-ratelimit-unified-fallback-percentage")
	if fallbackStatus != "" || fallbackPct != nil {
		p.Fallback = &Fallback{Status: fallbackStatus, Percentage: fallbackPct}
	}

	overageStatus := headerString(h, "anthropic-ratelimit-unified-overage-status")
	overageReason := headerString(h, "anthropic-ratelimit-unified-overage-disabled-reason")
	if overageStatus != "" || overageReason != "" {
		p.Overage = &Overage{Status: overageStatus, DisabledReason: overageReason}
	}

	if p.isEmpty() {
		return nil
	}

	// anthropic-organization-id and anthropic-workspace-id ride the same
	// response as the rate-limit headers above
	// but are not THEMSELVES rate-limit information, so they are read only
	// once the payload is known to carry real data — an org/workspace pair
	// with no usage data attached would be as meaningless as an empty
	// snapshot, and the isEmpty check above must stay the sole gate on
	// whether this response is worth persisting at all.
	p.OrganizationID = headerString(h, "anthropic-organization-id")
	p.WorkspaceID = headerString(h, "anthropic-workspace-id")
	return p
}

// isEmpty reports whether the payload carries no rate-limit information. It
// mirrors the sidecar's own isEmpty check, which rejects such a body with 400.
func (p *SnapshotPayload) isEmpty() bool {
	return p.UnifiedStatus == "" &&
		p.RepresentativeClaim == "" &&
		p.UnifiedReset == nil &&
		p.Windows == nil &&
		p.Fallback == nil &&
		p.Overage == nil
}

// parseWindowHeaders parses one window's headers. prefix is "5h" or "7d".
//
// Only the 5-hour window has a `surpassed-threshold` header in the confirmed
// set, so it is read for that prefix alone — mirroring
// ratelimit.ts::parseWindow. Reading it speculatively for 7d would put a
// header outside the allowlist into the snapshot.
func parseWindowHeaders(h http.Header, prefix string) *Window {
	base := "anthropic-ratelimit-unified-" + prefix
	w := &Window{
		Status:      headerString(h, base+"-status"),
		Utilization: headerFloat(h, base+"-utilization"),
		Reset:       headerUnixSeconds(h, base+"-reset"),
	}
	if prefix == "5h" {
		w.SurpassedThreshold = headerFloat(h, base+"-surpassed-threshold")
	}
	if w.Status == "" && w.Utilization == nil && w.Reset == nil && w.SurpassedThreshold == nil {
		return nil
	}
	return w
}

// headerString reads a header as a trimmed non-empty string, or "".
//
// A header present with a whitespace-only value counts as absent.
func headerString(h http.Header, name string) string {
	return strings.TrimSpace(h.Get(name))
}

// headerFloat reads a header as a finite float, or nil.
//
// The empty-value guard is load-bearing. Without it an empty header would
// parse as a real 0 and a reader could not tell it from a genuine zero
// utilisation. NaN and ±Inf are rejected for the same reason: they are not
// data, and persisting them would corrupt the snapshot for every reader.
//
// No range check is applied. The headers are documented as fractions from 0
// to 1, but clamping an out-of-range value would silently discard real data
// if Anthropic widens the range. Storing what the server said is the safer
// contract.
func headerFloat(h http.Header, name string) *float64 {
	raw := headerString(h, name)
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// headerUnixSeconds reads a header as integer unix seconds, or nil.
//
// The truncation matters: the sidecar decodes reset fields into a Go int64
// and REJECTS THE WHOLE REQUEST if a value carries a fractional part. Sending
// an integer keeps one odd header from discarding an otherwise good snapshot.
// Mirrors ratelimit.ts::readUnixSeconds, which does the same with Math.trunc.
func headerUnixSeconds(h http.Header, name string) *int64 {
	f := headerFloat(h, name)
	if f == nil {
		return nil
	}
	// Guard the float64 → int64 conversion: a value outside int64's range is
	// implementation-defined in Go, so reject it rather than store garbage.
	if *f > 1<<62 || *f < -(1<<62) {
		return nil
	}
	v := int64(*f)
	return &v
}

// ── Refresher ────────────────────────────────────────────────────────────────

// Refresher performs the single live request that refreshes a snapshot.
//
// The zero value is usable: BaseURL falls back to DefaultBaseURL, HTTPClient
// falls back to a client with DefaultTimeout, and ModelID falls back to
// refreshModelID.
type Refresher struct {
	// BaseURL is the API root, without a trailing slash. Empty means
	// DefaultBaseURL. This field is the ONLY way to point the refresh at a
	// different host, and only in-process code can set it — see the
	// DefaultBaseURL comment for why no environment variable may.
	BaseURL string
	// HTTPClient performs the request.
	HTTPClient *http.Client
	// ModelID names the model in the request body.
	ModelID string
}

// Refresh makes ONE request to `/v1/messages?beta=true` with the supplied
// bearer token and returns the rate-limit headers it carried.
//
// Exactly one request is issued. There is no retry: a retry would double the
// quota cost of a command whose whole purpose is to report quota, and the
// caller already degrades gracefully to stored data on any error.
//
// Errors:
//
//	ErrTokenRejected       HTTP 401 — the token is expired or revoked
//	*StatusError           a non-200, non-401 status with no usable
//	                       rate-limit header (e.g. a WAF rejection)
//	ErrNoRateLimitHeaders  HTTP 200 with no usable rate-limit header
//	other                  the request could not be built or sent
//
// A non-200, non-401 status WITH a usable rate-limit header (e.g. a
// quota-exhaustion 429) is not an error: the parsed payload is returned as
// normal, because header presence — not status — is what tells a genuine
// rate-limit response apart from a rejection with no usable headers.
//
// On every error path the stored snapshot must be left untouched by the
// caller. This function itself never writes anything.
func (r *Refresher) Refresh(ctx context.Context, token string) (*SnapshotPayload, error) {
	req, err := r.buildRequest(ctx, token)
	if err != nil {
		return nil, err
	}

	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		// A *url.Error carries the method and the URL. Neither holds the
		// token — it lives in a header, which url.Error does not reproduce.
		return nil, err
	}
	// AC: discard the body WITHOUT buffering it. io.Copy streams through a
	// small fixed buffer into io.Discard; nothing is retained. The body is
	// never parsed, so no error branch can echo it back to the user.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, discardLimit))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrTokenRejected
	}

	// Parse headers before branching on status. A quota-exhaustion error
	// response (e.g. 429) carries the same unified rate-limit header set as
	// a 200 — that is how a client learns when the limit resets — while a
	// WAF rejection or other non-200 carries none at all. Header presence,
	// not status, is what actually discriminates the two: a payload that
	// parses is used whatever the status; a non-200 with no parseable
	// payload still reports as a status error.
	payload := ParseRateLimitHeaders(resp.Header)
	if payload != nil {
		return payload, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{StatusCode: resp.StatusCode}
	}
	return nil, ErrNoRateLimitHeaders
}

// buildRequest assembles the request. It is separate from Refresh so the
// tests can assert on the wire shape — the three WAF-critical elements — with
// no server in the loop.
func (r *Refresher) buildRequest(ctx context.Context, token string) (*http.Request, error) {
	if token == "" {
		return nil, errors.New("usage: refresh needs an access token")
	}

	endpoint, err := r.requestURL()
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(r.requestBody())
	if err != nil {
		return nil, fmt.Errorf("usage: marshal refresh body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("usage: build refresh request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	applyOAuthHeaders(req.Header, token, r.modelID())
	return req, nil
}

// requestURL builds `<base>/v1/messages?beta=true`.
//
// Element 1 of the request shape. Mirror of oauth-headers.ts::buildRequestUrl,
// which sets the parameter only when it is absent, so an explicitly supplied
// value is preserved.
func (r *Refresher) requestURL() (string, error) {
	base := r.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimSuffix(base, "/") + messagesPath)
	if err != nil {
		return "", fmt.Errorf("usage: parse Anthropic base URL: %w", err)
	}
	q := u.Query()
	if !q.Has("beta") {
		q.Set("beta", "true")
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func (r *Refresher) modelID() string {
	if r.ModelID != "" {
		return r.ModelID
	}
	return refreshModelID
}

// requestBody builds the JSON body.
//
// Element 3 of the request shape lives here: claudeCodeIdentity is the FIRST
// system block and carries `cache_control: {type: ephemeral}`. Omitting it, or
// demoting it below another block, returns HTTP 429 with no retry-after and
// no rate-limit headers — the failure that reads as quota exhaustion and is
// not.
//
// `stream: true` mirrors pi: streamSimple is always SSE, so a non-streaming
// body would be a shape the OAuth path never otherwise sends. The body is
// discarded either way.
func (r *Refresher) requestBody() map[string]any {
	return map[string]any{
		"model":      r.modelID(),
		"max_tokens": refreshMaxTokens,
		"stream":     true,
		"system": []map[string]any{
			{
				"type":          "text",
				"text":          claudeCodeIdentity,
				"cache_control": map[string]any{"type": "ephemeral"},
			},
		},
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": []map[string]any{{"type": "text", "text": refreshProbeText}},
			},
		},
	}
}

// applyOAuthHeaders writes element 2 of the request shape onto h.
//
// Mirror of oauth-headers.ts::buildOAuthHeaders. Every header below is part
// of the fingerprint the WAF inspects; none is decorative.
//
// The token is written here and nowhere else in this package.
func applyOAuthHeaders(h http.Header, token, modelID string) {
	h.Set("authorization", "Bearer "+token)
	h.Set("anthropic-version", anthropicVersion)
	h.Set("anthropic-beta", strings.Join(modelBetas(modelID), ","))
	h.Set("anthropic-dangerous-direct-browser-access", "true")
	h.Set("x-app", "cli")
	h.Set("user-agent", userAgent())
	h.Set("x-client-request-id", uuid.NewString())
	h.Set("X-Claude-Code-Session-Id", sessionID)
	for name, value := range stainlessHeaders() {
		h.Set(name, value)
	}
}

// sessionID mirrors oauth-headers.ts's module-scope `sessionId`: one id per
// process, not per request.
var sessionID = uuid.NewString()

// cliVersion mirrors oauth-headers.ts::getCliVersion.
func cliVersion() string {
	if v := strings.TrimSpace(os.Getenv("ANTHROPIC_CLI_VERSION")); v != "" {
		return v
	}
	return ccVersion
}

// userAgent mirrors oauth-headers.ts::getUserAgent, including the
// $ANTHROPIC_USER_AGENT override.
func userAgent() string {
	if v := strings.TrimSpace(os.Getenv("ANTHROPIC_USER_AGENT")); v != "" {
		return v
	}
	return "claude-cli/" + cliVersion() + " (external, sdk-cli)"
}

// stainlessHeaders mirrors oauth-headers.ts::getStainlessHeaders.
//
// The upstream reads Node's process.arch / process.platform / process.version
// at request time. This process is Go, so arch and OS are translated from
// GOARCH and GOOS into the strings Node would report, and the runtime version
// is the constant documented above.
//
// Translation table for GOARCH → process.arch: Node calls x86-64 "x64" where
// Go calls it "amd64"; the two agree on "arm64". Every other value passes
// through, matching getStainlessHeaders' own passthrough branch.
func stainlessHeaders() map[string]string {
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "MacOS"
	}
	return map[string]string{
		"x-stainless-arch":            arch,
		"x-stainless-lang":            "js",
		"x-stainless-os":              osName,
		"x-stainless-package-version": stainlessPackageVersion,
		"x-stainless-retry-count":     "0",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": stainlessRuntimeVersion,
		"x-stainless-timeout":         "600",
	}
}

// modelBetas mirrors betas.ts::getModelBetas for the single case this file
// needs: the base list, the $ANTHROPIC_BETA_FLAGS override, and the
// per-model exclude list.
//
// The exclusion uses a filter over every occurrence, not a remove-first, for
// the same reason the upstream does: $ANTHROPIC_BETA_FLAGS is user-supplied
// and can name the same beta twice, and removing only the first would leave
// the second on the wire.
//
// The add-overrides upstream carries for opus-4-5 / 4-6 / 4-7 are not
// mirrored. This path only ever requests refreshModelID, which is a haiku.
//
// The excluded-beta cache and the long-context peel-off loop in betas.ts are
// deliberately not mirrored. Both exist to recover from a long-context error
// across successive requests; this path makes one request and never retries.
func modelBetas(modelID string) []string {
	betas := requiredBetas()
	lower := strings.ToLower(modelID)
	if strings.Contains(lower, "haiku") {
		betas = filterOut(betas, haikuExcludedBetas)
	}
	return betas
}

// requiredBetas mirrors betas.ts::getRequiredBetas: $ANTHROPIC_BETA_FLAGS
// replaces the base list wholesale when set, split on commas with empty
// entries dropped.
func requiredBetas() []string {
	raw := os.Getenv("ANTHROPIC_BETA_FLAGS")
	if strings.TrimSpace(raw) == "" {
		out := make([]string, len(baseBetas))
		copy(out, baseBetas)
		return out
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// filterOut returns items with every element of drop removed.
func filterOut(items, drop []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		excluded := false
		for _, d := range drop {
			if item == d {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, item)
		}
	}
	return out
}

// ── Shared request shape (exported seam) ─────────────────────────────────────
//
// The three elements of the WAF-critical request shape are described at the
// top of this file. They are needed by every caller that talks to
// `/v1/messages` on the Claude Code OAuth path, not only by the usage
// refresh — internal/titlegen is the second such caller.
//
// The seam is deliberately THIN: three symbols that expose the shape, and
// nothing that exposes a token, a body, or a response. The mirror discipline
// stated at the top of this file still applies and still has one home — when
// the pi extension changes the shape, this file changes, and every caller
// gets the new shape without a second edit. Do NOT copy any of the mirrored
// constants into another package: a second copy is a second thing to forget,
// and the failure mode is a 429 that reads as quota exhaustion.

// ClaudeCodeIdentity is element 3 of the request shape: this exact string,
// as the FIRST system block, with `cache_control: {type: ephemeral}`.
//
// A caller that adds its own system blocks must append them AFTER this one.
// Demoting it returns HTTP 429 with no retry-after and no rate-limit
// headers.
const ClaudeCodeIdentity = claudeCodeIdentity

// MessagesURL builds `<base>/v1/messages?beta=true` — element 1 of the
// request shape. An empty base means DefaultBaseURL.
//
// The `?beta=true` parameter is not optional and not decorative: omitting it
// is one of the three ways to be rejected by the WAF with a misleading 429.
func MessagesURL(base string) (string, error) {
	r := Refresher{BaseURL: base}
	return r.requestURL()
}

// ApplyOAuthHeaders writes element 2 of the request shape onto h: the
// `x-stainless-*` block, the `claude-cli/<version>` user-agent, `x-app`,
// `anthropic-dangerous-direct-browser-access`, `anthropic-version`, the
// per-model `anthropic-beta` list, and the bearer token.
//
// modelID selects the beta list (haiku excludes the effort beta), so pass
// the model the request body actually names.
//
// The token is written to the `authorization` header and to nothing else.
// This function does not log, retain, or return it.
func ApplyOAuthHeaders(h http.Header, token, modelID string) {
	applyOAuthHeaders(h, token, modelID)
}
