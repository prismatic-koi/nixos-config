package podmanproxy

// Tests for the volume-name auto-prefix policy.
//
// The policy lives in policy.go::applyVolumeNamePolicy and is gated on
// Config.VolumeNamePrefix. It is the volume twin of the container-name
// policy covered by proxy_name_prefix_test.go, and these tests mirror
// that file's branch coverage:
//
//  1. Prefix empty (back-compat) — body forwards unchanged.
//  2. Prefix non-empty, Name absent — proxy injects an auto-name.
//  3. Prefix non-empty, Name explicitly empty ("") — same as absent.
//  4. Prefix non-empty, no body at all — still injects, because podman
//     would otherwise generate its own unfindable name.
//  5. Prefix non-empty, Name set without the prefix — 403 deny.
//  6. Prefix non-empty, Name set with the prefix — forward unchanged.
//
// Injection assertions read the UPSTREAM's received body so the round
// trip from policy decision through the handler's body-restore to the
// upstream forward is covered end to end. The matching deny case (5) is
// the revert-and-watch-fail partner for the injection cases: loosening
// the prefix check turns it red.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// startProxyWithVolumePrefix stands up a proxy harness with the
// supplied VolumeNamePrefix wired in. Other policy fields stay at their
// defaults; these tests never touch HostConfig.
func startProxyWithVolumePrefix(t *testing.T, fu *fakeUpstream, prefix string) *proxyHarness {
	t.Helper()
	dir := shortSocketDir(t)
	listenPath := filepath.Join(dir, "proxy.sock")
	auditBuf := &bytes.Buffer{}
	cfg := Config{
		ListenerPath:     listenPath,
		UpstreamPath:     fu.sockPath,
		VolumeNamePrefix: prefix,
		AuditWriter:      auditBuf,
	}
	startProxyWithConfig(t, cfg, auditBuf, listenPath)
	return &proxyHarness{
		sock:  listenPath,
		audit: auditBuf,
	}
}

// postVolumeCreateRaw posts an arbitrary JSON body to /volumes/create.
// The body is expressed as a map so "Name absent" / "Name empty" /
// "Name present" are all directly spellable.
func postVolumeCreateRaw(t *testing.T, sock string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return postVolumeCreateBytes(t, sock, buf)
}

// postVolumeCreateBytes posts raw bytes to /volumes/create, so a test
// can send a body no Go map can express — an empty body in particular.
func postVolumeCreateBytes(t *testing.T, sock string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/volumes/create",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(t, sock, req)
}

// readUpstreamVolumeName parses fu.lastBody and returns the Name field
// the upstream actually received.
func readUpstreamVolumeName(t *testing.T, fu *fakeUpstream) string {
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

// TestVolumeNamePrefix_EmptyPrefix_NoOp pins the back-compat path: with
// VolumeNamePrefix unset the proxy neither injects a Name nor rejects a
// body that omits one. Out-of-tree callers that do not need session
// scoping keep the pre-change behaviour.
func TestVolumeNamePrefix_EmptyPrefix_NoOp(t *testing.T) {
	fu := newFakeUpstream(t)
	h := startProxyWithVolumePrefix(t, fu, "")
	resp := postVolumeCreateRaw(t, h.sock, map[string]any{"Driver": "local"})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if got := readUpstreamVolumeName(t, fu); got != "" {
		t.Errorf("upstream Name: got %q, want empty (no injection when prefix is empty)", got)
	}
}

// ── inject branches ─────────────────────────────────────────────────────

// TestVolumeNamePrefix_MissingName_Injects is the AC: a volumes/create
// request with no Name is forwarded with the name
// prism-<session>-<generated>.
func TestVolumeNamePrefix_MissingName_Injects(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@vol-inject-"
	h := startProxyWithVolumePrefix(t, fu, prefix)

	resp := postVolumeCreateRaw(t, h.sock, map[string]any{"Driver": "local"})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}

	got := readUpstreamVolumeName(t, fu)
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("upstream Name %q does not start with %q", got, prefix)
	}
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

// TestVolumeNamePrefix_EmptyName_Injects covers Name="" taking the same
// branch as an absent Name.
func TestVolumeNamePrefix_EmptyName_Injects(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@vol-empty-"
	h := startProxyWithVolumePrefix(t, fu, prefix)

	resp := postVolumeCreateRaw(t, h.sock, map[string]any{"Name": "", "Driver": "local"})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if got := readUpstreamVolumeName(t, fu); !strings.HasPrefix(got, prefix) {
		t.Errorf("upstream Name %q does not start with %q", got, prefix)
	}
}

// TestVolumeNamePrefix_EmptyBody_Injects covers the shape a bare
// `podman volume create` can send: no body at all. Before the name
// policy this was a plain allow, and podman then generated its own
// random volume name that the cleanup sweep could never find.
func TestVolumeNamePrefix_EmptyBody_Injects(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@vol-nobody-"
	h := startProxyWithVolumePrefix(t, fu, prefix)

	resp := postVolumeCreateBytes(t, h.sock, nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if got := readUpstreamVolumeName(t, fu); !strings.HasPrefix(got, prefix) {
		t.Errorf("upstream Name %q does not start with %q (empty body must still be named)", got, prefix)
	}
}

// TestVolumeNamePrefix_Injects_PreservesOtherFields proves the rewrite
// only adds a Name: every other admitted field reaches the upstream
// byte-identical.
func TestVolumeNamePrefix_Injects_PreservesOtherFields(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@vol-preserve-"
	h := startProxyWithVolumePrefix(t, fu, prefix)

	resp := postVolumeCreateRaw(t, h.sock, map[string]any{
		"Driver": "local",
		"Labels": map[string]string{"owner": "prism"},
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}

	raw, _ := fu.lastBody.Load().([]byte)
	var obj struct {
		Name   string            `json:"Name"`
		Driver string            `json:"Driver"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal upstream body: %v (raw=%q)", err, raw)
	}
	if obj.Driver != "local" {
		t.Errorf("Driver: got %q, want %q", obj.Driver, "local")
	}
	if obj.Labels["owner"] != "prism" {
		t.Errorf("Labels: got %v, want owner=prism", obj.Labels)
	}
	if !strings.HasPrefix(obj.Name, prefix) {
		t.Errorf("Name %q does not start with %q", obj.Name, prefix)
	}
}

// TestVolumeNamePrefix_LowercaseNameKeyStripped closes the same
// case-variant bypass injectNameIntoBody documents for containers: a
// body carrying `{"name":""}` must not survive the rewrite and override
// the injected value on the upstream's case-insensitive decoder.
//
// The proxy's own strict decoder matches "name" to the Name field
// case-insensitively, so this body takes the inject branch. Without the
// strip, json.Marshal would emit both "Name" and "name" and podman's
// last-wins decode would land on the empty one.
func TestVolumeNamePrefix_LowercaseNameKeyStripped(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@vol-case-"
	h := startProxyWithVolumePrefix(t, fu, prefix)

	resp := postVolumeCreateBytes(t, h.sock, []byte(`{"name":""}`))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}

	raw, _ := fu.lastBody.Load().([]byte)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal upstream body: %v (raw=%q)", err, raw)
	}
	for k := range obj {
		if k != "Name" && strings.EqualFold(k, "Name") {
			t.Errorf("upstream body retains case-variant Name key %q — the injected name can be overridden by last-wins decoding; body=%q", k, raw)
		}
	}
	if got := readUpstreamVolumeName(t, fu); !strings.HasPrefix(got, prefix) {
		t.Errorf("upstream Name %q does not start with %q", got, prefix)
	}
}

// ── deny branch ─────────────────────────────────────────────────────────

// TestVolumeNamePrefix_ForeignName_Denied is the security AC: a
// volumes/create request naming a volume outside the per-session prefix
// returns 403. It is also the revert-and-watch-fail partner for the
// injection tests — a policy that only injects and never rejects would
// let an agent create (and later mount) a volume the sweep cannot find.
func TestVolumeNamePrefix_ForeignName_Denied(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@vol-deny-"
	h := startProxyWithVolumePrefix(t, fu, prefix)

	for _, name := range []string{
		"not-our-prefix",
		"prism-other-session-cafebabe",
		// Substring trap: contains the prefix but does not start
		// with it.
		"user-prism-prism-test@vol-deny-data",
	} {
		t.Run(name, func(t *testing.T) {
			resp := postVolumeCreateRaw(t, h.sock, map[string]any{"Name": name})
			if resp.StatusCode != http.StatusForbidden {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status: got %d, want 403 (body=%q)", resp.StatusCode, body)
			}
		})
	}

	if !strings.Contains(h.audit.String(), "volume_name_prefix_mismatch") {
		t.Errorf("audit log does not carry reason volume_name_prefix_mismatch; log=%s", h.audit.String())
	}
}

// ── allow branch: correctly-prefixed explicit name ──────────────────────

// TestVolumeNamePrefix_PrefixedName_ForwardedUnchanged is the AC: an
// explicit Name that starts with prism-<session>- is forwarded with that
// name unchanged. A user-chosen suffix is admitted deliberately — the
// cleanup sweep matches on the prefix, so it finds these too.
func TestVolumeNamePrefix_PrefixedName_ForwardedUnchanged(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@vol-ok-"
	h := startProxyWithVolumePrefix(t, fu, prefix)

	want := prefix + "my-postgres-data"
	resp := postVolumeCreateRaw(t, h.sock, map[string]any{"Name": want, "Driver": "local"})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if got := readUpstreamVolumeName(t, fu); got != want {
		t.Errorf("upstream Name: got %q, want %q (an already-prefixed name must forward unchanged)", got, want)
	}
}

// ── ordering: the escape check still wins ───────────────────────────────

// TestVolumeNamePrefix_DriverOptsEscapeStillDeniedFirst pins the check
// order. A body that is BOTH a local-driver bind-volume escape AND
// correctly prefixed must still be denied, and the audit reason must
// name the escape, not the name policy. Adding the name policy must not
// displace the T3 mitigation.
func TestVolumeNamePrefix_DriverOptsEscapeStillDeniedFirst(t *testing.T) {
	fu := newFakeUpstream(t)
	prefix := "prism-prism-test@vol-order-"
	h := startProxyWithVolumePrefix(t, fu, prefix)

	resp := postVolumeCreateRaw(t, h.sock, map[string]any{
		"Name":   prefix + "escape",
		"Driver": "local",
		"DriverOpts": map[string]string{
			"type": "none", "device": "/etc", "o": "bind",
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 403 (body=%q)", resp.StatusCode, body)
	}
	if !strings.Contains(h.audit.String(), "volume_local_driver_opts") {
		t.Errorf("audit log must name the escape check, not the name policy; log=%s", h.audit.String())
	}
}
