package cmd

// End-to-end tests for the `prism account usage` active refresh
// (issue #2541, parent #2537).
//
// Every test here runs against three fakes wired together the way the real
// system is:
//
//	fake Anthropic  an httptest server reached via $ANTHROPIC_BASE_URL
//	fake sidecar    a unix-socket HTTP server at the exact path
//	                $XDG_STATE_HOME/prism/run/<dir>/hostapi.sock that the
//	                discovery code globs, decoding with DisallowUnknownFields
//	                exactly as the real endpoint does
//	fake accounts   $XDG_CONFIG_HOME/prism/accounts with a `current` pointer
//	                and one credential blob
//
// Nothing reaches the network or the real state directory. The fake sidecar
// decoding strictly is deliberate: it makes these tests fail if the wire
// payload ever grows a field POST /usage/snapshot would reject.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/usage"
)

const refreshTestToken = "sk-ant-oat01-TEST-TOKEN-MUST-NOT-APPEAR"

// ── Fixture ──────────────────────────────────────────────────────────────────

type refreshFixture struct {
	usageDir    string
	accountsDir string
	authJSON    string
	stateHome   string

	anthropic     *httptest.Server
	anthropicHits *atomic.Int64

	sidecarPosts *atomic.Int64
	sidecarPaths chan string
	sidecarSrv   *http.Server
	sidecarSock  string
}

// newRefreshFixture builds the full host-side environment. status and headers
// govern what the fake Anthropic endpoint replies with.
func newRefreshFixture(t *testing.T, status int, headers http.Header) *refreshFixture {
	t.Helper()

	// os.MkdirTemp rather than t.TempDir: the unix socket path below is built
	// under this directory and sun_path is capped at 108 bytes on Linux and
	// 104 on Darwin. t.TempDir embeds the (long) test name and blows that
	// budget. Same reasoning as internal/sidecar/sidecartest.
	stateHome, err := os.MkdirTemp("", "pu-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateHome) })

	configHome := t.TempDir()
	authJSON := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("PI_AUTH_JSON", authJSON)
	// Host-side by definition: PRISM_HOST_API is the sandbox sentinel and the
	// sidecar sets it only inside a sandbox.
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("ANTHROPIC_USER_AGENT", "")
	t.Setenv("ANTHROPIC_BETA_FLAGS", "")

	f := &refreshFixture{
		usageDir:      filepath.Join(stateHome, "prism", "usage"),
		accountsDir:   filepath.Join(configHome, "prism", "accounts"),
		authJSON:      authJSON,
		stateHome:     stateHome,
		anthropicHits: &atomic.Int64{},
		sidecarPosts:  &atomic.Int64{},
		sidecarPaths:  make(chan string, 16),
	}

	f.anthropic = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.anthropicHits.Add(1)
		for name, values := range headers {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	t.Cleanup(f.anthropic.Close)

	// In-process seam. There is deliberately NO environment variable that can
	// redirect the refresh: the request carries an OAuth bearer token, and an
	// env-var destination would be an exfiltration lever on a real host
	// (round-1 review-security finding).
	prev := newUsageRefresher
	t.Cleanup(func() { newUsageRefresher = prev })
	newUsageRefresher = func() *usage.Refresher {
		return &usage.Refresher{BaseURL: f.anthropic.URL, HTTPClient: f.anthropic.Client()}
	}

	f.startSidecar(t)
	return f
}

// startSidecar listens on the exact socket path discoverSidecarAPI globs and
// mirrors the real POST /usage/snapshot handler: strict decode, host-side
// account resolution, write through usage.Store.
func (f *refreshFixture) startSidecar(t *testing.T) {
	t.Helper()
	runDir := filepath.Join(f.stateHome, "prism", "run", "sess")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	sock := filepath.Join(runDir, "hostapi.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.sidecarPosts.Add(1)
		select {
		case f.sidecarPaths <- r.URL.Path:
		default:
		}
		if r.URL.Path != usageSnapshotEndpoint || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// DisallowUnknownFields mirrors the real endpoint. It is what stops a
		// caller supplying `account` or `captured_at`, and it makes this test
		// fail if the wire payload ever drifts from the endpoint's schema.
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var payload usage.SnapshotPayload
		if err := dec.Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid JSON: ` + err.Error() + `"}`))
			return
		}
		snap := payload.ToSnapshot(usage.CurrentAccountName(), time.Now())
		if err := usage.NewStore(f.usageDir).Write(snap); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account":"` + snap.Account + `"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	f.sidecarSrv = srv
	f.sidecarSock = sock
}

// stopSidecar shuts the fake sidecar down and removes its socket file,
// reproducing a host with no running prism session.
func (f *refreshFixture) stopSidecar(t *testing.T) {
	t.Helper()
	if err := f.sidecarSrv.Close(); err != nil {
		t.Fatalf("close fake sidecar: %v", err)
	}
	if err := os.Remove(f.sidecarSock); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove fake sidecar socket: %v", err)
	}
}

// oauthBlob renders one Anthropic OAuth blob. A zero expiresAt omits the
// expiry field entirely.
func oauthBlob(token string, expiresAt time.Time) string {
	if expiresAt.IsZero() {
		return fmt.Sprintf(`{"type":"oauth","access":%q,"refresh":"r"}`, token)
	}
	return fmt.Sprintf(`{"type":"oauth","access":%q,"refresh":"r","expires":%d}`,
		token, expiresAt.UnixMilli())
}

// seedAccount writes the `current` pointer, the stored accounts/<name>.json
// copy, and the LIVE auth.json blob — mirroring the state `prism account use`
// leaves behind.
//
// The live blob carries the same expiry as the stored copy here. Tests that
// need them to diverge (the real-world case, where pi has rotated the live
// token and the stored copy has gone stale) call seedStaleStoredCopy or
// writeLiveBlob directly.
func (f *refreshFixture) seedAccount(t *testing.T, name string, expiresAt time.Time) {
	t.Helper()
	if err := os.MkdirAll(f.accountsDir, 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	blob := oauthBlob(refreshTestToken, expiresAt)
	if err := os.WriteFile(filepath.Join(f.accountsDir, name+".json"), []byte(blob), 0o600); err != nil {
		t.Fatalf("write account blob: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.accountsDir, "current"), []byte(name+"\n"), 0o600); err != nil {
		t.Fatalf("write accounts/current: %v", err)
	}
	f.writeLiveBlob(t, blob)
}

// writeLiveBlob writes auth.json with the given anthropic blob, alongside an
// unrelated sibling key so the reader cannot accidentally depend on the file
// holding nothing else.
func (f *refreshFixture) writeLiveBlob(t *testing.T, blob string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.authJSON), 0o700); err != nil {
		t.Fatalf("mkdir pi agent dir: %v", err)
	}
	body := `{"github-copilot":{"type":"oauth"},"anthropic":` + blob + `}`
	if err := os.WriteFile(f.authJSON, []byte(body), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
}

// removeLiveBlob deletes auth.json, forcing the fallback to the stored copy.
func (f *refreshFixture) removeLiveBlob(t *testing.T) {
	t.Helper()
	if err := os.Remove(f.authJSON); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove auth.json: %v", err)
	}
}

// seedSnapshot writes a stored snapshot aged by `age`.
func (f *refreshFixture) seedSnapshot(t *testing.T, name string, age time.Duration, utilization float64) {
	t.Helper()
	reset := time.Now().Add(time.Hour).Unix()
	snap := usage.Snapshot{
		CapturedAt: usage.FormatCapturedAt(time.Now().Add(-age)),
		Account:    name,
		Windows: &usage.Windows{
			FiveHour: &usage.Window{Utilization: &utilization, Reset: &reset},
		},
	}
	if err := usage.NewStore(f.usageDir).Write(snap); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func (f *refreshFixture) readSnapshotBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.usageDir, name+".json"))
	if err != nil {
		t.Fatalf("read %s.json: %v", name, err)
	}
	return raw
}

// refreshHeaders is the header set the fake Anthropic endpoint returns on a
// successful refresh. 0.77 is chosen so "77%" is unambiguous in output.
func refreshHeaders() http.Header {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "allowed_warning")
	h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.77")
	h.Set("anthropic-ratelimit-unified-5h-reset", fmt.Sprintf("%d", time.Now().Add(2*time.Hour).Unix()))
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.31")
	h.Set("anthropic-ratelimit-unified-7d-reset", fmt.Sprintf("%d", time.Now().Add(96*time.Hour).Unix()))
	return h
}

// runAccountUsageWithRefresh runs the subcommand with the PRODUCTION flag set
// (refresh enabled by default) and returns stdout and stderr separately.
//
// It calls addAccountUsageFlags rather than hand-rolling a flag set, so a
// change to the real defaults is exercised here rather than silently bypassed.
func runAccountUsageWithRefresh(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	c := &cobra.Command{Use: "usage"}
	addAccountUsageFlags(c)
	if parseErr := c.Flags().Parse(args); parseErr != nil {
		t.Fatalf("parse flags %v: %v", args, parseErr)
	}
	var outBuf, errBuf bytes.Buffer
	c.SetOut(&outBuf)
	c.SetErr(&errBuf)
	err = runAccountUsage(c, nil)
	return outBuf.String(), errBuf.String(), err
}

// ── Functional ACs ───────────────────────────────────────────────────────────

// AC: when a snapshot is missing, one refresh request is made and the
// resulting data is printed. This is the cold-account case the whole feature
// exists for.
func TestAccountUsageRefresh_MissingSnapshotRefreshesAndPrints(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v (stderr: %s)", err, errOut)
	}
	if n := f.anthropicHits.Load(); n != 1 {
		t.Fatalf("Anthropic request count = %d, want 1 (stderr: %s)", n, errOut)
	}
	if !strings.Contains(out, "77%") {
		t.Errorf("expected the refreshed 77%% to be printed, got:\n%s\nstderr: %s", out, errOut)
	}
	if !strings.Contains(out, "work") {
		t.Errorf("expected the account name printed, got:\n%s", out)
	}
}

// AC: a snapshot older than 15 minutes triggers one refresh, and the result is
// printed.
func TestAccountUsageRefresh_StaleSnapshotRefreshesAndPrints(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 20*time.Minute, 0.10)

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v (stderr: %s)", err, errOut)
	}
	if n := f.anthropicHits.Load(); n != 1 {
		t.Fatalf("Anthropic request count = %d, want 1 (stderr: %s)", n, errOut)
	}
	if !strings.Contains(out, "77%") {
		t.Errorf("expected the refreshed 77%%, got:\n%s", out)
	}
	if strings.Contains(out, "10%") {
		t.Errorf("the stale 10%% must be replaced by the refreshed value, got:\n%s", out)
	}
	if strings.Contains(out, "(stale)") {
		t.Errorf("a refreshed row must not be marked stale, got:\n%s", out)
	}
}

// AC: a successful refresh persists through the sidecar endpoint #2538
// defines, rather than writing the files directly.
//
// Two halves, and both matter. The POST proves the write went through the
// endpoint; the changed file proves the endpoint's write actually landed.
func TestAccountUsageRefresh_PersistsThroughTheSidecarEndpoint(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	before := f.readSnapshotBytes(t, "work")

	_, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v (stderr: %s)", err, errOut)
	}
	if errOut != "" {
		t.Fatalf("a successful refresh must emit no warning, got: %s", errOut)
	}

	if n := f.sidecarPosts.Load(); n != 1 {
		t.Fatalf("sidecar request count = %d, want exactly 1", n)
	}
	select {
	case path := <-f.sidecarPaths:
		if path != usageSnapshotEndpoint {
			t.Errorf("sidecar path = %q, want %q", path, usageSnapshotEndpoint)
		}
	default:
		t.Fatal("the sidecar recorded no request path")
	}

	after := f.readSnapshotBytes(t, "work")
	if bytes.Equal(before, after) {
		t.Error("work.json is unchanged; the sidecar's write did not land")
	}
	if !strings.Contains(string(after), "0.77") {
		t.Errorf("work.json does not carry the refreshed utilization:\n%s", after)
	}
	// current.json is the sidecar's second write and is what the bottom bar
	// reads. It must have moved too.
	current, err := os.ReadFile(filepath.Join(f.usageDir, usage.CurrentFileName))
	if err != nil {
		t.Fatalf("read current.json: %v", err)
	}
	if !bytes.Equal(current, after) {
		t.Error("current.json must be a byte-identical copy of the account snapshot")
	}
}

// AC: --no-refresh performs no network request and prints stored data only.
func TestAccountUsageRefresh_NoRefreshFlagMakesNoRequest(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 45*time.Minute, 0.10)

	out, errOut, err := runAccountUsageWithRefresh(t, "--no-refresh")
	if err != nil {
		t.Fatalf("account usage --no-refresh: %v", err)
	}
	if n := f.anthropicHits.Load(); n != 0 {
		t.Fatalf("Anthropic request count = %d, want 0 under --no-refresh", n)
	}
	if n := f.sidecarPosts.Load(); n != 0 {
		t.Fatalf("sidecar request count = %d, want 0 under --no-refresh", n)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored 10%% printed, got:\n%s", out)
	}
	if !strings.Contains(out, "(stale)") {
		t.Errorf("stored data older than 15 minutes must still be marked stale, got:\n%s", out)
	}
	if errOut != "" {
		t.Errorf("--no-refresh must not warn, got: %s", errOut)
	}
}

// AC: a fresh snapshot, newer than 15 minutes, triggers no refresh request.
func TestAccountUsageRefresh_FreshSnapshotTriggersNoRequest(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", time.Minute, 0.10)

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v", err)
	}
	if n := f.anthropicHits.Load(); n != 0 {
		t.Fatalf("Anthropic request count = %d, want 0 for a fresh snapshot", n)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored 10%% printed, got:\n%s", out)
	}
	if errOut != "" {
		t.Errorf("a fresh snapshot must produce no warning, got: %s", errOut)
	}
}

// AC: at most one refresh request is made per account per invocation. Several
// accounts have snapshots, only one is active, and only the active one is
// refreshable — the sidecar endpoint takes no account parameter.
func TestAccountUsageRefresh_AtMostOneRequestPerInvocation(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "personal", 3*time.Hour, 0.20)
	f.seedSnapshot(t, "archive", 4*time.Hour, 0.30)
	f.seedSnapshot(t, "work", 2*time.Hour, 0.10)

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v (stderr: %s)", err, errOut)
	}
	if n := f.anthropicHits.Load(); n != 1 {
		t.Fatalf("Anthropic request count = %d, want exactly 1 for three stale accounts", n)
	}
	// The non-active accounts are still displayed, from their stored data.
	for _, want := range []string{"personal", "archive", "20%", "30%"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got:\n%s", want, out)
		}
	}
}

// ── Token source (#2569 review round 1: review-context) ─────────────────────

// The refresh must read the LIVE token from auth.json, not the frozen copy in
// accounts/<name>.json.
//
// The pi extension rotates the access token in auth.json and never writes it
// back to the accounts directory (anthropic-oauth/credentials.ts,
// writeCredentials). An Anthropic token lives 36000 s, so the stored copy is
// expired roughly ten hours after the snapshot that produced it. A refresh
// that read the stored copy would work once and then report "expired" on
// every later invocation, while pi held a perfectly good token — AC 1 and AC 2
// would stop holding on any real host after about ten hours.
func TestAccountUsageRefresh_PrefersTheLiveTokenOverTheStoredCopy(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)

	// The real-world divergence: the stored copy expired hours ago, and pi has
	// since rotated the live token.
	staleCopy := oauthBlob("stale-stored-token", time.Now().Add(-11*time.Hour))
	if err := os.WriteFile(filepath.Join(f.accountsDir, "work.json"), []byte(staleCopy), 0o600); err != nil {
		t.Fatalf("write stale stored copy: %v", err)
	}
	f.writeLiveBlob(t, oauthBlob(refreshTestToken, time.Now().Add(9*time.Hour)))

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v (stderr: %s)", err, errOut)
	}
	if n := f.anthropicHits.Load(); n != 1 {
		t.Fatalf("Anthropic request count = %d, want 1 — the expired STORED copy must "+
			"not gate a refresh when the LIVE token is good (stderr: %s)", n, errOut)
	}
	if strings.Contains(errOut, "expired") {
		t.Errorf("the stale stored copy must not produce an expiry warning, got: %s", errOut)
	}
	if !strings.Contains(out, "77%") {
		t.Errorf("expected the refreshed numbers, got:\n%s", out)
	}
}

// The stored copy is the fallback, for a host where auth.json holds no
// anthropic key — for example after `prism account login` without --use.
func TestAccountUsageRefresh_FallsBackToTheStoredCopyWhenAuthJSONHasNoBlob(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	f.removeLiveBlob(t)

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v (stderr: %s)", err, errOut)
	}
	if n := f.anthropicHits.Load(); n != 1 {
		t.Fatalf("Anthropic request count = %d, want 1 from the stored fallback (stderr: %s)", n, errOut)
	}
	if !strings.Contains(out, "77%") {
		t.Errorf("expected the refreshed numbers, got:\n%s", out)
	}
}

// An expired LIVE token is the case that genuinely needs a re-login, and it
// must still produce the AC-mandated instruction.
func TestAccountUsageRefresh_ExpiredLiveTokenTellsUserToLogIn(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	// Live blob expired; the stored copy is still nominally good. The live
	// blob wins, so the command must report the expiry.
	f.writeLiveBlob(t, oauthBlob(refreshTestToken, time.Now().Add(-time.Minute)))

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("must exit 0, got: %v", err)
	}
	if !strings.Contains(errOut, "prism account login work") {
		t.Errorf("expected the login instruction, got: %s", errOut)
	}
	if n := f.anthropicHits.Load(); n != 0 {
		t.Errorf("Anthropic request count = %d, want 0 for a known-expired token", n)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored data still printed, got:\n%s", out)
	}
}

// ── Failure ACs: never lose data ─────────────────────────────────────────────

// AC: a failed refresh prints the stored stale data with a warning naming the
// failure, and exits 0.
func TestAccountUsageRefresh_TransportFailureFallsBackToStoredData(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	before := f.readSnapshotBytes(t, "work")
	// Close the endpoint so the request fails at the transport layer.
	f.anthropic.Close()

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("a failed refresh must exit 0, got: %v", err)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored 10%% still printed, got:\n%s", out)
	}
	if !strings.Contains(errOut, "usage refresh failed") {
		t.Errorf("expected a warning naming the failure, got: %s", errOut)
	}
	if bytes.Equal(before, f.readSnapshotBytes(t, "work")) != true {
		t.Error("a failed refresh must leave the stored snapshot byte-identical")
	}
}

// AC: a refresh that returns a non-200 status prints a message naming the
// status code and does not overwrite the existing snapshot.
func TestAccountUsageRefresh_NonOKStatusNamesCodeAndKeepsSnapshot(t *testing.T) {
	f := newRefreshFixture(t, http.StatusTooManyRequests, http.Header{})
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	before := f.readSnapshotBytes(t, "work")

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("a non-200 refresh must exit 0, got: %v", err)
	}
	if !strings.Contains(errOut, "429") {
		t.Errorf("expected the status code named in the warning, got: %s", errOut)
	}
	// #2537: a 429 from a malformed OAuth request looks exactly like quota
	// exhaustion. The message must say so, or the next reader chases the
	// wrong cause.
	if !strings.Contains(errOut, "request shape") {
		t.Errorf("a 429 warning must mention the request-shape possibility, got: %s", errOut)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored 10%% still printed, got:\n%s", out)
	}
	if !bytes.Equal(before, f.readSnapshotBytes(t, "work")) {
		t.Error("a non-200 refresh must not overwrite the stored snapshot")
	}
	if n := f.sidecarPosts.Load(); n != 0 {
		t.Errorf("sidecar request count = %d, want 0 — nothing may be persisted", n)
	}
}

// AC: an expired access token produces a message telling the user to run
// `prism account login <name>`, and does not crash.
func TestAccountUsageRefresh_ExpiredTokenTellsUserToLogIn(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(-time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("an expired token must exit 0, got: %v", err)
	}
	if !strings.Contains(errOut, "prism account login work") {
		t.Errorf("expected the login instruction naming the account, got: %s", errOut)
	}
	if n := f.anthropicHits.Load(); n != 0 {
		t.Errorf("Anthropic request count = %d, want 0 — a known-expired token spends no quota", n)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored data still printed, got:\n%s", out)
	}
}

// The same instruction must appear when the local expiry said the token was
// good but the server disagreed.
func TestAccountUsageRefresh_ServerRejectedTokenTellsUserToLogIn(t *testing.T) {
	f := newRefreshFixture(t, http.StatusUnauthorized, http.Header{})
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	before := f.readSnapshotBytes(t, "work")

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("a 401 must exit 0, got: %v", err)
	}
	if !strings.Contains(errOut, "prism account login work") {
		t.Errorf("expected the login instruction, got: %s", errOut)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored data still printed, got:\n%s", out)
	}
	if !bytes.Equal(before, f.readSnapshotBytes(t, "work")) {
		t.Error("a 401 must not overwrite the stored snapshot")
	}
}

// AC: a 200 response carrying no rate-limit headers is reported as such and
// does not overwrite the existing snapshot.
func TestAccountUsageRefresh_OKWithoutHeadersKeepsSnapshot(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, http.Header{})
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	before := f.readSnapshotBytes(t, "work")

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("must exit 0, got: %v", err)
	}
	if !strings.Contains(errOut, "no rate-limit headers") {
		t.Errorf("expected the missing-headers case reported, got: %s", errOut)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored data still printed, got:\n%s", out)
	}
	if !bytes.Equal(before, f.readSnapshotBytes(t, "work")) {
		t.Error("a header-free 200 must not overwrite the stored snapshot")
	}
	if n := f.sidecarPosts.Load(); n != 0 {
		t.Errorf("sidecar request count = %d, want 0", n)
	}
}

// AC: when ~/.config/prism/accounts/ is unreadable because the command runs
// inside a sandbox, the refresh reports that it cannot run and prints stored
// data instead. This is the COMMON path — most sessions are sandboxed.
func TestAccountUsageRefresh_InsideSandboxReportsAndPrintsStoredData(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	// PRISM_HOST_API is the sandbox sentinel (internal/sandboxenv).
	t.Setenv("PRISM_HOST_API", "unix:///var/run/prism-host/hostapi.sock")

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("a sandboxed invocation must exit 0, got: %v", err)
	}
	if n := f.anthropicHits.Load(); n != 0 {
		t.Fatalf("Anthropic request count = %d, want 0 inside a sandbox", n)
	}
	if !strings.Contains(errOut, "sandbox") {
		t.Errorf("expected the sandbox reason reported, got: %s", errOut)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored data printed, got:\n%s", out)
	}
}

// A sandboxed invocation with a FRESH snapshot needs no refresh, so it must
// stay silent. Otherwise every sandboxed run of the command prints a warning
// about something it never needed.
func TestAccountUsageRefresh_InsideSandboxWithFreshSnapshotIsSilent(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", time.Minute, 0.10)
	t.Setenv("PRISM_HOST_API", "unix:///var/run/prism-host/hostapi.sock")

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v", err)
	}
	if errOut != "" {
		t.Errorf("a fresh snapshot inside a sandbox must warn about nothing, got: %s", errOut)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored data printed, got:\n%s", out)
	}
}

// No accounts directory at all (a host that has never run `prism account`).
// The refresh cannot run; the command must still exit 0 and say why.
func TestAccountUsageRefresh_NoAccountsDirectoryReportsAndExitsZero(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("must exit 0, got: %v", err)
	}
	if n := f.anthropicHits.Load(); n != 0 {
		t.Fatalf("Anthropic request count = %d, want 0 with no accounts directory", n)
	}
	if !strings.Contains(errOut, "prism account login") {
		t.Errorf("expected the login instruction, got: %s", errOut)
	}
	if !strings.Contains(out, "10%") {
		t.Errorf("expected the stored data printed, got:\n%s", out)
	}
}

// A missing usage directory AND a successful refresh: the cold-account case
// end to end. The "usage directory does not exist" message must be replaced
// by real numbers.
func TestAccountUsageRefresh_ColdAccountPrintsRefreshedDataNotTheMissingDirNotice(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v (stderr: %s)", err, errOut)
	}
	if strings.Contains(out, "does not exist") {
		t.Errorf("a successful refresh must replace the missing-directory notice, got:\n%s", out)
	}
	if !strings.Contains(out, "77%") {
		t.Errorf("expected the refreshed numbers, got:\n%s", out)
	}
}

// A refresh that succeeds but cannot reach a sidecar still prints the fresh
// numbers. Losing the display because the write path was unavailable would be
// the worst of both worlds: the user would see nothing and could not tell
// "no quota used" from "the tool broke".
func TestAccountUsageRefresh_NoSidecarStillPrintsFreshDataWithAWarning(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	before := f.readSnapshotBytes(t, "work")
	f.stopSidecar(t)

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("must exit 0, got: %v", err)
	}
	if n := f.anthropicHits.Load(); n != 1 {
		t.Fatalf("Anthropic request count = %d, want 1", n)
	}
	if !strings.Contains(out, "77%") {
		t.Errorf("expected the refreshed numbers printed even without a sidecar, got:\n%s", out)
	}
	if !strings.Contains(errOut, "not persisted") {
		t.Errorf("expected a warning that the snapshot was not persisted, got: %s", errOut)
	}
	if !bytes.Equal(before, f.readSnapshotBytes(t, "work")) {
		t.Error("nothing may write the snapshot files directly — only the sidecar writes")
	}
}

// ── Security AC ──────────────────────────────────────────────────────────────

// AC: no token value appears in command output or in an error message.
//
// Every failure branch is swept, because the token is in scope on all of them.
func TestAccountUsageRefresh_TokenNeverAppearsInOutput(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers http.Header
		expired bool
	}{
		{"success", http.StatusOK, refreshHeaders(), false},
		{"unauthorized", http.StatusUnauthorized, http.Header{}, false},
		{"rate limited", http.StatusTooManyRequests, http.Header{}, false},
		{"ok without headers", http.StatusOK, http.Header{}, false},
		{"expired token", http.StatusOK, refreshHeaders(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRefreshFixture(t, tc.status, tc.headers)
			expiry := time.Now().Add(time.Hour)
			if tc.expired {
				expiry = time.Now().Add(-time.Hour)
			}
			f.seedAccount(t, "work", expiry)
			f.seedSnapshot(t, "work", 30*time.Minute, 0.10)

			out, errOut, err := runAccountUsageWithRefresh(t)
			if err != nil {
				t.Fatalf("must exit 0, got: %v", err)
			}
			for label, text := range map[string]string{"stdout": out, "stderr": errOut} {
				if strings.Contains(text, refreshTestToken) {
					t.Fatalf("%s leaks the access token:\n%s", label, text)
				}
			}
			// The stored snapshot is user-readable too.
			if strings.Contains(string(f.readSnapshotBytes(t, "work")), refreshTestToken) {
				t.Fatal("the persisted snapshot leaks the access token")
			}
		})
	}
}

// ── --json interaction ───────────────────────────────────────────────────────

// A warning must never reach stdout: --json promises a parseable array, and a
// warning mixed into it breaks every consumer.
func TestAccountUsageRefresh_JSONStdoutStaysParseableWhenRefreshFails(t *testing.T) {
	f := newRefreshFixture(t, http.StatusTooManyRequests, http.Header{})
	f.seedAccount(t, "work", time.Now().Add(time.Hour))
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)

	var errBuf bytes.Buffer
	c := &cobra.Command{Use: "usage"}
	addAccountUsageFlags(c)
	if err := c.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json: %v", err)
	}
	c.SetErr(&errBuf)

	captured := captureStdout(t, func() {
		if err := runAccountUsage(c, nil); err != nil {
			t.Fatalf("account usage --json: %v", err)
		}
	})

	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(captured)), &rows); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v (raw: %q)", err, captured)
	}
	if len(rows) != 1 || rows[0]["account"] != "work" {
		t.Errorf("rows = %v, want one row for work", rows)
	}
	if !strings.Contains(errBuf.String(), "429") {
		t.Errorf("the warning must go to stderr, got: %q", errBuf.String())
	}
}

// ── Unit-level helpers ───────────────────────────────────────────────────────

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	fresh := &usage.Snapshot{CapturedAt: usage.FormatCapturedAt(now.Add(-time.Minute))}
	stale := &usage.Snapshot{CapturedAt: usage.FormatCapturedAt(now.Add(-time.Hour))}

	cases := []struct {
		name string
		rows []usage.AccountSnapshot
		want bool
	}{
		{"no rows at all", nil, true},
		{"no row for this account", []usage.AccountSnapshot{{Name: "other", Snapshot: fresh}}, true},
		{"fresh row", []usage.AccountSnapshot{{Name: "work", Snapshot: fresh}}, false},
		{"stale row", []usage.AccountSnapshot{{Name: "work", Snapshot: stale}}, true},
		{"unparseable row", []usage.AccountSnapshot{{Name: "work", ReadErr: fmt.Errorf("boom")}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsRefresh(tc.rows, "work", now); got != tc.want {
				t.Errorf("needsRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMergeRefreshed(t *testing.T) {
	snap := &usage.Snapshot{Account: "work", CapturedAt: usage.FormatCapturedAt(time.Now())}

	t.Run("replaces an existing row", func(t *testing.T) {
		rows := []usage.AccountSnapshot{
			{Name: "archive", Snapshot: &usage.Snapshot{Account: "archive"}},
			{Name: "work", ReadErr: fmt.Errorf("was unparseable")},
		}
		got := mergeRefreshed(rows, snap, "work")
		if len(got) != 2 {
			t.Fatalf("rows = %d, want 2", len(got))
		}
		for _, row := range got {
			if row.Name != "work" {
				continue
			}
			if row.Snapshot != snap {
				t.Error("the work row must carry the refreshed snapshot")
			}
			if row.ReadErr != nil {
				t.Error("a successful refresh must clear the stale read error")
			}
			if !row.Active {
				t.Error("the refreshed account is the active one")
			}
		}
	})

	t.Run("appends a new row in sorted order", func(t *testing.T) {
		rows := []usage.AccountSnapshot{{Name: "zeta"}, {Name: "archive"}}
		got := mergeRefreshed(rows, snap, "work")
		if len(got) != 3 {
			t.Fatalf("rows = %d, want 3", len(got))
		}
		names := make([]string, len(got))
		for i, row := range got {
			names[i] = row.Name
		}
		want := []string{"archive", "work", "zeta"}
		for i := range want {
			if names[i] != want[i] {
				t.Fatalf("names = %v, want %v", names, want)
			}
		}
	})

	t.Run("a nil snapshot is a no-op", func(t *testing.T) {
		rows := []usage.AccountSnapshot{{Name: "work"}}
		if got := mergeRefreshed(rows, nil, "work"); len(got) != 1 || got[0].Snapshot != nil {
			t.Errorf("rows = %+v, want the input unchanged", got)
		}
	})

	// Round-1 review-context finding. rows[].Active comes from
	// usage/current.json while the refresh target comes from
	// accounts/current. The two disagree between a `prism account use` and
	// the next capture, which is exactly the mid-session switch #2537 exists
	// to serve. Without the clearing loop the text output prints `*` on two
	// rows and --json emits two entries with "active": true.
	t.Run("exactly one row stays active after a switch", func(t *testing.T) {
		rows := []usage.AccountSnapshot{
			{Name: "personal", Active: true, Snapshot: &usage.Snapshot{Account: "personal"}},
			{Name: "work", Active: false, Snapshot: &usage.Snapshot{Account: "work"}},
		}
		got := mergeRefreshed(rows, snap, "work")

		active := make([]string, 0, len(got))
		for _, row := range got {
			if row.Active {
				active = append(active, row.Name)
			}
		}
		if len(active) != 1 || active[0] != "work" {
			t.Errorf("active rows = %v, want exactly [work]", active)
		}
	})

	t.Run("appending clears a previously active row", func(t *testing.T) {
		rows := []usage.AccountSnapshot{
			{Name: "personal", Active: true, Snapshot: &usage.Snapshot{Account: "personal"}},
		}
		got := mergeRefreshed(rows, snap, "work")

		active := make([]string, 0, len(got))
		for _, row := range got {
			if row.Active {
				active = append(active, row.Name)
			}
		}
		if len(active) != 1 || active[0] != "work" {
			t.Errorf("active rows = %v, want exactly [work]", active)
		}
	})
}

// End-to-end form of the same finding: after a `prism account use work` that
// has not yet been followed by a capture, current.json still names personal.
// The rendered output must carry one active marker, not two.
func TestAccountUsageRefresh_OnlyOneRowIsMarkedActiveAfterASwitch(t *testing.T) {
	f := newRefreshFixture(t, http.StatusOK, refreshHeaders())
	// personal was captured most recently, so usage/current.json names it.
	f.seedSnapshot(t, "personal", 30*time.Minute, 0.20)
	f.seedSnapshot(t, "work", 30*time.Minute, 0.10)
	if err := os.WriteFile(
		filepath.Join(f.usageDir, usage.CurrentFileName),
		f.readSnapshotBytes(t, "personal"), 0o600); err != nil {
		t.Fatalf("point current.json at personal: %v", err)
	}
	// The user has since switched to work.
	f.seedAccount(t, "work", time.Now().Add(time.Hour))

	out, errOut, err := runAccountUsageWithRefresh(t)
	if err != nil {
		t.Fatalf("account usage: %v (stderr: %s)", err, errOut)
	}
	if n := strings.Count(out, "* "); n != 1 {
		t.Errorf("active markers = %d, want exactly 1, got:\n%s", n, out)
	}
	if !strings.Contains(out, "* work") {
		t.Errorf("the refreshed account must carry the marker, got:\n%s", out)
	}
}

func TestDiscoverSidecarAPI_PrefersTheEnvironmentValue(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "unix:///tmp/explicit.sock")
	got, err := discoverSidecarAPI()
	if err != nil {
		t.Fatalf("discoverSidecarAPI: %v", err)
	}
	if got != "unix:///tmp/explicit.sock" {
		t.Errorf("apiURL = %q, want the PRISM_HOST_API value", got)
	}
}

func TestDiscoverSidecarAPI_NoLiveSidecar(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if _, err := discoverSidecarAPI(); err == nil {
		t.Fatal("expected an error when no sidecar socket exists")
	}
}

// A socket file whose listener is gone is a tombstone from a sidecar that
// exited without cleanup. Discovery must skip it rather than hand back a
// dead URL.
func TestDiscoverSidecarAPI_SkipsATombstoneSocket(t *testing.T) {
	t.Setenv("PRISM_HOST_API", "")
	stateHome, err := os.MkdirTemp("", "pu-tomb-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateHome) })
	t.Setenv("XDG_STATE_HOME", stateHome)

	dead := filepath.Join(stateHome, "prism", "run", "aaa")
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A plain file at the socket path: dialling it fails, exactly as a
	// tombstone does.
	if err := os.WriteFile(filepath.Join(dead, "hostapi.sock"), nil, 0o600); err != nil {
		t.Fatalf("write tombstone: %v", err)
	}

	live := filepath.Join(stateHome, "prism", "run", "bbb")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	liveSock := filepath.Join(live, "hostapi.sock")
	ln, err := net.Listen("unix", liveSock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	got, err := discoverSidecarAPI()
	if err != nil {
		t.Fatalf("discoverSidecarAPI: %v", err)
	}
	if got != "unix://"+liveSock {
		t.Errorf("apiURL = %q, want the live socket %q", got, "unix://"+liveSock)
	}
}
