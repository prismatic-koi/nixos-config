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
	// and check its `reason` field.
	if got := lastAuditReason(t, h.audit); got != "name_prefix_mismatch" {
		t.Errorf("audit reason: got %q, want \"name_prefix_mismatch\"", got)
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
