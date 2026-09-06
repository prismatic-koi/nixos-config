package sidecar

// Tests for the per-container resource caps the sidecar wires into the
// podman proxy.
//
// These tests deliberately run through the REAL sidecar wiring rather
// than constructing a podmanproxy.Config directly. The cap VALUES are
// the thing under test — a proxy that enforces whatever cap a test hands
// it proves nothing about what a spawned worker actually gets. Every
// assertion here therefore goes through runPodmanProxyIfEnabled and hits
// the constants in podman_proxy.go.
//
// The isolation convention applies as in podman_proxy_test.go:
// sidecartest.NewIsolated and a "prism-test@" session name, so no host
// podman socket, DB, or tmux server is touched.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// startStubPodmanUpstream binds a minimal HTTP server on a Unix socket
// under dir and returns its path. Every request gets 200 with an empty
// JSON object, which is all the cap tests need: they only have to
// distinguish "the proxy forwarded" from "the proxy denied".
//
// A real upstream is required for the forward case. A denied request
// never reaches a dial, so the deny cases would pass against a
// non-existent socket too — and would keep passing if the policy broke
// open, because the 503 path would mask it. Binding a real upstream is
// what makes the forward assertion load-bearing.
func startStubPodmanUpstream(t *testing.T, dir string) string {
	t.Helper()
	sockPath := filepath.Join(dir, "upstream.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen on stub upstream %s: %v", sockPath, err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			// The proxy probes GET /images/{ref}/json before it
			// records a pull, and records only when the image is
			// NOT already in the store. An empty store answers 404,
			// which is what makes a pull through this stub a
			// genuine new pull.
			if r.Method == http.MethodGet &&
				strings.HasPrefix(r.URL.Path, "/images/") &&
				strings.HasSuffix(r.URL.Path, "/json") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"No such image"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Id":"stub"}`))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	return sockPath
}

// capProbe is one containers/create request against the proxy plus the
// outcome the caps must produce for it.
//
// hostConfig is wrapped in a pointer-shaped marker so a probe can
// express three distinct bodies: a HostConfig with fields, an absent
// HostConfig key, and an explicit `"HostConfig": null`. The last two
// are the bypass shapes — see TestPodmanProxy_ResourceCaps_ prefixed
// probes "host config absent" and "host config null".
type capProbe struct {
	name       string
	hostConfig map[string]any
	// omitHostConfig drops the HostConfig key from the body entirely.
	omitHostConfig bool
	// nullHostConfig sets the HostConfig key to JSON null.
	nullHostConfig bool
	wantStatus     int
	// wantReason is the exact audit reason token the request must
	// produce. Empty means "do not assert on the reason" (the forward
	// case, whose reason is an implementation detail of the allow
	// path).
	wantReason string
}

// readLastAuditReason returns the `reason` field of the last complete
// line in the proxy's audit log. Audit writes are append-only and
// serialised under the proxy's own mutex, and these tests fire one
// request at a time, so the last line belongs to the request just made.
func readLastAuditReason(t *testing.T, auditPath string) string {
	t.Helper()
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log %s: %v", auditPath, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	last := lines[len(lines)-1]
	var rec struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(last), &rec); err != nil {
		t.Fatalf("audit line is not valid JSON: %v\nline=%q", err, last)
	}
	return rec.Reason
}

// TestPodmanProxy_ResourceCaps_WiredFromSidecar drives every documented
// cap outcome through a live sidecar-built proxy.
//
// The four deny cases are the security ACs: a container the agent starts
// through the proxy is a HOST process outside the bwrap / sandbox-exec
// sandbox, so an uncapped one can exhaust the host (threat T14). The
// forward case and the CpuQuota case are the matching
// not-a-no-op controls — a policy that denied everything would satisfy
// the deny assertions on its own.
func TestPodmanProxy_ResourceCaps_WiredFromSidecar(t *testing.T) {
	session := "prism-test@" + t.Name()
	bus := sidecartest.NewIsolated(t, session)

	upstream := startStubPodmanUpstream(t, bus.XDGStateHome)
	sc, listenerPath := newPodmanProxyTestSidecar(t, bus, session, upstream)
	setContainersEnabled(t, bus, session, true)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := runSidecarBackground(t, sc, ctx)
	if !waitForPath(listenerPath, 3*time.Second) {
		t.Fatalf("podman.sock listener did not appear at %s", listenerPath)
	}

	sessionDir, err := container.SessionWorkDirPath(sc.cfg.InstanceID)
	if err != nil {
		t.Fatalf("SessionWorkDirPath: %v", err)
	}
	auditPath := filepath.Join(sessionDir, "podman-proxy.log")

	// Values that sit inside both caps, used wherever a probe needs a
	// field it is not testing.
	const okMemory = 1 << 30         // 1 GiB
	const okNanoCpus = 1e9           // 1 CPU
	const overMemory = 8 << 30       // 8 GiB — over the 4 GiB cap
	const overNanoCpus = 4 * 1e9     // 4 CPUs — over the 2 CPU cap
	name := "prism-" + session + "-" // satisfies the name policy

	probes := []capProbe{
		{
			// The cap check must NOT be reachable only through a
			// present HostConfig. A body that omits the key entirely
			// is the cheapest possible bypass: podman defaults every
			// HostConfig field, so the container runs uncapped on the
			// host. This probe fails if the HostConfig policy is ever
			// put back behind a presence guard.
			name:           "host config absent",
			omitHostConfig: true,
			wantStatus:     http.StatusForbidden,
			wantReason:     "memory_required",
		},
		{
			// The same bypass spelled as an explicit JSON null, which
			// is a separate decode path from an absent key.
			name:           "host config null",
			nullHostConfig: true,
			wantStatus:     http.StatusForbidden,
			wantReason:     "memory_required",
		},
		{
			// An empty object was already caught before this change.
			// Kept so the three absent-ish shapes are pinned together.
			name:       "host config empty object",
			hostConfig: map[string]any{},
			wantStatus: http.StatusForbidden,
			wantReason: "memory_required",
		},
		{
			name:       "memory absent",
			hostConfig: map[string]any{"NanoCpus": okNanoCpus},
			wantStatus: http.StatusForbidden,
			wantReason: "memory_required",
		},
		{
			name:       "memory over cap",
			hostConfig: map[string]any{"Memory": overMemory, "NanoCpus": okNanoCpus},
			wantStatus: http.StatusForbidden,
			wantReason: "memory_over_cap",
		},
		{
			name:       "nano cpus absent",
			hostConfig: map[string]any{"Memory": okMemory},
			wantStatus: http.StatusForbidden,
			wantReason: "nano_cpus_required",
		},
		{
			name:       "nano cpus over cap",
			hostConfig: map[string]any{"Memory": okMemory, "NanoCpus": overNanoCpus},
			wantStatus: http.StatusForbidden,
			wantReason: "nano_cpus_over_cap",
		},
		{
			// Memory=0 is docker's "unlimited". The strict cap must
			// reject it rather than read it as "within the cap".
			name:       "memory zero is not unlimited",
			hostConfig: map[string]any{"Memory": 0, "NanoCpus": okNanoCpus},
			wantStatus: http.StatusForbidden,
			wantReason: "memory_nonpositive",
		},
		{
			name:       "both present and within caps",
			hostConfig: map[string]any{"Memory": okMemory, "NanoCpus": okNanoCpus},
			wantStatus: http.StatusOK,
		},
		{
			// The exact-boundary values. The caps are inclusive upper
			// bounds, so a request AT the cap must forward — this is
			// what proves the documented numbers are the numbers the
			// sidecar wired, not merely "some cap is set".
			name: "exactly at both caps",
			hostConfig: map[string]any{
				"Memory":   4294967296,
				"NanoCpus": 2000000000,
			},
			wantStatus: http.StatusOK,
		},
		{
			// One byte / one nanocpu past the documented caps must
			// deny. Paired with the case above, this pins the exact
			// boundary from both sides.
			name: "one byte over the memory cap",
			hostConfig: map[string]any{
				"Memory":   4294967297,
				"NanoCpus": okNanoCpus,
			},
			wantStatus: http.StatusForbidden,
			wantReason: "memory_over_cap",
		},
		{
			name: "one nanocpu over the cpu cap",
			hostConfig: map[string]any{
				"Memory":   okMemory,
				"NanoCpus": 2000000001,
			},
			wantStatus: http.StatusForbidden,
			wantReason: "nano_cpus_over_cap",
		},
		{
			// MaxCPUQuota stays 0, so CpuQuota is NOT mandatory. If it
			// were also capped, no docker/podman client could create a
			// container at all: clients refuse to send --cpus together
			// with --cpu-quota, so whichever field they omit would 403.
			// This request omits CpuQuota and must still forward.
			name:       "cpu quota absent is not required",
			hostConfig: map[string]any{"Memory": okMemory, "NanoCpus": okNanoCpus},
			wantStatus: http.StatusOK,
		},
	}

	client := proxyClientFor(listenerPath)
	for i, probe := range probes {
		t.Run(probe.name, func(t *testing.T) {
			body := map[string]any{
				"Image": "alpine",
				"Name":  fmt.Sprintf("%sprobe%d", name, i),
			}
			switch {
			case probe.omitHostConfig:
				// leave the key out entirely
			case probe.nullHostConfig:
				body["HostConfig"] = nil
			default:
				body["HostConfig"] = probe.hostConfig
			}
			buf, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			resp, err := client.Post("http://podman.sock/v1.41/containers/create",
				"application/json", strings.NewReader(string(buf)))
			if err != nil {
				t.Fatalf("POST containers/create: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != probe.wantStatus {
				t.Fatalf("status: got %d, want %d (body=%q)", resp.StatusCode, probe.wantStatus, raw)
			}

			gotReason := readLastAuditReason(t, auditPath)
			if probe.wantReason != "" {
				if gotReason != probe.wantReason {
					t.Errorf("audit reason: got %q, want %q", gotReason, probe.wantReason)
				}
				return
			}
			// Forward cases: assert the reason does NOT name a
			// CpuQuota rejection. This is the assertion that fails if
			// MaxCPUQuota is ever set alongside MaxNanoCpus.
			if strings.Contains(gotReason, "cpu_quota") {
				t.Errorf("audit reason %q names a CpuQuota rejection; MaxCPUQuota must stay 0 so clients that send --cpus can still create containers", gotReason)
			}
		})
	}

	cancel()
	<-done
}

// TestPodmanProxy_VolumeNamePrefix_WiredFromSession proves the sidecar
// wires VolumeNamePrefix, not just ContainerNamePrefix. Without it, a
// volume the agent creates carries a podman-generated name and survives
// `prism cleanup` forever.
//
// The probe is the deny half: an explicit Name outside the session
// prefix must 403 and the envelope must name the wired prefix. A
// non-existent upstream is fine here — the policy fires before any dial.
func TestPodmanProxy_VolumeNamePrefix_WiredFromSession(t *testing.T) {
	session := "prism-test@" + t.Name()
	bus := sidecartest.NewIsolated(t, session)

	upstream := filepath.Join(bus.XDGStateHome, "unused-upstream.sock")
	sc, listenerPath := newPodmanProxyTestSidecar(t, bus, session, upstream)
	setContainersEnabled(t, bus, session, true)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := runSidecarBackground(t, sc, ctx)
	if !waitForPath(listenerPath, 3*time.Second) {
		t.Fatalf("podman.sock listener did not appear at %s", listenerPath)
	}

	client := proxyClientFor(listenerPath)
	resp, err := client.Post("http://podman.sock/v1.41/volumes/create",
		"application/json", strings.NewReader(`{"Name":"not-our-prefix"}`))
	if err != nil {
		t.Fatalf("POST volumes/create: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403 (body=%q) — the sidecar did not wire VolumeNamePrefix",
			resp.StatusCode, raw)
	}
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%q)", err, raw)
	}
	if wantPrefix := "prism-" + session + "-"; !strings.Contains(env.Message, wantPrefix) {
		t.Errorf("envelope message does not name the wired prefix %q; got %q", wantPrefix, env.Message)
	}

	cancel()
	<-done
}

// TestPodmanProxy_ImageLedger_WiredFromSidecar proves the sidecar opens
// the per-session image ledger and hands it to the proxy, and that the
// file lands where `prism cleanup` looks for it. Without this wiring the
// image sweep has nothing to replay and every pulled image leaks.
func TestPodmanProxy_ImageLedger_WiredFromSidecar(t *testing.T) {
	session := "prism-test@" + t.Name()
	bus := sidecartest.NewIsolated(t, session)

	upstream := startStubPodmanUpstream(t, bus.XDGStateHome)
	sc, listenerPath := newPodmanProxyTestSidecar(t, bus, session, upstream)
	setContainersEnabled(t, bus, session, true)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := runSidecarBackground(t, sc, ctx)
	if !waitForPath(listenerPath, 3*time.Second) {
		t.Fatalf("podman.sock listener did not appear at %s", listenerPath)
	}

	client := proxyClientFor(listenerPath)
	resp, err := client.Post("http://podman.sock/v1.41/images/create?fromImage=alpine&tag=3.19",
		"application/json", nil)
	if err != nil {
		t.Fatalf("POST images/create: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	// Cancel first: the ledger handle is closed after the proxy
	// goroutine exits, which is what flushes the last line.
	cancel()
	<-done

	sessionDir, err := container.SessionWorkDirPath(sc.cfg.InstanceID)
	if err != nil {
		t.Fatalf("SessionWorkDirPath: %v", err)
	}
	ledgerPath := filepath.Join(sessionDir, "podman-images.log")
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read image ledger %s: %v — the sidecar did not wire PulledImageWriter", ledgerPath, err)
	}
	if !strings.Contains(string(raw), `"alpine:3.19"`) {
		t.Errorf("image ledger does not record the pulled reference; got %q", raw)
	}
}
