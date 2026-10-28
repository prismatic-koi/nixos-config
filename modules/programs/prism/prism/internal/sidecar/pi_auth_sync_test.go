package sidecar

// Tests for the pi auth sync of a bwrap or sandbox-exec session
// (pi_auth_sync.go, #3295): POST /pi-auth/sync and the timer sync.
//
// Isolation: each test sets HOME and XDG_STATE_HOME to tempdirs, so the
// host auth.json, the shared lock dir, and the per-session auth dir are all
// test-owned.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/container"
)

const piAuthTestInstance = "prism-test-pi-auth-instance"

// syncBuffer is a bytes.Buffer that the sidecar logger and the test can
// use at the same time.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

type piAuthSidecarEnv struct {
	s        *Sidecar
	logs     *syncBuffer
	hostAuth string
	sessAuth string
	lockDir  string
}

func newPIAuthSidecarEnv(t *testing.T, mode config.IsolationMode) piAuthSidecarEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	hostAuth := filepath.Join(home, ".pi", "agent", "auth.json")
	if err := os.MkdirAll(filepath.Dir(hostAuth), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, hostAuth, `{"anthropic":{"type":"oauth","access":"access-0","refresh":"refresh-0","expires":1000}}`)
	lockDir, _ := container.PISharedStateDirs(home)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	agentDir, err := container.PreparePIAuthSession(piAuthTestInstance, hostAuth)
	if err != nil {
		t.Fatalf("PreparePIAuthSession: %v", err)
	}
	logs := &syncBuffer{}
	s := New(Config{
		SessionName:   "prism-test@pi-auth",
		HarnessName:   "pi",
		IsolationMode: mode,
		InstanceID:    piAuthTestInstance,
		AgentRole:     "worker",
		Harness:       newSSEHarness(),
		Logger:        log.New(logs, "", 0),
	})
	return piAuthSidecarEnv{s: s, logs: logs, hostAuth: hostAuth, sessAuth: filepath.Join(agentDir, "auth.json"), lockDir: lockDir}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// replaceFile replaces path with a rename, as `prism account use` does.
func replaceFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	writeTestFile(t, tmp, content)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func readAccess(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	s, _ := m["anthropic"]["access"].(string)
	return s
}

func (e piAuthSidecarEnv) post(t *testing.T, method string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	e.s.hostAPIHandler().ServeHTTP(rec, httptest.NewRequest(method, "/pi-auth/sync", nil))
	return rec
}

// The endpoint copies a refreshed token from the session file to the host
// file, and logs it.
func TestPIAuthSyncEndpoint_RefreshReachesHost(t *testing.T) {
	e := newPIAuthSidecarEnv(t, config.IsolationBwrap)
	writeTestFile(t, e.sessAuth, `{"anthropic":{"type":"oauth","access":"access-1","refresh":"refresh-1","expires":5000}}`)

	rec := e.post(t, http.MethodPost)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("POST /pi-auth/sync = %d %s", rec.Code, rec.Body.String())
	}
	if got := readAccess(t, e.hostAuth); got != "access-1" {
		t.Errorf("host access = %q, want access-1", got)
	}
	if !strings.Contains(e.logs.String(), "copied the OAuth entry of [anthropic]") {
		t.Errorf("log has no copy line:\n%s", e.logs.String())
	}
	if strings.Contains(e.logs.String(), "access-1") || strings.Contains(e.logs.String(), "refresh-1") {
		t.Errorf("log holds a token:\n%s", e.logs.String())
	}
}

// A session file that is a symlink changes nothing on the host, and the
// sidecar logs the reason.
func TestPIAuthSyncEndpoint_SymlinkLogsReason(t *testing.T) {
	e := newPIAuthSidecarEnv(t, config.IsolationSandboxExec)
	target := filepath.Join(t.TempDir(), "target.json")
	writeTestFile(t, target, `{"anthropic":{"type":"oauth","access":"stolen","refresh":"stolen","expires":5000}}`)
	if err := os.Remove(e.sessAuth); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, e.sessAuth); err != nil {
		t.Fatal(err)
	}
	hostBefore, _ := os.ReadFile(e.hostAuth)

	if rec := e.post(t, http.MethodPost); rec.Code != http.StatusOK {
		t.Fatalf("POST /pi-auth/sync = %d %s", rec.Code, rec.Body.String())
	}
	if after, _ := os.ReadFile(e.hostAuth); !bytes.Equal(after, hostBefore) {
		t.Errorf("host file changed:\n%s", after)
	}
	if !strings.Contains(e.logs.String(), "is a symlink") {
		t.Errorf("log has no reason:\n%s", e.logs.String())
	}
}

// A github-copilot entry in the session file changes nothing on the host,
// and the sidecar logs the reason (#3314). pi sends each Copilot request to
// the proxy-ep host in the access value.
func TestPIAuthSyncEndpoint_CopilotEntryLogsReason(t *testing.T) {
	e := newPIAuthSidecarEnv(t, config.IsolationBwrap)
	writeTestFile(t, e.sessAuth, `{"anthropic":{"type":"oauth","access":"access-0","refresh":"refresh-0","expires":1000},`+
		`"github-copilot":{"type":"oauth","access":"tid=x;exp=9999999999;proxy-ep=proxy.attacker.example;","refresh":"r","expires":9999999999999}}`)
	hostBefore, _ := os.ReadFile(e.hostAuth)

	if rec := e.post(t, http.MethodPost); rec.Code != http.StatusOK {
		t.Fatalf("POST /pi-auth/sync = %d %s", rec.Code, rec.Body.String())
	}
	if after, _ := os.ReadFile(e.hostAuth); !bytes.Equal(after, hostBefore) {
		t.Errorf("host file changed:\n%s", after)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, `not copied to the host: provider "github-copilot": the sync copies only the "anthropic" entry to the host file`) {
		t.Errorf("log has no reason:\n%s", logs)
	}
	if strings.Contains(logs, "attacker") {
		t.Errorf("log holds a value from the session file:\n%s", logs)
	}
}

func TestPIAuthSyncEndpoint_MethodAndMode(t *testing.T) {
	e := newPIAuthSidecarEnv(t, config.IsolationBwrap)
	if rec := e.post(t, http.MethodGet); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /pi-auth/sync = %d, want 405", rec.Code)
	}
	host := newPIAuthSidecarEnv(t, config.IsolationHost)
	if rec := host.post(t, http.MethodPost); rec.Code != http.StatusNotFound {
		t.Errorf("POST /pi-auth/sync in host mode = %d, want 404", rec.Code)
	}
	container.RemovePIAuthSession(piAuthTestInstance)
	if rec := e.post(t, http.MethodPost); rec.Code != http.StatusNotFound {
		t.Errorf("POST /pi-auth/sync with no per-session dir = %d, want 404", rec.Code)
	}
}

// The timer sync brings `prism account use` to a running session with no
// restart. It does not run while the shared refresh lock is held.
func TestPIAuthSyncTimer_AccountUseReachesSession(t *testing.T) {
	prev := piAuthSyncInterval
	piAuthSyncInterval = 20 * time.Millisecond
	t.Cleanup(func() { piAuthSyncInterval = prev })
	e := newPIAuthSidecarEnv(t, config.IsolationBwrap)

	release, err := container.TakePIRefreshLock(e.lockDir, 0)
	if err != nil {
		t.Fatalf("TakePIRefreshLock: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.s.startPIAuthSync(ctx)

	replaceFile(t, e.hostAuth, `{"anthropic":{"type":"oauth","access":"acct2","refresh":"acct2-r","expires":2000}}`)
	time.Sleep(10 * piAuthSyncInterval)
	if got := readAccess(t, e.sessAuth); got != "access-0" {
		t.Errorf("session access = %q while the refresh lock is held, want access-0", got)
	}
	release()

	deadline := time.Now().Add(5 * time.Second)
	for readAccess(t, e.sessAuth) != "acct2" {
		if time.Now().After(deadline) {
			t.Fatalf("the account change did not reach the session file; log:\n%s", e.logs.String())
		}
		time.Sleep(piAuthSyncInterval)
	}
	cancel()
	time.Sleep(10 * piAuthSyncInterval)
	if _, err := os.Lstat(filepath.Join(e.lockDir, "anthropic-oauth-refresh.lock")); !os.IsNotExist(err) {
		t.Errorf("the timer sync did not release the refresh lock: %v", err)
	}
}

// The timer sync does not start for a host-mode session.
func TestPIAuthSyncTimer_NotInHostMode(t *testing.T) {
	prev := piAuthSyncInterval
	piAuthSyncInterval = 10 * time.Millisecond
	t.Cleanup(func() { piAuthSyncInterval = prev })
	e := newPIAuthSidecarEnv(t, config.IsolationHost)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.s.startPIAuthSync(ctx)

	replaceFile(t, e.hostAuth, `{"anthropic":{"type":"oauth","access":"acct2","refresh":"acct2-r","expires":2000}}`)
	time.Sleep(20 * piAuthSyncInterval)
	if got := readAccess(t, e.sessAuth); got != "access-0" {
		t.Errorf("session access = %q in host mode, want no sync", got)
	}
}

// A session that floods the endpoint gets 429 once the queue is full.
func TestPIAuthSyncEndpoint_RateLimit(t *testing.T) {
	e := newPIAuthSidecarEnv(t, config.IsolationBwrap)
	e.s.piAuth.mu.Lock()
	e.s.piAuth.lastEndpoint = time.Now().Add(2 * piAuthSyncMaxWait)
	e.s.piAuth.mu.Unlock()
	if rec := e.post(t, http.MethodPost); rec.Code != http.StatusTooManyRequests {
		t.Errorf("POST /pi-auth/sync with a full queue = %d, want 429", rec.Code)
	}
}

// A host-mode pi (a coordinator) and a sandboxed worker need a refresh at
// the same time. pi takes the lock of the host auth.json first, then waits
// for the shared refresh lock. The worker holds the refresh lock, refreshes,
// and asks for a push sync. The push must not wait for the lock of the host
// pi: the new token must be in the host file when the host pi gets the
// refresh lock, so that it adopts the token and sends no second request
// with the rotated refresh token.
func TestPIAuthSyncEndpoint_HostModePiHoldsItsLock(t *testing.T) {
	e := newPIAuthSidecarEnv(t, config.IsolationBwrap)

	// The worker holds the refresh lock.
	releaseWorker, err := container.TakePIRefreshLock(e.lockDir, 0)
	if err != nil {
		t.Fatalf("TakePIRefreshLock: %v", err)
	}

	// The host pi holds its auth.json.lock and waits for the refresh lock.
	if err := os.Mkdir(e.hostAuth+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 1)
	go func() {
		release, err := container.TakePIRefreshLock(e.lockDir, 10*time.Second)
		if err != nil {
			seen <- "lock error: " + err.Error()
			return
		}
		defer release()
		seen <- readAccess(t, e.hostAuth)
	}()

	// The worker refreshes, writes its file, and asks for a push.
	writeTestFile(t, e.sessAuth, `{"anthropic":{"type":"oauth","access":"access-w","refresh":"refresh-w","expires":5000}}`)
	started := time.Now()
	rec := e.post(t, http.MethodPost)
	if took := time.Since(started); took > time.Second {
		t.Errorf("the push waited %v for the lock of the host pi", took)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /pi-auth/sync = %d %s", rec.Code, rec.Body.String())
	}
	releaseWorker()

	select {
	case got := <-seen:
		if got != "access-w" {
			t.Errorf("the host pi read %q when it got the refresh lock, want access-w: it refreshes again with the rotated token", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the host pi did not get the refresh lock")
	}
}
