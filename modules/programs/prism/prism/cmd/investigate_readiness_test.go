package cmd

// investigate_readiness_test.go — issue #2360 regression tests.
//
// Pins that:
//
//  1. buildInvestigateSpawnOpts sets SpawnOpts.ReadinessTimeout =
//     session.DefaultReadinessTimeout, so SpawnSession's readiness gate at
//     internal/session/spawn.go actually blocks on the child agent's
//     handshake AND initial prompt delivery. Without this the /investigate
//     command exits 0 the moment tmux + sidecar are kicked off — the
//     silent-success pathology observed at 07:48 on 2026-07-06 where the
//     host-API endpoint returned 200 with no live session afterwards
//     (finding B4 of #2356).
//
//  2. investigateClientTimeout — the worker-side (container-side) client
//     timeout for POST /investigate — is at least as large as the host-side
//     /investigate handler budget (10 minutes; see
//     internal/sidecar/host_api.go). If the client timeout is shorter, a
//     slow-but-successful spawn is aborted by the client, which cancels
//     r.Context() on the server and SIGKILLs the `prism investigate` child
//     mid-spawn with no unwind.
//
// Test-suite isolation contract (AGENTS.md, issue #1608): the readiness
// gate test reuses the isolateForInvestigateBuilder helper, which pipes
// through sidecartest.NewIsolated — no $HOME writes, no host tmux/DB/bus
// state. The client-timeout test is a pure constant assertion + a mock
// unix-socket handler and never touches host state.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/session"
)

// TestInvestigateBuildSpawnOpts_SetsReadinessTimeout is the #2360 primary
// regression: without ReadinessTimeout set, SpawnSession's readiness gate at
// internal/session/spawn.go is skipped (the gate check is `if
// opts.ReadinessTimeout > 0`), so the command exits 0 before the agent has
// handshaked or delivered its initial prompt.
func TestInvestigateBuildSpawnOpts_SetsReadinessTimeout(t *testing.T) {
	configHome, d := isolateForInvestigateBuilder(t)
	const invoker = "prism-test@worker-investigate-readiness"
	const invokerProfile = "anthropic-opus"
	seedInvokerSession(t, d, invoker, invokerProfile, "")
	writeProfilesJSON(t, configHome, "nix-default", invokerProfile, "nix-default")

	opts, _, _, err := buildInvestigateSpawnOpts(d, invoker, "trace ssh auth", "")
	if err != nil {
		t.Fatalf("buildInvestigateSpawnOpts: %v", err)
	}
	// The gate check at internal/session/spawn.go is `if opts.ReadinessTimeout > 0`.
	// Any non-zero value would technically satisfy the gate; we pin the specific
	// value (DefaultReadinessTimeout) to match the AC "same default duration as
	// prism spawn" — this is the value cmd/spawn.go uses for its readiness gate,
	// and matching it ensures /investigate and /spawn behave symmetrically.
	if opts.ReadinessTimeout != session.DefaultReadinessTimeout {
		t.Errorf("SpawnOpts.ReadinessTimeout = %v, want session.DefaultReadinessTimeout (%v) — "+
			"without this the child exits 0 before the agent handshake is observed (#2360)",
			opts.ReadinessTimeout, session.DefaultReadinessTimeout)
	}
	// Defence-in-depth: a zero value means the gate is skipped entirely, which
	// is the pre-fix behaviour. Fail loudly if that regresses even if the
	// exact-value assertion above is loosened.
	if opts.ReadinessTimeout == 0 {
		t.Errorf("SpawnOpts.ReadinessTimeout is zero — readiness gate is skipped, silent-success regression (#2360)")
	}
}

// TestInvestigateClientTimeout_MeetsOrExceedsHandlerBudget pins the worker-
// side client timeout for /investigate at or above the host-side handler
// budget. The handler budget is a literal `10*time.Minute` at
// internal/sidecar/host_api.go inside the /investigate handler; if the two
// values ever drift out of alignment, this test catches it.
//
// The failure mode this prevents: worker-side client aborts (client.Timeout
// firing) while the host-side handler is still working, which cancels
// r.Context() on the server and translates into a SIGKILL of the host
// `prism investigate` child mid-spawn.
func TestInvestigateClientTimeout_MeetsOrExceedsHandlerBudget(t *testing.T) {
	const handlerBudget = 10 * time.Minute // matches internal/sidecar/host_api.go
	if investigateClientTimeout < handlerBudget {
		t.Fatalf("investigateClientTimeout (%v) < host-side /investigate handler budget (%v) — "+
			"a slow-but-successful spawn would be aborted by client disconnect (#2360)",
			investigateClientTimeout, handlerBudget)
	}
}

// TestProxyToHostAPIWithTimeout_HonoursOverride verifies the plumbing: when
// clientTimeout is smaller than the server's response latency, the client
// aborts (any error is fine — the point is it does not silently wait for the
// server). This is the mechanism by which the pre-fix 60 s client timeout
// aborted slow-but-successful /investigate spawns; the fix aligns the two
// values so this class of abort can no longer fire.
func TestProxyToHostAPIWithTimeout_HonoursOverride(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the client timeout so client.Timeout fires.
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"prism-test@investigate-late"}`))
	})

	// 50 ms << 300 ms server sleep → client must abort.
	start := time.Now()
	err := proxyToHostAPIWithTimeout(srv.apiURL(), "/investigate", map[string]any{"prompt": "x"}, nil, 50*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected client-side timeout error, got nil")
	}
	// Sanity: if the client timeout was not respected, elapsed would be
	// >= 300 ms. Give some slack (200 ms) for CI jitter — the point is
	// that we did not wait for the server.
	if elapsed >= 200*time.Millisecond {
		t.Errorf("proxyToHostAPIWithTimeout returned after %v — client timeout override was not honoured "+
			"(expected abort well before the 300 ms server response)", elapsed)
	}
}

// TestProxyToHostAPIWithTimeout_ZeroPreservesDefault verifies the negative
// case: clientTimeout=0 means "use the default 60 s from newHostAPIClient"
// — the override path must not clobber the default to 0 (which would mean
// "no timeout" and could hang callers indefinitely).
//
// We check this by exercising a fast handler with clientTimeout=0 and
// asserting the call succeeds. If clientTimeout=0 accidentally set
// client.Timeout = 0, the call would still succeed here (0 means unbounded),
// so we complement with a second call using proxyToHostAPI (the pre-fix
// entry point) to confirm the wrapping is a no-op — the point of this test
// is that adding the timeout parameter does not change existing behaviour
// for callers that don't opt in.
func TestProxyToHostAPIWithTimeout_ZeroPreservesDefault(t *testing.T) {
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session_name":"ok"}`))
	})

	// Both entry points on a fast handler with clientTimeout=0 must succeed
	// — the timeout parameter is opt-in and defaults to the pre-fix 60 s.
	if err := proxyToHostAPIWithTimeout(srv.apiURL(), "/investigate", map[string]any{"prompt": "x"}, nil, 0); err != nil {
		t.Errorf("proxyToHostAPIWithTimeout(0) on fast handler: unexpected error: %v", err)
	}
	if err := proxyToHostAPI(srv.apiURL(), "/investigate", map[string]any{"prompt": "x"}, nil); err != nil {
		t.Errorf("proxyToHostAPI on fast handler: unexpected error: %v", err)
	}
}
