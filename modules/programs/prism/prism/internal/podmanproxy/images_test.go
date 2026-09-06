package podmanproxy

// Tests for the per-session image ledger (images.go).
//
// Three layers are covered:
//
//  1. pulledImageRef — the query-to-reference derivation, including the
//     tag/digest join rules and the argument-injection allowlist.
//  2. The proxy end to end — POST /images/create forwards upstream AND
//     appends exactly one ledger line.
//  3. ReadImageLedger — the reader `prism cleanup` uses, including the
//     missing-file, torn-line, and hostile-content cases.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── pulledImageRef ──────────────────────────────────────────────────────

func TestPulledImageRef(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"bare fromImage", "fromImage=alpine", "alpine"},
		{"fromImage with tag param", "fromImage=alpine&tag=3.19", "alpine:3.19"},
		{"fully qualified with tag param", "fromImage=docker.io%2Flibrary%2Falpine&tag=3.19", "docker.io/library/alpine:3.19"},
		{"tag already in name wins over tag param", "fromImage=alpine%3A3.19&tag=latest", "alpine:3.19"},
		{"digest already in name wins over tag param", "fromImage=alpine%40sha256%3Aabc&tag=latest", "alpine@sha256:abc"},
		{"digest tag param joins with @", "fromImage=alpine&tag=sha256%3Aabcdef", "alpine@sha256:abcdef"},
		{"registry port is not a tag", "fromImage=registry%3A5000%2Falpine&tag=3.19", "registry:5000/alpine:3.19"},
		{"repo used when fromImage absent", "fromSrc=-&repo=imported&tag=v1", "imported:v1"},
		{"fromImage wins over repo", "fromImage=alpine&repo=other", "alpine"},

		// Unrecordable: nothing the sweep could name.
		{"no name parameter", "fromSrc=-", ""},
		{"empty query", "", ""},

		// Argument-injection allowlist. A reference the sweep would
		// hand to `podman rmi` as a positional argument must never be
		// able to look like a flag or carry a shell/whitespace shape.
		{"leading dash rejected", "fromImage=-f", ""},
		{"long flag rejected", "fromImage=--force", ""},
		{"whitespace rejected", "fromImage=alpine%20--force", ""},
		{"newline rejected", "fromImage=alpine%0A--force", ""},
		{"semicolon rejected", "fromImage=alpine%3Brm", ""},
		{"leading slash rejected", "fromImage=%2Fetc%2Fpasswd", ""},

		// Path traversal. The character allowlist admits '.' and '/',
		// so it admits dot segments on its own. The reference is
		// embedded in the path of an upstream probe against the
		// privileged podman socket, so a dot segment walks that probe
		// onto an endpoint the proxy's allowlist denies.
		{"parent traversal rejected", "fromImage=a%2F..%2F..%2Flibpod%2Fsecrets%2Fmysecret", ""},
		{"single dot segment rejected", "fromImage=a%2F.%2Fb", ""},
		{"double slash rejected", "fromImage=a%2F%2Fb", ""},
		{"trailing slash rejected", "fromImage=alpine%2F", ""},
		{"traversal via tag param rejected", "fromImage=alpine&tag=..%2F..%2Finfo", ""},

		// Dots that are NOT segments stay legal — a registry host is
		// full of them, so the structural check must not be a blanket
		// ban on '.'.
		{"registry host dots allowed", "fromImage=ghcr.io%2Fns%2Fimg&tag=1.2.3", "ghcr.io/ns/img:1.2.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.query, err)
			}
			if got := pulledImageRef(q); got != tc.want {
				t.Errorf("pulledImageRef(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestPulledImageRef_OverlongReferenceRejected(t *testing.T) {
	long := strings.Repeat("a", maxImageRefLen+1)
	q := url.Values{"fromImage": {long}}
	if got := pulledImageRef(q); got != "" {
		t.Errorf("pulledImageRef on a %d-byte reference = %q, want \"\"", len(long), got)
	}
}

// ── proxy end to end ────────────────────────────────────────────────────

// startProxyWithImageLedger stands up a proxy harness whose
// PulledImageWriter is the returned buffer.
func startProxyWithImageLedger(t *testing.T, fu *fakeUpstream) (*proxyHarness, *bytes.Buffer) {
	t.Helper()
	dir := shortSocketDir(t)
	listenPath := filepath.Join(dir, "proxy.sock")
	auditBuf := &bytes.Buffer{}
	ledger := &bytes.Buffer{}
	cfg := Config{
		ListenerPath:      listenPath,
		UpstreamPath:      fu.sockPath,
		AuditWriter:       auditBuf,
		PulledImageWriter: ledger,
	}
	startProxyWithConfig(t, cfg, auditBuf, listenPath)
	return &proxyHarness{sock: listenPath, audit: auditBuf}, ledger
}

func postImageCreate(t *testing.T, sock, rawQuery string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		"http://podman.sock/v1.41/images/create?"+rawQuery, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return doRequest(t, sock, req)
}

// TestImageLedger_PullIsRecordedAndForwarded is the core behaviour: the
// pull still reaches the upstream (the endpoint's permission is
// unchanged) AND the reference lands in the ledger so cleanup can remove
// it.
func TestImageLedger_PullIsRecordedAndForwarded(t *testing.T) {
	fu := newFakeUpstream(t)
	h, ledger := startProxyWithImageLedger(t, fu)

	resp := postImageCreate(t, h.sock, "fromImage=alpine&tag=3.19")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q) — images/create must still forward", resp.StatusCode, body)
	}
	if got := fu.lastRawQuery.Load(); got == nil || !strings.Contains(got.(string), "fromImage=alpine") {
		t.Errorf("upstream did not receive the pull query; got %v", got)
	}

	lines := nonEmptyLines(ledger.String())
	if len(lines) != 1 {
		t.Fatalf("ledger lines: got %d, want exactly 1; ledger=%q", len(lines), ledger.String())
	}
	var entry struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal ledger line %q: %v", lines[0], err)
	}
	if entry.Image != "alpine:3.19" {
		t.Errorf("ledger image: got %q, want %q", entry.Image, "alpine:3.19")
	}
	if !strings.Contains(h.audit.String(), "policy:images/create:recorded:alpine:3.19") {
		t.Errorf("audit log does not record the pulled reference; log=%s", h.audit.String())
	}
}

// TestImageLedger_UnrecordablePullStillForwards proves the ledger never
// gates the request: an import with no nameable reference is forwarded
// and simply left out of the ledger.
func TestImageLedger_UnrecordablePullStillForwards(t *testing.T) {
	fu := newFakeUpstream(t)
	h, ledger := startProxyWithImageLedger(t, fu)

	resp := postImageCreate(t, h.sock, "fromSrc=-")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if lines := nonEmptyLines(ledger.String()); len(lines) != 0 {
		t.Errorf("ledger lines: got %d, want 0; ledger=%q", len(lines), ledger.String())
	}
	if !strings.Contains(h.audit.String(), "policy:images/create:unrecordable") {
		t.Errorf("audit log does not explain why the image was not recorded; log=%s", h.audit.String())
	}
}

// TestImageLedger_NewlineInQueryCannotForgeALine is the ledger's
// injection defence at the file-format level. A query parameter can
// carry a literal newline, so a plain-text ledger would let the agent
// write extra entries. The reference is rejected outright here, but the
// assertion that matters is the line count: one request can never
// produce more than one line.
func TestImageLedger_NewlineInQueryCannotForgeALine(t *testing.T) {
	fu := newFakeUpstream(t)
	h, ledger := startProxyWithImageLedger(t, fu)

	resp := postImageCreate(t, h.sock, "fromImage="+url.QueryEscape("alpine\n{\"image\":\"evil\"}"))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	if lines := nonEmptyLines(ledger.String()); len(lines) != 0 {
		t.Errorf("a reference with an embedded newline must not be recorded; got %d line(s): %q", len(lines), ledger.String())
	}
}

// TestImageLedger_AlreadyPresentImageIsNotRecorded is the security
// assertion behind the existence probe. The ledger drives `podman rmi`
// against a store the user and every other session share, so recording
// the requested reference alone would let an agent enrol an image it
// did NOT pull for deletion — a deferred deletion primitive that prism,
// not the agent, would execute after the session ended.
//
// The pull must still forward. Only the ledger entry is withheld.
func TestImageLedger_AlreadyPresentImageIsNotRecorded(t *testing.T) {
	fu := newFakeUpstream(t)
	fu.markImagePresent("alpine:3.19")
	h, ledger := startProxyWithImageLedger(t, fu)

	resp := postImageCreate(t, h.sock, "fromImage=alpine&tag=3.19")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q) — the pull must still forward", resp.StatusCode, body)
	}
	if got := fu.captured(); got != 1 {
		t.Errorf("upstream forwarded requests: got %d, want 1", got)
	}
	if lines := nonEmptyLines(ledger.String()); len(lines) != 0 {
		t.Errorf("SECURITY: an image that was already on the host was enrolled for removal at cleanup: %q", ledger.String())
	}
	if !strings.Contains(h.audit.String(), "policy:images/create:already_present:alpine:3.19") {
		t.Errorf("audit log does not explain why the image was not recorded; log=%s", h.audit.String())
	}
}

// TestImageLedger_ProbeFailureDoesNotRecord pins the fail-closed
// direction. When the proxy cannot establish whether the image was
// already there, it must not record.
//
// The two failure modes are not symmetric: an unrecorded image leaks
// storage, a wrongly recorded one destroys data the session did not
// create. The tie goes to leaking.
func TestImageLedger_ProbeFailureDoesNotRecord(t *testing.T) {
	fu := newFakeUpstream(t)
	fu.setProbeStatus(http.StatusInternalServerError)
	h, ledger := startProxyWithImageLedger(t, fu)

	resp := postImageCreate(t, h.sock, "fromImage=alpine&tag=3.19")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q) — a probe failure must not fail the pull", resp.StatusCode, body)
	}
	if lines := nonEmptyLines(ledger.String()); len(lines) != 0 {
		t.Errorf("an unresolvable probe must not record; ledger=%q", ledger.String())
	}
	if !strings.Contains(h.audit.String(), "policy:images/create:probe_failed") {
		t.Errorf("audit log does not name the probe failure; log=%s", h.audit.String())
	}
}

// TestImageLedger_ProbeIsNotAuditedSeparately pins the "exactly one
// audit line per request" contract against the new probe: the probe is
// the proxy's own request, so it must not add a second line.
func TestImageLedger_ProbeIsNotAuditedSeparately(t *testing.T) {
	fu := newFakeUpstream(t)
	h, _ := startProxyWithImageLedger(t, fu)

	resp := postImageCreate(t, h.sock, "fromImage=alpine&tag=3.19")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := fu.probes.Load(); got != 1 {
		t.Fatalf("upstream probes: got %d, want 1", got)
	}
	if lines := nonEmptyLines(h.audit.String()); len(lines) != 1 {
		t.Errorf("audit lines: got %d, want exactly 1 per request; log=%s", len(lines), h.audit.String())
	}
}

// TestImageLedger_TraversalRefNeverReachesUpstream is the regression
// test for the probe path-traversal hole.
//
// The proxy builds the existence-probe URL by splicing the
// agent-controlled reference into a path on the privileged podman
// socket. A reference carrying dot segments turns that probe into a GET
// against an endpoint the proxy's own allowlist denies, and the status
// code comes back as an oracle the agent can read.
//
// The assertion is on the RAW request-target the upstream received, not
// on the handler's r.URL.Path: http.ServeMux cleans dot segments itself
// and answers with a 301, so a normalised capture would show a harmless
// path whether or not the bug is present.
func TestImageLedger_TraversalRefNeverReachesUpstream(t *testing.T) {
	fu := newFakeUpstream(t)
	h, ledger := startProxyWithImageLedger(t, fu)

	const hostile = "a/../../libpod/secrets/mysecret"
	resp := postImageCreate(t, h.sock, "fromImage="+url.QueryEscape(hostile))
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	for _, raw := range fu.capturedRawPaths() {
		// Only the PATH component matters. The forwarded images/create
		// request legitimately carries the reference in its query
		// string, percent-encoded, where it is an opaque parameter
		// value that podman resolves as an image name. The hazard is a
		// reference spliced into the path, which is what routes the
		// request somewhere else.
		rawPath := raw
		if i := strings.IndexByte(rawPath, '?'); i >= 0 {
			rawPath = rawPath[:i]
		}
		if strings.Contains(rawPath, "..") {
			t.Errorf("SECURITY: the proxy sent a traversal path upstream: %q", raw)
		}
		if strings.Contains(rawPath, "/secrets/") {
			t.Errorf("SECURITY: the proxy probed a denied endpoint: %q", raw)
		}
	}
	// The reference is rejected before the probe runs at all, so no
	// probe should have been issued.
	if got := fu.probes.Load(); got != 0 {
		t.Errorf("probes issued for a non-probe-safe reference: got %d, want 0", got)
	}
	if lines := nonEmptyLines(ledger.String()); len(lines) != 0 {
		t.Errorf("a traversal reference must not be recorded; ledger=%q", ledger.String())
	}
}

// TestImageExistsUpstream_RefusesNonProbeSafeRef pins the guard at the
// probe function itself, not only at its current caller. imageRefIsSweepable
// runs inside imageExistsUpstream too, so a future caller that forgets
// the check cannot reopen the hole.
func TestImageExistsUpstream_RefusesNonProbeSafeRef(t *testing.T) {
	fu := newFakeUpstream(t)
	dir := shortSocketDir(t)
	p, err := NewProxy(Config{
		ListenerPath: filepath.Join(dir, "proxy.sock"),
		UpstreamPath: fu.sockPath,
	})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	for _, ref := range []string{
		"a/../../libpod/secrets/x",
		"a/./b",
		"a//b",
		"-f",
		"",
	} {
		if _, err := p.imageExistsUpstream(context.Background(), ref); err == nil {
			t.Errorf("imageExistsUpstream(%q): got nil error, want refusal", ref)
		}
	}
	if got := fu.probes.Load(); got != 0 {
		t.Errorf("a refused reference must not reach the upstream; probes=%d", got)
	}
}

// TestImageLedger_NilWriterIsSafe pins the documented degradation: no
// ledger writer means no ledger lines and no failure. The sidecar takes
// this path when the ledger file cannot be opened.
func TestImageLedger_NilWriterIsSafe(t *testing.T) {
	fu := newFakeUpstream(t)
	dir := shortSocketDir(t)
	listenPath := filepath.Join(dir, "proxy.sock")
	auditBuf := &bytes.Buffer{}
	startProxyWithConfig(t, Config{
		ListenerPath: listenPath,
		UpstreamPath: fu.sockPath,
		AuditWriter:  auditBuf,
	}, auditBuf, listenPath)

	resp := postImageCreate(t, listenPath, "fromImage=alpine")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: got %d, want 200 (body=%q)", resp.StatusCode, body)
	}
	// With no ledger there is nothing to decide, so the proxy must not
	// spend an upstream round-trip on the probe.
	if got := fu.probes.Load(); got != 0 {
		t.Errorf("upstream probes with no ledger writer: got %d, want 0", got)
	}
}

// ── ReadImageLedger ─────────────────────────────────────────────────────

// TestReadImageLedger_MissingFileIsNotAnError is the load-bearing case
// for the "a session that never pulled an image issues no podman
// command" AC: cleanup must be able to tell "nothing to do" from "the
// read failed".
func TestReadImageLedger_MissingFileIsNotAnError(t *testing.T) {
	refs, err := ReadImageLedger(filepath.Join(t.TempDir(), "absent.log"))
	if err != nil {
		t.Fatalf("ReadImageLedger on a missing file: %v, want nil", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs: got %v, want none", refs)
	}
}

func TestReadImageLedger_ParsesDedupesAndFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "podman-images.log")
	content := strings.Join([]string{
		`{"image":"alpine:3.19"}`,
		`{"image":"docker.io/library/busybox:latest"}`,
		`{"image":"alpine:3.19"}`, // duplicate — one rmi, not two
		``,                        // blank line
		`not json at all`,         // torn / corrupt line
		`{"image":"--force"}`,     // hand-edited flag-shaped entry
		`{"image":""}`,            // empty reference
		`{"image":"alpine:3.19"`,  // truncated last line (killed mid-write)
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}

	refs, err := ReadImageLedger(path)
	if err != nil {
		t.Fatalf("ReadImageLedger: %v", err)
	}
	want := []string{"alpine:3.19", "docker.io/library/busybox:latest"}
	if len(refs) != len(want) {
		t.Fatalf("refs: got %v, want %v", refs, want)
	}
	for i, w := range want {
		if refs[i] != w {
			t.Errorf("refs[%d]: got %q, want %q (first-seen order)", i, refs[i], w)
		}
	}
}

// TestReadImageLedger_RejectsFlagShapedReferences is the reader half of
// the argument-injection defence. Validating on write alone is not
// enough: the ledger is a file on disk, and the sweep must not trust it.
func TestReadImageLedger_RejectsFlagShapedReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "podman-images.log")
	hostile := []string{
		`{"image":"-f"}`,
		`{"image":"--all"}`,
		`{"image":"alpine --force"}`,
		`{"image":"/etc/passwd"}`,
		`{"image":"alpine;rm -rf /"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(hostile, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	refs, err := ReadImageLedger(path)
	if err != nil {
		t.Fatalf("ReadImageLedger: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("SECURITY: flag-shaped ledger entries reached the sweep: %v", refs)
	}
}

// nonEmptyLines splits s on newlines and drops blank entries.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
