package podmanproxy

// Cycle-7 / #2324 tests for the container-name auto-prefix policy.
//
// The policy lives in policy.go::applyContainerNamePolicy and is gated
// on Config.ContainerNamePrefix. These tests cover the four documented
// branches:
//
//   1. Prefix empty (back-compat) — body forwards unchanged.
//   2. Prefix non-empty, Name absent — proxy injects an auto-name.
//   3. Prefix non-empty, Name explicitly empty (""), same as absent.
//   4. Prefix non-empty, Name set without the prefix — 403 deny.
//   5. Prefix non-empty, Name set with the correct prefix — forward
//      unchanged.
//
// The injection tests verify the rewritten body on the UPSTREAM side
// (via fakeUpstream.lastBody) so the round-trip from policy decision
// through handler body-restore to upstream forward is end-to-end
// covered. Per the cycle-6 schema-inversion discipline (see
// policy.go top-of-file comment), value-level checks added to an
// already-admitted schema field need a revert-and-watch-fail test:
// the matching "explicit Name without prefix" deny case is the
// load-bearing assertion for that discipline.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// startProxyWithPrefix stands up a proxy harness identical to
// startProxy but with the supplied ContainerNamePrefix wired in.
// Other policy fields are left at their defaults; bind sources are
// empty because the cycle-7 tests do not exercise HostConfig at all
// (every test body is `{"Image": "alpine"}` only).
func startProxyWithPrefix(t *testing.T, fu *fakeUpstream, prefix string) *proxyHarness {
	t.Helper()
	dir := shortSocketDir(t)
	listenPath := filepath.Join(dir, "proxy.sock")
	auditBuf := &bytes.Buffer{}
	cfg := Config{
		ListenerPath:        listenPath,
		UpstreamPath:        fu.sockPath,
		ContainerNamePrefix: prefix,
		AuditWriter:         auditBuf,
	}
	startProxyWithConfig(t, cfg, auditBuf, listenPath)
	return &proxyHarness{
		sock:  listenPath,
		audit: auditBuf,
	}
}

// postCreateRaw posts an arbitrary JSON body to /containers/create.
// Unlike postCreate (which fabricates an Image+HostConfig shape), this
// helper lets the cycle-7 tests express body shape directly so
// "Name absent" vs "Name empty string" vs "Name with value" are
// expressible as plain maps.
func postCreateRaw(t *testing.T, sock string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/containers/create",
		bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(t, sock, req)
}

// readUpstreamName parses fu.lastBody as JSON and returns the Name
// field (or "" if absent). Used by the auto-inject assertions to
// prove the rewritten body reached the upstream verbatim.
func readUpstreamName(t *testing.T, fu *fakeUpstream) string {
	t.Helper()
	raw, _ := fu.lastBody.Load().([]byte)
	if len(raw) == 0 {
		t.Fatalf("upstream received no body")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal upstream body: %v (raw=%q)", err, raw)
	}
	if v, ok := obj["Name"].(string); ok {
		return v
	}
	return ""
}

// ── back-compat: empty prefix is a no-op ─────────────────────────────────

// TestNamePrefix_EmptyPrefix_NoOp verifies the back-compat path:
// when ContainerNamePrefix is empty the proxy does NOT inject a Name
// and does NOT reject a body that omits Name. This is the contract
// for out-of-tree callers that construct the proxy without session
// scoping.
func TestNamePrefix_EmptyPrefix_NoOp(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithPrefix(t, fu, "")
	resp := postCreateRaw(t, h.sock, map[string]any{
		"Image": "alpine",
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if got := readUpstreamName(t, fu); got != "" {
		t.Errorf("upstream Name: got %q, want empty (no injection when prefix is empty)", got)
	}
}

// ── inject branches: Name absent / Name="" ──────────────────────────────

// TestNamePrefix_MissingName_Injects verifies that when the body has
// no Name field at all, the proxy injects an auto-generated name of
// the form prefix + 8 hex chars and forwards the rewritten body.
func TestNamePrefix_MissingName_Injects(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@cycle7-"
	h := startProxyWithPrefix(t, fu, prefix)
	resp := postCreateRaw(t, h.sock, map[string]any{
		"Image": "alpine",
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	got := readUpstreamName(t, fu)
	if !strings.HasPrefix(got, prefix) {
		t.Errorf("upstream Name %q does not start with %q", got, prefix)
	}
	// The injected suffix must be exactly 8 hex chars from crypto/rand
	// so the cleanup sweep can target the strict form
	// ^prism-<session>-[a-f0-9]{8}$. (Step 7 of #2317.)
	suffix := strings.TrimPrefix(got, prefix)
	if len(suffix) != 8 {
		t.Errorf("injected suffix %q has length %d, want 8", suffix, len(suffix))
	}
	for _, c := range suffix {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("injected suffix %q is not lowercase hex (offending char %q)", suffix, string(c))
			break
		}
	}
}

// TestNamePrefix_EmptyName_Injects verifies that an explicit empty
// string Name="" is treated identically to a missing Name field —
// both trigger the inject branch.
func TestNamePrefix_EmptyName_Injects(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@cycle7-"
	h := startProxyWithPrefix(t, fu, prefix)
	resp := postCreateRaw(t, h.sock, map[string]any{
		"Image": "alpine",
		"Name":  "",
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if got := readUpstreamName(t, fu); !strings.HasPrefix(got, prefix) {
		t.Errorf("upstream Name %q does not start with %q", got, prefix)
	}
}

// TestNamePrefix_Injects_PreservesOtherFields verifies that the
// rewritten body preserves every other top-level field the agent
// sent verbatim. The Image and a small HostConfig (with no policy
// violations) round-trip through the upstream unchanged.
func TestNamePrefix_Injects_PreservesOtherFields(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@roundtrip-"
	h := startProxyWithPrefix(t, fu, prefix)
	resp := postCreateRaw(t, h.sock, map[string]any{
		"Image": "alpine:3.20",
		"Cmd":   []string{"echo", "hi"},
		"Labels": map[string]string{
			"prism.test": "true",
		},
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	raw, _ := fu.lastBody.Load().([]byte)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	if obj["Image"] != "alpine:3.20" {
		t.Errorf("Image: got %v, want alpine:3.20", obj["Image"])
	}
	cmd, _ := obj["Cmd"].([]any)
	if len(cmd) != 2 || cmd[0] != "echo" || cmd[1] != "hi" {
		t.Errorf("Cmd: got %v, want [echo hi]", obj["Cmd"])
	}
	labels, _ := obj["Labels"].(map[string]any)
	if labels["prism.test"] != "true" {
		t.Errorf("Labels[prism.test]: got %v, want \"true\"", labels["prism.test"])
	}
}

// TestNamePrefix_Injects_UniqueAcrossCalls verifies that two
// consecutive auto-inject calls do not collide on the suffix. This
// guards against an accidental seeded-once stateful generator that
// would produce the same value on every call.
func TestNamePrefix_Injects_UniqueAcrossCalls(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@uniq-"
	h := startProxyWithPrefix(t, fu, prefix)
	post := func() string {
		t.Helper()
		resp := postCreateRaw(t, h.sock, map[string]any{"Image": "alpine"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("post: status %d", resp.StatusCode)
		}
		return readUpstreamName(t, fu)
	}
	a := post()
	b := post()
	if a == b {
		t.Errorf("two consecutive auto-inject calls produced the SAME name %q — randomness is not wired", a)
	}
}

// ── deny branch: explicit Name without the configured prefix ─────────────

// TestNamePrefix_NonMatchingName_Denied verifies the security AC: a
// request with an explicit Name that does not start with the
// configured prefix is rejected with 403 and the audit log records
// the reason "name_prefix_mismatch".
//
// This is the load-bearing test for the cycle-7 value-level check.
// Per the cycle-6 discipline (see policy.go), value-level checks on
// already-admitted fields must be paired with a test that fails when
// the production check is removed — see the matching revert-watch
// exercise in TestNamePrefix_NonMatchingName_Denied_RevertGuard.
func TestNamePrefix_NonMatchingName_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@deny-"
	h := startProxyWithPrefix(t, fu, prefix)
	resp := postCreateRaw(t, h.sock, map[string]any{
		"Image": "alpine",
		"Name":  "evil-container",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}
	env := readEnvelope(t, resp)
	if !strings.Contains(env.Message, "evil-container") {
		t.Errorf("error message should name the rejected value, got %q", env.Message)
	}
	if !strings.Contains(env.Message, prefix) {
		t.Errorf("error message should name the required prefix, got %q", env.Message)
	}
	assertNoForward(t, fu)

	// Audit log must record the structured reason. The audit
	// envelope is one JSON line per request — we read the last line
	// and check its `reason` field. The cycle-7 v2 implementation
	// distinguishes body-side from query-side mismatches in the
	// audit so an operator can tell which channel the agent used;
	// the body-side reason is `name_prefix_mismatch_body`.
	if got := lastAuditReason(t, h.audit); got != "name_prefix_mismatch_body" {
		t.Errorf("audit reason: got %q, want \"name_prefix_mismatch_body\"", got)
	}
}

// TestNamePrefix_MatchingName_Forwarded verifies the positive control
// for the explicit-Name path: a body whose Name correctly starts with
// the configured prefix forwards unchanged. This proves the deny in
// the test above is not a no-op resulting from some unrelated 403
// path firing first.
func TestNamePrefix_MatchingName_Forwarded(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@allow-"
	h := startProxyWithPrefix(t, fu, prefix)
	explicitName := prefix + "explicit-suffix"
	resp := postCreateRaw(t, h.sock, map[string]any{
		"Image": "alpine",
		"Name":  explicitName,
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if got := readUpstreamName(t, fu); got != explicitName {
		t.Errorf("upstream Name: got %q, want %q (no rewrite for correctly-prefixed Name)", got, explicitName)
	}
}

// TestNamePrefix_NonMatchingName_Denied_RevertGuard is the
// revert-and-watch-fail discipline check for the cycle-7 name-prefix
// policy. The test temporarily replaces randomHexSuffix with a panic
// stub so the inject branch CANNOT silently accept a non-matching
// Name; if the production prefix check is ever removed, the body
// would fall through to the inject branch and panic instead of
// returning 403. Either way the test fails — proving the assertion
// is not a no-op.
//
// This complements the load-bearing assertion in
// TestNamePrefix_NonMatchingName_Denied above.
func TestNamePrefix_NonMatchingName_Denied_RevertGuard(t *testing.T) {
	origGen := randomHexSuffix
	randomHexSuffix = func(_ int) (string, error) {
		// If the production code reaches this on a non-matching
		// Name, the prefix check has been removed.
		panic("randomHexSuffix called when explicit non-matching Name was supplied; the prefix check has been removed")
	}
	t.Cleanup(func() { randomHexSuffix = origGen })

	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@revert-"
	h := startProxyWithPrefix(t, fu, prefix)
	resp := postCreateRaw(t, h.sock, map[string]any{
		"Image": "alpine",
		"Name":  "not-our-prefix-container",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403 (the prefix check did not fire)", resp.StatusCode)
	}
}

// ── round-2 fixes: ?name= query parameter on /containers/create ──────────
//
// review-security and review-context both flagged that
// applyContainerNamePolicy on round 1 inspected only the body Name,
// not the URL query. Docker-compat clients (and `docker run --name`
// against DOCKER_HOST=podman.sock, which Step 4 of #2317 wires into
// the bwrap sandbox) set the name via `?name=` with NO body Name. The
// fix inspects both channels, and on the inject branch writes the
// auto-generated name into BOTH the body Name AND the URL query so
// the upstream sees a consistent name regardless of which it reads.

// postCreateRawWithQuery is the same as postCreateRaw but lets the
// test set arbitrary URL-query parameters on the request — used by
// the round-2 query-name tests. body=nil sends `{}` so the upstream
// (when reached) gets a valid empty JSON object.
func postCreateRawWithQuery(t *testing.T, sock string, query url.Values, body any) *http.Response {
	t.Helper()
	var reqBody []byte
	if body == nil {
		reqBody = []byte("{}")
	} else {
		var err error
		reqBody, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	urlStr := "http://podman.sock/v1.41/containers/create"
	if len(query) > 0 {
		urlStr += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodPost, urlStr, bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(t, sock, req)
}

// readUpstreamRequest captures the most-recent request the fake
// upstream observed and returns its rendered URL.RawQuery plus the
// captured body bytes. Used by the inject-into-both tests to assert
// the upstream sees the auto-generated name on both channels.
func readUpstreamRequest(t *testing.T, fu *fakeUpstream) (rawQuery string, body []byte) {
	t.Helper()
	body, _ = fu.lastBody.Load().([]byte)
	rawQuery, _ = fu.lastRawQuery.Load().(string)
	return rawQuery, body
}

// TestNamePrefix_QueryName_NonMatching_Denied verifies the round-2
// security fix: a request with `?name=<not-prefixed>` is rejected
// with 403 + audit reason `name_prefix_mismatch_query`. The body has
// no Name field — only the query carries a (mismatching) value, so
// without the round-2 fix this case would silently fall through into
// the inject branch and end up with a docker-compat container named
// after the query value instead of the injected body Name.
func TestNamePrefix_QueryName_NonMatching_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@query-deny-"
	h := startProxyWithPrefix(t, fu, prefix)
	query := url.Values{"name": []string{"evil-orphan"}}
	resp := postCreateRawWithQuery(t, h.sock, query, map[string]any{"Image": "alpine"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}
	env := readEnvelope(t, resp)
	if !strings.Contains(env.Message, "evil-orphan") {
		t.Errorf("error message should name the rejected query value, got %q", env.Message)
	}
	assertNoForward(t, fu)
	if got := lastAuditReason(t, h.audit); got != "name_prefix_mismatch_query" {
		t.Errorf("audit reason: got %q, want \"name_prefix_mismatch_query\"", got)
	}
}

// TestNamePrefix_QueryName_Matching_Allowed verifies the positive
// control: a request with `?name=<correctly-prefixed>` is allowed
// and forwarded without rewriting either channel.
func TestNamePrefix_QueryName_Matching_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@query-allow-"
	h := startProxyWithPrefix(t, fu, prefix)
	want := prefix + "explicit-suffix"
	query := url.Values{"name": []string{want}}
	resp := postCreateRawWithQuery(t, h.sock, query, map[string]any{"Image": "alpine"})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, b)
	}
	gotQuery, _ := readUpstreamRequest(t, fu)
	gotName := url.Values{}
	if parsed, err := url.ParseQuery(gotQuery); err == nil {
		gotName = parsed
	}
	if gotName.Get("name") != want {
		t.Errorf("upstream ?name=: got %q, want %q (must forward unchanged on the matching path)",
			gotName.Get("name"), want)
	}
}

// TestNamePrefix_QueryName_Inject_RewritesBothChannels verifies the
// load-bearing belt-and-braces injection: when neither the body Name
// nor the `?name=` query carry a value, the proxy injects the
// auto-generated `prefix + 8 hex chars` into BOTH the body AND the
// URL query. The upstream test stub captures both channels and the
// assertion is that they carry the SAME injected value.
//
// This is the cycle-7 round-2 fix for the docker-compat path:
// without the query-side injection, docker-compat would either pick
// the empty query (and generate a random name like
// "interesting_curie") or honour the query over the body and end up
// with the agent's empty name. Either way the cleanup sweep filter
// would miss the orphan container.
func TestNamePrefix_QueryName_Inject_RewritesBothChannels(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@inject-both-"
	h := startProxyWithPrefix(t, fu, prefix)
	resp := postCreateRawWithQuery(t, h.sock, nil, map[string]any{"Image": "alpine"})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, b)
	}
	gotQuery, gotBody := readUpstreamRequest(t, fu)

	// Body Name must be set to a prefixed 8-hex name.
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	bodyName, _ := obj["Name"].(string)
	if !strings.HasPrefix(bodyName, prefix) {
		t.Errorf("upstream body Name %q does not start with prefix %q", bodyName, prefix)
	}

	// Query ?name= must be set to the SAME value as the body Name.
	parsed, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parse upstream query %q: %v", gotQuery, err)
	}
	queryName := parsed.Get("name")
	if queryName == "" {
		t.Errorf("upstream ?name= was empty after inject (round-2 fix not wired); query=%q", gotQuery)
	}
	if queryName != bodyName {
		t.Errorf("upstream body Name %q != upstream ?name=%q (inject must write the SAME value to both channels)",
			bodyName, queryName)
	}
}

// TestNamePrefix_QueryName_Inject_OverwritesEmptyQueryName covers the
// edge case where the agent sends `?name=` with no value (a literal
// empty string). The proxy must treat this the same as "absent" and
// inject; the upstream must not see the original empty `?name=`.
func TestNamePrefix_QueryName_Inject_OverwritesEmptyQueryName(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@empty-query-"
	h := startProxyWithPrefix(t, fu, prefix)
	query := url.Values{"name": []string{""}}
	resp := postCreateRawWithQuery(t, h.sock, query, map[string]any{"Image": "alpine"})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, b)
	}
	gotQuery, _ := readUpstreamRequest(t, fu)
	parsed, _ := url.ParseQuery(gotQuery)
	name := parsed.Get("name")
	if !strings.HasPrefix(name, prefix) {
		t.Errorf("empty ?name= must be overwritten by inject; got %q", name)
	}
}

// TestNamePrefix_QueryName_BodyName_Both_Allowed verifies that when
// both the body Name and the ?name= query are set with correctly-
// prefixed values, the request is allowed (we do not enforce strict
// equality across channels because the cleanup security guarantee
// holds either way: any name on the resulting container is
// prefix-anchored and the sweep regex finds it).
func TestNamePrefix_QueryName_BodyName_Both_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@both-set-"
	h := startProxyWithPrefix(t, fu, prefix)
	query := url.Values{"name": []string{prefix + "queryname"}}
	resp := postCreateRawWithQuery(t, h.sock, query, map[string]any{
		"Image": "alpine",
		"Name":  prefix + "bodyname",
	})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, b)
	}
	// The upstream sees BOTH values as-supplied; no rewrite.
	gotQuery, gotBody := readUpstreamRequest(t, fu)
	parsed, _ := url.ParseQuery(gotQuery)
	if got := parsed.Get("name"); got != prefix+"queryname" {
		t.Errorf("upstream ?name=: got %q, want %q", got, prefix+"queryname")
	}
	var obj map[string]any
	_ = json.Unmarshal(gotBody, &obj)
	if got, _ := obj["Name"].(string); got != prefix+"bodyname" {
		t.Errorf("upstream body Name: got %q, want %q", got, prefix+"bodyname")
	}
}

// TestNamePrefix_QueryName_NonMatching_Denied_RevertGuard is the
// revert-and-watch-fail discipline check for the round-2 query-name
// inspection. Replaces randomHexSuffix with a panic stub so if the
// production code stops inspecting `?name=` and falls into the
// inject branch on a non-matching query value, the panic surfaces.
func TestNamePrefix_QueryName_NonMatching_Denied_RevertGuard(t *testing.T) {
	origGen := randomHexSuffix
	randomHexSuffix = func(_ int) (string, error) {
		panic("randomHexSuffix called when explicit non-matching ?name= was supplied; the query prefix check has been removed")
	}
	t.Cleanup(func() { randomHexSuffix = origGen })

	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@query-revert-"
	h := startProxyWithPrefix(t, fu, prefix)
	query := url.Values{"name": []string{"not-our-prefix-container"}}
	resp := postCreateRawWithQuery(t, h.sock, query, map[string]any{"Image": "alpine"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403 (the query prefix check did not fire)", resp.StatusCode)
	}
}

// ── round-2 fixes: POST /containers/{id}/rename inspected ────────────────
//
// review-security flagged that POST /containers/{id}/rename was
// classified as endpointAllow with no inspection — a one-call escape
// from the auto-prefix policy. The fix reclassifies it as
// endpointPolicyRename and validates the `?name=` query against the
// configured ContainerNamePrefix.

// postRename is a tiny helper for the rename endpoint. The endpoint
// has no body per docker spec, so the request body is empty.
func postRename(t *testing.T, sock, containerID string, query url.Values) *http.Response {
	t.Helper()
	urlStr := "http://podman.sock/v1.41/containers/" + containerID + "/rename"
	if len(query) > 0 {
		urlStr += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodPost, urlStr, nil)
	if err != nil {
		t.Fatalf("new rename request: %v", err)
	}
	return doRequest(t, sock, req)
}

// TestRename_NonMatchingName_Denied verifies that a rename request
// whose `?name=` does not start with the configured prefix is
// rejected with 403 + audit reason `rename_prefix_mismatch`. Without
// this fix an agent could create a correctly-prefixed container and
// then immediately rename it out of the prefix, leaving an orphan.
func TestRename_NonMatchingName_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@rename-deny-"
	h := startProxyWithPrefix(t, fu, prefix)
	resp := postRename(t, h.sock, "abc123", url.Values{"name": []string{"evil-name"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}
	env := readEnvelope(t, resp)
	if !strings.Contains(env.Message, "evil-name") {
		t.Errorf("error message should name the rejected value, got %q", env.Message)
	}
	assertNoForward(t, fu)
	if got := lastAuditReason(t, h.audit); got != "rename_prefix_mismatch" {
		t.Errorf("audit reason: got %q, want \"rename_prefix_mismatch\"", got)
	}
}

// TestRename_MatchingName_Allowed verifies the positive control: a
// rename to a value that does start with the prefix is forwarded.
// Proves the deny above is not a no-op resulting from an unrelated
// 4xx path firing first.
func TestRename_MatchingName_Allowed(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@rename-allow-"
	h := startProxyWithPrefix(t, fu, prefix)
	want := prefix + "newname"
	resp := postRename(t, h.sock, "abc123", url.Values{"name": []string{want}})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, b)
	}
	if fu.captured() != 1 {
		t.Errorf("upstream request count: got %d, want 1", fu.captured())
	}
}

// TestRename_MissingName_Denied verifies that a rename with no
// `?name=` query parameter is rejected with 400 (not 403 — the
// upstream would also reject; the proxy synthesises a 4xx with a
// useful message instead).
func TestRename_MissingName_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@rename-missing-"
	h := startProxyWithPrefix(t, fu, prefix)
	resp := postRename(t, h.sock, "abc123", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	assertNoForward(t, fu)
	if got := lastAuditReason(t, h.audit); got != "rename_missing_name" {
		t.Errorf("audit reason: got %q, want \"rename_missing_name\"", got)
	}
}

// TestRename_EmptyPrefix_NoOp verifies the back-compat branch: when
// ContainerNamePrefix is empty (out-of-tree caller), rename forwards
// unchanged regardless of the `?name=` value. This matches the
// containers/create back-compat path.
func TestRename_EmptyPrefix_NoOp(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithPrefix(t, fu, "")
	resp := postRename(t, h.sock, "abc123", url.Values{"name": []string{"anything-goes"}})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q) — empty prefix should be a no-op", resp.StatusCode, b)
	}
}

// TestRename_NonMatching_Denied_RevertGuard is the revert-and-watch-
// fail discipline check for the rename inspector. The test
// re-classifies the endpoint by name and asserts the deny path:
// if the rename inspector is removed the test will fail because the
// upstream stub records a forward (assertNoForward catches it).
func TestRename_NonMatching_Denied_RevertGuard(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@rename-revert-"
	h := startProxyWithPrefix(t, fu, prefix)
	resp := postRename(t, h.sock, "abc123", url.Values{"name": []string{"not-our-prefix-rename"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403 — the rename inspector did not fire", resp.StatusCode)
	}
	assertNoForward(t, fu)
}

// ── audit log helpers ────────────────────────────────────────────────────

// lastAuditReason returns the `reason` field of the most recent audit
// line written to buf. Used by tests that assert on the structured
// audit token rather than the full JSON line.
func lastAuditReason(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	lines := splitNonEmptyLines(buf.String())
	if len(lines) == 0 {
		t.Fatal("audit buffer is empty")
	}
	var entry struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("unmarshal last audit line: %v (raw=%q)", err, lines[len(lines)-1])
	}
	return entry.Reason
}

// ── compile-time guards ──────────────────────────────────────────────────
//
// fmt and context are imported for parity with the rest of the test
// package — both surface in scaffolding helpers above. Keep the
// imports honest so future additions to this file have them already
// available.
var _ = fmt.Sprintf
var _ = context.TODO
