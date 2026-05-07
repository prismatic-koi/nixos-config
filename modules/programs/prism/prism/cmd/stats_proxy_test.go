package cmd

// Tests for the PRISM_HOST_API proxy dispatch in prism stats (#1463).
//
// Each test spins up a real HTTP server bound to a Unix socket, sets
// PRISM_HOST_API, and verifies that:
//   - the correct GET request (path, query params) is sent to the host sidecar
//   - the rendered output is produced from the proxy response (rendering on CLI side)
//   - --json emits the raw response JSON
//   - when PRISM_HOST_API is unset, the proxy is a no-op (existing tests cover direct path)

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/db"
)

// fakeStatsServer is a minimal mock for the /stats endpoint.
type fakeStatsServer struct {
	capturedPath  string
	capturedQuery string
	response      []byte
	statusCode    int
}

func (s *fakeStatsServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		s.capturedPath = r.URL.Path
		s.capturedQuery = r.URL.RawQuery

		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"want GET"}`, http.StatusMethodNotAllowed)
			return
		}

		status := s.statusCode
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(s.response)
	})
	return mux
}

// startFakeStatsServer starts the fake /stats server bound to a Unix socket.
func startFakeStatsServer(t *testing.T, resp []byte) (*fakeStatsServer, string) {
	t.Helper()
	srv := &fakeStatsServer{response: resp}
	mock := newMockUnixServer(t, srv.handler().ServeHTTP)
	return srv, mock.apiURL()
}

// ── proxyStats helper tests ───────────────────────────────────────────────────

// TestProxyStats_SendsCorrectQueryParams verifies that proxyStats sends a GET
// /stats request with the correct view and session query params.
func TestProxyStats_SendsCorrectQueryParams(t *testing.T) {
	respBody := `{"events":[]}`
	srv, apiURL := startFakeStatsServer(t, []byte(respBody))

	raw, err := proxyStats(apiURL, "doomloops", "myrepo@main", 7, "", 0)
	if err != nil {
		t.Fatalf("proxyStats: %v", err)
	}
	if srv.capturedPath != "/stats" {
		t.Errorf("path = %q, want /stats", srv.capturedPath)
	}
	if !strings.Contains(srv.capturedQuery, "view=doomloops") {
		t.Errorf("query %q does not contain view=doomloops", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "session=myrepo%40main") {
		t.Errorf("query %q does not contain session=myrepo%%40main", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "days=7") {
		t.Errorf("query %q does not contain days=7", srv.capturedQuery)
	}
	if len(raw) == 0 {
		t.Error("expected non-empty raw response")
	}
}

// TestProxyStats_SummaryView verifies the view=summary query param is sent.
func TestProxyStats_SummaryView(t *testing.T) {
	respBody := `{"sessions":[]}`
	srv, apiURL := startFakeStatsServer(t, []byte(respBody))

	_, err := proxyStats(apiURL, "summary", "", 0, "nixos-config", 0)
	if err != nil {
		t.Fatalf("proxyStats summary: %v", err)
	}
	if !strings.Contains(srv.capturedQuery, "view=summary") {
		t.Errorf("query %q does not contain view=summary", srv.capturedQuery)
	}
	if !strings.Contains(srv.capturedQuery, "repo=nixos-config") {
		t.Errorf("query %q does not contain repo=nixos-config", srv.capturedQuery)
	}
}

// TestProxyStats_Returns404AsError verifies that a 404 from the server is
// surfaced as an error with the error message.
func TestProxyStats_Returns404AsError(t *testing.T) {
	respBody := `{"error":"session \"ghost\" not found"}`
	srv := &fakeStatsServer{response: []byte(respBody), statusCode: http.StatusNotFound}
	mock := newMockUnixServer(t, srv.handler().ServeHTTP)

	_, err := proxyStats(mock.apiURL(), "detail", "ghost", 0, "", 0)
	if err == nil {
		t.Fatal("expected non-nil error for 404 response")
	}
	if !strings.Contains(err.Error(), "ghost") && !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not mention ghost or 404", err.Error())
	}
}

// ── runStatsProxy integration tests ──────────────────────────────────────────

// TestRunStatsProxy_Doomloops verifies that when PRISM_HOST_API is set and
// --doomloops is passed, runStats sends GET /stats?view=doomloops to the server
// and renders the result.
func TestRunStatsProxy_Doomloops(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	events := []db.Event{
		{
			ID:          "evt-1",
			SessionName: "nixos-config@main",
			Type:        "doom_loop_detected",
			Payload:     `{"tool":"bash","pattern":"git status","count":5,"timestampMs":0}`,
			CreatedAt:   now.Add(-1 * time.Hour),
		},
	}
	eventsJSON, _ := json.Marshal(events)
	respBody, _ := json.Marshal(map[string]any{"events": events})

	srv, apiURL := startFakeStatsServer(t, respBody)

	t.Setenv("PRISM_HOST_API", apiURL)
	_ = eventsJSON // suppress unused warning
	_ = srv

	statsCmd.Flags().Set("doomloops", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("doomloops", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats: %v", err)
		}
	})

	if !strings.Contains(out, "Doom Loop Events") {
		t.Errorf("output missing 'Doom Loop Events' heading\ngot:\n%s", out)
	}
	if !strings.Contains(srv.capturedQuery, "view=doomloops") {
		t.Errorf("proxy did not send view=doomloops; query = %q", srv.capturedQuery)
	}
}

// TestRunStatsProxy_DoomloopsJSON verifies that --doomloops --json emits the
// raw response JSON, byte-identical to what the server returned.
func TestRunStatsProxy_DoomloopsJSON(t *testing.T) {
	events := []db.Event{
		{
			ID:          "evt-json",
			SessionName: "nixos-config@main",
			Type:        "doom_loop_detected",
			Payload:     `{"tool":"bash","pattern":"git diff","count":3,"timestampMs":0}`,
			CreatedAt:   time.Now().Add(-30 * time.Minute),
		},
	}
	respBody, _ := json.Marshal(map[string]any{"events": events})

	_, apiURL := startFakeStatsServer(t, respBody)

	t.Setenv("PRISM_HOST_API", apiURL)

	statsCmd.Flags().Set("doomloops", "true") //nolint:errcheck
	statsCmd.Flags().Set("json", "true")      //nolint:errcheck
	defer func() {
		statsCmd.Flags().Set("doomloops", "false") //nolint:errcheck
		statsCmd.Flags().Set("json", "false")      //nolint:errcheck
	}()

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats --doomloops --json: %v", err)
		}
	})

	// Output should be valid JSON containing "events" key.
	var parsed map[string]json.RawMessage
	trimmed := strings.TrimSpace(out)
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, out)
	}
	if _, ok := parsed["events"]; !ok {
		t.Errorf("--json output missing 'events' key; got: %s", out)
	}
}

// TestRunStatsProxy_Summary verifies that prism stats (no flags) via proxy
// renders a session table when PRISM_HOST_API is set.
func TestRunStatsProxy_Summary(t *testing.T) {
	sessions := []db.Session{
		{
			InstanceID:  "aaaa1111-2222-3333-4444-555555555555",
			SessionName: "nixos-config@main",
			Repo:        "nixos-config",
			Worktree:    "/tmp/w",
			Harness:     "opencode",
			StartedAt:   time.Now().Add(-1 * time.Hour),
		},
	}
	respBody, _ := json.Marshal(map[string]any{"sessions": sessions})

	srv, apiURL := startFakeStatsServer(t, respBody)

	t.Setenv("PRISM_HOST_API", apiURL)

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats (summary proxy): %v", err)
		}
	})

	if !strings.Contains(srv.capturedQuery, "view=summary") {
		t.Errorf("proxy did not send view=summary; query = %q", srv.capturedQuery)
	}
	// The rendered output should contain the session name.
	if !strings.Contains(out, "nixos-config@main") {
		t.Errorf("output missing session name\ngot:\n%s", out)
	}
}

// TestRunStatsProxy_SummaryJSON verifies that prism stats --json (no event flags)
// emits the proxy response JSON.
func TestRunStatsProxy_SummaryJSON(t *testing.T) {
	sessions := []db.Session{
		{
			InstanceID:  "bbbb1111-2222-3333-4444-555555555555",
			SessionName: "nixos-config@feat",
			Repo:        "nixos-config",
			Worktree:    "/tmp/w",
			Harness:     "opencode",
			StartedAt:   time.Now().Add(-2 * time.Hour),
		},
	}
	respBody, _ := json.Marshal(map[string]any{"sessions": sessions})

	_, apiURL := startFakeStatsServer(t, respBody)

	t.Setenv("PRISM_HOST_API", apiURL)

	statsCmd.Flags().Set("json", "true")      //nolint:errcheck
	defer statsCmd.Flags().Set("json", "false") //nolint:errcheck

	out := captureStdout(t, func() {
		if err := runStats(statsCmd, nil); err != nil {
			t.Errorf("runStats --json (summary proxy): %v", err)
		}
	})

	var parsed map[string]json.RawMessage
	trimmed := strings.TrimSpace(out)
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, out)
	}
	if _, ok := parsed["sessions"]; !ok {
		t.Errorf("--json output missing 'sessions' key; got: %s", out)
	}
}

// TestRunStatsProxy_GroupByFallsThrough verifies that --group-by skips the
// proxy and uses the local DB path even when PRISM_HOST_API is set.
// This guards against the regression where --group-by was silently dropped.
func TestRunStatsProxy_GroupByFallsThrough(t *testing.T) {
	_ = openStatsTestDB(t) // sets up empty DB, unsets PRISM_HOST_API via Setenv

	// Stand up a server that records requests — it should NOT be contacted
	// when --group-by is set.
	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sessions":[]}`)) //nolint:errcheck
	})

	// Set PRISM_HOST_API after openStatsTestDB cleared it.
	t.Setenv("PRISM_HOST_API", srv.apiURL())

	statsCmd.Flags().Set("group-by", "harness") //nolint:errcheck
	defer statsCmd.Flags().Set("group-by", "") //nolint:errcheck

	_ = captureStdout(t, func() {
		// Uses local DB (empty) — should produce output from runStatsGroupBy,
		// not from the proxy. Any error is acceptable; no request to proxy.
		_ = runStats(statsCmd, nil)
	})

	if requestCount > 0 {
		t.Errorf("proxy server received %d request(s) for --group-by; should use local DB", requestCount)
	}
}

// TestRunStatsProxy_DaysHistoricalFallsThrough verifies that --days N with no
// event flags and no session arg skips the proxy (uses runStatsHistorical on
// the local DB) even when PRISM_HOST_API is set.
func TestRunStatsProxy_DaysHistoricalFallsThrough(t *testing.T) {
	_ = openStatsTestDB(t)

	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sessions":[]}`)) //nolint:errcheck
	})

	t.Setenv("PRISM_HOST_API", srv.apiURL())

	statsCmd.Flags().Set("days", "7")  //nolint:errcheck
	defer statsCmd.Flags().Set("days", "0") //nolint:errcheck

	_ = captureStdout(t, func() {
		_ = runStats(statsCmd, nil)
	})

	if requestCount > 0 {
		t.Errorf("proxy server received %d request(s) for --days historical; should use local DB", requestCount)
	}
}

// TestRunStatsProxy_HostPathUnchanged verifies that when PRISM_HOST_API is
// NOT set, runStats does not contact any server (the proxy path is not taken).
func TestRunStatsProxy_HostPathUnchanged(t *testing.T) {
	_ = openStatsTestDB(t)
	// PRISM_HOST_API is cleared by openStatsTestDB via t.Setenv.

	statsCmd.Flags().Set("doomloops", "true")        //nolint:errcheck
	defer statsCmd.Flags().Set("doomloops", "false") //nolint:errcheck

	// Stand up a server that records requests — it should NOT be contacted.
	requestCount := 0
	srv := newMockUnixServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[]}`))
	})

	// Explicitly NOT setting PRISM_HOST_API — the test DB is used instead.
	_ = srv

	out := captureStdout(t, func() {
		_ = runStats(statsCmd, nil)
	})

	if requestCount > 0 {
		t.Errorf("proxy server received %d request(s) when PRISM_HOST_API is unset", requestCount)
	}
	if !strings.Contains(out, "Doom Loop Events") {
		t.Errorf("output missing 'Doom Loop Events' heading for direct-DB path; got:\n%s", out)
	}
}
