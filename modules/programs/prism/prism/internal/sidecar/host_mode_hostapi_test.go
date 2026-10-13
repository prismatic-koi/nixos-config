package sidecar

// host_mode_hostapi_test.go — a host-mode sidecar serves hostapi.sock the
// same way as a sandboxed sidecar (issue #3135).
//
// `prism prompt` to a pi session, and every prism notification to it, dial
// the hostapi.sock of the target. A host-mode sidecar that does not bind the
// socket cannot receive a prompt. These tests run a real Sidecar.Run in each
// isolation mode and use the real socket:
//
//   - The socket file and its parent directory have the same mode in every
//     isolation mode, and the directory is 0700.
//   - promptdelivery.DeliverToSessionEx — the delivery path of `prism prompt`
//     on the host — reaches the pi peer of the target in every mode.
//   - Every coordinator-only endpoint refuses a host-mode worker through its
//     socket, exactly as it refuses a sandboxed worker.
//
// cmd/sidecar_hostapi_path_test.go pins the other half: runSidecar resolves
// a socket path for host mode, so Config.HostAPISockPath is not empty.
//
// # Isolation contract
//
// Every sidecar is built on sidecartest.NewIsolated, so $XDG_STATE_HOME is a
// temp dir and PRISM_TEST_MODE_RESTRICT_HOSTAPI is set. The socket paths come
// from the production resolvers under that temp dir. Session names use the
// prism-test prefix. Every sidecar runs a failing stub in place of the prism
// binary, so a role gate that regresses execs the stub and returns 500, not
// 403.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/db"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
	prismsession "github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// modeSidecar is a Sidecar that runs in a goroutine, with the paths of its
// two sockets.
type modeSidecar struct {
	sc          *Sidecar
	hostAPISock string
	pipeSock    string
}

// startModeSidecar runs a pi sidecar for sessionName in the given isolation
// mode. The socket paths come from the production resolvers, so the caller
// must have called sidecartest.NewIsolated first. The sidecar stops in
// t.Cleanup, before the DB of the caller closes.
func startModeSidecar(t *testing.T, mode config.IsolationMode, sessionName, repo, role string, d *db.DB) *modeSidecar {
	t.Helper()

	hostAPISock, err := prismsession.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHostAPIPath: %v", err)
	}
	pipeSock, err := prismsession.SidecarHarnessPipePath(sessionName)
	if err != nil {
		t.Fatalf("SidecarHarnessPipePath: %v", err)
	}
	if len(hostAPISock) > maxSunPath || len(pipeSock) > maxSunPath {
		t.Fatalf("socket path too long: %s", hostAPISock)
	}

	stubPath := filepath.Join(t.TempDir(), "prism-stub")
	if err := os.WriteFile(stubPath, []byte(failingStub), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}

	sc := New(Config{
		SessionName:         sessionName,
		Repo:                repo,
		Worktree:            t.TempDir(),
		DB:                  d,
		Clock:               newTestClock(),
		AgentRole:           role,
		HarnessName:         "pi",
		Harness:             pih.New("", role, ""),
		IsolationMode:       mode,
		HostAPISockPath:     hostAPISock,
		HarnessPipeSockPath: pipeSock,
		PrismBinaryPath:     stubPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- sc.Run(ctx) }()
	t.Cleanup(func() {
		sc.Shutdown()
		cancel()
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Error("Run() did not return within 5s after Shutdown and cancel")
		}
	})

	waitForSocket(t, hostAPISock, 3*time.Second)
	return &modeSidecar{sc: sc, hostAPISock: hostAPISock, pipeSock: pipeSock}
}

// callHostAPISocket sends one request to the host API through the Unix
// socket at sockPath and returns the status code and the body.
func callHostAPISocket(t *testing.T, sockPath, method, path, body string) (int, string) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://prism-hostapi"+path, reqBody)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s through %s: %v", method, path, sockPath, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody)
}

// TestHostModeSidecar_SocketModesMatchSandboxedModes: the hostapi.sock of a
// host-mode session has the same file mode and the same parent-directory
// mode as that of a sandboxed session.
func TestHostModeSidecar_SocketModesMatchSandboxedModes(t *testing.T) {
	type modes struct {
		sock os.FileMode
		dir  os.FileMode
	}
	got := make(map[config.IsolationMode]modes)

	for _, mode := range config.ValidIsolationModes {
		t.Run(string(mode), func(t *testing.T) {
			bus := sidecartest.NewIsolated(t, "")
			ms := startModeSidecar(t, mode,
				"prism-test@3135-perm-"+string(mode), "prism-test", "worker", bus.DB)

			sockInfo, err := os.Lstat(ms.hostAPISock)
			if err != nil {
				t.Fatalf("lstat %s: %v", ms.hostAPISock, err)
			}
			if sockInfo.Mode()&os.ModeSocket == 0 {
				t.Fatalf("%s is not a socket: mode %v", ms.hostAPISock, sockInfo.Mode())
			}
			dirInfo, err := os.Stat(filepath.Dir(ms.hostAPISock))
			if err != nil {
				t.Fatalf("stat run dir: %v", err)
			}
			if perm := dirInfo.Mode().Perm(); perm != 0o700 {
				t.Errorf("run dir mode = %04o, want 0700", perm)
			}
			got[mode] = modes{sock: sockInfo.Mode(), dir: dirInfo.Mode()}
		})
	}

	host, ok := got[config.IsolationHost]
	if !ok {
		t.Fatal("no result for host mode")
	}
	for mode, m := range got {
		if m != host {
			t.Errorf("%s: socket mode %v, dir mode %v; host: socket mode %v, dir mode %v",
				mode, m.sock, m.dir, host.sock, host.dir)
		}
	}
}

// TestHostModeSidecar_ReceivesPromptDelivery: the host-side delivery path of
// `prism prompt` reaches a pi session in every isolation mode. For a
// host-mode target this is the sandboxed-coordinator direction: the
// coordinator sidecar runs `prism prompt` on the host, which calls
// DeliverToSessionEx. For a sandboxed target this is the host-mode-caller
// direction: a host-mode session has no PRISM_HOST_API, so its `prism
// prompt` calls DeliverToSessionEx directly.
func TestHostModeSidecar_ReceivesPromptDelivery(t *testing.T) {
	for _, mode := range config.ValidIsolationModes {
		t.Run(string(mode), func(t *testing.T) {
			bus := sidecartest.NewIsolated(t, "")
			target := "prism-test@3135-target-" + string(mode)
			ms := startModeSidecar(t, mode, target, "prism-test", "worker", bus.DB)

			conn, _ := dialAndHandshake(t, ms.pipeSock)
			defer conn.Close()

			harnessName := "pi"
			status := &db.Status{SessionName: target, Repo: "prism-test", State: "active", Harness: &harnessName}
			const text = "prompt for issue 3135"
			outcome, err := promptdelivery.DeliverToSessionEx(target, status, text, nil, "", "followUp")
			if err != nil {
				t.Fatalf("DeliverToSessionEx(%s target): %v", mode, err)
			}
			if outcome.Buffered || outcome.Replayed {
				t.Errorf("outcome = %+v, want a synchronous delivery", outcome)
			}

			rd := bufio.NewReader(conn)
			deadline := time.Now().Add(3 * time.Second)
			for {
				if time.Now().After(deadline) {
					t.Fatal("no prompt frame reached the pi peer within 3s")
				}
				_ = conn.SetReadDeadline(deadline)
				line, readErr := rd.ReadBytes('\n')
				if readErr != nil {
					t.Fatalf("read pipe frame: %v", readErr)
				}
				var frame map[string]any
				if err := json.Unmarshal(line, &frame); err != nil {
					t.Fatalf("unmarshal pipe frame %q: %v", line, err)
				}
				if frame["type"] != "prompt" {
					continue
				}
				if frame["text"] != text {
					t.Errorf("prompt frame text = %v, want %q", frame["text"], text)
				}
				if frame["deliver_as"] != "followUp" {
					t.Errorf("prompt frame deliver_as = %v, want followUp", frame["deliver_as"])
				}
				return
			}
		})
	}
}

// TestHostModeSidecar_RoleGatesRefuseWorkerThroughSocket: every
// coordinator-only endpoint refuses a worker that calls through its own
// hostapi.sock, with the same status in host mode as in a sandboxed mode.
// The gates read the session name and the DB row, not the isolation mode.
// If a gate starts to trust host mode, this test fails.
func TestHostModeSidecar_RoleGatesRefuseWorkerThroughSocket(t *testing.T) {
	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/spawn", `{"branch":"feature-x","prompt":"do the thing"}`},
		{http.MethodPost, "/cleanup", `{"session":"prism-test-rolegate@feature-x","yes":true}`},
		{http.MethodPost, "/close", `{"session":"prism-test-rolegate@feature-x"}`},
		{http.MethodPost, "/merge", `{"pr":1}`},
		{http.MethodGet, "/merges", ""},
		{http.MethodPost, "/merges/cancel", `{"pr":1}`},
		{http.MethodPost, "/investigate", `{"prompt":"look into this"}`},
		{http.MethodGet, "/logs?session=prism-test-rolegate@feature-x", ""},
		{http.MethodGet, "/db/query?sql=SELECT+1", ""},
		{http.MethodGet, "/db/schema", ""},
		{http.MethodGet, "/db/tables", ""},
		{http.MethodGet, "/audit", ""},
		// A worker can prompt only its own coordinator.
		{http.MethodPost, "/prompt", `{"session":"prism-test-rolegate@other-worker","prompt":"hello"}`},
	}

	for _, mode := range config.ValidIsolationModes {
		t.Run(string(mode), func(t *testing.T) {
			bus := sidecartest.NewIsolated(t, "")
			const worker = "prism-test-rolegate@feature-x"
			if err := bus.DB.UpsertStatusSeedRootAgentName(
				worker, "prism-test-rolegate", "/tmp/prism-test-rolegate", "active", nil, nil, "worker", "", "",
			); err != nil {
				t.Fatalf("seed worker status: %v", err)
			}
			if err := bus.DB.UpsertStatus("prism-test-rolegate@other-worker", "prism-test-rolegate",
				"/tmp/prism-test-rolegate-other", "active", nil, nil); err != nil {
				t.Fatalf("seed other worker status: %v", err)
			}
			ms := startModeSidecar(t, mode, worker, "prism-test-rolegate", "worker", bus.DB)

			for _, tc := range cases {
				code, body := callHostAPISocket(t, ms.hostAPISock, tc.method, tc.path, tc.body)
				if code != http.StatusForbidden {
					t.Errorf("%s: %s %s: status = %d, body = %q, want 403",
						mode, tc.method, tc.path, code, truncateForLog(body, 200))
				}
			}
		})
	}
}
